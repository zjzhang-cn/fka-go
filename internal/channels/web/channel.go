package web

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"sync"

	"github.com/zjzhang-cn/fka-go/internal/channels"
	"github.com/zjzhang-cn/fka-go/internal/config"
)

// maxOutMediaBytes 单个出站媒体的读取上限。与渠道侧一致——字节可能来自远端沙盒，
// 没有上限就是让远端决定我们分配多少内存。
const maxOutMediaBytes int64 = 64 << 20

// Channel 一个网页渠道实例。**单账号**：一个实例服务所有浏览器会话，按
// ConversationID 隔离。
type Channel struct {
	accountID string
	addr      string
	token     string
	staticDir string
	principal string

	hub   *hub
	blobs *blobStore

	mu        sync.Mutex
	server    *http.Server
	status    channels.Status
	onMessage func(channels.InboundMessage)
}

func newChannel(accountID, addr, token, staticDir, principal string) *Channel {
	return &Channel{
		accountID: accountID,
		addr:      addr,
		token:     token,
		staticDir: staticDir,
		principal: principal,
		hub:       newHub(),
		blobs:     newBlobStore(),
		status:    channels.StatusOffline,
	}
}

// ── Identity ────────────────────────────────────────────

func (c *Channel) ID() string        { return ID }
func (c *Channel) Label() string     { return Label }
func (c *Channel) AccountID() string { return c.accountID }

func (c *Channel) Status() channels.Status {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.status
}

// ── Outbound ────────────────────────────────────────────

// Capabilities 能力声明。
//
// **File/Image 只声明能发（receive=false）**：业务层没有入站媒体的消费者
// （`internal/messages` 收到媒体会如实拒答），声明能收却没有那条路就是「声明一个
// 跑不了的能力」。等哪天入站媒体有落点，再补 upload 并翻成 true。
func (c *Channel) Capabilities() channels.Capabilities {
	return channels.Capabilities{
		Text:          channels.KindCapability{Send: true, Receive: true},
		File:          channels.KindCapability{Send: true, Receive: false},
		Image:         channels.KindCapability{Send: true, Receive: false},
		Voice:         channels.KindCapability{Send: false, Receive: false},
		Video:         channels.KindCapability{Send: false, Receive: false},
		ProactivePush: true,
	}
}

// Senders 出站处理器，与 Capabilities 一一对应。
func (c *Channel) Senders() channels.Senders {
	return channels.Senders{
		Text:  c.sendText,
		File:  c.sendFile,
		Image: c.sendImage,
	}
}

// sendText 把一段文字推给该会话的浏览器。
func (c *Channel) sendText(ctx context.Context, p channels.SendTextParams) (channels.SendResult, error) {
	if !validConversation(p.Target.ConversationID) {
		return channels.SendResult{}, fmt.Errorf("会话 id %q 不合法", p.Target.ConversationID)
	}
	c.hub.broadcast(p.Target.ConversationID, "message", map[string]any{"text": p.Text})
	return channels.SendResult{MessageID: newID()}, nil
}

// sendFile / sendImage 把字节暂存起来，推一个下载 URL。
//
// **不内联 base64**：SSE 帧会被前端的 JSON 解析、也进不了浏览器缓存；给 URL 更省
// 也更稳。字节来源两种：Data（远端沙盒经 MCP 到达）或 Path（本机文件）。
func (c *Channel) sendFile(ctx context.Context, p channels.SendMediaParams) (channels.SendResult, error) {
	return c.sendMedia(p, "file")
}

func (c *Channel) sendImage(ctx context.Context, p channels.SendMediaParams) (channels.SendResult, error) {
	return c.sendMedia(p, "image")
}

func (c *Channel) sendMedia(p channels.SendMediaParams, kind string) (channels.SendResult, error) {
	if !validConversation(p.Target.ConversationID) {
		return channels.SendResult{}, fmt.Errorf("会话 id %q 不合法", p.Target.ConversationID)
	}

	data, err := readMedia(p)
	if err != nil {
		return channels.SendResult{}, err
	}
	mime := p.MimeType
	if mime == "" {
		mime = http.DetectContentType(data)
	}

	id, ok := c.blobs.put(data, p.FileName, mime)
	if !ok {
		return channels.SendResult{}, errors.New("媒体暂存超限，无法发送")
	}
	c.hub.broadcast(p.Target.ConversationID, "file", map[string]any{
		"url": "/files/" + id, "name": p.FileName, "mimeType": mime, "kind": kind,
	})
	return channels.SendResult{MessageID: id}, nil
}

// readMedia 取字节：Data 优先，否则读 Path。**大小在读取前先看**，免得先分配再拒。
func readMedia(p channels.SendMediaParams) ([]byte, error) {
	if p.Data != nil {
		if int64(len(p.Data)) > maxOutMediaBytes {
			return nil, fmt.Errorf("媒体 %d 字节，超过上限 %d", len(p.Data), maxOutMediaBytes)
		}
		return p.Data, nil
	}
	if p.Path == "" {
		return nil, errors.New("既没有 Data 也没有 Path")
	}
	info, err := os.Stat(p.Path)
	if err != nil {
		return nil, fmt.Errorf("读不到文件：%w", err)
	}
	if !info.Mode().IsRegular() {
		return nil, fmt.Errorf("不是普通文件：%s", p.Path)
	}
	if info.Size() > maxOutMediaBytes {
		return nil, fmt.Errorf("文件 %d 字节，超过上限 %d", info.Size(), maxOutMediaBytes)
	}
	data, err := os.ReadFile(p.Path)
	if err != nil {
		return nil, fmt.Errorf("读取失败：%w", err)
	}
	return data, nil
}

// ── Lifecycle ───────────────────────────────────────────

// Start 起 HTTP 服务。**先 Listen 再 Serve**：绑定失败要当场返回，而不是在
// goroutine 里悄悄死掉（那会让「端口被占」表现成「服务起来了但没人应答」）。
func (c *Channel) Start(ctx context.Context, onMessage func(channels.InboundMessage)) error {
	c.mu.Lock()
	if c.server != nil {
		c.mu.Unlock()
		return errors.New("网页渠道已经启动过了")
	}
	c.onMessage = onMessage
	c.mu.Unlock()

	listener, err := net.Listen("tcp", c.addr)
	if err != nil {
		return fmt.Errorf("网页渠道监听 %s 失败：%w", c.addr, err)
	}

	server := &http.Server{Handler: c.routes()}

	c.mu.Lock()
	c.server = server
	c.status = channels.StatusOnline
	c.mu.Unlock()

	go func() {
		if serveErr := server.Serve(listener); serveErr != nil && !errors.Is(serveErr, http.ErrServerClosed) {
			config.Log().Error(config.TypeCHAN, "网页渠道服务退出", config.Context{"error": serveErr.Error()})
		}
	}()

	config.Log().Info(config.TypeCHAN, "网页渠道已启动", config.Context{
		"addr": c.addr, "static": c.staticDir,
	})
	return nil
}

// Stop 停服务并断开所有 SSE 连接。
func (c *Channel) Stop(ctx context.Context) error {
	c.mu.Lock()
	server := c.server
	c.server = nil
	c.status = channels.StatusOffline
	c.onMessage = nil
	c.mu.Unlock()

	c.hub.closeAll()
	if server != nil {
		return server.Shutdown(ctx)
	}
	return nil
}

// ── EmitterProvider（可选能力）──────────────────────────

// Emitter 为一条消息提供 SSE 回显。会话 id 从 ctx 上取——消息层把它绑进了 ctx
// （见 internal/messages 的 Handle），这里只读它。
func (c *Channel) Emitter(ctx context.Context) channels.Emitter {
	var conversation string
	if fields := config.FieldsOf(ctx); fields != nil {
		conversation, _ = fields["conversation"].(string)
	}
	if conversation == "" {
		return nil // 退回消息层的默认（只记工具日志）
	}
	return &sseEmitter{hub: c.hub, conversation: conversation}
}
