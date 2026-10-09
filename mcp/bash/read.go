// read 工具：把沙盒里的一个文件**经由 MCP 内容块交给模型**。
//
// ## 和 `@引用` 同一套语义，但走的是 MCP 通道
//
// CLI 的 `@路径` 是在**输入层**把文件塞进本轮消息：文本并进问题、图片作附件、二进制
// 只附元信息。这里做的是同一件事，只是把「谁提供文件」从 CLI 挪到了 server。
//
// ## 返回的 content 数组可以混合多种类型
//
// 图片**必须**是一个独立的 `type:"image"` 内容节点（`data` 是 base64、`mimeType` 是
// 图片类型），不能把 base64 拼进文本——模型靠这个节点直接把图当图看。文本则是普通的
// `type:"text"` 节点。
//
// ## 为什么放在 server 里、为什么用标准内容块
//
// 沙盒可能不在 agent 本机。文件内容必须**以数据的形式穿过 MCP 通道**，而不是靠 agent
// 去读本地路径——stdio 现在能用，将来把这个 server 挪到 HTTP MCP 后面（同一份工具、
// 同一份 `CallToolResult`）也不用改一行：内容块是协议层的，不绑定传输。
//
// ## 边界与 run 完全一致
//
// 路径必须是沙盒根下的相对路径，走**词法 + 符号链接**两道越界检查（复用 sandbox.go
// 的 resolveWithin）。沙盒外一个字节都读不到。
//
// ## 上限是必须的
//
// path 是模型填的参数，没有上限就等于让它决定这一轮往上下文里塞多少字节。文本 32 KiB
// （超出截断），图片 5 MiB（超出直接拒，半张图没有意义）。
package main

import (
	"bytes"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
)

const (
	// ReadTextLimit 单个文本文件最多读多少字节，与 CLI `@引用` 的文本上限一致。
	ReadTextLimit = 32 * 1024
	// ReadImageLimit 单张图片最多读多少字节，超出直接拒，与 CLI `@引用` 一致。
	ReadImageLimit = 5 << 20
	// ReadAudioLimit 单个音频最多读多少字节，超出直接拒。
	ReadAudioLimit = 5 << 20
)

// fileKind 读出来的文件类别，对应 MCP 的几种内容节点。
type fileKind int

const (
	kindText fileKind = iota
	kindImage
	kindAudio
	kindBinary
)

// fileRead 一次读取的结果。
type fileRead struct {
	Path        string
	MIME        string
	Size        int64
	Kind        fileKind
	Text        string // Kind == kindText 时的正文
	ImageBase64 string // Kind == kindImage 时的 base64（不含 data: 前缀）
	AudioBase64 string // Kind == kindAudio 时的 base64（不含 data: 前缀）
	Truncated   bool
}

// ReadFile 在沙盒里读一个文件并按类别归类。**只对「读不了」返 error**；读到了就
// 走 fileRead，由调用方决定怎么交给模型。
func (s *Sandbox) ReadFile(raw string) (fileRead, error) {
	path, err := s.ResolveFile(raw)
	if err != nil {
		return fileRead{}, err
	}

	file, err := os.Open(path)
	if err != nil {
		return fileRead{}, fmt.Errorf("打不开这个文件：%w", err)
	}
	defer file.Close()

	info, err := file.Stat()
	if err != nil {
		return fileRead{}, fmt.Errorf("读取文件信息失败：%w", err)
	}

	// 先读开头一段做类型判断：图片与文本的上限不同，读多少取决于先判出来的类型
	head := make([]byte, 512)
	n, err := io.ReadFull(file, head)
	if err != nil && !errors.Is(err, io.EOF) && !errors.Is(err, io.ErrUnexpectedEOF) {
		return fileRead{}, errors.New("读取失败")
	}
	head = head[:n]

	out := fileRead{Path: raw, MIME: http.DetectContentType(head), Size: info.Size()}

	if strings.HasPrefix(out.MIME, "image/") {
		rest, err := io.ReadAll(io.LimitReader(file, ReadImageLimit+1))
		if err != nil {
			return fileRead{}, errors.New("读取失败")
		}
		data := append(append([]byte(nil), head...), rest...)
		if len(data) > ReadImageLimit {
			return fileRead{}, fmt.Errorf("图片超过 %d MiB，不读", ReadImageLimit>>20)
		}
		out.Kind = kindImage
		out.ImageBase64 = base64.StdEncoding.EncodeToString(data)
		return out, nil
	}

	if strings.HasPrefix(out.MIME, "audio/") {
		rest, err := io.ReadAll(io.LimitReader(file, ReadAudioLimit+1))
		if err != nil {
			return fileRead{}, errors.New("读取失败")
		}
		data := append(append([]byte(nil), head...), rest...)
		if len(data) > ReadAudioLimit {
			return fileRead{}, fmt.Errorf("音频超过 %d MiB，不读", ReadAudioLimit>>20)
		}
		out.Kind = kindAudio
		out.AudioBase64 = base64.StdEncoding.EncodeToString(data)
		return out, nil
	}

	// 二进制塞进文本只会浪费预算并弄坏编码：只回元信息
	if bytes.IndexByte(head, 0) >= 0 {
		out.Kind = kindBinary
		return out, nil
	}

	rest, err := io.ReadAll(io.LimitReader(file, ReadTextLimit+1))
	if err != nil {
		return fileRead{}, errors.New("读取失败")
	}
	data := append(append([]byte(nil), head...), rest...)
	if len(data) > ReadTextLimit {
		data = data[:ReadTextLimit]
		out.Truncated = true
	}
	out.Kind = kindText
	out.Text = string(data)
	return out, nil
}

// humanBytes 把字节数写成人看的大小。
func humanBytes(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for m := n / unit; m >= unit; m /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %cB", float64(n)/float64(div), "KMGTPE"[exp])
}
