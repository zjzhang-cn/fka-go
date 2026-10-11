// Package history 把「清空当前会话的历史」做成工具，交给模型调度。
//
// ## 为什么这是**第二个**内置工具源（第一个是 reply）
//
// 这个 agent 的原则是「不带任何内置能力，能力只从 MCP 与 skill 进来」。但**会话历史
// 是它自己拥有并落盘的状态**（`data/history/*.jsonl`，见 internal/llm/session.go），
// 不是一项外部能力——用户说「清空历史 / 忘掉前面说的」，模型需要一个动作把这份本地
// 状态删掉，否则旧对话会一直被逐字重放。它不拥有外部数据、不出网、不拉进程，和 reply
// 一样动的是 agent 自己的东西，所以没有破坏那条原则。
//
// ## 它删的是「哪一段」，不是「里面写了什么」
//
// 会话标识经 `tools.Context.SessionID` 从消息层传来（逐条消息的事实，见 RunnerInput），
// 工具只认这一个 id。这样它不需要、也拿不到会话内容——清空是一个「动自身状态」的动作，
// 不是一次查询。
//
// ## 默认关着
//
// effect 是 `delete`，而 `LLM_TOOL_EFFECTS` 默认只有 `read`——不给 `delete` 就看不到
// 这个工具。这与「删了就没了」的取舍一致：清空历史不可撤销，是部署方要显式打开的。
package history

import (
	"context"
	"strings"

	"github.com/zjzhang-cn/fka-go/internal/llm"
	"github.com/zjzhang-cn/fka-go/internal/tools"
)

// 工具名。**刻意不带前缀**——前缀由注册表加（`history__`）。
const ToolClear = "clear"

// SourceID 源前缀，出现在工具名里（`history__clear`）。
const SourceID = "history"

var clearSpec = tools.Spec{
	Name: ToolClear,
	Description: "清空当前会话的历史记录。用户明确要求「清空历史 / 忘掉之前的对话 / 重新开始」时调用；" +
		"此后不会再带上前面的对话内容。**不可撤销**。",
	Parameters: map[string]any{
		"type":       "object",
		"properties": map[string]any{},
	},
	Effect: tools.EffectDelete,
}

// source 会话历史工具源。store 是 agent 落盘会话历史的那份存储（组装根注入）。
type source struct {
	store llm.SessionHistoryStore
}

// NewSource 造源。store 为 nil 时**不要注册这个源**（工具声明了却跑不了比不声明更糟，
// 见组装根）；这里仍接受 nil 并让 Call 回一句，作为最后一道防线。
func NewSource(store llm.SessionHistoryStore) tools.Source {
	return &source{store: store}
}

func (s *source) ID() string    { return SourceID }
func (s *source) Label() string { return "会话历史" }

func (s *source) List(ctx context.Context, tc tools.Context) ([]tools.Spec, error) {
	return []tools.Spec{clearSpec}, nil
}

// PromptSection 没有额外说明：工具声明本身已经说清了用途，不需要往 system 里加字。
func (s *source) PromptSection(tc tools.Context) (string, error) { return "", nil }

func (s *source) Close() error { return nil }

func (s *source) Call(ctx context.Context, name string, args map[string]any, tc tools.Context) (tools.Result, error) {
	if name != ToolClear {
		// 注册表只按声明分发，走到这里说明声明与实现不同步——那是代码缺陷
		return tools.FailResult("会话历史源没有实现 %s", name), nil
	}
	if s.store == nil {
		return tools.FailResult("当前没有启用会话历史，无法清空。"), nil
	}
	if strings.TrimSpace(tc.SessionID) == "" {
		return tools.FailResult("当前没有会话，无法清空历史。"), nil
	}
	if err := s.store.Clear(tc.SessionID); err != nil {
		return tools.FailResult("清空历史失败：%s", err.Error()), nil
	}
	// 带上标记：循环据此**不落盘本轮**，否则这轮又会把「清空历史」这句写回去。
	result := tools.OKResult("已清空当前会话的历史，之后不会再带上前面的对话。")
	result.ClearedHistory = true
	return result, nil
}
