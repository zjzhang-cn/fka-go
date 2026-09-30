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

// defaultConsoleLevel 不给 --log-level / LOG_LEVEL 时的控制台级别。
//
// **值只有一处出处**：`config.DefaultConsoleLevel`（那边是 Warn）。
// 这里刻意不再写字面量——两处各写一份时，改一处只会影响一半路径，
// 而漏掉的那半条路正好是 `fka tools --json`：日志插进 JSON 前面，
// 退出码却仍是 0，调用方只看到「解析失败」，看不出是日志干的。
const defaultConsoleLevel = config.DefaultConsoleLevel

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

// applyLogLevel 定下**控制台**的日志级别。
//
// ## 为什么只在控制台，不动文件
//
// 文件那边**始终全量**（`Logger.write` 的既有约定），是排查的底。
// 控制台才是「这次想看多少」——`fka tools` 那种把 stdout 留给人的命令，
// 把内部日志混进去就是污染；而 `fka serve --log-level debug` 恰恰相反，
// 用户明确要看见全部。
//
// ## 为什么必须在分派子命令**之前**调
//
// `main` 一进来就 `SetConsoleLevel(LevelWarn)`，而首个被处理的入站消息可能
// 在任何子命令的代码跑起来之前就记日志。所以这一调用必须比 switch 更早。
//
// 返回 errLogLevel 时**别自己打印也别退出**——调用方在 `run` 里统一处理退出码，
// 这里只管判。
func applyLogLevel(parsed cliArgs) error {
	// 参数优先于环境变量：显式选择压倒一切，与 `.env` 里的显式设置优先于
	// 默认值是同一条道理
	value := flagOrEnv(parsed, logLevelFlag, logLevelEnv, "")
	if value == "" {
		return nil
	}

	level, err := parseLogLevel(value)
	if err != nil {
		return err
	}
	config.Log().SetConsoleLevel(level)
	return nil
}

// printLogLevelUsage 在用法里说明这个参数。
func printLogLevelUsage(out *os.File) {
	fmt.Fprintf(out, "  %s <%s>   控制台日志级别（不给则 %s，不给参数时默认 %s）\n",
		logLevelFlag, strings.Join(logLevels, "|"), logLevelEnv, defaultConsoleLevel)
}
