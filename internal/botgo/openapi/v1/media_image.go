package v1

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"

	"goqbot/internal/botgo/dto"
)

// PrepareMediaImage 素材管理-申请上传任务
func (o *openAPI) PrepareMediaImage(ctx context.Context, req *dto.MediaImagePrepareReq) (*dto.MediaImagePrepareResp, error) {
	resp, err := baseRequest(ctx,
		o.request(ctx).
			SetResult(dto.MediaImagePrepareResp{}).
			SetBody(req),
		http.MethodPost, o.getURL(mediaImagePrepareURI))
	if err != nil {
		return nil, err
	}
	return resp.Result().(*dto.MediaImagePrepareResp), nil
}

// PutMediaImage 将图片二进制 PUT 到预签名地址，返回响应头 ETag（去引号，即文件 md5）
func (o *openAPI) PutMediaImage(ctx context.Context, presignedURL, contentType string, data []byte) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPut, presignedURL, bytes.NewReader(data))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", contentType) // 必须与 prepare 的 content_type 一致
	resp, err := uploadPartClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		body, _ := io.ReadAll(resp.Body)
		return "", fmt.Errorf("upload media image failed: status=%d body=%s", resp.StatusCode, string(body))
	}
	return strings.Trim(resp.Header.Get("ETag"), `"`), nil
}

// CompleteMediaImage 素材管理-完成上传，换取引用链接
func (o *openAPI) CompleteMediaImage(ctx context.Context, req *dto.MediaImageCompleteReq) (*dto.MediaImageCompleteResp, error) {
	resp, err := baseRequest(ctx,
		o.request(ctx).
			SetResult(dto.MediaImageCompleteResp{}).
			SetBody(req),
		http.MethodPost, o.getURL(mediaImageCompleteURI))
	if err != nil {
		return nil, err
	}
	return resp.Result().(*dto.MediaImageCompleteResp), nil
}

// ListMediaImages 素材管理-查询有效图片列表（cursor 分页）
func (o *openAPI) ListMediaImages(ctx context.Context, pager *dto.MediaImageListPager) (*dto.MediaImageList, error) {
	req := o.request(ctx).SetResult(dto.MediaImageList{})
	if pager != nil && pager.Cursor != "" {
		req = req.SetQueryParam("cursor", pager.Cursor)
	}
	resp, err := baseRequest(ctx, req, http.MethodGet, o.getURL(mediaImagesURI))
	if err != nil {
		return nil, err
	}
	return resp.Result().(*dto.MediaImageList), nil
}

// DeleteMediaImage 素材管理-删除图片（释放额度；已发出的消息不受影响）
func (o *openAPI) DeleteMediaImage(ctx context.Context, imageID string) error {
	_, err := baseRequest(ctx,
		o.request(ctx).SetPathParam("image_id", imageID),
		http.MethodDelete, o.getURL(mediaImageDeleteURI))
	return err
}
