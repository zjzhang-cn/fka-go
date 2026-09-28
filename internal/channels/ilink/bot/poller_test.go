package bot

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// ── 退避 ────────────────────────────────────────────────

// Test退避翻倍并封顶 没有上限的话，服务端挂十分钟就会让重试间隔涨到几小时——
// 而服务恢复时第一个用户要等几小时才收到回复。
func Test退避翻倍并封顶(t *testing.T) {
	b := NewBackoff(BackoffOptions{
		Initial: 2 * time.Second, Max: 10 * time.Second, Factor: 2, Jitter: 0,
	})

	want := []time.Duration{2, 4, 8, 10, 10, 10}
	for i, expected := range want {
		if got := b.Next(); got != expected*time.Second {
			t.Errorf("第 %d 次 = %v，期望 %v", i+1, got, expected*time.Second)
		}
	}
	if b.Attempts() != len(want) {
		t.Errorf("Attempts = %d，期望 %d", b.Attempts(), len(want))
	}
}

// Test成功后要Reset 不 reset 的话，一次偶发失败会把后续所有正常轮询的间隔
// 都顶在上限上。
func Test成功后要Reset(t *testing.T) {
	b := NewBackoff(BackoffOptions{Initial: time.Second, Max: time.Minute})
	_, _, _ = b.Next(), b.Next(), b.Next()

	b.Reset()
	if b.Attempts() != 0 {
		t.Errorf("Reset 后 Attempts = %d，期望 0", b.Attempts())
	}
	if got := b.Next(); got != time.Second {
		t.Errorf("Reset 后第一次该从头开始，实际 %v", got)
	}
}

// Test抖动不超上限 抖动是给多账号错开用的，但**不能把延迟顶到上限之外**。
func Test抖动不超上限(t *testing.T) {
	// Random 固定返回 1（最坏情况：延迟取到下界），再取 0（取到上界）
	for _, random := range []float64{0, 0.5, 1} {
		b := NewBackoff(BackoffOptions{
			Initial: 2 * time.Second, Max: 10 * time.Second, Jitter: 0.2,
			Random: func() float64 { return random },
		})
		for i := 0; i < 6; i++ {
			got := b.Next()
			if got > 10*time.Second {
				t.Errorf("random=%v 第 %d 次 = %v，越过了上限", random, i+1, got)
			}
			if got <= 0 {
				t.Errorf("random=%v 第 %d 次 = %v，非正", random, i+1, got)
			}
		}
	}
}

// Test退避参数非法时归一而不是报错 这些是部署参数，写错了顶多重试节奏不理想，
// 不该让账号起不来。
func Test退避参数非法时归一而不是报错(t *testing.T) {
	for name, options := range map[string]BackoffOptions{
		"initial 为 0":    {Initial: 0, Max: time.Minute},
		"max 小于 initial": {Initial: time.Minute, Max: time.Second},
		"factor 小于 1":    {Initial: time.Second, Max: time.Minute, Factor: 0.5},
		"jitter 越界":      {Initial: time.Second, Max: time.Minute, Jitter: 5},
	} {
		b := NewBackoff(options)
		if got := b.Next(); got <= 0 {
			t.Errorf("%s：%v", name, got)
		}
	}
}

// ── 游标存储 ────────────────────────────────────────────

func Test游标往返(t *testing.T) {
	path := filepath.Join(t.TempDir(), "cursor.json")
	store := NewCursorStore(path)

	// 从未见过该账号时是空串——**那是「从头开始」，不是错误**
	if got := store.Get("acct-1"); got != "" {
		t.Errorf("未见过的账号该返回空串，实际 %q", got)
	}

	store.Set("acct-1", "buf-1")
	store.Set("acct-2", "buf-2")

	// 重新打开要能读回来——**游标必须活过进程**
	reopened := NewCursorStore(path)
	if reopened.Get("acct-1") != "buf-1" || reopened.Get("acct-2") != "buf-2" {
		t.Errorf("重开后读不回来：%q %q", reopened.Get("acct-1"), reopened.Get("acct-2"))
	}
	if reopened.Size() != 2 {
		t.Errorf("Size = %d，期望 2", reopened.Size())
	}
}

func Test游标无变化不写盘(t *testing.T) {
	path := filepath.Join(t.TempDir(), "cursor.json")
	store := NewCursorStore(path)

	store.Set("a", "buf-1")
	first, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}

	// 长轮询每轮都会拿到新 buf，而绝大多数轮次没消息——无变化还写盘会把磁盘写满
	time.Sleep(10 * time.Millisecond)
	store.Set("a", "buf-1")

	second, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if !first.ModTime().Equal(second.ModTime()) {
		t.Error("游标没变时不该写盘")
	}
}

func Test游标文件损坏当空处理(t *testing.T) {
	path := filepath.Join(t.TempDir(), "cursor.json")
	if err := os.WriteFile(path, []byte("{这不是 JSON"), 0o600); err != nil {
		t.Fatal(err)
	}

	store := NewCursorStore(path)
	// 一个坏游标文件不该让账号起不来：最坏是重放一次消息（幂等可容忍），
	// 而拒绝启动是确定的损失
	if got := store.Size(); got != 0 {
		t.Errorf("坏文件该当没有，实际 %d 条", got)
	}
	store.Set("a", "buf-1")
	if got := store.Get("a"); got != "buf-1" {
		t.Errorf("坏文件之后仍该能写：%q", got)
	}
}

func Test清游标(t *testing.T) {
	path := filepath.Join(t.TempDir(), "cursor.json")
	store := NewCursorStore(path)
	store.Set("a", "buf-1")

	store.Clear("a")
	if got := store.Get("a"); got != "" {
		t.Errorf("该清掉，实际 %q", got)
	}
	// 清掉的那个不该残留在文件里——否则重开进程它又回来了
	if got := NewCursorStore(path).Get("a"); got != "" {
		t.Errorf("清掉的游标不该留在文件里，实际 %q", got)
	}
	// 清一个不存在的账号不该建出文件
	store.Clear("nope")
}

// ── 长轮询 ──────────────────────────────────────────────

// pollerFixture 起一个假服务端 + 一个轮询器。
type pollerFixture struct {
	server    *httptest.Server
	cursors   *CursorStore
	poller    *Poller
	path      string
	replies   []string
	callCount int32
}

// newPollerFixture replies 依次作为每轮 getupdates 的应答；用完最后一轮就一直返回它。
func newPollerFixture(t *testing.T, replies ...string) *pollerFixture {
	t.Helper()

	fixture := &pollerFixture{replies: replies, path: filepath.Join(t.TempDir(), "cursor.json")}

	var server *httptest.Server
	server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/ilink/bot/getupdates" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		index := int(atomic.AddInt32(&fixture.callCount, 1)) - 1
		if index >= len(fixture.replies) {
			index = len(fixture.replies) - 1
		}
		if index < 0 || len(fixture.replies) == 0 {
			_, _ = w.Write([]byte(`{"msgs":[]}`))
			return
		}
		_, _ = w.Write([]byte(fixture.replies[index]))
	}))
	fixture.server = server
	t.Cleanup(server.Close)

	fixture.cursors = NewCursorStore(fixture.path)
	fixture.poller = NewPoller(
		WeixinAccount{ID: "acct-1", BaseURL: server.URL, BotToken: "t"},
		fixture.cursors, server.Client())
	return fixture
}

// waitFor 轮询直到条件成立。
func waitFor(t *testing.T, what string, condition func() bool) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if condition() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("3 秒内没等到：%s", what)
}

// Test长轮询收消息并推进游标
func Test长轮询收消息并推进游标(t *testing.T) {
	fixture := newPollerFixture(t, `{"msgs":[`+realTextMessage+`],"get_updates_buf":"buf-1"}`)

	var mu sync.Mutex
	var got []WeixinMessage
	fixture.poller.OnMessage = func(message WeixinMessage) {
		mu.Lock()
		defer mu.Unlock()
		got = append(got, message)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	fixture.poller.Start(ctx)
	defer fixture.poller.Stop()

	waitFor(t, "收到消息", func() bool {
		mu.Lock()
		defer mu.Unlock()
		return len(got) == 1
	})

	mu.Lock()
	first := got[0]
	mu.Unlock()

	if first.MessageID != "7391827364518293647382" {
		t.Errorf("message_id = %q", first.MessageID)
	}
	if first.AccountID != "acct-1" {
		t.Errorf("账号该由轮询器注入：%q", first.AccountID)
	}
	// **游标要先落盘再上抛消息**：万一上抛途中进程被杀，宁可重放也不要漏
	waitFor(t, "游标落盘", func() bool { return fixture.cursors.Get("acct-1") == "buf-1" })
}

// Test没有消息也推进游标 服务端每轮都返回新 buf，只在有消息时推进会导致
// **重复拉取同一区间**。
func Test没有消息也推进游标(t *testing.T) {
	fixture := newPollerFixture(t,
		`{"msgs":[],"get_updates_buf":"buf-1"}`,
		`{"msgs":[],"get_updates_buf":"buf-2"}`)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	fixture.poller.Start(ctx)
	defer fixture.poller.Stop()

	waitFor(t, "游标推进到 buf-1", func() bool { return fixture.cursors.Get("acct-1") == "buf-1" })
	waitFor(t, "游标推进到 buf-2", func() bool { return fixture.cursors.Get("acct-1") == "buf-2" })
}

// Test请求体带当前游标 不带就等于每次从头拉。
func Test请求体带当前游标(t *testing.T) {
	var gotBufs []string
	var mu sync.Mutex

	fixture := newPollerFixture(t, `{"msgs":[],"get_updates_buf":"buf-1"}`)
	fixture.server.Close()

	var server *httptest.Server
	server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			GetUpdatesBuf string `json:"get_updates_buf"`
			BaseInfo      struct {
				ChannelVersion string `json:"channel_version"`
			} `json:"base_info"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)

		mu.Lock()
		gotBufs = append(gotBufs, body.GetUpdatesBuf)
		mu.Unlock()

		if body.BaseInfo.ChannelVersion != ChannelVersion {
			t.Errorf("该带 base_info.channel_version，实际 %q", body.BaseInfo.ChannelVersion)
		}
		_, _ = w.Write([]byte(`{"msgs":[],"get_updates_buf":"buf-` +
			fmt.Sprint(len(gotBufs)) + `"}`))
	}))
	defer server.Close()

	cursors := NewCursorStore(fixture.path)
	cursors.Set("acct-1", "buf-start")

	poller := NewPoller(WeixinAccount{ID: "acct-1", BaseURL: server.URL, BotToken: "t"},
		cursors, server.Client())
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	poller.Start(ctx)
	defer poller.Stop()

	waitFor(t, "发出至少两轮", func() bool {
		mu.Lock()
		defer mu.Unlock()
		return len(gotBufs) >= 2
	})

	mu.Lock()
	defer mu.Unlock()
	if gotBufs[0] != "buf-start" {
		t.Errorf("第一轮该带已存的游标，实际 %q", gotBufs[0])
	}
	// 第二轮该带上一轮拿回来的
	if gotBufs[1] != "buf-1" {
		t.Errorf("第二轮该带推进后的游标，实际 %q", gotBufs[1])
	}
}

// TestSession过期时退出循环并清游标 **游标必须清掉**：留着旧游标的话，
// 重新登录后服务端会以为客户端已消费到那一段，于是那段时间的消息**永久收不到**。
func TestSession过期时退出循环并清游标(t *testing.T) {
	fixture := newPollerFixture(t, `{"ret":-14}`)

	cursors := NewCursorStore(fixture.path)
	cursors.Set("acct-1", "buf-old")

	poller := NewPoller(WeixinAccount{ID: "acct-1", BaseURL: fixture.server.URL, BotToken: "t"},
		cursors, fixture.server.Client())

	expired := make(chan struct{})
	var once sync.Once
	poller.OnSessionExpired = func() { once.Do(func() { close(expired) }) }

	poller.Start(context.Background())
	select {
	case <-expired:
	case <-time.After(3 * time.Second):
		t.Fatal("3 秒内没收到 session 过期")
	}

	poller.Wait() // 循环该退出了
	if got := cursors.Get("acct-1"); got != "" {
		t.Errorf("过期时该清游标，实际还留着 %q", got)
	}
}

// Test协议失败不退避退出 ret 非零是**暂时的**（限流、服务端抖动），退避后重试。
// 退出的后果是账号静默离线。
func Test协议失败不退避退出(t *testing.T) {
	fixture := newPollerFixture(t,
		`{"ret":-1}`,
		`{"msgs":[],"get_updates_buf":"buf-ok"}`)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	fixture.poller.Start(ctx)
	defer fixture.poller.Stop()

	// 第一轮失败，第二轮成功 → 循环活下来了
	waitFor(t, "重试后推进游标", func() bool { return fixture.cursors.Get("acct-1") == "buf-ok" })
}

// Test停机后goroutine一定退出 直接返回的话，调用方紧接着关掉 HTTP 传输层，
// 而长轮询还挂在那儿等着应答——那会产生一堆看不懂的连接错误。
func Test停机后goroutine一定退出(t *testing.T) {
	fixture := newPollerFixture(t, `{"msgs":[],"get_updates_buf":"b"}`)

	fixture.poller.Start(context.Background())
	waitFor(t, "轮询在跑", func() bool { return atomic.LoadInt32(&fixture.callCount) > 0 })

	done := make(chan struct{})
	go func() {
		fixture.poller.Stop()
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("Stop 三秒内没返回")
	}

	// 停机后不该再发请求
	settled := atomic.LoadInt32(&fixture.callCount)
	time.Sleep(50 * time.Millisecond)
	if now := atomic.LoadInt32(&fixture.callCount); now != settled {
		t.Errorf("停机后还在发请求：%d → %d", settled, now)
	}
}

func Test重复Start是空操作(t *testing.T) {
	fixture := newPollerFixture(t, `{"msgs":[],"get_updates_buf":"b"}`)

	fixture.poller.Start(context.Background())
	defer fixture.poller.Stop()
	fixture.poller.Start(context.Background()) // 不该起第二个循环
	fixture.poller.Stop()                      // 幂等
}

// ── 登录 ────────────────────────────────────────────────

// Test码面内容不是令牌本身 **踩过的一个坑**：原先编码的是 qrcode 令牌本身
// （一串 32 位十六进制），扫出来是一段纯文本，微信不认——
// 码面看着完全正常，扫了却没反应。
func Test码面内容不是令牌本身(t *testing.T) {
	code := QRCode{
		QRCode:     "3f2a91c4d8e7b6051a2f3c4d5e6f7081",
		ImgContent: "https://liteapp.weixin.qq.com/q/7GiQu1?qrcode=3f2a91c4d8e7b6051a2f3c4d5e6f7081&bot_type=3",
	}

	payload, err := QRPayload(code)
	if err != nil {
		t.Fatalf("不该报错：%v", err)
	}
	if !strings.HasPrefix(payload, "https://liteapp.weixin.qq.com/q/") {
		t.Errorf("码面该是那个链接，实际 %q", payload)
	}
}

// Test码面里的令牌必须与轮询的一致 不一致时用户扫开的是**另一个没人在轮询的会话**——
// 表现为「扫了、也确认了，然后一直等下去」，是最难查的一类故障。
func Test码面里的令牌必须与轮询的一致(t *testing.T) {
	_, err := QRPayload(QRCode{
		QRCode:     "aaaa",
		ImgContent: "https://liteapp.weixin.qq.com/q/7GiQu1?qrcode=bbbb&bot_type=3",
	})
	if err == nil {
		t.Fatal("令牌不一致该报错")
	}
	if !strings.Contains(err.Error(), "不一致") {
		t.Errorf("该说清是令牌不一致：%v", err)
	}
}

func Test码面为空时报错(t *testing.T) {
	// 服务端行为变了。**先别扫码**——扫一个生成不出来的码只会让人白等
	_, err := QRPayload(QRCode{QRCode: "aaaa", ImgContent: "   "})
	if err == nil {
		t.Fatal("空码面该报错")
	}
	if !strings.Contains(err.Error(), "先别扫码") {
		t.Errorf("该提示先别扫码：%v", err)
	}
}

// Test码面不是URL时跳过校验 内容不一定是 URL，取不到参数就跳过——
// 但那也不能放过「取到了却不一致」。
func Test码面不是URL时跳过校验(t *testing.T) {
	if _, err := QRPayload(QRCode{QRCode: "aaaa", ImgContent: "纯文本内容"}); err != nil {
		t.Errorf("不是 URL 时不该报错：%v", err)
	}
}

// Test扫码轮询到确认 真起假服务端，按 wait→wait→confirmed 推进。
func Test扫码轮询到确认(t *testing.T) {
	var attempts int32
	var server *httptest.Server
	server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/ilink/bot/get_qrcode_status" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		// 这两个头是登录接口专用的，与 getupdates 那套鉴权不同
		if r.Header.Get("SKRouteTag") != "1001" {
			t.Errorf("SKRouteTag = %q", r.Header.Get("SKRouteTag"))
		}
		if r.URL.Query().Get("qrcode") != "token-1" {
			t.Errorf("该带 qrcode 参数，实际 %q", r.URL.Query().Get("qrcode"))
		}
		if atomic.AddInt32(&attempts, 1) < 3 {
			_, _ = w.Write([]byte(`{"status":"scaned"}`))
			return
		}
		_, _ = w.Write([]byte(`{"status":"confirmed","bot_token":"tok","ilink_bot_id":"b1",
		  "ilink_user_id":"u1","baseurl":"https://ilinkai.weixin.qq.com"}`))
	}))
	defer server.Close()

	// base URL 由调用方传，所以这里能指向本地服务器——写死的话会真的打到微信去
	c := newClient(WeixinAccount{BaseURL: server.URL}, server.Client())

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// 假服务端要第三次才 confirm，所以得像真实流程那样反复查
	var credentials Credentials
	for i := 0; i < 10; i++ {
		var done bool
		var err error
		credentials, done, err = checkQRCodeStatus(ctx, c, "token-1")
		if done {
			if err != nil {
				t.Fatalf("不该失败：%v", err)
			}
			break
		}
		if err != nil {
			t.Fatalf("不该失败：%v", err)
		}
		time.Sleep(10 * time.Millisecond)
	}
	if credentials.BotToken == "" {
		t.Fatal("轮询几次后该拿到凭证")
	}
	if credentials.BotToken != "tok" || credentials.ILinkBotID != "b1" || credentials.ILinkUserID != "u1" {
		t.Errorf("凭证没取全：%+v", credentials)
	}
	if credentials.BaseURL == "" {
		t.Error("baseurl 该取回来——没有它重启后连不上")
	}
}

func Test扫码过期是失败(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"status":"expired"}`))
	}))
	defer server.Close()

	c := newClient(WeixinAccount{BaseURL: server.URL}, server.Client())
	_, done, err := checkQRCodeStatus(context.Background(), c, "token-1")
	if !done {
		t.Error("过期该结束轮询")
	}
	if err == nil || !strings.Contains(err.Error(), "过期") {
		t.Errorf("该说清过期了：%v", err)
	}
}

// Test说已确认却不给Token不算成功 那不是「登录成功」，而是一个残缺的应答——
// 当成功处理的话，后面每一条消息都会带着空 token 去请求。
func Test说已确认却不给Token不算成功(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"status":"confirmed"}`))
	}))
	defer server.Close()

	c := newClient(WeixinAccount{BaseURL: server.URL}, server.Client())
	_, done, err := checkQRCodeStatus(context.Background(), c, "token-1")
	if !done {
		t.Error("该结束轮询")
	}
	if err == nil {
		t.Fatal("没给 token 时不该算成功")
	}
}

func Test扫码轮询可取消(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"status":"wait"}`))
	}))
	defer server.Close()

	// CLI 被 Ctrl-C 后服务不该继续替一个没人看的二维码轮询
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, err := PollQRCodeStatus(ctx, server.URL, server.Client(), "token-1", nil)
	if err == nil || !strings.Contains(err.Error(), "取消") {
		t.Errorf("该报已取消：%v", err)
	}
}

func Test空令牌直接拒(t *testing.T) {
	if _, err := PollQRCodeStatus(context.Background(), "", nil, "", nil); err == nil {
		t.Fatal("空令牌该报错")
	}
}

// ── .env 块替换 ─────────────────────────────────────────

// realEnv 一个贴近真实的 .env：账号 2 的标签**后面还有别的字**。
//
// 这是 `ReplaceAccountBlock` 存在的理由——原先的正则要求账号号后紧跟换行，
// 于是匹配失败，新配置被**追加到文件末尾**而不是就地替换，
// 导致 `ILINK_ACCOUNT_2_ID` 出现两次。dotenv 通常后者覆盖前者，表面能跑；
// 但一旦文件顺序变化，加载器就会读到旧的占位值，表现为「账号莫名其妙登不上」。
const realEnv = `# 拷贝成 .env 后填

# 账号1
ILINK_ACCOUNT_1_ID=account_001
ILINK_ACCOUNT_1_BOT_TOKEN=old-token-1
ILINK_ACCOUNT_1_BASE_URL=https://a
ILINK_ACCOUNT_1_BOT_ID=b1
ILINK_ACCOUNT_1_USER_ID=u1

# 账号2（可选）
ILINK_ACCOUNT_2_ID=account_002
ILINK_ACCOUNT_2_BOT_TOKEN=old-token-2
ILINK_ACCOUNT_2_BASE_URL=https://b
ILINK_ACCOUNT_2_BOT_ID=b2
ILINK_ACCOUNT_2_USER_ID=u2

# 其他配置
LOG_LEVEL=info
`

// Test就地替换而不是追加 追加会让同一个键出现两次，而一旦文件顺序变化，
// 加载器就会读到旧的占位值。
func Test就地替换而不是追加(t *testing.T) {
	updated := ReplaceAccountBlock(realEnv, 2, AccountBlock(2, Credentials{
		BotToken: "new-token", BaseURL: "https://new",
		ILinkBotID: "nb2", ILinkUserID: "nu2",
	}))

	if strings.Count(updated, "ILINK_ACCOUNT_2_ID=") != 1 {
		t.Errorf("键不该出现两次：\n%s", updated)
	}
	if strings.Contains(updated, "old-token-2") {
		t.Errorf("旧 token 该被换掉：\n%s", updated)
	}
	if !strings.Contains(updated, "ILINK_ACCOUNT_2_BOT_TOKEN=new-token") {
		t.Errorf("新 token 没写进去：\n%s", updated)
	}
	// **账号 1 一个字都不该动**
	if !strings.Contains(updated, "ILINK_ACCOUNT_1_BOT_TOKEN=old-token-1") {
		t.Errorf("账号 1 被改了：\n%s", updated)
	}
	// 块之后的其它配置要留下
	if !strings.Contains(updated, "LOG_LEVEL=info") {
		t.Errorf("后面的配置丢了：\n%s", updated)
	}
	// 块之前的其它配置也要留下
	if !strings.Contains(updated, "# 拷贝成 .env 后填") {
		t.Errorf("前面的配置丢了：\n%s", updated)
	}
}

func Test替换后每个键只出现一次(t *testing.T) {
	updated := ReplaceAccountBlock(realEnv, 1, AccountBlock(1, Credentials{
		BotToken: "t", BaseURL: "u", ILinkBotID: "b", ILinkUserID: "i",
	}))

	for _, key := range []string{"ILINK_ACCOUNT_1_ID", "ILINK_ACCOUNT_1_BOT_TOKEN",
		"ILINK_ACCOUNT_1_BASE_URL", "ILINK_ACCOUNT_1_BOT_ID", "ILINK_ACCOUNT_1_USER_ID"} {
		if n := strings.Count(updated, key+"="); n != 1 {
			t.Errorf("%s 出现了 %d 次，期望 1 次：\n%s", key, n, updated)
		}
	}
	// 账号 2 得原封不动
	if !strings.Contains(updated, "ILINK_ACCOUNT_2_BOT_TOKEN=old-token-2") {
		t.Errorf("账号 2 被改了：\n%s", updated)
	}
}

func Test账号不存在时追加(t *testing.T) {
	updated := ReplaceAccountBlock(realEnv, 3, AccountBlock(3, Credentials{
		BotToken: "t3", BaseURL: "u3", ILinkBotID: "b3", ILinkUserID: "u3",
	}))

	if !strings.Contains(updated, "ILINK_ACCOUNT_3_BOT_TOKEN=t3") {
		t.Errorf("该追加到末尾：\n%s", updated)
	}
	// 追加不该动已有的
	if !strings.Contains(updated, "ILINK_ACCOUNT_1_BOT_TOKEN=old-token-1") {
		t.Errorf("追加时动了已有配置：\n%s", updated)
	}
}

func Test空文件时追加(t *testing.T) {
	updated := ReplaceAccountBlock("", 1, AccountBlock(1, Credentials{BotToken: "t"}))
	if !strings.HasPrefix(updated, "# 账号 1") {
		t.Errorf("空文件该从块开始：%q", updated)
	}
	// 结尾要有换行，否则下一次追加会与最后一行粘在一起
	if !strings.HasSuffix(updated, "\n") {
		t.Errorf("结尾该有换行：%q", updated)
	}
}

// Test标签行后面带字也算数 `# 账号2（可选）` —— 账号号后还有别的字。
func Test标签行后面带字也算数(t *testing.T) {
	if !isAccountLabel("# 账号2（可选）", 2) {
		t.Error("「# 账号2（可选）」该被认成账号 2 的标签")
	}
	if !isAccountLabel("# 账号 2", 2) {
		t.Error("「# 账号 2」该被认成账号 2 的标签")
	}
	// 账号 1 的标签不能被认成账号 2 的
	if isAccountLabel("# 账号1", 2) {
		t.Error("「# 账号1」不该被认成账号 2 的标签")
	}
	// 「# 账号2」不该被认成**账号 12** 的标签——而账号 12 的块通常就排在它后面
	if isAccountLabel("# 账号2", 12) {
		t.Error("「# 账号2」不该被认成账号 12 的标签")
	}
	if !isAccountLabel("# 账号12（可选）", 12) {
		t.Error("「# 账号12（可选）」该被认成账号 12 的标签")
	}
	// 配置行不是标签行
	if isAccountLabel("ILINK_ACCOUNT_2_ID=account_002", 2) {
		t.Error("配置行不该被认成标签行")
	}
}

func Test配置行形状(t *testing.T) {
	for _, line := range []string{
		"ILINK_ACCOUNT_2_ID=account_002",
		"ILINK_ACCOUNT_2_BOT_TOKEN=x",
		"  ILINK_ACCOUNT_2_BASE_URL=https://b",
		"# ILINK_ACCOUNT_2_BOT_ID=b2", // 被注释掉的占位行也算
	} {
		if !isAccountSetting(line, 2) {
			t.Errorf("%q 该被认成账号 2 的配置行", line)
		}
	}
	// 下一个账号的配置行不该被吞进来
	for _, line := range []string{
		"ILINK_ACCOUNT_1_ID=account_001",
		"ILINK_ACCOUNT_20_ID=x",
		"ILINK_ACCOUNT_2_ID",
		"# 别的注释",
		"LOG_LEVEL=info",
	} {
		if isAccountSetting(line, 2) {
			t.Errorf("%q 不该被认成账号 2 的配置行", line)
		}
	}
}

func Test保存凭证写进文件(t *testing.T) {
	envPath := filepath.Join(t.TempDir(), ".env")
	if err := os.WriteFile(envPath, []byte(realEnv), 0o600); err != nil {
		t.Fatal(err)
	}

	if err := SaveCredentials(envPath, 2, Credentials{
		BotToken: "fresh-token", BaseURL: "https://fresh",
		ILinkBotID: "fb", ILinkUserID: "fu",
	}); err != nil {
		t.Fatal(err)
	}

	data, err := os.ReadFile(envPath)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Count(string(data), "ILINK_ACCOUNT_2_ID=") != 1 {
		t.Errorf("键不该出现两次：\n%s", data)
	}
	if !strings.Contains(string(data), "fresh-token") {
		t.Errorf("新凭证没写进去：\n%s", data)
	}

	// **权限是 0600**：里面有 bot token
	info, err := os.Stat(envPath)
	if err != nil {
		t.Fatal(err)
	}
	if mode := info.Mode().Perm(); mode != 0o600 {
		t.Errorf("权限 = %o，期望 600（里面有 bot token）", mode)
	}
}

func Test保存凭证到不存在的文件(t *testing.T) {
	envPath := filepath.Join(t.TempDir(), "sub", "dir", ".env")
	if err := SaveCredentials(envPath, 1, Credentials{BotToken: "t"}); err != nil {
		t.Fatalf("该自动建目录：%v", err)
	}
	if _, err := os.Stat(envPath); err != nil {
		t.Errorf("文件该建出来：%v", err)
	}
}

func Test槽位标识补零到三位(t *testing.T) {
	// 账号号会进日志、进会话历史文件名；`account_2` 与 `account_002` 混用
	// 会让排查时以为有两个账号
	if got := accountSlotID(2); got != "account_002" {
		t.Errorf("accountSlotID(2) = %q", got)
	}
	if got := accountSlotID(12); got != "account_012" {
		t.Errorf("accountSlotID(12) = %q", got)
	}
}

// ── 超时判定 ────────────────────────────────────────────

func Test超时判定(t *testing.T) {
	// **长轮询本来就该挂着**，分不出超时的话每轮正常轮询都会被记成错误
	if !isTimeout(context.DeadlineExceeded) {
		t.Error("context 超时该认出来")
	}
	if !isTimeout(&net_TimeoutError{}) {
		t.Error("传输层读超时该认出来")
	}
	if isTimeout(errors.New("连接被拒")) {
		t.Error("普通错误不该当超时")
	}
	if isTimeout(nil) {
		t.Error("nil 不该是超时")
	}
}

// net_TimeoutError 一个最小的 net.Error 实现。
type net_TimeoutError struct{}

func (e *net_TimeoutError) Error() string   { return "读超时" }
func (e *net_TimeoutError) Timeout() bool   { return true }
func (e *net_TimeoutError) Temporary() bool { return true }
