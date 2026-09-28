package bot

import (
	"math"
	"math/rand"
	"time"
)

// 指数退避。
//
// 用于轮询失败后的等待、崩溃重启等场景：每次失败把延迟翻倍，到达上限后保持；
// 一旦成功，调用方应 Reset()，使下一次失败从初始延迟重新开始。
//
// ## 为什么必须有上限
//
// 没有上限的话，服务端挂 10 分钟就会让重试间隔涨到几小时——而服务恢复时
// 第一个用户要等几小时才收到回复。上限 60s 意味着最坏情况下多等一分钟。
type Backoff struct {
	initial time.Duration
	max     time.Duration
	factor  float64
	jitter  float64
	attempt int
	random  func() float64
}

// BackoffOptions 退避参数。零值用默认。
type BackoffOptions struct {
	// Initial 初始延迟。默认 2s
	Initial time.Duration
	// Max 延迟上限。默认 60s
	Max time.Duration
	// Factor 倍率。默认 2
	Factor float64
	// Jitter 抖动比例 0~1。默认 0
	//
	// **多账号同时失败时抖动很有用**：没有它，它们会在同一刻齐刷刷重连，
	// 而服务端刚恢复就被同一批请求再打垮一次。
	Jitter float64
	// Random 注入随机源，便于测试。默认 math/rand
	Random func() float64
}

// NewBackoff 造一个退避。参数非法时**归一到默认值**而不是返错——
// 这些是部署参数，写错了顶多重试节奏不理想，不该让账号起不来。
func NewBackoff(options BackoffOptions) *Backoff {
	b := &Backoff{
		initial: options.Initial,
		max:     options.Max,
		factor:  options.Factor,
		jitter:  options.Jitter,
		random:  options.Random,
	}
	if b.initial <= 0 {
		b.initial = 2 * time.Second
	}
	if b.max < b.initial {
		b.max = b.initial
	}
	if b.factor < 1 {
		b.factor = 2
	}
	if b.jitter < 0 || b.jitter > 1 {
		b.jitter = 0
	}
	if b.random == nil {
		b.random = rand.Float64
	}
	return b
}

// DefaultBackoff 长轮询用的退避：2s 起、封顶 60s、20% 抖动。
func DefaultBackoff() *Backoff {
	return NewBackoff(BackoffOptions{Jitter: 0.2})
}

// Attempts 当前已连续失败的次数。
func (b *Backoff) Attempts() int { return b.attempt }

// Next 记录一次失败，返回下次重试前应等待的时长。
func (b *Backoff) Next() time.Duration {
	base := float64(b.initial) * math.Pow(b.factor, float64(b.attempt))
	if base > float64(b.max) || math.IsInf(base, 0) {
		base = float64(b.max)
	}
	b.attempt++

	if b.jitter == 0 {
		return time.Duration(base)
	}
	// 在 [base*(1-jitter), base] 之间取值，避免延迟超过上限
	spread := base * b.jitter
	picked := base - spread*b.random()
	if picked < 0 {
		picked = 0
	}
	return time.Duration(picked)
}

// Reset 成功之后调用：下一次失败从头开始。
func (b *Backoff) Reset() { b.attempt = 0 }
