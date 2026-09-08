package dto

// GroupRobotEvent 群机器人增减(GROUP_ADD_ROBOT/GROUP_DEL_ROBOT)
// 及群主动消息推送开关(GROUP_MSG_REJECT/GROUP_MSG_RECEIVE)事件，payload.d 同形
type GroupRobotEvent struct {
	ID             string      `json:"id"`               // 事件 id（部分事件在 d 内携带）
	GroupOpenID    string      `json:"group_openid"`     // 群 openid
	OpMemberOpenID string      `json:"op_member_openid"` // 操作者 openid
	Timestamp      interface{} `json:"timestamp"`        // 时间戳，可能是 string 或 number
}

// GroupMemberEvent 群成员进退群(GROUP_MEMBER_ADD/GROUP_MEMBER_REMOVE)事件
type GroupMemberEvent struct {
	GroupOpenID    string      `json:"group_openid"`               // 群 openid
	MemberOpenID   string      `json:"member_openid"`              // 进退群成员 openid
	UserOpenID     string      `json:"user_openid,omitempty"`      // 用户 OpenID（跨应用统一标识，可能为空）
	OpMemberOpenID string      `json:"op_member_openid,omitempty"` // 操作者 openid（部分场景携带）
	Timestamp      interface{} `json:"timestamp"`                  // 时间戳，可能是 string 或 number
}

// C2CManageData C2C 主动消息推送开关(C2C_MSG_REJECT/C2C_MSG_RECEIVE)事件
type C2CManageData struct {
	OpenID    string `json:"openid"`    // 用户 openid
	Timestamp uint64 `json:"timestamp"` // 操作时间戳
}

// GroupJoinRequestEvent 用户申请加群(GROUP_JOIN_REQUEST)事件
type GroupJoinRequestEvent struct {
	GroupOpenID   string        `json:"group_openid"`            // 群 OpenID
	JoinRequestID string        `json:"join_request_id"`         // 申请 ID，审批时需回传
	RiskTips      string        `json:"risk_tips,omitempty"`     // 安全提示语
	UnionOpenID   string        `json:"union_openid,omitempty"`  // 跨应用统一标识
	MemberOpenID  string        `json:"member_openid"`           // 申请人 openid
	Username      string        `json:"username,omitempty"`      // 申请人昵称
	ApplyAt       string        `json:"apply_at,omitempty"`      // 申请时间（RFC3339）
	ApplySource   string        `json:"apply_source,omitempty"`  // self_apply / invited
	InvitedBy     string        `json:"invited_by,omitempty"`    // 邀请人 openid
	Bot           bool          `json:"bot,omitempty"`           // 是否为机器人账号
	VerifyInfo    *VerifyInfo   `json:"verify_info,omitempty"`   // 入群验证信息
	AutoApproved  *AutoApproved `json:"auto_approved,omitempty"` // 自动审批扩展（下行事件才有）
}

// AutoApproved 自动审批通过扩展信息
type AutoApproved struct {
	StrategyID string `json:"strategy_id"` // 自动审批策略 ID
}

// ---- 群聊管理 OpenAPI 请求/响应 ----

// GroupInfo 获取群基本信息响应 GET /v2/groups/{group_openid}/info
type GroupInfo struct {
	GroupOpenID     string   `json:"group_openid"`
	GroupName       string   `json:"group_name"`
	GroupFingerMemo string   `json:"group_finger_memo"`
	GroupClassText  string   `json:"group_class_text"`
	GroupTags       []string `json:"group_tags"`
	GroupMemberNum  int      `json:"group_member_num"`
}

// GroupBotState 获取机器人群内状态响应 GET /v2/groups/{group_openid}/bot_state
type GroupBotState struct {
	MemberOpenID      string `json:"member_openid"`
	JoinedAt          string `json:"joined_at"`
	AllowProactiveMsg bool   `json:"allow_proactive_msg"`
	RecvMsgSetting    string `json:"recv_msg_setting"` // all / only_mention / mention_and_context
	MemberRole        string `json:"member_role"`      // member / owner / admin
}

// GroupMember 群成员信息
// GET /v2/groups/{group_openid}/members 单项，及 GET /v2/groups/{group_openid}/members/{member_openid} 响应
type GroupMember struct {
	MemberOpenID string `json:"member_openid"`
	Username     string `json:"username"`
	MemberRole   string `json:"member_role"` // member / owner / admin
	Bot          bool   `json:"bot"`
	JoinedAt     string `json:"joined_at"` // RFC3339
	UnionOpenID  string `json:"union_openid,omitempty"`
}

// GroupMemberList 获取群成员列表响应 GET /v2/groups/{group_openid}/members
type GroupMemberList struct {
	Members    []*GroupMember `json:"members"`
	NextCursor string         `json:"next_cursor"`
}

// GroupMemberListPager 群成员列表分页参数（官方每次最多 30 条）
type GroupMemberListPager struct {
	Cursor string `json:"cursor,omitempty"`
}

// JoinRequestList 入群申请列表响应 GET /v2/groups/{group_openid}/join_request_list
type JoinRequestList struct {
	List       []*JoinRequest `json:"list"`
	NextCursor string         `json:"next_cursor"`
}

// JoinRequest 单条入群申请
type JoinRequest struct {
	JoinRequestID string      `json:"join_request_id"`
	RiskTips      string      `json:"risk_tips,omitempty"`
	UnionOpenID   string      `json:"union_openid,omitempty"`
	MemberOpenID  string      `json:"member_openid"`
	Username      string      `json:"username,omitempty"`
	ApplyAt       string      `json:"apply_at,omitempty"`
	ApplySource   string      `json:"apply_source,omitempty"` // self_apply / invited
	InvitedBy     string      `json:"invited_by,omitempty"`
	Bot           bool        `json:"bot,omitempty"`
	VerifyInfo    *VerifyInfo `json:"verify_info,omitempty"`
}

// VerifyInfo 用户入群验证方式
type VerifyInfo struct {
	Method        string      `json:"method"` // verify_message / admin_review_qa
	VerifyMessage string      `json:"verify_message,omitempty"`
	ReviewQAList  []*ReviewQA `json:"review_qa_list,omitempty"`
}

// ReviewQA 管理员设置的入群问答
type ReviewQA struct {
	Question string `json:"question"`
	Answer   string `json:"answer"`
}

// JoinRequestListPager 入群申请列表分页参数
type JoinRequestListPager struct {
	Cursor string `json:"cursor,omitempty"`
	Limit  int    `json:"limit,omitempty"`
}

// ApprovalJoinRequestReq 入群申请审批请求体
// POST /v2/groups/{group_openid}/approval_join_request/{member_openid}
type ApprovalJoinRequestReq struct {
	Op                   string `json:"op"` // approve / decline
	JoinRequestID        string `json:"join_request_id,omitempty"`
	RejectReason         string `json:"reject_reason,omitempty"`
	AddToMemberBlacklist bool   `json:"add_to_member_blacklist,omitempty"`
}

// RestrictChatSetting 查询群禁言状态响应 GET /v2/groups/{group_openid}/restrict_chat_setting
type RestrictChatSetting struct {
	GlobalRule *GlobalMuteRule    `json:"global_rule"`
	Members    []*MemberMuteState `json:"members"`
}

// GlobalMuteRule 群级禁言规则（全员禁言配置）
type GlobalMuteRule struct {
	Mode           string               `json:"mode"` // none / always / schedule
	ScheduleRules  []*MuteScheduleRule  `json:"schedule_rules,omitempty"`
	RecurringRules []*MuteRecurringRule `json:"recurring_rules,omitempty"`
}

// MuteScheduleRule 定时禁言规则
type MuteScheduleRule struct {
	TaskID  string `json:"task_id"`
	StartAt string `json:"start_at"`
	EndAt   string `json:"end_at"`
	Enabled bool   `json:"enabled"`
}

// MuteRecurringRule 周期禁言规则
type MuteRecurringRule struct {
	TaskID    string `json:"task_id"`
	Weekdays  []int  `json:"weekdays"`
	StartTime string `json:"start_time"`
	EndTime   string `json:"end_time"`
	Enabled   bool   `json:"enabled"`
}

// MemberMuteState 成员级禁言状态
type MemberMuteState struct {
	MemberOpenID string `json:"member_openid"`
	MuteExpireAt string `json:"mute_expire_at"`
	Username     string `json:"username,omitempty"`
	UnionOpenID  string `json:"union_openid,omitempty"`
}

// SetRestrictChatReq 设置群成员禁言请求体
// POST /v2/groups/{group_openid}/restrict_chat_setting
type SetRestrictChatReq struct {
	Members []*SetMemberMuteState `json:"members,omitempty"`
}

// SetMemberMuteState 单条成员禁言操作
type SetMemberMuteState struct {
	Op           string `json:"op"` // add / update / del
	MemberOpenID string `json:"member_openid"`
	MuteExpireAt string `json:"mute_expire_at,omitempty"` // RFC3339；op=del 时可空
}
