package config_test

import (
	"context"
	"testing"

	"github.com/zjzhang-cn/fka-go/internal/config"
)

// Test绑上去的字段被自动合并 这条是整件事的地基：消息层绑一次，底下每一层
// 打日志时 `config.Fields(ctx, …)` 就自动带上账号——**不用每处手写**。
func Test绑上去的字段被自动合并(t *testing.T) {
	ctx := config.Bind(context.Background(), config.Context{
		"account": "account_002", "messageId": "m-1",
	})

	fields := config.Fields(ctx, config.Context{"tool": "skills__load"})
	if fields["account"] != "account_002" {
		t.Errorf("绑定的 account 该自动带上，实际 %v", fields["account"])
	}
	if fields["messageId"] != "m-1" {
		t.Errorf("绑定的 messageId 该自动带上，实际 %v", fields["messageId"])
	}
	if fields["tool"] != "skills__load" {
		t.Errorf("调用点自己给的字段该在，实际 %v", fields["tool"])
	}
}

// Test没绑过就只给调用点的字段 别让「自动合并」变成往每条日志里塞一堆空字段
func Test没绑过就只给调用点的字段(t *testing.T) {
	fields := config.Fields(context.Background(), config.Context{"tool": "x"})
	if len(fields) != 1 || fields["tool"] != "x" {
		t.Errorf("该只有调用点给的那一个字段，实际 %v", fields)
	}
	if config.FieldsOf(context.Background()) != nil {
		t.Error("没绑过的 ctx 该返回 nil")
	}
}

// Test两边都空就返回nil Logger.write 会按「字段为空」处理，返回空 map 也能工作，
// 但那样每条进程级日志都会多出一段 `"context":{}`——噪音，且掩盖了
// 「这一条确实没有归属」这个事实
func Test两边都空就返回nil(t *testing.T) {
	if got := config.Fields(context.Background(), nil); got != nil {
		t.Errorf("该返回 nil，实际 %v", got)
	}
	if got := config.Fields(context.Background(), config.Context{}); got != nil {
		t.Errorf("该返回 nil，实际 %v", got)
	}
}

// Test派生出去的ctx也带着 真实链路是「绑一次，往下传」，所以派生必须保留
func Test派生出去的ctx也带着(t *testing.T) {
	base := config.Bind(context.Background(), config.Context{"account": "acct-1"})

	derived := context.WithValue(base, struct{ k string }{"别的键"}, "别的值")
	if config.FieldsOf(derived)["account"] != "acct-1" {
		t.Error("派生出去的 ctx 该保留绑定的字段")
	}

	// WithCancel / WithTimeout 这类也一样——真实代码里到处在用
	cancelCtx, cancel := context.WithCancel(base)
	defer cancel()
	if config.FieldsOf(cancelCtx)["account"] != "acct-1" {
		t.Error("WithCancel 之后该保留绑定的字段")
	}
}

// Test后绑的覆盖先绑的 同一轮里一层层往下传，每层都可能补充信息；
// **越晚绑的越具体**，该赢
func Test后绑的覆盖先绑的(t *testing.T) {
	base := config.Bind(context.Background(), config.Context{"account": "acct-1", "step": 0})
	deeper := config.Bind(base, config.Context{"step": 3})

	fields := config.FieldsOf(deeper)
	if fields["step"] != 3 {
		t.Errorf("后绑的该覆盖先绑的，实际 %v", fields["step"])
	}
	if fields["account"] != "acct-1" {
		t.Errorf("没被覆盖的该保留，实际 %v", fields["account"])
	}
}

// Test调用点的字段赢过绑定的 同一轮里更具体的信息（这一步的工具名）该盖过通用的
func Test调用点的字段赢过绑定的(t *testing.T) {
	ctx := config.Bind(context.Background(), config.Context{"tool": "通用的"})

	fields := config.Fields(ctx, config.Context{"tool": "skills__load"})
	if fields["tool"] != "skills__load" {
		t.Errorf("调用点给的该赢，实际 %v", fields["tool"])
	}
}

// Test合并不会改到绑定的原值 **合并出一份新的**——绑在 ctx 上的那份是所有下游
// 共享的，谁原地改它，谁就把别人的日志字段也改了
func Test合并不会改到绑定的原值(t *testing.T) {
	ctx := config.Bind(context.Background(), config.Context{"account": "acct-1"})

	merged := config.Fields(ctx, config.Context{"tool": "x", "step": 1})
	merged["account"] = "被改了"
	merged["新键"] = "只有这份有"

	bound := config.FieldsOf(ctx)
	if bound["account"] != "acct-1" {
		t.Errorf("绑定的原值不该被改，实际 %v", bound["account"])
	}
	if _, leaked := bound["新键"]; leaked {
		t.Error("合并出的新键漏回了绑定的那份")
	}
}

// Test空字段不绑 绑一个空集合只会白白多一层 context.Value
func Test空字段不绑(t *testing.T) {
	base := context.Background()
	if config.Bind(base, nil) != base {
		t.Error("绑空字段该原样返回 ctx")
	}
	if config.Bind(base, config.Context{}) != base {
		t.Error("绑空 map 该原样返回 ctx")
	}
}

// Test空指针的ctx不崩 边界上不该为了一个 nil 就 panic——日志路径上一次 panic
// 会把整轮问答带走
func Test空指针的ctx不崩(t *testing.T) {
	if config.FieldsOf(nil) != nil {
		t.Error("nil ctx 该返回 nil")
	}
	if config.Fields(nil, config.Context{"a": 1})["a"] != 1 {
		t.Error("nil ctx 也该能用")
	}
	// 绑到 nil ctx 上不该崩
	if got := config.FieldsOf(config.Bind(nil, config.Context{"a": 1})); got["a"] != 1 {
		t.Error("往 nil ctx 上绑字段该能绑上")
	}
}
