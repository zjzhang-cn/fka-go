package llm

import (
	"bufio"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/zjzhang-cn/fka-go/internal/config"
)

// SessionHistoryEnabled 读 SESSION_HISTORY。默认开；`0 / off / false / no` 关。
func SessionHistoryEnabled() bool {
	raw := strings.ToLower(strings.TrimSpace(os.Getenv("SESSION_HISTORY")))
	if raw == "" {
		return true
	}
	switch raw {
	case "0", "off", "false", "no":
		return false
	}
	return true
}

// HistoryDir 会话历史目录。与 .env / socket 同一条理由：**按安装根解析，不按 cwd**。
func HistoryDir() string { return config.DataPath("history") }

// SessionPath 一个会话的历史文件：<账号>_<会话>.jsonl。账号段可空（单账号部署）。
//
// 会话与账号都来自外部，**因此要压成能安全当文件名的形式**（见 safeSegment）。
func SessionPath(sessionID string, accountID string) string {
	name := historyFileName(sessionID, accountID)
	return filepath.Join(HistoryDir(), name)
}

// safeSegment 把一个标识压成能安全当文件名的形式：非法字符换 _，并限制长度。
//
// 与 ids.IsSafeWxid **刻意不同**：这里允许出现非法字符（外部标识可能带奇怪字符，
// 报错中断历史不如压一下），但压出来的名字仍然可能撞车——所以再截到 40 字符，
// 剩下的区分度靠调用方给的 accountID 前缀。
func safeSegment(value string) string {
	cleaned := strings.Map(func(r rune) rune {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '_', r == '.':
			return r
		default:
			return '_'
		}
	}, value)
	cleaned = strings.Trim(cleaned, ".")
	if cleaned == "" {
		cleaned = "unknown"
	}
	if len(cleaned) > 40 {
		cleaned = cleaned[:40]
	}
	return cleaned
}

func historyFileName(sessionID, accountID string) string {
	if accountID != "" {
		return safeSegment(accountID) + "_" + safeSegment(sessionID) + ".jsonl"
	}
	return safeSegment(sessionID) + ".jsonl"
}

type sessionHistory struct {
	dir string
	// 每个文件一把锁：同一会话可能被并发追加（两个账号同时轮询到同一段历史时
	// 不太可能，但 IPC 与主循环都可能写），不加锁会交错出半行 JSON。
	mu    sync.Mutex
	locks map[string]*sync.Mutex
}

// NewSessionHistory 造会话历史存储。**dir 为空 = 不持久化**（测试与关闭 SESSION_HISTORY 时）。
func NewSessionHistory(dir string) SessionHistoryStore {
	return &sessionHistory{dir: dir, locks: map[string]*sync.Mutex{}}
}

// NewDefaultSessionHistory 按配置造。SESSION_HISTORY 关闭时返回 nil。
func NewDefaultSessionHistory() SessionHistoryStore {
	if !SessionHistoryEnabled() {
		return nil
	}
	return NewSessionHistory(HistoryDir())
}

func (s *sessionHistory) lockFor(path string) *sync.Mutex {
	s.mu.Lock()
	defer s.mu.Unlock()
	lock, ok := s.locks[path]
	if !ok {
		lock = &sync.Mutex{}
		s.locks[path] = lock
	}
	return lock
}

func (s *sessionHistory) resolve(sessionID, accountID string) string {
	if s.dir == "" {
		return ""
	}
	return filepath.Join(s.dir, historyFileName(sessionID, accountID))
}

// Load 读回完整的会话前缀。
//
// **system 消息被排除**：提示词每轮重建，存下来只会让文件与实际发给模型的内容不一致。
// 坏行跳过（半行 JSON 来自进程被 kill），坏文件不删——先跳过，让用户看到日志再说。
func (s *sessionHistory) Load(sessionID, accountID string) []ChatMessage {
	path := s.resolve(sessionID, accountID)
	if path == "" {
		return nil
	}

	file, err := os.Open(path)
	if err != nil {
		if !os.IsNotExist(err) {
			config.Log().Warn("会话历史读取失败，按无历史处理", config.Context{
				"account": accountID, "session": sessionID, "error": err.Error(),
			})
		}
		return nil
	}
	defer func() { _ = file.Close() }()

	out := make([]ChatMessage, 0, 32)
	scanner := bufio.NewScanner(file)
	// 一行可能很大（一条消息带全部工具结果），默认 64KB 不够
	scanner.Buffer(make([]byte, 0, 64*1024), 8*1024*1024)

	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		var message ChatMessage
		if err := json.Unmarshal([]byte(line), &message); err != nil {
			continue // 坏行跳过，不影响其余
		}
		if message.Role == "" || message.Role == RoleSystem {
			continue
		}
		out = append(out, message)
	}

	if err := scanner.Err(); err != nil {
		config.Log().Warn("会话历史读取中断，已返回读到的部分", config.Context{
			"account": accountID, "session": sessionID, "error": err.Error(),
		})
	}
	return out
}

// Append 追加本轮新产生的消息。
//
// **空消息不写**：写进去会在下一轮变成一条空的 user/assistant 记录，白占预算也误导模型。
// I/O 失败只记日志——**绝不能因为存历史失败而影响这一轮的回答**。
func (s *sessionHistory) Append(sessionID, accountID string, messages []ChatMessage) {
	path := s.resolve(sessionID, accountID)
	if path == "" || len(messages) == 0 {
		return
	}

	lines := make([]string, 0, len(messages))
	for _, message := range messages {
		if message.Role == "" || message.Role == RoleSystem {
			continue
		}
		if message.Content == "" && len(message.ToolCalls) == 0 {
			continue
		}
		encoded, err := json.Marshal(message)
		if err != nil {
			continue
		}
		lines = append(lines, string(encoded))
	}
	if len(lines) == 0 {
		return
	}

	lock := s.lockFor(path)
	lock.Lock()
	defer lock.Unlock()

	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		config.Log().Warn("会话历史目录建不出来，本轮历史未持久化", config.Context{
			"account": accountID, "session": sessionID, "error": err.Error(),
		})
		return
	}

	file, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		config.Log().Warn("会话历史追加失败（不影响回答）", config.Context{
			"account": accountID, "session": sessionID, "error": err.Error(),
		})
		return
	}
	defer func() { _ = file.Close() }()

	if _, err := file.WriteString(strings.Join(lines, "\n") + "\n"); err != nil {
		config.Log().Warn("会话历史写入失败（不影响回答）", config.Context{
			"account": accountID, "session": sessionID, "error": err.Error(),
		})
	}
}
