package onebot

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"goqbot/internal/botgo/dto"
	"goqbot/internal/botgo/dto/keyboard"
	"github.com/tidwall/gjson"

	"goqbot/internal/idmap"
	"goqbot/internal/prettylog"
	"goqbot/internal/stats"
)

// msgCacheLimit get_msg 内存缓存的最大条数，超出按 FIFO 淘汰最早的
const msgCacheLimit = 10000

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
	case "get_openid":
		return c.getOpenid(params)
	case "get_gateway", "get_gateway_bot", "_get_gateway", "get_session_limit":
		return c.getGateway()
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

// sendMode 描述发送模式：被动(msg_id/event)/主动
func sendMode(out *OutMessage) string {
	switch {
	case out.EventID != "":
		return "被动(event) " + midEllipsis(out.EventID, 14, 8)
	case out.MsgID != "":
		return "被动 msg_id=" + midEllipsis(out.MsgID, 14, 8)
	default:
		return "主动"
	}
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
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
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
	if active {
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
	if active {
		action = "send_private_active_msg"
	}
	start := time.Now()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
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
	return ok(map[string]any{"message_id": mid})
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
				text.WriteString("<@" + openid + ">")
			}
		case "image":
			img := parseImageData(data.Get("file").String(), data.Get("url").String())
			if img != nil {
				flushText()
				out.Parts = append(out.Parts, MessagePart{Kind: PartImage, Image: *img})
			}
		case "markdown":
			if md, kb := parseMarkdownSeg(data); md != nil {
				flushText()
				out.Parts = append(out.Parts, MessagePart{Kind: PartMarkdown, Markdown: md, Keyboard: kb})
			}
		case "reply":
			if meta, found := c.ids.Msg(data.Get("id").Int()); found && meta.MsgID != "" {
				out.MsgID = meta.MsgID
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
		log.Printf("[onebot] 解析 markdown 段失败: %v", err)
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
		log.Printf("[onebot] 读取本地图片失败 %s: %v", path, err)
		return nil
	}
	return &OutImage{Base64: base64.StdEncoding.EncodeToString(data)}
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
		log.Printf("[onebot] 收到动作 action=%s params=%s echo=%v", action, p, j.Get("echo").Value())
	}
	ret := c.Call(action, params)
	if echo := j.Get("echo"); echo.Exists() {
		ret["echo"] = echo.Value()
	}
	b, _ := json.Marshal(ret)
	return b
}
