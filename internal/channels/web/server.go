package web

import (
	"crypto/subtle"
	"encoding/json"
	"html"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/zjzhang-cn/fka-go/internal/channels"
	"github.com/zjzhang-cn/fka-go/internal/config"
)

// cookieName 认证 cookie 名。**HttpOnly**：JS 读不到它；SSE 与媒体请求会自动带上。
const cookieName = "fka_web_token"

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

// handleMessages 浏览器发一条文字。**产出一条 InboundMessage 交给接缝**——
// 之后的一切（分发、问答、回话）与微信渠道走完全相同的路径。
func (c *Channel) handleMessages(w http.ResponseWriter, r *http.Request, principal string) {
	var body struct {
		Conversation string `json:"conversation"`
		Text         string `json:"text"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&body); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "请求体不是合法 JSON"})
		return
	}
	if !validConversation(body.Conversation) {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "会话 id 不合法"})
		return
	}
	if strings.TrimSpace(body.Text) == "" {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "text 不能为空"})
		return
	}

	c.mu.Lock()
	onMessage := c.onMessage
	c.mu.Unlock()
	if onMessage == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{"error": "渠道还没开始接收"})
		return
	}

	onMessage(channels.InboundMessage{
		ChannelID:   ID,
		AccountID:   c.accountID,
		MessageID:   newID(),
		SenderID:    principal,
		PrincipalID: principal,
		RecipientID: c.accountID,
		// 内部会话键纳入 principal：两个用户各取同名 conversation 也不会串（见 sessionConversation）
		ConversationID: sessionConversation(principal, body.Conversation),
		Parts:          []channels.Part{channels.TextPart(body.Text)},
		Timestamp:      nowMillis(),
	})

	writeJSON(w, http.StatusAccepted, map[string]any{"ok": true})
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
		// inline：图片能被 <img> 直接渲染；文件由前端的 <a download> 触发下载
		w.Header().Set("Content-Disposition", "inline; filename=\""+sanitizeFilename(item.name)+"\"")
	}
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
