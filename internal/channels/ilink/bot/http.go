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
// httpClient 为 nil 时用带超时的默认实现。
func NewClient(account WeixinAccount, httpClient *http.Client) *client {
	if httpClient == nil {
		httpClient = &http.Client{Timeout: 30 * time.Second}
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

// postBytes 发一段原始字节。**Content-Type 要精确指定**——
// 上传 CDN 收的是 application/octet-stream，发成 JSON 会被拒。
func (c *client) postBytes(ctx context.Context, url string, contentType string,
	data []byte, timeout time.Duration) (*http.Response, error) {
	return c.doResponse(ctx, http.MethodPost, url,
		http.Header{"Content-Type": []string{contentType}}, bytes.NewReader(data), timeout)
}

// getBytes 取一段原始字节。
func (c *client) getBytes(ctx context.Context, url string, timeout time.Duration) ([]byte, error) {
	response, err := c.doResponse(ctx, http.MethodGet, url, http.Header{}, nil, timeout)
	if err != nil {
		return nil, err
	}
	defer func() { _ = response.Body.Close() }()
	return io.ReadAll(response.Body)
}

// getResponse 取一段响应，**不关 body**——调用方要看状态码与响应头。
func (c *client) getResponse(ctx context.Context, url string, timeout time.Duration) (*http.Response, error) {
	return c.doResponse(ctx, http.MethodGet, url, http.Header{}, nil, timeout)
}

func (c *client) do(ctx context.Context, method, url string, requestHeaders http.Header,
	body io.Reader, timeout time.Duration) ([]byte, error) {
	response, err := c.doResponse(ctx, method, url, requestHeaders, body, timeout)
	if err != nil {
		return nil, err
	}
	defer func() { _ = response.Body.Close() }()
	return io.ReadAll(response.Body)
}

func (c *client) doResponse(ctx context.Context, method, url string, requestHeaders http.Header,
	body io.Reader, timeout time.Duration) (*http.Response, error) {
	if timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, timeout)
		defer cancel()
	}

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

// readRet 从响应字节里取 `ret`。
//
// **成功时服务端根本不返回这个字段**，所以「缺席」要与 0 同等看待——
// 见 IsSuccessRet。
func readRet(data []byte) (ret int, present bool) {
	var payload struct {
		Ret *int `json:"ret"`
	}
	if err := json.Unmarshal(data, &payload); err != nil || payload.Ret == nil {
		return 0, false
	}
	return *payload.Ret, true
}

// checkRet 判一次响应的 ret。失败时把响应片段带进错误里——
// 只报一个 ret 码的话，排查时还得再抓一次包。
func checkRet(data []byte, what string) error {
	ret, present := readRet(data)
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
