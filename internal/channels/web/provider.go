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
	// AccountID 这个渠道只有一个账号——由此带来一个要写明的代价：接缝按账号分片，
	// 所以**所有 web 会话串行执行**。要多用户并发得改成「一个用户一个账号实例」。
	AccountID = "web"
)

// 环境变量。
const (
	// EnvAddr 监听地址。**设了才启用本渠道**（如 `127.0.0.1:8787`）。空 = 不接。
	EnvAddr = "WEB_CHANNEL_ADDR"
	// EnvToken 访问令牌。地址设了但令牌为空时**拒绝启动本渠道**——不经认证把
	// agent 放到 HTTP 上是结构性风险。
	EnvToken = "WEB_CHANNEL_TOKEN"
	// EnvStatic 静态页目录。空 = `<安装根>/web`。
	EnvStatic = "WEB_CHANNEL_STATIC"
	// EnvPrincipal 这个渠道的用户身份（写进 PrincipalID）。空 = `web:default`。
	// 单账号单身份；多用户要靠每个用户一个 token/身份，见决策记录。
	EnvPrincipal = "WEB_CHANNEL_PRINCIPAL"
)

// defaultPrincipal 未配置时的全局身份。**带命名空间前缀**——PrincipalID 必须
// 跨渠道全局唯一（会话历史、权限过滤都靠它）。
const defaultPrincipal = "web:default"

// Provider 网页渠道 provider。字段用于测试注入；生产一律读环境变量。
type Provider struct {
	// Addr 覆盖监听地址。空 = 读 EnvAddr
	Addr string
	// Token 覆盖令牌。空 = 读 EnvToken
	Token string
	// StaticDir 覆盖静态目录。空 = 读 EnvStatic，再回落 `<安装根>/web`
	StaticDir string
	// Principal 覆盖身份。空 = 读 EnvPrincipal，再回落 defaultPrincipal
	Principal string
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
// 配了地址却没给令牌才返错——那是一次明确的配置错误。
func (p *Provider) Create(ctx context.Context) ([]channels.Channel, error) {
	addr := firstNonEmpty(p.Addr, os.Getenv(EnvAddr))
	if addr == "" {
		return nil, nil
	}

	token := firstNonEmpty(p.Token, os.Getenv(EnvToken))
	if token == "" {
		return nil, fmt.Errorf("%s 设了却没有 %s：网页渠道必须带令牌才能启用", EnvAddr, EnvToken)
	}

	staticDir := firstNonEmpty(p.StaticDir, os.Getenv(EnvStatic))
	if staticDir == "" {
		staticDir = filepath.Join(config.Home(), "web")
	}

	principal := firstNonEmpty(p.Principal, os.Getenv(EnvPrincipal), defaultPrincipal)

	return []channels.Channel{newChannel(AccountID, addr, token, staticDir, principal)}, nil
}

// ResolveAccount 解析账号选择器。单账号渠道：只有空串与 "web" 有效。
func (p *Provider) ResolveAccount(selector string) (string, bool) {
	switch strings.TrimSpace(selector) {
	case "", AccountID:
		return AccountID, true
	}
	return "", false
}

// DescribeAccounts 可选账号的一句话。
func (p *Provider) DescribeAccounts() string { return AccountID }

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
