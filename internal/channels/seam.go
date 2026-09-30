// Package channels 的**接缝**：渠道实例的注册表 + 入站广播 + 寻址解析。
//
// ## 它做四件事，一件都不认识任何渠道实现
//
//  1. 收下 provider（Register）——一个 provider 是一种渠道
//  2. 把渠道实例按 (channelId, accountId) 登记，**并保证两者全局唯一**
//  3. 把渠道的入站回调转成订阅事件（业务层是订阅者）
//  4. 按用户给的 --channel / --account 找到渠道，并请它解析「发给谁」
//
// 这一文件不 import 任何具体渠道——那条由 `channels_test.go` 守着。
//
// ## 唯一性检查为什么不能省
//
// 两个渠道共用一个 accountId 会让状态快照、会话历史文件、游标互相覆盖。
// 而这类错误**不会当场报**——它表现为「A 账号的消息出现在 B 的会话里」，
// 排查时完全想不起是注册表没拦住。所以重复就**拒绝启动**。
//
// ## 入站为什么是「有界队列 + 非阻塞投递」
//
// 渠道的收包循环**不能被慢消费者堵住**——堵住就等于停收消息，队列一满就开始丢。
// 所以：
//
//   - 每个订阅者一条有界队列，慢的那个只拖慢自己；
//   - 投递是**非阻塞**的，队列满时**记一条警告并丢弃**，不阻塞收包循环。
//
// **丢弃是要记日志的**：消息代理丢消息是不能接受的默认行为，所以宁可吵一点，
// 也要让人在日志里看见。
package channels

import (
	"context"
	"sort"
	"sync"

	"github.com/zjzhang-cn/fka-go/internal/config"
)

// inboundQueueSize 每个订阅者的入站队列长度。
//
// 256 条足够扛住「模型在想」那几秒。**再大也没用**——真堵住了说明消费者有问题，
// 那时该报警而不是把消息堆在内存里。
const inboundQueueSize = 256

// Event 一条入站事件：谁收到的、什么消息。
type Event struct {
	Channel Channel
	Message InboundMessage
}

// Subscription 一个订阅者。
type Subscription struct {
	// C 事件流。**退订与停机时会被关闭**，所以 `for range sub.C` 会自然结束——
	// 这是消费循环能退出、goroutine 不泄漏的唯一办法。
	C <-chan Event
	// cancel 停掉这个订阅者。**调用方负责调**
	cancel func()
}

// Close 退订。
func (s *Subscription) Close() { s.cancel() }

// Service 渠道接缝服务。
type Service struct {
	mu sync.RWMutex

	// providers 已注册的渠道**种类**，按注册顺序
	providers []Provider
	// instances 全部渠道**实例**，键是 channel:account
	instances map[string]Channel
	// accounts 账号标识 → 渠道种类。**账号标识必须跨渠道全局唯一**——
	// 会话历史文件是 `<账号>_<会话>.jsonl`，撞号会让两个渠道的话写进同一个文件
	accounts map[string]string
	// order 实例的稳定顺序（注册顺序），供 Instances 返回
	order []string

	subscribers []*subscription
	started     bool
}

type subscription struct {
	ch chan Event
	// closed 只是「已退订」的信号，让还在遍历的广播立刻跳过
	closed chan struct{}
	once   sync.Once

	// mu 护住 gone 与 ch。**关掉事件流与往里投递必须互斥**——
	// 否则一个广播可能刚检查完「没退订」、关流就发生，于是它往一个已关闭的
	// channel 发送，直接 panic。
	mu   sync.Mutex
	gone bool
}

// close 退订并**关闭事件流**，让消费方的 `for range` 结束。
func (sub *subscription) close() {
	sub.once.Do(func() {
		close(sub.closed)

		sub.mu.Lock()
		defer sub.mu.Unlock()
		sub.gone = true
		close(sub.ch)
	})
}

// trySend 非阻塞投递。已退订就**什么也不做**——不投递、也不往已关闭的流上写。
//
// 返回 false 表示队列满了（消息被丢弃，已记日志）。
func (sub *subscription) trySend(event Event) bool {
	sub.mu.Lock()
	defer sub.mu.Unlock()
	if sub.gone {
		return true // 退订不是「丢消息」，是「没人要了」
	}
	select {
	case sub.ch <- event:
		return true
	default:
		return false
	}
}

// NewService 造一个空接缝。
func NewService() *Service {
	return &Service{
		instances: map[string]Channel{},
		accounts:  map[string]string{},
	}
}

// key 渠道实例的**唯一**查找键。
//
// 三个部分缺一不可：AccountID 只在渠道内唯一（`docs/channel-design.md` §7 记的
// 就是这条未决项），而渠道种类之间也可能撞上账号号。
func key(channelID, accountID string) string { return channelID + ":" + accountID }

// Register 注册一种渠道，并登记它产出的实例。
//
// **账号标识重复会返错**：两个渠道共用一个 accountId 会让状态快照、会话历史与
// 游标互相覆盖——而那不会当场报，症状出现在很远的地方。
//
// ## 要么全部生效，要么全都不生效
//
// 这里以前是「边校验边写」：第 N 个实例撞账号时，前 N-1 个**已经进了**
// `instances` / `accounts` / `order`，而 `providers` 还没 append。那些实例于是活着、
// 能被 `Instances()` 列出来，但 `pickChannel` 不会再问它们的 provider
// （`ResolveAccount` 走 `providers`）——**半接入状态**，而注册期的唯一性检查本意是
// 「拒绝启动」（见包头）。
//
// 现在先在**一份草稿上**把活干完，全通过了才落到真表上。草稿还顺带挡住了
// 「同一个 provider 自己的两个实例撞号」——以前那种情况会写进去一个再报错。
func (s *Service) Register(ctx context.Context, provider Provider) ([]Channel, error) {
	created, err := provider.Create(ctx)
	if err != nil {
		return nil, Errorf(KindGeneric, "",
			"渠道 %s 起不来：%s", provider.Label(), err.Error())
	}

	s.mu.Lock()
	draft := &Service{
		instances: cloneChannels(s.instances),
		accounts:  cloneStrings(s.accounts),
		order:     append([]string(nil), s.order...),
	}
	for _, channel := range created {
		if err := ValidateChannel(channel); err != nil {
			s.mu.Unlock()
			return nil, Errorf(KindGeneric, "这是渠道实现的问题，去看它的能力声明与发送器对不对得上",
				"%s", err.Error())
		}
		if err := draft.attachLocked(channel); err != nil {
			s.mu.Unlock()
			return nil, err
		}
	}
	// 全过了才落真表
	s.instances, s.accounts, s.order = draft.instances, draft.accounts, draft.order
	s.providers = append(s.providers, provider)
	// 已经 startAll 过的渠道立即开收（登录那条路）：服务在收消息了，
	// 那一步没有「先挂订阅者再开收」的窗口可用
	shouldStart := s.started
	s.mu.Unlock()

	if shouldStart {
		for _, channel := range created {
			if err := s.startOne(ctx, channel); err != nil {
				config.Log().Warn(config.TypeCHAN, "渠道开始接收失败", config.Context{
					"channel": channel.ID(), "account": channel.AccountID(), "error": err.Error(),
				})
			}
		}
	}

	config.Log().Info(config.TypeCHAN, "渠道已注册", config.Context{
		"kind": provider.ID(), "accounts": len(created),
	})
	return created, nil
}

// Attach 动态接入一个渠道实例（登录那条路：服务已经起来了，新账号要用得上）。
//
// 与 Register 同一套能力核对与唯一性检查（后者见 `attachLocked` 的说明）。
// **调用方负责在接入后让它开始接收**。
func (s *Service) Attach(channel Channel) error {
	if err := ValidateChannel(channel); err != nil {
		return Errorf(KindGeneric, "这是渠道实现的问题，去看它的能力声明与发送器对不对得上",
			"%s", err.Error())
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	return s.attachLocked(channel)
}

// attachLocked 唯一性检查 + 登记。**调用方必须已持锁**。
//
// 检查分两处，缺一不可：
//
//  1. `(渠道种类, 账号)` 相同 —— 同一个渠道里的同一个账号，重复就是重复；
//  2. **账号标识跨渠道撞号** —— 键是 channel:account，所以 `ilink:1` 与 `tg:1`
//     上面那一条查不出来，但会话历史文件名是 `<账号>_<会话>.jsonl`，两者会写进
//     **同一个文件**。症状是「A 渠道的对话出现在 B 渠道的上下文里」，且不报错。
//
// 拆出来是为了让 `Register` 能在**草稿**上跑同一套检查（要么全部生效，要么全不生效）。
func (s *Service) attachLocked(channel Channel) error {
	k := key(channel.ID(), channel.AccountID())

	if _, exists := s.instances[k]; exists {
		return Errorf(KindGeneric, "换个渠道种类，或改一个账号标识",
			"渠道 %s 的账号 %s 已经接过了", channel.ID(), channel.AccountID())
	}
	if owner, taken := s.accounts[channel.AccountID()]; taken {
		return Errorf(KindGeneric, "给新渠道换一个不撞的账号标识",
			"账号标识 %s 已经被渠道 %s 用了", channel.AccountID(), owner)
	}

	s.instances[k] = channel
	s.accounts[channel.AccountID()] = channel.ID()
	s.order = append(s.order, k)
	return nil
}

// cloneChannels / cloneStrings 给「草稿」用：只在注册期跑一次，量级是渠道实例数。
func cloneChannels(source map[string]Channel) map[string]Channel {
	out := make(map[string]Channel, len(source))
	for k, v := range source {
		out[k] = v
	}
	return out
}

func cloneStrings(source map[string]string) map[string]string {
	out := make(map[string]string, len(source))
	for k, v := range source {
		out[k] = v
	}
	return out
}

// Instances 全部渠道**实例**（一个 provider 可以产出多个），按注册顺序。
func (s *Service) Instances() []Channel {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]Channel, 0, len(s.order))
	for _, k := range s.order {
		out = append(out, s.instances[k])
	}
	return out
}

// ResolveTargetParams 用户在命令行给出的寻址选择器。
type ResolveTargetParams struct {
	// Channel 渠道种类，如 ilink。不给就按账号唯一定位
	Channel string
	// Account 账号：精确 id，或 provider 认得的槽位写法（`2`）
	Account string
	// To 收件人。不给就请渠道用「最近一次入站」推断
	To string
	// Token 显式令牌，覆盖渠道自动取到的
	Token string
}

// ResolvedTarget 解析结果。
type ResolvedTarget struct {
	Channel Channel
	Address OutboundAddress
}

// ResolveTarget 解析「发给谁」。失败时返 AddressError（带解决办法的 Hint）。
func (s *Service) ResolveTarget(params ResolveTargetParams) (ResolvedTarget, error) {
	channel, err := s.pickChannel(params)
	if err != nil {
		return ResolvedTarget{}, err
	}

	// **「不支持推断」靠类型断言，不靠返回值。** 以前这两件事挤在同一个 `ok=false`：
	// 无状态的渠道（不实现）与「实现了但这次推不出来」（还没收到过消息）长得一样，
	// 于是提示语只能说一句最含糊的。
	resolver, canResolve := channel.(AddressResolver)
	if !canResolve {
		return ResolvedTarget{}, AddressError(
			"用 --to 明确指定收件人",
			"渠道 %s 不能自己推断发给谁", channel.Label())
	}

	address, err := resolver.ResolveAddress(ResolveAddressParams{To: params.To, Token: params.Token})
	if err != nil {
		return ResolvedTarget{}, Errorf(KindAddress, "去发条消息，或用 --to / --token 明确指定",
			"渠道 %s 解析收件人失败：%s", channel.Label(), err.Error())
	}

	return ResolvedTarget{Channel: channel, Address: address}, nil
}

func (s *Service) pickChannel(params ResolveTargetParams) (Channel, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	// 渠道种类 + 账号：最明确的一条
	if params.Channel != "" {
		accountID := params.Account
		// 账号可以写成 provider 认得的槽位形式（`2`）
		if accountID != "" {
			for _, provider := range s.providers {
				if provider.ID() != params.Channel {
					continue
				}
				if resolved, ok := provider.ResolveAccount(accountID); ok {
					accountID = resolved
				}
			}
		}
		channel, ok := s.instances[key(params.Channel, accountID)]
		if !ok {
			return nil, AddressError("跑 `fka tools` 看当前接了哪些渠道与账号",
				"没有渠道 %s 的账号 %s", params.Channel, accountID)
		}
		return channel, nil
	}

	// 只给账号：全渠道里找
	if params.Account != "" {
		accountID := params.Account
		for _, provider := range s.providers {
			if resolved, ok := provider.ResolveAccount(accountID); ok {
				accountID = resolved
			}
		}

		var found Channel
		for _, k := range s.order {
			channel := s.instances[k]
			if channel.AccountID() != accountID {
				continue
			}
			if found != nil {
				// 重名：列出全部，让用户自己说清楚
				return nil, AddressError("用 --channel 指定渠道种类",
					"账号 %s 在多个渠道上都有（%s）", accountID, s.accountChannelsLocked(accountID))
			}
			found = channel
		}
		if found == nil {
			return nil, AddressError("跑 `fka tools` 看当前接了哪些渠道与账号",
				"没有账号 %s", accountID)
		}
		return found, nil
	}

	// 什么都没给：只有一个渠道实例时用它
	if len(s.order) == 1 {
		return s.instances[s.order[0]], nil
	}

	return nil, AddressError("用 --channel 与 --account 指定",
		"当前接了 %d 个渠道实例，说不清发给谁", len(s.order))
}

// accountChannelsLocked 列出某账号出现在哪些渠道上。**调用方必须已持锁**。
func (s *Service) accountChannelsLocked(accountID string) string {
	var ids []string
	for _, k := range s.order {
		if s.instances[k].AccountID() == accountID {
			ids = append(ids, k)
		}
	}
	sort.Strings(ids)
	out := ""
	for i, id := range ids {
		if i > 0 {
			out += "、"
		}
		out += id
	}
	return out
}

// Subscribe 订阅入站消息。
//
// **顺序要紧：先 Subscribe 再 StartAll。** 反过来会有一个丢消息的窗口——
// 渠道一开收就可能来消息，而那时还没有订阅者。所以 StartAll 不在这里自动调。
//
// 消费方写成 `for event := range sub.C`：退订或 StopAll 时事件流会被关闭，
// 这个循环自然结束——**这是消费 goroutine 能退出的唯一办法**。
func (s *Service) Subscribe() *Subscription {
	sub := &subscription{ch: make(chan Event, inboundQueueSize), closed: make(chan struct{})}

	s.mu.Lock()
	s.subscribers = append(s.subscribers, sub)
	count := len(s.subscribers)
	s.mu.Unlock()

	config.Log().Debug(config.TypeCHAN, "入站订阅已建立", config.Context{"subscribers": count})
	return &Subscription{C: sub.ch, cancel: sub.close}
}

func (s *Service) broadcast(channel Channel, message InboundMessage) {
	event := Event{Channel: channel, Message: message}

	s.mu.RLock()
	subs := append([]*subscription(nil), s.subscribers...)
	s.mu.RUnlock()

	for _, sub := range subs {
		if sub.trySend(event) {
			continue
		}
		// **队列满**：宁可吵一点也不阻塞收包循环。消息代理丢消息是不能接受的
		// 默认行为，所以一定要留下痕迹
		config.Log().Warn(config.TypeCHAN, "入站队列已满，丢弃一条消息", config.Context{
			"channel": channel.ID(), "account": channel.AccountID(),
			"messageId": message.MessageID, "queue": inboundQueueSize,
		})
	}
}

// StartAll 开始接收。
//
// **由订阅者调用**：先 Subscribe，再 StartAll。顺序反了会有丢消息的窗口。
func (s *Service) StartAll(ctx context.Context) {
	s.mu.Lock()
	if s.started {
		s.mu.Unlock()
		return
	}
	s.started = true
	channels := make([]Channel, 0, len(s.order))
	for _, k := range s.order {
		channels = append(channels, s.instances[k])
	}
	s.mu.Unlock()

	for _, channel := range channels {
		if err := s.startOne(ctx, channel); err != nil {
			// 一个渠道起不来**不能把别的带走**——少一个渠道，服务照常
			config.Log().Warn(config.TypeCHAN, "渠道开始接收失败", config.Context{
				"channel": channel.ID(), "account": channel.AccountID(), "error": err.Error(),
			})
		}
	}
}

func (s *Service) startOne(ctx context.Context, channel Channel) error {
	return channel.Start(ctx, func(message InboundMessage) {
		s.broadcast(channel, message)
	})
}

// StopAll 停掉所有渠道的接收。
func (s *Service) StopAll(ctx context.Context) {
	for _, channel := range s.Instances() {
		if err := channel.Stop(ctx); err != nil {
			config.Log().Warn(config.TypeCHAN, "渠道停止接收失败", config.Context{
				"channel": channel.ID(), "account": channel.AccountID(), "error": err.Error(),
			})
		}
	}
	s.mu.Lock()
	subs := s.subscribers
	s.subscribers = nil
	s.started = false
	s.mu.Unlock()

	for _, sub := range subs {
		sub.close()
	}
}
