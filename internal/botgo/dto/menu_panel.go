package dto

// ---- 自定义菜单与指令面板（/v2/menu、/v2/panels）----
// 文档：https://bot.q.qq.com/wiki/develop/api-v2/server-inter/menu-panel/

// Switch 开关配置（MenuItem.Type=switch 时有效）
type Switch struct {
	SwitchID string `json:"switch_id,omitempty"` // 开关唯一标识，切换后消息 ext 携带 "<switch_id>=1"
	Default  bool   `json:"default,omitempty"`   // 初始状态，true=默认打开
}

// SubMenuItem 二级子菜单项（不支持再嵌套、不支持 menu/switch 类型）
type SubMenuItem struct {
	Name        string `json:"name,omitempty"`         // 按钮名称，最多 14 字符
	Type        string `json:"type,omitempty"`         // send_message / link
	SendMessage string `json:"send_message,omitempty"` // type=send_message 时填入输入框的内容
	Link        string `json:"link,omitempty"`         // type=link 时的跳转地址，须 https://
}

// MenuItem 一级菜单项
type MenuItem struct {
	Name         string        `json:"name,omitempty"`           // 按钮名称，最多 10 字符（1 汉字算 2）
	Type         string        `json:"type,omitempty"`           // switch / send_message / link / menu
	SubMenuItems []SubMenuItem `json:"sub_menu_items,omitempty"` // type=menu 时的子菜单，最多 5 个
	SendMessage  string        `json:"send_message,omitempty"`   // type=send_message 时的内容
	Link         string        `json:"link,omitempty"`           // type=link 时的地址，须 https://
	Switch       *Switch       `json:"switch,omitempty"`         // type=switch 时的开关配置
}

// Menu 自定义菜单配置
type Menu struct {
	Items   []MenuItem `json:"items,omitempty"`   // 菜单项，最多 10 个
	Version int        `json:"version,omitempty"` // 版本号
}

// MenuGetResp 查询全局自定义菜单响应 GET /v2/menu
type MenuGetResp struct {
	Version int   `json:"version"`
	Menu    *Menu `json:"menu"`
}

// MenuPutReq 修改全局自定义菜单请求体 PUT /v2/menu
type MenuPutReq struct {
	Menu Menu `json:"menu"`
}

// VersionResp PUT /v2/menu 与 PUT /v2/panels/{id} 的通用响应
type VersionResp struct {
	Version int `json:"version"`
}

// PanelItem 指令面板元素
type PanelItem struct {
	Name      string `json:"name,omitempty"`       // 元素名称，最多 14 字符（约 7 汉字）
	Desc      string `json:"desc,omitempty"`       // 元素描述，最多 30 字符（约 15 汉字）
	Type      string `json:"type,omitempty"`       // command（指令）/ link（链接）
	OnlyAdmin bool   `json:"only_admin,omitempty"` // 是否仅管理员可点击
	Link      string `json:"link,omitempty"`       // type=link 时的地址，须 https://
}

// Panel 指令面板配置内容
type Panel struct {
	Items   []PanelItem `json:"items,omitempty"`   // 面板元素，最多 20 个
	Remark  string      `json:"remark,omitempty"`  // 开发者备注，最多 255 字符，不对用户展示
	Version int         `json:"version,omitempty"` // 版本号
}

// PanelCreateReq 创建指令面板请求体 POST /v2/panels
type PanelCreateReq struct {
	Scope        string   `json:"scope"`                   // c2c / group / channel / dm
	TargetType   string   `json:"target_type,omitempty"`   // all / specific（specific 仅 c2c、group）
	UserOpenids  []string `json:"user_openids,omitempty"`  // c2c + specific，一次最多 20
	GroupOpenids []string `json:"group_openids,omitempty"` // group + specific，一次最多 20
	Panel        Panel    `json:"panel"`
}

// PanelPutReq 修改指令面板请求体 PUT /v2/panels/{panel_id}
type PanelPutReq struct {
	Panel Panel `json:"panel"`
}

// PanelTargetReq 修改指令面板关联对象请求体 PUT /v2/panels/{panel_id}/target
type PanelTargetReq struct {
	Op           string   `json:"op"`                      // add / del
	UserOpenids  []string `json:"user_openids,omitempty"`  // c2c 场景
	GroupOpenids []string `json:"group_openids,omitempty"` // group 场景
}

// PanelDetail 面板详情 / 列表记录
type PanelDetail struct {
	PanelID      string   `json:"panel_id"`
	Scope        string   `json:"scope"`
	TargetType   string   `json:"target_type"`
	Panel        Panel    `json:"panel"`
	CreatedAt    string   `json:"created_at"`
	UpdatedAt    string   `json:"updated_at"`
	Version      int      `json:"version"`
	UserOpenids  []string `json:"user_openids"`
	GroupOpenids []string `json:"group_openids"`
}

// PanelListResp 查询面板列表响应 GET /v2/panels
type PanelListResp struct {
	Records    []PanelDetail `json:"records"`
	NextCursor string        `json:"next_cursor"`
	IsEnd      bool          `json:"is_end"`
}

// PanelListPager 查询面板列表分页参数
type PanelListPager struct {
	Scope  string // c2c / group / channel / dm，必填
	Limit  int    // 每页条数
	Cursor string // 上一页 next_cursor
}

// PanelCreateResp 创建面板响应
type PanelCreateResp struct {
	PanelID string `json:"panel_id"`
}
