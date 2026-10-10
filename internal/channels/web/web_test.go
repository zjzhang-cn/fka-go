// 网页渠道的用例：能力声明、多用户认证、入站产出、SSE 推送、媒体收发、静态兜底。
//
// 这一层的判据与接缝一致：**业务层不认识它**。所以这里只测渠道自己的边界——
// HTTP 面的东西（越权、越界、非法会话 id、跨用户串台）与微信渠道那批「静默失效」
// 一一对应。
package web

import (
	"bufio"
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/zjzhang-cn/fka-go/internal/channels"
	"github.com/zjzhang-cn/fka-go/internal/config"
)

// testAccount 测试用的账号名。**不复用 defaultAccount**：用例不该因为默认值改了
// 就跟着红——那条约束（默认不叫 web）由它自己的用例钉。
const testAccount = "acct"

// testUsers 两个用户：secret → web:alice，bobtok → web:bob。
func testUsers() map[string]string {
	return map[string]string{"secret": "web:alice", "bobtok": "web:bob"}
}

func testChannel(t *testing.T) *Channel {
	t.Helper()
	return newChannel(testAccount, "127.0.0.1:0", testUsers(), "")
}

func cookie(token string) *http.Cookie {
	return &http.Cookie{Name: cookieName, Value: token}
}

// ── Provider ────────────────────────────────────────────

// TestProvider_未配置不产实例 默认不接网页渠道是常态，不是故障。
func TestProvider_未配置不产实例(t *testing.T) {
	t.Setenv(EnvAddr, "")
	p := &Provider{}
	chans, err := p.Create(context.Background())
	if err != nil || len(chans) != 0 {
		t.Fatalf("未配置时该产出 0 个实例，实际 %d，err=%v", len(chans), err)
	}
}

// TestProvider_有地址没用户要拒 不经认证把 agent 放到 HTTP 上是结构性风险；
// 配了地址却一个用户都没有，同样拒绝启用。
func TestProvider_有地址没用户要拒(t *testing.T) {
	t.Setenv("FKA_HOME", t.TempDir())
	t.Setenv(EnvToken, "")
	p := &Provider{Addr: "127.0.0.1:0"}
	if _, err := p.Create(context.Background()); err == nil {
		t.Fatal("配了地址却没配用户，该拒绝启用")
	}
}

// TestProvider_启用产一个实例且能力自洽 一个实例、Text 收发、File/Image 仅发、
// 主动推送为真；能力声明与发送器必须对得上（ValidateChannel）。
func TestProvider_启用产一个实例且能力自洽(t *testing.T) {
	p := &Provider{Addr: "127.0.0.1:0", Users: []User{{Token: "t", User: "u"}}}
	chans, err := p.Create(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(chans) != 1 {
		t.Fatalf("该产 1 个实例，实际 %d", len(chans))
	}
	if err := channels.ValidateChannel(chans[0]); err != nil {
		t.Fatalf("能力声明与发送器不一致：%v", err)
	}
	caps := chans[0].Capabilities()
	if !caps.Text.Send || !caps.Text.Receive {
		t.Error("文字该收发都支持")
	}
	if !caps.File.Send || caps.File.Receive || !caps.Image.Send || caps.Image.Receive {
		t.Error("文件/图片该只发不收（业务层没有入站媒体的消费者）")
	}
	if !caps.ProactivePush {
		t.Error("SSE 能主动推，ProactivePush 该为真")
	}
}

// TestProvider_身份加命名空间 用户表里写裸名，落到 PrincipalID 要带 web: 前缀。
func TestProvider_身份加命名空间(t *testing.T) {
	p := &Provider{Addr: "127.0.0.1:0", Users: []User{{Token: "t", User: "alice"}}}
	users, err := p.loadUsers()
	if err != nil {
		t.Fatal(err)
	}
	if got := users["t"]; got != "web:alice" {
		t.Errorf("身份该补成 web:alice，实际 %q", got)
	}

	// 已经是 web: 前缀的原样保留（兼容旧配置）
	p2 := &Provider{Addr: "127.0.0.1:0", Users: []User{{Token: "t", User: "web:default"}}}
	users2, _ := p2.loadUsers()
	if got := users2["t"]; got != "web:default" {
		t.Errorf("已命名的身份不该再加前缀，实际 %q", got)
	}
}

// TestProvider_用户表缺字段报错 静默跳过会让「我明明配了这个人」变成无头案。
func TestProvider_用户表缺字段报错(t *testing.T) {
	p := &Provider{Addr: "127.0.0.1:0", Users: []User{{Token: "t", User: ""}}}
	if _, err := p.Create(context.Background()); err == nil {
		t.Fatal("缺 user 该报错")
	}
}

// ── 会话 id 与认证 ──────────────────────────────────────

// Test会话id校验 会话 id 进 SessionKey、最终进历史文件名，必须限制字符集与长度。
func Test会话id校验(t *testing.T) {
	for _, ok := range []string{"abc", "web-123", "a.b_c", "A1"} {
		if !validConversation(ok) {
			t.Errorf("%q 该合法", ok)
		}
	}
	for _, bad := range []string{"", "../etc", "a/b", "带空格", strings.Repeat("x", 65), "a?b"} {
		if validConversation(bad) {
			t.Errorf("%q 该被拒", bad)
		}
	}
}

// Test认证_无令牌被拒 接口默认要认证。
func Test认证_无令牌被拒(t *testing.T) {
	c := testChannel(t)
	ts := httptest.NewServer(c.routes())
	defer ts.Close()

	resp, err := http.Post(ts.URL+"/messages", "application/json",
		strings.NewReader(`{"conversation":"conv","text":"hi"}`))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("未认证该 401，实际 %d", resp.StatusCode)
	}
}

// Test登录_令牌对错 错的 401、对的种下 HttpOnly cookie。
func Test登录_令牌对错(t *testing.T) {
	c := testChannel(t)
	ts := httptest.NewServer(c.routes())
	defer ts.Close()

	post := func(token string) *http.Response {
		resp, err := http.Post(ts.URL+"/login", "application/json",
			strings.NewReader(`{"token":"`+token+`"}`))
		if err != nil {
			t.Fatal(err)
		}
		return resp
	}

	bad := post("nope")
	bad.Body.Close()
	if bad.StatusCode != http.StatusUnauthorized {
		t.Fatalf("错令牌该 401，实际 %d", bad.StatusCode)
	}

	good := post("secret")
	defer good.Body.Close()
	if good.StatusCode != http.StatusOK {
		t.Fatalf("对令牌该 200，实际 %d", good.StatusCode)
	}
	var found *http.Cookie
	for _, ck := range good.Cookies() {
		if ck.Name == cookieName {
			found = ck
		}
	}
	if found == nil || !found.HttpOnly {
		t.Fatalf("该种下 HttpOnly cookie：%+v", found)
	}
}

// ── 入站 ────────────────────────────────────────────────

// Test入站产出InboundMessage POST /messages 产出的形状要能被接缝与业务层直接用：
// 身份（含命名空间）、内部会话键、时间戳、文本 part 一个都不能少。
func Test入站产出InboundMessage(t *testing.T) {
	c := testChannel(t)
	var got channels.InboundMessage
	c.onMessage = func(m channels.InboundMessage) { got = m }

	ts := httptest.NewServer(c.routes())
	defer ts.Close()

	req, _ := http.NewRequest("POST", ts.URL+"/messages",
		strings.NewReader(`{"conversation":"room-1","text":"你好"}`))
	req.AddCookie(cookie("secret"))
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("该 202，实际 %d", resp.StatusCode)
	}

	if got.ChannelID != ID || got.AccountID != testAccount {
		t.Errorf("渠道/账号不对：%+v", got)
	}
	if got.PrincipalID != "web:alice" {
		t.Errorf("身份该是 token 对应的 principal：%q", got.PrincipalID)
	}
	if want := sessionConversation("web:alice", "room-1"); got.ConversationID != want {
		t.Errorf("内部会话键该是 %q，实际 %q", want, got.ConversationID)
	}
	if got.Text() != "你好" {
		t.Errorf("正文不对：%q", got.Text())
	}
	if got.MessageID == "" || got.Timestamp == 0 {
		t.Errorf("消息 id / 时间戳不能空：%+v", got)
	}
}

// Test多用户_同名会话不串 两个用户各自取同一个 conversation id，内部会话键必须
// 分开——否则历史、记忆、SSE 房间全串在一起，且不报错。
func Test多用户_同名会话不串(t *testing.T) {
	c := testChannel(t)
	var seen []channels.InboundMessage
	c.onMessage = func(m channels.InboundMessage) { seen = append(seen, m) }
	ts := httptest.NewServer(c.routes())
	defer ts.Close()

	post := func(token string) {
		req, _ := http.NewRequest("POST", ts.URL+"/messages",
			strings.NewReader(`{"conversation":"room","text":"hi"}`))
		req.AddCookie(cookie(token))
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
	}
	post("secret") // web:alice
	post("bobtok") // web:bob

	if len(seen) != 2 {
		t.Fatalf("该收到两条，实际 %d", len(seen))
	}
	if seen[0].ConversationID == seen[1].ConversationID {
		t.Fatalf("两个用户的同名会话不该落同一个内部键：%q", seen[0].ConversationID)
	}
}

// Test入站_非法会话id被拒。
func Test入站_非法会话id被拒(t *testing.T) {
	c := testChannel(t)
	called := false
	c.onMessage = func(channels.InboundMessage) { called = true }
	ts := httptest.NewServer(c.routes())
	defer ts.Close()

	req, _ := http.NewRequest("POST", ts.URL+"/messages",
		strings.NewReader(`{"conversation":"../x","text":"hi"}`))
	req.AddCookie(cookie("secret"))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("非法会话 id 该 400，实际 %d", resp.StatusCode)
	}
	if called {
		t.Error("被拒的请求不该走到 onMessage")
	}
}

// ── SSE 推送 ────────────────────────────────────────────

// TestSSE_收到答复 一条连接按会话订阅，Senders.Text 推来的文字要能读到。
func TestSSE_收到答复(t *testing.T) {
	c := testChannel(t)
	ts := httptest.NewServer(c.routes())
	defer ts.Close()

	req, _ := http.NewRequest("GET", ts.URL+"/events?conversation=conv", nil)
	req.AddCookie(cookie("secret"))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("SSE 该 200，实际 %d", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/event-stream") {
		t.Fatalf("Content-Type 不对：%q", ct)
	}

	reader := bufio.NewReader(resp.Body)
	if event, _ := nextEvent(t, reader); event != "ready" {
		t.Fatalf("第一帧该是 ready，实际 %q", event)
	}

	// 推一条文字到同一个用户的内部会话键。
	go func() {
		time.Sleep(50 * time.Millisecond)
		_, _ = c.sendText(context.Background(), channels.SendTextParams{
			Target: channels.SendTarget{ConversationID: sessionConversation("web:alice", "conv")},
			Text:   "答案在这",
		})
	}()

	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		event, data := nextEvent(t, reader)
		if event == "message" {
			if !strings.Contains(data, "答案在这") {
				t.Fatalf("data 不对：%s", data)
			}
			return
		}
	}
	t.Fatal("没等到 message 事件")
}

// nextEvent 读一帧 SSE，返回事件名与 data 行内容。超时后返回空。
func nextEvent(t *testing.T, reader *bufio.Reader) (string, string) {
	t.Helper()
	done := make(chan [2]string, 1)
	go func() {
		event, data := "", ""
		for {
			line, err := reader.ReadString('\n')
			if err != nil {
				break
			}
			line = strings.TrimRight(line, "\r\n")
			if strings.HasPrefix(line, "event: ") {
				event = strings.TrimPrefix(line, "event: ")
			} else if strings.HasPrefix(line, "data: ") {
				data = strings.TrimPrefix(line, "data: ")
			} else if line == "" {
				break // 帧结束
			}
		}
		done <- [2]string{event, data}
	}()
	select {
	case pair := <-done:
		return pair[0], pair[1]
	case <-time.After(2 * time.Second):
		return "", ""
	}
}

// ── 媒体 ────────────────────────────────────────────────

// Test出站媒体_暂存并可下载 字节以 URL 交给浏览器，不内联 base64。
func Test出站媒体_暂存并可下载(t *testing.T) {
	c := testChannel(t)
	ts := httptest.NewServer(c.routes())
	defer ts.Close()

	result, err := c.sendImage(context.Background(), channels.SendMediaParams{
		Target:   channels.SendTarget{ConversationID: sessionConversation("web:alice", "conv")},
		Data:     []byte("PNGDATA"),
		FileName: "pic.png",
		MimeType: "image/png",
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.MessageID == "" {
		t.Fatal("该返回一个下载 id")
	}

	req, _ := http.NewRequest("GET", ts.URL+"/files/"+result.MessageID, nil)
	req.AddCookie(cookie("secret"))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("下载该 200，实际 %d", resp.StatusCode)
	}
	if resp.Header.Get("Content-Type") != "image/png" {
		t.Errorf("Content-Type 该是 image/png：%q", resp.Header.Get("Content-Type"))
	}
	buf := make([]byte, 16)
	n, _ := resp.Body.Read(buf)
	if string(buf[:n]) != "PNGDATA" {
		t.Errorf("字节不对：%q", buf[:n])
	}
}

// Test出站媒体_下载要认证 媒体 URL 不能匿名可拉。
func Test出站媒体_下载要认证(t *testing.T) {
	c := testChannel(t)
	ts := httptest.NewServer(c.routes())
	defer ts.Close()

	id, _ := c.blobs.put([]byte("x"), "a.bin", "application/octet-stream")
	resp, err := http.Get(ts.URL + "/files/" + id)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("匿名下载该 401，实际 %d", resp.StatusCode)
	}
}

// ── 静态页与 Emitter ────────────────────────────────────

// Test静态目录不存在给说明 配错目录时要一眼看得出，而不是 500。
func Test静态目录不存在给说明(t *testing.T) {
	c := newChannel(testAccount, "127.0.0.1:0", testUsers(), "/nonexistent/"+newID())
	ts := httptest.NewServer(c.routes())
	defer ts.Close()

	resp, err := http.Get(ts.URL + "/")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("该 200 给一段说明，实际 %d", resp.StatusCode)
	}
	buf := make([]byte, 512)
	n, _ := resp.Body.Read(buf)
	if !strings.Contains(string(buf[:n]), "前端目录不存在") {
		t.Errorf("该说明目录缺失：%s", buf[:n])
	}
}

// TestEmitter_按会话绑定 没有会话就退回默认（返回 nil），有会话才输出。
func TestEmitter_按会话绑定(t *testing.T) {
	c := testChannel(t)
	if c.Emitter(context.Background()) != nil {
		t.Error("ctx 里没有会话时该返回 nil，退回默认回显")
	}
	ctx := config.Bind(context.Background(), config.Context{"conversation": "conv"})
	if c.Emitter(ctx) == nil {
		t.Error("有会话时该返回一个回显")
	}
}

// Test生命周期_起停 绑不上端口要当场报错；起来后状态是 online，停了是 offline。
func Test生命周期_起停(t *testing.T) {
	c := testChannel(t)
	ctx := context.Background()
	if err := c.Start(ctx, func(channels.InboundMessage) {}); err != nil {
		t.Fatalf("启动失败：%v", err)
	}
	if c.Status() != channels.StatusOnline {
		t.Errorf("起来后该 online，实际 %s", c.Status())
	}
	if err := c.Stop(ctx); err != nil {
		t.Fatalf("停机失败：%v", err)
	}
	if c.Status() != channels.StatusOffline {
		t.Errorf("停了该 offline，实际 %s", c.Status())
	}
}

// TestEmitter_不重复发答案 sseEmitter.Answer 必须是空的——答案由 Senders.Text 送出。
func TestEmitter_不重复发答案(t *testing.T) {
	c := testChannel(t)
	ctx := config.Bind(context.Background(), config.Context{"conversation": "conv"})

	client := c.hub.join("conv")
	defer c.hub.leave("conv", client)

	emitter := c.Emitter(ctx)
	if emitter == nil {
		t.Fatal("该有回显")
	}
	emitter.Answer("最终答案")
	select {
	case frame := <-client.ch:
		t.Fatalf("Answer 不该推帧（会与 Senders.Text 重复）：%s", frame)
	case <-time.After(100 * time.Millisecond):
		// 符合预期
	}

	emitter.Reasoning("想一下")
	select {
	case frame := <-client.ch:
		if !strings.Contains(string(frame), "reasoning") {
			t.Fatalf("该推 reasoning 帧：%s", frame)
		}
	case <-time.After(time.Second):
		t.Fatal("推理增量没推出来")
	}
}

// TestProvider_账号可配且默认不叫web 账号名进会话键、进而进历史文件名，所以要能改；
// 默认刻意不与渠道种类同名。
func TestProvider_账号可配且默认不叫web(t *testing.T) {
	t.Setenv(EnvAccount, "")

	p := &Provider{Addr: "127.0.0.1:0", Users: []User{{Token: "t", User: "u"}}}
	chans, err := p.Create(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if got := chans[0].AccountID(); got != defaultAccount {
		t.Errorf("默认账号该是 %q，实际 %q", defaultAccount, got)
	}
	if defaultAccount == ID {
		t.Error("默认账号名不该与渠道种类同名")
	}
	if _, ok := p.ResolveAccount(defaultAccount); !ok {
		t.Error("默认账号名该能被解析")
	}
	if _, ok := p.ResolveAccount(""); !ok {
		t.Error("空选择器该解析成默认账号")
	}

	t.Setenv(EnvAccount, "home")
	chans, err = (&Provider{Addr: "127.0.0.1:0", Users: []User{{Token: "t", User: "u"}}}).Create(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if got := chans[0].AccountID(); got != "home" {
		t.Errorf("环境变量该覆盖账号名，实际 %q", got)
	}

	chans, err = (&Provider{Addr: "127.0.0.1:0", Users: []User{{Token: "t", User: "u"}}, Account: "explicit"}).Create(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if got := chans[0].AccountID(); got != "explicit" {
		t.Errorf("显式字段该压过环境变量，实际 %q", got)
	}
}
