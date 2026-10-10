package main

import (
	"fmt"
	"os"
	"strconv"
	"strings"

	"github.com/zjzhang-cn/fka-go/internal/config"
)

// logLevelFlag 控制台日志级别的参数名。
const logLevelFlag = "--log-level"

// logLevelEnv 控制台日志级别的环境变量名。**参数没给时用它**。
//
// 环境变量留着是有理由的：部署里往往不控制命令行（systemd 的 Environment、
// 容器编排），而「这次要开到 debug 看一下」又恰恰是临时行为——
// 两种场景各自需要一个入口。
const logLevelEnv = "LOG_LEVEL"

// defaultConsoleLevel 大部分子命令不给 --log-level / LOG_LEVEL 时的控制台级别。
//
// **值只有一处出处**：`config.DefaultConsoleLevel`（那边是 Warn）。
// 这里刻意不再写字面量——两处各写一份时，改一处只会影响一半路径，
// 而漏掉的那半条路正好是 `fka tools --json`：日志插进 JSON 前面，
// 退出码却仍是 0，调用方只看到「解析失败」，看不出是日志干的。
const defaultConsoleLevel = config.DefaultConsoleLevel

// serveConsoleLevel `serve` 子命令的控制台默认级别。
//
// serve 是**常驻进程**，前台就是要看「它起来没有、在收什么」——压到 Warn 会让
// 「网页渠道已启动 / 收到消息 / 已作答」这些唯一能证明它在干活的行全都不见，
// 而部署的人只会以为它没起来。其余子命令的 stdout 是给人/脚本消费的结果，
// 保持 Warn 少噪音。两套默认各管各的，靠 `consoleLevelFor` 分派。
const serveConsoleLevel = config.LevelInfo

// consoleLevelFor 按子命令取控制台默认级别。
//
// 只有 `serve` 不同：它是长驻服务，INFO 是它的正常前台噪音下限；其余命令跑完就退，
// 控制台保持安静。**`--log-level` / `LOG_LEVEL` 仍然压过这里的任何一个。**
func consoleLevelFor(command string) config.Level {
	if command == "serve" {
		return serveConsoleLevel
	}
	return defaultConsoleLevel
}

// logLevels 合法取值。**印在用法错误里**——
// 让人去翻文档确认「verbose 行不行」比直接列出来更烦。
var logLevels = []string{"debug", "info", "warn", "error", "critical"}

// errLogLevel 参数认错了。它**必须是 exitUsage（2）而不是 exitFail（1）**：
// 1 是「预期内的失败」，2 是「你敲错了命令」——级别字符串敲错显然是后者。
type errLogLevel struct{ value string }

func (e *errLogLevel) Error() string {
	return "日志级别认不出：" + strconv.Quote(e.value) +
		"。可用的是 " + strings.Join(logLevels, " / ")
}

// parseLogLevel 认一个日志级别。**认不出就返错**，绝不静默退回默认。
func parseLogLevel(value string) (config.Level, error) {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "debug":
		return config.LevelDebug, nil
	case "info":
		return config.LevelInfo, nil
	case "warn", "warning":
		return config.LevelWarn, nil
	case "error":
		return config.LevelError, nil
	case "critical":
		return config.LevelCritical, nil
	}
	return 0, &errLogLevel{value: value}
}

// resolveLogLevel 认下这次要用的控制台级别。**只判、不落地**。
//
// ## 为什么「判」与「落地」必须分开
//
// 这两件事必须发生在**不同的时间点**：
//
//   - 参数校验要早于纯输出子命令（version/help）返回——`fka version --log-level verbose`
//     该按用法错以 2 退出，不能因为那个命令自己不记日志就跳过校验；
//   - 而级别落地要**晚于**它们返回——`config.Log()` 是惰性构造，第一次调用就会
//     建 `logs/` 目录。在别人机器上跑 `fka version` 却多一个目录，那是副作用不是功能。
//
// 以前它俩挤在一个 `applyLogLevel` 里，于是校验被提到了前面、落地跟着一起提前，
// 而主流程里那句「设默认值」仍在原地——**先落地成 debug、紧接着被默认值压回去**。
// 症状是 `--log-level debug` 与 `LOG_LEVEL=debug` 双双失效，而解析、校验、退出码
// 全是对的：用例只测那个函数，覆盖它的却是 main 里的下一句，于是全绿。
func resolveLogLevel(parsed cliArgs) (config.Level, bool, error) {
	// 参数优先于环境变量：显式选择压倒一切，与 `.env` 里的显式设置优先于
	// 默认值是同一条道理
	value := flagOrEnv(parsed, logLevelFlag, logLevelEnv, "")
	if value == "" {
		return 0, false, nil
	}

	level, err := parseLogLevel(value)
	if err != nil {
		return 0, false, err
	}
	return level, true, nil
}

// useConsoleLevel 落地控制台级别：**先默认，再让参数/环境变量覆盖**。
//
// ## 顺序就是这条链路的全部内容
//
// 写反的后果不是报错，而是「参数静默失效」：控制台一行不多，而日志文件里什么都有
// （文件那边**始终全量**，见 `Logger.write` 的既有约定），于是看起来像「这条链路
// 没问题，只是没日志可看」。凡是要定级别的地方，都该照这个顺序写。
func useConsoleLevel(command string, level config.Level, given bool) {
	config.Log().SetConsoleLevel(consoleLevelFor(command))
	if given {
		config.Log().SetConsoleLevel(level)
	}
}

// printLogLevelUsage 在用法里说明这个参数。
func printLogLevelUsage(out *os.File) {
	fmt.Fprintf(out, "  %s <%s>   控制台日志级别（不给则 %s；serve 默认 %s，其余默认 %s）\n",
		logLevelFlag, strings.Join(logLevels, "|"), logLevelEnv, serveConsoleLevel, defaultConsoleLevel)
}
