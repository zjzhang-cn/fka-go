package web

import (
	"encoding/json"
	"sync"
	"time"

	"github.com/zjzhang-cn/fka-go/internal/config"
)

// sseFrame 一帧 SSE。**命名事件 + JSON data**：前端 `addEventListener("message", …)`
// 之外还要按事件名分派，所以事件名是契约的一部分。
func sseFrame(event string, payload any) []byte {
	data, err := json.Marshal(payload)
	if err != nil {
		// payload 都是我们自己拼的 map/结构体，编不出来是代码缺陷；退回一句说明
		data = []byte(`{"error":"事件序列化失败"}`)
	}
	frame := make([]byte, 0, len(event)+len(data)+16)
	frame = append(frame, "event: "...)
	frame = append(frame, event...)
	frame = append(frame, "\ndata: "...)
	frame = append(frame, data...)
	frame = append(frame, "\n\n"...)
	return frame
}

// client 一个 SSE 连接。`ch` 是有界队列，满了就丢（记日志）——
// 慢浏览器不能堵住投递，与接缝的订阅队列同一条原则。
type client struct {
	ch   chan []byte
	done chan struct{}
	once sync.Once
}

func newClient() *client {
	return &client{ch: make(chan []byte, clientQueueSize), done: make(chan struct{})}
}

// close 让该连接的事件循环退出。
func (c *client) close() { c.once.Do(func() { close(c.done) }) }

// hub 会话 → 连接集合。**按会话隔离**：一个浏览器只该收到自己那条会话的事件。
type hub struct {
	mu    sync.Mutex
	rooms map[string]map[*client]struct{}
}

func newHub() *hub { return &hub{rooms: map[string]map[*client]struct{}{}} }

// clientQueueSize 单连接事件队列长度。256 条足够扛住一段流式推理的突发。
const clientQueueSize = 256

// join 把一条新连接挂进某个会话。
func (h *hub) join(conversation string) *client {
	c := newClient()
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.rooms[conversation] == nil {
		h.rooms[conversation] = map[*client]struct{}{}
	}
	h.rooms[conversation][c] = struct{}{}
	return c
}

// leave 摘掉一条连接。房间空了就删掉，避免 map 无限增长。
func (h *hub) leave(conversation string, c *client) {
	c.close()
	h.mu.Lock()
	defer h.mu.Unlock()
	room := h.rooms[conversation]
	if room == nil {
		return
	}
	delete(room, c)
	if len(room) == 0 {
		delete(h.rooms, conversation)
	}
}

// broadcast 往某个会话的所有连接推一帧。**非阻塞**：某个连接队列满只丢它自己那一条。
func (h *hub) broadcast(conversation, event string, payload any) int {
	frame := sseFrame(event, payload)

	h.mu.Lock()
	room := make([]*client, 0, len(h.rooms[conversation]))
	for c := range h.rooms[conversation] {
		room = append(room, c)
	}
	h.mu.Unlock()

	delivered := 0
	for _, c := range room {
		select {
		case c.ch <- frame:
			delivered++
		default:
			config.Log().Warn(config.TypeCHAN, "网页会话的事件队列满了，丢弃一条",
				config.Context{"conversation": conversation, "event": event})
		}
	}
	return delivered
}

// closeAll 停机时关掉所有连接，让各事件循环退出。
func (h *hub) closeAll() {
	h.mu.Lock()
	defer h.mu.Unlock()
	for _, room := range h.rooms {
		for c := range room {
			c.close()
		}
	}
	h.rooms = map[string]map[*client]struct{}{}
}

// keepAlive 注释帧的心跳间隔。SSE 长连接需要它穿过中间的代理。
const keepAlive = 20 * time.Second
