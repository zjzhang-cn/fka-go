package bot

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"math/big"
	"net/http"
	"strings"
	"time"
)

// WeixinAccount 一个 iLink 账号。
//
// **一个账号一个渠道实例**——接缝据此保证 `(种类, 账号)` 与跨渠道账号标识唯一。
type WeixinAccount struct {
	// ID 账号唯一标识。**跨渠道必须唯一**（接缝会查），
	// 而它同时是会话历史文件名的前半段
	ID string
	// BotToken Bearer Token
	BotToken string
	// BaseURL API 基座地址
	BaseURL string
	// ILinkBotID Bot ID
	ILinkBotID string
	// ILinkUserID 用户 ID
	ILinkUserID string
	// Status 账号状态
	Status string
}

// 账号状态。**与 channels.Status 的取值刻意一致**——
// 转换放在渠道适配层，不在协议层。
const (
	AccountOnline  = "online"
	AccountOffline = "offline"
	AccountExpired = "expired"
)

// ── 鉴权头 ──────────────────────────────────────────────

// generateUin 生成 X-WECHAT-UIN。协议要求每次请求带一个随机 uint32 的
// **十进制字符串的 base64**。
//
// 注意是**字符串的 base64**，不是 4 个字节的 base64——后者会解出乱码。
func generateUin() string {
	// crypto/rand 而不是 math/rand：这个值每次都换，而可预测的随机数在这里
	// 没有价值却要多一个依赖
	var buf [4]byte
	if _, err := rand.Read(buf[:]); err != nil {
		// 读不到随机源时退回时间戳低位——请求头而已，不值得因此失败
		binary.LittleEndian.PutUint32(buf[:], uint32(time.Now().UnixNano()))
	}
	// 取 31 位，保持与 Node 侧 `Math.floor(Math.random() * 0xFFFFFFFF)` 同一个量级，
	// 且十进制长度稳定在 9-10 位
	value := binary.BigEndian.Uint32(buf[:]) % 0x7FFFFFFF
	return base64.StdEncoding.EncodeToString([]byte(big.NewInt(int64(value)).String()))
}

// headers 构造 iLink 请求头。getupdates 与 sendmessage 用同一套鉴权，
// 因此统一在此。
func headers(token string) http.Header {
	out := make(http.Header, 5)
	out.Set("Content-Type", "application/json")
	out.Set("AuthorizationType", "ilink_bot_token")
	out.Set("Authorization", "Bearer "+token)
	out.Set("X-WECHAT-UIN", generateUin())
	return out
}

// ── 极简 HTTP ───────────────────────────────────────────

// client 一个够用的 HTTP 客户端。**只封装这个协议需要的三种请求**——
// 引入一个通用客户端库只会带来一堆用不上的能力。
type client struct {
	http    *http.Client
	baseURL string
	token   string
}

// NewClient 造客户端。**导出是因为渠道适配层要用**：它要主动发请求
// （取媒体），而那时它拿得到账号、拿不到本包里的私有 client。
//
// httpClient 为 nil 时用一个**不设 `Timeout`** 的默认实现。
//
// ## 为什么刻意不设 http.Client.Timeout
//
// 它是「整次请求」的上限，与每条路径自己的 ctx 超时**取先到者**。设 30s 的话：
//
//   - `pollTimeout`（35s，注释写着「必须大于服务端的挂起时间，否则等于把长轮询
//     退化成短轮询」）永远达不到；
//   - `mediaTimeout` 与 `defaultDownloadTimeout`（各 60s）也永远达不到——
//     大文件传到一半被掐，而报错是 `context deadline exceeded`，指不到根因。
//
// 超时一律由 ctx 表达（见 `withTimeout`）。这与 `llm/openai` 那边是同一条理由：
// 两套超时并存时，短的那个先生效，另一套就成了摆设。
func NewClient(account WeixinAccount, httpClient *http.Client) *client {
	if httpClient == nil {
		httpClient = &http.Client{}
	}
	return &client{http: httpClient, baseURL: strings.TrimRight(account.BaseURL, "/"), token: account.BotToken}
}

// postJSON 发一个 JSON 请求，返回**原始响应字节**。
//
// ## 为什么要原始字节而不是解好的结构
//
// 响应里的 `message_id` 是 uint64。解成结构体时若声明成数字就会丢精度（超过 2^53），
// 而那个 id 正是引用还原要对上的那把钥匙。所以这里只负责把字节拿回来，
// **解析交给上层**——上层用 `json.RawMessage` 那一套。
func (c *client) postJSON(ctx context.Context, path string, body any, timeout time.Duration) ([]byte, error) {
	encoded, err := json.Marshal(body)
	if err != nil {
		return nil, fmt.Errorf("编码请求体失败：%w", err)
	}
	return c.do(ctx, http.MethodPost, c.baseURL+path, headers(c.token), bytes.NewReader(encoded), timeout)
}

// withTimeout 给一次请求套上限。**timeout <= 0 = 不限**。
//
// ## 谁持有 cancel，是一条必须写死的规矩
//
// **只有「读完整 body 之后才返回」的那一层能 `defer cancel()`。** 把超时 ctx 套在
// 「把响应交出去」的函数里（`doResponse` 以前就是这么干的），`cancel` 会在返回的
// 那一瞬间触发，而调用方是**返回之后**才读 body 的——大于读缓冲的响应会以
// `context canceled` 收尾。这不是理论：实测 8MB 的响应只读到 8061 字节。
//
// 反过来，只看响应头（`postBytes`）的路径可以放心 `defer cancel()`：头已经在内存里。
func (c *client) withTimeout(ctx context.Context, timeout time.Duration) (context.Context, context.CancelFunc) {
	if timeout <= 0 {
		return ctx, func() {}
	}
	return context.WithTimeout(ctx, timeout)
}

// postBytes 发一段原始字节。**Content-Type 要精确指定**——
// 上传 CDN 收的是 application/octet-stream，发成 JSON 会被拒。
//
// 调用方**不读 body**（只看响应头与状态码），所以这里的 `defer cancel()` 是安全的。
func (c *client) postBytes(ctx context.Context, url string, contentType string,
	data []byte, timeout time.Duration) (*http.Response, error) {
	ctx, cancel := c.withTimeout(ctx, timeout)
	defer cancel()

	return c.doResponse(ctx, http.MethodPost, url,
		http.Header{"Content-Type": []string{contentType}}, bytes.NewReader(data))
}

// getBytes 取一段原始字节，并把 HTTP 状态码一并交给调用方。
//
// **超时与读 body 都归它**（见 withTimeout 那条规矩），状态码则必须交出去——
// CDN 那两条路各自有重试策略，判据是「4xx 不重试 / 5xx 重试」。
func (c *client) getBytes(ctx context.Context, url string, timeout time.Duration) ([]byte, int, error) {
	ctx, cancel := c.withTimeout(ctx, timeout)
	defer cancel()

	response, err := c.doResponse(ctx, http.MethodGet, url, http.Header{}, nil)
	if err != nil {
		return nil, 0, err
	}
	defer func() { _ = response.Body.Close() }()

	data, err := io.ReadAll(response.Body)
	if err != nil {
		return nil, response.StatusCode, err
	}
	return data, response.StatusCode, nil
}

// do 发一次请求并读完响应体。**HTTP 状态码在这里判**。
//
// ## 为什么状态码必须在这里判，而不是留给 checkRet
//
// `checkRet` 判的是协议里的 `ret`，而「`ret` 缺席 = 成功」是实测结论（服务端成功时
// 不返回这个字段）。两者叠在一起，**网关的 502 HTML** 与 **401 的错误 JSON** 都会被
// 读成「没有 ret → 成功」——最后表现为 `SendText` 返回 nil error，而消息根本没出去。
//
// 只在这条路拦是安全的：它只服务 `postJSON`；CDN 那两条自己看状态码做
// 「4xx 不重试 / 5xx 重试」的判断，不能在这里被提前吃掉。
func (c *client) do(ctx context.Context, method, url string, requestHeaders http.Header,
	body io.Reader, timeout time.Duration) ([]byte, error) {
	ctx, cancel := c.withTimeout(ctx, timeout)
	defer cancel()

	response, err := c.doResponse(ctx, method, url, requestHeaders, body)
	if err != nil {
		return nil, err
	}
	defer func() { _ = response.Body.Close() }()

	data, err := io.ReadAll(response.Body)
	if err != nil {
		return nil, fmt.Errorf("读响应失败（%s）：%w", url, err)
	}
	if response.StatusCode >= 400 {
		// 错误体带进消息里：只报一个状态码的话，排查时还得再抓一次包
		return nil, fmt.Errorf("请求失败（%s）：HTTP %d %s", url, response.StatusCode, snippet(data))
	}
	return data, nil
}

// doResponse 只负责「把请求发出去、拿回响应」。**超时归调用方**，见 withTimeout。
func (c *client) doResponse(ctx context.Context, method, url string, requestHeaders http.Header,
	body io.Reader) (*http.Response, error) {
	request, err := http.NewRequestWithContext(ctx, method, url, body)
	if err != nil {
		return nil, fmt.Errorf("构造请求失败：%w", err)
	}
	for key, values := range requestHeaders {
		for _, value := range values {
			request.Header.Add(key, value)
		}
	}

	response, err := c.http.Do(request)
	if err != nil {
		return nil, fmt.Errorf("请求失败（%s）：%w", url, err)
	}
	return response, nil
}

// readRet 从响应字节里取 `ret`，并说明**这是不是一个 JSON 对象**。
//
// 两个布尔值必须分开：
//
//   - present：字段在不在。**成功时服务端根本不返回它**（实测），所以「缺席」与
//     ret=0 同等看待——见 IsSuccessRet 那段说明；
//   - isObject：这**是不是我们的服务端给的应答**。网关的 HTML、空体、别家的错误
//     JSON 都不是。把它们与「没有 ret」混为一谈，就是静默丢消息。
func readRet(data []byte) (ret int, present bool, isObject bool) {
	trimmed := bytes.TrimSpace(data)
	// `null` / 数组 / 裸标量都不是我们要的应答；先看首字节，免得
	// `json.Unmarshal` 对 `null` 默默成功
	if len(trimmed) == 0 || trimmed[0] != '{' {
		return 0, false, false
	}

	var payload struct {
		Ret *int `json:"ret"`
	}
	if err := json.Unmarshal(trimmed, &payload); err != nil {
		return 0, false, false
	}
	if payload.Ret == nil {
		return 0, false, true
	}
	return *payload.Ret, true, true
}

// checkRet 判一次响应的 ret。失败时把响应片段带进错误里——
// 只报一个 ret 码的话，排查时还得再抓一次包。
func checkRet(data []byte, what string) error {
	ret, present, isObject := readRet(data)
	if !isObject {
		return fmt.Errorf("%s失败：应答不是 JSON 对象（多半不是我们的服务端回的）：%s",
			what, snippet(data))
	}
	if IsSuccessRet(present, ret) {
		return nil
	}
	return fmt.Errorf("%s失败，ret=%d：%s", what, ret, snippet(data))
}

// statusOf 从 HTTP 响应里取状态码。
func statusOf(response *http.Response) int {
	if response == nil {
		return 0
	}
	return response.StatusCode
}

// IsClientError 4xx。**这类失败是确定性的，重试没有意义**——
// 重试只会把同一个错误重复三次再报一遍。
func IsClientError(status int) bool { return status >= 400 && status < 500 }
