package main

import (
	"os"
	"strings"
	"testing"

	"github.com/zjzhang-cn/fka-go/internal/config"
)

// Test级别认得出 五个取值都要认，大小写与首尾空格都放过
func Test级别认得出(t *testing.T) {
	cases := map[string]config.Level{
		"debug": config.LevelDebug, "DEBUG": config.LevelDebug, " debug ": config.LevelDebug,
		"info": config.LevelInfo,
		// warning 也认：它是 warn 最常见的误拼，拒掉它只会让人去猜
		"warn": config.LevelWarn, "warning": config.LevelWarn,
		"error": config.LevelError, "critical": config.LevelCritical,
	}
	for input, want := range cases {
		got, err := parseLogLevel(input)
		if err != nil {
			t.Errorf("%q 该认得出：%v", input, err)
			continue
		}
		if got != want {
			t.Errorf("%q 该是 %v，实际 %v", input, want, got)
		}
	}
}

// Test级别认不出就报错 **绝不静默退回默认**——
// 静默的话用户以为自己开到 debug 了，实际什么都没变，而「日志不出现」
// 会被当成「程序没记日志」，查错方向完全跑偏。
func Test级别认不出就报错(t *testing.T) {
	for _, input := range []string{"verbose", "trace", "", "5", "warn warning", "调试"} {
		if _, err := parseLogLevel(input); err == nil {
			t.Errorf("%q 该被拒", input)
		}
	}
}

// Test认不出的错误里列得出合法取值 让人不必去翻文档就能改对
func Test认不出的错误里列得出合法取值(t *testing.T) {
	_, err := parseLogLevel("verbose")
	if err == nil {
		t.Fatal("该报错")
	}
	message := err.Error()
	for _, want := range []string{"verbose", "debug", "info", "warn", "error", "critical"} {
		if !contains(message, want) {
			t.Errorf("错误信息该列出 %q，实际：%s", want, message)
		}
	}
}

// Test级别错误的类型就是用法错 **退出码 2 是仓库的契约**（「用法错」），
// 而 errLogLevel 那个具名类型就是 run 里选择退出码的依据
func Test级别错误的类型就是用法错(t *testing.T) {
	_, err := parseLogLevel("verbose")

	typed, ok := err.(*errLogLevel)
	if !ok {
		t.Fatalf("该是 *errLogLevel，实际 %T", err)
	}
	if typed.value != "verbose" {
		t.Errorf("该把原值带在身上（报错要说清收到的是什么），实际 %q", typed.value)
	}
}

// Test参数压过环境变量 显式选择压倒一切——和 .env 里显式设置优先于默认同一条道理
func Test参数压过环境变量(t *testing.T) {
	t.Setenv("LOG_LEVEL", "error")
	original := config.Log().ConsoleLevel()
	t.Cleanup(func() { config.Log().SetConsoleLevel(original) })

	if err := applyLogLevel(mustParse(t, "serve", "--log-level", "debug")); err != nil {
		t.Fatalf("不该报错：%v", err)
	}
	if got := config.Log().ConsoleLevel(); got != config.LevelDebug {
		t.Errorf("参数该压过环境变量，实际 %v", got)
	}
}

// Test没给参数时不动默认值 **不给就该一字未变**——
// 上一条说「先设默认再让参数覆盖」，这条钉的就是「覆盖不了时别动」
func Test没给参数时不动默认值(t *testing.T) {
	t.Setenv("LOG_LEVEL", "")
	original := config.Log().ConsoleLevel()
	t.Cleanup(func() { config.Log().SetConsoleLevel(original) })

	if err := applyLogLevel(mustParse(t, "serve")); err != nil {
		t.Fatalf("不该报错：%v", err)
	}
	if got := config.Log().ConsoleLevel(); got != original {
		t.Errorf("没给参数不该动级别：%v → %v", original, got)
	}
}

// Test只给环境变量也行 部署里常常不控制命令行（systemd / 容器编排）
func Test只给环境变量也行(t *testing.T) {
	t.Setenv("LOG_LEVEL", "info")
	original := config.Log().ConsoleLevel()
	t.Cleanup(func() { config.Log().SetConsoleLevel(original) })

	if err := applyLogLevel(mustParse(t, "serve")); err != nil {
		t.Fatalf("不该报错：%v", err)
	}
	if got := config.Log().ConsoleLevel(); got != config.LevelInfo {
		t.Errorf("该用环境变量里的值，实际 %v", got)
	}
}

// Test参数写在哪都认 `fka serve --log-level debug` 与 `fka --log-level debug serve`
// 都该被认到——后者虽然分派不了（args[0] 不是子命令名），但级别是**全局**的，
// 分派失败也要先按它调好控制台
func Test参数写在哪都认(t *testing.T) {
	original := config.Log().ConsoleLevel()
	t.Cleanup(func() { config.Log().SetConsoleLevel(original) })
	t.Setenv("LOG_LEVEL", "")

	if err := applyLogLevel(mustParse(t, "--log-level=debug", "serve")); err != nil {
		t.Fatalf("等号写法该认：%v", err)
	}
	if got := config.Log().ConsoleLevel(); got != config.LevelDebug {
		t.Errorf("实际 %v", got)
	}
}

// Test认错了会一路传到 run 也就是**真的以 2 退出**，而不只是内部返错
func Test认错了会一路传到run(t *testing.T) {
	original := os.Getenv("LOG_LEVEL")
	t.Cleanup(func() { _ = os.Setenv("LOG_LEVEL", original) })
	t.Setenv("LOG_LEVEL", "")

	// **下面这一句是 `run(["serve"])` 不去连真 iLink 的全部理由**：serve 认得出
	// 账号就会起真长轮询，然后卡在网络请求上直到测试超时。临时安装根 + 置空账号
	// 键让它每次都落在「没接上渠道 → 以 1 退出」那条路上，与前面跑了谁无关
	setupInstallRoot(t)
	blankAccountEnv(t)

	if code := run([]string{"serve", "--log-level", "verbose"}); code != exitUsage {
		t.Errorf("级别认错该以 %d（用法错）退出，实际 %d", exitUsage, code)
	}
	// 认对了就该照常往下走——没账号时 serve 以 1 退出（那是另一回事）
	if code := run([]string{"serve", "--log-level", "warn"}); code != exitFail {
		t.Errorf("级别认对了就该照常跑，实际 %d", code)
	}
}

// mustParse 走真实的参数解析。**测试不自己造 cliArgs**——
// 那样测的是「我以为解析完长什么样」，而参数解析恰恰是这一组用例要守的东西。
func mustParse(t *testing.T, args ...string) cliArgs {
	t.Helper()

	parsed, err := parseFlags(args)
	if err != nil {
		t.Fatalf("参数该认得出：%v", err)
	}
	return parsed
}

// contains 就是 strings.Contains。**这里自己写一份**是为了让这个文件
// 看起来只关心参数解析那一件事
func contains(haystack string, needle string) bool {
	return strings.Contains(haystack, needle)
}
