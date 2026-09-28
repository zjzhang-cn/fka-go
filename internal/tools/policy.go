package tools

import (
	"os"
	"strings"

	"github.com/zjzhang-cn/fka-go/internal/config"
)

// AllEffects 全部合法的动作类型，**顺序即日志、`tools` 命令与报错的展示顺序**：
// 从「错了也没什么」到「错了收不回来」。
var AllEffects = []Effect{
	EffectRead,
	EffectMemory,
	EffectSend,
	EffectDelete,
	EffectExternal,
}

// EffectLabels 一句话说明每一类放行的是什么。
//
// **在这里而不是在 CLI 里**：`tools` 命令要打这张图，报错时也要能说清「你漏了哪个」
// ——两处各写一份，改了效果名就会有一处开始说谎。
var EffectLabels = map[Effect]string{
	EffectRead:     "查资料（搜文档 / 列文档 / 取正文 / 查记忆）",
	EffectMemory:   "记一条记忆（只增，不改不删）",
	EffectSend:     "以 Bot 的身份发文件 / 图片给用户",
	EffectDelete:   "删文档（文件、解析结果与索引一并删）",
	EffectExternal: "出网或拉起别的进程（MCP）",
}

// DefaultEffects 默认只读。要写或出网必须显式打开。
var DefaultEffects = []Effect{EffectRead}

// ToolEffectsEnv 环境变量名。写在这里，测试与 doctor 都从这里取，
// 避免两处字符串漂移。
const ToolEffectsEnv = "LLM_TOOL_EFFECTS"

// Policy 放行范围。
type Policy struct {
	allow map[Effect]bool
}

// Allows 判断某一类是否放行。
func (p Policy) Allows(effect Effect) bool { return p.allow[effect] }

// Allowed 列出已放行的类，按 AllEffects 的顺序。
func (p Policy) Allowed() []Effect {
	out := make([]Effect, 0, len(AllEffects))
	for _, effect := range AllEffects {
		if p.allow[effect] {
			out = append(out, effect)
		}
	}
	return out
}

// DefaultPolicy 默认只读。
func DefaultPolicy() Policy { return newPolicy(DefaultEffects) }

func newPolicy(effects []Effect) Policy {
	allow := make(map[Effect]bool, len(effects))
	for _, effect := range effects {
		allow[effect] = true
	}
	return Policy{allow: allow}
}

// DescribeToolPolicy 人看的说法。启动日志用它——「为什么它没有删文件的能力」
// 应当一眼看得出来。
func DescribeToolPolicy(policy Policy) string {
	on := policy.Allowed()
	off := make([]string, 0, len(AllEffects))
	for _, effect := range AllEffects {
		if !policy.allow[effect] {
			off = append(off, string(effect))
		}
	}

	head := strings.Join(toStrings(on), "、")
	if head == "" {
		head = "（空）"
	}
	if len(off) > 0 {
		return head + "（" + strings.Join(off, "、") + "未启用）"
	}
	return head + "（全部）"
}

// ReadToolPolicy 从环境变量读放行范围。
//
// 两个刻意的选择：
//   - **认不出的词只警告并忽略**，不报错也不整个作废：配置笔误不该让服务起不来，
//     但必须留下痕迹，否则「我明明写了 write」会变成一桩无头案。
//   - **一个都认不出时退回默认只读**：空集等于「什么都没放行」，那与 LLM_TOOLS=0
//     （整关）语义重叠；配置写坏了应该退回**更安全**的那一边，默认只读正是更安全的一边。
func ReadToolPolicy() Policy {
	raw := strings.TrimSpace(os.Getenv(ToolEffectsEnv))
	if raw == "" {
		return DefaultPolicy()
	}

	allow := make([]Effect, 0, len(AllEffects))
	unknown := make([]string, 0)

	for _, token := range strings.Split(raw, ",") {
		value := strings.ToLower(strings.TrimSpace(token))
		if value == "" {
			continue
		}
		if effect := Effect(value); isKnownEffect(effect) {
			allow = append(allow, effect)
		} else {
			unknown = append(unknown, value)
		}
	}

	if len(unknown) > 0 {
		config.Log().Warn(ToolEffectsEnv+" 里有认不出的取值，已忽略", config.Context{
			"unknown": unknown,
			"valid":   toStrings(AllEffects),
		})
	}

	if len(allow) == 0 {
		config.Log().Warn(ToolEffectsEnv+" 没解析出任何合法取值，按默认只读处理", config.Context{
			"raw": raw,
		})
		return DefaultPolicy()
	}

	return newPolicy(allow)
}

func isKnownEffect(effect Effect) bool {
	for _, candidate := range AllEffects {
		if candidate == effect {
			return true
		}
	}
	return false
}

func toStrings(effects []Effect) []string {
	out := make([]string, 0, len(effects))
	for _, effect := range effects {
		out = append(out, string(effect))
	}
	return out
}
