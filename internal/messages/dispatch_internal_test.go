package messages

import (
	"testing"

	"github.com/zjzhang-cn/fka-go/internal/channels"
)

// Test分片键带渠道种类 AccountID 只在渠道内唯一，而渠道种类之间也可能撞号——
// 接缝自己的查找键 `key(channelID, accountID)` 就是因为这个才带渠道的。
//
// 分片键要是只按账号切，两个渠道种类的同号账号会被塞进**同一个 worker**，
// 于是它们并发跑、各自基于同一份旧历史作答，然后交叉写进同一个历史文件。
// 症状是「历史偶尔错乱」，不报错。
//
// 这条放在包内测，因为 `shardKeyOf` 不导出——**它是实现细节，业务代码不该开始
// 依赖它**，但形状得有人钉住。
func Test分片键带渠道种类(t *testing.T) {
	first := shardKeyOf(channels.Event{
		Message: channels.InboundMessage{ChannelID: "ilink", AccountID: "account_002"},
	})
	second := shardKeyOf(channels.Event{
		Message: channels.InboundMessage{ChannelID: "memory", AccountID: "account_002"},
	})

	if first != "ilink:account_002" {
		t.Errorf("分片键该是 渠道:账号（身份为空时不缀尾），实际 %q", first)
	}
	if first == second {
		t.Errorf("两个渠道种类的同号账号不该撞进同一个分片：%q", first)
	}

	// 同一渠道的同一账号该稳定落在同一个分片——否则每条消息一个新 worker
	again := shardKeyOf(channels.Event{
		Message: channels.InboundMessage{ChannelID: "ilink", AccountID: "account_002"},
	})
	if again != first {
		t.Errorf("同一账号的分片键该稳定：%q vs %q", first, again)
	}
}

// Test分片键带身份 单账号渠道（web 的所有 token 共用一个 AccountID）里，
// **两个身份必须落进两个分片**，否则它们被塞进同一个 worker 串行执行——
// 症状是「两个 token 也并行不起来」。
//
// 反过来也要钉：同一账号同一身份必须稳定在同一分片，否则同一路会话的历史
// 会被两个 worker 交叉写。
func Test分片键带身份(t *testing.T) {
	alice := shardKeyOf(channels.Event{
		Message: channels.InboundMessage{ChannelID: "web", AccountID: "default", PrincipalID: "web:alice"},
	})
	bob := shardKeyOf(channels.Event{
		Message: channels.InboundMessage{ChannelID: "web", AccountID: "default", PrincipalID: "web:bob"},
	})
	aliceAgain := shardKeyOf(channels.Event{
		Message: channels.InboundMessage{ChannelID: "web", AccountID: "default", PrincipalID: "web:alice"},
	})

	if alice == bob {
		t.Fatalf("同账号下两个身份不该撞进同一个分片：%q", alice)
	}
	if alice != aliceAgain {
		t.Errorf("同一身份的分片键该稳定：%q vs %q", alice, aliceAgain)
	}
	if alice != "web:default:web:alice" {
		t.Errorf("分片键该是 渠道:账号:身份，实际 %q", alice)
	}
}
