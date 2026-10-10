package web

import (
	"crypto/rand"
	"encoding/hex"
	"sync"
	"time"
)

// blobTTL 出站媒体在内存里活多久。够浏览器点开下载即可，不做长期存储——
// 这个 agent 不拥有数据，媒体只是过路。
const blobTTL = 10 * time.Minute

// blobStoreMaxBytes 所有待下载媒体的总量上限。**必须有**：字节来自工具结果，
// 没有上限就是让远端决定我们占多少内存。超了淘汰最旧的。
const blobStoreMaxBytes = 64 << 20

// blob 一份待下载的媒体。
type blob struct {
	data    []byte
	name    string
	mime    string
	expires time.Time
	seq     uint64
}

// blobStore 出站媒体的内存暂存。**单账号单进程**，所以就是一个带 TTL 的 map。
type blobStore struct {
	mu     sync.Mutex
	items  map[string]blob
	total  int
	nextID uint64
}

func newBlobStore() *blobStore { return &blobStore{items: map[string]blob{}} }

// put 存一份字节，返回下载 id。超过总量上限时先淘汰最旧的，再放不下就拒。
func (s *blobStore) put(data []byte, name, mime string) (string, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.sweepLocked(time.Now())
	if s.total+len(data) > blobStoreMaxBytes {
		s.evictOldestLocked(len(data))
	}
	if s.total+len(data) > blobStoreMaxBytes {
		return "", false
	}

	s.nextID++
	id := newID()
	s.items[id] = blob{data: data, name: name, mime: mime, expires: time.Now().Add(blobTTL), seq: s.nextID}
	s.total += len(data)
	return id, true
}

// get 取一份字节。过期即视为不存在。
func (s *blobStore) get(id string) (blob, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	item, ok := s.items[id]
	if !ok || time.Now().After(item.expires) {
		if ok {
			s.total -= len(item.data)
			delete(s.items, id)
		}
		return blob{}, false
	}
	return item, true
}

// sweepLocked 清掉过期项。**调用方必须已持锁**。
func (s *blobStore) sweepLocked(now time.Time) {
	for id, item := range s.items {
		if now.After(item.expires) {
			s.total -= len(item.data)
			delete(s.items, id)
		}
	}
}

// evictOldestLocked 淘汰最旧的项直到能容下 need 字节。**调用方必须已持锁**。
func (s *blobStore) evictOldestLocked(need int) {
	for s.total+need > blobStoreMaxBytes && len(s.items) > 0 {
		oldestID, oldestSeq := "", ^uint64(0)
		for id, item := range s.items {
			if item.seq < oldestSeq {
				oldestID, oldestSeq = id, item.seq
			}
		}
		if oldestID == "" {
			return
		}
		s.total -= len(s.items[oldestID].data)
		delete(s.items, oldestID)
	}
}

// newID 16 字节随机十六进制。**随机而不是自增**：下载 URL 不带鉴权之外的信息，
// 但随机 id 让"猜一个别人的文件"不可行。
func newID() string {
	var buf [16]byte
	if _, err := rand.Read(buf[:]); err != nil {
		// crypto/rand 失败极罕见；退回时间戳等价于可猜，但好过 panic
		return hex.EncodeToString([]byte(time.Now().Format(time.RFC3339Nano)))
	}
	return hex.EncodeToString(buf[:])
}
