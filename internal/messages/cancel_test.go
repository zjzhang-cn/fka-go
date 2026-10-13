package messages_test

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/zjzhang-cn/fka-go/internal/agent"
	"github.com/zjzhang-cn/fka-go/internal/channels"
	"github.com/zjzhang-cn/fka-go/internal/messages"
)

// blockingRunner 一轮会一直挂着直到 ctx 被取消——用来验「停止」真的能掐断。
type blockingRunner struct {
	started chan struct{}
	once    sync.Once
}

func (b *blockingRunner) Run(ctx context.Context, _ agent.RunnerInput) (agent.RunResult, error) {
	b.once.Do(func() { close(b.started) })
	<-ctx.Done()
	return agent.RunResult{}, ctx.Err()
}

func hasTurnState(notices []channels.Notice, running bool) bool {
	for _, notice := range notices {
		if state, ok := notice.(channels.TurnStateNotice); ok && state.Running == running {
			return true
		}
	}
	return false
}

// Test取消_能停掉进行中的一轮 网页「停止」按钮最终走的就是 `CancelTurn`：取消这一轮
// 的 ctx → 工具循环返回 ctx 错 → 回一句平静的「（已停止）」，而不是把它当成失败。
// 顺带钉住执行中状态：进 Handle 发 running=true、出 Handle 发 running=false。
func Test取消_能停掉进行中的一轮(t *testing.T) {
	runner := &blockingRunner{started: make(chan struct{})}
	handler := messages.NewHandler(runner)
	channel := &fakeChannel{accountID: "acct-1"}
	message := textMessage("长问题")

	done := make(chan struct{})
	go func() {
		handler.Handle(context.Background(), channels.Event{Channel: channel, Message: message})
		close(done)
	}()

	select {
	case <-runner.started:
	case <-time.After(2 * time.Second):
		t.Fatal("一轮没跑起来")
	}

	if !hasTurnState(channel.noticesOf(), true) {
		t.Errorf("该先发一条「执行中」通知：%+v", channel.noticesOf())
	}

	if !handler.CancelTurn(channels.SessionKeyOf(&message)) {
		t.Fatal("该取消成功")
	}

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("取消后 Handle 没返回——ctx 没传到工具循环")
	}

	texts := channel.sentTexts()
	if len(texts) != 1 || !strings.Contains(texts[0].text, "已停止") {
		t.Errorf("该回一句「已停止」，实际 %+v", texts)
	}
	if !hasTurnState(channel.noticesOf(), false) {
		t.Errorf("该在结束后发一条「已结束」通知：%+v", channel.noticesOf())
	}
}

// Test取消_没有进行中时返回false 没在跑就如实说没有——前端据此决定要不要把键切回去，
// 不假装成功。
func Test取消_没有进行中时返回false(t *testing.T) {
	handler := messages.NewHandler(&blockingRunner{started: make(chan struct{})})
	if handler.CancelTurn("fake:acct-1:c1") {
		t.Error("没有在跑时该返回 false")
	}
}
