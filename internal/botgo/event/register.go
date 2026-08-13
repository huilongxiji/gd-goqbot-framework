package event

import (
	"goqbot/internal/botgo/dto"
)

// DefaultHandlers 默认的 handler 结构，管理所有支持的 handler 类型
var DefaultHandlers struct {
	Ready       ReadyHandler
	ErrorNotify ErrorNotifyHandler
	Plain       PlainEventHandler

	Guild       GuildEventHandler
	GuildMember GuildMemberEventHandler
	Channel     ChannelEventHandler

	Message             MessageEventHandler
	MessageReaction     MessageReactionEventHandler
	ATMessage           ATMessageEventHandler
	DirectMessage       DirectMessageEventHandler
	MessageAudit        MessageAuditEventHandler
	MessageDelete       MessageDeleteEventHandler
	PublicMessageDelete PublicMessageDeleteEventHandler
	DirectMessageDelete DirectMessageDeleteEventHandler

	Audio AudioEventHandler

	Thread     ThreadEventHandler
	Post       PostEventHandler
	Reply      ReplyEventHandler
	ForumAudit ForumAuditEventHandler

	Interaction InteractionEventHandler

	GroupATMessage     GroupATMessageEventHandler
	GroupMessage       GroupMessageEventHandler
	C2CMessage         C2CMessageEventHandler
	SubscribeMsgStatus SubscribeMsgStatusEventHandler
	C2CFriend          C2CFriendEventHandler

	GroupAddRobot     GroupAddRobotEventHandler
	GroupDelRobot     GroupDelRobotEventHandler
	GroupMsgReject    GroupMsgRejectEventHandler
	GroupMsgReceive   GroupMsgReceiveEventHandler
	GroupJoinRequest  GroupJoinRequestEventHandler
	GroupMember       GroupMemberEventHandler
	C2CMsgReject      C2CMsgRejectEventHandler
	C2CMsgReceive     C2CMsgReceiveEventHandler

	EnterAIO EnterAIOEventHandler
}

// ReadyHandler 可以处理 ws 的 ready 事件
type ReadyHandler func(event *dto.WSPayload, data *dto.WSReadyData)

// ErrorNotifyHandler 当 ws 连接发生错误的时候，会回调，方便使用方监控相关错误
// 比如 reconnect invalidSession 等错误，错误可以转换为 bot.Err
type ErrorNotifyHandler func(err error)

// PlainEventHandler 透传handler
type PlainEventHandler func(event *dto.WSPayload, message []byte) error

// GuildEventHandler 频道事件handler
type GuildEventHandler func(event *dto.WSPayload, data *dto.WSGuildData) error

// GuildMemberEventHandler 频道成员事件 handler
type GuildMemberEventHandler func(event *dto.WSPayload, data *dto.WSGuildMemberData) error

// ChannelEventHandler 子频道事件 handler
type ChannelEventHandler func(event *dto.WSPayload, data *dto.WSChannelData) error

// MessageEventHandler 消息事件 handler
type MessageEventHandler func(event *dto.WSPayload, data *dto.WSMessageData) error

// MessageDeleteEventHandler 消息事件 handler
type MessageDeleteEventHandler func(event *dto.WSPayload, data *dto.WSMessageDeleteData) error

// PublicMessageDeleteEventHandler 消息事件 handler
type PublicMessageDeleteEventHandler func(event *dto.WSPayload, data *dto.WSPublicMessageDeleteData) error

// DirectMessageDeleteEventHandler 消息事件 handler
type DirectMessageDeleteEventHandler func(event *dto.WSPayload, data *dto.WSDirectMessageDeleteData) error

// MessageReactionEventHandler 表情表态事件 handler
type MessageReactionEventHandler func(event *dto.WSPayload, data *dto.WSMessageReactionData) error

// ATMessageEventHandler at 机器人消息事件 handler
type ATMessageEventHandler func(event *dto.WSPayload, data *dto.WSATMessageData) error

// DirectMessageEventHandler 私信消息事件 handler
type DirectMessageEventHandler func(event *dto.WSPayload, data *dto.WSDirectMessageData) error

// AudioEventHandler 音频机器人事件 handler
type AudioEventHandler func(event *dto.WSPayload, data *dto.WSAudioData) error

// MessageAuditEventHandler 消息审核事件 handler
type MessageAuditEventHandler func(event *dto.WSPayload, data *dto.WSMessageAuditData) error

// ThreadEventHandler 论坛主题事件 handler
type ThreadEventHandler func(event *dto.WSPayload, data *dto.WSThreadData) error

// PostEventHandler 论坛回帖事件 handler
type PostEventHandler func(event *dto.WSPayload, data *dto.WSPostData) error

// ReplyEventHandler 论坛帖子回复事件 handler
type ReplyEventHandler func(event *dto.WSPayload, data *dto.WSReplyData) error

// ForumAuditEventHandler 论坛帖子审核事件 handler
type ForumAuditEventHandler func(event *dto.WSPayload, data *dto.WSForumAuditData) error

// InteractionEventHandler 互动事件 handler
type InteractionEventHandler func(event *dto.WSPayload, data *dto.WSInteractionData) error

// ***************** 群消息/C2C消息  *****************

// GroupATMessageEventHandler 群中at机器人消息事件 handler
type GroupATMessageEventHandler func(event *dto.WSPayload, data *dto.WSGroupATMessageData) error

// GroupMessageEventHandler 群全量消息事件 handler（群主开启"获取群内全部消息"）
type GroupMessageEventHandler func(event *dto.WSPayload, data *dto.WSGroupMessageData) error

// C2CMessageEventHandler 机器人消息事件 handler
type C2CMessageEventHandler func(event *dto.WSPayload, data *dto.WSC2CMessageData) error

// ***************** C2C 添加/删除好友 *******************************

// C2CFriendEventHandler C2C 好友事件 handler
type C2CFriendEventHandler func(event *dto.WSPayload, data *dto.WSC2CFriendData) error

// ************************************************

// SubscribeMsgStatusEventHandler 订阅消息模板授权状态变更事件 handler
type SubscribeMsgStatusEventHandler func(event *dto.WSPayload, data *dto.WSSubscribeMsgStatus) error

// EnterAIOEventHandler 进入AIO事件 handler
type EnterAIOEventHandler func(event *dto.WSPayload, data *dto.WSEnterAIOData) error

// ***************** 群机器人增减 / 推送开关 *****************

// GroupAddRobotEventHandler 群添加机器人事件 handler
type GroupAddRobotEventHandler func(event *dto.WSPayload, data *dto.WSGroupRobotData) error

// GroupDelRobotEventHandler 群删除机器人事件 handler
type GroupDelRobotEventHandler func(event *dto.WSPayload, data *dto.WSGroupRobotData) error

// GroupMsgRejectEventHandler 群关闭机器人主动消息推送事件 handler
type GroupMsgRejectEventHandler func(event *dto.WSPayload, data *dto.WSGroupRobotData) error

// GroupMsgReceiveEventHandler 群开启机器人主动消息推送事件 handler
type GroupMsgReceiveEventHandler func(event *dto.WSPayload, data *dto.WSGroupRobotData) error

// GroupJoinRequestEventHandler 用户申请加群事件 handler（需机器人为群管理员）
type GroupJoinRequestEventHandler func(event *dto.WSPayload, data *dto.WSGroupJoinRequestData) error

// GroupMemberEventHandler 群成员进退群事件 handler（GROUP_MEMBER_ADD / GROUP_MEMBER_REMOVE）
type GroupMemberEventHandler func(event *dto.WSPayload, data *dto.WSGroupMemberData) error

// C2CMsgRejectEventHandler 用户关闭机器人 C2C 主动消息推送事件 handler
type C2CMsgRejectEventHandler func(event *dto.WSPayload, data *dto.WSC2CManageData) error

// C2CMsgReceiveEventHandler 用户开启机器人 C2C 主动消息推送事件 handler
type C2CMsgReceiveEventHandler func(event *dto.WSPayload, data *dto.WSC2CManageData) error

// RegisterHandlers 注册事件回调，并返回 intent 用于 websocket 的鉴权
func RegisterHandlers(handlers ...interface{}) dto.Intent {
	var i dto.Intent
	for _, h := range handlers {
		switch handle := h.(type) {
		case ReadyHandler:
			DefaultHandlers.Ready = handle
		case ErrorNotifyHandler:
			DefaultHandlers.ErrorNotify = handle
		case PlainEventHandler:
			DefaultHandlers.Plain = handle
		case AudioEventHandler:
			DefaultHandlers.Audio = handle
			i = i | dto.EventToIntent(
				dto.EventAudioStart, dto.EventAudioFinish,
				dto.EventAudioOnMic, dto.EventAudioOffMic,
			)
		case InteractionEventHandler:
			DefaultHandlers.Interaction = handle
			i = i | dto.EventToIntent(dto.EventInteractionCreate)
		case SubscribeMsgStatusEventHandler:
			DefaultHandlers.SubscribeMsgStatus = handle
			i = i | dto.EventToIntent(dto.EventSubscribeMsgStatus)
		case C2CFriendEventHandler:
			DefaultHandlers.C2CFriend = handle
			i = i | dto.EventToIntent(dto.EventC2CFriendAdd)
		case GroupAddRobotEventHandler:
			DefaultHandlers.GroupAddRobot = handle
			i = i | dto.EventToIntent(dto.EventGroupAddRobot)
		case GroupDelRobotEventHandler:
			DefaultHandlers.GroupDelRobot = handle
			i = i | dto.EventToIntent(dto.EventGroupDelRobot)
		case GroupMsgRejectEventHandler:
			DefaultHandlers.GroupMsgReject = handle
			i = i | dto.EventToIntent(dto.EventGroupMsgReject)
		case GroupMsgReceiveEventHandler:
			DefaultHandlers.GroupMsgReceive = handle
			i = i | dto.EventToIntent(dto.EventGroupMsgReceive)
		case GroupJoinRequestEventHandler:
			DefaultHandlers.GroupJoinRequest = handle
			i = i | dto.EventToIntent(dto.EventGroupJoinRequest)
		case GroupMemberEventHandler:
			DefaultHandlers.GroupMember = handle
			i = i | dto.EventToIntent(dto.EventGroupMemberAdd, dto.EventGroupMemberRemove)
		case C2CMsgRejectEventHandler:
			DefaultHandlers.C2CMsgReject = handle
			i = i | dto.EventToIntent(dto.EventC2CMsgReject)
		case C2CMsgReceiveEventHandler:
			DefaultHandlers.C2CMsgReceive = handle
			i = i | dto.EventToIntent(dto.EventC2CMsgReceive)
		case EnterAIOEventHandler:
			DefaultHandlers.EnterAIO = handle
			i = i | dto.EventToIntent(dto.EventEnterAIO)
		default:
		}
	}
	i = i | registerRelationHandlers(i, handlers...)
	i = i | registerMessageHandlers(i, handlers...)
	i = i | registerForumHandlers(i, handlers...)

	return i
}

func registerForumHandlers(i dto.Intent, handlers ...interface{}) dto.Intent {
	for _, h := range handlers {
		switch handle := h.(type) {
		case ThreadEventHandler:
			DefaultHandlers.Thread = handle
			i = i | dto.EventToIntent(
				dto.EventForumThreadCreate, dto.EventForumThreadUpdate, dto.EventForumThreadDelete,
			)
		case PostEventHandler:
			DefaultHandlers.Post = handle
			i = i | dto.EventToIntent(dto.EventForumPostCreate, dto.EventForumPostDelete)
		case ReplyEventHandler:
			DefaultHandlers.Reply = handle
			i = i | dto.EventToIntent(dto.EventForumReplyCreate, dto.EventForumReplyDelete)
		case ForumAuditEventHandler:
			DefaultHandlers.ForumAudit = handle
			i = i | dto.EventToIntent(dto.EventForumAuditResult)
		default:
		}
	}
	return i
}

// registerRelationHandlers 注册频道关系链相关handlers
func registerRelationHandlers(i dto.Intent, handlers ...interface{}) dto.Intent {
	for _, h := range handlers {
		switch handle := h.(type) {
		case GuildEventHandler:
			DefaultHandlers.Guild = handle
			i = i | dto.EventToIntent(dto.EventGuildCreate, dto.EventGuildDelete, dto.EventGuildUpdate)
		case GuildMemberEventHandler:
			DefaultHandlers.GuildMember = handle
			i = i | dto.EventToIntent(dto.EventGuildMemberAdd, dto.EventGuildMemberRemove, dto.EventGuildMemberUpdate)
		case ChannelEventHandler:
			DefaultHandlers.Channel = handle
			i = i | dto.EventToIntent(dto.EventChannelCreate, dto.EventChannelDelete, dto.EventChannelUpdate)
		default:
		}
	}
	return i
}

// registerMessageHandlers 注册消息相关的 handler
func registerMessageHandlers(i dto.Intent, handlers ...interface{}) dto.Intent {
	for _, h := range handlers {
		switch handle := h.(type) {
		case MessageEventHandler:
			DefaultHandlers.Message = handle
			i = i | dto.EventToIntent(dto.EventMessageCreate)
		case ATMessageEventHandler:
			DefaultHandlers.ATMessage = handle
			i = i | dto.EventToIntent(dto.EventAtMessageCreate)
		case DirectMessageEventHandler:
			DefaultHandlers.DirectMessage = handle
			i = i | dto.EventToIntent(dto.EventDirectMessageCreate)
		case MessageDeleteEventHandler:
			DefaultHandlers.MessageDelete = handle
			i = i | dto.EventToIntent(dto.EventMessageDelete)
		case PublicMessageDeleteEventHandler:
			DefaultHandlers.PublicMessageDelete = handle
			i = i | dto.EventToIntent(dto.EventPublicMessageDelete)
		case DirectMessageDeleteEventHandler:
			DefaultHandlers.DirectMessageDelete = handle
			i = i | dto.EventToIntent(dto.EventDirectMessageDelete)
		case MessageReactionEventHandler:
			DefaultHandlers.MessageReaction = handle
			i = i | dto.EventToIntent(dto.EventMessageReactionAdd, dto.EventMessageReactionRemove)
		case MessageAuditEventHandler:
			DefaultHandlers.MessageAudit = handle
			i = i | dto.EventToIntent(dto.EventMessageAuditPass, dto.EventMessageAuditReject)
		case GroupATMessageEventHandler:
			DefaultHandlers.GroupATMessage = handle
			i = i | dto.EventToIntent(dto.EventGroupAtMessageCreate)
		case GroupMessageEventHandler:
			DefaultHandlers.GroupMessage = handle
			i = i | dto.EventToIntent(dto.EventGroupMessageCreate)
		case C2CMessageEventHandler:
			DefaultHandlers.C2CMessage = handle
			i = i | dto.EventToIntent(dto.EventC2CMessageCreate)
		default:
		}
	}
	return i
}
