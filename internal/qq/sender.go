package qq

import (
	"context"
	"crypto/md5"
	"crypto/sha1"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"strings"
	"time"

	"golang.org/x/sync/errgroup"

	"goqbot/internal/botgo/dto"
	"goqbot/internal/botgo/dto/keyboard"
	"goqbot/internal/botgo/openapi"

	"goqbot/internal/filelog"
	"goqbot/internal/idmap"
	"goqbot/internal/onebot"
)

// combineParts 判断能否把整条消息合并为「单图富媒体 + 文本」一个气泡：
// 当恰好 1 张图片且无 markdown 段时，返回所有文本按原顺序拼接的字符串、该图片与 ok=true；
// 否则返回 ok=false，交由调用方逐段发送。
func combineParts(msg *onebot.OutMessage) (text string, img *onebot.OutImage, ok bool) {
	var b strings.Builder
	imageCount := 0
	mdCount := 0
	otherCount := 0 // 视频/语音/文件等富媒体段，存在时不能并入单图气泡
	var theImage onebot.OutImage
	for i := range msg.Parts {
		p := &msg.Parts[i]
		switch p.Kind {
		case onebot.PartText:
			b.WriteString(p.Text)
		case onebot.PartImage:
			imageCount++
			theImage = p.Image
		case onebot.PartMarkdown:
			mdCount++
		default:
			otherCount++
		}
	}
	if imageCount == 1 && mdCount == 0 && otherCount == 0 {
		return b.String(), &theImage, true
	}
	return "", nil, false
}

// sender 基于 botgo openapi 实现 onebot.MessageSender
type sender struct {
	api openapi.OpenAPI // QQ 官方 openapi 客户端
	ids *idmap.Store    // 被动消息 msg_seq 计数来源
}

var _ onebot.MessageSender = (*sender)(nil)

// newSender 创建发送实现
func newSender(api openapi.OpenAPI, ids *idmap.Store) *sender {
	return &sender{api: api, ids: ids}
}

// seqKey 取被动凭证作 msg_seq 去重键（event_id 优先于 msg_id）；都空=主动消息返回空
func seqKey(msgID, eventID string) string {
	if eventID != "" {
		return eventID
	}
	return msgID
}

// seqFor 仅被动消息（key 非空）才分配 msg_seq；主动消息无凭证，msg_seq 无意义（返回 0，omitempty 省略）
func (s *sender) seqFor(key string) uint32 {
	if key == "" {
		return 0
	}
	return s.ids.NextSeq(key)
}

// msgReference 构造 message_reference；refMsgID 为空返回 nil（omitempty 省略）。
// ignore_get_message_error=true：即使拉取被引用消息详情失败也照常下发本条消息，不整条丢弃。
func msgReference(refMsgID string) *dto.MessageReference {
	if refMsgID == "" {
		return nil
	}
	return &dto.MessageReference{MessageID: refMsgID, IgnoreGetMessageError: true}
}

// ---- 群聊 ----

// chunkThreshold 本地 base64 数据超过该体积时改走分片上传（对齐 nonebot-adapter-qq 的 10MB 阈值）
const chunkThreshold = 10 * 1024 * 1024

// mediaFileType 把发送段类型映射为 QQ 富媒体 file_type：视频 2、语音 3、文件 4
func mediaFileType(k onebot.PartKind) uint64 {
	switch k {
	case onebot.PartVideo:
		return 2
	case onebot.PartRecord:
		return 3
	default: // onebot.PartFile
		return 4
	}
}

// ensureMediaFileName 为分片上传补齐文件名。视频（file_type=2）缺扩展名时补 .mp4，
// 否则 QQ 会按无后缀名当作普通文件下发，无法作为视频直接播放。
func ensureMediaFileName(name string, fileType uint64) string {
	if name == "" {
		name = "file"
	}
	if fileType == 2 {
		base := name
		if i := strings.LastIndexAny(base, `/\`); i >= 0 {
			base = base[i+1:]
		}
		if !strings.Contains(base, ".") {
			name += ".mp4"
		}
	}
	return name
}

// uploadGroupImage 把图片上传为群富媒体（不立即发送），返回 file_info 供后续发送引用
func (s *sender) uploadGroupImage(ctx context.Context, groupOpenID string, img onebot.OutImage) ([]byte, error) {
	rm := &dto.RichMediaMessage{FileType: 1, SrvSendMsg: false}
	if img.URL != "" {
		rm.URL = img.URL
	} else {
		rm.FileData = img.Base64
	}
	resp, err := s.api.PostGroupMessage(ctx, groupOpenID, rm)
	if err != nil {
		return nil, err
	}
	return resp.FileInfo, nil
}

// uploadGroupMedia 上传群富媒体（视频/语音/文件），返回 file_info。
// URL 或小文件走简单 JSON 上传；本地大文件（>10MB base64）走分片上传。
func (s *sender) uploadGroupMedia(ctx context.Context, groupOpenID string, fileType uint64, m onebot.OutMedia) ([]byte, error) {
	if m.URL == "" && m.Base64 != "" {
		if data, err := base64.StdEncoding.DecodeString(m.Base64); err == nil && len(data) > chunkThreshold {
			return s.uploadGroupMediaChunked(ctx, groupOpenID, fileType, m.FileName, data)
		}
	}
	rm := &dto.RichMediaMessage{FileType: fileType, SrvSendMsg: false}
	if m.URL != "" {
		rm.URL = m.URL
	} else {
		rm.FileData = m.Base64
	}
	if fileType == 4 {
		rm.FileName = m.FileName // 仅文件透传文件名，避免图片/视频/语音被渲染成文件附件
	}
	resp, err := s.api.PostGroupMessage(ctx, groupOpenID, rm)
	if err != nil {
		return nil, err
	}
	return resp.FileInfo, nil
}

// uploadGroupMediaChunked 群本地大文件分片上传：prepare → 并发 PUT 分片 + 逐片 finish → files(upload_id) 换 file_info
func (s *sender) uploadGroupMediaChunked(ctx context.Context, groupOpenID string, fileType uint64, fileName string, data []byte) ([]byte, error) {
	fileName = ensureMediaFileName(fileName, fileType)
	prep, err := s.api.PostGroupUploadPrepare(ctx, groupOpenID, &dto.RichMediaPrepareReq{
		FileType: fileType,
		FileName: fileName,
		FileSize: int64(len(data)),
		MD5:      md5Hex(data),
		SHA1:     sha1Hex(data),
		MD510M:   md5Hex(first10M(data)),
	})
	if err != nil {
		return nil, err
	}
	if err := uploadParts(ctx, prep, data, func(c context.Context, fin *dto.RichMediaPartFinishReq) error {
		return s.api.PostGroupUploadPartFinish(c, groupOpenID, fin)
	}, s.api.PutUploadPart); err != nil {
		return nil, err
	}
	resp, err := s.api.PostGroupFileByUploadID(ctx, groupOpenID, prep.UploadID)
	if err != nil {
		return nil, err
	}
	return resp.FileInfo, nil
}

// sendGroupText 发送群文本消息，返回 QQ 消息 id。
// 被动 msgID 占用被动回复窗口；refMsgID 非空时置入 message_reference 渲染「引用」气泡。
func (s *sender) sendGroupText(ctx context.Context, groupOpenID, msgID, eventID, refMsgID, content string) (string, error) {
	m := &dto.MessageToCreate{
		Content:          content,
		MsgType:          dto.TextMsg,
		MsgID:            msgID,
		EventID:          eventID,
		MessageReference: msgReference(refMsgID),
		MsgSeq:           s.seqFor(seqKey(msgID, eventID)),
		Timestamp:        time.Now().Unix(),
	}
	resp, err := s.api.PostGroupMessage(ctx, groupOpenID, m)
	if err != nil {
		return "", err
	}
	return resp.ID, nil
}

// sendGroupMedia 用已上传的 file_info 发送群富媒体（图片等）消息
func (s *sender) sendGroupMedia(ctx context.Context, groupOpenID, msgID, eventID, refMsgID, content string, fileInfo []byte) (string, error) {
	m := &dto.MessageToCreate{
		Content:          content,
		MsgType:          dto.RichMediaMsg,
		Media:            &dto.MediaInfo{FileInfo: fileInfo},
		MsgID:            msgID,
		EventID:          eventID,
		MessageReference: msgReference(refMsgID),
		MsgSeq:           s.seqFor(seqKey(msgID, eventID)),
		Timestamp:        time.Now().Unix(),
	}
	resp, err := s.api.PostGroupMessage(ctx, groupOpenID, m)
	if err != nil {
		return "", err
	}
	return resp.ID, nil
}

// sendGroupMarkdown 发送群 markdown 消息（可带 keyboard 按钮）。
// 注意：markdown 不能附带 message_reference——群内二者同发会被平台降级成未渲染的纯文本。
func (s *sender) sendGroupMarkdown(ctx context.Context, groupOpenID, msgID, eventID string, md *dto.Markdown, kb *keyboard.MessageKeyboard) (string, error) {
	m := &dto.MessageToCreate{
		Content:   "markdown",
		MsgType:   dto.MarkdownMsg,
		Markdown:  md,
		Keyboard:  kb,
		MsgID:     msgID,
		EventID:   eventID,
		MsgSeq:    s.seqFor(seqKey(msgID, eventID)),
		Timestamp: time.Now().Unix(),
	}
	resp, err := s.api.PostGroupMessage(ctx, groupOpenID, m)
	if err != nil {
		return "", err
	}
	return resp.ID, nil
}

// SendGroup 发群消息：单图(无 markdown)时把全部文本并入富媒体一条气泡，否则按 Parts 逐段发送。
// 引用气泡只加在首条已发消息上，避免多段回复出现多个引用。
func (s *sender) SendGroup(ctx context.Context, groupOpenID string, msg *onebot.OutMessage) (string, error) {
	if msg.Stream != nil {
		filelog.Warnf("[qq] 群聊不支持流式消息（stream 仅 C2C），已忽略 stream 参数按普通消息发送")
	}
	if text, img, ok := combineParts(msg); ok {
		fi, err := s.uploadGroupImage(ctx, groupOpenID, *img)
		if err != nil {
			return "", err
		}
		// content 可空：空串经 omitempty 省略，避免纯图片下方出现空白文本气泡
		return s.sendGroupMedia(ctx, groupOpenID, msg.MsgID, msg.EventID, msg.RefMsgID, text, fi)
	}
	var lastID string
	ref := msg.RefMsgID // 仅首条已发消息带引用，发出后清空
	for _, p := range msg.Parts {
		switch p.Kind {
		case onebot.PartText:
			if p.Text == "" {
				continue
			}
			id, err := s.sendGroupText(ctx, groupOpenID, msg.MsgID, msg.EventID, ref, p.Text)
			if err != nil {
				return "", err
			}
			ref = ""
			lastID = id
		case onebot.PartImage:
			fi, err := s.uploadGroupImage(ctx, groupOpenID, p.Image)
			if err != nil {
				return "", err
			}
			id, err := s.sendGroupMedia(ctx, groupOpenID, msg.MsgID, msg.EventID, ref, "", fi)
			if err != nil {
				return "", err
			}
			ref = ""
			lastID = id
		case onebot.PartVideo, onebot.PartRecord, onebot.PartFile:
			fi, err := s.uploadGroupMedia(ctx, groupOpenID, mediaFileType(p.Kind), p.Media)
			if err != nil {
				return "", err
			}
			id, err := s.sendGroupMedia(ctx, groupOpenID, msg.MsgID, msg.EventID, ref, "", fi)
			if err != nil {
				return "", err
			}
			ref = ""
			lastID = id
		case onebot.PartMarkdown:
			// markdown 不带引用（见 sendGroupMarkdown 注释），故不消费 ref
			id, err := s.sendGroupMarkdown(ctx, groupOpenID, msg.MsgID, msg.EventID, p.Markdown, p.Keyboard)
			if err != nil {
				return "", err
			}
			lastID = id
		}
	}
	return lastID, nil
}

// ---- 单聊 ----

// uploadC2CImage 把图片上传为单聊富媒体（不立即发送），返回 file_info 供后续发送引用
func (s *sender) uploadC2CImage(ctx context.Context, userOpenID string, img onebot.OutImage) ([]byte, error) {
	rm := &dto.RichMediaMessage{FileType: 1, SrvSendMsg: false}
	if img.URL != "" {
		rm.URL = img.URL
	} else {
		rm.FileData = img.Base64
	}
	resp, err := s.api.PostC2CMessage(ctx, userOpenID, rm)
	if err != nil {
		return nil, err
	}
	return resp.FileInfo, nil
}

// uploadC2CMedia 上传单聊富媒体（视频/语音/文件），返回 file_info。规则同 uploadGroupMedia。
func (s *sender) uploadC2CMedia(ctx context.Context, userOpenID string, fileType uint64, m onebot.OutMedia) ([]byte, error) {
	if m.URL == "" && m.Base64 != "" {
		if data, err := base64.StdEncoding.DecodeString(m.Base64); err == nil && len(data) > chunkThreshold {
			return s.uploadC2CMediaChunked(ctx, userOpenID, fileType, m.FileName, data)
		}
	}
	rm := &dto.RichMediaMessage{FileType: fileType, SrvSendMsg: false}
	if m.URL != "" {
		rm.URL = m.URL
	} else {
		rm.FileData = m.Base64
	}
	if fileType == 4 {
		rm.FileName = m.FileName
	}
	resp, err := s.api.PostC2CMessage(ctx, userOpenID, rm)
	if err != nil {
		return nil, err
	}
	return resp.FileInfo, nil
}

// uploadC2CMediaChunked 单聊本地大文件分片上传，流程同群聊。
func (s *sender) uploadC2CMediaChunked(ctx context.Context, userOpenID string, fileType uint64, fileName string, data []byte) ([]byte, error) {
	fileName = ensureMediaFileName(fileName, fileType)
	prep, err := s.api.PostC2CUploadPrepare(ctx, userOpenID, &dto.RichMediaPrepareReq{
		FileType: fileType,
		FileName: fileName,
		FileSize: int64(len(data)),
		MD5:      md5Hex(data),
		SHA1:     sha1Hex(data),
		MD510M:   md5Hex(first10M(data)),
	})
	if err != nil {
		return nil, err
	}
	if err := uploadParts(ctx, prep, data, func(c context.Context, fin *dto.RichMediaPartFinishReq) error {
		return s.api.PostC2CUploadPartFinish(c, userOpenID, fin)
	}, s.api.PutUploadPart); err != nil {
		return nil, err
	}
	resp, err := s.api.PostC2CFileByUploadID(ctx, userOpenID, prep.UploadID)
	if err != nil {
		return nil, err
	}
	return resp.FileInfo, nil
}

// sendC2CText 发送单聊文本消息，返回 QQ 消息 id
func (s *sender) sendC2CText(ctx context.Context, userOpenID, msgID, eventID, refMsgID string, isWakeup bool, content string) (string, error) {
	m := &dto.MessageToCreate{
		Content:          content,
		MsgType:          dto.TextMsg,
		MsgID:            msgID,
		EventID:          eventID,
		MessageReference: msgReference(refMsgID),
		IsWakeup:         isWakeup,
		MsgSeq:           s.seqFor(seqKey(msgID, eventID)),
		Timestamp:        time.Now().Unix(),
	}
	resp, err := s.api.PostC2CMessage(ctx, userOpenID, m)
	if err != nil {
		return "", err
	}
	return resp.ID, nil
}

// sendC2CStream 发送单聊流式文本分片（msg_type=0 + stream）。
// 首帧 stream.ID 为空，返回的 resp.ID 即流式 ID，需由下游回传到后续帧的 stream.id。
func (s *sender) sendC2CStream(ctx context.Context, userOpenID, msgID, eventID string, isWakeup bool, content string, stream *dto.Stream) (string, error) {
	m := &dto.MessageToCreate{
		Content:   content,
		MsgType:   dto.TextMsg,
		MsgID:     msgID,
		EventID:   eventID,
		IsWakeup:  isWakeup,
		Stream:    stream,
		MsgSeq:    s.seqFor(seqKey(msgID, eventID)),
		Timestamp: time.Now().Unix(),
	}
	resp, err := s.api.PostC2CMessage(ctx, userOpenID, m)
	if err != nil {
		return "", err
	}
	return resp.ID, nil
}

// sendC2CMedia 用已上传的 file_info 发送单聊富媒体（图片等）消息
func (s *sender) sendC2CMedia(ctx context.Context, userOpenID, msgID, eventID, refMsgID string, isWakeup bool, content string, fileInfo []byte) (string, error) {
	m := &dto.MessageToCreate{
		Content:          content,
		MsgType:          dto.RichMediaMsg,
		Media:            &dto.MediaInfo{FileInfo: fileInfo},
		MsgID:            msgID,
		EventID:          eventID,
		MessageReference: msgReference(refMsgID),
		IsWakeup:         isWakeup,
		MsgSeq:           s.seqFor(seqKey(msgID, eventID)),
		Timestamp:        time.Now().Unix(),
	}
	resp, err := s.api.PostC2CMessage(ctx, userOpenID, m)
	if err != nil {
		return "", err
	}
	return resp.ID, nil
}

// sendC2CMarkdown 发送单聊 markdown 消息（可带 keyboard 按钮）。
// 注意：markdown 不能附带 message_reference——与群同理，二者同发会被降级成未渲染的纯文本。
func (s *sender) sendC2CMarkdown(ctx context.Context, userOpenID, msgID, eventID string, isWakeup bool, md *dto.Markdown, kb *keyboard.MessageKeyboard) (string, error) {
	m := &dto.MessageToCreate{
		Content:   "markdown",
		MsgType:   dto.MarkdownMsg,
		Markdown:  md,
		Keyboard:  kb,
		MsgID:     msgID,
		EventID:   eventID,
		IsWakeup:  isWakeup,
		MsgSeq:    s.seqFor(seqKey(msgID, eventID)),
		Timestamp: time.Now().Unix(),
	}
	resp, err := s.api.PostC2CMessage(ctx, userOpenID, m)
	if err != nil {
		return "", err
	}
	return resp.ID, nil
}

// SendC2C 发单聊消息：单图(无 markdown)时把全部文本并入富媒体一条气泡，否则按 Parts 逐段发送
func (s *sender) SendC2C(ctx context.Context, userOpenID string, msg *onebot.OutMessage) (string, error) {
	// 流式消息（仅 C2C）：整条按一帧分片发送，内容取所有文本段拼接，跳过常规逐段逻辑
	if msg.Stream != nil {
		var b strings.Builder
		for _, p := range msg.Parts {
			if p.Kind == onebot.PartText {
				b.WriteString(p.Text)
			}
		}
		return s.sendC2CStream(ctx, userOpenID, msg.MsgID, msg.EventID, msg.IsWakeup, b.String(), msg.Stream)
	}
	if text, img, ok := combineParts(msg); ok {
		fi, err := s.uploadC2CImage(ctx, userOpenID, *img)
		if err != nil {
			return "", err
		}
		// content 可空：空串经 omitempty 省略，避免纯图片下方出现空白文本气泡
		return s.sendC2CMedia(ctx, userOpenID, msg.MsgID, msg.EventID, msg.RefMsgID, msg.IsWakeup, text, fi)
	}
	var lastID string
	ref := msg.RefMsgID // 仅首条已发消息带引用，发出后清空
	for _, p := range msg.Parts {
		switch p.Kind {
		case onebot.PartText:
			if p.Text == "" {
				continue
			}
			id, err := s.sendC2CText(ctx, userOpenID, msg.MsgID, msg.EventID, ref, msg.IsWakeup, p.Text)
			if err != nil {
				return "", err
			}
			ref = ""
			lastID = id
		case onebot.PartImage:
			fi, err := s.uploadC2CImage(ctx, userOpenID, p.Image)
			if err != nil {
				return "", err
			}
			id, err := s.sendC2CMedia(ctx, userOpenID, msg.MsgID, msg.EventID, ref, msg.IsWakeup, "", fi)
			if err != nil {
				return "", err
			}
			ref = ""
			lastID = id
		case onebot.PartVideo, onebot.PartRecord, onebot.PartFile:
			fi, err := s.uploadC2CMedia(ctx, userOpenID, mediaFileType(p.Kind), p.Media)
			if err != nil {
				return "", err
			}
			id, err := s.sendC2CMedia(ctx, userOpenID, msg.MsgID, msg.EventID, ref, msg.IsWakeup, "", fi)
			if err != nil {
				return "", err
			}
			ref = ""
			lastID = id
		case onebot.PartMarkdown:
			// markdown 不带引用（见 sendC2CMarkdown 注释），故不消费 ref
			id, err := s.sendC2CMarkdown(ctx, userOpenID, msg.MsgID, msg.EventID, msg.IsWakeup, p.Markdown, p.Keyboard)
			if err != nil {
				return "", err
			}
			lastID = id
		}
	}
	return lastID, nil
}

// RetractGroup 撤回一条群消息
func (s *sender) RetractGroup(ctx context.Context, groupOpenID, qqMsgID string) error {
	return s.api.RetractGroupMessage(ctx, groupOpenID, qqMsgID)
}

// RetractC2C 撤回一条单聊消息
func (s *sender) RetractC2C(ctx context.Context, userOpenID, qqMsgID string) error {
	return s.api.RetractC2CMessage(ctx, userOpenID, qqMsgID)
}

// AckInteraction 回执互动事件（按钮点击），code 0=成功，非0=失败/频繁/无权限等
func (s *sender) AckInteraction(ctx context.Context, interactionID string, code int) error {
	return s.api.PutInteraction(ctx, interactionID, fmt.Sprintf(`{"code":%d}`, code))
}

// ---- 群聊管理 ----

// GetGroupInfo 获取群基本信息
func (s *sender) GetGroupInfo(ctx context.Context, groupOpenID string) (*onebot.GroupInfo, error) {
	info, err := s.api.GroupInfo(ctx, groupOpenID)
	if err != nil {
		return nil, err
	}
	return &onebot.GroupInfo{
		GroupOpenID:     info.GroupOpenID,
		GroupName:       info.GroupName,
		GroupFingerMemo: info.GroupFingerMemo,
		GroupClassText:  info.GroupClassText,
		GroupTags:       info.GroupTags,
		GroupMemberNum:  info.GroupMemberNum,
	}, nil
}

// GetGroupBotState 获取机器人群内状态
func (s *sender) GetGroupBotState(ctx context.Context, groupOpenID string) (*onebot.GroupBotState, error) {
	st, err := s.api.GroupBotState(ctx, groupOpenID)
	if err != nil {
		return nil, err
	}
	return &onebot.GroupBotState{
		MemberOpenID:      st.MemberOpenID,
		JoinedAt:          st.JoinedAt,
		AllowProactiveMsg: st.AllowProactiveMsg,
		RecvMsgSetting:    st.RecvMsgSetting,
		MemberRole:        st.MemberRole,
	}, nil
}

// GetGroupJoinRequests 拉取入群申请列表
func (s *sender) GetGroupJoinRequests(ctx context.Context, groupOpenID string, cursor string, limit int) (*onebot.JoinRequestList, error) {
	pager := &dto.JoinRequestListPager{Cursor: cursor, Limit: limit}
	list, err := s.api.GroupJoinRequestList(ctx, groupOpenID, pager)
	if err != nil {
		return nil, err
	}
	out := &onebot.JoinRequestList{NextCursor: list.NextCursor}
	for _, item := range list.List {
		if item == nil {
			continue
		}
		jr := onebot.JoinRequest{
			JoinRequestID: item.JoinRequestID,
			RiskTips:      item.RiskTips,
			UnionOpenID:   item.UnionOpenID,
			MemberOpenID:  item.MemberOpenID,
			Username:      item.Username,
			ApplyAt:       item.ApplyAt,
			ApplySource:   item.ApplySource,
			InvitedBy:     item.InvitedBy,
			Bot:           item.Bot,
		}
		if item.VerifyInfo != nil {
			jr.VerifyMethod = item.VerifyInfo.Method
			jr.VerifyMessage = item.VerifyInfo.VerifyMessage
		}
		out.List = append(out.List, jr)
	}
	return out, nil
}

// ApproveGroupJoinRequest 审批入群申请
func (s *sender) ApproveGroupJoinRequest(ctx context.Context, groupOpenID, memberOpenID, joinRequestID, op, rejectReason string, blacklist bool) error {
	return s.api.ApprovalGroupJoinRequest(ctx, groupOpenID, memberOpenID, &dto.ApprovalJoinRequestReq{
		Op:                   op,
		JoinRequestID:        joinRequestID,
		RejectReason:         rejectReason,
		AddToMemberBlacklist: blacklist,
	})
}

// GetGroupMute 查询群禁言状态
func (s *sender) GetGroupMute(ctx context.Context, groupOpenID string) (*onebot.RestrictChatSetting, error) {
	st, err := s.api.GroupRestrictChatSetting(ctx, groupOpenID)
	if err != nil {
		return nil, err
	}
	out := &onebot.RestrictChatSetting{}
	if st.GlobalRule != nil {
		out.GlobalMode = st.GlobalRule.Mode
	}
	for _, m := range st.Members {
		if m == nil {
			continue
		}
		out.Members = append(out.Members, onebot.MemberMute{
			MemberOpenID: m.MemberOpenID,
			MuteExpireAt: m.MuteExpireAt,
			Username:     m.Username,
			UnionOpenID:  m.UnionOpenID,
		})
	}
	return out, nil
}

// SetGroupMute 设置群成员禁言
func (s *sender) SetGroupMute(ctx context.Context, groupOpenID string, ops []onebot.MuteOp) error {
	req := &dto.SetRestrictChatReq{}
	for _, op := range ops {
		req.Members = append(req.Members, &dto.SetMemberMuteState{
			Op:           op.Op,
			MemberOpenID: op.MemberOpenID,
			MuteExpireAt: op.MuteExpireAt,
		})
	}
	return s.api.SetGroupRestrictChat(ctx, groupOpenID, req)
}

// ---- 分片上传辅助 ----

// uploadParts 按 prepare 返回的分片信息并发上传每一片：PUT 到预签名地址后调用 finish 确认。
// 切片偏移用枚举位置 i×block_size（对齐 nonebot），finish 的 part_index 用服务端返回的 part.Index。
func uploadParts(
	ctx context.Context,
	prep *dto.RichMediaPrepareResp,
	data []byte,
	finish func(context.Context, *dto.RichMediaPartFinishReq) error,
	put func(context.Context, string, []byte) error,
) error {
	blockSize := int64(prep.BlockSize)
	if blockSize <= 0 {
		return fmt.Errorf("分片上传 block_size 非法: %d", blockSize)
	}
	conc := int(prep.UploadConfig.Concurrency)
	if conc <= 0 {
		conc = 1
	}
	g, gctx := errgroup.WithContext(ctx)
	g.SetLimit(conc)
	for i, part := range prep.Parts {
		i, part := i, part
		start := int64(i) * blockSize
		if start >= int64(len(data)) {
			continue
		}
		end := start + blockSize
		if end > int64(len(data)) {
			end = int64(len(data))
		}
		chunk := data[start:end]
		g.Go(func() error {
			if err := put(gctx, part.PresignedURL, chunk); err != nil {
				return fmt.Errorf("分片 %d PUT 失败: %w", int(part.Index), err)
			}
			return finish(gctx, &dto.RichMediaPartFinishReq{
				UploadID:  prep.UploadID,
				PartIndex: int(part.Index),
				BlockSize: int64(len(chunk)),
				MD5:       md5Hex(chunk),
			})
		})
	}
	return g.Wait()
}

// md5Hex 返回数据的 md5 十六进制字符串
func md5Hex(data []byte) string {
	sum := md5.Sum(data)
	return hex.EncodeToString(sum[:])
}

// sha1Hex 返回数据的 sha1 十六进制字符串
func sha1Hex(data []byte) string {
	sum := sha1.Sum(data)
	return hex.EncodeToString(sum[:])
}

// first10M 返回前 10MB（不足则全部），用于 prepare 的 md5_10m 校验
func first10M(data []byte) []byte {
	if len(data) > 10*1024*1024 {
		return data[:10*1024*1024]
	}
	return data
}
