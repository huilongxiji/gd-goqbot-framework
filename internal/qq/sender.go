package qq

import (
	"context"
	"fmt"
	"time"

	"goqbot/internal/botgo/dto"
	"goqbot/internal/botgo/dto/keyboard"
	"goqbot/internal/botgo/openapi"

	"goqbot/internal/idmap"
	"goqbot/internal/onebot"
)

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

// ---- 群聊 ----

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

// sendGroupText 发送群文本消息，返回 QQ 消息 id
func (s *sender) sendGroupText(ctx context.Context, groupOpenID, msgID, eventID, content string) (string, error) {
	m := &dto.MessageToCreate{
		Content:   content,
		MsgType:   dto.TextMsg,
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

// sendGroupMedia 用已上传的 file_info 发送群富媒体（图片等）消息
func (s *sender) sendGroupMedia(ctx context.Context, groupOpenID, msgID, eventID, content string, fileInfo []byte) (string, error) {
	m := &dto.MessageToCreate{
		Content:   content,
		MsgType:   dto.RichMediaMsg,
		Media:     &dto.MediaInfo{FileInfo: fileInfo},
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

// sendGroupMarkdown 发送群 markdown 消息（可带 keyboard 按钮）
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

// SendGroup 按 Parts 顺序逐条发送，每个段对应一条独立 QQ 消息
func (s *sender) SendGroup(ctx context.Context, groupOpenID string, msg *onebot.OutMessage) (string, error) {
	var lastID string
	for _, p := range msg.Parts {
		switch p.Kind {
		case onebot.PartText:
			if p.Text == "" {
				continue
			}
			id, err := s.sendGroupText(ctx, groupOpenID, msg.MsgID, msg.EventID, p.Text)
			if err != nil {
				return "", err
			}
			lastID = id
		case onebot.PartImage:
			fi, err := s.uploadGroupImage(ctx, groupOpenID, p.Image)
			if err != nil {
				return "", err
			}
			id, err := s.sendGroupMedia(ctx, groupOpenID, msg.MsgID, msg.EventID, " ", fi)
			if err != nil {
				return "", err
			}
			lastID = id
		case onebot.PartMarkdown:
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

// sendC2CText 发送单聊文本消息，返回 QQ 消息 id
func (s *sender) sendC2CText(ctx context.Context, userOpenID, msgID, eventID, content string) (string, error) {
	m := &dto.MessageToCreate{
		Content:   content,
		MsgType:   dto.TextMsg,
		MsgID:     msgID,
		EventID:   eventID,
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
func (s *sender) sendC2CMedia(ctx context.Context, userOpenID, msgID, eventID, content string, fileInfo []byte) (string, error) {
	m := &dto.MessageToCreate{
		Content:   content,
		MsgType:   dto.RichMediaMsg,
		Media:     &dto.MediaInfo{FileInfo: fileInfo},
		MsgID:     msgID,
		EventID:   eventID,
		MsgSeq:    s.seqFor(seqKey(msgID, eventID)),
		Timestamp: time.Now().Unix(),
	}
	resp, err := s.api.PostC2CMessage(ctx, userOpenID, m)
	if err != nil {
		return "", err
	}
	return resp.ID, nil
}

// sendC2CMarkdown 发送单聊 markdown 消息（可带 keyboard 按钮）
func (s *sender) sendC2CMarkdown(ctx context.Context, userOpenID, msgID, eventID string, md *dto.Markdown, kb *keyboard.MessageKeyboard) (string, error) {
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
	resp, err := s.api.PostC2CMessage(ctx, userOpenID, m)
	if err != nil {
		return "", err
	}
	return resp.ID, nil
}

// SendC2C 按 Parts 顺序逐条发送，每个段对应一条独立 QQ 消息
func (s *sender) SendC2C(ctx context.Context, userOpenID string, msg *onebot.OutMessage) (string, error) {
	var lastID string
	for _, p := range msg.Parts {
		switch p.Kind {
		case onebot.PartText:
			if p.Text == "" {
				continue
			}
			id, err := s.sendC2CText(ctx, userOpenID, msg.MsgID, msg.EventID, p.Text)
			if err != nil {
				return "", err
			}
			lastID = id
		case onebot.PartImage:
			fi, err := s.uploadC2CImage(ctx, userOpenID, p.Image)
			if err != nil {
				return "", err
			}
			id, err := s.sendC2CMedia(ctx, userOpenID, msg.MsgID, msg.EventID, " ", fi)
			if err != nil {
				return "", err
			}
			lastID = id
		case onebot.PartMarkdown:
			id, err := s.sendC2CMarkdown(ctx, userOpenID, msg.MsgID, msg.EventID, p.Markdown, p.Keyboard)
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
