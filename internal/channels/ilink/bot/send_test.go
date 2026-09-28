package bot

import (
	"bytes"
	"context"
	"crypto/md5"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// ── 出站报文形状 ────────────────────────────────────────

// Test出站报文信封字段 漏字段的症状是 `ret=-2 invalid arguments`，
// 而服务端**不会告诉你缺了哪一个**——所以每个字段都得在这里钉住。
func Test出站报文信封字段(t *testing.T) {
	msg := BuildOutboundMsg(BuildOutboundMsgParams{
		ToUserID:     "o9cq80_abc",
		ContextToken: "ctx-1",
		ClientID:     "pinix-weixin:1758000000000-deadbeef",
		Items:        []MessageItem{{Type: ItemTypeText, Text: "在的"}},
	})

	// 线上形状：信封字段全是 snake_case
	encoded, err := json.Marshal(msg)
	if err != nil {
		t.Fatal(err)
	}
	var wire map[string]any
	if err := json.Unmarshal(encoded, &wire); err != nil {
		t.Fatal(err)
	}

	for _, key := range []string{"from_user_id", "to_user_id", "client_id",
		"message_type", "message_state", "context_token", "item_list"} {
		if _, ok := wire[key]; !ok {
			t.Errorf("报文缺字段 %q：%s", key, encoded)
		}
	}
	// from_user_id 出站是空串，协议规定——不是省略，是空串
	if value, ok := wire["from_user_id"].(string); !ok || value != "" {
		t.Errorf("from_user_id 该是空串，实际 %v", wire["from_user_id"])
	}
	if wire["message_type"].(float64) != MessageTypeBot {
		t.Errorf("出站 message_type 必须是 %d（Bot 发出）", MessageTypeBot)
	}
	if wire["message_state"].(float64) != MessageStateFinish {
		t.Errorf("出站 message_state 必须是 %d（FINISH）", MessageStateFinish)
	}
	// run_id 缺省时不该出现（omitempty），多一个空字段可能被服务端当参数错
	if _, ok := wire["run_id"]; ok {
		t.Error("没给 run_id 时不该发这个字段")
	}
}

// Test文本挂在text_item下 **内容挂在 `*_item` 子对象上**——这个挂载点是协议定的，
// 不是我们选的。挂错了服务端不报错，只是收不到内容。
func Test文本挂在textItem下(t *testing.T) {
	msg := BuildOutboundMsg(BuildOutboundMsgParams{
		ToUserID: "u", ContextToken: "c", ClientID: "id",
		Items: []MessageItem{{Type: ItemTypeText, Text: "你好"}},
	})

	encoded, _ := json.Marshal(msg)
	if !strings.Contains(string(encoded), `"text_item":{"text":"你好"}`) {
		t.Errorf("文本该挂在 text_item 下：%s", encoded)
	}
	if strings.Contains(string(encoded), `"text":"你好"`) &&
		!strings.Contains(string(encoded), "text_item") {
		t.Errorf("不该直接平铺 text 字段：%s", encoded)
	}
}

// Test文件项len是字符串 协议要求 `file_item.len` 是**十进制字符串**。
// 传数字的失败方式是服务端算不对大小，而错误信息不指向这里。
func Test文件项Len是字符串(t *testing.T) {
	msg := BuildOutboundMsg(BuildOutboundMsgParams{
		ToUserID: "u", ContextToken: "c", ClientID: "id",
		Items: []MessageItem{{
			Type: ItemTypeFile,
			File: &FileItem{Media: CDNMedia{EncryptQueryParam: "p"}, FileName: "a.pdf", Len: "204800"},
		}},
	})
	encoded, _ := json.Marshal(msg)
	if !strings.Contains(string(encoded), `"len":"204800"`) {
		t.Errorf("len 该是字符串：%s", encoded)
	}
}

// Test图片项密钥要平铺 图片项与文件项的关键差异：`aeskey` 平铺在 image_item 上。
// 文件项**没有**这个字段——照图片项去读文件项的 aeskey 会得到空串。
func Test图片项密钥要平铺(t *testing.T) {
	msg := BuildOutboundMsg(BuildOutboundMsgParams{
		ToUserID: "u", ContextToken: "c", ClientID: "id",
		Items: []MessageItem{{
			Type: ItemTypeImage,
			Image: &ImageItem{
				Aeskey:  "0123456789abcdef0123456789abcdef",
				Media:   CDNMedia{EncryptQueryParam: "p", AesKey: "b64", EncryptType: 1},
				MidSize: 1234,
			},
		}},
	})
	encoded, _ := json.Marshal(msg)

	if !strings.Contains(string(encoded), `"aeskey":"0123456789abcdef0123456789abcdef"`) {
		t.Errorf("图片项该平铺 aeskey：%s", encoded)
	}
	if !strings.Contains(string(encoded), `"mid_size":1234`) {
		t.Errorf("该报 mid_size：%s", encoded)
	}
	// 上传时固定 no_need_thumb，所以不该有 thumb_* / hd_size
	for _, absent := range []string{"thumb_size", "hd_size", "full_url"} {
		if strings.Contains(string(encoded), absent) {
			t.Errorf("不该带 %s：%s", absent, encoded)
		}
	}
	// 密钥在 media 里也留一份 base64 的，与入站形状一致
	if !strings.Contains(string(encoded), `"aes_key":"b64"`) {
		t.Errorf("media 里该也留一份 aes_key：%s", encoded)
	}
}

// TestClientId格式 缺失它会得到 `ret=-2 invalid arguments`。
func TestClientId格式(t *testing.T) {
	id := GenerateClientID()
	if !strings.HasPrefix(id, "pinix-weixin:") {
		t.Errorf("client_id 该以 pinix-weixin: 开头，实际 %q", id)
	}
	parts := strings.Split(id, ":")
	if len(parts) != 2 {
		t.Fatalf("格式不对：%q", id)
	}
	stamp, hexPart, _ := strings.Cut(parts[1], "-")
	if _, err := hex.DecodeString(hexPart); err != nil {
		t.Errorf("后半段该是十六进制：%q", hexPart)
	}
	ms, err := time.ParseDuration(stamp + "ms")
	if err != nil {
		t.Fatalf("前半段该是毫秒时间戳：%q", stamp)
	}
	_ = ms
	// 每条都要不同——重复了服务端可能当成重发
	seen := map[string]bool{}
	for i := 0; i < 100; i++ {
		got := GenerateClientID()
		if seen[got] {
			t.Fatalf("client_id 重复了：%q", got)
		}
		seen[got] = true
	}
}

// Test缺ContextToken要在发之前就拒 **不能发一个空 token 的请求**让服务端回
// `ret=-2`——那样只得到一个不指向根因的错误。
func Test缺ContextToken要在发之前就拒(t *testing.T) {
	s := newSender(WeixinAccount{ID: "a", BaseURL: "http://x", BotToken: "t"}, nil)

	cases := map[string]struct{ to, token, text string }{
		"缺 context_token": {"u", "", "hi"},
		"缺 to_user_id":    {"", "ctx", "hi"},
		"空文本":             {"u", "ctx", ""},
	}
	for name, c := range cases {
		_, err := s.SendText(context.Background(), c.to, c.token, c.text)
		if err == nil {
			t.Errorf("%s：该在发之前就拒绝", name)
		}
	}
}

// ── 鉴权头 ──────────────────────────────────────────────

func Test鉴权头(t *testing.T) {
	got := headers("my-token")

	if got.Get("Authorization") != "Bearer my-token" {
		t.Errorf("Authorization = %q", got.Get("Authorization"))
	}
	// 少了这个头服务端不认账号，而报错是 401，不指向原因
	if got.Get("AuthorizationType") != "ilink_bot_token" {
		t.Errorf("AuthorizationType = %q", got.Get("AuthorizationType"))
	}
	// 每次都要不同
	first, second := got.Get("X-WECHAT-UIN"), headers("my-token").Get("X-WECHAT-UIN")
	if first == "" {
		t.Error("X-WECHAT-UIN 不能为空")
	}
	if first == second {
		t.Error("X-WECHAT-UIN 该每次都换")
	}
	// **是十进制字符串的 base64**，不是 4 个字节的 base64
	decoded, err := base64.StdEncoding.DecodeString(first)
	if err != nil {
		t.Fatalf("X-WECHAT-UIN 不是合法 base64：%v", err)
	}
	for _, b := range decoded {
		if b < '0' || b > '9' {
			t.Errorf("解出来该是十进制数字，实际 %q", decoded)
			break
		}
	}
}

// ── 上传 ────────────────────────────────────────────────

// Test密钥给Media时是Hex串的Base64 **不是原始密钥字节的 base64。**
//
// 官方说明就是「把十六进制密钥字符串按 base64 编码」，真机入站消息印证了这一点：
// 那个 base64 解出来是 32 个 hex 字符，再 hex 解一次才是 16 字节密钥。
// 搞反了的话，加密能自洽但**服务端解不开**——而报错只会说「上传失败」。
func Test密钥给Media时是Hex串的Base64(t *testing.T) {
	hexKey := "0123456789abcdef0123456789abcdef"
	got := EncodeAesKeyForMedia(hexKey)

	decoded, err := base64.StdEncoding.DecodeString(got)
	if err != nil {
		t.Fatal(err)
	}
	if string(decoded) != hexKey {
		t.Errorf("解出来该是那 32 个 hex 字符，实际 %q", decoded)
	}
	// 顺带确认它与「原始密钥的 base64」**不是同一个值**——那正是容易混的地方
	raw := make([]byte, 16)
	_, _ = hex.Decode(raw, []byte(hexKey))
	if got == base64.StdEncoding.EncodeToString(raw) {
		t.Error("这两个值不该相同，测试数据有问题")
	}
}

func Test生成密钥的形状(t *testing.T) {
	aeskey, err := GenerateAesKey()
	if err != nil {
		t.Fatal(err)
	}
	if len(aeskey) != 32 {
		t.Errorf("aeskey 该是 32 个 hex 字符，实际 %d：%q", len(aeskey), aeskey)
	}
	if _, err := hex.DecodeString(aeskey); err != nil {
		t.Errorf("aeskey 该是合法 hex：%v", err)
	}

	filekey, err := GenerateFileKey()
	if err != nil {
		t.Fatal(err)
	}
	if len(filekey) != 32 {
		t.Errorf("filekey 该是 32 个 hex 字符，实际 %d", len(filekey))
	}
	// 两次要不同——filekey 参与拼上传地址，重复会撞
	if aeskey == filekey {
		t.Error("aeskey 与 filekey 不该相同")
	}
}

// TestPaddedLength整块时也要补 **整块时要补一整块**（16 → 32），不是不补。
// 不补的话解出来与原文同长，调用方无法区分「没填充」与「刚好整块」。
func TestPaddedLength整块时也要补(t *testing.T) {
	cases := map[int]int{0: 16, 1: 16, 15: 16, 16: 32, 17: 32, 31: 32, 32: 48, 33: 48}
	for in, want := range cases {
		if got := PaddedLength(in); got != want {
			t.Errorf("PaddedLength(%d) = %d，期望 %d", in, got, want)
		}
	}
}

func Test上传地址两种来源(t *testing.T) {
	// 优先 full_url
	got, err := BuildUploadURL(uploadURLResponse{
		UploadFullURL: "https://cdn.example/full", UploadParam: "p",
	}, "fk")
	if err != nil || got != "https://cdn.example/full" {
		t.Errorf("该优先用 upload_full_url：%q %v", got, err)
	}

	// 缺失时用 param + filekey 拼
	got, err = BuildUploadURL(uploadURLResponse{UploadParam: "p1"}, "fk")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(got, "encrypted_query_param=p1") || !strings.Contains(got, "filekey=fk") {
		t.Errorf("拼出来的地址不对：%q", got)
	}

	// 两个都没有
	if _, err := BuildUploadURL(uploadURLResponse{}, "fk"); err == nil {
		t.Error("两个都没有时该报错")
	}
}

// Test上传往返 真起一个假 CDN：验请求体形状、密文确实被加密、拿到
// x-encrypted-param 组出 media 引用。
func Test上传往返(t *testing.T) {
	const aeskey = "0123456789abcdef0123456789abcdef"
	plain := []byte("这是要上传的图片内容")

	var gotBody map[string]any
	var gotCipher []byte
	var gotContentType string

	// handler 里要用到 server 自己的地址，所以先声明后赋值。
	//
	// **必须给 upload_full_url 指向自己**：`BuildUploadURL` 在没有
	// `upload_full_url` 时会拼出真实 CDN 域名（那是官方实现的行为），
	// 只给 upload_param 的话测试会真的联网去。
	var server *httptest.Server
	server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/ilink/bot/getuploadurl":
			raw, _ := io.ReadAll(r.Body)
			_ = json.Unmarshal(raw, &gotBody)
			if gotBody["no_need_thumb"] != true {
				t.Errorf("官方固定 no_need_thumb=true，实际 %v", gotBody["no_need_thumb"])
			}
			// 密钥是**每次随机生成**的，所以只能验形状不能验具体值
			key, _ := gotBody["aeskey"].(string)
			if len(key) != 32 {
				t.Errorf("aeskey 该是 32 个 hex 字符传给 getuploadurl，实际 %q", key)
			}
			if _, err := hex.DecodeString(key); err != nil {
				t.Errorf("aeskey 该是合法 hex：%v", err)
			}
			// **必须给 upload_full_url 指向自己**：只给 upload_param 时
			// BuildUploadURL 会拼出真实 CDN 域名，测试就联网去了
			_, _ = w.Write([]byte(`{"upload_param":"param-1","upload_full_url":"` +
				server.URL + `/upload"}`))
		case "/upload":
			gotContentType = r.Header.Get("Content-Type")
			gotCipher, _ = io.ReadAll(r.Body)
			w.Header().Set("x-encrypted-param", "encrypted-param-xyz")
			w.WriteHeader(http.StatusOK)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer server.Close()

	result, err := UploadMedia(context.Background(),
		newClient(WeixinAccount{BaseURL: server.URL, BotToken: "t"}, server.Client()),
		"to-user", plain, UploadMediaFile, 5*time.Second)
	if err != nil {
		t.Fatalf("上传失败：%v", err)
	}

	// 声明的 filesize 必须是**填充后密文**的大小，而 rawsize 是明文的
	if int(gotBody["rawsize"].(float64)) != len(plain) {
		t.Errorf("rawsize 该是明文字节数 %d，实际 %v", len(plain), gotBody["rawsize"])
	}
	if int(gotBody["filesize"].(float64)) != PaddedLength(len(plain)) {
		t.Errorf("filesize 该是填充后密文 %d，实际 %v", PaddedLength(len(plain)), gotBody["filesize"])
	}
	// **rawfilemd5 是明文的 md5**
	digest := md5.Sum(plain)
	wantMD5 := hex.EncodeToString(digest[:])
	if gotBody["rawfilemd5"] != wantMD5 {
		t.Errorf("rawfilemd5 = %v，期望 %s", gotBody["rawfilemd5"], wantMD5)
	}
	// media_type 3 = 文件，与 item 的 type=4 不同
	if int(gotBody["media_type"].(float64)) != UploadMediaFile {
		t.Errorf("media_type = %v，期望 %d", gotBody["media_type"], UploadMediaFile)
	}

	if gotContentType != "application/octet-stream" {
		t.Errorf("上传的 Content-Type = %q", gotContentType)
	}
	if bytes.Equal(gotCipher, plain) {
		t.Error("发上去的**不该是明文**")
	}
	// 发上去的密文能解回原文
	decrypted, err := DecryptMedia(gotCipher, result.Aeskey)
	if err != nil {
		t.Fatalf("解不开自己发的东西：%v", err)
	}
	if !bytes.Equal(decrypted, plain) {
		t.Error("往返后内容不同")
	}

	if result.Media.EncryptQueryParam != "encrypted-param-xyz" {
		t.Errorf("该取响应头的 x-encrypted-param：%+v", result.Media)
	}
	if result.Media.AesKey != EncodeAesKeyForMedia(result.Aeskey) {
		t.Errorf("media.aes_key 该是 hex 串的 base64")
	}
	if result.Size != len(plain) {
		t.Errorf("Size = %d", result.Size)
	}
}

// Test上传时AESKey是随机生成的 每��次上传都用新密钥——复用密钥等于所有文件
// 共用一把锁，而 key 是会被记录在日志与请求体里的。
func Test上传时AESKey是随机生成的(t *testing.T) {
	seen := map[string]bool{}
	for i := 0; i < 20; i++ {
		key, err := GenerateAesKey()
		if err != nil {
			t.Fatal(err)
		}
		if seen[key] {
			t.Fatalf("密钥重复了：%q", key)
		}
		seen[key] = true
	}
}

// Test4xx不重试 那类失败是确定性的（地址错了、密文格式不对），
// 重试只会把同一个错误重复三次再报一遍，还平白多等两轮退避。
func Test4xx不重试(t *testing.T) {
	var attempts int32
	var server *httptest.Server
	server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/ilink/bot/getuploadurl" {
			_, _ = w.Write([]byte(`{"upload_full_url":"` + server.URL + `/upload"}`))
			return
		}
		atomic.AddInt32(&attempts, 1)
		w.WriteHeader(http.StatusForbidden)
	}))
	defer server.Close()

	_, err := UploadMedia(context.Background(),
		newClient(WeixinAccount{BaseURL: server.URL, BotToken: "t"}, server.Client()),
		"to", []byte("x"), UploadMediaFile, 2*time.Second)
	if err == nil {
		t.Fatal("403 该报错")
	}
	if got := atomic.LoadInt32(&attempts); got != 1 {
		t.Errorf("403 不该重试，实际试了 %d 次", got)
	}
	if !strings.Contains(err.Error(), "不重试") {
		t.Errorf("该说清不重试：%v", err)
	}
}

// Test5xx要重试 那是确定性的**暂时**失败，重试有意义。
func Test5xx要重试(t *testing.T) {
	var attempts int32
	var server *httptest.Server
	server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/ilink/bot/getuploadurl" {
			_, _ = w.Write([]byte(`{"upload_full_url":"` + server.URL + `/upload"}`))
			return
		}
		if atomic.AddInt32(&attempts, 1) < 2 {
			w.WriteHeader(http.StatusBadGateway)
			return
		}
		w.Header().Set("x-encrypted-param", "ok")
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	// 退避 1s 会让用例变慢——这里只验「会重试」，所以把 BaseDelay 临时压到最小
	restore := mediaRetry.BaseDelay
	mediaRetry.BaseDelay = time.Millisecond
	defer func() { mediaRetry.BaseDelay = restore }()

	result, err := UploadMedia(context.Background(),
		newClient(WeixinAccount{BaseURL: server.URL, BotToken: "t"}, server.Client()),
		"to", []byte("x"), UploadMediaFile, 2*time.Second)
	if err != nil {
		t.Fatalf("5xx 之后重试该成功：%v", err)
	}
	if result.Media.EncryptQueryParam != "ok" {
		t.Errorf("该拿到重试后的结果：%+v", result.Media)
	}
	if atomic.LoadInt32(&attempts) != 2 {
		t.Errorf("该试两次，实际 %d", attempts)
	}
}

// Test上传空内容直接拒 返回一个指向空资源的引用会让发送**静默失效**。
func Test上传空内容直接拒(t *testing.T) {
	_, err := UploadMedia(context.Background(), newClient(WeixinAccount{}, nil),
		"to", nil, UploadMediaFile, time.Second)
	if err == nil {
		t.Fatal("空内容该报错")
	}
}

// ── 下载 ────────────────────────────────────────────────

func Test下载地址两种来源(t *testing.T) {
	// 优先 full_url（实测入站消息提供，省去手工拼）
	got, err := BuildDownloadURL(CDNMedia{FullURL: "https://cdn.example/full", EncryptQueryParam: "p"})
	if err != nil || got != "https://cdn.example/full" {
		t.Errorf("该优先用 full_url：%q %v", got, err)
	}

	got, err = BuildDownloadURL(CDNMedia{EncryptQueryParam: "p1"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(got, "encrypted_query_param=p1") {
		t.Errorf("拼出来的地址不对：%q", got)
	}

	// 一个都没有
	if _, err := BuildDownloadURL(CDNMedia{}); err == nil {
		t.Error("既没 full_url 也没 encrypt_query_param 时该报错")
	}
}

// Test下载解密往返 真起一个假 CDN 发密文，验能解回原文。
func Test下载解密往返(t *testing.T) {
	plain := []byte("这是要下载的图片内容\xff\xd8\xff")
	key := "0123456789abcdef0123456789abcdef"
	ciphertext, err := EncryptMedia(plain, key)
	if err != nil {
		t.Fatal(err)
	}

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/download" {
			_, _ = w.Write(ciphertext)
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	defer server.Close()

	media := CDNMedia{
		FullURL:           server.URL + "/download",
		EncryptQueryParam: "p",
		AesKey:            EncodeAesKeyForMedia(key),
	}

	// 优先用平铺的 aeskey（图片项那样）
	got, err := DownloadMedia(context.Background(),
		newClient(WeixinAccount{}, server.Client()), media, key)
	if err != nil {
		t.Fatalf("下载失败：%v", err)
	}
	if !bytes.Equal(got, plain) {
		t.Errorf("解出来的内容不对：%q", got)
	}

	// 文件项没有平铺 aeskey——该回落到 media.aes_key
	got, err = DownloadMedia(context.Background(),
		newClient(WeixinAccount{}, server.Client()), media, "")
	if err != nil {
		t.Fatalf("只给 media.aes_key 也该能解：%v", err)
	}
	if !bytes.Equal(got, plain) {
		t.Errorf("回落路径解出来的内容不对：%q", got)
	}
}

// Test下载4xx不重试
func Test下载4xx不重试(t *testing.T) {
	var attempts int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&attempts, 1)
		w.WriteHeader(http.StatusNotFound)
	}))
	defer server.Close()

	_, err := DownloadMedia(context.Background(), newClient(WeixinAccount{}, server.Client()),
		CDNMedia{FullURL: server.URL + "/x"}, "0123456789abcdef0123456789abcdef")
	if err == nil {
		t.Fatal("404 该报错")
	}
	if atomic.LoadInt32(&attempts) != 1 {
		t.Errorf("404 不该重试，实际 %d 次", attempts)
	}
}

// Test解密失败不重试 重下同一份字节，解出来还是同样的错。
// 而「填充不一致」恰恰说明**字节本身有问题**，换个时间再下一遍说不定就好了
// ——但那会掩盖真正的原因。
func Test解密失败不重试(t *testing.T) {
	var attempts int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&attempts, 1)
		_, _ = w.Write(bytes.Repeat([]byte{0xAA}, 32)) // 不是合法密文
	}))
	defer server.Close()

	_, err := DownloadMedia(context.Background(), newClient(WeixinAccount{}, server.Client()),
		CDNMedia{FullURL: server.URL + "/x"}, "0123456789abcdef0123456789abcdef")
	if err == nil {
		t.Fatal("解不开该报错")
	}
	if atomic.LoadInt32(&attempts) != 1 {
		t.Errorf("解密失败不该重试，实际下了 %d 次", attempts)
	}
	if !strings.Contains(err.Error(), "解密") {
		t.Errorf("该说清是解密失败：%v", err)
	}
}

func Test下载缺密钥要拒(t *testing.T) {
	_, err := DownloadMedia(context.Background(), newClient(WeixinAccount{}, nil),
		CDNMedia{FullURL: "https://x/y"}, "")
	if err == nil {
		t.Fatal("没有密钥该报错")
	}
	if !strings.Contains(err.Error(), "aeskey") {
		t.Errorf("该点名缺 aeskey：%v", err)
	}
}

// ── 发送（真起假服务端）────────────────────────────────

// Test发送走对端点 端点路径写错的话，404 会被当成「服务端没收到」，
// 而真正的错误在 URL 里。
func Test发送走对端点(t *testing.T) {
	var gotPath string
	var gotBody map[string]any

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		raw, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(raw, &gotBody)
		// 成功时服务端**不返回 ret**
		_, _ = w.Write([]byte(`{"message_id":"999888777666555444333"}`))
	}))
	defer server.Close()

	s := newSender(WeixinAccount{BaseURL: server.URL, BotToken: "t"}, server.Client())
	result, err := s.SendText(context.Background(), "to-user", "ctx-1", "在的")
	if err != nil {
		t.Fatalf("发送失败：%v", err)
	}

	if gotPath != "/ilink/bot/sendmessage" {
		t.Errorf("端点 = %q", gotPath)
	}
	if gotBody["base_info"].(map[string]any)["channel_version"] != ChannelVersion {
		t.Errorf("该带 base_info.channel_version：%v", gotBody["base_info"])
	}
	// message_id 是 uint64，超过 2^53——解成数字就丢精度
	if result.MessageID != "999888777666555444333" {
		t.Errorf("message_id 丢了精度：%q", result.MessageID)
	}
}

// Test发送失败要抛 **静默失败会让上层以为消息已送达。**
func Test发送失败要抛(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"ret":-2,"errmsg":"invalid arguments"}`))
	}))
	defer server.Close()

	s := newSender(WeixinAccount{BaseURL: server.URL, BotToken: "t"}, server.Client())
	_, err := s.SendText(context.Background(), "to-user", "ctx-1", "在的")
	if err == nil {
		t.Fatal("ret=-2 该报错")
	}
	// 报错要带 ret 码与应答片段，否则只能靠猜
	if !strings.Contains(err.Error(), "ret=-2") {
		t.Errorf("该带上 ret 码：%v", err)
	}
	if !strings.Contains(err.Error(), "invalid arguments") {
		t.Errorf("该带上应答内容：%v", err)
	}
}

// Test发文件先上传再引用 **不直接引用入站消息里的 CDN 资源**——
// 2026-09-14 实测：服务端接受请求并返回 message_id，但**不投递**。
func Test发文件先上传再引用(t *testing.T) {
	key := "0123456789abcdef0123456789abcdef"
	var paths []string

	var server *httptest.Server
	server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		paths = append(paths, r.URL.Path)
		switch r.URL.Path {
		case "/ilink/bot/getuploadurl":
			_, _ = w.Write([]byte(`{"upload_param":"uploaded-param","upload_full_url":"` +
				server.URL + `/upload"}`))
		case "/upload":
			w.Header().Set("x-encrypted-param", "uploaded-encrypted-param")
			w.WriteHeader(http.StatusOK)
		case "/ilink/bot/sendmessage":
			_, _ = w.Write([]byte(`{"message_id":"1"}`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer server.Close()

	dir := t.TempDir()
	path := filepath.Join(dir, "房产证.pdf")
	if err := os.WriteFile(path, []byte("PDF 内容"), 0o644); err != nil {
		t.Fatal(err)
	}

	s := newSender(WeixinAccount{BaseURL: server.URL, BotToken: "t"}, server.Client())
	if _, err := s.SendFile(context.Background(), "to-user", "ctx-1", path, ""); err != nil {
		t.Fatalf("发文件失败：%v", err)
	}

	// 顺序不能变：先取地址、再传、最后发
	want := []string{"/ilink/bot/getuploadurl", "/upload", "/ilink/bot/sendmessage"}
	if len(paths) != len(want) {
		t.Fatalf("该走 %v，实际走了 %v", want, paths)
	}
	for i := range want {
		if paths[i] != want[i] {
			t.Errorf("第 %d 步 = %q，期望 %q", i+1, paths[i], want[i])
		}
	}
	_ = key
}

// Test发图片用mediaType1 图片的 media_type 是 1，而文件是 3——
// 混起来是这里最容易错的一处。
func Test发图片用MediaType1(t *testing.T) {
	var mediaType float64
	var server *httptest.Server
	server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/ilink/bot/getuploadurl" {
			raw, _ := io.ReadAll(r.Body)
			var body map[string]any
			_ = json.Unmarshal(raw, &body)
			mediaType, _ = body["media_type"].(float64)
			_, _ = w.Write([]byte(`{"upload_full_url":"` + server.URL + `/upload"}`))
			return
		}
		if r.URL.Path == "/upload" {
			w.Header().Set("x-encrypted-param", "enc")
			w.WriteHeader(http.StatusOK)
			return
		}
		_, _ = w.Write([]byte(`{"message_id":"1"}`))
	}))
	defer server.Close()

	path := filepath.Join(t.TempDir(), "a.png")
	if err := os.WriteFile(path, []byte("PNG"), 0o644); err != nil {
		t.Fatal(err)
	}

	s := newSender(WeixinAccount{BaseURL: server.URL, BotToken: "t"}, server.Client())
	if _, err := s.SendImage(context.Background(), "to-user", "ctx-1", path, ""); err != nil {
		t.Fatalf("发图片失败：%v", err)
	}
	if int(mediaType) != UploadMediaImage {
		t.Errorf("图片的 media_type = %v，期望 %d", mediaType, UploadMediaImage)
	}
	if UploadMediaImage == ItemTypeImage {
		t.Error("media_type 与 item 的 type 不该是同一个序号")
	}
}
