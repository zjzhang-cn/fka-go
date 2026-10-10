// Package web 是**网页渠道**：浏览器经 SSE 收、经 POST 发。
//
// ## 它提供什么
//
// 与微信渠道对齐的是**能力面**，不是微信的机制——没有扫码登录、没有
// `context_token`、没有 AES 上传 CDN。这里只有：
//
//   - `POST /messages`：浏览器发一条文字 → 产出一条 InboundMessage；
//   - `GET /events`：SSE，服务端把答复与过程事件推回同一个会话；
//   - `GET /files/{id}`：出站媒体的下载口（SSE 事件里给相对 URL，不内联 base64）；
//   - `POST /login`：用 token 换一个 HttpOnly cookie（`EventSource` 与 `<img>`
//     都带不了 header，cookie 一次解决三处）。
//
// ## 多用户：token → PrincipalID
//
// 每个用户一个 token，配在用户表里（`<安装根>/web-users.json`，或
// `WEB_CHANNEL_USERS` 指定的文件）。登录时 token 反查 PrincipalID，之后这条
// 会话的一切（历史、SSE 房间、工具身份）都带它。**身份由配置决定，模型碰不到**。
//
// 单用户仍可只配 `WEB_CHANNEL_TOKEN` + `WEB_CHANNEL_PRINCIPAL`（旧写法，作为回落）。
//
// ## 为什么实现 EmitterProvider
//
// `channels.Emitter` 接口早就存在却无人实现，而 SSE 是它最自然的落点：推理增量与
// 工具调用边发生边推。**最终答案不走 Emitter**——它经 `Senders.Text` 送出，与
// `Emitter.Answer` 是同一份文本，两边都发就会重复（见 emitter.go）。
//
// ## 与接缝的关系
//
// 本包 import `internal/channels`（父包），而 `internal/channels` **不认识任何具体
// 渠道**——那条由 `channels_test.go` 守着。加这个渠道，接缝与业务层一行未改。
package web

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/zjzhang-cn/fka-go/internal/channels"
	"github.com/zjzhang-cn/fka-go/internal/config"
)

// 渠道标识。**小写与下划线**，会进工具名与日志。
const (
	// ID 渠道种类标识
	ID = "web"
	// Label 人类可读名称
	Label = "网页"
)

// defaultAccount 这个渠道默认的账号标识。**刻意不叫 "web"**：账号是跨渠道的选择器，
// 与渠道种类同名只会让人分不清说的是种类还是账号。用 WEB_CHANNEL_ACCOUNT 覆盖。
//
// 单账号渠道——由此带来一个要写明的代价：接缝按账号分片，所以**所有网页会话
// 串行执行**。要多用户并发得改成「一个用户一个账号实例」。
const defaultAccount = "default"

// 环境变量。
const (
	// EnvAddr 监听地址。**设了才启用本渠道**（如 `127.0.0.1:8787`）。空 = 不接。
	EnvAddr = "WEB_CHANNEL_ADDR"
	// EnvToken 单用户回落的访问令牌。配了用户表时忽略它。
	EnvToken = "WEB_CHANNEL_TOKEN"
	// EnvStatic 静态页目录。空 = `<安装根>/web`。
	EnvStatic = "WEB_CHANNEL_STATIC"
	// EnvPrincipal 单用户回落的身份（写进 PrincipalID）。空 = `web:default`。
	EnvPrincipal = "WEB_CHANNEL_PRINCIPAL"
	// EnvAccount 账号标识。空 = defaultAccount（`default`）。它进会话键，
	// 因此也决定会话历史的文件名——改它等于换一套历史。
	EnvAccount = "WEB_CHANNEL_ACCOUNT"
	// EnvUsers 用户表文件路径。空 = `<安装根>/web-users.json`。
	EnvUsers = "WEB_CHANNEL_USERS"
)

// defaultPrincipal 单用户回落的全局身份。**带命名空间前缀**——PrincipalID 必须
// 跨渠道全局唯一（会话历史、权限过滤、租户目录都靠它）。
const defaultPrincipal = "web:default"

// defaultUsersFile 用户表文件名（在安装根下）。**含密钥，不入库**。
const defaultUsersFile = "web-users.json"

// User 一个网页渠道用户。
//
// **User 是身份名的裸值**（写 `alice`），落到 PrincipalID 时补成 `web:alice`——
// 命名空间由代码加，配置里不手写前缀。
type User struct {
	Token string `json:"token"`
	User  string `json:"user"`
}

// usersFileDoc 用户表文件的形状：`{"users":[{"token":"…","user":"alice"}, …]}`。
type usersFileDoc struct {
	Users []User `json:"users"`
}

// Provider 网页渠道 provider。字段用于测试注入；生产一律读环境变量/文件。
type Provider struct {
	// Addr 覆盖监听地址。空 = 读 EnvAddr
	Addr string
	// Users 显式用户表（测试注入）。非空 = 不读文件、也不走单用户回落
	Users []User
	// UsersFile 用户表文件。空 = 读 EnvUsers，再回落 `<安装根>/web-users.json`
	UsersFile string
	// StaticDir 覆盖静态目录。空 = 读 EnvStatic，再回落 `<安装根>/web`
	StaticDir string
	// Account 覆盖账号标识。空 = 读 EnvAccount，再回落 defaultAccount
	Account string
}

// NewProvider 造 provider。
func NewProvider() *Provider { return &Provider{} }

// ID 渠道种类标识。
func (p *Provider) ID() string { return ID }

// Label 人类可读名称。
func (p *Provider) Label() string { return Label }

// Create 产出渠道实例。
//
// **未配置就返回空切片而不是返错**：默认不接 web 渠道是常态，不是故障。
// 配了地址却没配任何用户才返错——那是一次明确的配置错误。
func (p *Provider) Create(ctx context.Context) ([]channels.Channel, error) {
	addr := firstNonEmpty(p.Addr, os.Getenv(EnvAddr))
	if addr == "" {
		return nil, nil
	}

	users, err := p.loadUsers()
	if err != nil {
		return nil, err
	}
	if len(users) == 0 {
		return nil, fmt.Errorf("网页渠道没配任何用户：写 %s，或用 %s 指定用户表文件；"+
			"单用户也可以只设 %s",
			filepath.Join(config.Home(), defaultUsersFile), EnvUsers, EnvToken)
	}

	staticDir := firstNonEmpty(p.StaticDir, os.Getenv(EnvStatic))
	if staticDir == "" {
		staticDir = filepath.Join(config.Home(), "web")
	}

	return []channels.Channel{newChannel(p.accountName(), addr, users, staticDir)}, nil
}

// loadUsers 得到 token → PrincipalID 的表。优先级从高到低：
//
//  1. Provider.Users（测试注入）；
//  2. 用户表文件（UsersFile / EnvUsers / `<安装根>/web-users.json`）；
//  3. 单用户回落：`WEB_CHANNEL_TOKEN` + `WEB_CHANNEL_PRINCIPAL`。
//
// **没有任何用户时返回空表**，由 Create 决定拒绝启用。
func (p *Provider) loadUsers() (map[string]string, error) {
	table := map[string]string{}

	if len(p.Users) > 0 {
		if err := addUsers(table, p.Users); err != nil {
			return nil, err
		}
		return table, nil
	}

	path := firstNonEmpty(p.UsersFile, os.Getenv(EnvUsers), filepath.Join(config.Home(), defaultUsersFile))
	data, err := os.ReadFile(path)
	if err == nil {
		var doc usersFileDoc
		if err := json.Unmarshal(data, &doc); err != nil {
			return nil, fmt.Errorf("用户表 %s 解析失败：%w", path, err)
		}
		if err := addUsers(table, doc.Users); err != nil {
			return nil, fmt.Errorf("用户表 %s：%w", path, err)
		}
		return table, nil
	}
	if !os.IsNotExist(err) {
		return nil, fmt.Errorf("用户表 %s 读不了：%w", path, err)
	}

	// 单用户回落
	if token := strings.TrimSpace(os.Getenv(EnvToken)); token != "" {
		table[token] = withNamespace(firstNonEmpty(os.Getenv(EnvPrincipal), defaultPrincipal))
	}
	return table, nil
}

// addUsers 把配置项并进表里。**空 token / 空 user 直接报错**——静默跳过会让
// 「我明明配了这个人」变成一桩无头案。
func addUsers(table map[string]string, users []User) error {
	for i, u := range users {
		token := strings.TrimSpace(u.Token)
		user := strings.TrimSpace(u.User)
		if token == "" || user == "" {
			return fmt.Errorf("第 %d 条用户缺少 token 或 user", i+1)
		}
		table[token] = withNamespace(user)
	}
	return nil
}

// withNamespace 给身份名补渠道前缀。已经带了 `web:` 的原样返回——兼容
// `WEB_CHANNEL_PRINCIPAL=web:default` 这种已命名的旧配置。
func withNamespace(name string) string {
	if strings.HasPrefix(name, ID+":") {
		return name
	}
	return ID + ":" + name
}

// ResolveAccount 解析账号选择器。单账号渠道：只有空串与配置的账号名有效。
func (p *Provider) ResolveAccount(selector string) (string, bool) {
	account := p.accountName()
	if trimmed := strings.TrimSpace(selector); trimmed == "" || trimmed == account {
		return account, true
	}
	return "", false
}

// DescribeAccounts 可选账号的一句话。
func (p *Provider) DescribeAccounts() string { return p.accountName() }

// accountName 账号标识：显式字段 > 环境变量 > 默认。它进会话键与历史文件名。
func (p *Provider) accountName() string {
	return firstNonEmpty(p.Account, os.Getenv(EnvAccount), defaultAccount)
}

// Ops 没有登录/状态这类运维操作。
func (p *Provider) Ops() channels.Ops { return nil }

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if trimmed := strings.TrimSpace(value); trimmed != "" {
			return trimmed
		}
	}
	return ""
}
