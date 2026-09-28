package config

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
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
	// LevelCritical 会额外触发告警通道（微信告警在渠道层接上）
	LevelCritical Level = 50
)

var levelNames = map[Level]string{
	LevelDebug:    "debug",
	LevelInfo:     "info",
	LevelWarn:     "warn",
	LevelError:    "error",
	LevelCritical: "critical",
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
	case "critical":
		return LevelCritical, true
	}
	return 0, false
}

// Context 是日志的附加字段。**值不再加工**——与 Node 版一致，写进文件的就是
// 调用方给的那份，排查时看到什么就是什么。
type Context map[string]any

// Logger 进程级日志：控制台按级别过滤，**文件始终全量**——日志文件是排查用的，
// 不该因为控制台调静音而丢信息。
type Logger struct {
	mu         sync.Mutex
	dir        string
	file       *os.File
	fileDate   string
	override   *Level
	onCritical func(message string, ctx Context)
}

// Logger 进程单例。
var logger = newLogger(LogDir())

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

// rotate 换到当天的文件。**按天轮转**，文件名 app.<date>.log。
func (l *Logger) rotate(now time.Time) {
	date := now.Format("2006-01-02")
	if l.file != nil && l.fileDate == date {
		return
	}
	if l.file != nil {
		_ = l.file.Close()
		l.file = nil
	}
	f, err := os.OpenFile(filepath.Join(l.dir, "app."+date+".log"),
		os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		// 日志写不了不该让服务起不来：控制台仍然可用
		l.file = nil
		return
	}
	l.file = f
	l.fileDate = date
}

// SetConsoleLevel 设置控制台的最低输出级别。
//
// CLI 用它把噪音降到最低——CLI 的 stdout 是给人和脚本消费的结果输出，不该混入
// 内部日志（那些仍然完整写进日志文件）。
func (l *Logger) SetConsoleLevel(level Level) {
	l.mu.Lock()
	defer l.mu.Unlock()
	v := level
	l.override = &v
}

// SetCriticalHandler 挂上 critical 的额外通道（微信告警）。**渠道层接上**。
func (l *Logger) SetCriticalHandler(fn func(message string, ctx Context)) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.onCritical = fn
}

// currentConsoleLevel 在**每次写入时**解析，因此环境变量能覆盖到最早的那条日志。
func (l *Logger) currentConsoleLevel() Level {
	l.mu.Lock()
	override := l.override
	l.mu.Unlock()
	if override != nil {
		return *override
	}
	if level, ok := parseLevel(os.Getenv("LOG_LEVEL")); ok {
		return level
	}
	return LevelDebug
}

type logEntry struct {
	Timestamp string  `json:"timestamp"`
	Level     string  `json:"level"`
	Message   string  `json:"message"`
	Context   Context `json:"context,omitempty"`
}

func (l *Logger) write(level Level, message string, ctx Context) {
	now := time.Now().UTC()

	// 文件输出：始终全量
	l.mu.Lock()
	l.rotate(now)
	if l.file != nil {
		entry, err := json.Marshal(logEntry{
			Timestamp: now.Format("2006-01-02T15:04:05.000Z"),
			Level:     level.String(),
			Message:   message,
			Context:   ctx,
		})
		if err == nil {
			_, _ = l.file.Write(append(entry, '\n'))
		}
	}
	handler := l.onCritical
	l.mu.Unlock()

	// 控制台输出：按级别过滤
	if level < l.currentConsoleLevel() {
		return
	}
	line := fmt.Sprintf("[%s] %s", strings.ToUpper(level.String()), message)
	if len(ctx) > 0 {
		if rendered, err := json.Marshal(ctx); err == nil {
			line += " " + string(rendered)
		}
	}
	if level >= LevelError {
		fmt.Fprintln(os.Stderr, line)
	} else {
		fmt.Fprintln(os.Stdout, line)
	}

	if level == LevelCritical && handler != nil {
		handler(message, ctx)
	}
}

func (l *Logger) Debug(message string, ctx Context) { l.write(LevelDebug, message, ctx) }
func (l *Logger) Info(message string, ctx Context)  { l.write(LevelInfo, message, ctx) }
func (l *Logger) Warn(message string, ctx Context)  { l.write(LevelWarn, message, ctx) }
func (l *Logger) Error(message string, ctx Context) { l.write(LevelError, message, ctx) }

// Critical 记一条并触发告警通道。
func (l *Logger) Critical(message string, ctx Context) { l.write(LevelCritical, message, ctx) }

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
