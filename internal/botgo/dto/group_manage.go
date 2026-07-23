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
	OpMemberOpenID string      `json:"op_member_openid,omitempty"` // 操作者 openid（部分场景携带）
	Timestamp      interface{} `json:"timestamp"`                  // 时间戳，可能是 string 或 number
}

// C2CManageData C2C 主动消息推送开关(C2C_MSG_REJECT/C2C_MSG_RECEIVE)事件
type C2CManageData struct {
	OpenID    string `json:"openid"`    // 用户 openid
	Timestamp uint64 `json:"timestamp"` // 操作时间戳
}
