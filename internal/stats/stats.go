// Package stats 维护机器人收发消息的轻量统计，供 OneBot 心跳元事件填充 status。
package stats

import (
	"sync/atomic"
	"time"
)

// 进程级累计计数（原子访问）
var (
	messageReceived int64 // 累计收到（上报）的消息数
	messageSent     int64 // 累计成功发送的消息数
	lastMessageTime int64 // 最近一次收发的 unix 时间（秒）
)

// RecordReceived 记录一条收到（上报）的消息
func RecordReceived() {
	atomic.AddInt64(&messageReceived, 1)
	atomic.StoreInt64(&lastMessageTime, time.Now().Unix())
}

// RecordSent 记录一条成功发送的消息
func RecordSent() {
	atomic.AddInt64(&messageSent, 1)
	atomic.StoreInt64(&lastMessageTime, time.Now().Unix())
}

// Snapshot 收发统计快照
type Snapshot struct {
	MessageReceived int64 // 累计收到的消息数
	MessageSent     int64 // 累计发送的消息数
	LastMessageTime int64 // 最近一次收发的 unix 时间（秒）
}

// Get 返回当前统计快照
func Get() Snapshot {
	return Snapshot{
		MessageReceived: atomic.LoadInt64(&messageReceived),
		MessageSent:     atomic.LoadInt64(&messageSent),
		LastMessageTime: atomic.LoadInt64(&lastMessageTime),
	}
}
