package qq

import (
	"context"
	"crypto/md5"
	"crypto/sha1"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
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
	api       openapi.OpenAPI // QQ 官方 openapi 客户端
	ids       *idmap.Store    // 被动消息 msg_seq 计数来源
	streamSeq sync.Map        // stream_msg_id -> streamSeqEntry：同一条流式消息各帧沿用首帧的 msg_seq
}

// streamSeqEntry 流式消息首帧分配的 msg_seq 及最近使用时间（用于回收未正常收尾的流）
type streamSeqEntry struct {
	seq uint32
	at  time.Time
}

// streamSeqTTL 流式 msg_seq 记录保留时长（被动窗口 60 分钟，超过即无意义）
const streamSeqTTL = 60 * time.Minute

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

// msgIDs 从发消息回包取出 QQ msg_id 与本条 REFIDX（供后续引用机器人自己发的消息，含 markdown）。
func msgIDs(resp *dto.Message) (id, refIdx string) {
	if resp == nil {
		return "", ""
	}
	if resp.ExtInfo != nil {
		return resp.ID, resp.ExtInfo.RefIdx
	}
	return resp.ID, ""
}

// ---- 群聊 ----

// chunkThreshold 本地 base64 数据超过该体积时改走分片上传（官方 files 接口 file_data 上限 5MB）
const chunkThreshold = 5 * 1024 * 1024

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
	return s.uploadRich(ctx, s.groupScene(groupOpenID), 1, img.URL, img.Base64, "")
}

// uploadGroupMedia 上传群富媒体（视频/语音/文件），返回 file_info。规则见 uploadRich。
func (s *sender) uploadGroupMedia(ctx context.Context, groupOpenID string, fileType uint64, m onebot.OutMedia) ([]byte, error) {
	return s.uploadRich(ctx, s.groupScene(groupOpenID), fileType, m.URL, m.Base64, m.FileName)
}

// groupScene 群聊上传接口组
func (s *sender) groupScene(groupOpenID string) uploadScene {
	return uploadScene{
		label: "群",
		simple: func(ctx context.Context, rm *dto.RichMediaMessage) (*dto.Message, error) {
			return s.api.PostGroupMessage(ctx, groupOpenID, rm)
		},
		chunked: func(ctx context.Context, fileType uint64, fileName string, data []byte) ([]byte, error) {
			return s.uploadGroupMediaChunked(ctx, groupOpenID, fileType, fileName, data)
		},
	}
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
func (s *sender) sendGroupText(ctx context.Context, groupOpenID, msgID, eventID, refMsgID, content string) (string, string, error) {
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
		return "", "", err
	}
	id, ref := msgIDs(resp)
	return id, ref, nil
}

// sendGroupMedia 用已上传的 file_info 发送群富媒体（图片等）消息
func (s *sender) sendGroupMedia(ctx context.Context, groupOpenID, msgID, eventID, refMsgID, content string, fileInfo []byte) (string, string, error) {
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
		return "", "", err
	}
	id, ref := msgIDs(resp)
	return id, ref, nil
}

// sendGroupMarkdown 发送群 markdown 消息（可带 keyboard 按钮）。
// 官方规定填写 markdown 后 content 必须为空。refMsgID 非空时置入 message_reference，
// 可引用用户消息或机器人自己发过的消息（含 markdown，REFIDX 来自入站 msg_idx 或出站 ext_info.ref_idx）。
func (s *sender) sendGroupMarkdown(ctx context.Context, groupOpenID, msgID, eventID, refMsgID string, md *dto.Markdown, kb *keyboard.MessageKeyboard) (string, string, error) {
	// 图片转存失败时官方直接报错、消息不会发出（默认 false 时可能发出裂图）
	if md != nil {
		md.ForceVerifyImageResource = true
	}
	m := &dto.MessageToCreate{
		MsgType:          dto.MarkdownMsg,
		Markdown:         md,
		Keyboard:         kb,
		MsgID:            msgID,
		EventID:          eventID,
		MessageReference: msgReference(refMsgID),
		MsgSeq:           s.seqFor(seqKey(msgID, eventID)),
		Timestamp:        time.Now().Unix(),
	}
	resp, err := s.api.PostGroupMessage(ctx, groupOpenID, m)
	if err != nil {
		return "", "", err
	}
	id, ref := msgIDs(resp)
	return id, ref, nil
}

// sendGroupCard 发送群图文卡片消息（msg_type=8），只填 card，不带 content/markdown。
func (s *sender) sendGroupCard(ctx context.Context, groupOpenID, msgID, eventID, refMsgID string, card *dto.MessageCard) (string, string, error) {
	m := &dto.MessageToCreate{
		MsgType:          dto.CardMsg,
		Card:             card,
		MsgID:            msgID,
		EventID:          eventID,
		MessageReference: msgReference(refMsgID),
		MsgSeq:           s.seqFor(seqKey(msgID, eventID)),
		Timestamp:        time.Now().Unix(),
	}
	resp, err := s.api.PostGroupMessage(ctx, groupOpenID, m)
	if err != nil {
		return "", "", err
	}
	id, ref := msgIDs(resp)
	return id, ref, nil
}

// SendGroup 发群消息：单图(无 markdown)时把全部文本并入富媒体一条气泡，否则按 Parts 逐段发送。
// 引用气泡只加在首条已发消息上，避免多段回复出现多个引用。
func (s *sender) SendGroup(ctx context.Context, groupOpenID string, msg *onebot.OutMessage) (string, string, error) {
	if msg.Stream != nil {
		filelog.Warnf("[qq] 群聊不支持流式消息（stream 仅 C2C），已忽略 stream 参数按普通消息发送")
	}
	if text, img, ok := combineParts(msg); ok {
		fi, err := s.uploadGroupImage(ctx, groupOpenID, *img)
		if err != nil {
			return "", "", err
		}
		// content 可空：空串经 omitempty 省略，避免纯图片下方出现空白文本气泡
		return s.sendGroupMedia(ctx, groupOpenID, msg.MsgID, msg.EventID, msg.RefMsgID, text, fi)
	}
	var lastID, lastRef string
	ref := msg.RefMsgID // 仅首条已发消息带引用，发出后清空
	for _, p := range msg.Parts {
		switch p.Kind {
		case onebot.PartText:
			if p.Text == "" {
				continue
			}
			id, newRef, err := s.sendGroupText(ctx, groupOpenID, msg.MsgID, msg.EventID, ref, p.Text)
			if err != nil {
				return "", "", err
			}
			ref = ""
			lastID, lastRef = id, newRef
		case onebot.PartImage:
			fi, err := s.uploadGroupImage(ctx, groupOpenID, p.Image)
			if err != nil {
				return "", "", err
			}
			id, newRef, err := s.sendGroupMedia(ctx, groupOpenID, msg.MsgID, msg.EventID, ref, "", fi)
			if err != nil {
				return "", "", err
			}
			ref = ""
			lastID, lastRef = id, newRef
		case onebot.PartVideo, onebot.PartRecord, onebot.PartFile:
			fi, err := s.uploadGroupMedia(ctx, groupOpenID, mediaFileType(p.Kind), p.Media)
			if err != nil {
				return "", "", err
			}
			id, newRef, err := s.sendGroupMedia(ctx, groupOpenID, msg.MsgID, msg.EventID, ref, "", fi)
			if err != nil {
				return "", "", err
			}
			ref = ""
			lastID, lastRef = id, newRef
		case onebot.PartMarkdown:
			id, newRef, err := s.sendGroupMarkdown(ctx, groupOpenID, msg.MsgID, msg.EventID, ref, p.Markdown, p.Keyboard)
			if err != nil {
				return "", "", err
			}
			ref = ""
			lastID, lastRef = id, newRef
		case onebot.PartCard:
			id, newRef, err := s.sendGroupCard(ctx, groupOpenID, msg.MsgID, msg.EventID, ref, p.Card)
			if err != nil {
				return "", "", err
			}
			ref = ""
			lastID, lastRef = id, newRef
		}
	}
	return lastID, lastRef, nil
}

// ---- 单聊 ----

// uploadC2CImage 把图片上传为单聊富媒体（不立即发送），返回 file_info 供后续发送引用
func (s *sender) uploadC2CImage(ctx context.Context, userOpenID string, img onebot.OutImage) ([]byte, error) {
	return s.uploadRich(ctx, s.c2cScene(userOpenID), 1, img.URL, img.Base64, "")
}

// uploadC2CMedia 上传单聊富媒体（视频/语音/文件），返回 file_info。规则见 uploadRich。
func (s *sender) uploadC2CMedia(ctx context.Context, userOpenID string, fileType uint64, m onebot.OutMedia) ([]byte, error) {
	return s.uploadRich(ctx, s.c2cScene(userOpenID), fileType, m.URL, m.Base64, m.FileName)
}

// c2cScene 单聊上传接口组
func (s *sender) c2cScene(userOpenID string) uploadScene {
	return uploadScene{
		label: "单聊",
		simple: func(ctx context.Context, rm *dto.RichMediaMessage) (*dto.Message, error) {
			return s.api.PostC2CMessage(ctx, userOpenID, rm)
		},
		chunked: func(ctx context.Context, fileType uint64, fileName string, data []byte) ([]byte, error) {
			return s.uploadC2CMediaChunked(ctx, userOpenID, fileType, fileName, data)
		},
	}
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
func (s *sender) sendC2CText(ctx context.Context, userOpenID, msgID, eventID, refMsgID string, isWakeup bool, content string) (string, string, error) {
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
		return "", "", err
	}
	id, ref := msgIDs(resp)
	return id, ref, nil
}

// sendC2CStream 走独立接口 POST /v2/users/{user_openid}/stream_messages 发送一帧流式消息。
// 首帧 stream.ID 为空，平台返回的 id 即 stream_msg_id，下游需回填到后续帧 stream.id。
// 同一条流的各帧沿用首帧分配的 msg_seq（与官方示例一致），收尾帧(state=10)后释放。
func (s *sender) sendC2CStream(ctx context.Context, userOpenID, msgID, eventID string, isWakeup bool, content string, st *dto.Stream) (string, string, int, error) {
	state := st.State
	switch state {
	case 11:
		state = dto.StreamStateGenerating
	case 20:
		state = dto.StreamStateFinished
	case dto.StreamStateGenerating, dto.StreamStateFinished:
	default:
		state = dto.StreamStateGenerating
	}
	mode := strings.ToLower(strings.TrimSpace(st.InputMode))
	if mode == "" && st.Reset {
		mode = "replace"
	}
	if mode != "replace" {
		mode = "append"
	}
	ctype := strings.ToLower(strings.TrimSpace(st.ContentType))
	if ctype != "markdown" {
		ctype = "text"
	}

	var seq uint32
	if st.ID != "" {
		if v, ok := s.streamSeq.Load(st.ID); ok {
			e := v.(streamSeqEntry)
			seq = e.seq
			s.streamSeq.Store(st.ID, streamSeqEntry{seq: seq, at: time.Now()})
		}
	}
	if seq == 0 {
		seq = s.seqFor(seqKey(msgID, eventID))
	}

	m := &dto.StreamMessageToCreate{
		InputMode:   mode,
		InputState:  state,
		Index:       st.Index,
		ContentType: ctype,
		ContentRaw:  content,
		MsgID:       msgID,
		EventID:     eventID,
		StreamMsgID: st.ID,
		MsgSeq:      seq,
		IsWakeup:    isWakeup,
	}
	if msgID != "" && eventID != "" {
		m.MsgID = "" // 二者互斥，event_id 优先（与 seqKey 一致）
	}
	resp, err := s.api.PostC2CStreamMessage(ctx, userOpenID, m)
	if err != nil {
		return "", "", -1, err
	}
	id := resp.ID
	if id == "" {
		id = st.ID
	}
	ref := ""
	if resp.ExtInfo != nil {
		ref = resp.ExtInfo.RefIdx
	}
	if state == dto.StreamStateFinished {
		s.streamSeq.Delete(st.ID)
		s.streamSeq.Delete(id)
	} else if id != "" && seq != 0 {
		s.streamSeq.Store(id, streamSeqEntry{seq: seq, at: time.Now()})
		s.sweepStreamSeq()
	}
	return id, ref, resp.RemainMsgLen, nil
}

// sweepStreamSeq 回收超时未收尾的流式 msg_seq 记录
func (s *sender) sweepStreamSeq() {
	now := time.Now()
	s.streamSeq.Range(func(k, v any) bool {
		if now.Sub(v.(streamSeqEntry).at) > streamSeqTTL {
			s.streamSeq.Delete(k)
		}
		return true
	})
}

// sendC2CMedia 用已上传的 file_info 发送单聊富媒体（图片等）消息
func (s *sender) sendC2CMedia(ctx context.Context, userOpenID, msgID, eventID, refMsgID string, isWakeup bool, content string, fileInfo []byte) (string, string, error) {
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
		return "", "", err
	}
	id, ref := msgIDs(resp)
	return id, ref, nil
}

// sendC2CMarkdown 发送单聊 markdown 消息（可带 keyboard 按钮）。
// 与群同理：content 必须为空；refMsgID 非空时置入 message_reference。
func (s *sender) sendC2CMarkdown(ctx context.Context, userOpenID, msgID, eventID, refMsgID string, isWakeup bool, md *dto.Markdown, kb *keyboard.MessageKeyboard) (string, string, error) {
	if md != nil {
		md.ForceVerifyImageResource = true
	}
	m := &dto.MessageToCreate{
		MsgType:          dto.MarkdownMsg,
		Markdown:         md,
		Keyboard:         kb,
		MsgID:            msgID,
		EventID:          eventID,
		MessageReference: msgReference(refMsgID),
		IsWakeup:         isWakeup,
		MsgSeq:           s.seqFor(seqKey(msgID, eventID)),
		Timestamp:        time.Now().Unix(),
	}
	resp, err := s.api.PostC2CMessage(ctx, userOpenID, m)
	if err != nil {
		return "", "", err
	}
	id, ref := msgIDs(resp)
	return id, ref, nil
}

// sendC2CCard 发送单聊图文卡片消息（msg_type=8），只填 card，不带 content/markdown。
func (s *sender) sendC2CCard(ctx context.Context, userOpenID, msgID, eventID, refMsgID string, isWakeup bool, card *dto.MessageCard) (string, string, error) {
	m := &dto.MessageToCreate{
		MsgType:          dto.CardMsg,
		Card:             card,
		MsgID:            msgID,
		EventID:          eventID,
		MessageReference: msgReference(refMsgID),
		IsWakeup:         isWakeup,
		MsgSeq:           s.seqFor(seqKey(msgID, eventID)),
		Timestamp:        time.Now().Unix(),
	}
	resp, err := s.api.PostC2CMessage(ctx, userOpenID, m)
	if err != nil {
		return "", "", err
	}
	id, ref := msgIDs(resp)
	return id, ref, nil
}

// SendC2C 发单聊消息：单图(无 markdown)时把全部文本并入富媒体一条气泡，否则按 Parts 逐段发送
func (s *sender) SendC2C(ctx context.Context, userOpenID string, msg *onebot.OutMessage) (string, string, error) {
	// 流式消息（仅 C2C）：整条作为一帧走 stream_messages，内容取所有文本段拼接；
	// 若未指定 content_type 且消息里带 markdown 段，则改用 markdown 正文并按 markdown 发送。
	if msg.Stream != nil {
		var b strings.Builder
		var md strings.Builder
		for _, p := range msg.Parts {
			switch p.Kind {
			case onebot.PartText:
				b.WriteString(p.Text)
			case onebot.PartMarkdown:
				if p.Markdown != nil {
					md.WriteString(p.Markdown.Content)
				}
			case onebot.PartImage, onebot.PartVideo, onebot.PartRecord, onebot.PartFile, onebot.PartCard:
				filelog.Warnf("[qq] 流式消息仅支持文本/markdown，已忽略富媒体/卡片段")
			}
		}
		content := b.String()
		if md.Len() > 0 && msg.Stream.ContentType == "" {
			msg.Stream.ContentType = "markdown"
			content = md.String() + content
		}
		id, ref, remain, err := s.sendC2CStream(ctx, userOpenID, msg.MsgID, msg.EventID, msg.IsWakeup, content, msg.Stream)
		msg.StreamRemainLen = remain
		return id, ref, err
	}
	if text, img, ok := combineParts(msg); ok {
		fi, err := s.uploadC2CImage(ctx, userOpenID, *img)
		if err != nil {
			return "", "", err
		}
		// content 可空：空串经 omitempty 省略，避免纯图片下方出现空白文本气泡
		return s.sendC2CMedia(ctx, userOpenID, msg.MsgID, msg.EventID, msg.RefMsgID, msg.IsWakeup, text, fi)
	}
	var lastID, lastRef string
	ref := msg.RefMsgID // 仅首条已发消息带引用，发出后清空
	for _, p := range msg.Parts {
		switch p.Kind {
		case onebot.PartText:
			if p.Text == "" {
				continue
			}
			id, newRef, err := s.sendC2CText(ctx, userOpenID, msg.MsgID, msg.EventID, ref, msg.IsWakeup, p.Text)
			if err != nil {
				return "", "", err
			}
			ref = ""
			lastID, lastRef = id, newRef
		case onebot.PartImage:
			fi, err := s.uploadC2CImage(ctx, userOpenID, p.Image)
			if err != nil {
				return "", "", err
			}
			id, newRef, err := s.sendC2CMedia(ctx, userOpenID, msg.MsgID, msg.EventID, ref, msg.IsWakeup, "", fi)
			if err != nil {
				return "", "", err
			}
			ref = ""
			lastID, lastRef = id, newRef
		case onebot.PartVideo, onebot.PartRecord, onebot.PartFile:
			fi, err := s.uploadC2CMedia(ctx, userOpenID, mediaFileType(p.Kind), p.Media)
			if err != nil {
				return "", "", err
			}
			id, newRef, err := s.sendC2CMedia(ctx, userOpenID, msg.MsgID, msg.EventID, ref, msg.IsWakeup, "", fi)
			if err != nil {
				return "", "", err
			}
			ref = ""
			lastID, lastRef = id, newRef
		case onebot.PartMarkdown:
			id, newRef, err := s.sendC2CMarkdown(ctx, userOpenID, msg.MsgID, msg.EventID, ref, msg.IsWakeup, p.Markdown, p.Keyboard)
			if err != nil {
				return "", "", err
			}
			ref = ""
			lastID, lastRef = id, newRef
		case onebot.PartCard:
			id, newRef, err := s.sendC2CCard(ctx, userOpenID, msg.MsgID, msg.EventID, ref, msg.IsWakeup, p.Card)
			if err != nil {
				return "", "", err
			}
			ref = ""
			lastID, lastRef = id, newRef
		}
	}
	return lastID, lastRef, nil
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

func mapGroupMember(m *dto.GroupMember) onebot.GroupMember {
	if m == nil {
		return onebot.GroupMember{}
	}
	return onebot.GroupMember{
		MemberOpenID: m.MemberOpenID,
		Username:     m.Username,
		MemberRole:   m.MemberRole,
		Bot:          m.Bot,
		JoinedAt:     m.JoinedAt,
		UnionOpenID:  m.UnionOpenID,
	}
}

// GetGroupMembers 获取群成员列表
func (s *sender) GetGroupMembers(ctx context.Context, groupOpenID string, cursor string) (*onebot.GroupMemberList, error) {
	list, err := s.api.GroupMembers(ctx, groupOpenID, &dto.GroupMemberListPager{Cursor: cursor})
	if err != nil {
		return nil, err
	}
	out := &onebot.GroupMemberList{NextCursor: list.NextCursor}
	for _, item := range list.Members {
		if item == nil {
			continue
		}
		out.Members = append(out.Members, mapGroupMember(item))
	}
	return out, nil
}

// GetGroupMember 获取指定群成员信息
func (s *sender) GetGroupMember(ctx context.Context, groupOpenID, memberOpenID string) (*onebot.GroupMember, error) {
	m, err := s.api.GroupMember(ctx, groupOpenID, memberOpenID)
	if err != nil {
		return nil, err
	}
	mapped := mapGroupMember(m)
	return &mapped, nil
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

// ---- 自定义菜单与指令面板 ----

// GetMenu 查询全局自定义菜单
func (s *sender) GetMenu(ctx context.Context) (*dto.MenuGetResp, error) {
	return s.api.GetMenu(ctx)
}

// SetMenu 修改全局自定义菜单（整体覆盖）
func (s *sender) SetMenu(ctx context.Context, req *dto.MenuPutReq) (*dto.VersionResp, error) {
	return s.api.SetMenu(ctx, req)
}

// ListCommandPanels 查询指令面板列表
func (s *sender) ListCommandPanels(ctx context.Context, pager *dto.PanelListPager) (*dto.PanelListResp, error) {
	return s.api.ListCommandPanels(ctx, pager)
}

// GetCommandPanel 查询指令面板详情
func (s *sender) GetCommandPanel(ctx context.Context, panelID string) (*dto.PanelDetail, error) {
	return s.api.GetCommandPanel(ctx, panelID)
}

// CreateCommandPanel 创建指令面板
func (s *sender) CreateCommandPanel(ctx context.Context, req *dto.PanelCreateReq) (*dto.PanelCreateResp, error) {
	return s.api.CreateCommandPanel(ctx, req)
}

// SetCommandPanel 修改指令面板（整体覆盖）
func (s *sender) SetCommandPanel(ctx context.Context, panelID string, req *dto.PanelPutReq) (*dto.VersionResp, error) {
	return s.api.SetCommandPanel(ctx, panelID, req)
}

// DeleteCommandPanel 删除指令面板
func (s *sender) DeleteCommandPanel(ctx context.Context, panelID string) error {
	return s.api.DeleteCommandPanel(ctx, panelID)
}

// SetCommandPanelTarget 修改指令面板关联对象（仅 specific 面板）
func (s *sender) SetCommandPanelTarget(ctx context.Context, panelID string, req *dto.PanelTargetReq) error {
	return s.api.SetCommandPanelTarget(ctx, panelID, req)
}

// ---- 素材管理 ----

// mediaImageMaxSize 素材图片大小上限（官方默认 10MB）
const mediaImageMaxSize = 10 << 20

// mediaImageFetchClient 下载 URL 来源的素材图片
var mediaImageFetchClient = &http.Client{Timeout: 30 * time.Second}

// mediaImageReuseMargin 复用已有素材时要求的最少剩余有效期，留出发送余量
const mediaImageReuseMargin = time.Hour

// UploadMediaImage 上传素材图片：prepare → PUT 预签名地址 → complete，返回 qqbot-media:// 引用链接。
// force=false 时先按内容 sha1 查本地上传记录，并用官方列表确认仍有效，命中则直接复用不再上传。
func (s *sender) UploadMediaImage(ctx context.Context, m onebot.OutMedia, force bool) (*onebot.MediaImageInfo, error) {
	data, err := mediaImageBytes(ctx, m)
	if err != nil {
		return nil, err
	}
	if len(data) == 0 {
		return nil, fmt.Errorf("素材图片内容为空")
	}
	if len(data) > mediaImageMaxSize {
		return nil, fmt.Errorf("素材图片超过 %dMB 上限（%d 字节）", mediaImageMaxSize>>20, len(data))
	}
	contentType := http.DetectContentType(data)
	switch contentType {
	case "image/png", "image/jpeg", "image/gif", "image/webp":
	default:
		return nil, fmt.Errorf("素材图片仅支持 png/jpeg/gif/webp，当前为 %s", contentType)
	}
	sha1Sum := sha1.Sum(data)
	sha1Hex := hex.EncodeToString(sha1Sum[:])
	if !force {
		if info := s.reuseMediaImage(ctx, sha1Hex, m.FileName); info != nil {
			filelog.Infof("[qq] 素材图片已存在，直接复用 image_id=%s sha1=%s", info.ImageID, sha1Hex)
			return info, nil
		}
	}

	fileName := mediaImageFileName(m.FileName, data)
	md5Sum := md5.Sum(data)
	md5Hex := hex.EncodeToString(md5Sum[:])
	prep, err := s.api.PrepareMediaImage(ctx, &dto.MediaImagePrepareReq{
		FileName:    fileName,
		FileSize:    uint64(len(data)),
		ContentType: contentType,
		MD5:         md5Hex,
		SHA1:        sha1Hex,
	})
	if err != nil {
		return nil, fmt.Errorf("素材上传 prepare 失败: %w", err)
	}
	etag, err := s.api.PutMediaImage(ctx, prep.PresignedURL, contentType, data)
	if err != nil {
		return nil, fmt.Errorf("素材上传 PUT 失败: %w", err)
	}
	if etag == "" {
		etag = md5Hex
	}
	resp, err := s.api.CompleteMediaImage(ctx, &dto.MediaImageCompleteReq{UploadID: prep.UploadID, MD5: etag})
	if err != nil {
		return nil, fmt.Errorf("素材上传 complete 失败: %w", err)
	}
	filelog.Infof("[qq] 素材图片上传成功 image_id=%s file=%s size=%d", resp.ImageID, fileName, len(data))

	localName := m.FileName
	if localName == "" {
		localName = fileName
	}
	now := time.Now().Unix()
	s.ids.PutMediaImage(idmap.MediaImageRecord{
		ImageID:    resp.ImageID,
		SHA1:       sha1Hex,
		URL:        resp.URL,
		FileNames:  []string{localName},
		UploadedAt: now,
	})
	return &onebot.MediaImageInfo{
		ImageID:    resp.ImageID,
		URL:        resp.URL,
		FileName:   fileName,
		LocalNames: []string{localName},
		SHA1:       sha1Hex,
		PreviewURL: resp.PreviewURL,
		CreatedAt:  now,
		ExpiresAt:  now + int64(resp.TTL),
	}, nil
}

// reuseMediaImage 按内容 sha1 查本地上传记录，并用官方列表确认素材仍在且未临近过期；命中返回素材信息，
// 并把本次传入的文件名追加到记录里，便于之后按该名查询。
func (s *sender) reuseMediaImage(ctx context.Context, sha1Hex, name string) *onebot.MediaImageInfo {
	recs := s.ids.MediaImagesBySHA1(sha1Hex)
	if len(recs) == 0 {
		return nil
	}
	live, err := s.allMediaImages(ctx)
	if err != nil {
		filelog.Warnf("[qq] 查询素材列表失败，跳过查重直接上传: %v", err)
		return nil
	}
	byID := make(map[string]*dto.MediaImage, len(live))
	for _, img := range live {
		byID[img.ImageID] = img
	}
	for _, rec := range recs { // 最近上传的优先
		img, ok := byID[rec.ImageID]
		if !ok {
			s.ids.DeleteMediaImage(rec.ImageID) // 官方已删除 / 已过期：丢弃记录
			continue
		}
		if img.ExpiresAt > 0 && time.Until(time.Unix(int64(img.ExpiresAt), 0)) < mediaImageReuseMargin {
			continue // 即将过期，不复用；记录保留到官方列表里消失为止
		}
		s.ids.AddMediaImageName(rec.ImageID, name)
		if name != "" && !containsString(rec.FileNames, name) {
			rec.FileNames = append(rec.FileNames, name)
		}
		info := mediaImageInfo(img, &rec)
		info.Cached = true
		return info
	}
	return nil
}

func containsString(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}

// FindMediaImages 按 image_id / 内容 sha1 / 文件名查询素材。以官方列表为准，只返回仍有效的素材；
// 顺带清理本地记录里官方已不存在的条目。
func (s *sender) FindMediaImages(ctx context.Context, q onebot.MediaImageQuery) ([]*onebot.MediaImageInfo, error) {
	live, err := s.allMediaImages(ctx)
	if err != nil {
		return nil, err
	}
	recs := map[string]*idmap.MediaImageRecord{} // image_id -> 本地上传记录
	for _, r := range s.ids.MediaImages() {
		r := r
		recs[r.ImageID] = &r
	}
	alive := make(map[string]bool, len(live))
	now := time.Now().Unix()
	var out []*onebot.MediaImageInfo
	for _, img := range live {
		alive[img.ImageID] = true
		if img.ExpiresAt > 0 && int64(img.ExpiresAt) <= now {
			continue
		}
		rec := recs[img.ImageID]
		if matchMediaImage(q, img, rec) {
			out = append(out, mediaImageInfo(img, rec))
		}
	}
	for id := range recs {
		if !alive[id] {
			s.ids.DeleteMediaImage(id)
		}
	}
	return out, nil
}

// ListMediaImages 查询素材图片列表
func (s *sender) ListMediaImages(ctx context.Context, pager *dto.MediaImageListPager) (*dto.MediaImageList, error) {
	return s.api.ListMediaImages(ctx, pager)
}

// DeleteMediaImage 删除素材图片，成功后清理本地上传记录
func (s *sender) DeleteMediaImage(ctx context.Context, imageID string) error {
	if err := s.api.DeleteMediaImage(ctx, imageID); err != nil {
		return err
	}
	s.ids.DeleteMediaImage(imageID)
	return nil
}

// allMediaImages 翻页拉取全部有效素材（单机器人上限 100 张，页数很少）
func (s *sender) allMediaImages(ctx context.Context) ([]*dto.MediaImage, error) {
	var out []*dto.MediaImage
	pager := &dto.MediaImageListPager{}
	for i := 0; i < 50; i++ { // 兜底页数上限，防止 cursor 异常时死循环
		page, err := s.api.ListMediaImages(ctx, pager)
		if err != nil {
			return nil, err
		}
		out = append(out, page.Images...)
		if page.NextCursor == "" || page.NextCursor == pager.Cursor {
			break
		}
		pager.Cursor = page.NextCursor
	}
	return out, nil
}

// mediaImageInfo 合并官方素材信息与本地上传记录（rec 可空）
func mediaImageInfo(img *dto.MediaImage, rec *idmap.MediaImageRecord) *onebot.MediaImageInfo {
	info := &onebot.MediaImageInfo{
		ImageID:    img.ImageID,
		URL:        img.URL,
		FileName:   img.FileName,
		PreviewURL: img.PreviewURL,
		CreatedAt:  int64(img.CreatedAt),
		ExpiresAt:  int64(img.ExpiresAt),
	}
	if rec != nil {
		info.LocalNames = rec.FileNames
		info.SHA1 = rec.SHA1
		if info.URL == "" {
			info.URL = rec.URL
		}
	}
	return info
}

// matchMediaImage 判断素材是否满足查询条件（各条件之间为「且」）
func matchMediaImage(q onebot.MediaImageQuery, img *dto.MediaImage, rec *idmap.MediaImageRecord) bool {
	if q.ImageID != "" && q.ImageID != img.ImageID {
		return false
	}
	if q.SHA1 != "" && (rec == nil || !strings.EqualFold(rec.SHA1, q.SHA1)) {
		return false
	}
	if q.FileName != "" {
		names := []string{img.FileName}
		if rec != nil {
			names = append(names, rec.FileNames...)
		}
		if !matchFileName(q.FileName, names...) {
			return false
		}
	}
	return true
}

// matchFileName 文件名匹配：原样相等，或与入站图片段的 <md5>.image 形式相等
func matchFileName(want string, names ...string) bool {
	for _, n := range names {
		if n == "" {
			continue
		}
		if n == want || onebot.ImageFileID(n) == want {
			return true
		}
	}
	return false
}

// mediaImageFileName 生成 prepare 用的文件名：保留原始文件名主体，扩展名按内容修正（须与 content_type 一致）
func mediaImageFileName(name string, data []byte) string {
	want := imageFileName(data) // image.png / image.jpg / ...
	if name == "" {
		return want
	}
	ext := want[strings.LastIndex(want, "."):]
	if strings.EqualFold(fileExt(name), ext) {
		return name
	}
	if i := strings.LastIndex(name, "."); i > 0 {
		name = name[:i]
	}
	return name + ext
}

// mediaImageBytes 取素材图片字节：base64 直接解码，URL 来源先下载（限 mediaImageMaxSize）
func mediaImageBytes(ctx context.Context, m onebot.OutMedia) ([]byte, error) {
	if m.Base64 != "" {
		data, err := base64.StdEncoding.DecodeString(m.Base64)
		if err != nil {
			return nil, fmt.Errorf("素材图片 base64 解码失败: %w", err)
		}
		return data, nil
	}
	if m.URL == "" {
		return nil, fmt.Errorf("缺少素材图片来源")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, m.URL, nil)
	if err != nil {
		return nil, err
	}
	resp, err := mediaImageFetchClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("下载素材图片失败: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("下载素材图片失败: status=%d", resp.StatusCode)
	}
	// 多读 1 字节用于判断是否超限
	data, err := io.ReadAll(io.LimitReader(resp.Body, mediaImageMaxSize+1))
	if err != nil {
		return nil, fmt.Errorf("下载素材图片失败: %w", err)
	}
	return data, nil
}

// fileExt 取文件扩展名（含点），.jpeg 归一为 .jpg
func fileExt(name string) string {
	i := strings.LastIndex(name, ".")
	if i < 0 {
		return ""
	}
	ext := name[i:]
	if strings.EqualFold(ext, ".jpeg") {
		return ".jpg"
	}
	return ext
}

// ---- 富媒体上传（群/单聊共用） ----

// uploadScene 抽象群/单聊两套富媒体上传接口（两者 file_info 不互通）
type uploadScene struct {
	label   string                                                                                   // 日志用场景名
	simple  func(ctx context.Context, rm *dto.RichMediaMessage) (*dto.Message, error)                // POST .../files
	chunked func(ctx context.Context, fileType uint64, fileName string, data []byte) ([]byte, error) // 分片上传
}

// uploadRich 上传富媒体并返回 file_info：
//   - 有 URL：交给平台拉取（官方文档的整文件上传方式）；
//   - 本地/base64 数据 > chunkThreshold：直接走分片上传（官方推荐的本地文件方式）；
//   - 其余先用 file_data 直传（官方要求 5MB 以内），失败则自动回退分片上传。
func (s *sender) uploadRich(ctx context.Context, sc uploadScene, fileType uint64, url, b64, fileName string) ([]byte, error) {
	rm := &dto.RichMediaMessage{FileType: fileType, SrvSendMsg: false}
	if fileType == 4 {
		rm.FileName = fileName // 仅文件透传文件名，避免图片/视频/语音被渲染成文件附件
	}
	if url != "" {
		rm.URL = url
		resp, err := sc.simple(ctx, rm)
		if err != nil {
			return nil, err
		}
		return resp.FileInfo, nil
	}

	data, decErr := base64.StdEncoding.DecodeString(b64)
	if decErr == nil && fileType == 1 && fileName == "" {
		fileName = imageFileName(data)
	}
	if decErr == nil && len(data) > chunkThreshold {
		return sc.chunked(ctx, fileType, fileName, data)
	}

	rm.FileData = b64
	resp, err := sc.simple(ctx, rm)
	if err == nil {
		return resp.FileInfo, nil
	}
	if decErr != nil || len(data) == 0 || ctx.Err() != nil {
		return nil, err
	}
	filelog.Warnf("[qq] %s富媒体 file_data 直传失败，改走分片上传: %v", sc.label, err)
	fi, cerr := sc.chunked(ctx, fileType, fileName, data)
	if cerr != nil {
		return nil, fmt.Errorf("file_data 直传失败: %v；分片上传回退也失败: %w", err, cerr)
	}
	return fi, nil
}

// imageFileName 按图片内容嗅探扩展名，生成分片上传所需的文件名
func imageFileName(data []byte) string {
	switch http.DetectContentType(data) {
	case "image/png":
		return "image.png"
	case "image/gif":
		return "image.gif"
	case "image/webp":
		return "image.webp"
	case "image/bmp":
		return "image.bmp"
	default:
		return "image.jpg"
	}
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

// md5Prefix10MSize 官方 upload_prepare 文档定义的 md5_10m 取样长度：文件前 10,002,432 字节
// （约 9.54MB，并非 10MiB=10,485,760）。
const md5Prefix10MSize = 10002432

// first10M 返回文件前 md5Prefix10MSize 字节（不足则全部），用于 prepare 的 md5_10m 校验
func first10M(data []byte) []byte {
	if len(data) > md5Prefix10MSize {
		return data[:md5Prefix10MSize]
	}
	return data
}
