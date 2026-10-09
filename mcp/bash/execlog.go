// 执行日志：把模型在沙盒里跑的每一条命令，按天写成一个 JSON Lines 文件。
//
// ## 与 internal/log 的分工
//
// internal/log 记的是「server 自己怎么了」（启动、连接、命令被拒），读者多半在
// 排查起不来；这里记的是「模型让沙盒干了什么」（命令、退出码、耗时），读者在做
// 审计与复盘。两者格式不同（一个带级别的文本、一个 JSON Lines），混在一起两边
// 都不好读，所以各写各的文件。
//
// ## 为什么放在安装根 logs/ 下，而不是沙盒根里
//
// 沙盒根对模型可写。审计日志要是放在**被审计者能删**的地方，等于让它自己擦掉
// 证据。所以它落在 log.LogDir()（FKA_LOG_DIR 优先，否则 `<安装根>/logs`）：bwrap
// 模式下 `/` 只读绑定，模型既写不进也删不掉。**默认开启**——命令执行本就该留痕。
//
// ## 格式：JSON Lines，一行一条
//
// 一行一个 JSON 对象，`jq` / `grep` 都好使。命令里的换行由 JSON 转义，
// **一条记录始终占一行**。按 UTC 日期轮转，文件名 `bash-exec-<日期>.log`。
package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// execLogDateLayout 文件名里的日期，**UTC**。与 internal/log 用同一套锚点，
// 免得同一个时刻两份日志落在不同的日子。
const execLogDateLayout = "2006-01-02"

// execLogPrefix 执行日志文件名前缀。
const execLogPrefix = "bash-exec-"

// execRecord 一条执行记录。**只记元信息不记输出正文**：输出走 stdout/stderr 那条
// 路，这里要的是「什么时候、在哪个目录、跑了什么、结果如何」。
//
// ExitCode 用指针：被策略拒 / cwd 越界 / 启动失败时命令**根本没跑**，此时
// exit_code 该整个缺席，而不是伪装成 0。
type execRecord struct {
	Time       string `json:"time"`
	Cwd        string `json:"cwd,omitempty"`
	Command    string `json:"command"`
	ExitCode   *int   `json:"exit_code,omitempty"`
	TimedOut   bool   `json:"timed_out,omitempty"`
	Truncated  bool   `json:"truncated,omitempty"`
	DurationMS int64  `json:"duration_ms"`
	Error      string `json:"error,omitempty"`
}

// ExecLog 按天轮转的 JSON Lines 执行日志。**会被多个工具调用并发写**，内部加锁。
type ExecLog struct {
	mu       sync.Mutex
	dir      string
	file     *os.File
	fileName string
}

// NewExecLog 在 dir 下开执行日志；目录在第一次写入时按需创建。
func NewExecLog(dir string) *ExecLog { return &ExecLog{dir: dir} }

// Record 追加一条记录。**写不进去不报错**：日志是旁路，不该因为磁盘满 / 权限问题
// 把一条本来能跑的命令挡下来（与 internal/log 同一条取舍）。Time 为空时自动补上。
func (l *ExecLog) Record(rec execRecord) {
	if l == nil {
		return
	}
	now := time.Now().UTC()
	if rec.Time == "" {
		rec.Time = now.Format(time.RFC3339Nano)
	}
	line, err := json.Marshal(rec)
	if err != nil {
		return
	}

	l.mu.Lock()
	defer l.mu.Unlock()
	l.rotate(now)
	if l.file == nil {
		return
	}
	_, _ = l.file.Write(append(line, '\n'))
}

// rotate 换到当天的文件。**按天轮转**，与 internal/log 同一条理由：一个文件不会
// 无限长大，且「今天跑了什么」直接看今天那个文件。
func (l *ExecLog) rotate(now time.Time) {
	name := execLogPrefix + now.UTC().Format(execLogDateLayout) + ".log"
	if l.file != nil && l.fileName == name {
		return
	}
	if l.file != nil {
		_ = l.file.Close()
		l.file = nil
	}
	if err := os.MkdirAll(l.dir, 0o755); err != nil {
		return
	}
	f, err := os.OpenFile(filepath.Join(l.dir, name),
		os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return
	}
	l.file = f
	l.fileName = name
}

// Close 关掉文件句柄。进程退出与测试清理用。
func (l *ExecLog) Close() error {
	if l == nil {
		return nil
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.file == nil {
		return nil
	}
	err := l.file.Close()
	l.file = nil
	return err
}
