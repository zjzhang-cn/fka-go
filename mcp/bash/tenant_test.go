// 多租户隔离的用例：身份从 `_meta` 来、缺身份 fail-closed、每个 principal 一个
// 沙盒根、跨身份读不到对方的文件。
//
// 用 direct 模式（与 newTestSandbox 一致）：这一组钉的是"根怎么切、文件在哪"，
// 与用不用 bwrap 无关；真实的 bwrap 隔离另有一组用例。
package main

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
)

func testTenantServer(t *testing.T, base string) *bashServer {
	t.Helper()
	return &bashServer{opts: Options{Root: base, Mode: ModeDirect}, sandboxes: map[string]*Sandbox{}}
}

// Test多租户_缺身份被拒 fail-closed：没有 `_meta` 身份就拒绝，绝不落到共享目录——
// 那等于「绕过 agent 就全共享」。
func Test多租户_缺身份被拒(t *testing.T) {
	requireBash(t)
	impl := testTenantServer(t, t.TempDir())

	result, err := impl.handleRun(context.Background(),
		callRequestAs("", map[string]any{"command": "echo hi"}))
	if err != nil {
		t.Fatal(err)
	}
	if !result.IsError {
		t.Fatal("缺身份该被拒")
	}
	if body := resultText(t, result); !strings.Contains(body, "身份") {
		t.Errorf("该说清是缺身份：%s", body)
	}
}

// Test多租户_身份各占一个子目录 每个 principal 一个根：`<基根>/<safe(principal)>`，
// 同一身份复用同一条（缓存）。
func Test多租户_身份各占一个子目录(t *testing.T) {
	requireBash(t)
	base := t.TempDir()
	impl := testTenantServer(t, base)

	alice, err := impl.sandboxFor("web:alice")
	if err != nil {
		t.Fatal(err)
	}
	bob, err := impl.sandboxFor("web:bob")
	if err != nil {
		t.Fatal(err)
	}

	if want := filepath.Join(base, "web_alice"); alice.Root != want {
		t.Errorf("alice 的根该是 %q，实际 %q", want, alice.Root)
	}
	if alice.Root == bob.Root {
		t.Fatal("两个身份的根不该相同")
	}
	again, _ := impl.sandboxFor("web:alice")
	if again != alice {
		t.Error("同一身份该复用同一条沙盒")
	}
}

// Test多租户_隔离真实生效 alice 写的文件，bob 读不到；alice 自己读得到。
func Test多租户_隔离真实生效(t *testing.T) {
	requireBash(t)
	base := t.TempDir()
	impl := testTenantServer(t, base)

	res, err := impl.handleRun(context.Background(),
		callRequestAs("web:alice", map[string]any{"command": "printf secret > mine.txt"}))
	if err != nil || res.IsError {
		t.Fatalf("alice 建文件失败：err=%v res=%+v", err, res)
	}

	read, err := impl.handleRead(context.Background(),
		callRequestAs("web:bob", map[string]any{"path": "mine.txt"}))
	if err != nil {
		t.Fatal(err)
	}
	if !read.IsError {
		t.Fatal("bob 不该读到 alice 的文件")
	}

	own, err := impl.handleRead(context.Background(),
		callRequestAs("web:alice", map[string]any{"path": "mine.txt"}))
	if err != nil || own.IsError {
		t.Fatalf("alice 该读得到自己的文件：err=%v res=%+v", err, own)
	}
	if !strings.Contains(resultText(t, own), "secret") {
		t.Error("alice 读到的内容不对")
	}
}
