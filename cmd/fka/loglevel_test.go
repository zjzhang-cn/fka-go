package main

import (
	"os"
	"path/filepath"
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

// useTempConsoleLevel 让用例独占控制台级别，结束时**恢复成默认值**。
//
// 恢复成默认值而不是「进入时的值」：`ConsoleLevel()` 会把环境变量也算进去，
// 于是「进入时的值」可能来自上一个用例的临时环境——把它写回 override 等于把污染
// 传给下一个用例（这里真发生过：一条用例把 error 留在了 override 上）。
func useTempConsoleLevel(t *testing.T) {
	t.Helper()
	t.Cleanup(func() { config.Log().SetConsoleLevel(config.DefaultConsoleLevel) })
}

// runLogLevelWiring 走一遍 main 里那段顺序：解析 → （纯输出子命令早退）→ 落地。
//
// **用例必须验「真正生效的结果」**：只测 `resolveLogLevel` 的话，上一版那个 bug
// （落地被主流程的默认值压回去）永远测不出来——解析全对，级别却没用上。
func runLogLevelWiring(t *testing.T, parsed cliArgs) error {
	t.Helper()

	level, given, err := resolveLogLevel(parsed)
	if err != nil {
		return err
	}
	useConsoleLevel(level, given)
	return nil
}

// Test参数压过环境变量 显式选择压倒一切——和 .env 里显式设置优先于默认同一条道理
func Test参数压过环境变量(t *testing.T) {
	t.Setenv("LOG_LEVEL", "error")
	useTempConsoleLevel(t)

	if err := runLogLevelWiring(t, mustParse(t, "serve", "--log-level", "debug")); err != nil {
		t.Fatalf("不该报错：%v", err)
	}
	if got := config.Log().ConsoleLevel(); got != config.LevelDebug {
		t.Errorf("参数该压过环境变量，实际 %v", got)
	}
}

// Test没给参数时落在默认值 **不给参数、环境变量也没有 = 默认值**（Warn）。
//
// 这条以前写的是「不该动级别」——那是按函数级视角写的，而 `main` 的契约从来是
// 「落地一个默认值，再让参数覆盖它」。按旧写法，用例会依赖「上一个用例把 override
// 留成什么」，读起来像在验契约，其实在验残留状态。
func Test没给参数时落在默认值(t *testing.T) {
	t.Setenv("LOG_LEVEL", "")
	useTempConsoleLevel(t)

	// 先故意设成别的值：不这样的话「落地默认值」这一步看不出来
	config.Log().SetConsoleLevel(config.LevelCritical)

	if err := runLogLevelWiring(t, mustParse(t, "serve")); err != nil {
		t.Fatalf("不该报错：%v", err)
	}
	if got := config.Log().ConsoleLevel(); got != config.DefaultConsoleLevel {
		t.Errorf("控制台级别 = %v，期望默认值 %v", got, config.DefaultConsoleLevel)
	}
}

// Test只给环境变量也行 部署里常常不控制命令行（systemd / 容器编排）
func Test只给环境变量也行(t *testing.T) {
	t.Setenv("LOG_LEVEL", "info")
	useTempConsoleLevel(t)

	if err := runLogLevelWiring(t, mustParse(t, "serve")); err != nil {
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
	useTempConsoleLevel(t)
	t.Setenv("LOG_LEVEL", "")

	if err := runLogLevelWiring(t, mustParse(t, "--log-level=debug", "serve")); err != nil {
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

// Test默认控制台级别不污染stdout 这条钉的是**默认值**，不是某次显式指定。
//
// CLI 的 stdout 是给人和脚本消费的结果输出：`fka tools --json` 的第一行必须是
// `{`。默认值一旦退回 info/debug，日志就会插到 JSON 前面，而**退出码仍是 0**——
// 调用方只看到「解析失败」，看不出是日志干的（`make smoke` 里也有一条同样的断言）。
//
// 值只有一处出处（`config.DefaultConsoleLevel`），所以这里比的是那个常量本身。
func Test默认控制台级别不污染stdout(t *testing.T) {
	if defaultConsoleLevel < config.LevelWarn {
		t.Errorf("默认控制台级别是 %v：info/debug 会被打进 stdout，"+
			"`fka tools --json` 就不再是合法 JSON 了", defaultConsoleLevel)
	}
	if defaultConsoleLevel != config.DefaultConsoleLevel {
		t.Errorf("CLI 的默认级别 %v 与 config.DefaultConsoleLevel %v 不一致——"+
			"默认值只该有一处出处", defaultConsoleLevel, config.DefaultConsoleLevel)
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

// Test落地顺序_默认在前覆盖在后 这条钉的是**顺序**，不是解析。
//
// 上一版把「解析」提前到了 version/help 早退之前，落地跟着一起提前，而主流程里那句
// 「设默认值」仍在原地——于是先落地成 debug、紧接着被默认值压回 Warn。所有函数级
// 用例全绿，而 `fka serve --log-level debug` 与 `LOG_LEVEL=debug` 双双失效：
// **控制台 0 行 INFO，日志文件里 19 行**（文件始终全量），看起来像「本来就没什么可看的」。
func Test落地顺序_默认在前覆盖在后(t *testing.T) {
	original := config.Log().ConsoleLevel()
	t.Cleanup(func() { config.Log().SetConsoleLevel(original) })

	cases := []struct {
		name string
		env  string
		args []string
		want config.Level
	}{
		{"参数给 debug", "error", []string{"serve", "--log-level", "debug"}, config.LevelDebug},
		{"环境变量给 info", "info", []string{"serve"}, config.LevelInfo},
		{"两者都没给 → 默认", "", []string{"serve"}, config.DefaultConsoleLevel},
		{"参数压过环境变量", "error", []string{"--log-level=debug", "serve"}, config.LevelDebug},
		{"参数写在子命令后面", "", []string{"serve", "--log-level", "debug"}, config.LevelDebug},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Setenv("LOG_LEVEL", c.env)
			// 先故意设成别的值：不这样的话「默认在前」那一步看不出来
			config.Log().SetConsoleLevel(config.LevelCritical)

			if err := runLogLevelWiring(t, mustParse(t, c.args...)); err != nil {
				t.Fatalf("不该报错：%v", err)
			}
			if got := config.Log().ConsoleLevel(); got != c.want {
				t.Errorf("控制台级别 = %v，期望 %v", got, c.want)
			}
		})
	}
}

// Test纯输出子命令不为日志建目录 校验在前、落地在后，所以 `version` 那条路
// 一次都不该碰 `config.Log()`（惰性构造会建 logs/）。
func Test纯输出子命令不为日志建目录(t *testing.T) {
	home := t.TempDir()
	t.Setenv("FKA_HOME", home)
	t.Setenv("LOG_LEVEL", "")

	// 走 main 里 version 那条路：解析 → 早退（不落地）
	level, given, err := resolveLogLevel(mustParse(t, "version", "--log-level", "debug"))
	if err != nil {
		t.Fatalf("合法级别不该报错：%v", err)
	}
	if !given || level != config.LevelDebug {
		t.Fatalf("该认下 debug：given=%v level=%v", given, level)
	}
	if _, err := os.Stat(filepath.Join(home, "logs")); !os.IsNotExist(err) {
		t.Errorf("version 不该建 logs/（err=%v）", err)
	}
}
