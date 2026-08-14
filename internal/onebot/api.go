package onebot

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"goqbot/internal/botgo/dto"
	"goqbot/internal/botgo/dto/keyboard"
	"github.com/tidwall/gjson"

	"goqbot/internal/filelog"
	"goqbot/internal/idmap"
	"goqbot/internal/prettylog"
	"goqbot/internal/stats"
)

// msgCacheLimit get_msg 内存缓存的最大条数，超出按 FIFO 淘汰最早的
const msgCacheLimit = 10000

// fwdCacheLimit get_forward_msg 内存缓存的最大条数，超出按 FIFO 淘汰最早的
const fwdCacheLimit = 1000

// Caller OneBot v11 API 动作调用器
type Caller struct {
	mu         sync.RWMutex   // 保护下列可在运行期被注入/更新的字段
	selfID     int64          // 上报用 self_id
	appID      string         // 机器人 AppID，昵称缺省时作回退
	nickname   string         // READY 后写入的真实机器人用户名
	appName    string         // get_version_info 用程序名
	appVersion string         // get_version_info 用版本号
	sender     MessageSender  // QQ 发送通道，由 qq.Client 注入
	gateway    GatewayQuerier // /gateway/bot 查询器，由 qq.Client 注入
	debug      bool           // true=打印收到的动作日志

	ids *idmap.Store // openid<->数字 id 映射，用于动作里的 id 反查

	// get_msg 内存缓存（仅内存，不持久化）：message_id -> 事件快照
	cacheMu    sync.RWMutex
	msgCache   map[int64]map[string]any
	cacheOrder []int64 // 插入顺序，用于上限淘汰

	// get_forward_msg 内存缓存：forward id (QQ msg_id) -> node 数组
	fwdCache map[string][]map[string]any
	fwdOrder []string
}

// NewCaller 创建 API 调用器
func NewCaller(selfID int64, appID string, ids *idmap.Store, debug bool, appName, appVersion string) *Caller {
	return &Caller{
		selfID:     selfID,
		appID:      appID,
		appName:    appName,
		appVersion: appVersion,
		ids:        ids,
		debug:      debug,
		msgCache:   make(map[int64]map[string]any),
		fwdCache:   make(map[string][]map[string]any),
	}
}

// CacheMsg 缓存一条已构造的消息事件快照，供 get_msg 取回（有界，FIFO 淘汰）
func (c *Caller) CacheMsg(id int64, snap map[string]any) {
	if id == 0 || snap == nil {
		return
	}
	c.cacheMu.Lock()
	defer c.cacheMu.Unlock()
	if _, exists := c.msgCache[id]; !exists {
		c.cacheOrder = append(c.cacheOrder, id)
		for len(c.cacheOrder) > msgCacheLimit {
			oldest := c.cacheOrder[0]
			c.cacheOrder = c.cacheOrder[1:]
			delete(c.msgCache, oldest)
		}
	}
	c.msgCache[id] = snap
}

// CacheForward 缓存合并转发节点，供 get_forward_msg 取回（有界，FIFO 淘汰）
func (c *Caller) CacheForward(id string, nodes []map[string]any) {
	if id == "" || len(nodes) == 0 {
		return
	}
	c.cacheMu.Lock()
	defer c.cacheMu.Unlock()
	if _, exists := c.fwdCache[id]; !exists {
		c.fwdOrder = append(c.fwdOrder, id)
		for len(c.fwdOrder) > fwdCacheLimit {
			oldest := c.fwdOrder[0]
			c.fwdOrder = c.fwdOrder[1:]
			delete(c.fwdCache, oldest)
		}
	}
	c.fwdCache[id] = nodes
}

// getForward 按 forward id 取回缓存的节点数组
func (c *Caller) getForward(id string) ([]map[string]any, bool) {
	c.cacheMu.RLock()
	defer c.cacheMu.RUnlock()
	nodes, ok := c.fwdCache[id]
	return nodes, ok
}

// getCachedMsg 按 message_id 取回缓存的事件快照
func (c *Caller) getCachedMsg(id int64) (map[string]any, bool) {
	c.cacheMu.RLock()
	defer c.cacheMu.RUnlock()
	snap, ok := c.msgCache[id]
	return snap, ok
}

// SetSelfID 更新 self_id
func (c *Caller) SetSelfID(id int64) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.selfID = id
}

// SetSender 注入 QQ 发送实现（由 qq.Client 在建立 openapi 后调用）
func (c *Caller) SetSender(s MessageSender) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.sender = s
}

// SetGatewayQuerier 注入 /gateway/bot 查询实现（由 qq.Client 在建立 openapi 后调用）
func (c *Caller) SetGatewayQuerier(q GatewayQuerier) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.gateway = q
}

// getGatewayQuerier 读取已注入的网关查询器（可能为 nil）
func (c *Caller) getGatewayQuerier() GatewayQuerier {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.gateway
}

// getSelfID 读取当前 self_id
func (c *Caller) getSelfID() int64 {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.selfID
}

// SetNickname 由 READY 事件回调写入真实机器人用户名
func (c *Caller) SetNickname(name string) {
	c.mu.Lock()
	c.nickname = name
	c.mu.Unlock()
}

// getNickname 返回真实昵称，READY 前回退到 appID
func (c *Caller) getNickname() string {
	c.mu.RLock()
	n := c.nickname
	c.mu.RUnlock()
	if n == "" {
		return c.appID
	}
	return n
}

// getSender 读取已注入的发送通道（可能为 nil）
func (c *Caller) getSender() MessageSender {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.sender
}

// Call 处理 OneBot v11 动作请求
func (c *Caller) Call(action string, params gjson.Result) map[string]any {
	switch action {
	case "get_login_info":
		return ok(map[string]any{
			"user_id":  c.getSelfID(),
			"nickname": c.getNickname(),
		})
	case "get_status":
		return ok(map[string]any{"online": true, "good": true})
	case "get_version_info":
		return ok(map[string]any{
			"app_name":    c.appName,
			"app_version": c.appVersion,
			"protocol_version": map[string]any{
				"major": 1, "minor": 0, "patch": 0,
			},
		})
	case "send_group_msg":
		return c.sendGroup(params, false)
	case "send_private_msg":
		return c.sendPrivate(params, false)
	case "send_msg":
		return c.sendMsg(params, false)
	case "send_group_active_msg":
		return c.sendGroup(params, true)
	case "send_private_active_msg":
		return c.sendPrivate(params, true)
	case "put_interaction", "send_interaction_ack":
		return c.ackInteraction(params)
	case "delete_msg":
		return c.deleteMsg(params)
	case "get_msg":
		return c.getMsg(params)
	case "get_forward_msg":
		return c.getForwardMsg(params)
	case "get_openid":
		return c.getOpenid(params)
	case "get_gateway", "get_gateway_bot", "_get_gateway", "get_session_limit":
		return c.getGateway()
	case "get_group_info":
		return c.getGroupInfo(params)
	case "get_group_bot_state":
		return c.getGroupBotState(params)
	case "get_group_member_info":
		return c.getGroupMemberInfo(params)
	case "get_group_join_requests", "get_group_system_msg":
		return c.getGroupJoinRequests(params)
	case "set_group_add_request":
		return c.setGroupAddRequest(params)
	case "set_group_ban":
		return c.setGroupBan(params)
	case "get_group_ban", "get_group_mute":
		return c.getGroupBan(params)
	case "set_group_whole_ban":
		return failed(1404, "QQ群v2设置禁言接口仅支持成员级，不支持全员禁言")
	case "get_c2c_msg_state", "get_friend_msg_state":
		return c.getC2CMsgState(params)
	case "get_menu":
		return c.getMenu()
	case "set_menu":
		return c.setMenu(params)
	case "get_command_panels":
		return c.getCommandPanels(params)
	case "get_command_panel":
		return c.getCommandPanel(params)
	case "create_command_panel":
		return c.createCommandPanel(params)
	case "set_command_panel":
		return c.setCommandPanel(params)
	case "delete_command_panel":
		return c.deleteCommandPanel(params)
	case "set_command_panel_target":
		return c.setCommandPanelTarget(params)
	default:
		return ok(map[string]any{})
	}
}

// getOpenid 把哈希后的 user_id/group_id 反查回 QQ 原始 openid
func (c *Caller) getOpenid(params gjson.Result) map[string]any {
	data := map[string]any{}
	if uid := params.Get("user_id").Int(); uid != 0 {
		if oid, ok := c.ids.OpenID(uid); ok {
			data["user_openid"] = oid
		}
	}
	if gid := params.Get("group_id").Int(); gid != 0 {
		if oid, ok := c.ids.OpenID(gid); ok {
			data["group_openid"] = oid
		}
	}
	if len(data) == 0 {
		return failed(1404, "未知 id 或未收到过相关消息")
	}
	return ok(data)
}

// getMsg 按 message_id 取回收到时缓存的消息（仅内存）
func (c *Caller) getMsg(params gjson.Result) map[string]any {
	id := params.Get("message_id").Int()
	if id == 0 {
		id = params.Get("message").Int() // 兼容误用 message= 传参
	}
	if id == 0 {
		return failed(1400, "缺少 message_id")
	}
	snap, found := c.getCachedMsg(id)
	if !found {
		return failed(1404, "消息不存在或未缓存")
	}
	return ok(snap)
}

// getForwardMsg 按 forward id 取回合并转发节点（go-cqhttp/NoneBot 兼容 messages 字段）
func (c *Caller) getForwardMsg(params gjson.Result) map[string]any {
	id := strings.TrimSpace(params.Get("id").String())
	if id == "" {
		id = strings.TrimSpace(params.Get("message_id").String())
	}
	if id == "" {
		return failed(1400, "缺少 id")
	}
	nodes, found := c.getForward(id)
	if !found || len(nodes) == 0 {
		return failed(1404, "未知 forward id")
	}
	return ok(map[string]any{"messages": nodes})
}

// resolveGroupOpenID 从 group_id 反查群 openid
func (c *Caller) resolveGroupOpenID(params gjson.Result) (string, map[string]any) {
	gid := params.Get("group_id").Int()
	if gid == 0 {
		return "", failed(1400, "缺少 group_id")
	}
	oid, ok := c.ids.OpenID(gid)
	if !ok || oid == "" {
		return "", failed(1404, "未知 group_id 或未收到过该群消息")
	}
	return oid, nil
}

// getGroupInfo 获取群基本信息（官方白名单接口）
func (c *Caller) getGroupInfo(params gjson.Result) map[string]any {
	groupOpenID, fail := c.resolveGroupOpenID(params)
	if fail != nil {
		return fail
	}
	sender := c.getSender()
	if sender == nil {
		return failed(1404, "发送通道未就绪（QQ 连接尚未初始化）")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	info, err := sender.GetGroupInfo(ctx, groupOpenID)
	if err != nil {
		return failed(1500, err.Error())
	}
	gid := params.Get("group_id").Int()
	return ok(map[string]any{
		"group_id":          gid,
		"group_openid":      info.GroupOpenID,
		"group_name":        info.GroupName,
		"group_memo":        info.GroupFingerMemo,
		"group_finger_memo": info.GroupFingerMemo,
		"group_class_text":  info.GroupClassText,
		"group_tags":        info.GroupTags,
		"member_count":      info.GroupMemberNum,
		"max_member_count":  0,
	})
}

// getC2CMsgState 查询用户 C2C 主动消息推送开关（基于本地事件缓存，非官方接口）
// 入参优先 user_openid，其次 user_id；从未收到过该用户开关事件时 known=false。
func (c *Caller) getC2CMsgState(params gjson.Result) map[string]any {
	openid := params.Get("user_openid").String()
	uid := params.Get("user_id").Int()
	if openid == "" {
		if uid == 0 {
			return failed(1400, "缺少 user_id 或 user_openid")
		}
		oid, found := c.ids.OpenID(uid)
		if !found || oid == "" {
			return failed(1404, "未知 user_id 或未收到过该用户消息")
		}
		openid = oid
	}
	if uid == 0 {
		uid = c.ids.ID(openid)
	}
	st, known := c.ids.GetC2CMsgState(openid)
	data := map[string]any{
		"user_id":     uid,
		"user_openid": openid,
		"known":       known,
	}
	if known {
		data["allow_proactive_msg"] = st.Allow
		data["updated_at"] = st.UpdatedAt
	} else {
		data["allow_proactive_msg"] = nil
		data["updated_at"] = 0
	}
	return ok(data)
}

// getGroupBotState 获取机器人群内状态（官方白名单接口）
func (c *Caller) getGroupBotState(params gjson.Result) map[string]any {
	groupOpenID, fail := c.resolveGroupOpenID(params)
	if fail != nil {
		return fail
	}
	sender := c.getSender()
	if sender == nil {
		return failed(1404, "发送通道未就绪（QQ 连接尚未初始化）")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	st, err := sender.GetGroupBotState(ctx, groupOpenID)
	if err != nil {
		return failed(1500, err.Error())
	}
	return ok(map[string]any{
		"group_id":            params.Get("group_id").Int(),
		"group_openid":        groupOpenID,
		"member_openid":       st.MemberOpenID,
		"joined_at":           st.JoinedAt,
		"allow_proactive_msg": st.AllowProactiveMsg,
		"recv_msg_setting":    st.RecvMsgSetting,
		"member_role":         st.MemberRole,
	})
}

// getGroupMemberInfo 获取群成员信息：仅支持查询机器人自身（走 bot_state）
func (c *Caller) getGroupMemberInfo(params gjson.Result) map[string]any {
	groupOpenID, fail := c.resolveGroupOpenID(params)
	if fail != nil {
		return fail
	}
	uid := params.Get("user_id").Int()
	if uid == 0 {
		return failed(1400, "缺少 user_id")
	}
	if uid != c.getSelfID() {
		return failed(1404, "QQ群v2无获取其他成员信息的接口；仅支持查询机器人自身（user_id=self_id，走 bot_state）")
	}
	sender := c.getSender()
	if sender == nil {
		return failed(1404, "发送通道未就绪（QQ 连接尚未初始化）")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	st, err := sender.GetGroupBotState(ctx, groupOpenID)
	if err != nil {
		return failed(1500, err.Error())
	}
	role := st.MemberRole
	if role == "" {
		role = "member"
	}
	return ok(map[string]any{
		"group_id":              params.Get("group_id").Int(),
		"user_id":               uid,
		"nickname":              c.getNickname(),
		"card":                  "",
		"sex":                   "unknown",
		"age":                   0,
		"area":                  "",
		"join_time":             0,
		"last_sent_time":        0,
		"level":                 "0",
		"role":                  role,
		"unfriendly":            false,
		"title":                 "",
		"title_expire_time":     0,
		"card_changeable":       false,
		"joined_at":             st.JoinedAt,
		"allow_proactive_msg":   st.AllowProactiveMsg,
		"recv_msg_setting":      st.RecvMsgSetting,
		"member_openid":         st.MemberOpenID,
	})
}

// getGroupJoinRequests 拉取入群申请列表（需群管理员）
func (c *Caller) getGroupJoinRequests(params gjson.Result) map[string]any {
	groupOpenID, fail := c.resolveGroupOpenID(params)
	if fail != nil {
		return fail
	}
	sender := c.getSender()
	if sender == nil {
		return failed(1404, "发送通道未就绪（QQ 连接尚未初始化）")
	}
	cursor := params.Get("cursor").String()
	limit := int(params.Get("limit").Int())
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	list, err := sender.GetGroupJoinRequests(ctx, groupOpenID, cursor, limit)
	if err != nil {
		return failed(1500, err.Error())
	}
	items := make([]map[string]any, 0, len(list.List))
	for _, jr := range list.List {
		c.ids.PutJoinRequest(jr.JoinRequestID, idmap.JoinRequestMeta{
			GroupOpenID:   groupOpenID,
			MemberOpenID:  jr.MemberOpenID,
			JoinRequestID: jr.JoinRequestID,
		})
		items = append(items, map[string]any{
			"join_request_id": jr.JoinRequestID,
			"flag":            jr.JoinRequestID,
			"user_id":         c.ids.ID(jr.MemberOpenID),
			"user_openid":     jr.MemberOpenID,
			"union_openid":    jr.UnionOpenID,
			"username":        jr.Username,
			"apply_at":        jr.ApplyAt,
			"apply_source":    jr.ApplySource,
			"invited_by":      jr.InvitedBy,
			"bot":             jr.Bot,
			"comment":         jr.VerifyMessage,
			"verify_method":   jr.VerifyMethod,
			"risk_tips":       jr.RiskTips,
		})
	}
	return ok(map[string]any{
		"group_id":     params.Get("group_id").Int(),
		"group_openid": groupOpenID,
		"list":         items,
		"next_cursor":  list.NextCursor,
	})
}

// setGroupAddRequest 处理加群请求（需群管理员）
func (c *Caller) setGroupAddRequest(params gjson.Result) map[string]any {
	flag := strings.TrimSpace(params.Get("flag").String())
	if flag == "" {
		flag = strings.TrimSpace(params.Get("join_request_id").String())
	}
	if flag == "" {
		return failed(1400, "缺少 flag / join_request_id")
	}
	meta, okMeta := c.ids.GetJoinRequest(flag)
	if !okMeta {
		// 允许调用方显式传入 group_id + user_id 兜底
		groupOpenID := ""
		memberOpenID := ""
		if gid := params.Get("group_id").Int(); gid != 0 {
			if oid, ok := c.ids.OpenID(gid); ok {
				groupOpenID = oid
			}
		}
		if uid := params.Get("user_id").Int(); uid != 0 {
			if oid, ok := c.ids.OpenID(uid); ok {
				memberOpenID = oid
			}
		}
		if groupOpenID == "" || memberOpenID == "" {
			return failed(1404, "未知 flag：未收到对应入群申请事件，且未提供可反查的 group_id/user_id")
		}
		meta = idmap.JoinRequestMeta{
			GroupOpenID:   groupOpenID,
			MemberOpenID:  memberOpenID,
			JoinRequestID: flag,
		}
	}
	approve := true
	if params.Get("approve").Exists() {
		approve = params.Get("approve").Bool()
	}
	op := "approve"
	if !approve {
		op = "decline"
	}
	reason := params.Get("reason").String()
	if reason == "" {
		reason = params.Get("reject_reason").String()
	}
	blacklist := params.Get("add_to_member_blacklist").Bool()
	if params.Get("blacklist").Exists() {
		blacklist = params.Get("blacklist").Bool()
	}
	sender := c.getSender()
	if sender == nil {
		return failed(1404, "发送通道未就绪（QQ 连接尚未初始化）")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if err := sender.ApproveGroupJoinRequest(ctx, meta.GroupOpenID, meta.MemberOpenID, meta.JoinRequestID, op, reason, blacklist); err != nil {
		return failed(1500, err.Error())
	}
	c.ids.DeleteJoinRequest(flag)
	return ok(map[string]any{})
}

// setGroupBan 设置/解除群成员禁言（需群管理员）
func (c *Caller) setGroupBan(params gjson.Result) map[string]any {
	groupOpenID, fail := c.resolveGroupOpenID(params)
	if fail != nil {
		return fail
	}
	uid := params.Get("user_id").Int()
	if uid == 0 {
		return failed(1400, "缺少 user_id")
	}
	memberOpenID, okOID := c.ids.OpenID(uid)
	if !okOID || memberOpenID == "" {
		return failed(1404, "未知 user_id 或未收到过该用户消息")
	}
	duration := params.Get("duration").Int() // 秒；0=解除
	op := onebotMuteOp("add", memberOpenID, duration)
	if duration <= 0 {
		op = onebotMuteOp("del", memberOpenID, 0)
	}
	sender := c.getSender()
	if sender == nil {
		return failed(1404, "发送通道未就绪（QQ 连接尚未初始化）")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if err := sender.SetGroupMute(ctx, groupOpenID, []MuteOp{op}); err != nil {
		return failed(1500, err.Error())
	}
	return ok(map[string]any{})
}

// onebotMuteOp 构造禁言操作
func onebotMuteOp(op, memberOpenID string, durationSec int64) MuteOp {
	m := MuteOp{Op: op, MemberOpenID: memberOpenID}
	if op == "add" || op == "update" {
		m.MuteExpireAt = time.Now().Add(time.Duration(durationSec) * time.Second).Format(time.RFC3339)
	}
	return m
}

// getGroupBan 查询群禁言状态（需群管理员）
func (c *Caller) getGroupBan(params gjson.Result) map[string]any {
	groupOpenID, fail := c.resolveGroupOpenID(params)
	if fail != nil {
		return fail
	}
	sender := c.getSender()
	if sender == nil {
		return failed(1404, "发送通道未就绪（QQ 连接尚未初始化）")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	st, err := sender.GetGroupMute(ctx, groupOpenID)
	if err != nil {
		return failed(1500, err.Error())
	}
	members := make([]map[string]any, 0, len(st.Members))
	for _, m := range st.Members {
		members = append(members, map[string]any{
			"user_id":         c.ids.ID(m.MemberOpenID),
			"user_openid":     m.MemberOpenID,
			"mute_expire_at":  m.MuteExpireAt,
			"username":        m.Username,
			"union_openid":    m.UnionOpenID,
		})
	}
	return ok(map[string]any{
		"group_id":     params.Get("group_id").Int(),
		"group_openid": groupOpenID,
		"global_mode":  st.GlobalMode,
		"members":      members,
	})
}

// ---- 自定义菜单与指令面板 ----

// okJSON 把任意可 JSON 序列化的值转为 OneBot 成功响应 data（map）
func okJSON(v any) map[string]any {
	b, err := json.Marshal(v)
	if err != nil {
		return failed(1500, "序列化响应失败: "+err.Error())
	}
	var data map[string]any
	if err := json.Unmarshal(b, &data); err != nil {
		return failed(1500, "解析响应失败: "+err.Error())
	}
	if data == nil {
		data = map[string]any{}
	}
	return ok(data)
}

// requireSender 取发送通道；未就绪时返回失败响应
func (c *Caller) requireSender() (MessageSender, map[string]any) {
	sender := c.getSender()
	if sender == nil {
		return nil, failed(1404, "发送通道未就绪（QQ 连接尚未初始化）")
	}
	return sender, nil
}

// getMenu 查询全局自定义菜单
func (c *Caller) getMenu() map[string]any {
	sender, fail := c.requireSender()
	if fail != nil {
		return fail
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	resp, err := sender.GetMenu(ctx)
	if err != nil {
		return failed(1500, err.Error())
	}
	return okJSON(resp)
}

// setMenu 修改全局自定义菜单（整体覆盖）
func (c *Caller) setMenu(params gjson.Result) map[string]any {
	sender, fail := c.requireSender()
	if fail != nil {
		return fail
	}
	var req dto.MenuPutReq
	if err := json.Unmarshal([]byte(params.Raw), &req); err != nil {
		return failed(1400, "参数解析失败: "+err.Error())
	}
	// 兼容 params.menu 或直接传 items
	if len(req.Menu.Items) == 0 {
		if items := params.Get("items"); items.Exists() {
			_ = json.Unmarshal([]byte(items.Raw), &req.Menu.Items)
		}
	}
	if len(req.Menu.Items) == 0 {
		return failed(1400, "缺少 menu.items")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	resp, err := sender.SetMenu(ctx, &req)
	if err != nil {
		return failed(1500, err.Error())
	}
	return okJSON(resp)
}

// getCommandPanels 查询指令面板列表
func (c *Caller) getCommandPanels(params gjson.Result) map[string]any {
	sender, fail := c.requireSender()
	if fail != nil {
		return fail
	}
	scope := strings.TrimSpace(params.Get("scope").String())
	if scope == "" {
		return failed(1400, "缺少 scope（c2c/group/channel/dm）")
	}
	pager := &dto.PanelListPager{
		Scope:  scope,
		Limit:  int(params.Get("limit").Int()),
		Cursor: params.Get("cursor").String(),
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	resp, err := sender.ListCommandPanels(ctx, pager)
	if err != nil {
		return failed(1500, err.Error())
	}
	return okJSON(resp)
}

// getCommandPanel 查询指令面板详情
func (c *Caller) getCommandPanel(params gjson.Result) map[string]any {
	sender, fail := c.requireSender()
	if fail != nil {
		return fail
	}
	panelID := strings.TrimSpace(params.Get("panel_id").String())
	if panelID == "" {
		return failed(1400, "缺少 panel_id")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	resp, err := sender.GetCommandPanel(ctx, panelID)
	if err != nil {
		return failed(1500, err.Error())
	}
	return okJSON(resp)
}

// createCommandPanel 创建指令面板
func (c *Caller) createCommandPanel(params gjson.Result) map[string]any {
	sender, fail := c.requireSender()
	if fail != nil {
		return fail
	}
	var req dto.PanelCreateReq
	if err := json.Unmarshal([]byte(params.Raw), &req); err != nil {
		return failed(1400, "参数解析失败: "+err.Error())
	}
	if strings.TrimSpace(req.Scope) == "" {
		return failed(1400, "缺少 scope（c2c/group/channel/dm）")
	}
	if len(req.Panel.Items) == 0 {
		return failed(1400, "缺少 panel.items")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	resp, err := sender.CreateCommandPanel(ctx, &req)
	if err != nil {
		return failed(1500, err.Error())
	}
	return okJSON(resp)
}

// setCommandPanel 修改指令面板（整体覆盖）
func (c *Caller) setCommandPanel(params gjson.Result) map[string]any {
	sender, fail := c.requireSender()
	if fail != nil {
		return fail
	}
	panelID := strings.TrimSpace(params.Get("panel_id").String())
	if panelID == "" {
		return failed(1400, "缺少 panel_id")
	}
	var req dto.PanelPutReq
	if err := json.Unmarshal([]byte(params.Raw), &req); err != nil {
		return failed(1400, "参数解析失败: "+err.Error())
	}
	if len(req.Panel.Items) == 0 {
		return failed(1400, "缺少 panel.items")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	resp, err := sender.SetCommandPanel(ctx, panelID, &req)
	if err != nil {
		return failed(1500, err.Error())
	}
	return okJSON(resp)
}

// deleteCommandPanel 删除指令面板
func (c *Caller) deleteCommandPanel(params gjson.Result) map[string]any {
	sender, fail := c.requireSender()
	if fail != nil {
		return fail
	}
	panelID := strings.TrimSpace(params.Get("panel_id").String())
	if panelID == "" {
		return failed(1400, "缺少 panel_id")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if err := sender.DeleteCommandPanel(ctx, panelID); err != nil {
		return failed(1500, err.Error())
	}
	return ok(map[string]any{})
}

// setCommandPanelTarget 修改指令面板关联对象
func (c *Caller) setCommandPanelTarget(params gjson.Result) map[string]any {
	sender, fail := c.requireSender()
	if fail != nil {
		return fail
	}
	panelID := strings.TrimSpace(params.Get("panel_id").String())
	if panelID == "" {
		return failed(1400, "缺少 panel_id")
	}
	var req dto.PanelTargetReq
	if err := json.Unmarshal([]byte(params.Raw), &req); err != nil {
		return failed(1400, "参数解析失败: "+err.Error())
	}
	if strings.TrimSpace(req.Op) == "" {
		return failed(1400, "缺少 op（add/del）")
	}
	if len(req.UserOpenids) == 0 && len(req.GroupOpenids) == 0 {
		return failed(1400, "缺少 user_openids 或 group_openids")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if err := sender.SetCommandPanelTarget(ctx, panelID, &req); err != nil {
		return failed(1500, err.Error())
	}
	return ok(map[string]any{})
}

// getGateway 查询 /gateway/bot，返回 ws 接入点与连接频控（剩余可建连次数等）
func (c *Caller) getGateway() map[string]any {
	q := c.getGatewayQuerier()
	if q == nil {
		return failed(1404, "gateway 查询器未就绪（QQ 连接尚未初始化）")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	info, err := q(ctx)
	if err != nil {
		return failed(1500, err.Error())
	}
	return ok(map[string]any{
		"url":             info.URL,
		"shards":          info.Shards,
		"total":           info.Total,
		"remaining":       info.Remaining,
		"reset_after":     info.ResetAfter,
		"max_concurrency": info.MaxConcurrency,
	})
}

// logSend 渲染一条"发消息"边框块（成功绿/失败红，含模式与耗时）
func logSend(action, sceneLabel string, sceneID int64, out *OutMessage, qqMsgID string, mid int64, start time.Time, err error) {
	tone := prettylog.ToneSend
	if err != nil {
		tone = prettylog.ToneErr
	}
	title := "发消息 · " + action
	if sceneLabel != "" {
		title += " · " + sceneLabel + " " + strconv.FormatInt(sceneID, 10)
	}
	blk := prettylog.New(title, tone)
	if out != nil {
		blk.Row("段", partSummary(out))
		blk.Row("模式", sendMode(out))
	}
	elapsed := time.Since(start).Round(time.Millisecond).String()
	if err != nil {
		blk.Row("结果", prettylog.Fail("✘ 失败 "+err.Error())+prettylog.Dim("  耗时 "+elapsed))
	} else {
		res := prettylog.Ok(fmt.Sprintf("✔ 成功  message_id=%d", mid))
		if qqMsgID != "" {
			res += prettylog.Dim("  QQ " + midEllipsis(qqMsgID, 14, 8))
		}
		res += prettylog.Dim("  耗时 " + elapsed)
		blk.Row("结果", res)
	}
	blk.Emit()
}

// partSummary 汇总待发送消息段，如 'text("ok"), image, markdown'
func partSummary(out *OutMessage) string {
	if out == nil || len(out.Parts) == 0 {
		return "(空)"
	}
	parts := make([]string, 0, len(out.Parts))
	for _, p := range out.Parts {
		switch p.Kind {
		case PartText:
			parts = append(parts, fmt.Sprintf("text(%q)", clip(p.Text, 30)))
		case PartImage:
			parts = append(parts, "image")
		case PartMarkdown:
			parts = append(parts, "markdown")
		}
	}
	return strings.Join(parts, ", ")
}

// sendMode 描述发送模式：被动(msg_id/event)/互动召回/主动，并标注是否带 message_reference 引用。
func sendMode(out *OutMessage) string {
	var base string
	switch {
	case out.Stream != nil:
		base = fmt.Sprintf("流式(state=%d,index=%d)", out.Stream.State, out.Stream.Index)
		if out.Stream.ID != "" {
			base += " id=" + midEllipsis(out.Stream.ID, 14, 8)
		}
		return base
	case out.IsWakeup:
		base = "互动召回(is_wakeup)"
	case out.EventID != "":
		base = "被动(event) " + midEllipsis(out.EventID, 14, 8)
	case out.MsgID != "":
		base = "被动 msg_id=" + midEllipsis(out.MsgID, 14, 8)
	default:
		base = "主动"
	}
	if out.RefMsgID != "" {
		base += " +引用=" + midEllipsis(out.RefMsgID, 14, 8)
	}
	return base
}

// ok 构造 OneBot 成功响应（status=ok, retcode=0）
func ok(data map[string]any) map[string]any {
	return map[string]any{"status": "ok", "retcode": 0, "data": data}
}

// failed 构造 OneBot 失败响应（status=failed，附错误码与文案）
func failed(retcode int, msg string) map[string]any {
	return map[string]any{"status": "failed", "retcode": retcode, "data": nil, "message": msg, "wording": msg}
}

// sendMsg 按 message_type 分派
func (c *Caller) sendMsg(params gjson.Result, active bool) map[string]any {
	mt := params.Get("message_type").Str
	if mt == "" {
		if params.Get("group_id").Exists() && params.Get("group_id").Raw != "0" && params.Get("group_id").Str != "0" {
			mt = "group"
		} else {
			mt = "private"
		}
	}
	if mt == "group" {
		return c.sendGroup(params, active)
	}
	return c.sendPrivate(params, active)
}

// sendGroup 发群消息：解析 message、按需套用被动 msg_id/event_id（active 时强制主动），
// 调用 QQ 发送并分配 message_id，打印发消息日志块
func (c *Caller) sendGroup(params gjson.Result, active bool) map[string]any {
	sender := c.getSender()
	if sender == nil {
		return failed(1404, "QQ 发送通道未就绪")
	}
	groupID := params.Get("group_id").Int()
	groupOpenID, okID := c.ids.OpenID(groupID)
	if !okID {
		return failed(1400, "未知 group_id（尚未收到该群消息）")
	}
	out := c.parseMessage(params.Get("message"))
	if active {
		out.MsgID = "" // 强制主动：忽略 reply、不套 lazy
	} else if out.MsgID == "" {
		userOpenID := ""
		if uid := params.Get("user_id").Int(); uid != 0 {
			userOpenID, _ = c.ids.OpenID(uid)
		}
		if id, kind, found := c.ids.LazyGroup(groupOpenID, userOpenID); found {
			if kind == "event" {
				out.EventID = id
			} else {
				out.MsgID = id
			}
		}
	}
	if !out.HasContent() {
		return failed(1400, "空消息")
	}
	action := "send_group_msg"
	if active {
		action = "send_group_active_msg"
	}
	start := time.Now()
	ctx, cancel := context.WithTimeout(context.Background(), sendTimeout(out))
	defer cancel()
	qqMsgID, err := sender.SendGroup(ctx, groupOpenID, out)
	if err != nil {
		logSend(action, "群", groupID, out, "", 0, start, err)
		return failed(1500, err.Error())
	}
	stats.RecordSent()
	mid := c.ids.AllocMsgID(idmap.MsgMeta{
		MsgID: qqMsgID, GroupOpenID: groupOpenID, Scene: idmap.SceneGroup, Time: time.Now(),
	})
	logSend(action, "群", groupID, out, qqMsgID, mid, start, nil)
	return ok(map[string]any{"message_id": mid})
}

// sendTimeout 按消息里的富媒体体积估算发送整体超时。
// 纯文本/小图仍是 30s 快速失败；带本地大文件（分片上传）时按体积放宽，
// 保守速率 512KB/s 折算上传耗时，封顶 30 分钟，避免大视频被 30s 硬截断。
func sendTimeout(out *OutMessage) time.Duration {
	const base = 30 * time.Second
	var mediaBytes int64
	for i := range out.Parts {
		p := &out.Parts[i]
		switch p.Kind {
		case PartImage:
			mediaBytes += int64(len(p.Image.Base64)) * 3 / 4
		case PartVideo, PartRecord, PartFile:
			mediaBytes += int64(len(p.Media.Base64)) * 3 / 4
		}
	}
	if mediaBytes <= 0 {
		return base
	}
	d := base + time.Duration(mediaBytes/(512*1024))*time.Second
	if max := 30 * time.Minute; d > max {
		d = max
	}
	return d
}

// sendPrivate 发私聊(C2C)消息：逻辑同 sendGroup，按 user_id 定位 openid 与被动凭证
func (c *Caller) sendPrivate(params gjson.Result, active bool) map[string]any {
	sender := c.getSender()
	if sender == nil {
		return failed(1404, "QQ 发送通道未就绪")
	}
	userID := params.Get("user_id").Int()
	userOpenID, okID := c.ids.OpenID(userID)
	if !okID {
		return failed(1400, "未知 user_id（尚未收到该用户消息）")
	}
	out := c.parseMessage(params.Get("message"))
	// is_wakeup：单聊互动召回消息，与 msg_id/event_id 互斥，故强制走主动语义（不套被动凭证）
	wakeup := params.Get("is_wakeup").Bool()
	if wakeup {
		out.IsWakeup = true
		out.MsgID = ""
		out.EventID = ""
	} else if active {
		out.MsgID = "" // 强制主动：忽略 reply、不套 lazy
	} else if out.MsgID == "" {
		if id, kind, found := c.ids.LazyC2C(userOpenID); found {
			if kind == "event" {
				out.EventID = id
			} else {
				out.MsgID = id
			}
		}
	}
	if !out.HasContent() {
		return failed(1400, "空消息")
	}
	action := "send_private_msg"
	if wakeup {
		action = "send_private_wakeup_msg"
	} else if active {
		action = "send_private_active_msg"
	}
	start := time.Now()
	ctx, cancel := context.WithTimeout(context.Background(), sendTimeout(out))
	defer cancel()
	qqMsgID, err := sender.SendC2C(ctx, userOpenID, out)
	if err != nil {
		logSend(action, "私聊", userID, out, "", 0, start, err)
		return failed(1500, err.Error())
	}
	stats.RecordSent()
	mid := c.ids.AllocMsgID(idmap.MsgMeta{
		MsgID: qqMsgID, UserOpenID: userOpenID, Scene: idmap.SceneC2C, Time: time.Now(),
	})
	logSend(action, "私聊", userID, out, qqMsgID, mid, start, nil)
	data := map[string]any{"message_id": mid}
	if out.Stream != nil {
		// 流式：把原始 QQ 消息 id 作为 stream_id 回传，供插件在后续分片的 stream.id 里回填
		data["stream_id"] = qqMsgID
	}
	return ok(data)
}

// ackInteraction 回执互动事件（按钮点击），由 NoneBot 决定 code（默认 0=成功）
func (c *Caller) ackInteraction(params gjson.Result) map[string]any {
	sender := c.getSender()
	if sender == nil {
		return failed(1404, "QQ 发送通道未就绪")
	}
	id := params.Get("interaction_id").String()
	if id == "" {
		return failed(1400, "缺少 interaction_id")
	}
	code := int(params.Get("code").Int())
	start := time.Now()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	err := sender.AckInteraction(ctx, id, code)
	tone := prettylog.ToneSend
	if err != nil {
		tone = prettylog.ToneErr
	}
	blk := prettylog.New("互动回执 · put_interaction", tone).
		Row("interaction", midEllipsis(id, 14, 8)).
		Row("code", strconv.Itoa(code))
	if err != nil {
		blk.Row("结果", prettylog.Fail("✘ 失败 "+err.Error())+prettylog.Dim("  耗时 "+time.Since(start).Round(time.Millisecond).String()))
		blk.Emit()
		return failed(1500, err.Error())
	}
	blk.Row("结果", prettylog.Ok("✔ 成功")+prettylog.Dim("  耗时 "+time.Since(start).Round(time.Millisecond).String()))
	blk.Emit()
	return ok(map[string]any{})
}

// deleteMsg 撤回消息：按 message_id 查回 QQ msg_id 与场景，调用对应撤回接口
func (c *Caller) deleteMsg(params gjson.Result) map[string]any {
	sender := c.getSender()
	if sender == nil {
		return failed(1404, "QQ 发送通道未就绪")
	}
	meta, found := c.ids.Msg(params.Get("message_id").Int())
	if !found || meta.MsgID == "" {
		return failed(1400, "未知 message_id")
	}
	start := time.Now()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	var err error
	if meta.Scene == idmap.SceneGroup {
		err = sender.RetractGroup(ctx, meta.GroupOpenID, meta.MsgID)
	} else {
		err = sender.RetractC2C(ctx, meta.UserOpenID, meta.MsgID)
	}
	tone := prettylog.ToneSend
	if err != nil {
		tone = prettylog.ToneErr
	}
	blk := prettylog.New("撤回 · delete_msg", tone).
		Row("message_id", strconv.FormatInt(params.Get("message_id").Int(), 10)).
		Row("QQ", midEllipsis(meta.MsgID, 14, 8))
	if err != nil {
		blk.Row("结果", prettylog.Fail("✘ 失败 "+err.Error()))
		blk.Emit()
		return failed(1500, err.Error())
	}
	blk.Row("结果", prettylog.Ok("✔ 成功")+prettylog.Dim("  耗时 "+time.Since(start).Round(time.Millisecond).String()))
	blk.Emit()
	return ok(map[string]any{})
}

// cqRE 匹配 CQ 码段 [CQ:type,k=v,...]，用于解析 CQ 字符串形式的消息
var cqRE = regexp.MustCompile(`\[CQ:([a-zA-Z0-9_-]+)((?:,[^,\]]+)*)\]`)

// cqUnescape 还原 CQ 码转义字符
func cqUnescape(s string) string {
	r := strings.NewReplacer("&#91;", "[", "&#93;", "]", "&#44;", ",", "&amp;", "&")
	return r.Replace(s)
}

// parseMessage 解析 OneBot message（array 或 CQ 字符串）为 OutMessage。
// 按 NoneBot 发来的段顺序构建 Parts：同类连续文本/at 合并为一个 PartText，
// 遇到 image/markdown 段时先 flush 文本缓冲，再追加对应 Part。
func (c *Caller) parseMessage(msg gjson.Result) *OutMessage {
	out := &OutMessage{}
	var text strings.Builder
	flushText := func() {
		if text.Len() > 0 {
			out.Parts = append(out.Parts, MessagePart{Kind: PartText, Text: text.String()})
			text.Reset()
		}
	}
	addSeg := func(typ string, data gjson.Result) {
		switch typ {
		case "text":
			text.WriteString(data.Get("text").String())
		case "at":
			qq := data.Get("qq").String()
			if qq == "" || qq == "all" {
				return
			}
			id, err := strconv.ParseInt(qq, 10, 64)
			if err != nil || id == c.getSelfID() {
				return
			}
			if openid, found := c.ids.OpenID(id); found {
				text.WriteString("<@")
				text.WriteString(openid)
				text.WriteString(">")
			}
		case "image":
			img := parseImageData(data.Get("file").String(), data.Get("url").String())
			if img != nil {
				flushText()
				out.Parts = append(out.Parts, MessagePart{Kind: PartImage, Image: *img})
			}
		case "video":
			if m := parseMediaData(data.Get("file").String(), data.Get("url").String(), data.Get("name").String()); m != nil {
				flushText()
				out.Parts = append(out.Parts, MessagePart{Kind: PartVideo, Media: *m})
			}
		case "record", "voice", "audio":
			if m := parseMediaData(data.Get("file").String(), data.Get("url").String(), data.Get("name").String()); m != nil {
				flushText()
				out.Parts = append(out.Parts, MessagePart{Kind: PartRecord, Media: *m})
			}
		case "file":
			if m := parseMediaData(data.Get("file").String(), data.Get("url").String(), data.Get("name").String()); m != nil {
				flushText()
				out.Parts = append(out.Parts, MessagePart{Kind: PartFile, Media: *m})
			}
		case "markdown":
			if md, kb := parseMarkdownSeg(data); md != nil {
				flushText()
				out.Parts = append(out.Parts, MessagePart{Kind: PartMarkdown, Markdown: md, Keyboard: kb})
			}
		case "stream":
			// 流式消息（仅 C2C）：state 1正文生成中/10正文结束/11引导生成中/20引导结束；
			// 首帧不填 id，后续帧回传首帧返回的 stream_id，index 从 1 递增。分片文本走相邻 text 段。
			out.Stream = &dto.Stream{
				State: int32(data.Get("state").Int()),
				ID:    data.Get("id").String(),
				Index: int32(data.Get("index").Int()),
				Reset: data.Get("reset").Bool(),
			}
		case "reply":
			// NoneBot MessageSegment.reply 的 id 多为字符串；兼容数字
			rid := data.Get("id").Int()
			if rid == 0 {
				if s := strings.TrimSpace(data.Get("id").String()); s != "" {
					rid, _ = strconv.ParseInt(s, 10, 64)
				}
			}
			if rid == 0 {
				filelog.Warnf("[onebot] reply 段缺少有效 id，原始 data=%s", data.Raw)
				break
			}
			if meta, found := c.ids.Msg(rid); found && meta.MsgID != "" {
				// 被动 msg_id（ROBOT1.0_）占用 5 分钟被动回复窗口；
				// message_reference 须用 REFIDX 引用索引令牌（来自入站 msg_idx）才能在群/单聊渲染「引用」气泡，
				// 用 ROBOT1.0_ 消息 id 平台虽返回 200 但不渲染（对齐 nonebot-adapter-qq 实现）。
				out.MsgID = meta.MsgID
				out.RefMsgID = meta.RefIdx
				if meta.RefIdx != "" {
					log.Printf("[onebot] reply 命中缓存 message_id=%d → 被动 msg_id=%s，引用 REFIDX=%s", rid, meta.MsgID, meta.RefIdx)
				} else {
					log.Printf("[onebot] reply 命中缓存 message_id=%d → 被动 msg_id=%s，但无 REFIDX（该消息未带 msg_idx，无法渲染引用气泡）", rid, meta.MsgID)
				}
			} else {
				filelog.Warnf("[onebot] reply 段 message_id=%d 未命中本机缓存，无法引用（将退化为主动消息）", rid)
			}
		}
	}

	if msg.IsArray() {
		for _, seg := range msg.Array() {
			addSeg(seg.Get("type").String(), seg.Get("data"))
		}
	} else {
		// CQ 字符串：交替提取 CQ 段与普通文本
		raw := msg.String()
		idx := 0
		for _, loc := range cqRE.FindAllStringSubmatchIndex(raw, -1) {
			if loc[0] > idx {
				text.WriteString(cqUnescape(raw[idx:loc[0]]))
			}
			typ := raw[loc[2]:loc[3]]
			data := parseCQParams(raw[loc[4]:loc[5]])
			addSeg(typ, gjson.Parse(mustJSON(data)))
			idx = loc[1]
		}
		if idx < len(raw) {
			text.WriteString(cqUnescape(raw[idx:]))
		}
	}
	flushText()
	return out
}

// parseMarkdownSeg 解析 markdown 段的 data.data（见 src/markdown_segment.py 构建格式）。
// 支持模板 markdown(custom_template_id+params) 与原生 markdown(content)，
// keyboard 来源优先级：data.keyboard > 顶层 rows。
// 兼容 data.data 为 base64:// 字符串的形式。
func parseMarkdownSeg(data gjson.Result) (*dto.Markdown, *keyboard.MessageKeyboard) {
	inner := data.Get("data")
	var raw string
	if inner.Type == gjson.String {
		s := inner.String()
		if strings.HasPrefix(s, "base64://") {
			if b, err := base64.StdEncoding.DecodeString(strings.TrimPrefix(s, "base64://")); err == nil {
				raw = string(b)
			} else {
				return nil, nil
			}
		} else {
			raw = s
		}
	} else if inner.Exists() {
		raw = inner.Raw
	} else {
		return nil, nil
	}

	var tmp struct {
		Markdown *dto.Markdown             `json:"markdown"`
		Keyboard *keyboard.MessageKeyboard `json:"keyboard"`
		Rows     []*keyboard.Row           `json:"rows"`
	}
	if err := json.Unmarshal([]byte(raw), &tmp); err != nil {
		filelog.Warnf("[onebot] 解析 markdown 段失败: %v", err)
		return nil, nil
	}
	if tmp.Markdown == nil {
		return nil, nil
	}

	kb := tmp.Keyboard
	if kb == nil && len(tmp.Rows) > 0 {
		kb = &keyboard.MessageKeyboard{Content: &keyboard.CustomKeyboard{Rows: tmp.Rows}}
	}
	return tmp.Markdown, kb
}

// parseImageData 按 file/url 判别图片来源（base64://、http(s)、file://、本地绝对路径）并构造 OutImage
func parseImageData(file, url string) *OutImage {
	src := file
	if src == "" {
		src = url
	}
	if src == "" {
		return nil
	}
	switch {
	case strings.HasPrefix(src, "base64://"):
		return &OutImage{Base64: strings.TrimPrefix(src, "base64://")}
	case strings.HasPrefix(src, "http://"), strings.HasPrefix(src, "https://"):
		return &OutImage{URL: src}
	case strings.HasPrefix(src, "file://"):
		return readLocalImage(strings.TrimPrefix(src, "file://"))
	case strings.HasPrefix(src, "/"):
		return readLocalImage(src)
	default:
		if url != "" && (strings.HasPrefix(url, "http://") || strings.HasPrefix(url, "https://")) {
			return &OutImage{URL: url}
		}
		return nil
	}
}

// readLocalImage 读取本地图片文件并 base64 编码（走 FileData 直传，无需图床）
func readLocalImage(path string) *OutImage {
	// file:///abs 形式 TrimPrefix 后可能仍带前导 //，规整为 /
	for strings.HasPrefix(path, "//") {
		path = path[1:]
	}
	data, err := os.ReadFile(path)
	if err != nil {
		filelog.Errorf("[onebot] 读取本地图片失败 %s: %v", path, err)
		return nil
	}
	return &OutImage{Base64: base64.StdEncoding.EncodeToString(data)}
}

// parseMediaData 解析视频/语音/文件段的来源（base64://、http(s)、file://、本地绝对路径）为 OutMedia。
// name 为下游显式文件名，缺省时从 URL/路径的 basename 兜底（供 file_type=4 透传/分片上传）。
func parseMediaData(file, url, name string) *OutMedia {
	src := file
	if src == "" {
		src = url
	}
	if src == "" {
		return nil
	}
	m := &OutMedia{FileName: name}
	switch {
	case strings.HasPrefix(src, "base64://"):
		m.Base64 = strings.TrimPrefix(src, "base64://")
	case strings.HasPrefix(src, "http://"), strings.HasPrefix(src, "https://"):
		m.URL = src
		if m.FileName == "" {
			m.FileName = fileNameFromURL(src)
		}
	case strings.HasPrefix(src, "file://"):
		path := strings.TrimPrefix(src, "file://")
		for strings.HasPrefix(path, "//") {
			path = path[1:]
		}
		data, err := os.ReadFile(path)
		if err != nil {
			filelog.Errorf("[onebot] 读取本地富媒体失败 %s: %v", path, err)
			return nil
		}
		m.Base64 = base64.StdEncoding.EncodeToString(data)
		if m.FileName == "" {
			m.FileName = filepath.Base(path)
		}
	case strings.HasPrefix(src, "/"):
		data, err := os.ReadFile(src)
		if err != nil {
			filelog.Errorf("[onebot] 读取本地富媒体失败 %s: %v", src, err)
			return nil
		}
		m.Base64 = base64.StdEncoding.EncodeToString(data)
		if m.FileName == "" {
			m.FileName = filepath.Base(src)
		}
	default:
		if url != "" && (strings.HasPrefix(url, "http://") || strings.HasPrefix(url, "https://")) {
			m.URL = url
			if m.FileName == "" {
				m.FileName = fileNameFromURL(url)
			}
		} else {
			return nil
		}
	}
	return m
}

// fileNameFromURL 从 URL 路径尾段取文件名（去掉 query），无法判定时返回空串
func fileNameFromURL(u string) string {
	s := u
	if i := strings.IndexAny(s, "?#"); i >= 0 {
		s = s[:i]
	}
	s = strings.TrimRight(s, "/")
	base := s[strings.LastIndex(s, "/")+1:]
	if base == "" || !strings.Contains(base, ".") {
		return ""
	}
	return base
}

// parseCQParams 解析 ",k=v,k=v" 为 map（值做 CQ 反转义）
func parseCQParams(s string) map[string]string {
	m := map[string]string{}
	for _, part := range strings.Split(s, ",") {
		if part == "" {
			continue
		}
		if k, v, ok := strings.Cut(part, "="); ok {
			m[k] = cqUnescape(v)
		}
	}
	return m
}

// mustJSON 把字符串 map 序列化为 JSON（供 CQ 段参数转 gjson 复用 addSeg 逻辑）
func mustJSON(m map[string]string) string {
	b, _ := json.Marshal(m)
	return string(b)
}

// HandleRequest 解析 WS 动作请求并返回 JSON 响应
func (c *Caller) HandleRequest(payload []byte) []byte {
	j := gjson.ParseBytes(payload)
	action := strings.TrimSuffix(j.Get("action").Str, "_async")
	params := j.Get("params")
	if c.debug {
		p := params.Raw
		if len(p) > 500 {
			p = p[:500] + "..."
		}
		_, _ = os.Stderr.WriteString(filelog.FormatInfo(fmt.Sprintf(
			"[onebot] 收到动作 action=%s params=%s echo=%v",
			action, p, j.Get("echo").Value(),
		)))
	}
	if filelog.Enabled() {
		filelog.WriteFile([]byte(filelog.FormatInfo(fmt.Sprintf(
			"[onebot] action=%s params=%s echo=%v",
			action, params.Raw, j.Get("echo").Value(),
		))))
	}
	ret := c.Call(action, params)
	if echo := j.Get("echo"); echo.Exists() {
		ret["echo"] = echo.Value()
	}
	b, _ := json.Marshal(ret)
	return b
}
