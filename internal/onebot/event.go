package onebot

import (
	"encoding/json"
	"sync"
	"time"

	"goqbot/internal/stats"
)

// Event OneBot v11 事件
type Event struct {
	Raw []byte
}

// EventBus 事件总线，供 QQ 侧推送、OneBot 侧订阅
type EventBus struct {
	mu       sync.RWMutex
	nextID   int
	handlers []busHandler
}

type busHandler struct {
	id int
	fn func(*Event) int
}

// NewEventBus 创建事件总线
func NewEventBus() *EventBus {
	return &EventBus{}
}

// OnPush 注册事件推送回调，回调返回值为成功投递的连接数。返回的函数用于取消注册（可重复调用）
func (b *EventBus) OnPush(fn func(*Event) int) func() {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.nextID++
	id := b.nextID
	b.handlers = append(b.handlers, busHandler{id: id, fn: fn})
	return func() {
		b.mu.Lock()
		defer b.mu.Unlock()
		for i, h := range b.handlers {
			if h.id == id {
				b.handlers = append(b.handlers[:i:i], b.handlers[i+1:]...)
				return
			}
		}
	}
}

// Push 广播事件，返回成功投递的下游连接总数
func (b *EventBus) Push(ev *Event) int {
	b.mu.RLock()
	handlers := append([]busHandler{}, b.handlers...)
	b.mu.RUnlock()
	delivered := 0
	for _, h := range handlers {
		delivered += h.fn(ev)
	}
	return delivered
}

// PushRaw 广播原始 JSON 事件，返回成功投递的下游连接总数
func (b *EventBus) PushRaw(raw []byte) int {
	return b.Push(&Event{Raw: append([]byte(nil), raw...)})
}

// LifecycleConnect 构造 lifecycle connect 元事件
func LifecycleConnect(selfID int64) []byte {
	ev := map[string]any{
		"post_type":       "meta_event",
		"meta_event_type": "lifecycle",
		"sub_type":        "connect",
		"time":            time.Now().Unix(),
		"self_id":         selfID,
	}
	b, _ := json.Marshal(ev)
	return b
}

// Heartbeat 构造 heartbeat 元事件（interval 单位毫秒，NoneBot 必填）
func Heartbeat(selfID int64, interval int64) []byte {
	s := stats.Get()
	ev := map[string]any{
		"post_type":       "meta_event",
		"meta_event_type": "heartbeat",
		"time":            time.Now().Unix(),
		"self_id":         selfID,
		"interval":        interval,
		"status": map[string]any{
			"app_enabled":     true,
			"app_good":        true,
			"app_initialized": true,
			"good":            true,
			"online":          true,
			"plugins_good":    nil,
			"stat": map[string]any{
				"packet_received":   0,
				"packet_sent":       0,
				"packet_lost":       0,
				"message_received":  s.MessageReceived,
				"message_sent":      s.MessageSent,
				"disconnect_times":  0,
				"lost_times":        0,
				"last_message_time": s.LastMessageTime,
			},
		},
	}
	b, _ := json.Marshal(ev)
	return b
}
