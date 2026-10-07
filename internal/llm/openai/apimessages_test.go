package openai

import (
	"testing"

	"github.com/zjzhang-cn/fka-go/internal/llm"
)

// 一个 role 各自该长成什么样。
//
// ## 这组用例钉的是什么
//
// `toAPIMessages` 是**每一轮模型请求都要过**的翻译层，而它以前只有三条分支：
// 「assistant 且带 tool_calls」「tool」「system」，剩下的全落进 `default` 发成
// `role=user`。于是每一轮的最终答案（就是那条**不带** tool_calls 的 assistant）
// 在下一轮请求里冒充用户说话。
//
// 它不报错——provider 照单全收，模型看到的是「用户连着说了两句」，表现只有
// 回答质量悄悄变差，所以光靠跑通是查不出来的。**翻译层的用例必须逐个 role 断言**，
// 不能只验「请求发出去了」。
func TestAPI消息翻译_四种角色各有各的分支(t *testing.T) {
	cases := []struct {
		name     string
		in       llm.ChatMessage
		wantRole string
	}{
		{
			name:     "user 还是 user",
			in:       llm.ChatMessage{Role: llm.RoleUser, Content: "去年三亚怎么样"},
			wantRole: "user",
		},
		{
			name:     "不带工具调用的 assistant 是上一轮的答案，不能发成 user",
			in:       llm.ChatMessage{Role: llm.RoleAssistant, Content: "查到了，去年的事。"},
			wantRole: "assistant",
		},
		{
			name:     "system 还是 system",
			in:       llm.ChatMessage{Role: llm.RoleSystem, Content: "你是一个助手"},
			wantRole: "system",
		},
		{
			name:     "tool 结果还是 tool",
			in:       llm.ChatMessage{Role: llm.RoleTool, Content: "查到 3 条", ToolCallID: "call_1"},
			wantRole: "tool",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := toAPIMessages([]llm.ChatMessage{tc.in})
			if len(got) != 1 {
				t.Fatalf("该原样转出 1 条，实际 %d 条", len(got))
			}
			if got[0].Role != tc.wantRole {
				t.Errorf("role = %q，期望 %q（内部 %q → SDK 角色串必须逐字对上）",
					got[0].Role, tc.wantRole, tc.in.Role)
			}
			if got[0].Content != tc.in.Content {
				t.Errorf("content 被改写了：%q → %q，历史必须逐字重放",
					tc.in.Content, got[0].Content)
			}
		})
	}
}

// TestAPI消息翻译_多轮历史整段不串位 按**真实的第二轮请求**拼一段历史：
// 上一轮的 user → assistant(带工具) → tool → assistant(答案)，再加这一轮的
// user。逐条验角色，因为「哪一句是模型说的」错位时 API 一声不吭。
func TestAPI消息翻译_多轮历史整段不串位(t *testing.T) {
	history := []llm.ChatMessage{
		{Role: llm.RoleUser, Content: "查一下销量"},
		{Role: llm.RoleAssistant, Content: "先查工具", ToolCalls: []llm.ToolCall{{
			ID: "call_1", Name: "mcp__memory__search_memories", Arguments: `{"query":"销量"}`,
		}}},
		{Role: llm.RoleTool, ToolCallID: "call_1", Content: "3 条"},
		{Role: llm.RoleAssistant, Content: "去年 1200 台"},
		{Role: llm.RoleUser, Content: "今年呢"},
	}
	wantRoles := []string{"user", "assistant", "tool", "assistant", "user"}

	got := toAPIMessages(history)
	if len(got) != len(wantRoles) {
		t.Fatalf("条数 = %d，期望 %d", len(got), len(wantRoles))
	}
	for i, want := range wantRoles {
		if got[i].Role != want {
			t.Errorf("第 %d 条 role = %q，期望 %q（整段串位的话模型会把上一轮答案当成用户新说的）",
				i+1, got[i].Role, want)
		}
	}

	// 带工具调用那条：id、函数名、**原始参数字符串**都要原样过去
	call := got[1].ToolCalls
	if len(call) != 1 {
		t.Fatalf("tool_calls 该有 1 条，实际 %d 条", len(call))
	}
	if call[0].ID != "call_1" || call[0].Function.Name != "mcp__memory__search_memories" {
		t.Errorf("工具标识被改写：%+v", call[0])
	}
	if call[0].Function.Arguments != `{"query":"销量"}` {
		t.Errorf("参数必须是原始 JSON 字符串，实际 %q", call[0].Function.Arguments)
	}

	// tool 结果要带上它对应的 call id，否则 provider 认不出配对
	if got[2].ToolCallID != "call_1" {
		t.Errorf("tool 消息的 tool_call_id = %q，期望 call_1", got[2].ToolCallID)
	}
}

// TestAPI消息翻译_带工具调用的assistant不发空content OpenAI 规定带 tool_calls
// 时 content 可以是 null 但**不能是空串**；SDK 的 omitempty 把空串删掉正好得到
// 「没有这个字段」。这条钉住那次改写没有把两个分支搞混。
func TestAPI消息翻译_带工具调用的assistant不发空content(t *testing.T) {
	got := toAPIMessages([]llm.ChatMessage{{
		Role: llm.RoleAssistant, Content: "",
		ToolCalls: []llm.ToolCall{{ID: "call_1", Name: "skills__load", Arguments: "{}"}},
	}})

	if len(got) != 1 {
		t.Fatalf("该转出 1 条，实际 %d 条", len(got))
	}
	if got[0].Role != "assistant" {
		t.Errorf("role = %q，期望 assistant", got[0].Role)
	}
	if got[0].Content != "" {
		t.Errorf("content 该保持空串（交给 omitempty 删字段），实际 %q", got[0].Content)
	}
	if len(got[0].ToolCalls) != 1 {
		t.Errorf("tool_calls 该原样带上，实际 %d 条", len(got[0].ToolCalls))
	}
}
