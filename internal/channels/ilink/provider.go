package ilink

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"github.com/zjzhang-cn/fka-go/internal/channels"
	"github.com/zjzhang-cn/fka-go/internal/channels/ilink/bot"
	"github.com/zjzhang-cn/fka-go/internal/config"
)

// maxAccountSlots 一个安装最多支持几个账号。
//
// **必须有上限**：槽位是从 `ILINK_ACCOUNT_<N>_*` 顺序扫出来的，没有上限的话
// 变量名里带个 1 的会一路扫到 `ILINK_ACCOUNT_1000000_*`。
const maxAccountSlots = 16

// Provider iLink 渠道 provider：**「微信」这一种渠道的完整实现**。
//
// 与渠道有关的东西都收在这里，对外只经 `channels.Service` 暴露：
//
//  1. 造出「一个账号一个」的渠道实例（每个跑自己的长轮询）
//  2. 记下入站会话上下文——它是渠道自己的状态，不让业务层写
//  3. 交运维端口 ops：状态 / 上下文 / 扫码登录
//
// 协议实现在 `./bot/**`，是这一层的内部。
type Provider struct {
	// Accounts 显式账号列表。**给了就不读环境里的槽位**（测试与多环境用）
	Accounts []bot.WeixinAccount
	// DataDir 游标等运行时数据落哪儿。空时按安装根解析
	DataDir string
	// HTTPClient 注入。测试用假服务器；nil 时用默认
	HTTPClient *http.Client
	// BaseURL 登录接口的服务基址。空时用 bot.DefaultBaseURL
	BaseURL string
	// NewPoller 造轮询器。nil 时用真的
	NewPoller pollerFor

	once     bool
	table    *accountTable
	state    *sessionState
	cursors  *bot.CursorStore
	channels []*Channel
}

// NewProvider 造 provider。
func NewProvider() *Provider { return &Provider{} }

// ID 渠道种类标识。
func (p *Provider) ID() string { return ID }

// Label 人类可读名称。
func (p *Provider) Label() string { return Label }

// Create 产出这个种类当前的渠道实例（一个账号一个）。
//
// **没有账号也返回空切片而不是返错**：登录是经由本服务做的，
// 若这里直接失败，全新环境就陷入死锁——没账号起不了服务，没服务又登不了账号。
// 真正「接不上了」由调用方按「零个实例」来判断。
func (p *Provider) Create(ctx context.Context) ([]channels.Channel, error) {
	accounts := p.Accounts
	if len(accounts) == 0 {
		accounts = AccountsFromEnv(p.DataDir)
	}

	p.table = newAccountTable(accounts)
	p.state = newSessionState()
	p.cursors = bot.NewCursorStore(p.cursorPath())
	p.channels = nil

	// 按 ID 排序：**注册顺序决定接缝里的稳定顺序**，而 map 遍历是随机的——
	// 不排序的话每次启动账号顺序都不同，日志与 `fka tools` 的输出就没法比对
	sort.Slice(accounts, func(i, j int) bool { return accounts[i].ID < accounts[j].ID })

	out := make([]channels.Channel, 0, len(accounts))
	for _, account := range accounts {
		instance := NewChannel(account, p.table, p.state, p.cursors, p.pollerFor())
		p.channels = append(p.channels, instance)
		out = append(out, instance)
	}
	p.once = true
	return out, nil
}

func (p *Provider) pollerFor() pollerFor {
	if p.NewPoller != nil {
		return p.NewPoller
	}
	return func(account bot.WeixinAccount, cursors *bot.CursorStore,
		onMessage func(bot.WeixinMessage), onExpired func()) poller {
		poller := bot.NewPoller(account, cursors, p.HTTPClient)
		poller.OnMessage = onMessage
		poller.OnSessionExpired = onExpired
		// **失败必须有人接。** 这个回调以前全仓无人赋值：网络与协议失败于是彻底
		// 静默，而 `parse.go` 的注释正好在骂这件事（「静默忽略正是过去『出问题
		// 完全看不见』的原因」）。协议层是唯一有话说却没嘴的一层——整棵 ilink
		// 树里一条日志都没有。
		poller.OnError = func(err error) {
			config.Log().Warn(config.TypeCHAN, "长轮询失败，退避后重试", config.Context{
				"channel": ID, "account": account.ID, "error": err.Error(),
			})
		}
		return poller
	}
}

// cursorPath 游标文件路径。
//
// **安装根只认 `internal/config` 一处**：以前这里自己实现了一遍 `FKA_HOME > 可执行
// 文件目录 > cwd`，而那份实现比 config 少一层兜底（没有 `EvalSymlinks`、没有 cwd
// 回退）。同一份安装里两处各解析一次，迟早会在某个启动方式下指到不同的盘上——
// 而那正是 AGENTS.md 列在最前面的那个坑。
func (p *Provider) cursorPath() string {
	if p.DataDir != "" {
		return filepath.Join(p.DataDir, "cursors.json")
	}
	return config.DataPath("cursors.json")
}

// ResolveAccount 把用户给的账号选择器（`2` / `account_002`）解析成账号 id。
//
// **选择器怎么读是渠道自己的事**（`ILINK_ACCOUNT_N_ID`），所以这一步留在
// provider 里，而不是写进接缝。
func (p *Provider) ResolveAccount(selector string) (string, bool) {
	selector = strings.TrimSpace(selector)
	if selector == "" {
		return "", false
	}

	for _, account := range p.accounts() {
		if account.ID == selector {
			return account.ID, true
		}
	}
	// 槽位号：把 `2` 变成 `account_002`
	if index, err := strconv.Atoi(selector); err == nil {
		candidate := fmt.Sprintf("account_%03d", index)
		for _, account := range p.accounts() {
			if account.ID == candidate {
				return account.ID, true
			}
		}
	}
	return "", false
}

// DescribeAccounts 把可选账号列成一句话，用在「你选的账号不存在」的提示里。
func (p *Provider) DescribeAccounts() string {
	accounts := p.accounts()
	if len(accounts) == 0 {
		return "当前没有配置任何 iLink 账号（跑 `fka login` 扫码登录）"
	}

	parts := make([]string, 0, len(accounts))
	for _, account := range accounts {
		parts = append(parts, fmt.Sprintf("%s（%s）", account.ID, account.Status))
	}
	return strings.Join(parts, "、")
}

// Ops 运维端口：状态 / 上下文 / 扫码登录。
//
// **它不被接缝理解**——接缝只把返回的东西原样转给控制面，形状由渠道自定义。
func (p *Provider) Ops() channels.Ops { return p }

// Status 运行状态报告。
func (p *Provider) Status() any {
	accounts := p.accounts()
	items := make([]map[string]any, 0, len(accounts))
	for _, account := range accounts {
		items = append(items, map[string]any{
			"accountId": account.ID,
			"status":    account.Status,
			"userId":    account.ILinkUserID,
			"botId":     account.ILinkBotID,
		})
	}
	return map[string]any{"channel": ID, "accounts": items}
}

// Context 「现在能发给谁」的上下文。
func (p *Provider) Context() any {
	if p.state == nil {
		return map[string]any{"channel": ID, "lastInbound": nil}
	}

	items := make([]map[string]any, 0, 4)
	for _, account := range p.accounts() {
		entry, ok := p.state.lastOf(account.ID)
		if !ok {
			continue
		}
		items = append(items, map[string]any{
			"accountId": account.ID,
			"to":        entry.conversation,
			"at":        entry.at,
			// **令牌本身不给**：状态快照会落盘，而它等同于发消息的资格
			"hasToken": entry.replyToken != "",
		})
	}
	return map[string]any{"channel": ID, "lastInbound": items}
}

// Login 交互式登录（扫码）。
//
// account 选择器：给空串或 `next` 时用「下一个还没登录的槽位」。
func (p *Provider) Login(params channels.LoginParams) (any, error) {
	// **先确认 Create 过了**：下面 `pickSlot` 与 `table.replace` 都要用账号表。
	// 这句检查以前在 `pickSlot` **之后**，而「下一个空槽」那条路会直接读 nil 表
	// （`accountTable.get` 对 nil 接收者取锁 → panic）——于是这句话永远不可达。
	if p.table == nil {
		return nil, fmt.Errorf("provider 还没 Create 过")
	}

	index, err := p.pickSlot(params.Account)
	if err != nil {
		return nil, err
	}

	emit := params.Emit
	if emit == nil {
		emit = func(string, any) {}
	}

	// 前缀 `ilink:` 是渠道自己的命名空间——控制面只转发不理解内容，
	// 所以多个渠道可以各发各的事件而不打架
	emit("qrcode:fetching", map[string]any{})

	code, err := bot.GetQRCode(params.Ctx, p.loginBaseURL(), p.HTTPClient)
	if err != nil {
		return nil, err
	}
	payload, err := bot.QRPayload(code)
	if err != nil {
		// 内容取不到就**不要发码**：让人扫一个生成不出来的码，
		// 只会等上几分钟再听到「没反应」
		return nil, err
	}
	emit("qrcode:ready", map[string]any{
		"accountIndex": index, "content": payload,
	})

	credentials, err := bot.PollQRCodeStatus(params.Ctx, p.loginBaseURL(), p.HTTPClient, code.QRCode,
		func(status string) { emit("qrcode:"+status, map[string]any{"accountIndex": index}) })
	if err != nil {
		return nil, err
	}

	// **先落盘再接进服务**：凭证写不进 .env 的话，重启后账号就消失了，
	// 而用户会以为「登录成功了但消息收不到」
	if err := bot.SaveCredentials(config.EnvPath(), index, credentials); err != nil {
		return nil, err
	}

	account := bot.WeixinAccount{
		ID:          fmt.Sprintf("account_%03d", index),
		BotToken:    credentials.BotToken,
		BaseURL:     credentials.BaseURL,
		ILinkBotID:  credentials.ILinkBotID,
		ILinkUserID: credentials.ILinkUserID,
		Status:      bot.AccountOnline,
	}
	p.table.replace(account)
	// **游标要清掉**：留着旧游标的话，重新登录后服务端会以为客户端已消费到
	// 那一段，于是那段时间的消息永久收不到。
	// 清不掉也要让用户知道——它的后果是「刚登录就漏消息」，而那看上去像「没人发消息」
	if err := p.cursors.Clear(account.ID); err != nil {
		config.Log().Warn(config.TypeCHAN, "清游标失败，重新登录后那一段消息可能收不到", config.Context{
			"account": account.ID, "error": err.Error(),
		})
	}

	emit("login:done", map[string]any{"accountId": account.ID})
	return map[string]any{"accountId": account.ID, "status": account.Status}, nil
}

// pickSlot 定这次登录用哪个槽位。
func (p *Provider) pickSlot(selector string) (int, error) {
	selector = strings.TrimSpace(selector)

	if selector != "" && selector != "next" {
		index, err := strconv.Atoi(selector)
		if err != nil || index < 1 || index > maxAccountSlots {
			return 0, fmt.Errorf("账号槽位要填 1 到 %d 之间的数字，收到 %q", maxAccountSlots, selector)
		}
		return index, nil
	}

	// 「下一个空槽」：跳过已经有 token 的
	for index := 1; index <= maxAccountSlots; index++ {
		id := fmt.Sprintf("account_%03d", index)
		if existing, ok := p.table.get(id); ok && existing.BotToken != "" {
			continue
		}
		return index, nil
	}
	return 0, fmt.Errorf("%d 个槽位都用满了", maxAccountSlots)
}

func (p *Provider) loginBaseURL() string {
	if p.BaseURL != "" {
		return p.BaseURL
	}
	return bot.DefaultBaseURL
}

func (p *Provider) accounts() []bot.WeixinAccount {
	if p.table == nil {
		return nil
	}
	accounts := p.table.list()
	sort.Slice(accounts, func(i, j int) bool { return accounts[i].ID < accounts[j].ID })
	return accounts
}

// ── 从环境读账号 ────────────────────────────────────────

// AccountsFromEnv 扫 `ILINK_ACCOUNT_<N>_*` 读账号表。
//
// **扫到第一个没有 `_ID` 的槽位就停**：中间的空洞允许（删掉一个账号不必重排
// 剩下的），而末尾的空洞说明后面没有了。
func AccountsFromEnv(dataDir string) []bot.WeixinAccount {
	var out []bot.WeixinAccount

	for index := 1; index <= maxAccountSlots; index++ {
		prefix := "ILINK_ACCOUNT_" + strconv.Itoa(index) + "_"

		accountID := strings.TrimSpace(os.Getenv(prefix + "ID"))
		token := strings.TrimSpace(os.Getenv(prefix + "BOT_TOKEN"))
		// **没有 token 的槽位不算账号**：那多半是 `.env.example` 复制过来时
		// 留下的空壳，而接进来只会得到一个一路报错的渠道实例
		if accountID == "" || token == "" {
			continue
		}

		account := bot.WeixinAccount{
			ID:          accountID,
			BotToken:    token,
			BaseURL:     strings.TrimSpace(os.Getenv(prefix + "BASE_URL")),
			ILinkBotID:  strings.TrimSpace(os.Getenv(prefix + "BOT_ID")),
			ILinkUserID: strings.TrimSpace(os.Getenv(prefix + "USER_ID")),
			Status:      bot.AccountOffline,
		}
		if account.BaseURL == "" {
			account.BaseURL = bot.DefaultBaseURL
		}
		out = append(out, account)
	}
	return out
}
