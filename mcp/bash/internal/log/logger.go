// Package log 是 **stdio 子进程**用的极简 logger。
//
// ## 为什么自己有一份而不共用 agent 或 memory 的
//
// `mcp/bash` 自给自足：`boundary_test.go` 扫 import，禁止它引用本仓库树外的任何包
// （`mcp/memory/internal/log` 也在禁止之列）。共用一份要么让两个 server 互相依赖，
// 要么引入第三个「工具库仓库」——都不值得为 120 行代码。**该复制的就复制**，
// logger 是最容易独立的一块。代价是这份与 `mcp/memory/internal/log` 的同名文件
// 是**刻意副本**，改名/改格式时要一起改。
//
// ## 输出到哪
//
// **stderr + 按天轮转的文件**。stdout 一个字都不能有——那是 JSON-RPC 的通道，
// 一个 fmt.Println 就能把 server 打挂（见 main.go）。
//
// ## 落盘格式：普通文本，不是 JSON
//
// 一行一条，`[级别] 消息 键=值 键=值`，文件行多一个 UTC 时间戳。
// **换行会被转义、带空格的值会被加引号**，守住「一条日志 = 一行」。
package log

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Level 日志级别。数字越大越严重。
type Level int

const (
	LevelDebug Level = 10
	LevelInfo  Level = 20
	LevelWarn  Level = 30
	LevelError Level = 40
)

var levelNames = map[Level]string{
	LevelDebug: "debug",
	LevelInfo:  "info",
	LevelWarn:  "warn",
	LevelError: "error",
}

func (l Level) String() string { return levelNames[l] }

// parseLevel 从环境变量取值。认不出的返回 false——由调用方决定退回什么。
func parseLevel(value string) (Level, bool) {
	switch strings.ToLower(value) {
	case "debug":
		return LevelDebug, true
	case "info":
		return LevelInfo, true
	case "warn":
		return LevelWarn, true
	case "error":
		return LevelError, true
	}
	return 0, false
}

// Context 是日志的附加字段。**值不加工**——排查时看到什么就是调用方给的那份。
// 唯一的例外是**排版**：换行转义、带空格的值加引号（见 `oneLine`）。
type Context map[string]any

// Logger 进程级日志：控制台按级别过滤，**文件始终全量**——日志文件是排查用的，
// 不该因为控制台调静音而丢信息。
type Logger struct {
	mu       sync.Mutex
	dir      string
	file     *os.File
	fileName string
}

// logger 进程单例。
var logger = newLogger(LogDir())

// LogDir 日志目录。FKA_LOG_DIR 优先，否则安装根下 logs/，再否则临时目录。
//
// ## 为什么回退到临时目录而不是什么都不做
//
// 这些是 **stdio 子进程**，由主程序拉起；用户不会给它们单独配目录。而「日志写不了
// 就静默丢掉」会让排查时完全看不到 server 到底干了什么——所以宁可落到临时目录。
func LogDir() string {
	if dir := strings.TrimSpace(os.Getenv("FKA_LOG_DIR")); dir != "" {
		return dir
	}
	if home := strings.TrimSpace(os.Getenv("FKA_HOME")); home != "" {
		return filepath.Join(home, "logs")
	}
	return filepath.Join(os.TempDir(), "fka-mcp-logs")
}

func newLogger(dir string) *Logger {
	l := &Logger{dir: dir}
	l.init()
	return l
}

// Log 返回进程级 logger。
func Log() *Logger { return logger }

func (l *Logger) init() {
	_ = os.MkdirAll(l.dir, 0o755)
	l.rotate(time.Now())
}

// rotate 换到当天的文件。**按天轮转**，文件名 <UTC 日期>.log。
//
// **与 agent 那边（internal/config）同名**：FKA_HOME 相同时三个进程写进**同一个**
// 文件，这是有意的——排查「server 起不来」时，主程序报的那些错得在同一份里。
// 而名字不能各叫各的：不一样就是同一天写出两个文件，`logs/` 里立刻分不清谁是谁。
func (l *Logger) rotate(now time.Time) {
	name := logFileName(now)
	if l.file != nil && l.fileName == name {
		return
	}
	if l.file != nil {
		_ = l.file.Close()
		l.file = nil
	}
	f, err := os.OpenFile(filepath.Join(l.dir, name),
		os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		// 日志写不了不该让服务起不来：控制台仍然可用
		l.file = nil
		return
	}
	l.file = f
	l.fileName = name
}

// logDateLayout 日志文件名里的日期，**UTC**。
const logDateLayout = "2006-01-02"

// logFileName 某个时刻对应的文件名。
func logFileName(now time.Time) string {
	return now.UTC().Format(logDateLayout) + ".log"
}

// currentConsoleLevel 在**每次写入时**解析，因此环境变量能覆盖到最早的那条日志。
func (l *Logger) currentConsoleLevel() Level {
	if level, ok := parseLevel(os.Getenv("LOG_LEVEL")); ok {
		return level
	}
	return LevelDebug
}

func (l *Logger) write(level Level, message string, ctx Context) {
	now := time.Now().UTC()

	// 文件输出：始终全量
	l.mu.Lock()
	l.rotate(now)
	if l.file != nil {
		_, _ = l.file.WriteString(renderEntry(now, level, message, ctx) + "\n")
	}
	l.mu.Unlock()

	// 控制台输出：按级别过滤
	if level < l.currentConsoleLevel() {
		return
	}

	// **一律 stderr，包括 info 与 debug。**
	//
	// 这个进程是 **stdio 子进程**：stdout 是与主程序之间的 JSON-RPC 通道，
	// 一行非 JSON 的日志就可能让对端解析失败（main.go 里 ServeStdio 前后都
	// 明写着「不要往 stdout 打任何东西」）。
	fmt.Fprintln(os.Stderr, renderLine(level, message, ctx))
}

// timeLayout 日志行开头那个时间戳的写法。**保持 UTC**。
const timeLayout = "2006-01-02T15:04:05.000Z"

// renderLine 拼控制台那一行：`[级别] 消息 键=值 …`。**纯函数**：格式能被直接测。
func renderLine(level Level, message string, ctx Context) string {
	var line strings.Builder
	line.WriteByte('[')
	line.WriteString(strings.ToUpper(level.String()))
	line.WriteString("] ")
	line.WriteString(oneLine(message))
	line.WriteString(renderFields(ctx))
	return line.String()
}

// renderEntry 拼**文件里**那一行：时间戳 + 与控制台完全一样的排版。
func renderEntry(now time.Time, level Level, message string, ctx Context) string {
	return now.Format(timeLayout) + " " + renderLine(level, message, ctx)
}

// renderFields 把字段拼成行尾的 ` 键=值 键=值`，**键按字典序排**。
func renderFields(ctx Context) string {
	if len(ctx) == 0 {
		return ""
	}
	keys := make([]string, 0, len(ctx))
	for key := range ctx {
		keys = append(keys, key)
	}
	sort.Strings(keys)

	var out strings.Builder
	for _, key := range keys {
		out.WriteByte(' ')
		out.WriteString(oneLine(key))
		out.WriteByte('=')
		out.WriteString(field(fmt.Sprint(ctx[key])))
	}
	return out.String()
}

// oneLine 把换行、回车、制表符换成看得见的两字符转义——**一条日志必须占一行**。
func oneLine(text string) string {
	return lineEscaper.Replace(text)
}

// lineEscaper 预编译的替换表。
var lineEscaper = strings.NewReplacer("\n", `\n`, "\r", `\r`, "\t", `\t`)

// field 收拾一个**字段值**（或键）：带空格、引号、`=` 的加引号。
//
// **加引号那条路上不能先过 `oneLine`**：真换行先被换成 `\n` 两个字符再 Quote，
// 出来的是 `\\n`。
func field(text string) string {
	for _, r := range text {
		if r == ' ' || r == '"' || r == '=' || r == 0x3000 {
			return strconv.Quote(text)
		}
	}
	return oneLine(text)
}

func (l *Logger) Debug(message string, ctx Context) { l.write(LevelDebug, message, ctx) }
func (l *Logger) Info(message string, ctx Context)  { l.write(LevelInfo, message, ctx) }
func (l *Logger) Warn(message string, ctx Context)  { l.write(LevelWarn, message, ctx) }
func (l *Logger) Error(message string, ctx Context) { l.write(LevelError, message, ctx) }

// Close 关掉文件句柄。进程退出与测试清理用。
func (l *Logger) Close() error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.file == nil {
		return nil
	}
	err := l.file.Close()
	l.file = nil
	return err
}
