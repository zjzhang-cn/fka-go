package web

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/zjzhang-cn/fka-go/internal/channels"
	"github.com/zjzhang-cn/fka-go/internal/config"
)

// maxOutMediaBytes 单个出站媒体的读取上限。与渠道侧一致——字节可能来自远端沙盒，
// 没有上限就是让远端决定我们分配多少内存。
const maxOutMediaBytes int64 = 64 << 20

// Channel 一个网页渠道实例。**单账号、多用户**：一个实例服务所有浏览器会话，
// 按 `PrincipalID` 区分用户、按 `ConversationID` 区分会话。
type Channel struct {
	accountID string
	addr      string
	// users token → PrincipalID。认证与身份都由它决定（见 server.go 的 principalFor）
	users     map[string]string
	staticDir string

	hub   *hub
	blobs *blobStore

	mu        sync.Mutex
	server    *http.Server
	status    channels.Status
	onMessage func(channels.InboundMessage)
}

func newChannel(accountID, addr string, users map[string]string, staticDir string) *Channel {
	return &Channel{
		accountID: accountID,
		addr:      addr,
		users:     users,
		staticDir: staticDir,
		hub:       newHub(),
		blobs:     newBlobStore(),
		status:    channels.StatusOffline,
	}
}

// sessionConversation 内部会话键：`<PrincipalID>:<原始会话>`。
//
// **必须纳入 principal**：否则两个用户各自取同一个 conversation id（比如都叫
// `room1`）会落进同一条会话键——历史、记忆、SSE 房间全部串在一起，且不报错。
// 客户端只看到自己的原始 conversation，命名空间只在这一层加。
func sessionConversation(principal, conversation string) string {
	return principal + ":" + conversation
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
//
// ConversationID 到这里已经是**内部会话键**（`<principal>:<原始会话>`，见
// sessionConversation）——它来自本渠道产出的 InboundMessage，不由客户端直接给，
// 所以这里不按客户端会话 id 的规则校验，只挡空串。
func (c *Channel) sendText(ctx context.Context, p channels.SendTextParams) (channels.SendResult, error) {
	if strings.TrimSpace(p.Target.ConversationID) == "" {
		return channels.SendResult{}, errors.New("会话为空，不知道发给谁")
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
	if strings.TrimSpace(p.Target.ConversationID) == "" {
		return channels.SendResult{}, errors.New("会话为空，不知道发给谁")
	}

	data, err := readMedia(p)
	if err != nil {
		return channels.SendResult{}, err
	}
	mime := mediaMime(p.FileName, p.MimeType, data)

	id, ok := c.blobs.put(data, p.FileName, mime)
	if !ok {
		return channels.SendResult{}, errors.New("媒体暂存超限，无法发送")
	}
	c.hub.broadcast(p.Target.ConversationID, "file", map[string]any{
		"url": "/files/" + id, "name": p.FileName, "mimeType": mime, "kind": kind,
	})
	return channels.SendResult{MessageID: id}, nil
}

// mediaMime 定下这次出站媒体的 MIME，决定 `/files/{id}` 的 `Content-Type`，
// 从而决定浏览器怎么渲染。按可信度排序：
//
//  1. 调用方给的**媒体类**具体类型（`image/png`、`video/mp4`、`application/pdf`）——直接用；
//  2. 内容嗅探（http.DetectContentType）——覆盖 png/jpeg/gif/webp/bmp 与 mp4/webm/mp3/wav；
//  3. 扩展名——嗅探认不出的（**SVG** 是 XML 文本、mov/mkv/ogg/m4a 等）按后缀补。
//
// 关键一条：`text/*`、`application/octet-stream` 这类**不是渲染类型**，不能无条件信任。
// `mcp__bash__export` 把 SVG 嗅探成 `text/xml` 就是这么来的——它带着正确的文件名到这儿，
// 有后缀就该让后缀把它纠回 `image/svg+xml`，否则浏览器拿着 `text/xml` 不解码。
func mediaMime(name, declared string, data []byte) string {
	declared = strings.TrimSpace(declared)
	if isMediaMime(declared) {
		return declared
	}
	sniffed := http.DetectContentType(data)
	if isMediaMime(sniffed) {
		return sniffed
	}
	if m := mediaExtMime[strings.ToLower(filepath.Ext(name))]; m != "" {
		return m
	}
	// 兜底：既不是媒体类、后缀也认不出，就退回一个具体的声明（`text/plain; charset=utf-8`），
	// 至少比 sniff 的 octet-stream 更贴切；通配（`image/*`）不是合法 Content-Type，不用。
	if declared != "" && !strings.Contains(declared, "*") {
		return declared
	}
	return sniffed
}

// isMediaMime 是不是浏览器能直接渲染的媒体类型。**排除通配**（`image/*`）——它不是合法
// 的 `Content-Type`，只有它的「大类」有用，那由嗅探与后缀去落实。
func isMediaMime(m string) bool {
	if m == "" || strings.Contains(m, "*") {
		return false
	}
	m = strings.ToLower(m)
	return strings.HasPrefix(m, "image/") || strings.HasPrefix(m, "video/") ||
		strings.HasPrefix(m, "audio/") || strings.HasPrefix(m, "application/pdf")
}

// mediaExtMime 只列**嗅探认不出**或需要更精确后缀的类型；png/jpeg/gif/webp/bmp 与
// mp4/webm 已由嗅探覆盖，这里仍列全是为了「明明有后缀却退成下载」不出现。
var mediaExtMime = map[string]string{
	".svg": "image/svg+xml", ".bmp": "image/bmp", ".png": "image/png",
	".jpg": "image/jpeg", ".jpeg": "image/jpeg", ".gif": "image/gif",
	".webp": "image/webp", ".ico": "image/x-icon", ".avif": "image/avif",
	".mp4": "video/mp4", ".webm": "video/webm", ".mov": "video/quicktime",
	".mkv": "video/x-matroska", ".avi": "video/x-msvideo", ".ogv": "video/ogg",
	".mp3": "audio/mpeg", ".wav": "audio/wav", ".ogg": "audio/ogg",
	".oga": "audio/ogg", ".m4a": "audio/mp4", ".aac": "audio/aac",
	".flac": "audio/flac", ".opus": "audio/opus",
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
