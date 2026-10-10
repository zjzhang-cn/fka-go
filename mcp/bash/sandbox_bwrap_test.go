// Bubblewrap 模式的两组用例：**参数拼得对不对**（纯函数，随时能跑）与
// **隔离是不是真的**（真起一个 bwrap，缺 bwrap 或命名空间不可用时跳过）。
//
// 为什么值得单独一组：bwrap 的参数**顺序敏感**——`--bind <root> <root>` 若排在
// `--tmpfs /tmp` 之前，沙盒根（常落在 /tmp 下）会被 tmpfs 盖住，症状是 `--chdir`
// 直接失败。这种错只在真实调用里才暴露，所以既要钉参数顺序，也要真跑一次。
package main

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// bwrapUsable 真起一个最小 bwrap，确认内核允许非特权命名空间。
// 不允许（容器里常见）时跳过集成用例，而不是把环境问题报成代码问题。
func bwrapUsable(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("bwrap"); err != nil {
		t.Skip("没有安装 bubblewrap")
	}
	if err := exec.Command("bwrap", "--unshare-all", "--ro-bind", "/", "/", "--", "true").Run(); err != nil {
		t.Skipf("bwrap 不可用（命名空间被禁？）：%v", err)
	}
}

func indexOf(list []string, want string) int {
	for i, item := range list {
		if item == want {
			return i
		}
	}
	return -1
}

// TestBwrap_参数包含隔离项与正确顺序 钉住参数集合与「可写绑定必须最后」的顺序。
func TestBwrap_参数包含隔离项与正确顺序(t *testing.T) {
	bwrapUsable(t)
	s, err := NewSandbox(Options{Root: t.TempDir(), Mode: ModeBwrap})
	if err != nil {
		t.Fatalf("建沙盒失败：%v", err)
	}

	dir := filepath.Join(s.Root, "sub")
	args := s.bwrapArgs(dir, "echo hi")

	for _, want := range []string{"--die-with-parent", "--new-session", "--unshare-all", "--ro-bind", "--tmpfs", "--proc", "--dev", "--chdir"} {
		if indexOf(args, want) < 0 {
			t.Errorf("bwrap 参数里少了 %s：%v", want, args)
		}
	}

	// root 的可写绑定必须排在只读根与 /tmp 的 tmpfs **之后**
	bindAt := indexOf(args, "--bind")
	if bindAt < 0 {
		t.Fatalf("没有 --bind：%v", args)
	}
	if bindAt < indexOf(args, "--ro-bind") || bindAt < indexOf(args, "--tmpfs") {
		t.Errorf("--bind 必须排在 --ro-bind / --tmpfs 之后，否则沙盒根会被盖住：%v", args)
	}

	// 命令行以 `--` 分出 bwrap 参数与真正的命令
	tail := args[len(args)-4:]
	if tail[0] != "--" || tail[1] != s.Shell || tail[2] != "-c" || tail[3] != "echo hi" {
		t.Errorf("命令尾巴不对：%v", tail)
	}
}

// TestBwrap_shareNet保留网络 默认独立网络，显式 --share-net 时才保留。
func TestBwrap_shareNet保留网络(t *testing.T) {
	bwrapUsable(t)
	s, err := NewSandbox(Options{Root: t.TempDir(), Mode: ModeBwrap, ShareNetwork: true})
	if err != nil {
		t.Fatalf("建沙盒失败：%v", err)
	}
	if indexOf(s.bwrapArgs(s.Root, "true"), "--share-net") < 0 {
		t.Error("ShareNetwork=true 时该带 --share-net")
	}
}

// TestBwrap_找不到就拒绝启动 没有 bwrap 时不能静默退化成不隔离的直接执行——
// 那正是最坏的失败形态：看起来配好了，其实没有沙盒。
func TestBwrap_找不到就拒绝启动(t *testing.T) {
	_, err := NewSandbox(Options{
		Root: t.TempDir(), Mode: ModeBwrap, Bwrap: "/nonexistent/bwrap-xyz",
	})
	if err == nil {
		t.Fatal("指定了不存在的 bwrap 路径，该拒绝启动")
	}
	if !strings.Contains(err.Error(), "bubblewrap") {
		t.Errorf("错误信息该点名 bubblewrap：%v", err)
	}
}

// TestBwrap_写边界是真的 真起沙盒：沙盒内可写、沙盒外写不进去、cwd 落在沙盒根。
func TestBwrap_写边界是真的(t *testing.T) {
	bwrapUsable(t)
	s, err := NewSandbox(Options{Root: t.TempDir(), Mode: ModeBwrap})
	if err != nil {
		t.Fatalf("建沙盒失败：%v", err)
	}

	// cwd 落在沙盒根
	result, err := s.Run(context.Background(), RunRequest{Command: "pwd -P"})
	if err != nil {
		t.Fatalf("Run 返错：%v", err)
	}
	if got := strings.TrimSpace(result.Stdout); got != real(t, s.Root) {
		t.Errorf("cwd 该是 %s，实际 %s（stderr：%s）", real(t, s.Root), got, result.Stderr)
	}

	// 沙盒内可写，且文件真的落在宿主上
	inside, err := s.Run(context.Background(), RunRequest{Command: "printf hi > made.txt && printf ok"})
	if err != nil {
		t.Fatalf("Run 返错：%v", err)
	}
	if inside.ExitCode != 0 {
		t.Fatalf("沙盒内写文件该成功，退出码 %d：%s", inside.ExitCode, inside.Stderr)
	}
	if _, err := os.Stat(filepath.Join(s.Root, "made.txt")); err != nil {
		t.Errorf("沙盒内写的文件该真的落在宿主沙盒根：%v", err)
	}

	// 沙盒外（/etc）写不进去——只读根绑定
	outside, err := s.Run(context.Background(), RunRequest{Command: "printf x > /etc/leak-fka-bash"})
	if err != nil {
		t.Fatalf("Run 返错：%v", err)
	}
	if outside.ExitCode == 0 {
		t.Error("往 /etc 写该失败（只读根绑定）")
	}
}

// TestBwrap_读不到宿主与兄弟目录 最小根（不再 `--ro-bind / /`）：宿主其余部分
// 在沙盒里**根本不存在**——安装根里的 .env/data/logs、别的租户目录都读不到。
//
// 这是多租户下最要紧的一条：以前整盘只读暴露，一个被注入的会话能 `cat <安装根>/.env`
// 拿走 LLM key 与微信 token。
func TestBwrap_读不到宿主与兄弟目录(t *testing.T) {
	bwrapUsable(t)
	base := t.TempDir()
	if err := os.MkdirAll(filepath.Join(base, "other"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(base, "other", "secret.txt"), []byte("SIBLING-CONTENT"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(base, ".env"), []byte("SECRETKEY"), 0o600); err != nil {
		t.Fatal(err)
	}
	self := filepath.Join(base, "self")
	if err := os.MkdirAll(self, 0o755); err != nil {
		t.Fatal(err)
	}

	s, err := NewSandbox(Options{Root: self, Mode: ModeBwrap})
	if err != nil {
		t.Fatalf("建沙盒失败：%v", err)
	}

	sibling := filepath.Join(base, "other", "secret.txt")
	res, err := s.Run(context.Background(), RunRequest{
		Command: "cat '" + sibling + "' 2>/dev/null || echo DENIED",
	})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(res.Stdout, "SIBLING-CONTENT") {
		t.Errorf("租户读到了兄弟目录的文件：%q", res.Stdout)
	}

	env := filepath.Join(base, ".env")
	res2, err := s.Run(context.Background(), RunRequest{
		Command: "cat '" + env + "' 2>/dev/null || echo NO-ENV",
	})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(res2.Stdout, "SECRETKEY") {
		t.Errorf("租户读到了安装根的 .env：%q", res2.Stdout)
	}
}

// TestBwrap_额外只读绑定可见 自装工具链（node/python…）不在内置白名单里；
// 用 ReadOnlyExtra 点名挂进来后，沙盒里读得到；不挂则读不到。
func TestBwrap_额外只读绑定可见(t *testing.T) {
	bwrapUsable(t)
	tooling := t.TempDir() // 模拟一份自装工具链目录
	if err := os.WriteFile(filepath.Join(tooling, "hello.txt"), []byte("TOOL"), 0o644); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(tooling, "hello.txt")

	self := t.TempDir()
	read := func(s *Sandbox) string {
		res, err := s.Run(context.Background(), RunRequest{Command: "cat '" + target + "' 2>/dev/null || echo DENIED"})
		if err != nil {
			t.Fatal(err)
		}
		return res.Stdout
	}

	plain, err := NewSandbox(Options{Root: self, Mode: ModeBwrap})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(read(plain), "TOOL") {
		t.Error("没挂的目录不该可见")
	}

	withTool, err := NewSandbox(Options{Root: self, Mode: ModeBwrap, ReadOnlyExtra: []string{tooling}})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(read(withTool), "TOOL") {
		t.Error("挂了只读绑定后该读得到")
	}
}
