// Package skills 是**技能源**：把 `<安装根>/skills/<名字>/SKILL.md` 变成可调用的工具。
//
// ## 技能不是工具，是「做法」
//
// MCP 工具是「查一下 / 调一下外部系统」，技能是**写给人看的操作步骤**——先做什么、
// 注意什么、怎么算做完。所以模型不需要在系统提示里看到全文：**只列名字与一句话
// 说明，需要时再调 skills__load 取全文**。理由有两条：
//
//   - 全文动辄几千字，全塞进 prompt 会把上下文预算吃光，且大部分轮次用不上；
//   - 与「先看目录，再读正文」的组织方式一致。
//
// **它是能力的第二条路。** 第一条是 MCP（`internal/tools/mcp`）。两条路都空的话，
// 这个 agent 什么都不会——而它**刻意不带任何内置能力**：文档、记忆、检索全在
// MCP server 里，agent 自己不拥有任何数据。
//
// ## 目录可以有多个，后一个覆盖前一个
//
// 默认只读 `<安装根>/skills`，但 `FKA_SKILLS_DIR` 可以给**多个目录**（用系统路径
// 分隔符 `:` 隔开）。扫描按配置顺序，**同名技能后者覆盖前者**——这样一份安装自带的
// 基础技能，可以被用户自己目录里的同名技能顶掉，而不必改安装包。
//
// ## 改完不用重启
//
// 每次取技能都先看一眼目录（名字 + `SKILL.md` 的 mtime）。变了才重新读盘。
// 所以往目录里丢一个新技能，下一轮就能用上——**技能是随时会变的运维对象**，
// 不该逼人重启服务。
//
// ## 没有目录也是正常的
//
// 目录不存在、或一个技能都没有，都返回空，**不报错也不告警**：那只是「这份安装
// 没放技能」，与「技能坏了」不是一回事。
//
// ## 格式
//
// 允许（但不要求）一段极简 front matter：
//
//	---
//	name: 备份照片
//	description: 把手机里的照片归档到 NAS
//	---
//	正文……
//
// **不引 YAML 库**——只认 `key: value` 一行一条，认不出的一律当正文，
// **绝不因为格式不合就丢掉一个技能**。
package skills

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"

	"github.com/zjzhang-cn/fka-go/internal/config"
	"github.com/zjzhang-cn/fka-go/internal/tools"
)

// SkillFile 技能入口文件名。**大小写都要认**：手工放进去的目录不会讲究。
const SkillFile = "SKILL.md"

// 工具名。**刻意不带前缀**——前缀由上层的工具注册表加（`skills__`）。
const (
	ToolList = "list"
	ToolLoad = "load"
)

var listSpec = tools.Spec{
	Name: ToolList,
	Description: "列出这份安装里有哪些技能（名字 + 一句话说明）。" +
		"想确认技能是否存在、或系统提示里的清单拿不准时用它；真正要做的时候再用 load 取全文。",
	Parameters: map[string]any{"type": "object", "properties": map[string]any{}, "required": []any{}},
	Effect:     tools.EffectRead,
}

var loadSpec = tools.Spec{
	Name: ToolLoad,
	Description: "取一个技能的完整说明（操作步骤、注意事项）。" +
		"列表里只有名字与一句话，真正要做的时候先取全文，再按它一步步做。",
	Parameters: map[string]any{
		"type": "object",
		"properties": map[string]any{
			"name": map[string]any{"type": "string", "description": "技能名，见系统提示里列出的可用技能"},
		},
		"required": []any{"name"},
	},
	Effect: tools.EffectRead,
}

// Skill 一个技能。
type Skill struct {
	// Name 显示名。front matter 里的 name，没写就用目录名
	Name string
	// Description 一句话说明。front matter 里的 description，没写就取正文第一行
	Description string
	// Body 正文（不含 front matter）
	Body string
	// Dir 技能目录名。按名字找时也认它——front matter 的名字可能带空格或标点
	Dir string
}

// ResolveDirs 技能目录列表。`FKA_SKILLS_DIR` 可覆盖，**可用系统路径分隔符给多个**；
// 相对路径**按安装根**解析，不按 cwd——与 .env、日志同一条理由
// （fka 是全局命令，cwd 是任意的）。
func ResolveDirs() []string {
	configured := strings.TrimSpace(os.Getenv("FKA_SKILLS_DIR"))
	if configured == "" {
		return []string{filepath.Join(config.Home(), "skills")}
	}

	var dirs []string
	for _, part := range strings.Split(configured, string(os.PathListSeparator)) {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		if filepath.IsAbs(part) {
			dirs = append(dirs, part)
		} else {
			dirs = append(dirs, filepath.Join(config.Home(), part))
		}
	}

	if len(dirs) == 0 {
		return []string{filepath.Join(config.Home(), "skills")}
	}
	return dirs
}

// ParseSkill 解析一个 SKILL.md。纯函数，便于钉住 front matter 的行为。
//
// fallbackName 是目录名——没写 name: 时用它，总比没有名字强。
func ParseSkill(raw string, fallbackName string) Skill {
	text := strings.TrimPrefix(raw, "\ufeff")
	fields, body := readFrontMatter(text)

	name := strings.TrimSpace(fields["name"])
	if name == "" {
		name = fallbackName
	}
	description := strings.TrimSpace(fields["description"])
	if description == "" {
		description = firstLine(body)
	}

	return Skill{Name: name, Description: description, Body: strings.TrimSpace(body), Dir: fallbackName}
}

func readFrontMatter(text string) (map[string]string, string) {
	lines := strings.Split(strings.ReplaceAll(text, "\r\n", "\n"), "\n")

	// front matter 必须以 --- 单独一行开头、以 --- 单独一行结束
	if len(lines) == 0 || strings.TrimSpace(lines[0]) != "---" {
		return map[string]string{}, text
	}

	end := -1
	for i := 1; i < len(lines); i++ {
		if strings.TrimSpace(lines[i]) == "---" {
			end = i
			break
		}
	}
	if end < 0 {
		return map[string]string{}, text
	}

	fields := map[string]string{}
	for _, line := range lines[1:end] {
		at := strings.IndexByte(line, ':')
		if at <= 0 {
			continue
		}
		key := strings.ToLower(strings.TrimSpace(line[:at]))
		fields[key] = strings.TrimSpace(line[at+1:])
	}

	return fields, strings.Join(lines[end+1:], "\n")
}

// firstLine 取正文里第一行有内容、且不是标题/列表符的文字，当作一句话说明。
func firstLine(body string) string {
	for _, line := range strings.Split(strings.ReplaceAll(body, "\r\n", "\n"), "\n") {
		text := strings.TrimSpace(strings.TrimLeft(strings.TrimLeft(strings.TrimLeft(line, "#>-*"), " "), " "))
		if text == "" {
			continue
		}
		if runes := []rune(text); len(runes) > 120 {
			return string(runes[:120]) + "…"
		}
		return text
	}
	return "（这个技能没有写说明）"
}

// normalizeName 名字归一：**大小写与空白都不算差别**。模型可能把「备份照片」写成
// 「备份 照片」或带上大小写，为这个回一句「没有这个技能」太苛刻。
//
// **不做子串匹配**——那会把「报销」错配到「报销流程」上，宁可让模型照候选重叫。
func normalizeName(raw string) string {
	return strings.ToLower(strings.Join(strings.Fields(raw), ""))
}

// source 技能源。
type source struct {
	dirs []string

	mu sync.Mutex
	// 上一次读盘的结果与当时的签名。签名没变就复用，变了才重新读
	loaded    []Skill
	signature string
	haveCache bool
	// warnedDirs 已经告警过的目录，避免每轮扫描都刷同一条日志
	warnedDirs map[string]bool
}

// NewSource 造技能源。dirs 为空时按 FKA_SKILLS_DIR 解析。
func NewSource(dirs []string) tools.Source {
	if len(dirs) == 0 {
		dirs = ResolveDirs()
	}
	return &source{dirs: dirs, warnedDirs: map[string]bool{}}
}

func (s *source) ID() string    { return "skills" }
func (s *source) Label() string { return "技能" }

// scan 看一眼每个目录有哪些技能、各自 SKILL.md 的 mtime——**这就是「变没变」的全部依据**。
func (s *source) scan() [][2]any {
	result := make([][2]any, 0, len(s.dirs))

	for _, dir := range s.dirs {
		entries, err := os.ReadDir(dir)
		if err != nil {
			// 目录不存在是**正常状态**（这份安装没放技能）；别的错误才值得看一眼，且只看一次
			if !os.IsNotExist(err) && !s.warnedDirs[dir] {
				s.warnedDirs[dir] = true
				config.Log().Warn("技能目录读不了，按没有技能处理",
					config.Context{"dir": dir, "error": err.Error()})
			}
			continue
		}

		names := make([]string, 0, len(entries))
		for _, entry := range entries {
			if entry.IsDir() {
				names = append(names, entry.Name())
			}
		}
		sort.Strings(names)
		result = append(result, [2]any{dir, names})
	}

	return result
}

// safeMtime 读不到 mtime 就当没变——**读不到不影响名字本身的变化**
func safeMtime(path string) int64 {
	info, err := os.Stat(path)
	if err != nil {
		return 0
	}
	return info.ModTime().UnixMilli()
}

func (s *source) signatureOf(scanned [][2]any) string {
	var parts []string
	for _, item := range scanned {
		dir, _ := item[0].(string)
		names, _ := item[1].([]string)
		for _, name := range names {
			parts = append(parts, dir+"\x00"+name+"\x00"+
				itoa(safeMtime(filepath.Join(dir, name, SkillFile))))
		}
	}
	return strings.Join(parts, "\x01")
}

func (s *source) load() []Skill {
	scanned := s.scan()
	signatureNow := s.signatureOf(scanned)

	if s.haveCache && signatureNow == s.signature {
		return s.loaded
	}

	// 按归一后的名字去重：后一个目录的同名技能覆盖前一个
	byKey := map[string]Skill{}
	for _, item := range scanned {
		dir, _ := item[0].(string)
		names, _ := item[1].([]string)
		for _, name := range names {
			raw, err := os.ReadFile(filepath.Join(dir, name, SkillFile))
			if err != nil {
				// 一个技能读不了，不能把别的技能一起带走
				config.Log().Warn("技能读不了，已跳过",
					config.Context{"skill": dir + "/" + name, "error": err.Error()})
				continue
			}
			skill := ParseSkill(string(raw), name)
			key := normalizeName(skill.Name)
			if key == "" {
				key = normalizeName(name)
			}
			byKey[key] = skill
		}
	}

	// 按名字排一遍，让系统提示里的清单顺序稳定——否则 map 的随机顺序会让
	// 每轮拼出来的提示词前缀不同，**provider 的前缀缓存就命中不了**
	keys := make([]string, 0, len(byKey))
	for key := range byKey {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	loaded := make([]Skill, 0, len(keys))
	for _, key := range keys {
		loaded = append(loaded, byKey[key])
	}

	s.loaded = loaded
	s.signature = signatureNow
	s.haveCache = true
	return loaded
}

func (s *source) List(ctx context.Context, tc tools.Context) ([]tools.Spec, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	// **一个技能都没有时返回空**——不声明 list/load。空不等于错，但让模型去调
	// 一个必然失败的工具是白烧一轮
	if len(s.load()) == 0 {
		return nil, nil
	}
	return []tools.Spec{listSpec, loadSpec}, nil
}

func (s *source) PromptSection(ctx tools.Context) (string, error) {
	s.mu.Lock()
	skills := s.load()
	s.mu.Unlock()

	if len(skills) == 0 {
		return "", nil
	}

	lines := []string{"可用技能（需要时用 skills__list 看清单、skills__load 取全文后再照做）："}
	for _, skill := range skills {
		lines = append(lines, "- "+skill.Name+"："+skill.Description)
	}
	return strings.Join(lines, "\n"), nil
}

func (s *source) Call(ctx context.Context, name string, args map[string]any, tc tools.Context) (tools.Result, error) {
	if name != ToolList && name != ToolLoad {
		// 注册表只按声明分发，走到这里说明声明与实现不同步——那是代码缺陷
		return tools.FailResult("技能源没有实现 %s", name), nil
	}

	s.mu.Lock()
	skills := s.load()
	s.mu.Unlock()

	if len(skills) == 0 {
		return tools.FailResult("这份安装里没有技能。"), nil
	}

	if name == ToolList {
		return tools.OKResult(marshalSkills(skills)), nil
	}

	wanted, _ := args["name"].(string)
	wanted = strings.TrimSpace(wanted)
	found := findSkill(skills, wanted)
	if found == nil {
		names := make([]string, 0, len(skills))
		for _, skill := range skills {
			names = append(names, skill.Name)
		}
		return tools.FailResult("没有叫「%s」的技能。可用的有：%s", wanted, strings.Join(names, "、")), nil
	}

	return tools.OKResult(`{"name":` + quoteJSON(found.Name) + `,"body":` + quoteJSON(found.Body) + `}`), nil
}

func (s *source) Close() error { return nil }

// findSkill 按名字找技能：front matter 的名字与目录名都认，大小写/空白归一。
func findSkill(skills []Skill, wanted string) *Skill {
	key := normalizeName(wanted)
	if key == "" {
		return nil
	}
	for i := range skills {
		if normalizeName(skills[i].Name) == key || normalizeName(skills[i].Dir) == key {
			return &skills[i]
		}
	}
	return nil
}

func marshalSkills(skills []Skill) string {
	parts := make([]string, 0, len(skills))
	for _, skill := range skills {
		parts = append(parts, `{"name":`+quoteJSON(skill.Name)+`,"description":`+quoteJSON(skill.Description)+`}`)
	}
	return "[" + strings.Join(parts, ",") + "]"
}

// quoteJSON 编码一个字符串值。手写转义就要枚举规则，而这里只是给模型看的一行 JSON。
func quoteJSON(s string) string {
	encoded, err := json.Marshal(s)
	if err != nil {
		return `""`
	}
	return string(encoded)
}

func itoa(n int64) string {
	if n == 0 {
		return "0"
	}
	digits := make([]byte, 0, 20)
	for n > 0 {
		digits = append([]byte{byte('0' + n%10)}, digits...)
		n /= 10
	}
	return string(digits)
}
