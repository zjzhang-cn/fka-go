package config

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

// Context 是日志的附加字段。**值不加工**——与 Node 版一致，排查时看到什么
// 就是调用方给的那份。唯一的例外是**排版**：换行会被转义、带空格的值会被加引号，
// 那是为了守住「一条日志一行」这条不变式（见 `oneLine`）。
type Context map[string]any

// AccountField 账号那个字段的**保留键名**。
//
// 它同时是三处共用的那个名字，所以单独立出来：`Bind` 绑上去的是它、
// `Fields` 自动合并的是它、Logger 抠出来印前缀的也是它。写成字符串字面量
// 分散在三处的话，改名那天只会改掉其中一处——**而那处的表现是
// 「前缀里的账号空了」，不报错**。
const AccountField = "account"

// noAccount 控制台格式里「这条不属于任何账号」那一格。
//
// **印 `-` 而不是留空**：两格都空的行看起来像坏了，`-` 则是明确的一条信息
// ——启动、MCP 连接、配置读不了，这些事本来就没有账号。
const noAccount = "-"

// Type 这条日志说的是**哪一段事**。
//
// ## 它为什么不能靠字段里的某个值推出来
//
// 字段是**每一行自己带**的，而「这是消息层还是模型层」是**代码位置**的性质。
// 让调用点显式说出自己属于哪一段，`grep '\[LLM\]'` 就等于「把模型这一段
// 单独捞出来看」——多账号并行时这是最常做的一件事。
//
// 取值刻意短：三格前缀要占一行的开头，短才不挤。
type Type string

// 管道阶段。**代码即前缀里印出来的那几���**。
const (
	// TypeSYS 启动、配置、停机。没有账号。
	TypeSYS Type = "SYS"
	// TypeCHAN 渠道的接入与收发：长轮询、入站投递、发不出去。
	TypeCHAN Type = "CHAN"
	// TypeMSG 一条入站消息的处理：收到、拒答、已作答、答复已发出。
	TypeMSG Type = "MSG"
	// TypePRM 提示词怎么拼出来的：长度、工具数、历史压缩掉了多少。
	TypePRM Type = "PRM"
	// TypeLLM 提交给模型与模型返回。
	TypeLLM Type = "LLM"
	// TypeRSN 模型返回的推理内容。
	TypeRSN Type = "RSN"
	// TypeTOOL 工具的调用与结果。
	TypeTOOL Type = "TOOL"
	// TypeHIST 会话历史的读写。
	TypeHIST Type = "HIST"
)

func (t Type) String() string {
	if t == "" {
		return string(TypeSYS)
	}
	return string(t)
}

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

// ConsoleLevel 当前的控制台级别。
//
// **是 SetConsoleLevel 的另一半，不是多余的**：CLI 要在参数没给时保留默认值、
// 测试要验「参数真的压过了环境变量」，两件都得读得回现在是多少。
// 少一个读取口，那些用例就只能去猜或者去翻私有字段。
func (l *Logger) ConsoleLevel() Level {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.override != nil {
		return *l.override
	}
	if level, ok := parseLevel(os.Getenv("LOG_LEVEL")); ok {
		return level
	}
	return defaultConsoleLevel
}

// SetCriticalHandler 挂上 critical 的额外通道（微信告警）。**渠道层接上**。
func (l *Logger) SetCriticalHandler(fn func(message string, ctx Context)) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.onCritical = fn
}

// defaultConsoleLevel 环境变量也没给时用的级别。
const defaultConsoleLevel = LevelDebug

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
	return defaultConsoleLevel
}

// timeLayout 日志行开头那个时间戳的写法。
//
// **保持 UTC**：机器时区会变，而按天轮转的文件名是按 UTC 算的——同一个 `2026-01-02`
// 里的两行如果一个本地时区一个 UTC，排查跨零点的链路时排序就乱了。
const timeLayout = "2006-01-02T15:04:05.000Z"

func (l *Logger) write(level Level, typ Type, message string, ctx Context) {
	now := time.Now().UTC()

	// 文件输出：始终全量
	l.mu.Lock()
	l.rotate(now)
	if l.file != nil {
		_, _ = l.file.WriteString(renderEntry(now, level, typ, message, ctx) + "\n")
	}
	handler := l.onCritical
	l.mu.Unlock()

	// 控制台输出：按级别过滤
	if level < l.currentConsoleLevel() {
		return
	}
	line := renderLine(level, typ, message, ctx)
	if level >= LevelError {
		fmt.Fprintln(os.Stderr, line)
	} else {
		fmt.Fprintln(os.Stdout, line)
	}

	if level == LevelCritical && handler != nil {
		handler(message, ctx)
	}
}

// renderLine 拼控制台那一行：`[级别][账号][哪一段] 消息 字段=值 …`。
//
// **三格而不是一格**：账号那一格是为了让 `grep '\[account_002\]'` 能一把捞出
// **那个账号**的整条链路；`\[LLM\]` 则把模型这一段单独摘出来。
//
// **纯函数，刻意不碰任何全局状态**——所以格式能被直接测。
// 用「换掉 os.Stdout 抓输出」那种测法是有竞态的（`write` 与恢复函数
// 会同时读写那个变量），这类「看起来只能这么测」的地方多半是
// 该把纯的部分摘出来。
func renderLine(level Level, typ Type, message string, ctx Context) string {
	var line strings.Builder
	line.WriteByte('[')
	line.WriteString(strings.ToUpper(level.String()))
	line.WriteString("][")
	line.WriteString(field(accountOf(ctx)))
	line.WriteString("][")
	line.WriteString(typ.String())
	line.WriteString("] ")
	line.WriteString(oneLine(message))
	line.WriteString(renderFields(ctx))
	return line.String()
}

// renderEntry 拼**文件里**那一行：时间戳 + 与控制台完全一样的排版。
//
// ## 为什么要复用控制台那一套，而不是各写一种
//
// 之前文件是 JSON、控制台是文本，两边长得不一样，于是「控制台里看到的那句」和
// 「文件里搜到的那行」对不上号，`grep` 出来的模式要写两遍。现在两边同一个
// `renderLine`，**控制台上看见的排版就是文件里的排版**（只多一个时间戳）。
func renderEntry(now time.Time, level Level, typ Type, message string, ctx Context) string {
	return now.Format(timeLayout) + " " + renderLine(level, typ, message, ctx)
}

// renderFields 把字段拼成行尾的 ` 键=值 键=值`。
//
// **键按字典序排**：Go 遍历 map 的顺序是随机的，不排序的话同一份字段两次落盘
// 排出来的行不一样，diff 出来的全是噪音，而「跟昨天比多了哪个字段」正是
// 排查时要做的事。
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
		out.WriteString(field(valueText(ctx[key])))
	}
	return out.String()
}

// valueText 把任意类型的字段值转成一行文本。
//
// **值仍然不加工**（与 Node 版一致，`Context` 那条注释里说的事没变），只是把
// `map[string]any` 的参数按 `fmt` 的默认写法摊开——`fmt` 对 map 是按键排序的，
// 所以同一份参数每次也是同一个样子。
func valueText(value any) string {
	return fmt.Sprint(value)
}

// oneLine 把会**把一行弄断或弄歪**的字符换成看得见的两字符转义。
//
// ## 它守的是「一条日志 = 一行」这条不变式
//
// 日志是拿 `grep`、`tail`、`cut -d' ' -f3` 看的，而这三个都把**行**当单位。
// 工具参数、推理片段、数据库驱动的报错都能带换行——不收拾的话，一条日志会摊成
// 两三行，后半截看起来像**另一条**日志的，`cut` 出来的级别与阶段全错。
// 之前文件是 JSON，换行被 `\n` 天然转义掉了；**换成普通文本就得自己转**，
// 这是改格式之后最容易丢的一条性质。
//
// **只转这三个**：换行与回车会断行，制表符会让整行错位；其余控制字符既不断行
// 也不对齐，实际日志里也不出现，真出现了也只是难看，不值得为它加一条规则。
func oneLine(text string) string {
	return lineEscaper.Replace(text)
}

// lineEscaper 预编译的替换表。`strings.NewReplacer` 第一次调用会按最长匹配
// 建表，之后是线性扫描——比每次 `for range` 逐字符判断便宜。
var lineEscaper = strings.NewReplacer("\n", `\n`, "\r", `\r`, "\t", `\t`)

// field 收拾一个**字段值**（或键）：既占一行，也不跟相邻的键值粘在一起。
//
// ## 什么时候加引号
//
// 带空格、引号、`=` 的值加引号。不加的话 `text=第一行\n第二行` 会被读成两个
// 字段——排查时那个多出来的「字段」根本不存在。干净的短值保持裸着，
// `grep 'account=acct-1'` 这类朴素写法才成立。
//
// 加引号时走 `strconv.Quote`（它顺带把换行转成 `\n`），**所以那条路上不能先
// 过 `oneLine`**：先把真换行换成 `\n` 两个字符再 `Quote`，出来的是 `\\n`。
func field(text string) string {
	for _, r := range text {
		// 半角空格会切开字段；全角空格不断字段，只是对齐会歪，一并归到引号里
		if r == ' ' || r == '"' || r == '=' || r == 0x3000 {
			return strconv.Quote(text)
		}
	}
	return oneLine(text)
}

// accountOf 从字段里取账号。**取不到就印 `-`**——
// 启动、配置、MCP 连接这些事本来就没有账号，那一格该如实说「没有」，
// 而不是留个空让人以为漏了。
func accountOf(ctx Context) string {
	raw, ok := ctx[AccountField]
	if !ok || raw == nil {
		return noAccount
	}
	value := strings.TrimSpace(fmt.Sprint(raw))
	if value == "" {
		return noAccount
	}
	return value
}

func (l *Logger) Debug(typ Type, message string, ctx Context) {
	l.write(LevelDebug, typ, message, ctx)
}
func (l *Logger) Info(typ Type, message string, ctx Context) { l.write(LevelInfo, typ, message, ctx) }
func (l *Logger) Warn(typ Type, message string, ctx Context) { l.write(LevelWarn, typ, message, ctx) }
func (l *Logger) Error(typ Type, message string, ctx Context) {
	l.write(LevelError, typ, message, ctx)
}

// Critical 记一条并触发告警通道。
func (l *Logger) Critical(typ Type, message string, ctx Context) {
	l.write(LevelCritical, typ, message, ctx)
}

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
