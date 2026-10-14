package web

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/zjzhang-cn/fka-go/internal/channels"
)

// pngBytes 一段能被 http.DetectContentType 认成 image/png 的字节。
func pngBytes() []byte { return []byte("\x89PNG\r\n\x1a\nresource-png") }

func testServer(t *testing.T) (*Channel, *httptest.Server) {
	t.Helper()
	c := testChannel(t)
	c.onMessage = func(channels.InboundMessage) {} // 收下即可；要抓消息的用例自己覆盖
	ts := httptest.NewServer(c.routes())
	t.Cleanup(ts.Close)
	return c, ts
}

func uploadFile(t *testing.T, ts *httptest.Server, conversation, text, name string, data []byte, token string) {
	t.Helper()
	body, contentType := multipartBody(t, conversation, text, name, data)
	req, _ := http.NewRequest("POST", ts.URL+"/messages", body)
	req.AddCookie(cookie(token))
	req.Header.Set("Content-Type", contentType)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("上传该 202，实际 %d", resp.StatusCode)
	}
}

func postText(t *testing.T, ts *httptest.Server, conversation, text, token string) {
	t.Helper()
	payload, _ := json.Marshal(map[string]string{"conversation": conversation, "text": text})
	req, _ := http.NewRequest("POST", ts.URL+"/messages", bytes.NewReader(payload))
	req.AddCookie(cookie(token))
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("发消息该 202，实际 %d", resp.StatusCode)
	}
}

func getResources(t *testing.T, ts *httptest.Server, token, conversation string) []map[string]any {
	t.Helper()
	req, _ := http.NewRequest("GET", ts.URL+"/resources?conversation="+url.QueryEscape(conversation), nil)
	if token != "" {
		req.AddCookie(cookie(token))
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("该 200，实际 %d", resp.StatusCode)
	}
	var body struct {
		Resources []map[string]any `json:"resources"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatal(err)
	}
	return body.Resources
}

// Test解析引用_名字与引号 parseRefs 认 `#名字`、含空格写 `#"名 字"`；只认词首、遇句读停，
// `C#`、markdown 标题 `# 标题` 都不算。
func Test解析引用_名字与引号(t *testing.T) {
	cases := []struct {
		text string
		want []string
	}{
		{"看看 #photo.png", []string{"photo.png"}},
		{"#a.png 和 #b.pdf", []string{"a.png", "b.pdf"}},
		{`#"我的 图.png" 是什么`, []string{"我的 图.png"}},
		{"#a.png，谢谢", []string{"a.png"}},
		{"没有引用", nil},
		{"C# 不算", nil},
		{"# 标题不算", nil},
		{"foo#bar", nil},
	}
	for _, tc := range cases {
		got := parseRefs(tc.text)
		if len(got) != len(tc.want) {
			t.Errorf("parseRefs(%q) = %v，想要 %v", tc.text, got, tc.want)
			continue
		}
		for i := range tc.want {
			if got[i] != tc.want[i] {
				t.Errorf("parseRefs(%q)[%d] = %q，想要 %q", tc.text, i, got[i], tc.want[i])
			}
		}
	}
}

// Test会话资源_上传后可列出 用户上传的文件进目录，GET /resources 列得到。
func Test会话资源_上传后可列出(t *testing.T) {
	_, ts := testServer(t)
	uploadFile(t, ts, "room-1", "", "photo.png", pngBytes(), "secret")

	items := getResources(t, ts, "secret", "room-1")
	if len(items) != 1 || items[0]["name"] != "photo.png" {
		t.Fatalf("该列出 photo.png，实际 %v", items)
	}
	if items[0]["kind"] != "image" {
		t.Errorf("png 该是 image，实际 %v", items[0]["kind"])
	}
	if items[0]["source"] != sourceMedia {
		t.Errorf("来源该是 %q，实际 %v", sourceMedia, items[0]["source"])
	}
}

// Test会话资源_出站媒体也进目录 agent 发来的媒体同样可以被 `#引用`。
func Test会话资源_出站媒体也进目录(t *testing.T) {
	c, ts := testServer(t)
	conv := sessionConversation("web:alice", "room-1")
	if _, err := c.sendMedia(channels.SendMediaParams{
		Target:   channels.SendTarget{ConversationID: conv},
		Data:     pngBytes(),
		FileName: "out.png",
		MimeType: "image/png",
	}, "image"); err != nil {
		t.Fatal(err)
	}

	items := getResources(t, ts, "secret", "room-1")
	if len(items) != 1 || items[0]["name"] != "out.png" {
		t.Fatalf("该列出 out.png，实际 %v", items)
	}
}

// Test会话资源_按身份隔离 两个用户同名会话不共享目录。
func Test会话资源_按身份隔离(t *testing.T) {
	_, ts := testServer(t)
	uploadFile(t, ts, "room-1", "", "photo.png", pngBytes(), "secret") // web:alice

	if items := getResources(t, ts, "bobtok", "room-1"); len(items) != 0 {
		t.Fatalf("bob 不该看到 alice 的媒体，实际 %v", items)
	}
}

// Test入站_引用会话媒体并进本轮 先传一张图，再用 `#名字` 引用它——目标消息里带一个图片
// part，FetchMedia 能取回原字节；引用 token 原样留在正文里。
func Test入站_引用会话媒体并进本轮(t *testing.T) {
	c, ts := testServer(t)
	var got channels.InboundMessage
	c.onMessage = func(m channels.InboundMessage) { got = m }

	uploadFile(t, ts, "room-1", "", "photo.png", pngBytes(), "secret")
	postText(t, ts, "room-1", "看看 #photo.png", "secret")

	ref := imageRefOf(t, got)
	data, err := c.FetchMedia(context.Background(), ref)
	if err != nil || !bytes.Equal(data, pngBytes()) {
		t.Fatalf("该取回原图（err=%v，data=%q）", err, data)
	}
	if !strings.Contains(got.Text(), "#photo.png") {
		t.Errorf("引用 token 该留在正文里：%q", got.Text())
	}
}

// Test入站_引用出站媒体 引用 agent 发来的媒体：从 blobs 取字节、重新落 inbound，
// 消息层仍从 inbound 一个暂存取。
func Test入站_引用出站媒体(t *testing.T) {
	c, ts := testServer(t)
	conv := sessionConversation("web:alice", "room-1")
	if _, err := c.sendMedia(channels.SendMediaParams{
		Target:   channels.SendTarget{ConversationID: conv},
		Data:     pngBytes(),
		FileName: "out.png",
		MimeType: "image/png",
	}, "image"); err != nil {
		t.Fatal(err)
	}

	var got channels.InboundMessage
	c.onMessage = func(m channels.InboundMessage) { got = m }
	postText(t, ts, "room-1", "#out.png 处理一下", "secret")

	ref := imageRefOf(t, got)
	data, err := c.FetchMedia(context.Background(), ref)
	if err != nil || !bytes.Equal(data, pngBytes()) {
		t.Fatalf("该取回出站媒体的字节（err=%v，data=%q）", err, data)
	}
}

// Test入站_未知引用原样留着 引用一个不存在的名字：不产 part、不报错，文字照旧。
func Test入站_未知引用原样留着(t *testing.T) {
	c, ts := testServer(t)
	var got channels.InboundMessage
	c.onMessage = func(m channels.InboundMessage) { got = m }
	postText(t, ts, "room-1", "看 #nope.png", "secret")

	for _, p := range got.Parts {
		if p.Kind != channels.KindText {
			t.Fatalf("未知引用不该产 part：%+v", p)
		}
	}
	if got.Text() != "看 #nope.png" {
		t.Errorf("文字该原样：%q", got.Text())
	}
}

// Test会话资源_接口校验 未认证 401、非法会话 id 400。
func Test会话资源_接口校验(t *testing.T) {
	_, ts := testServer(t)

	resp, err := http.Get(ts.URL + "/resources?conversation=room-1")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("未认证该 401，实际 %d", resp.StatusCode)
	}

	req, _ := http.NewRequest("GET", ts.URL+"/resources?conversation=../etc", nil)
	req.AddCookie(cookie("secret"))
	resp2, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp2.Body.Close()
	if resp2.StatusCode != http.StatusBadRequest {
		t.Errorf("非法会话该 400，实际 %d", resp2.StatusCode)
	}
}
