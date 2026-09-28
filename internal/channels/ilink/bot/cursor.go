package bot

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
)

// getupdates 游标持久化。
//
// ## 为什么必须落盘
//
// 进程重启后如果游标归零，iLink 会**重新下发已处理过的消息**（重放）；
// 反过来服务端已推进而客户端从头拉，就是**丢失**。游标属于**账号**而不是
// 某个 goroutine，所以它得活过 goroutine。
//
// 当前实现是单个 JSON 文件（账号多时它们一起读写，量级只有几十条）。
type CursorStore struct {
	path string

	mu      sync.Mutex
	cursors map[string]string
}

// NewCursorStore 造游标存储并把已有内容读进来。
//
// **文件损坏时当空处理**：一个坏游标文件不该让账号起不来——最坏的结果是
// 重放一次消息（幂等可容忍），而拒绝启动是确定的损失。
func NewCursorStore(path string) *CursorStore {
	store := &CursorStore{path: path, cursors: map[string]string{}}
	store.load()
	return store
}

// Get 读账号的游标。从未见过该账号时返回空串——
// **空串是 iLink getupdates 约定的「从头开始」，不是错误**。
func (s *CursorStore) Get(accountID string) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.cursors[accountID]
}

// Set 写入并立即落盘。
func (s *CursorStore) Set(accountID, cursor string) {
	s.mu.Lock()
	defer s.mu.Unlock()

	// **无变化不写盘**：长轮询每轮都会拿到新的 buf，但绝大多数轮次没有消息，
	// 而落盘是每轮一次的话会把磁盘写满
	if s.cursors[accountID] == cursor {
		return
	}
	s.cursors[accountID] = cursor
	s.persistLocked()
}

// Clear 移除账号的游标（session 过期需重新登录时）。
//
// **过期必须清游标**：留着旧游标的话，重新登录后服务端会以为客户端已经消费到
// 那一段，于是那段时间的消息**永久收不到**。
func (s *CursorStore) Clear(accountID string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.cursors[accountID]; !ok {
		return
	}
	delete(s.cursors, accountID)
	s.persistLocked()
}

// Size 当前记录了多少个账号的游标。
func (s *CursorStore) Size() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.cursors)
}

func (s *CursorStore) load() {
	data, err := os.ReadFile(s.path)
	if err != nil {
		return
	}
	var cursors map[string]string
	if err := json.Unmarshal(data, &cursors); err != nil {
		return // 坏文件当没有，见 NewCursorStore 的说明
	}
	if cursors != nil {
		s.cursors = cursors
	}
}

// persistLocked 落盘。**先写临时文件再 rename**——
// 直接覆盖的话，进程在写一半时被杀，文件就只剩半截 JSON，
// 而那会让**所有账号**的游标一起丢。
// **调用方必须已持锁。**
func (s *CursorStore) persistLocked() {
	if s.path == "" {
		return
	}
	if err := os.MkdirAll(filepath.Dir(s.path), 0o755); err != nil {
		return
	}

	data, err := json.Marshal(s.cursors)
	if err != nil {
		return
	}
	temp := s.path + ".tmp"
	if err := os.WriteFile(temp, data, 0o600); err != nil {
		return
	}
	// 凭证级数据：0600。命名成 0644 的话同机器任何用户都能读游标
	_ = os.Rename(temp, s.path)
}
