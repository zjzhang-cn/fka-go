// resources 的用例：把沙盒文件按 MCP 标准资源暴露出去。
//
//   - 固定资源 `sandbox:///`：读出本租户文件清单（SDK 不能按租户动态列举，用读取兜底）；
//   - 资源模板 `sandbox:///{+path}`：按路径读文件，分类与 read 工具一致；
//   - 身份仍走 `_meta`，缺身份 fail-closed。
package main

import (
	"context"
	"strings"
	"testing"

	"github.com/mark3labs/mcp-go/mcp"
)

// readResourceRequest 造一个资源读取请求。path 为空表示读根清单。
func readResourceRequest(principal, path string) mcp.ReadResourceRequest {
	var request mcp.ReadResourceRequest
	if principal != "" {
		request.Params.Meta = mcp.NewMetaFromMap(map[string]any{metaPrincipalKey: principal})
	}
	if path != "" {
		// **照着 SDK 的形状**：uritemplate 把模板变量塞成 `[]string`，不是 string。
		request.Params.Arguments = map[string]any{"path": []string{path}}
	}
	return request
}

// Test资源_根清单列出文件 固定资源 `sandbox:///` 读出本租户根下的文件（相对路径 + 大小）。
func Test资源_根清单列出文件(t *testing.T) {
	s := newTestSandbox(t)
	writeFile(t, s, "note.txt", []byte("hi"))
	impl := newTestServer(s)

	contents, err := impl.handleResourceRoot(context.Background(), readResourceRequest(testPrincipal, ""))
	if err != nil {
		t.Fatal(err)
	}
	if len(contents) != 1 {
		t.Fatalf("该有一份清单，实际 %d", len(contents))
	}
	text, ok := contents[0].(mcp.TextResourceContents)
	if !ok {
		t.Fatalf("清单该是文本资源，实际 %T", contents[0])
	}
	if !strings.Contains(text.Text, "note.txt") {
		t.Errorf("清单该含文件名：%q", text.Text)
	}
	if text.URI != ResourceRootURI {
		t.Errorf("URI 该是 %q，实际 %q", ResourceRootURI, text.URI)
	}
}

// Test资源_按路径读文本 模板按路径读文本文件，返回 text 内容块。
func Test资源_按路径读文本(t *testing.T) {
	s := newTestSandbox(t)
	writeFile(t, s, "note.txt", []byte("hello 资源"))
	impl := newTestServer(s)

	contents, err := impl.handleResourceFile(context.Background(), readResourceRequest(testPrincipal, "note.txt"))
	if err != nil {
		t.Fatal(err)
	}
	text, ok := contents[0].(mcp.TextResourceContents)
	if !ok {
		t.Fatalf("该是文本资源，实际 %T", contents[0])
	}
	if text.Text != "hello 资源" {
		t.Errorf("内容不对：%q", text.Text)
	}
	if text.URI != ResourceRootURI+"note.txt" {
		t.Errorf("URI 不对：%q", text.URI)
	}
}

// Test资源_按路径读图片是blob节点 图片必须是 blob 资源（base64），不是塞进文本。
func Test资源_按路径读图片是blob节点(t *testing.T) {
	s := newTestSandbox(t)
	png := []byte("\x89PNG\r\n\x1a\n\x00\x00\x00\x00")
	writeFile(t, s, "pic.png", png)
	impl := newTestServer(s)

	contents, err := impl.handleResourceFile(context.Background(), readResourceRequest(testPrincipal, "pic.png"))
	if err != nil {
		t.Fatal(err)
	}
	blob, ok := contents[0].(mcp.BlobResourceContents)
	if !ok {
		t.Fatalf("图片该是 blob 资源，实际 %T", contents[0])
	}
	if !strings.HasPrefix(blob.MIMEType, "image/") {
		t.Errorf("MIME 该是 image/*，实际 %q", blob.MIMEType)
	}
	if blob.Blob == "" {
		t.Error("blob 不该为空")
	}
}

// Test资源_越界被拒 路径走与 read 同一套越界检查。
func Test资源_越界被拒(t *testing.T) {
	impl := newTestServer(newTestSandbox(t))
	if _, err := impl.handleResourceFile(context.Background(), readResourceRequest(testPrincipal, "../secret")); err == nil {
		t.Error("越界路径该被拒")
	}
	if _, err := impl.handleResourceFile(context.Background(), readResourceRequest(testPrincipal, "/etc/passwd")); err == nil {
		t.Error("绝对路径该被拒")
	}
}

// Test资源_缺身份被拒 fail-closed：没有 `_meta` 身份就拒绝，绝不落到共享目录。
func Test资源_缺身份被拒(t *testing.T) {
	impl := newTestServer(newTestSandbox(t))
	if _, err := impl.handleResourceRoot(context.Background(), readResourceRequest("", "")); err == nil {
		t.Error("根清单缺身份该被拒")
	}
	if _, err := impl.handleResourceFile(context.Background(), readResourceRequest("", "note.txt")); err == nil {
		t.Error("按路径读缺身份该被拒")
	}
}
