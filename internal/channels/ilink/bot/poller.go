package bot

import (
	"context"
	"errors"
	"net"
	"net/http"
	"time"
)

// isTimeout 判断一个错误是不是「等太久」。
//
// **长轮询本来就该在服务端挂起时挂着**，所以超时不是错误——分不出来的话，
// 每一次正常的长轮询都会被记成一次错误，几小时后日志就没法看了。
//
// 用 `errors.Is(err, context.DeadlineExceeded)` 与「net.Error.Timeout()」两条：
// 我们的 ctx 超时走前者，传输层的读超时走后者。
func isTimeout(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return true
	}
	var netErr net.Error
	return errors.As(err, &netErr) && netErr.Timeout()
}

// iLink 长轮询。
//
// ## Go 版比 Node 版简单一整层
//
// Node 用 `worker_threads` 把每个账号的轮询扔进独立线程，再由主进程管理
// worker 的生死与崩溃重启（`MultiAccountPoller`，300 行）。Go 里 **goroutine 就是
// 那个 worker**，于是「崩溃重启」这件事整个消失了：
//
//   - 暂时性失败（网络抖动、服务端 5xx）→ **循环内退避后重试**，不退出；
//   - session 过期（`ret=-14`）→ 凭证真的死了，循环退出并上报，等重新登录。
//
// 剩下的唯一「重启」是**换了新凭证之后**，而那由渠道适配层显式调 Restart 触发。
//
// ## 游标推进的顺序：先落盘，再上抛消息
//
// 万一上抛途中进程被杀，游标已持久化——**宁可重放（幂等可容忍）也不要漏消费**。
// 反过来的话就是静默丢消息，而那种错极难查。
//
// 即使这一轮没有消息也要推进：服务端每轮都会返回新的 buf，只在有消息时推进
// 会导致重复拉取同一区间。

// 长轮询超时。**必须大于服务端的挂起时间**，否则我们会在服务端还没准备好
// 答案时就断开，然后立刻重连——那等于把长轮询退化成短轮询狂刷。
const pollTimeout = 35 * time.Second

// pollIdleGap 两轮之间的间隔。短暂停顿，避免服务端刚回就立刻再问。
const pollIdleGap = time.Second

// Poller 一个账号的轮询器。
type Poller struct {
	account WeixinAccount
	client  *client
	cursors *CursorStore

	// OnMessage 收到一条消息
	OnMessage func(WeixinMessage)
	// OnError 一轮失败。**只是上报**，循环会继续
	OnError func(error)
	// OnSessionExpired 凭证失效。循环随即退出
	OnSessionExpired func()

	cancel context.CancelFunc
	done   chan struct{}
}

// NewPoller 造一个账号的轮询器。httpClient 为 nil 时用默认实现。
func NewPoller(account WeixinAccount, cursors *CursorStore, httpClient *http.Client) *Poller {
	return &Poller{
		account: account,
		client:  NewClient(account, httpClient),
		cursors: cursors,
		done:    make(chan struct{}),
	}
}

// Start 开始轮询。**幂等**：已经在跑时什么也不做。
//
// 返回的 cancel 用于停机；停机后 goroutine 一定已经退出（`Wait` 返回）。
func (p *Poller) Start(ctx context.Context) {
	if p.cancel != nil {
		return
	}
	// 父 ctx 取消时整条链一起停——这是「关服务」与「停这个账号」的共同入口
	ctx, p.cancel = context.WithCancel(ctx)

	go func() {
		defer close(p.done)
		p.loop(ctx)
	}()
}

// Wait 等轮询 goroutine 退出。
func (p *Poller) Wait() { <-p.done }

// Stop 停机。**幂等**，且会等 goroutine 真的退出。
//
// 等是有意义的：直接返回的话，调用方紧接着关掉 HTTP 传输层，
// 而长轮询还挂在那儿等着应答——那会产生一堆看不懂的连接错误。
func (p *Poller) Stop() {
	if p.cancel == nil {
		return
	}
	p.cancel()
	p.Wait()
	p.cancel = nil
}

// loop 轮询主循环。
func (p *Poller) loop(ctx context.Context) {
	backoff := DefaultBackoff()

	for {
		if ctx.Err() != nil {
			return
		}

		err := p.pollOnce(ctx, backoff)
		if err == nil {
			backoff.Reset()
		}

		// 凭证失效是**唯一一种要退出循环的失败**
		if errors.Is(err, errSessionExpired) {
			if p.OnSessionExpired != nil {
				p.OnSessionExpired()
			}
			return
		}

		select {
		case <-ctx.Done():
			return
		case <-time.After(pollIdleGap):
		}
	}
}

// errSessionExpired 凭证失效的哨兵错误。
var errSessionExpired = errors.New("session 过期，需重新登录")

// pollOnce 跑一轮 getupdates。
func (p *Poller) pollOnce(ctx context.Context, backoff *Backoff) error {
	data, err := p.client.postJSON(ctx, "/ilink/bot/getupdates", struct {
		GetUpdatesBuf string   `json:"get_updates_buf"`
		BaseInfo      baseInfo `json:"base_info"`
	}{p.cursors.Get(p.account.ID), baseInfo{ChannelVersion}}, pollTimeout)

	if err != nil {
		// **超时不是错误**：长轮询本来就该在服务端挂起时挂着。区分不出来时
		// 会把每一次正常的长轮询都记成错误，几小时后日志就没法看了
		if ctx.Err() != nil {
			return nil // 停机导致的失败，不算
		}
		if isTimeout(err) {
			return nil
		}
		if p.OnError != nil {
			p.OnError(err)
		}
		// 网络层失败：退避后重试，**不退出**
		sleepCtx(ctx, backoff.Next())
		return nil
	}

	batch, err := ParseGetUpdates(data, p.account.ID)
	if err != nil {
		if p.OnError != nil {
			p.OnError(err)
		}
		// 协议层失败（ret 非零）：同样是暂时的，退避后重试
		sleepCtx(ctx, backoff.Next())
		return nil
	}

	if batch.SessionExpired {
		// **游标要清掉**：留着旧游标的话，重新登录后服务端会以为客户端已消费到
		// 那一段，于是那段时间的消息永久收不到
		if p.cursors != nil {
			p.cursors.Clear(p.account.ID)
		}
		return errSessionExpired
	}

	// **先落盘游标，再上抛消息**
	if batch.Cursor != "" && batch.Cursor != p.cursors.Get(p.account.ID) {
		p.cursors.Set(p.account.ID, batch.Cursor)
	}

	for _, message := range batch.Messages {
		if p.OnMessage != nil {
			p.OnMessage(message)
		}
	}
	return nil
}

// sleepCtx 睡一会儿，但**响应 ctx 取消**。
//
// 退避最长 60s，而停机不该等那么久。
func sleepCtx(ctx context.Context, d time.Duration) {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
	case <-timer.C:
	}
}
