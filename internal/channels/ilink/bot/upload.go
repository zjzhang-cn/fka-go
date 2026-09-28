package bot

import (
	"context"
	"crypto/md5"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"time"
)

// CDN 媒体上传。
//
// 流程（来自腾讯官方实现 `Tencent/openclaw-weixin` 的 `docs/protocol.md`）：
//
//  1. 读明文，算大小与 MD5
//  2. 生成 16 字节 AES 密钥 + 一个 filekey
//  3. 算填充后的密文大小（PKCS#7）
//  4. POST /ilink/bot/getuploadurl 取上传地址
//  5. AES-128-ECB 加密（PKCS#7）
//  6. POST 密文到上传地址，Content-Type: application/octet-stream
//  7. 从响应头读 `x-encrypted-param`
//  8. 把该参数与密钥塞进 sendmessage 的 media 引用
//
// ## 出站与入站的一个关键差异
//
// **出站的 AES 密钥是客户端自己生成的**（入站消息是服务端给的）。
// 密钥以 hex 字符串传给 getuploadurl，放进 media 时则编码为**该 hex 字符串的
// base64**——不是原始密钥的 base64。这两者的区别很容易搞混，测试里钉住了。
const defaultCDNBase = "https://novac2c.cdn.weixin.qq.com/c2c"

// 上传时声明的媒体类型。**注意序号与 MessageItem.Type 不同**——
// 混起来是这里最容易错的一处。
const (
	UploadMediaImage = 1
	UploadMediaVideo = 2
	UploadMediaFile  = 3
	UploadMediaVoice = 4
)

// GenerateAesKey 生成本次的 AES 密钥。返回 **32 个十六进制字符**——
// 这是协议要求的传递形态。
func GenerateAesKey() (string, error) {
	var buf [16]byte
	if _, err := rand.Read(buf[:]); err != nil {
		// 这里**必须失败**：密钥是本次上传的唯一凭据，退化成时间戳等于
		// 用可预测的值加密用户文件
		return "", fmt.Errorf("生成 AES 密钥失败：%w", err)
	}
	return hex.EncodeToString(buf[:]), nil
}

// GenerateFileKey 生成 filekey。官方实现用它参与拼上传地址，因此需要唯一。
func GenerateFileKey() (string, error) {
	var buf [16]byte
	if _, err := rand.Read(buf[:]); err != nil {
		return "", fmt.Errorf("生成 filekey 失败：%w", err)
	}
	return hex.EncodeToString(buf[:]), nil
}

// EncodeAesKeyForMedia 把 hex 密钥字符串编码为 media 里使用的 base64 形态。
//
// **是 hex 字符串的 base64，不是原始密钥字节的 base64。** 官方说明就是
// 「把十六进制密钥字符串按 base64 编码」。真机入站消息印证了这一点——
// 那个 base64 解出来是 32 个 hex 字符，再 hex 解一次才是 16 字节密钥。
func EncodeAesKeyForMedia(hexKey string) string {
	return base64.StdEncoding.EncodeToString([]byte(hexKey))
}

// PaddedLength 算 PKCS#7 填充后的长度。
//
// 整块时**要补一整块**（16 → 32），不是不补。
func PaddedLength(plainLength int) int { return plainLength + pkcs7PadSize(plainLength) }

// UploadResult 上传的结果。
type UploadResult struct {
	// Media 可直接放进 sendmessage 的 media 引用
	Media CDNMedia
	// Aeskey 本次使用的 AES 密钥，**hex 形态**（32 个字符）。
	//
	// Media.AesKey 是它的 base64；图片项还要求把 hex 平铺在 `image_item.aeskey`
	// 上，所以两份都留着，而不是让调用方从 base64 反解回 hex。
	Aeskey string
	// MD5 明文 MD5（十六进制）
	MD5 string
	// Size 明文字节数
	Size int
}

// uploadURLResponse getuploadurl 的应答。
type uploadURLResponse struct {
	UploadParam   string `json:"upload_param"`
	UploadFullURL string `json:"upload_full_url"`
}

// getUploadURLBody getuploadurl 的请求体。
//
// 字段名照线上形状（snake_case）。**rawsize / rawfilemd5 是明文的**，
// filesize 是**填充后密文**的大小——两者不同，传错服务端会算不对。
type getUploadURLBody struct {
	Filekey    string `json:"filekey"`
	MediaType  int    `json:"media_type"`
	ToUserID   string `json:"to_user_id"`
	RawSize    int    `json:"rawsize"`
	RawFileMD5 string `json:"rawfilemd5"`
	FileSize   int    `json:"filesize"`
	// NoNeedThumb 官方实现固定为 true：只传原图/原文件，不做缩略图
	NoNeedThumb bool     `json:"no_need_thumb"`
	Aeskey      string   `json:"aeskey"`
	BaseInfo    baseInfo `json:"base_info"`
}

// GetUploadURL 取上传地址。
//
// 官方实现优先使用 `upload_full_url`；缺失时客户端自行用
// `upload_param` + `filekey` 拼 CDN 地址。
func GetUploadURL(ctx context.Context, c *client, filekey string, mediaType int,
	toUserID string, rawSize int, rawMD5 string, cipherSize int, aeskey string,
	timeout time.Duration) (uploadURLResponse, error) {

	var response uploadURLResponse
	data, err := c.postJSON(ctx, "/ilink/bot/getuploadurl", getUploadURLBody{
		Filekey:     filekey,
		MediaType:   mediaType,
		ToUserID:    toUserID,
		RawSize:     rawSize,
		RawFileMD5:  rawMD5,
		FileSize:    cipherSize,
		NoNeedThumb: true,
		Aeskey:      aeskey,
		BaseInfo:    baseInfo{ChannelVersion},
	}, timeout)
	if err != nil {
		return response, fmt.Errorf("取上传地址失败：%w", err)
	}
	if err := checkRet(data, "getuploadurl"); err != nil {
		return response, err
	}

	// 上传地址**只从 data 里取，不从 resp 里取**——resp 那一层是 HTTP 包装
	if err := json.Unmarshal(data, &response); err != nil {
		return response, fmt.Errorf("解析 getuploadurl 响应失败：%w", err)
	}
	return response, nil
}

// BuildUploadURL 拼 CDN 上传地址（无 upload_full_url 时的回退路径）。
func BuildUploadURL(response uploadURLResponse, filekey string) (string, error) {
	if response.UploadFullURL != "" {
		return response.UploadFullURL, nil
	}
	if response.UploadParam == "" {
		return "", fmt.Errorf("getuploadurl 响应里既无 upload_full_url 也无 upload_param")
	}
	// **只转义一次**：对已经转义过的值再转义会把 `%` 变成 `%25`，
	// 而服务端解出来的东西就不对了
	return defaultCDNBase + "/upload?encrypted_query_param=" +
		url.QueryEscape(response.UploadParam) +
		"&filekey=" + url.QueryEscape(filekey), nil
}

// retryPolicy 重试策略。
//
// **4xx 一律不重试**：那类失败是确定性的（地址错了、密文格式不对），
// 重试只会把同一个错误重复三次再报一遍，还平白多等两轮退避。
type retryPolicy struct {
	// Attempts 总共试几次（含第一次）
	Attempts int
	// BaseDelay 第一次退避。**逐次翻倍**
	BaseDelay time.Duration
}

var mediaRetry = retryPolicy{Attempts: 3, BaseDelay: time.Second}

// Backoff 第 attempt 次失败后该等多久（attempt 从 0 起）。
func (p retryPolicy) Backoff(attempt int) time.Duration {
	return p.BaseDelay << attempt
}

// UploadMedia 上传一段字节到 CDN，返回可直接用于 sendmessage 的 media 引用。
//
// 失败一律返错——**返回一个指向空资源的引用会让发送静默失效**：
// 上层会以为发出去了，而对方什么都收不到。
func UploadMedia(ctx context.Context, c *client, toUserID string, data []byte,
	mediaType int, timeout time.Duration) (UploadResult, error) {

	var result UploadResult
	if len(data) == 0 {
		return result, fmt.Errorf("待上传内容为空")
	}

	filekey, err := GenerateFileKey()
	if err != nil {
		return result, err
	}
	aeskey, err := GenerateAesKey()
	if err != nil {
		return result, err
	}

	digest := md5.Sum(data)
	cipherSize := PaddedLength(len(data))

	response, err := GetUploadURL(ctx, c, filekey, mediaType, toUserID,
		len(data), hex.EncodeToString(digest[:]), cipherSize, aeskey, timeout)
	if err != nil {
		return result, err
	}

	uploadURL, err := BuildUploadURL(response, filekey)
	if err != nil {
		return result, err
	}

	// **先加密再上传**：CDN 收的就是密文，密钥只经 getuploadurl 传给服务端
	ciphertext, err := EncryptMedia(data, aeskey)
	if err != nil {
		return result, fmt.Errorf("加密待上传内容失败：%w", err)
	}
	// 声明的 filesize 必须与真正发上去的字节数一致
	if len(ciphertext) != cipherSize {
		return result, fmt.Errorf("密文长度 %d 与声明的 filesize %d 不一致", len(ciphertext), cipherSize)
	}

	var lastErr error
	for attempt := 0; attempt < mediaRetry.Attempts; attempt++ {
		encryptedParam, retryable, err := postCiphertext(ctx, c, uploadURL, ciphertext, timeout)
		if err == nil {
			return UploadResult{
				Media: CDNMedia{
					EncryptQueryParam: encryptedParam,
					AesKey:            EncodeAesKeyForMedia(aeskey),
					EncryptType:       1,
				},
				Aeskey: aeskey,
				MD5:    hex.EncodeToString(digest[:]),
				Size:   len(data),
			}, nil
		}
		lastErr = err
		if !retryable {
			return result, err
		}
		if attempt < mediaRetry.Attempts-1 {
			select {
			case <-time.After(mediaRetry.Backoff(attempt)):
			case <-ctx.Done():
				return result, ctx.Err()
			}
		}
	}
	return result, fmt.Errorf("CDN 上传失败（已试 %d 次）：%w", mediaRetry.Attempts, lastErr)
}

// postCiphertext 把密文 POST 上去，返回 `x-encrypted-param`。
//
// 第二个返回值 false 表示**不该重试**。
func postCiphertext(ctx context.Context, c *client, uploadURL string,
	ciphertext []byte, timeout time.Duration) (string, bool, error) {

	response, err := c.postBytes(ctx, uploadURL, "application/octet-stream", ciphertext, timeout)
	if err != nil {
		return "", true, err // 网络层失败：可重试
	}
	defer func() { _ = response.Body.Close() }()

	// 官方：成功需 HTTP 200 且 x-encrypted-param 非空
	param := response.Header.Get("x-encrypted-param")
	if IsClientError(statusOf(response)) {
		return "", false, fmt.Errorf("CDN 上传失败，HTTP %d（不重试）", statusOf(response))
	}
	if statusOf(response) != http.StatusOK {
		return "", true, fmt.Errorf("CDN 上传返回 HTTP %d", statusOf(response))
	}
	if param == "" {
		return "", false, fmt.Errorf("CDN 上传未返回 x-encrypted-param（HTTP %d），无法构造下载引用",
			statusOf(response))
	}
	return param, false, nil
}
