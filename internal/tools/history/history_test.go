package history

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/zjzhang-cn/fka-go/internal/llm"
	"github.com/zjzhang-cn/fka-go/internal/tools"
)

// fakeStore 只记下 Clear 被谁调用，Load/Append 不参与这条链路。
type fakeStore struct {
	cleared []string
	err     error
}

func (f *fakeStore) Load(string) []llm.ChatMessage    { return nil }
func (f *fakeStore) Append(string, []llm.ChatMessage) {}
func (f *fakeStore) Clear(sessionID string) error {
	f.cleared = append(f.cleared, sessionID)
	return f.err
}

func call(t *testing.T, store llm.SessionHistoryStore, tc tools.Context) tools.Result {
	t.Helper()
	res, err := NewSource(store).Call(context.Background(), ToolClear, map[string]any{}, tc)
	if err != nil {
		t.Fatal(err)
	}
	return res
}

// TestHistory_清空的是当前会话 清空只认 `Context.SessionID`——它逐条消息从消息层
// 传来。用错会话（清到别人头上）是这里最坏的错，所以钉住「清了哪一个」。
func TestHistory_清空的是当前会话(t *testing.T) {
	store := &fakeStore{}
	res := call(t, store, tools.Context{SessionID: "web:acct:room1"})

	if !res.OK {
		t.Fatalf("该成功，实际：%s", res.Content)
	}
	if len(store.cleared) != 1 || store.cleared[0] != "web:acct:room1" {
		t.Errorf("该清空当前会话，实际清了 %v", store.cleared)
	}
	// 标记必须带上：循环据此不落盘本轮，否则「清空历史」这句又被写回去。
	if !res.ClearedHistory {
		t.Error("成功清空时该带上 ClearedHistory 标记")
	}
}

// TestHistory_没有会话要拒 没有 SessionID（比如单发路径拼出来的 Context）时不能
// 瞎清：报一句而不是清掉一个空/错误的名字。
func TestHistory_没有会话要拒(t *testing.T) {
	store := &fakeStore{}
	res := call(t, store, tools.Context{})

	if res.OK {
		t.Error("没有会话时不该成功")
	}
	if len(store.cleared) != 0 {
		t.Errorf("没有会话时不该调用 Clear，实际 %v", store.cleared)
	}
}

// TestHistory_未启用历史要拒 SESSION_HISTORY=0 时 store 为 nil，清空无意义。
func TestHistory_未启用历史要拒(t *testing.T) {
	res := call(t, nil, tools.Context{SessionID: "s"})
	if res.OK {
		t.Error("没有历史存储时不该成功")
	}
}

// TestHistory_清空失败要如实说 存储报错时必须把原因带回去，不能假装成功——
// 静默的「已清空」会让用户以为旧数据没了，实际还在。
func TestHistory_清空失败要如实说(t *testing.T) {
	store := &fakeStore{err: errors.New("磁盘满了")}
	res := call(t, store, tools.Context{SessionID: "s"})

	if res.OK {
		t.Fatal("存储报错时不该成功")
	}
	if !strings.Contains(res.Content, "磁盘满了") {
		t.Errorf("该把失败原因告知模型，实际：%s", res.Content)
	}
}

// TestHistory_是delete类 清空不可撤销，归在 delete 类；默认策略只有 read，所以默认
// 看不见它——这条钉住「要显式打开 delete 才会暴露」。
func TestHistory_是delete类(t *testing.T) {
	specs, err := NewSource(&fakeStore{}).List(context.Background(), tools.Context{})
	if err != nil {
		t.Fatal(err)
	}
	if len(specs) != 1 || specs[0].Effect != tools.EffectDelete {
		t.Fatalf("该是一个 delete 类工具，实际 %+v", specs)
	}
	if tools.DefaultPolicy().Allows(specs[0].Effect) {
		t.Error("默认策略不该放行 delete：清空历史必须由部署方显式打开")
	}
}
