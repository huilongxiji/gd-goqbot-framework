package onebot

import (
	"context"

	"goqbot/internal/botgo/dto"
	"goqbot/internal/botgo/dto/keyboard"
)

// OutImage 待发送的图片，URL 与 Base64 二选一
type OutImage struct {
	URL    string // http/https 链接，由 QQ 拉取
	Base64 string // base64 原始数据（不含 data: 前缀），走 FileData 直传
}

// PartKind 消息段类型
type PartKind int

const (
	PartText     PartKind = iota // 文本（含 <@openid> 形式的 at）
	PartImage                    // 图片
	PartMarkdown                 // markdown(+可选 keyboard)
)

// MessagePart 一个有序消息段，对应一条独立 QQ 消息
type MessagePart struct {
	Kind     PartKind
	Text     string                    // PartText
	Image    OutImage                  // PartImage
	Markdown *dto.Markdown             // PartMarkdown
	Keyboard *keyboard.MessageKeyboard // PartMarkdown 可选
}

// OutMessage 已解析、openid 已就绪的发送请求，Parts 保持 OneBot 段的原始顺序
type OutMessage struct {
	MsgID    string // 被动回复目标 QQ msg_id，可空
	EventID  string // 被动回复目标 event_id（互动事件），可空；与 MsgID 二选一，都空=主动消息
	IsWakeup bool   // 单聊互动召回消息：与 MsgID/EventID 互斥，仅 C2C 生效
	RefMsgID string // 要引用的 QQ 原始 msg_id（message_reference 引用气泡），可空
	Parts    []MessagePart
}

// HasContent 是否有可发送内容
func (m *OutMessage) HasContent() bool {
	return len(m.Parts) > 0
}

// MessageSender 实际调用 QQ openapi 发送/撤回消息，由 qq.Client 注入实现
type MessageSender interface {
	// SendGroup 向群发送消息，返回 QQ 侧消息 id
	SendGroup(ctx context.Context, groupOpenID string, msg *OutMessage) (qqMsgID string, err error)
	// SendC2C 向单聊用户发送消息，返回 QQ 侧消息 id
	SendC2C(ctx context.Context, userOpenID string, msg *OutMessage) (qqMsgID string, err error)
	// RetractGroup 撤回一条群消息
	RetractGroup(ctx context.Context, groupOpenID, qqMsgID string) error
	// RetractC2C 撤回一条单聊消息
	RetractC2C(ctx context.Context, userOpenID, qqMsgID string) error
	// AckInteraction 回执互动事件（按钮点击），code 为回执结果码
	AckInteraction(ctx context.Context, interactionID string, code int) error
}

// GatewayInfo /gateway/bot 返回的 ws 接入点频控信息
type GatewayInfo struct {
	URL            string // ws 接入点 URL
	Shards         uint32 // 建议分片数
	Total          uint32 // 每日可建连总次数
	Remaining      uint32 // 当前剩余可建连次数
	ResetAfter     uint32 // 距离配额重置的秒数
	MaxConcurrency uint32 // 每 5s 允许的最大并发建连数
}

// GatewayQuerier 查询 /gateway/bot 网关频控信息，由 qq.Client 注入实现
type GatewayQuerier func(ctx context.Context) (*GatewayInfo, error)
