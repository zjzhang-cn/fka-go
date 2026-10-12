// 沙盒的行为用例。
//
// ## 这些用例守的是什么
//
// 沙盒的三条硬边界（工作目录、超时/输出、命令策略）全是**静默失效**型的：
// cwd 越界不会报错，只是命令跑到了别处；超时不生效只是命令挂久一点；策略没命中
// 只是某个危险命令被放行。界面上都看不出来，所以只能在这里钉。
package main

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

// newTestSandbox 在一个临时目录上建沙盒。
//
// **用 direct 模式**：这一组用例钉的是 cwd / 超时 / 输出 / 策略，与用不用 bwrap
// 无关；走 direct 才能在没有 Bubblewrap 的机器上也跑。真实的 bwrap 隔离另有一组
// 用例（sandbox_bwrap_test.go），那条在缺 bwrap 时跳过。
func newTestSandbox(t *testing.T) *Sandbox {
	t.Helper()
	s, err := NewSandbox(Options{Root: t.TempDir(), Mode: ModeDirect})
	if err != nil {
		t.Fatalf("建沙盒失败：%v", err)
	}
	return s
}

// requireBash 沙盒执行依赖 bash，Windows 上跳过（本 server 主要面向 Unix）。
func requireBash(t *testing.T) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("Windows 上不保证有 bash")
	}
}

// real 取符号链接解析后的路径，用于和 `pwd -P` 对齐比较。
func real(t *testing.T, path string) string {
	t.Helper()
	resolved, err := filepath.EvalSymlinks(path)
	if err != nil {
		t.Fatalf("解析 %s 失败：%v", path, err)
	}
	return resolved
}

func runOK(t *testing.T, s *Sandbox, req RunRequest) Result {
	t.Helper()
	result, err := s.Run(context.Background(), req)
	if err != nil {
		t.Fatalf("Run 返错：%v", err)
	}
	return result
}

// TestSandbox_工作目录固定在沙盒内 空 cwd 落在沙盒根，给了子目录就落在子目录，
// 两者都必须是 `pwd -P` 能看到的事实，而不是我们以为的事实。
func TestSandbox_工作目录固定在沙盒内(t *testing.T) {
	requireBash(t)
	s := newTestSandbox(t)

	root := runOK(t, s, RunRequest{Command: "pwd -P"})
	if got := strings.TrimSpace(root.Stdout); got != real(t, s.Root) {
		t.Errorf("空 cwd 该落在沙盒根 %s，实际 %s", real(t, s.Root), got)
	}

	sub := runOK(t, s, RunRequest{Command: "pwd -P", Cwd: "a/b"})
	want := real(t, filepath.Join(s.Root, "a", "b"))
	if got := strings.TrimSpace(sub.Stdout); got != want {
		t.Errorf("cwd=a/b 该落在 %s，实际 %s", want, got)
	}
}

// TestSandbox_HOME落进沙盒 HOME/TMPDIR 被指到沙盒里，`~` 展开不会碰到宿主目录。
func TestSandbox_HOME落进沙盒(t *testing.T) {
	requireBash(t)
	s := newTestSandbox(t)

	result := runOK(t, s, RunRequest{Command: `printf '%s' "$HOME"`})
	if got := strings.TrimSpace(result.Stdout); got != s.Root {
		t.Errorf("HOME 该是 %s，实际 %s", s.Root, got)
	}
}

// TestSandbox_cwd越界被拒 词法越界（..、绝对路径）与符号链接越界三条都要拒。
func TestSandbox_cwd越界被拒(t *testing.T) {
	requireBash(t)
	s := newTestSandbox(t)

	if _, err := s.ResolveDir("../escape"); err == nil {
		t.Error("cwd=../escape 该被拒")
	}
	if _, err := s.ResolveDir("/etc"); err == nil {
		t.Error("cwd 是绝对路径该被拒")
	}

	// 沙盒里放一个指向外面的软链
	outside := t.TempDir()
	link := filepath.Join(s.Root, "link")
	if err := os.Symlink(outside, link); err != nil {
		t.Fatalf("建软链失败：%v", err)
	}
	if _, err := s.ResolveDir("link"); err == nil {
		t.Error("cwd 经符号链接指向沙盒外该被拒")
	}
}

// TestSandbox_超时被杀 硬超时必须真的把命令收掉，而不是把控制权交回来、命令还在跑。
func TestSandbox_超时被杀(t *testing.T) {
	requireBash(t)
	s := newTestSandbox(t)

	started := time.Now()
	result := runOK(t, s, RunRequest{Command: "sleep 5", Timeout: 200 * time.Millisecond})
	if !result.TimedOut {
		t.Fatalf("sleep 5 加上 200ms 超时该报超时，实际 TimedOut=%v", result.TimedOut)
	}
	if elapsed := time.Since(started); elapsed > 3*time.Second {
		t.Errorf("超时后该很快返回，实际等了 %s", elapsed)
	}
}

// TestSandbox_输出被截断 超过上限的输出要截断并标记，而且**不能**让命令因为写满
// 管道而死——capWriter 永远返回 len(p)，继续把管道读干。
func TestSandbox_输出被截断(t *testing.T) {
	requireBash(t)
	s := newTestSandbox(t)

	result := runOK(t, s, RunRequest{Command: `printf 'a%.0s' {1..100000}`})
	if !result.Truncated {
		t.Error("10 万字节输出该被截断（上限 32KiB）")
	}
	if len(result.Stdout) > DefaultStreamCap {
		t.Errorf("截断后不该还留着 %d 字节", len(result.Stdout))
	}
	if result.ExitCode != 0 {
		t.Errorf("输出太多不该让命令失败，退出码 %d", result.ExitCode)
	}
}

// TestSandbox_退出码如实带回 命令跑完但退出码非 0 是**正常结果**，不是 Run 的 error。
func TestSandbox_退出码如实带回(t *testing.T) {
	requireBash(t)
	s := newTestSandbox(t)

	result := runOK(t, s, RunRequest{Command: "exit 3"})
	if result.ExitCode != 3 {
		t.Errorf("退出码该是 3，实际 %d", result.ExitCode)
	}
}

// TestSandbox_stderr分流 stdout 与 stderr 分开带回来。
func TestSandbox_stderr分流(t *testing.T) {
	requireBash(t)
	s := newTestSandbox(t)

	result := runOK(t, s, RunRequest{Command: `printf out; printf err 1>&2`})
	if strings.TrimSpace(result.Stdout) != "out" {
		t.Errorf("stdout 该是 out，实际 %q", result.Stdout)
	}
	if strings.TrimSpace(result.Stderr) != "err" {
		t.Errorf("stderr 该是 err，实际 %q", result.Stderr)
	}
}

// TestSandbox_内置黑名单拦危险命令 内置那份不能被空配置关掉。
func TestSandbox_内置黑名单拦危险命令(t *testing.T) {
	s := newTestSandbox(t)

	for _, command := range []string{"shutdown -h now", "mount /dev/sda1 /mnt", "sudo ls"} {
		if err := s.Check(command); err == nil {
			t.Errorf("%q 该被内置黑名单拦下", command)
		}
	}
}

// TestSandbox_前缀命令也被检查 `env shutdown`、`timeout 5 shutdown` 不能因为首词是
// 前缀命令就溜过去——前缀命令的名字与它包裹的命令都要过策略。
func TestSandbox_前缀命令也被检查(t *testing.T) {
	s := newTestSandbox(t)

	for _, command := range []string{"env shutdown", "timeout 5 shutdown", "env A=1 sudo ls"} {
		if err := s.Check(command); err == nil {
			t.Errorf("%q 包裹的危险命令该被拦下", command)
		}
	}
}

// TestSandbox_白名单外的命令被拒 白名单非空时进入白名单模式。
func TestSandbox_白名单外的命令被拒(t *testing.T) {
	s, err := NewSandbox(Options{Root: t.TempDir(), Mode: ModeDirect, Allow: []string{"echo", "cat"}})
	if err != nil {
		t.Fatalf("建沙盒失败：%v", err)
	}

	if err := s.Check("echo hi"); err != nil {
		t.Errorf("echo 在白名单里，不该被拒：%v", err)
	}
	if err := s.Check("ls"); err == nil {
		t.Error("ls 不在白名单里，该被拒")
	}
}

// TestSandbox_管道每段都过白名单 `echo x | cat` 里的 cat 也得在白名单里，
// 否则「只有第一个词受管」会让管道成为绕过口。
func TestSandbox_管道每段都过白名单(t *testing.T) {
	s, err := NewSandbox(Options{Root: t.TempDir(), Mode: ModeDirect, Allow: []string{"echo"}})
	if err != nil {
		t.Fatalf("建沙盒失败：%v", err)
	}

	if err := s.Check("echo x | cat"); err == nil {
		t.Error("管道第二段的 cat 不在白名单里，整条该被拒")
	}
}

// TestSandbox_空命令被拒 空命令没有可执行的程序，直接拒。
func TestSandbox_空命令被拒(t *testing.T) {
	s := newTestSandbox(t)

	if err := s.Check("   "); err == nil {
		t.Error("空命令该被拒")
	}
}

// TestSandbox_根不能是文件系统根 空根（会退化成 cwd）与 `/`（可写整台机器）都拒。
func TestSandbox_根不能是文件系统根(t *testing.T) {
	if _, err := NewSandbox(Options{Root: "  ", Mode: ModeDirect}); err == nil {
		t.Error("空沙盒根该被拒")
	}
	if _, err := NewSandbox(Options{Root: "/", Mode: ModeDirect}); err == nil {
		t.Error("沙盒根是 / 该被拒")
	}
}

// Test命令解析_抽出的程序名 直接钉 commandPrograms 的抽取口径。
func Test命令解析_抽出的程序名(t *testing.T) {
	cases := []struct {
		command string
		want    []string
	}{
		{"ls -la", []string{"ls"}},
		{"/bin/ls -la", []string{"ls"}},
		{"A=1 ls", []string{"ls"}},
		{"echo a | grep b && tail -1", []string{"echo", "grep", "tail"}},
		{"env FOO=1 rm -rf x", []string{"env", "rm"}},
		{"timeout 5 sleep 10", []string{"timeout", "sleep"}},
		{"echo $(id) `whoami`", []string{"echo", "id", "whoami"}},
	}

	for _, c := range cases {
		got := commandPrograms(c.command)
		if strings.Join(got, ",") != strings.Join(c.want, ",") {
			t.Errorf("commandPrograms(%q) = %v，期望 %v", c.command, got, c.want)
		}
	}
}
