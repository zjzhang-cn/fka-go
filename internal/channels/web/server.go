package web

import (
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"io"
	"mime/multipart"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/zjzhang-cn/fka-go/internal/channels"
	"github.com/zjzhang-cn/fka-go/internal/config"
)

// cookieName 认证 cookie 名。**HttpOnly**：JS 读不到它；SSE 与媒体请求会自动带上。
const cookieName = "fka_web_token"

// 一次上传请求体与单件入站文件的上限。比单件宽是容许一条消息带几个文件；
// 单件的上限与消息层一致，免得「网页收下了、消息层又拒了」。
const (
	maxUploadBodyBytes  int64 = 16 << 20
	maxInboundFileBytes int64 = 8 << 20
)

// conversationPattern 会话 id 的合法形状。它进 SessionKey，最终进会话历史文件名，
// 所以必须限制字符集与长度——否则 `../` 之类会写到别处。
var conversationPattern = regexp.MustCompile(`^[A-Za-z0-9._-]{1,64}$`)

func validConversation(id string) bool { return conversationPattern.MatchString(id) }

// routes 路由表。**方法+路径模式**（Go 1.22+ 的 ServeMux）：非法方法自动 405。
//
// 静态页不鉴权（页面本身不含机密，登录框就在页面里）；接口一律鉴权。
func (c *Channel) routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /", c.handleStatic)
	mux.HandleFunc("POST /login", c.handleLogin)
	mux.HandleFunc("GET /events", c.requireAuth(c.handleEvents))
	mux.HandleFunc("POST /messages", c.requireAuth(c.handleMessages))
	mux.HandleFunc("POST /stop", c.requireAuth(c.handleStop))
	mux.HandleFunc("GET /files/{id}", c.requireAuth(c.handleFiles))
	return mux
}

// ── 认证 ────────────────────────────────────────────────

// principalOf 用 token 反查 PrincipalID。**遍历整张表做常量时间比较**：
// 表很小，且避免「命中即返回」泄露前缀。空 token 直接不认。
func (c *Channel) principalOf(token string) (string, bool) {
	if token == "" {
		return "", false
	}
	principal := ""
	found := false
	for known, name := range c.users {
		if tokenEqual(token, known) {
			principal, found = name, true
		}
	}
	return principal, found
}

// principalFor 认证并取出这条请求的 PrincipalID。两种携带方式：Bearer 头
// （非浏览器客户端）、cookie（浏览器）。
//
// 为什么 browser 走 cookie：`EventSource` 不能带自定义头，`<img>`/`<a>` 也不能——
// 只有 cookie 能让 SSE、媒体、POST 三处统一认证，且令牌不进 URL、不进日志。
func (c *Channel) principalFor(r *http.Request) (string, bool) {
	if header := r.Header.Get("Authorization"); strings.HasPrefix(header, "Bearer ") {
		return c.principalOf(strings.TrimSpace(strings.TrimPrefix(header, "Bearer ")))
	}
	if cookie, err := r.Cookie(cookieName); err == nil {
		return c.principalOf(cookie.Value)
	}
	return "", false
}

// tokenEqual 常量时间比较，避免按字符提前返回泄露令牌长度。
func tokenEqual(got, want string) bool {
	return subtle.ConstantTimeCompare([]byte(got), []byte(want)) == 1
}

// authHandler 认证后的处理器。**principally 参数是这条请求的 PrincipalID**——
// 认证与取身份是同一步，所以由这里传给下游，而不是让每个 handler 再查一遍。
type authHandler func(http.ResponseWriter, *http.Request, string)

func (c *Channel) requireAuth(next authHandler) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		principal, ok := c.principalFor(r)
		if !ok {
			writeJSON(w, http.StatusUnauthorized, map[string]any{"error": "未认证"})
			return
		}
		next(w, r, principal)
	}
}

// handleLogin 用 token 换一个 HttpOnly cookie。
func (c *Channel) handleLogin(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Token string `json:"token"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096)).Decode(&body); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "请求体不是合法 JSON"})
		return
	}
	token := strings.TrimSpace(body.Token)
	if _, ok := c.principalOf(token); !ok {
		config.Log().Warn(config.TypeCHAN, "网页渠道登录失败", config.Context{
			"addr": c.addr, "remote": r.RemoteAddr,
		})
		writeJSON(w, http.StatusUnauthorized, map[string]any{"error": "令牌不对"})
		return
	}
	// cookie 里放 token 本身；服务端再反查 principal。HttpOnly 让 JS 读不到它。
	http.SetCookie(w, &http.Cookie{
		Name:     cookieName,
		Value:    token,
		Path:     "/",
		HttpOnly: true,
		SameSite: http.SameSiteStrictMode,
	})
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// ── 入站 ────────────────────────────────────────────────

// handleMessages 浏览器发一条消息。**产出一条 InboundMessage 交给接缝**——
// 之后的一切（分发、问答、回话）与微信渠道走完全相同的路径。
//
// 支持两种线格式：**JSON**（纯文字，旧行为不变）与 **multipart**（带图：字段
// conversation/text + 若干 `image` 文件）。图片字节落进 `inbound` 暂存，产出的
// part 只带句柄——消息层稍后经 `FetchMedia` 取回，与微信走同一条路。
func (c *Channel) handleMessages(w http.ResponseWriter, r *http.Request, principal string) {
	conversation, text, images, ok := c.parseInbound(w, r)
	if !ok {
		return // parseInbound 已经写好应答
	}
	if !validConversation(conversation) {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "会话 id 不合法"})
		return
	}
	if strings.TrimSpace(text) == "" && len(images) == 0 {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "text 与文件至少要有一个"})
		return
	}

	c.mu.Lock()
	onMessage := c.onMessage
	c.mu.Unlock()
	if onMessage == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{"error": "渠道还没开始接收"})
		return
	}

	parts := make([]channels.Part, 0, len(images)+1)
	if text != "" {
		parts = append(parts, channels.TextPart(text))
	}
	parts = append(parts, images...)

	onMessage(channels.InboundMessage{
		ChannelID:   ID,
		AccountID:   c.accountID,
		MessageID:   newID(),
		SenderID:    principal,
		PrincipalID: principal,
		RecipientID: c.accountID,
		// 内部会话键纳入 principal：两个用户各取同名 conversation 也不会串（见 sessionConversation）
		ConversationID: sessionConversation(principal, conversation),
		Parts:          parts,
		Timestamp:      nowMillis(),
	})

	writeJSON(w, http.StatusAccepted, map[string]any{"ok": true})
}

// parseInbound 读一条入站请求：会话 id、文字、图片 part。按 Content-Type 分派。
// 失败时**已经写好应答**，返回 ok=false。
func (c *Channel) parseInbound(w http.ResponseWriter, r *http.Request) (conversation, text string, images []channels.Part, ok bool) {
	if strings.HasPrefix(r.Header.Get("Content-Type"), "multipart/form-data") {
		return c.parseMultipartInbound(w, r)
	}

	var body struct {
		Conversation string `json:"conversation"`
		Text         string `json:"text"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&body); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "请求体不是合法 JSON"})
		return "", "", nil, false
	}
	return body.Conversation, body.Text, nil, true
}

// parseMultipartInbound 读带文件的那条路。**接受任意文件**（不限图片）：每个文件落进
// `inbound` 暂存，按嗅探出的 MIME 定 part 的 Kind（图片是 KindImage、其余是 KindFile），
// 由消息层再按三类语义分派（图片作附件、文本并进问题、二进制给元信息）。
func (c *Channel) parseMultipartInbound(w http.ResponseWriter, r *http.Request) (string, string, []channels.Part, bool) {
	r.Body = http.MaxBytesReader(w, r.Body, maxUploadBodyBytes)
	if err := r.ParseMultipartForm(maxUploadBodyBytes); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "上传解析失败（或超过上限）"})
		return "", "", nil, false
	}

	// 字段名排序，让同一批文件的 part 顺序稳定（map 遍历随机）
	fieldNames := make([]string, 0, len(r.MultipartForm.File))
	for name := range r.MultipartForm.File {
		fieldNames = append(fieldNames, name)
	}
	sort.Strings(fieldNames)

	var files []channels.Part
	for _, field := range fieldNames {
		for _, header := range r.MultipartForm.File[field] {
			data, err := readUploadedFile(header)
			if err != nil {
				writeJSON(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
				return "", "", nil, false
			}
			mime := http.DetectContentType(data)
			id, stored := c.inbound.put(data, header.Filename, mime)
			if !stored {
				writeJSON(w, http.StatusInsufficientStorage, map[string]any{"error": "文件暂存超限，请少发几个"})
				return "", "", nil, false
			}
			kind := channels.KindFile
			if strings.HasPrefix(mime, "image/") {
				kind = channels.KindImage
			}
			files = append(files, channels.Part{
				Kind: kind,
				Media: channels.MediaRef{
					ID: id, FileName: header.Filename, MimeType: mime, Size: int64(len(data)),
				},
			})
		}
	}
	return r.FormValue("conversation"), r.FormValue("text"), files, true
}

// readUploadedFile 读一个上传的文件，**读取时就卡上限**（不先分配再拒）。
func readUploadedFile(header *multipart.FileHeader) ([]byte, error) {
	file, err := header.Open()
	if err != nil {
		return nil, errors.New("文件打不开")
	}
	defer file.Close()

	data, err := io.ReadAll(io.LimitReader(file, maxInboundFileBytes+1))
	if err != nil {
		return nil, errors.New("文件读不了")
	}
	if int64(len(data)) > maxInboundFileBytes {
		return nil, fmt.Errorf("单个文件超过 %d MiB", maxInboundFileBytes>>20)
	}
	if len(data) == 0 {
		return nil, errors.New("文件是空的")
	}
	return data, nil
}

// handleStop 停掉当前会话进行中的一轮（网页「停止」按钮）。**没有在跑就如实说没有**，
// 不假装成功——前端据此决定要不要把键切回「发送」。
func (c *Channel) handleStop(w http.ResponseWriter, r *http.Request, principal string) {
	var body struct {
		Conversation string `json:"conversation"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096)).Decode(&body); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "请求体不是合法 JSON"})
		return
	}
	if !validConversation(body.Conversation) {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "会话 id 不合法"})
		return
	}
	stopped := c.cancelTurn(principal, body.Conversation)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "stopped": stopped})
}

// ── SSE ─────────────────────────────────────────────────

// handleEvents 一条 SSE 长连接。客户端按会话订阅，服务端把答复与过程事件推来。
func (c *Channel) handleEvents(w http.ResponseWriter, r *http.Request, principal string) {
	raw := r.URL.Query().Get("conversation")
	if !validConversation(raw) {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "会话 id 不合法"})
		return
	}
	// 原始 conversation 只用于校验与回显；路由用纳入 principal 的内部键
	conversation := sessionConversation(principal, raw)
	flusher, ok := w.(http.Flusher)
	if !ok {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "这个连接不支持流式"})
		return
	}

	header := w.Header()
	header.Set("Content-Type", "text/event-stream")
	header.Set("Cache-Control", "no-cache")
	header.Set("Connection", "keep-alive")
	header.Set("X-Accel-Buffering", "no") // 让 nginx 之类别缓冲
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(sseFrame("ready", map[string]any{"conversation": raw, "account": c.accountID}))
	flusher.Flush()

	client := c.hub.join(conversation)
	defer c.hub.leave(conversation, client)

	ticker := time.NewTicker(keepAlive)
	defer ticker.Stop()

	for {
		select {
		case <-r.Context().Done():
			return
		case <-client.done:
			return
		case frame := <-client.ch:
			if _, err := w.Write(frame); err != nil {
				return
			}
			flusher.Flush()
		case <-ticker.C:
			// 注释帧：内容为空，只为穿过中间代理、并探活连接
			if _, err := w.Write([]byte(": ping\n\n")); err != nil {
				return
			}
			flusher.Flush()
		}
	}
}

// ── 出站媒体 ─────────────────────────────────────────────

// handleFiles 下载一份出站媒体。
func (c *Channel) handleFiles(w http.ResponseWriter, r *http.Request, _ string) {
	id := r.PathValue("id")
	item, ok := c.blobs.get(id)
	if !ok {
		http.NotFound(w, r)
		return
	}
	if item.mime != "" {
		w.Header().Set("Content-Type", item.mime)
	}
	if item.name != "" {
		// inline：图片/视频/音频/PDF 由前端**直接显示在页面上**，不做附件下载。
		w.Header().Set("Content-Disposition", "inline; filename=\""+sanitizeFilename(item.name)+"\"")
	}
	// SVG 是文本（可内联 <script>），而这个端点在客户端**同源**上。用 <img> 嵌它时脚本
	// 本就不执行，但用户直接打开这条 URL 时脚本会在同源下跑——那是个 XSS 面。所以把
	// 媒体**内联展示**（不退回附件下载）的同时，禁掉脚本与外链，让同源不再是可乘之机。
	w.Header().Set("Content-Security-Policy", "default-src 'none'; style-src 'unsafe-inline'; script-src 'none'; img-src 'self' data:; media-src 'self'")
	_, _ = w.Write(item.data)
}

// ── 静态页 ──────────────────────────────────────────────

// handleStatic 从静态目录读文件。**禁止目录列表，且拒绝越界路径**。
// 静态目录不存在时 `/` 返回一段说明而不是 500——「页面放错了地方」要一眼看得出。
func (c *Channel) handleStatic(w http.ResponseWriter, r *http.Request) {
	rel := path.Clean("/" + r.URL.Path)
	if rel == "/" {
		rel = "/index.html"
	}
	full := filepath.Join(c.staticDir, filepath.FromSlash(rel))
	if !within(c.staticDir, full) {
		http.NotFound(w, r)
		return
	}
	info, err := os.Stat(full)
	if err != nil || info.IsDir() {
		if rel == "/index.html" {
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			_, _ = w.Write([]byte("<!doctype html><meta charset=utf-8><title>fka web</title>" +
				"<body style='font-family:system-ui;max-width:40rem;margin:3rem auto'>" +
				"<h1>前端目录不存在</h1><p>把页面放到：<code>" + html.EscapeString(c.staticDir) + "</code></p>" +
				"<p>或用 <code>" + EnvStatic + "</code> 指定目录。</p>"))
			return
		}
		http.NotFound(w, r)
		return
	}
	http.ServeFile(w, r, full)
}

// within 判断 path 是否在 root 内（含 root 本身）。
func within(root, target string) bool {
	rel, err := filepath.Rel(root, target)
	if err != nil {
		return false
	}
	return rel == "." || (rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)))
}

// ── 小工具 ──────────────────────────────────────────────

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}

// nowMillis 毫秒时间戳，与 InboundMessage.Timestamp 的口径一致。
func nowMillis() int64 { return time.Now().UnixMilli() }

// sanitizeFilename 去掉会破坏 Content-Disposition 头的字符。
func sanitizeFilename(name string) string {
	return strings.NewReplacer(`"`, "", `\`, "", "\r", "", "\n", "").Replace(name)
}
