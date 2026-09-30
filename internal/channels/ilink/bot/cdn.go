package bot

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"time"
)

// CDN 媒体下载。
//
// 流程：GET `media.FullURL` → 得到加密字节 → AES 解密 → 原始文件。
//
// `full_url` 是实测发现的字段（技术方案没提，但它省去了手工拼 URL）。
// 若某条消息没有 `full_url`，则回退到用 `encrypt_query_param` 拼默认 CDN 地址。
//
// ## 这里的错误必须往上抛
//
// 静默返回空切片会让上层以为「文件是空的」而不是「没取到」——
// 而那两种要走的路完全不同（前者会走解析入库，产出难以追查的脏数据）。
// 渠道层的 `FetchMedia` 同样如此。
const defaultDownloadTimeout = 60 * time.Second

// maxDownloadBytes 一次下载的字节上限。**是 var**：用例要把它调小（见 send_test）。
//
// CDN 上的那份长度是远端说了算的，而无条件 `ReadAll` 等于让对面决定我们分配多少内存。
// 512MB 对家庭照片/视频够用，又不至于一次 OOM。
var maxDownloadBytes int64 = 512 << 20

// BuildDownloadURL 拼出下载地址。
//
// 优先用实测提供的 `full_url`；缺失时回退到按 `encrypt_query_param` 拼接。
func BuildDownloadURL(media CDNMedia) (string, error) {
	if media.FullURL != "" {
		return media.FullURL, nil
	}
	if media.EncryptQueryParam != "" {
		// **只转义一次**：对已经转义过的值再转义会把 `%` 变成 `%25`
		return defaultCDNBase + "/download?encrypted_query_param=" +
			url.QueryEscape(media.EncryptQueryParam), nil
	}
	return "", fmt.Errorf("媒体引用里既无 full_url 也无 encrypt_query_param，无法构造下载地址")
}

// DownloadMedia 下载并解密媒体。
//
// aeskey 取自**同级字段**（`image_item.aeskey` 是平铺的 hex，文件项则只能从
// `media.aes_key` 取）——两级都试，因为协议在图片与文件上不一致，而报错时
// 「解不开」远不如「没有密钥」能指路。
func DownloadMedia(ctx context.Context, c *client, media CDNMedia, aeskey string) ([]byte, error) {
	if aeskey == "" {
		// 图片项有平铺 aeskey，文件项没有——所以这里回落到 media 里那份
		aeskey = media.AesKey
	}
	if aeskey == "" {
		return nil, fmt.Errorf("缺少 aeskey，无法解密媒体")
	}

	downloadURL, err := BuildDownloadURL(media)
	if err != nil {
		return nil, err
	}

	var lastErr error
	for attempt := 0; attempt < mediaRetry.Attempts; attempt++ {
		encrypted, retryable, err := fetchEncrypted(ctx, c, downloadURL)
		if err != nil {
			lastErr = err
			if !retryable {
				return nil, err
			}
		} else {
			plain, decryptErr := DecryptMedia(encrypted, aeskey)
			if decryptErr != nil {
				// **解密失败不重试**：重下同一份字节，解出来还是同样的错。
				// 而这个错（填充不一致、长度不对）恰恰说明**字节本身有问题**，
				// 换个时间再下一遍说不定就好了——但那掩盖了真正的原因
				return nil, fmt.Errorf("解密媒体失败：%w", decryptErr)
			}
			return plain, nil
		}

		if attempt < mediaRetry.Attempts-1 {
			select {
			case <-time.After(mediaRetry.Backoff(attempt)):
			case <-ctx.Done():
				return nil, ctx.Err()
			}
		}
	}
	return nil, fmt.Errorf("媒体下载失败（已试 %d 次）：%w", mediaRetry.Attempts, lastErr)
}

// fetchEncrypted 取回加密字节。第二个返回值 false 表示**不该重试**。
//
// 读 body 与超时都交给 `getBytes`：**谁读完 body 谁才持有那个超时 ctx**
// （见 http.go 的 withTimeout）。以前这里是「先拿响应、再自己读 body」，而超时的
// `cancel` 在响应交出来的那一刻就触发了——大于读缓冲的媒体一律以 `context canceled`
// 收尾，而且重试三次都栽在同一个原因上。
func fetchEncrypted(ctx context.Context, c *client, downloadURL string) ([]byte, bool, error) {
	body, status, err := c.getBytes(ctx, downloadURL, defaultDownloadTimeout, maxDownloadBytes)
	if err != nil {
		// 超过上限是**确定性的**：重下同一份还是这么大，试三次只是白等
		if errors.Is(err, errResponseTooLarge) {
			return nil, false, err
		}
		return nil, true, err // 网络层失败：可重试
	}

	// 403/404 这类是确定性的——资源没了或没权限，重下多少次都一样
	if IsClientError(status) {
		return nil, false, fmt.Errorf("媒体下载失败，HTTP %d（不重试）", status)
	}
	if status != http.StatusOK {
		return nil, true, fmt.Errorf("媒体下载返回 HTTP %d", status)
	}
	if len(body) == 0 {
		return nil, true, fmt.Errorf("媒体下载返回空内容")
	}
	return body, false, nil
}
