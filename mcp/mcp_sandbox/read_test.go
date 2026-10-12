// read 工具的用例：把沙盒文件按类别经 MCP 内容块交给模型，以及「只读沙盒内」这条边界。
package main

import (
	"bytes"
	"context"
	"image"
	"image/color"
	"image/png"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/mark3labs/mcp-go/mcp"
)

func writeFile(t *testing.T, s *Sandbox, name string, data []byte) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(s.Root, name), data, 0o644); err != nil {
		t.Fatalf("写文件失败：%v", err)
	}
}

// TestRead_文本返回text内容块 文本以 text 内容块返回，并带一段头（对齐 @引用）。
func TestRead_文本返回text内容块(t *testing.T) {
	s := newTestSandbox(t)
	writeFile(t, s, "note.txt", []byte("hello sandbox"))
	impl := newTestServer(s)

	result, err := impl.handleRead(context.Background(), callRequest(map[string]any{"path": "note.txt"}))
	if err != nil {
		t.Fatal(err)
	}
	if result.IsError {
		t.Fatalf("读文本不该是 IsError：%s", resultText(t, result))
	}
	if got := result.Content[0].(mcp.TextContent).Type; got != "text" {
		t.Errorf("文本该是 text 节点，实际 type=%q", got)
	}
	body := resultText(t, result)
	if !strings.Contains(body, "hello sandbox") {
		t.Errorf("结果里该有正文：%s", body)
	}
	if !strings.Contains(body, "──── note.txt ────") {
		t.Errorf("该有段落头（对齐 @引用）：%s", body)
	}
}

// TestRead_图片返回image节点 图片**必须**是一个独立的 `type:"image"` 节点，data 是
// base64、mimeType 是图片类型——模型靠这个节点把图当图看，base64 不能拼进文本。
func TestRead_图片返回image节点(t *testing.T) {
	s := newTestSandbox(t)

	img := image.NewRGBA(image.Rect(0, 0, 1, 1))
	img.Set(0, 0, color.RGBA{R: 255, A: 255})
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatalf("编码 PNG 失败：%v", err)
	}
	writeFile(t, s, "pic.png", buf.Bytes())

	impl := newTestServer(s)
	result, err := impl.handleRead(context.Background(), callRequest(map[string]any{"path": "pic.png"}))
	if err != nil {
		t.Fatal(err)
	}
	if result.IsError {
		t.Fatalf("读图片不该是 IsError：%s", resultText(t, result))
	}
	if len(result.Content) != 2 {
		t.Fatalf("图片该返回 [text 说明, image 内容] 两块，实际 %d 块", len(result.Content))
	}
	content, ok := result.Content[1].(mcp.ImageContent)
	if !ok {
		t.Fatalf("第二块该是 ImageContent，实际 %T", result.Content[1])
	}
	if content.Type != "image" {
		t.Errorf("节点类型该是 image，实际 %q", content.Type)
	}
	if content.Data == "" || content.MIMEType != "image/png" {
		t.Errorf("图片内容不对：mime=%q data 长度=%d", content.MIMEType, len(content.Data))
	}
}

// TestRead_音频返回audio节点 音频文件以 `type:"audio"` 内容节点返回（base64 + mime）。
func TestRead_音频返回audio节点(t *testing.T) {
	s := newTestSandbox(t)
	// net/http 的 sniff 认 "ID3" 前缀为 audio/mpeg
	writeFile(t, s, "song.mp3", []byte("ID3\x03\x00\x00\x00\x00\x00"))
	impl := newTestServer(s)

	result, err := impl.handleRead(context.Background(), callRequest(map[string]any{"path": "song.mp3"}))
	if err != nil {
		t.Fatal(err)
	}
	if result.IsError {
		t.Fatalf("读音频不该是 IsError：%s", resultText(t, result))
	}
	if len(result.Content) != 2 {
		t.Fatalf("该返回 [text, audio] 两块，实际 %d 块", len(result.Content))
	}
	content, ok := result.Content[1].(mcp.AudioContent)
	if !ok {
		t.Fatalf("第二块该是 AudioContent，实际 %T", result.Content[1])
	}
	if content.Type != "audio" || content.Data == "" || content.MIMEType != "audio/mpeg" {
		t.Errorf("音频节点不对：type=%q mime=%q data 长度=%d", content.Type, content.MIMEType, len(content.Data))
	}
}

// TestRead_二进制只回元信息 非图片二进制不回内容，只给类型与大小。
func TestRead_二进制只回元信息(t *testing.T) {
	s := newTestSandbox(t)
	writeFile(t, s, "blob.bin", []byte{0, 1, 2, 3, 0, 4})
	impl := newTestServer(s)

	result, err := impl.handleRead(context.Background(), callRequest(map[string]any{"path": "blob.bin"}))
	if err != nil {
		t.Fatal(err)
	}
	body := resultText(t, result)
	if !strings.Contains(body, "二进制") {
		t.Errorf("该说明是二进制文件：%s", body)
	}
}

// TestRead_文本超限截断 超过上限的文本要截断并标记。
func TestRead_文本超限截断(t *testing.T) {
	s := newTestSandbox(t)
	writeFile(t, s, "big.txt", bytes.Repeat([]byte("a"), ReadTextLimit+100))

	file, err := s.ReadFile("big.txt")
	if err != nil {
		t.Fatal(err)
	}
	if !file.Truncated {
		t.Error("超过上限该标记截断")
	}
	if len(file.Text) != ReadTextLimit {
		t.Errorf("截断后正文该正好 %d 字节，实际 %d", ReadTextLimit, len(file.Text))
	}
}

// TestRead_越界被拒 绝对路径与 ../ 越界都不行。
func TestRead_越界被拒(t *testing.T) {
	s := newTestSandbox(t)

	if _, err := s.ReadFile("/etc/passwd"); err == nil {
		t.Error("绝对路径该被拒")
	}
	if _, err := s.ReadFile("../outside.txt"); err == nil {
		t.Error("../ 越界该被拒")
	}
}

// TestRead_符号链接越界被拒 沙盒里指向外面的软链不能被当成合法文件。
func TestRead_符号链接越界被拒(t *testing.T) {
	s := newTestSandbox(t)

	outside := filepath.Join(t.TempDir(), "secret.txt")
	if err := os.WriteFile(outside, []byte("top secret"), 0o644); err != nil {
		t.Fatalf("写外部文件失败：%v", err)
	}
	if err := os.Symlink(outside, filepath.Join(s.Root, "link.txt")); err != nil {
		t.Fatalf("建软链失败：%v", err)
	}
	if _, err := s.ReadFile("link.txt"); err == nil {
		t.Error("经符号链接指向沙盒外的文件该被拒")
	}
}

// TestRead_目录被拒 目录不是文件。
func TestRead_目录被拒(t *testing.T) {
	s := newTestSandbox(t)
	if err := os.Mkdir(filepath.Join(s.Root, "sub"), 0o755); err != nil {
		t.Fatalf("建目录失败：%v", err)
	}
	if _, err := s.ReadFile("sub"); err == nil {
		t.Error("目录该被拒")
	}
}

// TestRead_缺path要拒 没有路径就不知道读什么——回一句给模型看的话。
func TestRead_缺path要拒(t *testing.T) {
	impl := newTestServer(newTestSandbox(t))

	result, err := impl.handleRead(context.Background(), callRequest(map[string]any{}))
	if err != nil {
		t.Fatal(err)
	}
	if !result.IsError {
		t.Error("缺 path 该回一条 IsError 的结果")
	}
	if body := resultText(t, result); !strings.Contains(body, "path") {
		t.Errorf("该说清缺的是哪个参数：%s", body)
	}
}
