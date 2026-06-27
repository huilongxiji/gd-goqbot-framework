package dto

// User 用户
type User struct {
	ID               string `json:"id"`
	Username         string `json:"username"`
	Avatar           string `json:"avatar"`
	Bot              bool   `json:"bot"`
	MemberOpenID     string `json:"member_openid,omitempty"` // 群成员 openid（群消息 author）
	MemberRole       string `json:"member_role,omitempty"`   // 群成员角色 owner/admin/member
	IsYou            bool   `json:"is_you,omitempty"`        // mentions 中是否 @ 当前机器人
	UnionOpenID      string `json:"union_openid"`            // 特殊关联应用的 openid
	UnionUserAccount string `json:"union_user_account"`      // 机器人关联的用户信息，与union_openid关联的应用是同一个
}
