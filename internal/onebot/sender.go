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

// OutMedia 待发送的富媒体（视频/语音/文件），URL 与 Base64 二选一。
// Base64 承载本地/内联数据，据其大小决定走简单 JSON 上传还是分片上传。
type OutMedia struct {
	URL      string // http/https 链接，由 QQ 拉取
	Base64   string // base64 原始数据（不含 data: 前缀），走 FileData 直传或分片上传
	FileName string // 文件名（含扩展名），file_type=4 透传；分片上传必填
}

// PartKind 消息段类型
type PartKind int

const (
	PartText     PartKind = iota // 文本（含 <@openid> 形式的 at）
	PartImage                    // 图片
	PartMarkdown                 // markdown(+可选 keyboard)
	PartVideo                    // 视频 file_type=2
	PartRecord                   // 语音 file_type=3
	PartFile                     // 文件 file_type=4
)

// MessagePart 一个有序消息段，对应一条独立 QQ 消息
type MessagePart struct {
	Kind     PartKind
	Text     string                    // PartText
	Image    OutImage                  // PartImage
	Media    OutMedia                  // PartVideo/PartRecord/PartFile
	Markdown *dto.Markdown             // PartMarkdown
	Keyboard *keyboard.MessageKeyboard // PartMarkdown 可选
}

// OutMessage 已解析、openid 已就绪的发送请求，Parts 保持 OneBot 段的原始顺序
type OutMessage struct {
	MsgID    string      // 被动回复目标 QQ msg_id，可空；用于占用被动回复窗口（5 分钟时效）
	EventID  string      // 被动回复目标 event_id（互动事件），可空；与 MsgID 二选一，都空=主动消息
	RefMsgID string      // 引用回复目标 QQ msg_id，可空；置入 message_reference 渲染「引用」气泡
	IsWakeup bool        // 单聊互动召回消息：与 MsgID/EventID 互斥，仅 C2C 生效
	Stream   *dto.Stream // 流式消息信息，仅 C2C 生效；非空时整条按流式分片发送（内容取 Parts 里的文本）
	Parts    []MessagePart
}

// HasContent 是否有可发送内容。流式末帧可能没有文本，只要带了 Stream 也算有内容。
func (m *OutMessage) HasContent() bool {
	return len(m.Parts) > 0 || m.Stream != nil
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

	// GetGroupInfo 获取群基本信息（白名单）
	GetGroupInfo(ctx context.Context, groupOpenID string) (*GroupInfo, error)
	// GetGroupBotState 获取机器人群内状态（白名单）
	GetGroupBotState(ctx context.Context, groupOpenID string) (*GroupBotState, error)
	// GetGroupJoinRequests 拉取入群申请列表（需群管理员）
	GetGroupJoinRequests(ctx context.Context, groupOpenID string, cursor string, limit int) (*JoinRequestList, error)
	// ApproveGroupJoinRequest 审批入群申请（需群管理员）
	ApproveGroupJoinRequest(ctx context.Context, groupOpenID, memberOpenID, joinRequestID, op, rejectReason string, blacklist bool) error
	// GetGroupMute 查询群禁言状态（需群管理员）
	GetGroupMute(ctx context.Context, groupOpenID string) (*RestrictChatSetting, error)
	// SetGroupMute 设置群成员禁言（需群管理员；单次最多 10 人）
	SetGroupMute(ctx context.Context, groupOpenID string, ops []MuteOp) error

	// GetMenu 查询全局自定义菜单
	GetMenu(ctx context.Context) (*dto.MenuGetResp, error)
	// SetMenu 修改全局自定义菜单（整体覆盖）
	SetMenu(ctx context.Context, req *dto.MenuPutReq) (*dto.VersionResp, error)
	// ListCommandPanels 查询指令面板列表
	ListCommandPanels(ctx context.Context, pager *dto.PanelListPager) (*dto.PanelListResp, error)
	// GetCommandPanel 查询指令面板详情
	GetCommandPanel(ctx context.Context, panelID string) (*dto.PanelDetail, error)
	// CreateCommandPanel 创建指令面板
	CreateCommandPanel(ctx context.Context, req *dto.PanelCreateReq) (*dto.PanelCreateResp, error)
	// SetCommandPanel 修改指令面板（整体覆盖）
	SetCommandPanel(ctx context.Context, panelID string, req *dto.PanelPutReq) (*dto.VersionResp, error)
	// DeleteCommandPanel 删除指令面板
	DeleteCommandPanel(ctx context.Context, panelID string) error
	// SetCommandPanelTarget 修改指令面板关联对象（仅 specific 面板）
	SetCommandPanelTarget(ctx context.Context, panelID string, req *dto.PanelTargetReq) error
}

// GroupInfo 群基本信息（OneBot 侧视图）
type GroupInfo struct {
	GroupOpenID     string
	GroupName       string
	GroupFingerMemo string
	GroupClassText  string
	GroupTags       []string
	GroupMemberNum  int
}

// GroupBotState 机器人群内状态
type GroupBotState struct {
	MemberOpenID      string
	JoinedAt          string
	AllowProactiveMsg bool
	RecvMsgSetting    string
	MemberRole        string
}

// JoinRequestList 入群申请列表
type JoinRequestList struct {
	List       []JoinRequest
	NextCursor string
}

// JoinRequest 单条入群申请
type JoinRequest struct {
	JoinRequestID string
	RiskTips      string
	UnionOpenID   string
	MemberOpenID  string
	Username      string
	ApplyAt       string
	ApplySource   string
	InvitedBy     string
	Bot           bool
	VerifyMessage string
	VerifyMethod  string
}

// RestrictChatSetting 群禁言状态
type RestrictChatSetting struct {
	GlobalMode string
	Members    []MemberMute
}

// MemberMute 成员禁言项
type MemberMute struct {
	MemberOpenID string
	MuteExpireAt string
	Username     string
	UnionOpenID  string
}

// MuteOp 成员禁言操作
type MuteOp struct {
	Op           string // add / update / del
	MemberOpenID string
	MuteExpireAt string // RFC3339；op=del 时可空
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
