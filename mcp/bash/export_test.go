// export 工具的用例：把沙盒文件作为**标注给用户**的嵌入资源返回。
//
// 与 read 的边界完全一致（只读沙盒内），区别只在受众：read 给模型，export 给用户。
package main

import (
	"context"
	"encoding/base64"
	"os"
	"path/filepath"
	"testing"

	"github.com/mark3labs/mcp-go/mcp"
)

// exportedResource 从结果里取出那个嵌入资源块，并断言它就是 audience=user 的 blob。
func exportedResource(t *testing.T, result *mcp.CallToolResult) mcp.BlobResourceContents {
	t.Helper()
	for _, block := range result.Content {
		resource, ok := block.(mcp.EmbeddedResource)
		if !ok {
			continue
		}
		if resource.Annotations == nil {
			t.Fatal("嵌入资源没有注解：agent 拿不到 audience，就不知道该发给谁")
		}
		isUser := false
		for _, role := range resource.Annotations.Audience {
			if role == mcp.RoleUser {
				isUser = true
			}
		}
		if !isUser {
			t.Fatalf("audience 里没有 user：%v", resource.Annotations.Audience)
		}
		blob, ok := resource.Resource.(mcp.BlobResourceContents)
		if !ok {
			t.Fatalf("资源该是 blob（二进制安全的 base64），实际 %T", resource.Resource)
		}
		return blob
	}
	t.Fatal("结果里没有嵌入资源块")
	return mcp.BlobResourceContents{}
}

// TestExport_返回给用户的资源块 导出成功时：有头、有 blob、blob 解码等于原文、
// 且标注了 audience=user——agent 只认这个字段，不认它来自 bash。
func TestExport_返回给用户的资源块(t *testing.T) {
	s := newTestSandbox(t)
	writeFile(t, s, "报告.txt", []byte("hello 用户"))
	impl := newTestServer(s)

	result, err := impl.handleExport(context.Background(), callRequest(map[string]any{"path": "报告.txt"}))
	if err != nil {
		t.Fatal(err)
	}
	if result.IsError {
		t.Fatalf("导出不该是 IsError：%s", resultText(t, result))
	}

	blob := exportedResource(t, result)
	data, err := base64.StdEncoding.DecodeString(blob.Blob)
	if err != nil {
		t.Fatalf("blob 不是合法 base64：%v", err)
	}
	if string(data) != "hello 用户" {
		t.Errorf("blob 解码后应等于原文，实际 %q", data)
	}
}

// TestExport_越界被拒 发送路径与 read 同一条边界：只认沙盒根下的相对路径。
func TestExport_越界被拒(t *testing.T) {
	s := newTestSandbox(t)
	impl := newTestServer(s)

	for _, bad := range []string{"../secret.txt", "/etc/passwd"} {
		result, err := impl.handleExport(context.Background(), callRequest(map[string]any{"path": bad}))
		if err != nil {
			t.Fatal(err)
		}
		if !result.IsError {
			t.Errorf("路径 %q 该被拒", bad)
		}
	}
}

// TestExport_目录被拒 只能发普通文件。
func TestExport_目录被拒(t *testing.T) {
	s := newTestSandbox(t)
	if err := os.Mkdir(filepath.Join(s.Root, "sub"), 0o755); err != nil {
		t.Fatal(err)
	}
	impl := newTestServer(s)

	result, _ := impl.handleExport(context.Background(), callRequest(map[string]any{"path": "sub"}))
	if !result.IsError {
		t.Error("目录不该被当作可导出文件")
	}
}

// TestExport_超过上限被拒 上限是「模型指名的字节不让它决定我们分配多少内存」。
func TestExport_超过上限被拒(t *testing.T) {
	s := newTestSandbox(t)
	writeFile(t, s, "big.bin", make([]byte, 64))
	impl := newTestServer(s)

	original := ExportMaxBytes
	ExportMaxBytes = 16
	t.Cleanup(func() { ExportMaxBytes = original })

	result, _ := impl.handleExport(context.Background(), callRequest(map[string]any{"path": "big.bin"}))
	if !result.IsError {
		t.Error("超过上限该被拒")
	}
}
