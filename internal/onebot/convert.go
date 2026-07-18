// Package onebot 实现 OneBot v11 协议层：把 QQ 上游事件转换成 OneBot 事件
// （convert.go），把下游动作翻译成 QQ 接口调用（api.go），并提供事件总线
// （event.go）与正向/反向 WebSocket 收发（server.go / client.go）。
package onebot

import (
	"context"
	"encoding/json"
	"fmt"
	"hash/fnv"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"goqbot/internal/botgo/dto"

	"goqbot/internal/idmap"
	"goqbot/internal/prettylog"
	"goqbot/internal/stats"
)

// seenMsgLimit 群消息 qq_msg_id 去重缓存上限（防 GROUP_AT 与 GROUP_MESSAGE 重复上报）
const seenMsgLimit = 5000

var atOpenIDRE = regexp.MustCompile(`<@!?[^>]+>`)

// Converter 把 QQ 事件转换为 OneBot v11 事件并推入 EventBus
type Converter struct {
	ids        *idmap.Store
	bus        *EventBus
	selfID     func() int64
	addAtGroup bool
	msgFormat  string  // array 或 string
	downtime   string  // 全下游掉线时回发 QQ 的兜底文本，空=关闭
	caller     *Caller // 用于兜底时直接调用 QQ 发送

	seenMu    sync.Mutex
	seen      map[string]struct{}
	seenOrder []string
}

// NewConverter 创建转换器
func NewConverter(ids *idmap.Store, bus *EventBus, selfID func() int64, addAtGroup bool, msgFormat, downtime string, caller *Caller) *Converter {
	if msgFormat == "" {
		msgFormat = "array"
	}
	return &Converter{
		ids:        ids,
		bus:        bus,
		selfID:     selfID,
		addAtGroup: addAtGroup,
		msgFormat:  msgFormat,
		downtime:   downtime,
		caller:     caller,
		seen:       make(map[string]struct{}),
	}
}

// Segment OneBot v11 消息段，序列化为 {"type":"at","data":{"qq":"123"}}
type Segment struct {
	Type string         `json:"type"`
	Data map[string]any `json:"data"`
}

// Message OneBot v11 消息段数组（主用格式）
type Message []Segment

// MarshalJSON 始终序列化为 array；空消息输出 [] 而非 null
func (m Message) MarshalJSON() ([]byte, error) {
	if len(m) == 0 {
		return []byte("[]"), nil
	}
	return json.Marshal([]Segment(m))
}

// messageField 返回 message 字段值，始终返回段数组（OneBot v11 标准要求）。
// 若需 CQ 字符串，raw_message 字段始终包含；配置 message_format=string 时额外输出 message_cq 字段。
func (c *Converter) messageField(segs Message, _ string) any {
	return segs
}

// messageCQField 当 msgFormat 为 string 时返回 CQ 字符串，否则返回 nil（不输出该字段）
func (c *Converter) messageCQField(raw string) map[string]any {
	if c.msgFormat == "string" {
		return map[string]any{"message_cq": raw}
	}
	return nil
}

// nick 提取发送者昵称：优先 Author.Username，回退 Member.Nick
func nick(data *dto.Message) string {
	if data.Author != nil && data.Author.Username != "" {
		return data.Author.Username
	}
	if data.Member != nil && data.Member.Nick != "" {
		return data.Member.Nick
	}
	return ""
}

// cqEscape 转义 CQ 码文本
func cqEscape(s string) string {
	r := strings.NewReplacer("&", "&amp;", "[", "&#91;", "]", "&#93;")
	return r.Replace(s)
}

// imageFileID 由附件 FileName 生成 go-cqhttp 风格的 file 字段（<md5>.image），与 Gensokyo 对齐
func imageFileID(fileName string) string {
	id := fileName
	for _, ext := range []string{"{", "}", ".png", ".jpg", ".gif", "-"} {
		id = strings.ReplaceAll(id, ext, "")
	}
	return id + ".image"
}

// markSeen 按 QQ msg_id 去重；已存在返回 true
func (c *Converter) markSeen(qqMsgID string) bool {
	if qqMsgID == "" {
		return false
	}
	c.seenMu.Lock()
	defer c.seenMu.Unlock()
	if _, ok := c.seen[qqMsgID]; ok {
		return true
	}
	c.seen[qqMsgID] = struct{}{}
	c.seenOrder = append(c.seenOrder, qqMsgID)
	for len(c.seenOrder) > seenMsgLimit {
		oldest := c.seenOrder[0]
		c.seenOrder = c.seenOrder[1:]
		delete(c.seen, oldest)
	}
	return false
}

// recentMsgSplitRE 按 "=== 消息 N ===" 分块（最近十条 msg_elements 摘要）
var recentMsgSplitRE = regexp.MustCompile(`(?s)===\s*消息\s*\d+\s*===`)

// parseRecentMessages 解析「最近十条」msg_elements 文本摘要为结构化数组。
// 每条仅能拿到 content(文本) 与 sender(昵称)；其余字段 QQ 不提供。
func (c *Converter) parseRecentMessages(elements []*dto.MsgElement) []map[string]any {
	var out []map[string]any
	idx := 0
	for _, el := range elements {
		if el == nil || el.Content == "" {
			continue
		}
		for _, blk := range recentMsgSplitRE.Split(el.Content, -1) {
			blk = strings.TrimSpace(blk)
			if blk == "" {
				continue
			}
			content, sender := parseRecentBlock(blk)
			if content == "" && sender == "" {
				continue
			}
			idx++
			out = append(out, map[string]any{
				"index":   idx,
				"sender":  sender,
				"content": content,
				"message": c.messageField(Message{{Type: "text", Data: map[string]any{"text": content}}}, content),
			})
		}
	}
	return out
}

// parseRecentBlock 从单块中提取 [消息内容] 与 [发送者]
func parseRecentBlock(blk string) (content, sender string) {
	const cMark, sMark = "[消息内容]", "[发送者]"
	ci := strings.Index(blk, cMark)
	si := strings.LastIndex(blk, sMark)
	if ci >= 0 {
		end := len(blk)
		if si > ci {
			end = si
		}
		content = strings.TrimSpace(blk[ci+len(cMark) : end])
	}
	if si >= 0 {
		sender = strings.TrimSpace(blk[si+len(sMark):])
	}
	return
}

// mentionedSelf 全量群消息 payload 中是否 @ 了当前机器人
func mentionedSelf(data *dto.Message) bool {
	for _, u := range data.Mentions {
		if u != nil && u.IsYou {
			return true
		}
	}
	return false
}

// cleanMentionMarkup 去掉 QQ 内嵌 @ 标记（openid 非纯数字，ETLInput 的 <@!\d+> 无法覆盖）
func cleanMentionMarkup(s string) string {
	s = atOpenIDRE.ReplaceAllString(s, "")
	return strings.Trim(s, " \u00A0")
}

// groupOpenIDOf 取群 openid，优先 GroupOpenID，回退兼容旧字段 GroupID
func groupOpenIDOf(data *dto.Message) string {
	if data.GroupOpenID != "" {
		return data.GroupOpenID
	}
	return data.GroupID
}

// userOpenIDOf 取发送者 openid，优先 Author.ID，回退 Author.MemberOpenID
func userOpenIDOf(data *dto.Message) string {
	if data.Author == nil {
		return ""
	}
	if data.Author.ID != "" {
		return data.Author.ID
	}
	return data.Author.MemberOpenID
}

// unionOpenIDOf 取发送者跨 AppID 关联标识 union_openid（需向平台申请配置后才会下发），无则空
func unionOpenIDOf(data *dto.Message) string {
	if data == nil || data.Author == nil {
		return ""
	}
	return data.Author.UnionOpenID
}

// addUnionOpenID 若 union_openid 非空，则写入事件级与 sender 级字段（存在才写入，避免污染无该字段的事件）
func addUnionOpenID(ev map[string]any, unionOpenID string) {
	if unionOpenID == "" {
		return
	}
	ev["union_openid"] = unionOpenID
	if sender, ok := ev["sender"].(map[string]any); ok {
		sender["union_openid"] = unionOpenID
	}
}

// buildSegments 由 QQ 消息构造 OneBot message 段数组（主格式）与 raw_message（CQ 兜底）
// atSelf 为 true 时在段首注入 at self（NoneBot to_me() 为真）
func (c *Converter) buildSegments(data *dto.Message, atSelf bool) (Message, string) {
	segs := Message{}
	var raw strings.Builder

	if atSelf {
		self := strconv.FormatInt(c.selfID(), 10)
		segs = append(segs, Segment{Type: "at", Data: map[string]any{"qq": self}})
		raw.WriteString("[CQ:at,qq=")
		raw.WriteString(self)
		raw.WriteString("]")
	}

	if text := cleanMentionMarkup(data.Content); text != "" {
		segs = append(segs, Segment{Type: "text", Data: map[string]any{"text": text}})
		raw.WriteString(cqEscape(text))
	}

	for _, att := range data.Attachments {
		if att == nil || att.URL == "" {
			continue
		}
		url := att.URL
		if !strings.HasPrefix(url, "http://") && !strings.HasPrefix(url, "https://") {
			url = "https://" + url
		}
		typ := "image"
		switch {
		case strings.HasPrefix(att.ContentType, "video"):
			typ = "video"
		case strings.HasPrefix(att.ContentType, "voice"), strings.HasPrefix(att.ContentType, "audio"):
			typ = "record"
		}
		if typ == "image" {
			// 与 Gensokyo 对齐：file=<md5>.image, subType=0, url=真实链接
			file := imageFileID(att.FileName)
			imgData := map[string]any{
				"file":    file,
				"subType": "0",
				"url":     url,
			}
			// 透传图片宽高，便于下游（如 markdown 图片 #宽px #高px）按比例展示
			if att.Width > 0 {
				imgData["width"] = att.Width
			}
			if att.Height > 0 {
				imgData["height"] = att.Height
			}
			segs = append(segs, Segment{Type: "image", Data: imgData})
			raw.WriteString("[CQ:image,file=")
			raw.WriteString(cqEscape(file))
			raw.WriteString(",url=")
			raw.WriteString(cqEscape(url))
			raw.WriteString("]")
		} else {
			segs = append(segs, Segment{Type: typ, Data: map[string]any{"url": url, "file": url}})
			raw.WriteString("[CQ:")
			raw.WriteString(typ)
			raw.WriteString(",file=")
			raw.WriteString(cqEscape(url))
			raw.WriteString("]")
		}
	}

	return segs, raw.String()
}

// Group 处理群 @ 消息（GROUP_AT_MESSAGE_CREATE）
func (c *Converter) Group(data *dto.Message) {
	if data == nil || userOpenIDOf(data) == "" {
		return
	}
	if c.markSeen(data.ID) {
		return
	}
	stats.RecordReceived()
	groupOpenID := groupOpenIDOf(data)
	if groupOpenID == "" {
		return
	}
	userOpenID := userOpenIDOf(data)
	groupID := c.ids.ID(groupOpenID)
	userID := c.ids.ID(userOpenID)

	c.ids.StoreLazyGroup(groupOpenID, userOpenID, data.ID)
	msgID := c.ids.AllocMsgID(idmap.MsgMeta{
		MsgID:       data.ID,
		GroupOpenID: groupOpenID,
		UserOpenID:  userOpenID,
		Scene:       idmap.SceneGroup,
		Time:        time.Now(),
	})

	segs, raw := c.buildSegments(data, c.addAtGroup)
	c.pushGroupMessage(data, groupOpenID, userOpenID, groupID, userID, msgID, segs, raw)
}

// GroupAll 处理群全量消息（GROUP_MESSAGE_CREATE，需群主开启"获取群内全部消息"）
func (c *Converter) GroupAll(data *dto.Message) {
	if data == nil || userOpenIDOf(data) == "" {
		return
	}
	if c.markSeen(data.ID) {
		return
	}
	stats.RecordReceived()
	groupOpenID := groupOpenIDOf(data)
	if groupOpenID == "" {
		return
	}
	userOpenID := userOpenIDOf(data)
	groupID := c.ids.ID(groupOpenID)
	userID := c.ids.ID(userOpenID)

	c.ids.StoreLazyGroup(groupOpenID, userOpenID, data.ID)
	msgID := c.ids.AllocMsgID(idmap.MsgMeta{
		MsgID:       data.ID,
		GroupOpenID: groupOpenID,
		UserOpenID:  userOpenID,
		Scene:       idmap.SceneGroup,
		Time:        time.Now(),
	})

	atSelf := mentionedSelf(data)
	segs, raw := c.buildSegments(data, atSelf)
	c.pushGroupMessage(data, groupOpenID, userOpenID, groupID, userID, msgID, segs, raw)
}

// pushGroupMessage 组装群 message 事件、写入 get_msg 缓存、推送下游并打印收消息日志块
// roleOf 返回真实群成员角色，未知/空回退 member
func roleOf(data *dto.Message) string {
	if data == nil || data.Author == nil {
		return "member"
	}
	switch data.Author.MemberRole {
	case "owner", "admin":
		return data.Author.MemberRole
	default:
		return "member"
	}
}

func (c *Converter) pushGroupMessage(data *dto.Message, groupOpenID, userOpenID string, groupID, userID, msgID int64, segs Message, raw string) {
	ev := map[string]any{
		"post_type":       "message",
		"message_type":    "group",
		"sub_type":        "normal",
		"time":            time.Now().Unix(),
		"self_id":         c.selfID(),
		"group_id":        groupID,
		"user_id":         userID,
		"message_id":      msgID,
		"raw_message":     raw,
		"font":            0,
		"message":         c.messageField(segs, raw),
		"user_openid":     userOpenID,
		"group_openid":    groupOpenID,
		"qq_msg_id":       data.ID,
		"qq_message_type": data.QQMessageType,
		"msg_elements":    data.MsgElements,
		"msg_scene_ext":   data.MessageScene.Ext,
		"sender": map[string]any{
			"user_id":  userID,
			"nickname": nick(data),
			"card":     "",
			"sex":      "unknown",
			"age":      0,
			"area":     "",
			"level":    "",
			"title":    "",
			"role":     roleOf(data),
		},
	}
	addUnionOpenID(ev, unionOpenIDOf(data))
	if rm := c.parseRecentMessages(data.MsgElements); len(rm) > 0 {
		ev["recent_messages"] = rm
	}
	for k, v := range c.messageCQField(raw) {
		ev[k] = v
	}
	c.cacheForGetMsg(msgID, ev)
	delivered, fellBack := c.pushFallback(ev, idmap.SceneGroup, groupOpenID, "", data.ID)
	c.logRecvMessage("群", groupID, data, segs, msgID, delivered, fellBack)
}

// cacheForGetMsg 把已构造的消息事件快照写入内存缓存（供 get_msg 取回，仅内存）
func (c *Converter) cacheForGetMsg(msgID int64, ev map[string]any) {
	if c.caller == nil || msgID == 0 {
		return
	}
	snap := map[string]any{
		"time":         ev["time"],
		"message_type": ev["message_type"],
		"message_id":   ev["message_id"],
		"real_id":      ev["message_id"],
		"sender":       ev["sender"],
		"message":      ev["message"],
		"raw_message":  ev["raw_message"],
		"user_id":      ev["user_id"],
		"user_openid":  ev["user_openid"],
		"qq_msg_id":    ev["qq_msg_id"],
	}
	if gid, ok := ev["group_id"]; ok {
		snap["group_id"] = gid
	}
	if goid, ok := ev["group_openid"]; ok {
		snap["group_openid"] = goid
	}
	if mcq, ok := ev["message_cq"]; ok {
		snap["message_cq"] = mcq
	}
	c.caller.CacheMsg(msgID, snap)
}

// C2C 处理单聊消息
func (c *Converter) C2C(data *dto.Message) {
	if data == nil || data.Author == nil {
		return
	}
	stats.RecordReceived()
	userOpenID := data.Author.ID
	userID := c.ids.ID(userOpenID)

	c.ids.StoreLazyC2C(userOpenID, data.ID)
	msgID := c.ids.AllocMsgID(idmap.MsgMeta{
		MsgID:      data.ID,
		UserOpenID: userOpenID,
		Scene:      idmap.SceneC2C,
		Time:       time.Now(),
	})

	segs, raw := c.buildSegments(data, false)
	ev := map[string]any{
		"post_type":       "message",
		"message_type":    "private",
		"sub_type":        "friend",
		"time":            time.Now().Unix(),
		"self_id":         c.selfID(),
		"user_id":         userID,
		"message_id":      msgID,
		"raw_message":     raw,
		"font":            0,
		"message":         c.messageField(segs, raw),
		"user_openid":     userOpenID,
		"qq_msg_id":       data.ID,
		"qq_message_type": data.QQMessageType,
		"msg_elements":    data.MsgElements,
		"msg_scene_ext":   data.MessageScene.Ext,
		"sender": map[string]any{
			"user_id":  userID,
			"nickname": nick(data),
			"sex":      "unknown",
			"age":      0,
		},
	}
	addUnionOpenID(ev, unionOpenIDOf(data))
	for k, v := range c.messageCQField(raw) {
		ev[k] = v
	}
	c.cacheForGetMsg(msgID, ev)
	delivered, fellBack := c.pushFallback(ev, idmap.SceneC2C, "", userOpenID, data.ID)
	c.logRecvMessage("私聊", userID, data, segs, msgID, delivered, fellBack)
}

// pushFallback 推送事件；若所有下游投递数为 0 且配置了兜底文本，则直接被动回复 QQ。
// 返回投递成功的下游数，以及是否触发了兜底回发。
func (c *Converter) pushFallback(ev map[string]any, scene idmap.Scene, groupOpenID, userOpenID, msgID string) (delivered int, fellBack bool) {
	b, err := json.Marshal(ev)
	if err != nil {
		return 0, false
	}
	delivered = c.bus.PushRaw(b)
	if delivered > 0 || c.downtime == "" || c.caller == nil {
		return delivered, false
	}
	sender := c.caller.getSender()
	if sender == nil {
		return delivered, false
	}
	out := &OutMessage{MsgID: msgID, Parts: []MessagePart{{Kind: PartText, Text: c.downtime}}}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if scene == idmap.SceneGroup {
		_, _ = sender.SendGroup(ctx, groupOpenID, out)
	} else {
		_, _ = sender.SendC2C(ctx, userOpenID, out)
	}
	return delivered, true
}

// pushNotice 序列化并推送一个管理类 notice 事件（不走被动回复/兜底）
func (c *Converter) pushNotice(ev map[string]any) {
	b, err := json.Marshal(ev)
	if err != nil {
		return
	}
	delivered := c.bus.PushRaw(b)
	c.logNotice(ev, delivered)
}

// logRecvMessage 渲染一条"收消息"边框块
func (c *Converter) logRecvMessage(scene string, sceneID int64, data *dto.Message, segs Message, msgID int64, delivered int, fellBack bool) {
	title := "收消息 · " + scene + " " + strconv.FormatInt(sceneID, 10)
	if n := nick(data); n != "" {
		title += " · " + n
	}
	blk := prettylog.New(title, prettylog.ToneRecv)
	if content := strings.TrimSpace(data.Content); content != "" {
		blk.Row("内容", clip(content, 100))
	}
	blk.Row("段", segTypes(segs))
	if data.QQMessageType != 0 || len(data.MsgElements) > 0 {
		blk.Row("QQ类型", fmt.Sprintf("%d  msg_elements×%d  recent×%d",
			data.QQMessageType, len(data.MsgElements), len(c.parseRecentMessages(data.MsgElements))))
	}
	blk.Row("msgid", fmt.Sprintf("%d  %s", msgID, prettylog.Dim("QQ "+midEllipsis(data.ID, 14, 8))))
	if fellBack {
		blk.Row("推送", prettylog.Warn(fmt.Sprintf("下游全断，已兜底回发 (×%d)", delivered)))
	} else {
		blk.Row("推送", prettylog.Dim(fmt.Sprintf("NoneBot ×%d", delivered)))
	}
	blk.Emit()
}

// logNotice 渲染一条"通知事件"边框块（仅 post_type=notice 时）
func (c *Converter) logNotice(ev map[string]any, delivered int) {
	if ev["post_type"] != "notice" {
		return
	}
	nt, _ := ev["notice_type"].(string)
	blk := prettylog.New("通知 · "+nt, prettylog.ToneNotice)
	if v, ok := ev["group_id"]; ok {
		blk.Row("群", fmt.Sprintf("%v", v))
	}
	if v, ok := ev["user_id"]; ok {
		blk.Row("用户", fmt.Sprintf("%v", v))
	}
	if v, ok := ev["operator_id"]; ok {
		blk.Row("操作者", fmt.Sprintf("%v", v))
	}
	blk.Row("推送", prettylog.Dim(fmt.Sprintf("NoneBot ×%d", delivered)))
	blk.Emit()
}

// segTypes 汇总消息段类型列表，如 "at, text, image"
func segTypes(segs Message) string {
	if len(segs) == 0 {
		return "(空)"
	}
	parts := make([]string, 0, len(segs))
	for _, s := range segs {
		parts = append(parts, s.Type)
	}
	return strings.Join(parts, ", ")
}

// midEllipsis 保留首尾、中段省略，便于展示很长的 QQ msg_id
func midEllipsis(s string, head, tail int) string {
	rs := []rune(s)
	if len(rs) <= head+tail+1 {
		return s
	}
	return string(rs[:head]) + "…" + string(rs[len(rs)-tail:])
}

// clip 截断到 n 个字符，超出加省略号
func clip(s string, n int) string {
	rs := []rune(s)
	if len(rs) <= n {
		return s
	}
	return string(rs[:n]) + "…"
}

// tsToUnix 把 QQ 事件里的 timestamp（可能为 number/string/nil）转成秒级 unix，缺省取当前时间
func tsToUnix(v any) int64 {
	switch t := v.(type) {
	case nil:
		return time.Now().Unix()
	case float64:
		return int64(t)
	case int64:
		return t
	case int:
		return int64(t)
	case uint64:
		return int64(t)
	case json.Number:
		if n, err := t.Int64(); err == nil {
			return n
		}
	case string:
		if n, err := strconv.ParseInt(t, 10, 64); err == nil {
			return n
		}
	}
	return time.Now().Unix()
}

// GroupAddRobot 群添加机器人 -> notice/group_increase
func (c *Converter) GroupAddRobot(data *dto.WSGroupRobotData) {
	if data == nil {
		return
	}
	c.pushNotice(map[string]any{
		"post_type":       "notice",
		"notice_type":     "group_increase",
		"sub_type":        "invite",
		"time":            tsToUnix(data.Timestamp),
		"self_id":         c.selfID(),
		"group_id":        c.ids.ID(data.GroupOpenID),
		"group_openid":    data.GroupOpenID,
		"operator_id":     c.ids.ID(data.OpMemberOpenID),
		"operator_openid": data.OpMemberOpenID,
		"user_id":         c.selfID(),
	})
}

// GroupDelRobot 群删除机器人 -> notice/group_decrease
func (c *Converter) GroupDelRobot(data *dto.WSGroupRobotData) {
	if data == nil {
		return
	}
	c.pushNotice(map[string]any{
		"post_type":       "notice",
		"notice_type":     "group_decrease",
		"sub_type":        "kick_me",
		"time":            tsToUnix(data.Timestamp),
		"self_id":         c.selfID(),
		"group_id":        c.ids.ID(data.GroupOpenID),
		"group_openid":    data.GroupOpenID,
		"operator_id":     c.ids.ID(data.OpMemberOpenID),
		"operator_openid": data.OpMemberOpenID,
		"user_id":         c.selfID(),
	})
}

// GroupMsgReject 群关闭主动消息推送 -> notice/group_msg_reject（自定义）
func (c *Converter) GroupMsgReject(data *dto.WSGroupRobotData) {
	if data == nil {
		return
	}
	opID := c.ids.ID(data.OpMemberOpenID)
	c.pushNotice(map[string]any{
		"post_type":       "notice",
		"notice_type":     "group_msg_reject",
		"time":            tsToUnix(data.Timestamp),
		"self_id":         c.selfID(),
		"group_id":        c.ids.ID(data.GroupOpenID),
		"group_openid":    data.GroupOpenID,
		"operator_id":     opID,
		"operator_openid": data.OpMemberOpenID,
		"user_id":         opID,
		"user_openid":     data.OpMemberOpenID,
	})
}

// GroupMsgReceive 群开启主动消息推送 -> notice/group_msg_receive（自定义）
func (c *Converter) GroupMsgReceive(data *dto.WSGroupRobotData) {
	if data == nil {
		return
	}
	opID := c.ids.ID(data.OpMemberOpenID)
	c.pushNotice(map[string]any{
		"post_type":       "notice",
		"notice_type":     "group_msg_receive",
		"time":            tsToUnix(data.Timestamp),
		"self_id":         c.selfID(),
		"group_id":        c.ids.ID(data.GroupOpenID),
		"group_openid":    data.GroupOpenID,
		"operator_id":     opID,
		"operator_openid": data.OpMemberOpenID,
		"user_id":         opID,
		"user_openid":     data.OpMemberOpenID,
	})
}

// FriendAdd 用户添加机器人为好友 -> notice/friend_add（标准）
func (c *Converter) FriendAdd(data *dto.WSC2CFriendData) {
	if data == nil {
		return
	}
	c.pushNotice(map[string]any{
		"post_type":   "notice",
		"notice_type": "friend_add",
		"time":        tsToUnix(data.Timestamp),
		"self_id":     c.selfID(),
		"user_id":     c.ids.ID(data.OpenID),
		"user_openid": data.OpenID,
	})
}

// FriendDel 用户删除机器人好友 -> notice/friend_decrease（自定义）
func (c *Converter) FriendDel(data *dto.WSC2CFriendData) {
	if data == nil {
		return
	}
	c.pushNotice(map[string]any{
		"post_type":   "notice",
		"notice_type": "friend_decrease",
		"time":        tsToUnix(data.Timestamp),
		"self_id":     c.selfID(),
		"user_id":     c.ids.ID(data.OpenID),
		"user_openid": data.OpenID,
	})
}

// C2CMsgReject 用户关闭 C2C 主动消息推送 -> notice/c2c_msg_reject（自定义）
func (c *Converter) C2CMsgReject(data *dto.WSC2CManageData) {
	if data == nil {
		return
	}
	c.pushNotice(map[string]any{
		"post_type":   "notice",
		"notice_type": "c2c_msg_reject",
		"time":        tsToUnix(data.Timestamp),
		"self_id":     c.selfID(),
		"user_id":     c.ids.ID(data.OpenID),
		"user_openid": data.OpenID,
	})
}

// interactionSubType 按互动数据类型给出 OneBot notice 的 sub_type
func interactionSubType(t uint32) string {
	switch t {
	case 9:
		return "chat_search"
	case 11:
		return "inline_keyboard"
	case 12:
		return "callback_command"
	case 13:
		return "message_feedback"
	case 14:
		return "clear_session"
	default:
		return "other"
	}
}

// fnvHash 对字符串做 FNV64a 正整数哈希（用于合成互动 message_id）
func fnvHash(s string) int64 {
	h := fnv.New64a()
	_, _ = h.Write([]byte(s))
	return int64(h.Sum64() & 0x7FFFFFFFFFFFFFFF)
}

// Interaction 处理按钮回调/互动事件：存被动 event_id 凭证，并推 notice(interaction) + message 两类事件
func (c *Converter) Interaction(data *dto.WSInteractionData) {
	if data == nil {
		return
	}
	stats.RecordReceived()

	var buttonID, buttonData string
	var dataType uint32
	if data.Data != nil {
		dataType = uint32(data.Data.Type)
		if len(data.Data.Resolved) > 0 {
			var r struct {
				ButtonID   string `json:"button_id"`
				ButtonData string `json:"button_data"`
			}
			if err := json.Unmarshal(data.Data.Resolved, &r); err == nil {
				buttonID = r.ButtonID
				buttonData = r.ButtonData
			}
		}
	}

	isGroup := data.ChatType == 1 || data.Scene == "group"
	groupOpenID := data.GroupOpenID
	userOpenID := data.UserOpenID
	if userOpenID == "" {
		userOpenID = data.GroupMemberOpenID
	}
	userID := c.ids.ID(userOpenID)
	ts := tsToUnix(data.Timestamp)

	// 把外层 payload 的 event_id 作为被动 event_id 凭证存入 lazy 池，供后续回复走被动。
	// 注意：内层 data.ID（互动 id）仅用于 PUT /interactions 回执，不能当 event_id 发消息，
	// 否则 QQ 会返回 40034025「请求参数event_id无效」。缺失时回退到 data.ID 以兼容旧行为。
	passiveEventID := data.EventID
	if passiveEventID == "" {
		passiveEventID = data.ID
	}
	if isGroup {
		c.ids.StoreLazyGroupEvent(groupOpenID, userOpenID, passiveEventID)
	} else {
		c.ids.StoreLazyC2CEvent(userOpenID, passiveEventID)
	}

	notice := map[string]any{
		"post_type":      "notice",
		"notice_type":    "interaction",
		"sub_type":       interactionSubType(dataType),
		"time":           ts,
		"self_id":        c.selfID(),
		"user_id":        userID,
		"user_openid":    userOpenID,
		"chat_type":      data.ChatType,
		"interaction_id": data.ID,
		"data_type":      dataType,
		"button_id":      buttonID,
		"button_data":    buttonData,
	}

	segs := Message{{Type: "text", Data: map[string]any{"text": buttonData}}}
	msg := map[string]any{
		"post_type":      "message",
		"time":           ts,
		"self_id":        c.selfID(),
		"user_id":        userID,
		"user_openid":    userOpenID,
		"message_id":     fnvHash(data.ID),
		"raw_message":    buttonData,
		"font":           0,
		"interaction_id": data.ID,
		"message":        c.messageField(segs, buttonData),
		"sender":         map[string]any{"user_id": userID},
	}

	if isGroup {
		groupID := c.ids.ID(groupOpenID)
		notice["group_id"] = groupID
		notice["group_openid"] = groupOpenID
		msg["message_type"] = "group"
		msg["sub_type"] = "normal"
		msg["group_id"] = groupID
		msg["group_openid"] = groupOpenID
	} else {
		msg["message_type"] = "private"
		msg["sub_type"] = "friend"
	}
	for k, v := range c.messageCQField(buttonData) {
		msg[k] = v
	}

	c.pushNotice(notice)
	c.pushNotice(msg)
}

// C2CMsgReceive 用户开启 C2C 主动消息推送 -> notice/c2c_msg_receive（自定义）
func (c *Converter) C2CMsgReceive(data *dto.WSC2CManageData) {
	if data == nil {
		return
	}
	c.pushNotice(map[string]any{
		"post_type":   "notice",
		"notice_type": "c2c_msg_receive",
		"time":        tsToUnix(data.Timestamp),
		"self_id":     c.selfID(),
		"user_id":     c.ids.ID(data.OpenID),
		"user_openid": data.OpenID,
	})
}
