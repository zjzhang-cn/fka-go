package tools

import (
	"context"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/zjzhang-cn/fka-go/internal/config"
	"github.com/zjzhang-cn/fka-go/internal/llm"
)

// ToolNameSeparator 源前缀与工具名之间的分隔。**不是 `.`**：有些接口只允许
// `[a-zA-Z0-9_-]`。
const ToolNameSeparator = "__"

// MaxSourceIDChars 源前缀的长度上限。名字总长各接口一般限 64，留足余量。
const MaxSourceIDChars = 16

// toolResultLogChars 日志里工具返回值最多记多少字。完整结果仍在 transcript
// 与回给模型的消息里。
const toolResultLogChars = 200

var sourceIDIllegal = regexp.MustCompile(`[^a-z0-9_]`)

// registry 把若干 Source 合成**一张给模型看的工具表**。
//
// ## 四件事，都只做一次
//
//   - 合并与前缀：技能里可能有个 load，内置也可能有；名字里带上源前缀
//     （skills__load）就不会撞。模型看到的是全名，源只认识自己的短名。
//   - 参数校验：模型给的参数是**字符串猜出来的 JSON**，没人能保证它对。
//     这里按声明里的一小撮 JSON Schema 检查，不合格就回一句「参数不合法」——
//     模型下一轮能自己改。**不返错**：那是它的常态，不是故障。
//   - 兜错：一个源抛了不该抛的东西，不能把整轮问答带走。
//   - 按 effect 放行：没放行的工具**不告诉模型**（见 policy.go）。
//
// ## 为什么快照一次
//
// 一次问答开始时就 Tools 一次，整轮用同一张表。中途某个源变了（比如技能被删）
// 会让「模型看到的名字」与「执行时的名字」对不上——那是最难查的一类错。
// Use 之后缓存作废，下一次 Tools 重新构建。
type registry struct {
	mu      sync.Mutex
	sources []Source
	policy  Policy

	// 快照。byName 键是模型看到的全名
	byName  map[string]boundTool
	ordered []RegisteredTool
	// blocked 记下被策略挡下的工具：模型凭记忆叫到它时，能回一句「为什么没有」
	blocked map[string]Effect
	built   bool
}

type boundTool struct {
	source Source
	spec   Spec
}

// NewRegistry 造注册表。policy 决定哪些 effect 的工具会被交给模型——**默认只读**，
// 与 policy.go 的默认一致：少给一个工具不会出错，多给一个会。
func NewRegistry(initial []Source, policy Policy) Service {
	return &registry{
		sources: append([]Source(nil), initial...),
		policy:  policy,
		blocked: map[string]Effect{},
	}
}

func (r *registry) Use(source Source) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.sources = append(r.sources, source)
	// 快照作废：下一次 Tools 重新构建
	r.built = false
}

func (r *registry) Sources() []Source {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]Source(nil), r.sources...)
}

func (r *registry) Tools(ctx context.Context, tc Context) ([]RegisteredTool, error) {
	if err := r.ensure(ctx, tc); err != nil {
		return nil, err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]RegisteredTool(nil), r.ordered...), nil
}

func (r *registry) ToToolDefs(ctx context.Context, tc Context) ([]llm.ToolDef, error) {
	tools, err := r.Tools(ctx, tc)
	if err != nil {
		return nil, err
	}
	defs := make([]llm.ToolDef, 0, len(tools))
	for _, tool := range tools {
		defs = append(defs, llm.ToolDef{
			Name:        tool.FullName,
			Description: tool.Spec.Description,
			Parameters:  tool.Spec.Parameters,
		})
	}
	return defs, nil
}

// ensure 需要时才重建快照。
//
// ⚠️ 快照**每轮问答重建一次**（Node 版是进程内缓存 + use() 失效）。Go 版每次重建
// 的代价是一次 tools/list；对 MCP 源那是一次进程内 JSON-RPC 往返，家用规模下
// 毫秒级，换来的是「技能目录被改动后下一轮立刻生效」——而缓存版要等到重启。
// 真嫌慢就在这里加 TTL 缓存，**别把快照摊回到调用方**。
func (r *registry) ensure(ctx context.Context, tc Context) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.built {
		return nil
	}

	ordered := make([]RegisteredTool, 0)
	byName := map[string]boundTool{}
	blocked := map[string]Effect{}
	usedIDs := map[string]bool{}

	for _, source := range r.sources {
		id := SanitizeSourceID(source.ID())

		if id == "" {
			config.Log().Warn("工具源的前缀不合法，整源跳过", config.Context{"source": source.ID()})
			continue
		}
		if usedIDs[id] {
			// 两个源共用一个前缀，后一个的工具会覆盖前一个。宁可整个跳过
			config.Log().Warn("工具源前缀重复，后一个整源跳过",
				config.Context{"source": source.ID(), "prefix": id})
			continue
		}
		usedIDs[id] = true

		specs, err := source.List(ctx, tc)
		if err != nil {
			// 源坏了不等于服务坏了：少一批工具，别的照常。这与嵌入模型缺失同一条原则
			config.Log().Warn("工具源列举失败，按没有工具处理",
				config.Context{"source": source.ID(), "error": err.Error()})
			continue
		}

		for _, spec := range specs {
			fullName := id + ToolNameSeparator + spec.Name
			if _, exists := byName[fullName]; exists {
				config.Log().Warn("工具名重复，后一个跳过",
					config.Context{"tool": fullName, "source": source.ID()})
				continue
			}

			if !r.policy.Allows(spec.Effect) {
				// 不放行就不告诉模型。**记下来**只为了它硬叫时能给一句人话
				blocked[fullName] = spec.Effect
				continue
			}

			byName[fullName] = boundTool{source: source, spec: spec}
			ordered = append(ordered, RegisteredTool{
				FullName: fullName,
				SourceID: source.ID(),
				Label:    source.Label(),
				Spec:     spec,
			})
		}
	}

	// 顺序固定：源之间按注册顺序，源内按声明顺序。**不排序**——模型看到的顺序
	// 应与我们在代码里读到的顺序一致，排查时少一层猜测
	r.byName = byName
	r.ordered = ordered
	r.blocked = blocked
	r.built = true
	return nil
}

func (r *registry) Call(ctx context.Context, fullName string, args map[string]any, tc Context) Result {
	// 本轮要用的表是**进循环时快照的那张**：中途某个源变了会让「模型看到的名字」
	// 与「执行时的名字」对不上。快照不存在时（直调注册表的测试）现建。
	_ = r.ensure(ctx, tc)

	r.mu.Lock()
	found, ok := r.byName[fullName]
	denied, wasBlocked := r.blocked[fullName]
	r.mu.Unlock()

	started := time.Now()

	// 每次调用都记一条「入」的 DEBUG：叫了什么、带什么参数。参数是模型给的，
	// 可能很大，但 DEBUG 本就写进文件用于排查，不在这里改写它。
	//
	// **账号与消息号由 ctx 带过来**（`config.Fields` 自动合并）——工具这一层
	// 看不见渠道，不绑在 ctx 上的话这条日志就只有工具名，看不出是谁触发的
	config.Log().Debug("工具调用", config.Fields(ctx, config.Context{
		"tool": fullName, "args": args,
	}))

	var result Result
	switch {
	case !ok && wasBlocked:
		// 两种「没有」要分开说：一个是名字错了，一个是这类动作没被放行。
		// 混成一句话会让部署的人查错方向——以为工具不存在，实际是没打开
		result = FailResult("%s 属于 %s 类操作，当前没有启用。请告诉用户这件事，不要重试。",
			fullName, denied)
	case !ok:
		result = FailResult("没有叫 %s 的工具。", fullName)
	default:
		if problem := ValidateArgs(found.spec.Parameters, args); problem != "" {
			result = FailResult("参数不合法：%s", problem)
		} else {
			called, err := found.source.Call(ctx, found.spec.Name, args, tc)
			if err != nil {
				config.Log().Warn("工具执行抛错，已转成给模型的一句话",
					config.Fields(ctx, config.Context{"tool": fullName, "error": err.Error()}))
				result = FailResult("工具执行出错：%s", err.Error())
			} else {
				result = called
			}
		}
	}

	// 「出」的 DEBUG：成败、耗时、长度与截断后的内容
	config.Log().Debug("工具返回", config.Fields(ctx, config.Context{
		"tool":   fullName,
		"ok":     result.OK,
		"ms":     time.Since(started).Milliseconds(),
		"chars":  len([]rune(result.Content)),
		"result": snippet(result.Content, toolResultLogChars),
	}))

	return result
}

func (r *registry) PromptSections(ctx Context) []string {
	sections := make([]string, 0, len(r.sources))

	for _, source := range r.Sources() {
		section, err := source.PromptSection(ctx)
		if err != nil {
			// 少一段说明不影响回答，但要留下痕迹——否则「技能怎么没列出来」无从查起
			config.Log().Warn("工具源的说明生成失败，已跳过",
				config.Context{"source": source.ID(), "error": err.Error()})
			continue
		}
		if trimmed := strings.TrimSpace(section); trimmed != "" {
			sections = append(sections, trimmed)
		}
	}

	return sections
}

func (r *registry) Close() error {
	for _, source := range r.Sources() {
		if err := source.Close(); err != nil {
			config.Log().Warn("工具源关闭失败",
				config.Context{"source": source.ID(), "error": err.Error()})
		}
	}
	return nil
}

// snippet 日志里截断一段文本，避免一条 DEBUG 塞进整页正文。
func snippet(text string, max int) string {
	runes := []rune(text)
	if len(runes) <= max {
		return text
	}
	return string(runes[:max]) + "…（共 " + itoa(len(runes)) + " 字）"
}

func itoa(n int) string { return strconv.Itoa(n) }

// SanitizeSourceID 前缀只留小写字母、数字、下划线。返回空串表示这个名字没法用。
func SanitizeSourceID(raw string) string {
	cleaned := strings.ToLower(strings.TrimSpace(raw))
	cleaned = sourceIDIllegal.ReplaceAllString(cleaned, "_")
	cleaned = strings.Trim(cleaned, "_")
	if len(cleaned) > MaxSourceIDChars {
		cleaned = cleaned[:MaxSourceIDChars]
	}
	return cleaned
}

// ValidateArgs 按声明里的一小撮 JSON Schema 校验参数。**返回空串表示通过**。
//
// **只支持够用的那部分**：type: object、required、以及每个属性的 type。
// 不引 JSON Schema 库——那是个几万行的规范，而我们只需要拦住「该给字符串却给了
// 对象」「必填没填」这类会直接让工具出错的输入。
func ValidateArgs(parameters map[string]any, args map[string]any) string {
	if raw, ok := parameters["required"]; ok {
		if required, ok := raw.([]any); ok {
			for _, key := range required {
				name, ok := key.(string)
				if !ok {
					continue
				}
				if value, present := args[name]; !present || value == nil {
					return "缺少必填参数 " + name
				}
			}
		}
	}

	rawProperties, ok := parameters["properties"]
	if !ok {
		return ""
	}
	properties, ok := rawProperties.(map[string]any)
	if !ok {
		return ""
	}

	for key, value := range args {
		if value == nil {
			continue
		}
		declared, ok := properties[key].(map[string]any)
		if !ok {
			continue
		}
		expected, ok := declared["type"].(string)
		if !ok {
			continue
		}
		if !matchesType(expected, value) {
			return "参数 " + key + " 应为 " + expected
		}
	}

	return ""
}

func matchesType(expected string, value any) bool {
	switch expected {
	case "string":
		_, ok := value.(string)
		return ok
	case "number":
		return isFiniteNumber(value)
	case "integer":
		number, ok := value.(float64)
		return ok && number == float64(int64(number))
	case "boolean":
		_, ok := value.(bool)
		return ok
	case "array":
		_, ok := value.([]any)
		return ok
	case "object":
		_, ok := value.(map[string]any)
		return ok
	default:
		// 不认识的类型不拦：拦了会把「声明写得比校验更宽」变成工具用不了
		return true
	}
}

func isFiniteNumber(value any) bool {
	switch number := value.(type) {
	case float64:
		return number == number && number < 1e308 && number > -1e308
	case float32:
		return number == number && number < 1e38 && number > -1e38
	case int, int32, int64, uint, uint32, uint64:
		return true
	}
	return false
}
