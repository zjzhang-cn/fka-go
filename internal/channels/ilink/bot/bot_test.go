package bot

import (
	"bytes"
	"encoding/hex"
	"encoding/json"
	"strings"
	"testing"
)

// ── 真实报文形状的 fixture ──────────────────────────────
//
// 字段名与类型照 Node 版 `parse.ts` / `types.ts` 里记的**真机实测**结论：
// snake_case、`file_item.len` 是字符串、`image_item.aeskey` 是平铺 32 位 hex、
// `media.aes_key` 是 base64、`message_id` 可能是数字也可能是引号化的字符串。

// realTextMessage 一条纯文本消息。message_id 用了**超过 2^53 的值**——
// 那正是 uint64 丢精度会咬人的地方。
const realTextMessage = `{
  "seq": 42,
  "message_id": "7391827364518293647382",
  "from_user_id": "o9cq80_abc",
  "to_user_id": "o9cq80_bot",
  "session_id": "sess-1",
  "message_type": 1,
  "message_state": 2,
  "context_token": "ctx-token-1",
  "create_time_ms": 1758000000000,
  "client_id": "cli-1",
  "item_list": [
    { "type": 1, "msg_id": "9007199254740993", "text_item": { "text": "  去年三亚挺好玩的  " } }
  ]
}`

// realImageMessage 真实图片消息（2026-09-14 实测形状）。
const realImageMessage = `{
  "seq": 43,
  "message_id": 12345678901234,
  "from_user_id": "o9cq80_abc",
  "to_user_id": "o9cq80_bot",
  "session_id": "sess-1",
  "message_type": 1,
  "message_state": 2,
  "context_token": "ctx-2",
  "create_time_ms": 1758000001000,
  "item_list": [
    {
      "type": 2,
      "msg_id": 55555,
      "image_item": {
        "aeskey": "0123456789abcdef0123456789abcdef",
        "mid_size": 288784,
        "thumb_size": 2048,
        "thumb_height": 100,
        "thumb_width": 80,
        "media": {
          "encrypt_query_param": "param-xyz",
          "aes_key": "MDEyMzQ1Njc4OWFiY2RlZjAxMjM0NTY3ODlhYmNkZWY=",
          "encrypt_type": 1,
          "full_url": "https://cdn.example/media/abc?token=zzz"
        }
      }
    }
  ]
}`

// realFileMessage 真实 .docx 文件消息（2026-09-14 实测形状）。
//
// 关键差异：**文件项没有平铺 aeskey**，密钥只能从 `media.aes_key` 取；
// 大小字段是 `len` 且**是字符串**。
const realFileMessage = `{
  "seq": 44,
  "message_id": 777,
  "from_user_id": "o9cq80_abc",
  "to_user_id": "o9cq80_bot",
  "session_id": "sess-1",
  "message_type": 1,
  "message_state": 2,
  "context_token": "ctx-3",
  "create_time_ms": 1758000002000,
  "item_list": [
    {
      "type": 4,
      "file_item": {
        "file_name": "房产证.docx",
        "md5": "d41d8cd98f00b204e9800998ecf8427e",
        "len": "204800",
        "media": {
          "encrypt_query_param": "param-file",
          "aes_key": "MDEyMzQ1Njc4OWFiY2RlZjAxMjM0NTY3ODlhYmNkZWY="
        }
      }
    }
  ]
}`

// realQuoteMessage 新版客户端的引用：**只给 svr_id**。
//
// **刻意不带 title、不带 message_item**——那才是「正文真的拿不到」的情况。
// 只给 svr_id 时还原不出正文，因为那需要本地消息缓存（本模块不做）。
const realQuoteMessage = `{
  "seq": 45,
  "message_id": 888,
  "from_user_id": "o9cq80_abc",
  "to_user_id": "o9cq80_bot",
  "session_id": "sess-1",
  "message_type": 1,
  "message_state": 2,
  "context_token": "ctx-4",
  "create_time_ms": 1758000003000,
  "item_list": [
    {
      "type": 1,
      "text_item": { "text": "这个怎么办" },
      "ref_msg": { "svr_id": "889900112233" }
    }
  ]
}`

// realOldQuoteMessage 老客户端的引用：整条带过来。
const realOldQuoteMessage = `{
  "seq": 46,
  "message_id": 999,
  "from_user_id": "o9cq80_abc",
  "to_user_id": "o9cq80_bot",
  "session_id": "sess-1",
  "message_type": 1,
  "message_state": 2,
  "context_token": "ctx-5",
  "create_time_ms": 1758000004000,
  "item_list": [
    {
      "type": 1,
      "text_item": { "text": "那这个呢" },
      "ref_msg": {
        "title": "总结",
        "message_item": { "type": 1, "msg_id": "777777", "text_item": { "text": "去年三亚挺好玩的" } }
      }
    }
  ]
}`

// ── uint64 无损 ─────────────────────────────────────────

// TestUint64Id不能丢精度 这是**整个解析层最要紧的一条**。
//
// `message_id` 线上是 uint64。声明成 int64 还算好的，声明成 float64 就完了——
// 而 float64 连「丢了」都不告诉你。症状是「消息偶尔对不上号」，不报错。
func TestUint64Id不能丢精度(t *testing.T) {
	message := mustParseOne(t, realTextMessage)

	// 2^63 以上，float64 在这个量级已经只能表示偶数 → 末位会变
	if message.MessageID != "7391827364518293647382" {
		t.Errorf("message_id 丢了精度：%q", message.MessageID)
	}
	// item 的 msg_id 同理，9007199254740993 = 2^53+1，正是 float64 的分界
	if got := message.Items[0].MsgID; got != "9007199254740993" {
		t.Errorf("item 的 msg_id 丢了精度：%q", got)
	}
}

func TestStringId三种输入都认(t *testing.T) {
	cases := []struct {
		name  string
		input string
		want  string
	}{
		{"带引号的长 id", `"7391827364518293647382"`, "7391827364518293647382"},
		{"裸数字的短 id", `12345678901234`, "12345678901234"},
		{"空串", `""`, ""},
		{"缺席", ``, ""},
		{"null", `null`, ""},
	}
	for _, c := range cases {
		if got := stringID(json.RawMessage(c.input)); got != c.want {
			t.Errorf("%s：stringID(%s) = %q，期望 %q", c.name, c.input, got, c.want)
		}
	}
}

// TestFileLen是字符串 协议里 `file_item.len` 就是字符串。照数字解会得到 0。
func TestFileLen是字符串(t *testing.T) {
	message := mustParseOne(t, realFileMessage)
	if len(message.Items) != 1 || message.Items[0].File == nil {
		t.Fatalf("该解析出一个文件项，实际：%+v", message.Items)
	}
	if message.Items[0].File.Len != "204800" {
		t.Errorf("len = %q，期望 \"204800\"", message.Items[0].File.Len)
	}
	if message.Items[0].File.FileName != "房产证.docx" {
		t.Errorf("file_name = %q", message.Items[0].File.FileName)
	}
}

// TestFileItem的密钥只能从Media取 **文件项没有平铺 aeskey**，图片项才有。
// 这个差异踩过一次：照图片项去读文件项的 aeskey，得到空串，然后解密失败。
func TestFileItem的密钥只能从Media取(t *testing.T) {
	message := mustParseOne(t, realFileMessage)
	file := message.Items[0].File

	if _, err := ParseAesKey(file.Media.AesKey); err != nil {
		t.Errorf("media.aes_key 应当能解出密钥：%v", err)
	}
}

func TestImageItem的字段全是SnakeCase(t *testing.T) {
	message := mustParseOne(t, realImageMessage)
	image := message.Items[0].Image
	if image == nil {
		t.Fatal("该解析出图片项")
	}
	if image.Aeskey != "0123456789abcdef0123456789abcdef" {
		t.Errorf("aeskey = %q", image.Aeskey)
	}
	if image.MidSize != 288784 {
		t.Errorf("mid_size = %d，期望 288784（真机实测值）", image.MidSize)
	}
	if image.Media.EncryptQueryParam != "param-xyz" {
		t.Errorf("encrypt_query_param = %q", image.Media.EncryptQueryParam)
	}
	if image.Media.FullURL != "https://cdn.example/media/abc?token=zzz" {
		t.Errorf("full_url = %q", image.Media.FullURL)
	}
}

// ── 引用 ───────────────────────────────────────────────

// Test新版引用只给svrId **拿不到正文时不编造。**
//
// 编一个空正文会让上层以为「引用是空的」，而真相是「新版客户端没给内容」。
// 那两种情况要分开说，所以 BodyKnown 与 IDKnown 是两个字段而不是一个空串。
func Test新版引用只给svrId(t *testing.T) {
	message := mustParseOne(t, realQuoteMessage)
	quote := message.Quote
	if quote == nil {
		t.Fatal("该解析出引用")
	}
	if !quote.IDKnown || quote.ID != "889900112233" {
		t.Errorf("svr_id 该被认成 id：%+v", quote)
	}
	if quote.BodyKnown {
		t.Errorf("不该凭空有正文：%q", quote.Body)
	}
	if IsQuoteResolved(quote) {
		t.Error("只有 id 没有正文时不该算「已还原」——上层要能说「我看不到内容」")
	}
}

// Test老客户端引用带全文
// Test只有title时那也算正文 摘要是服务端给的，和被引用消息本体是同一件事的两个
// 来源——**任一个都算「看到了内容」**，所以 title 存在时不该说「看不到」。
func Test只有title时那也算正文(t *testing.T) {
	raw := `{"seq":1,"message_id":1,"from_user_id":"a","to_user_id":"b",
      "session_id":"s","message_type":1,"message_state":2,"context_token":"c",
      "create_time_ms":1,
      "item_list":[{"type":1,"text_item":{"text":"这个"},
        "ref_msg":{"svr_id":"42","title":"冰箱保修还有30天"}}]}`
	message := mustParseOne(t, raw)
	if !message.Quote.BodyKnown {
		t.Fatalf("有 title 就该算看到了内容：%+v", message.Quote)
	}
	if message.Quote.Body != "冰箱保修还有30天" {
		t.Errorf("body = %q", message.Quote.Body)
	}
	if !IsQuoteResolved(message.Quote) {
		t.Error("有 title 就该算已还原")
	}
}

func Test老客户端引用带全文(t *testing.T) {
	message := mustParseOne(t, realOldQuoteMessage)
	quote := message.Quote
	if quote == nil {
		t.Fatal("该解析出引用")
	}
	if !quote.BodyKnown {
		t.Fatalf("该拿到正文，实际：%+v", quote)
	}
	// 摘要是服务端给的、本体是客户端给的，用 " | " 连起来信息最全
	if quote.Body != "总结 | 去年三亚挺好玩的" {
		t.Errorf("body = %q", quote.Body)
	}
	// 拿不到服务端 id 时退回 item 自己的 id
	if !quote.IDKnown || quote.ID != "777777" {
		t.Errorf("该退回 message_item.msg_id：%+v", quote)
	}
	if !IsQuoteResolved(quote) {
		t.Error("有正文就该算已还原")
	}
}

// Test引用一条文件时说出文件名 **用户看到的是文件名**，我们要能说出
// 「你引用的是那份文件」——而不是一句空白的引用。
func Test引用一条文件时说出文件名(t *testing.T) {
	raw := `{
      "seq": 47, "message_id": 1, "from_user_id": "a", "to_user_id": "b",
      "session_id": "s", "message_type": 1, "message_state": 2,
      "context_token": "c", "create_time_ms": 1,
      "item_list": [
        { "type": 1, "text_item": { "text": "这个" },
          "ref_msg": { "message_item": { "type": 4, "file_item": { "file_name": "房产证.docx" } } } }
      ]
    }`
	message := mustParseOne(t, raw)
	if message.Quote == nil || !message.Quote.BodyKnown {
		t.Fatalf("该拿到正文：%+v", message.Quote)
	}
	if !strings.Contains(message.Quote.Body, "房产证.docx") {
		t.Errorf("body 该说出文件名：%q", message.Quote.Body)
	}
}

func Test没有引用时是Nil(t *testing.T) {
	message := mustParseOne(t, realTextMessage)
	if message.Quote != nil {
		t.Errorf("没有引用时该是 nil：%+v", message.Quote)
	}
	if IsQuoteResolved(nil) {
		t.Error("nil 不该算已还原")
	}
}

// Test引用挂在Item上不是消息外层 一条消息可以有多个 item，引用只在其中一个上。
func Test引用挂在Item上不是消息外层(t *testing.T) {
	raw := `{
      "seq": 48, "message_id": 1, "from_user_id": "a", "to_user_id": "b",
      "session_id": "s", "message_type": 1, "message_state": 2,
      "context_token": "c", "create_time_ms": 1,
      "item_list": [
        { "type": 2, "image_item": { "aeskey": "0123456789abcdef0123456789abcdef" } },
        { "type": 1, "text_item": { "text": "这个" }, "ref_msg": { "svr_id": "42" } }
      ]
    }`
	message := mustParseOne(t, raw)
	if message.Quote == nil || message.Quote.ID != "42" {
		t.Fatalf("该在第二个 item 上找到引用：%+v", message.Quote)
	}
}

// ── 原文保真 ───────────────────────────────────────────

// Test原文保留没建模的字段 重新序列化会把没建模的字段全丢掉，而保留它们正是
// 存原文的目的——没建模的字段应该只是「我们暂时没读」，而不是「丢了」。
func Test原文保留没建模的字段(t *testing.T) {
	raw := `{"seq":1,"message_id":1,"from_user_id":"a","to_user_id":"b","session_id":"s",
	  "message_type":1,"message_state":2,"context_token":"c","create_time_ms":1,
	  "is_completed":true,"button_item_list":[{"id":"x"}],"root_id":"r9",
	  "item_list":[{"type":1,"text_item":{"text":"你好"}}]}`

	var parsed RawMessage
	if err := json.Unmarshal([]byte(raw), &parsed); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(parsed.OriginalJSON, "is_completed") ||
		!strings.Contains(parsed.OriginalJSON, "button_item_list") ||
		!strings.Contains(parsed.OriginalJSON, "root_id") {
		t.Errorf("原文里没建模的字段被丢了：%s", parsed.OriginalJSON)
	}
}

// ── 整批解析 ───────────────────────────────────────────

// Test成功响应不返回ret **成功时服务端根本不返回 ret 字段。**
//
// 照 `ret == 0` 判会把**每一次成功响应都判成失败**——症状是「长轮询正常、
// 游标推进、却一条消息都收不到」。这是过去最难查的一类。
func Test成功响应不返回ret(t *testing.T) {
	body := `{"msgs":[` + realTextMessage + `],"get_updates_buf":"buf-1"}`

	batch, err := ParseGetUpdates([]byte(body), "acct-1")
	if err != nil {
		t.Fatalf("成功响应不该返错：%v", err)
	}
	if len(batch.Messages) != 1 {
		t.Fatalf("该解析出一条消息，实际 %d 条", len(batch.Messages))
	}
	if batch.Cursor != "buf-1" {
		t.Errorf("cursor = %q", batch.Cursor)
	}
	if batch.SessionExpired {
		t.Error("不该报 session 过期")
	}
	if batch.Messages[0].AccountID != "acct-1" {
		t.Errorf("账号该由调用方注入：%q", batch.Messages[0].AccountID)
	}
}

// TestSession过期不是错误 -14 是「该重新扫码了」，是个正常状态，抛错会让
// 上层把它当成故障退避，而正确反应是走登录流程。
func TestSession过期不是错误(t *testing.T) {
	batch, err := ParseGetUpdates([]byte(`{"ret":-14}`), "acct-1")
	if err != nil {
		t.Fatalf("-14 不该返错：%v", err)
	}
	if !batch.SessionExpired {
		t.Error("该认出 session 过期")
	}
	if len(batch.Messages) != 0 {
		t.Errorf("该没有消息，实际 %d 条", len(batch.Messages))
	}
}

// Test非零Ret要抛错 它不是「这一轮没消息」而是协议层面的异常——
// 静默忽略正是过去「出问题完全看不见」的原因。
func Test非零Ret要抛错(t *testing.T) {
	_, err := ParseGetUpdates([]byte(`{"ret":-1,"msgs":[]}`), "acct-1")
	if err == nil {
		t.Fatal("非零 ret 该抛错")
	}
	// 报错要带响应片段，否则只能靠猜
	if !strings.Contains(err.Error(), "ret=-1") {
		t.Errorf("该带上 ret 码：%v", err)
	}
}

func Test空响应体等价于没消息(t *testing.T) {
	for _, body := range []string{"", "   ", "{}"} {
		batch, err := ParseGetUpdates([]byte(body), "acct-1")
		if err != nil {
			t.Errorf("空响应不该返错（%q）：%v", body, err)
		}
		if len(batch.Messages) != 0 {
			t.Errorf("空响应不该有消息：%+v", batch.Messages)
		}
	}
}

// Test游标类型不对就当没有 游标错了会重放或漏消息，而那两种都不该由「猜一个」来修。
func Test游标类型不对就当没有(t *testing.T) {
	batch, err := ParseGetUpdates([]byte(`{"msgs":[],"get_updates_buf":12345}`), "acct-1")
	if err != nil {
		t.Fatal(err)
	}
	if batch.Cursor != "" {
		t.Errorf("类型不对就该当没有，实际 %q", batch.Cursor)
	}
}

func Test多条保持服务端顺序(t *testing.T) {
	body := `{"msgs":[` + realTextMessage + `,` + realImageMessage + `,` + realFileMessage + `]}`
	batch, err := ParseGetUpdates([]byte(body), "acct-1")
	if err != nil {
		t.Fatal(err)
	}
	if len(batch.Messages) != 3 {
		t.Fatalf("该解析出三条，实际 %d 条", len(batch.Messages))
	}
	if batch.Messages[0].MessageID != "7391827364518293647382" ||
		batch.Messages[1].MessageID != "12345678901234" ||
		batch.Messages[2].MessageID != "777" {
		t.Errorf("顺序或 id 不对：%q %q %q",
			batch.Messages[0].MessageID, batch.Messages[1].MessageID, batch.Messages[2].MessageID)
	}
}

// Test中间态也照收 状态 1=生成中 说明这条可能还没说完。丢掉它就丢了用户
// 真正说过的话——而「用户说了半句就撤了」是常见行为。
func Test中间态也照收(t *testing.T) {
	message := mustParseOne(t, `{"seq":1,"message_id":1,"from_user_id":"a","to_user_id":"b",
      "session_id":"s","message_type":1,"message_state":1,"context_token":"c",
      "create_time_ms":1,"item_list":[{"type":1,"text_item":{"text":"我"}}]}`)
	if message.MessageState != MessageStateGenerating {
		t.Errorf("状态该原样保留，实际 %d", message.MessageState)
	}
	if message.Text() == "" {
		t.Error("中间态也该有正文")
	}
}

// Test坏引用不炸掉整条消息 引用读不出来就当没有——否则连正文一起丢了。
func Test坏引用不炸掉整条消息(t *testing.T) {
	message := mustParseOne(t, `{"seq":1,"message_id":1,"from_user_id":"a","to_user_id":"b",
      "session_id":"s","message_type":1,"message_state":2,"context_token":"c",
      "create_time_ms":1,
      "item_list":[{"type":1,"text_item":{"text":"正文还在"},"ref_msg":"这不是对象"}]}`)

	if message.Text() != "正文还在" {
		t.Errorf("正文该保住，实际 %q", message.Text())
	}
	if message.Quote != nil {
		t.Errorf("坏引用该当没有，实际 %+v", message.Quote)
	}
}

// ── 加解密 ─────────────────────────────────────────────

const testAESKey = "0123456789abcdef0123456789abcdef"

// Test加解密往返 三个长度都要过：**整块**那个最容易被漏（PKCS#7 在整块时
// 也要补一整块，否则解出来与原文同长，调用方无法区分「没填充」与「刚好整块」）。
func Test加解密往返(t *testing.T) {
	// 0、1、15、16、17、31、32、33 字节——覆盖不足一块、恰好一块、超一块
	for _, size := range []int{0, 1, 15, 16, 17, 31, 32, 33, 4096} {
		plain := bytes.Repeat([]byte{byte(size % 251)}, size)

		encrypted, err := EncryptMedia(plain, testAESKey)
		if err != nil {
			t.Fatalf("%d 字节：加密失败：%v", size, err)
		}
		// 密文长度必须向上补齐到块，且**整块时要多一整块**
		wantLen := ((size / aesBlockBytes) + 1) * aesBlockBytes
		if len(encrypted) != wantLen {
			t.Errorf("%d 字节：密文长度 %d，期望 %d", size, len(encrypted), wantLen)
		}

		decrypted, err := DecryptMedia(encrypted, testAESKey)
		if err != nil {
			t.Fatalf("%d 字节：解密失败：%v", size, err)
		}
		if !bytes.Equal(decrypted, plain) {
			t.Errorf("%d 字节：往返后内容不同", size)
		}
	}
}

// TestEcb每块独立 协议要的是 ECB——每块独立。用「零 IV 的 CBC」去凑，
// 解密看着能跑通（因为每块都独立解），但**加密方向产生的密文是错的**。
func TestEcb每块独立(t *testing.T) {
	// 两块相同的明文，ECB 下两段密文必须完全一样
	plain := bytes.Repeat([]byte("A"), aesBlockBytes*2)

	encrypted, err := EncryptMedia(plain, testAESKey)
	if err != nil {
		t.Fatal(err)
	}
	first := encrypted[:aesBlockBytes]
	second := encrypted[aesBlockBytes : aesBlockBytes*2]
	if !bytes.Equal(first, second) {
		t.Errorf("ECB 下相同明文块该产生相同密文：%x vs %x", first, second)
	}
}

// Test坏填充不被当成好 填充字节要**逐个核对**：只看最后一个的话，一个被篡改或
// 随机损坏的密文有 1/256 的概率通过校验，然后解出尾部糊掉的明文——
// 而这里解出来的正是图片，会渲染成半张图且不报错。
func Test坏填充不被当成好(t *testing.T) {
	encrypted, err := EncryptMedia([]byte("hello world"), testAESKey)
	if err != nil {
		t.Fatal(err)
	}

	// 把最后一个填充字节改掉
	tampered := append([]byte(nil), encrypted...)
	tampered[len(tampered)-1] ^= 0xFF
	if _, err := DecryptMedia(tampered, testAESKey); err == nil {
		t.Error("填充字节被改过，该报错")
	}

	// 翻转中间一块的密文位，解出来极可能填充不一致
	flipped := append([]byte(nil), encrypted...)
	flipped[0] ^= 0x01
	if _, err := DecryptMedia(flipped, testAESKey); err == nil {
		t.Log("这次翻转恰好没破坏填充（概率 1/256），不算失败")
	}
}

func Test密文长度不对要报错(t *testing.T) {
	if _, err := DecryptMedia([]byte{}, testAESKey); err == ErrEmptyCiphertext {
		return
	} else if err == nil {
		t.Error("空密文该报 ErrEmptyCiphertext")
	}
	if _, err := DecryptMedia(bytes.Repeat([]byte{1}, 17), testAESKey); err == nil {
		t.Error("非整块的密文该报错")
	}
}

// TestAesKey三种格式
//
// **前两种是同一个值，第三种不是**——这是最容易搞混的地方：
//
//   - 32 位 hex「0123456789abcdef0123456789abcdef」按 hex 解 = 16 个原始字节；
//   - base64 包着的那个 hex 字符串，解出同样的 16 个原始字节 —— 真机 `media.aes_key`
//     就是这一种；
//   - 而 base64「MDEyMzQ1Njc4OWFiY2RlZg==」解出的是 ASCII 的那 16 个字符，**值不同**。
//
// 所以「三种格式都支持」不等于「三者等价」。真机数据落在第二种上。
func TestAesKey三种格式(t *testing.T) {
	raw16, _ := hex.DecodeString(testAESKey)

	cases := []struct {
		name string
		key  string
		want []byte
	}{
		{"32 位 hex", testAESKey, raw16},
		{"base64 包着的 hex", "MDEyMzQ1Njc4OWFiY2RlZjAxMjM0NTY3ODlhYmNkZWY=", raw16},
		{"原始 base64", "MDEyMzQ1Njc4OWFiY2RlZg==", []byte("0123456789abcdef")},
	}
	for _, c := range cases {
		got, err := ParseAesKey(c.key)
		if err != nil {
			t.Errorf("%s：%v", c.name, err)
			continue
		}
		if !bytes.Equal(got, c.want) {
			t.Errorf("%s：解出 %q，期望 %q", c.name, got, c.want)
		}
	}

	if _, err := ParseAesKey(""); err == nil {
		t.Error("空 aeskey 该报错")
	}
	if _, err := ParseAesKey("这不是密钥"); err == nil {
		t.Error("认不出的格式该报错")
	}
}

// ── ret 归一化 ─────────────────────────────────────────

// TestRet缺席算成功 见 IsSuccessRet 的说明：这是「长轮询正常、游标推进、
// 却一条消息都收不到」的根因。
func TestRet缺席算成功(t *testing.T) {
	if !IsSuccessRet(false, 0) {
		t.Error("ret 缺席该算成功")
	}
	if !IsSuccessRet(true, RetOK) {
		t.Error("ret=0 该算成功")
	}
	if IsSuccessRet(true, -1) {
		t.Error("ret=-1 不该算成功")
	}
}

// ── 辅助 ───────────────────────────────────────────────

func mustParseOne(t *testing.T, raw string) WeixinMessage {
	t.Helper()
	var message RawMessage
	if err := json.Unmarshal([]byte(raw), &message); err != nil {
		t.Fatalf("解析 fixture 失败：%v", err)
	}
	return ParseMessage(message, "acct-1")
}
