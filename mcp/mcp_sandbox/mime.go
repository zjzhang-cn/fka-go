// 文件类型判定：内容嗅探为主，扩展名兜底。**只用于 export**（发给用户的媒体）。
//
// read（发给模型）刻意不用这套：它要的是纯嗅探，好让 SVG 保持 `text/xml`、当文本喂给
// 模型（视觉接口普遍不收 image/svg+xml），而不是被后缀纠正成图片节点。
//
// ## 为什么需要后缀兜底
//
// `http.DetectContentType` 只认字节特征。**SVG 是 XML 文本**，会被它认成
// `text/xml; charset=utf-8`（带 `<?xml`）或 `text/plain; charset=utf-8`（裸 `<svg`）——
// 类型错了，`export` 交给渠道时就不是 `image/svg+xml`，浏览器不会当图片渲染。
// 嗅探同样认不出的还有 avif / mov / mkv / ogg / m4a / flac 等，一并按后缀补。
//
// ## 只兜「嗅探说不出具体类型」的
//
// 嗅探给出 `image/png`、`audio/mpeg`、`video/mp4` 这类具体结果时**以内容为准**——内容
// 比后缀可信（后缀能随手改）。只有当结果落在 `text/*` 或 `application/octet-stream`
// （即“是文本”或“不知道”）时，才拿后缀再判一次。
package main

import (
	"net/http"
	"path/filepath"
	"strings"
)

// extMIME 只列**嗅探认不出**、必须靠后缀补的媒体类型。png/jpeg/gif/webp/bmp、
// mp4/webm、mp3/wav/aiff/midi 等已被嗅探覆盖，列进来也不会被用到，故不列。
var extMIME = map[string]string{
	".svg":  "image/svg+xml",
	".avif": "image/avif",
	".mov":  "video/quicktime",
	".mkv":  "video/x-matroska",
	".ogv":  "video/ogg",
	".ogg":  "audio/ogg",
	".oga":  "audio/ogg",
	".m4a":  "audio/mp4",
	".aac":  "audio/aac",
	".flac": "audio/flac",
	".opus": "audio/opus",
}

// detectMIME 判定一个文件的 MIME：先嗅探（head 只需开头一段），认不出再按后缀。
func detectMIME(name string, head []byte) string {
	mime := http.DetectContentType(head)
	if !genericMIME(mime) {
		return mime
	}
	if mapped := extMIME[strings.ToLower(filepath.Ext(name))]; mapped != "" {
		return mapped
	}
	return mime
}

// genericMIME 嗅探结果「没说出具体类型」：任意字节认成 octet-stream，文本认成 text/*。
func genericMIME(m string) bool {
	return m == "application/octet-stream" || strings.HasPrefix(m, "text/")
}

// isTextMIME MIME 是不是明确的文本类型。`text/*` 之外，还认几个常见的文本型
// application 类型——read 该把它们当文本读，而不是当二进制只回元信息。
//
// 与 internal/messages/media.go 的同名判定保持同一套取值：两边都要「PDF/zip 是二进制、
// JSON/XML 是文本」这条一致，否则同一条消息在入站与沙盒里表现不同。
func isTextMIME(m string) bool {
	if strings.HasPrefix(m, "text/") {
		return true
	}
	switch m {
	case "application/json", "application/ld+json", "application/xml", "application/xhtml+xml",
		"application/javascript", "application/x-javascript", "application/ecmascript",
		"application/x-sh", "application/x-shellscript", "application/x-yaml", "application/yaml",
		"application/toml", "application/sql", "application/csv", "application/rtf",
		"application/x-httpd-php", "application/x-python", "application/x-ruby":
		return true
	}
	return false
}
