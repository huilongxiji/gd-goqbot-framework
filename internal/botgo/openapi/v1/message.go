package v1

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/go-resty/resty/v2"
	"github.com/tidwall/gjson"

	"goqbot/internal/botgo/dto"
	"goqbot/internal/botgo/errs"
	"goqbot/internal/botgo/openapi/options"
)

// Message 拉取单条消息
func (o *openAPI) Message(ctx context.Context, channelID string, messageID string, opt ...options.Option) (
	*dto.Message, error) {
	reqCMD := o.request(ctx).
		SetResult(dto.Message{}).
		SetPathParam("channel_id", channelID).
		SetPathParam("message_id", messageID)

	resp, err := baseRequest(ctx, reqCMD, http.MethodGet, o.getURL(messagesURI), opt...)
	if err != nil {
		return nil, err
	}

	// 兼容处理
	result := resp.Result().(*dto.Message)
	if result.ID == "" {
		body := gjson.Get(resp.String(), "message")
		if err := json.Unmarshal([]byte(body.String()), result); err != nil {
			return nil, err
		}
	}
	return result, nil
}

// Messages 拉取消息列表
func (o *openAPI) Messages(ctx context.Context, channelID string, pager *dto.MessagesPager, opt ...options.Option) (
	[]*dto.Message, error) {
	if pager == nil {
		return nil, errs.ErrPagerIsNil
	}
	reqCMD := o.request(ctx).
		SetPathParam("channel_id", channelID).
		SetQueryParams(pager.QueryParams())

	resp, err := baseRequest(ctx, reqCMD, http.MethodGet, o.getURL(messagesURI), opt...)
	if err != nil {
		return nil, err
	}
	messages := make([]*dto.Message, 0)
	if err := json.Unmarshal(resp.Body(), &messages); err != nil {
		return nil, err
	}
	return messages, nil
}

// PostMessage 发消息
func (o *openAPI) PostMessage(ctx context.Context, channelID string, msg *dto.MessageToCreate,
	opt ...options.Option) (*dto.Message, error) {
	reqCMD := o.request(ctx).
		SetResult(dto.Message{}).
		SetPathParam("channel_id", channelID).
		SetBody(msg)

	resp, err := baseRequest(ctx, reqCMD, http.MethodPost, o.getURL(messagesURI), opt...)
	if err != nil {
		return nil, err
	}
	return resp.Result().(*dto.Message), nil
}

// PatchMessage 编辑消息
func (o *openAPI) PatchMessage(ctx context.Context,
	channelID string, messageID string, msg *dto.MessageToCreate, opt ...options.Option) (*dto.Message, error) {
	reqCMD := o.request(ctx).
		SetResult(dto.Message{}).
		SetPathParam("channel_id", channelID).
		SetPathParam("message_id", messageID).
		SetBody(msg)

	resp, err := baseRequest(ctx, reqCMD, http.MethodPatch, o.getURL(messageURI), opt...)
	if err != nil {
		return nil, err
	}

	return resp.Result().(*dto.Message), nil
}

// RetractMessage 撤回消息
func (o *openAPI) RetractMessage(ctx context.Context,
	channelID, msgID string, opt ...options.Option) error {
	reqCMD := o.request(ctx).
		SetPathParam("channel_id", channelID).
		SetPathParam("message_id", string(msgID))

	_, err := baseRequest(ctx, reqCMD, http.MethodDelete, o.getURL(messageURI), opt...)
	return err
}

// RetractC2CMessage 撤回C2C消息
func (o *openAPI) RetractC2CMessage(ctx context.Context,
	userID, msgID string, opt ...options.Option) error {
	reqCMD := o.request(ctx).
		SetPathParam("user_id", userID).
		SetPathParam("message_id", msgID)
	_, err := baseRequest(ctx, reqCMD, http.MethodDelete, o.getURL(retractC2cMessageURI), opt...)
	return err
}

// RetractGroupMessage 撤回群消息
func (o *openAPI) RetractGroupMessage(ctx context.Context,
	groupID, msgID string, opt ...options.Option) error {
	reqCMD := o.request(ctx).
		SetPathParam("group_id", groupID).
		SetPathParam("message_id", msgID)

	_, err := baseRequest(ctx, reqCMD, http.MethodDelete, o.getURL(retractGroupMessageURI), opt...)
	return err
}

// PostSettingGuide 发送设置引导消息, atUserID为要at的用户
func (o *openAPI) PostSettingGuide(ctx context.Context,
	channelID string, atUserIDs []string, opt ...options.Option) (*dto.Message, error) {
	var content string
	for _, userID := range atUserIDs {
		content += fmt.Sprintf("<@%s>", userID)
	}
	msg := &dto.SettingGuideToCreate{
		Content: content,
	}
	reqCMD := o.request(ctx).
		SetResult(dto.Message{}).
		SetPathParam("channel_id", channelID).
		SetBody(msg)

	resp, err := baseRequest(ctx, reqCMD, http.MethodPost, o.getURL(settingGuideURI), opt...)
	if err != nil {
		return nil, err
	}
	return resp.Result().(*dto.Message), nil
}

func getGroupURIBySendType(msgType dto.SendType) uri {
	switch msgType {
	case dto.RichMedia:
		return groupRichMediaURI
	default:
		return groupMessagesURI
	}
}

// PostGroupMessage 回复群消息
func (o *openAPI) PostGroupMessage(ctx context.Context, groupID string, msg dto.APIMessage,
	opt ...options.Option) (*dto.Message, error) {
	reqCMD := o.request(ctx).
		SetResult(dto.Message{}).
		SetPathParam("group_id", groupID).
		SetBody(msg)

	resp, err := baseRequest(ctx, reqCMD, http.MethodPost, o.getURL(getGroupURIBySendType(msg.GetSendType())), opt...)
	if err != nil {
		return nil, err
	}
	return resp.Result().(*dto.Message), nil
}

func getC2CURIBySendType(msgType dto.SendType) uri {
	switch msgType {
	case dto.RichMedia:
		return c2cRichMediaURI
	default:
		return c2cMessagesURI
	}
}

// PostC2CMessage 回复C2C消息
func (o *openAPI) PostC2CMessage(ctx context.Context, userID string, msg dto.APIMessage,
	opt ...options.Option) (*dto.Message, error) {
	reqCMD := o.request(ctx).
		SetResult(dto.Message{}).
		SetPathParam("user_id", userID).
		SetBody(msg)

	resp, err := baseRequest(ctx, reqCMD, http.MethodPost, o.getURL(getC2CURIBySendType(msg.GetSendType())), opt...)
	if err != nil {
		return nil, err
	}
	return resp.Result().(*dto.Message), nil
}

// ---- 富媒体分片上传 ----

// PostGroupUploadPrepare 群分片上传-准备，返回 upload_id 与预签名分片列表
func (o *openAPI) PostGroupUploadPrepare(ctx context.Context, groupID string, req *dto.RichMediaPrepareReq,
	opt ...options.Option) (*dto.RichMediaPrepareResp, error) {
	reqCMD := o.request(ctx).
		SetResult(dto.RichMediaPrepareResp{}).
		SetPathParam("group_id", groupID).
		SetBody(req)
	resp, err := baseRequest(ctx, reqCMD, http.MethodPost, o.getURL(groupUploadPrepareURI), opt...)
	if err != nil {
		return nil, err
	}
	return resp.Result().(*dto.RichMediaPrepareResp), nil
}

// PostC2CUploadPrepare 单聊分片上传-准备
func (o *openAPI) PostC2CUploadPrepare(ctx context.Context, userID string, req *dto.RichMediaPrepareReq,
	opt ...options.Option) (*dto.RichMediaPrepareResp, error) {
	reqCMD := o.request(ctx).
		SetResult(dto.RichMediaPrepareResp{}).
		SetPathParam("user_id", userID).
		SetBody(req)
	resp, err := baseRequest(ctx, reqCMD, http.MethodPost, o.getURL(c2cUploadPrepareURI), opt...)
	if err != nil {
		return nil, err
	}
	return resp.Result().(*dto.RichMediaPrepareResp), nil
}

// PostGroupUploadPartFinish 群分片上传-单片完成确认
func (o *openAPI) PostGroupUploadPartFinish(ctx context.Context, groupID string, req *dto.RichMediaPartFinishReq,
	opt ...options.Option) error {
	reqCMD := o.request(ctx).
		SetPathParam("group_id", groupID).
		SetBody(req)
	_, err := baseRequest(ctx, reqCMD, http.MethodPost, o.getURL(groupUploadPartFinishURI), opt...)
	return err
}

// PostC2CUploadPartFinish 单聊分片上传-单片完成确认
func (o *openAPI) PostC2CUploadPartFinish(ctx context.Context, userID string, req *dto.RichMediaPartFinishReq,
	opt ...options.Option) error {
	reqCMD := o.request(ctx).
		SetPathParam("user_id", userID).
		SetBody(req)
	_, err := baseRequest(ctx, reqCMD, http.MethodPost, o.getURL(c2cUploadPartFinishURI), opt...)
	return err
}

// PostGroupFileByUploadID 群分片全部完成后，用 upload_id 换取 file_info
func (o *openAPI) PostGroupFileByUploadID(ctx context.Context, groupID, uploadID string,
	opt ...options.Option) (*dto.Message, error) {
	reqCMD := o.request(ctx).
		SetResult(dto.Message{}).
		SetPathParam("group_id", groupID).
		SetBody(&dto.RichMediaByUploadIDReq{UploadID: uploadID})
	resp, err := baseRequest(ctx, reqCMD, http.MethodPost, o.getURL(groupRichMediaURI), opt...)
	if err != nil {
		return nil, err
	}
	return resp.Result().(*dto.Message), nil
}

// PostC2CFileByUploadID 单聊分片全部完成后，用 upload_id 换取 file_info
func (o *openAPI) PostC2CFileByUploadID(ctx context.Context, userID, uploadID string,
	opt ...options.Option) (*dto.Message, error) {
	reqCMD := o.request(ctx).
		SetResult(dto.Message{}).
		SetPathParam("user_id", userID).
		SetBody(&dto.RichMediaByUploadIDReq{UploadID: uploadID})
	resp, err := baseRequest(ctx, reqCMD, http.MethodPost, o.getURL(c2cRichMediaURI), opt...)
	if err != nil {
		return nil, err
	}
	return resp.Result().(*dto.Message), nil
}

// uploadPartClient 用于分片 PUT 到腾讯预签名地址：这些 URL 自带签名鉴权，
// 不能附带 bot 的 Authorization 头（会与预签名冲突），故用独立裸 http.Client。
var uploadPartClient = &http.Client{Timeout: 120 * time.Second}

// PutUploadPart 将单个分片二进制 PUT 到预签名地址
func (o *openAPI) PutUploadPart(ctx context.Context, presignedURL string, data []byte) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodPut, presignedURL, bytes.NewReader(data))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/octet-stream")
	resp, err := uploadPartClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		body, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("upload part failed: status=%d body=%s", resp.StatusCode, string(body))
	}
	return nil
}

func baseRequest(ctx context.Context, reqCMD *resty.Request, method, url string, opt ...options.Option) (
	*resty.Response, error) {
	opts := getOptions(ctx, opt...)
	if opts.URL != "" {
		url = opts.URL
	}
	if opts.HideTip {
		reqCMD = reqCMD.SetQueryParam("hidetip", "true")
	}

	return reqCMD.Execute(method, url)
}

func getOptions(_ context.Context, opt ...options.Option) *options.Options {
	opts := &options.Options{}
	for _, o := range opt {
		o(opts)
	}
	return opts
}
