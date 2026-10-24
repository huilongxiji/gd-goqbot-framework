package dto

// 素材管理相关结构（对齐 /v2/media-images 接口）。
// 上传流程：prepare 拿 upload_id 与预签名地址 → PUT 原始字节到 presigned_url → complete 换取引用链接。
// 引用链接形如 qqbot-media://image/v1/<token>，可在 markdown 消息中以 ![描述 #宽px #高px](url) 引用。

// MediaImagePrepareReq 申请上传任务 POST /v2/media-images/prepare
type MediaImagePrepareReq struct {
	FileName    string `json:"file_name"`    // 文件名（含扩展名）
	FileSize    uint64 `json:"file_size"`    // 文件字节数
	ContentType string `json:"content_type"` // image/png / image/jpeg / image/gif / image/webp
	MD5         string `json:"md5"`          // 全文件 md5（hex，32 位）
	SHA1        string `json:"sha1"`         // 全文件 sha1（hex，40 位）
}

// MediaImagePrepareResp 申请上传任务响应
type MediaImagePrepareResp struct {
	UploadID     string    `json:"upload_id"`     // 上传任务 ID（4 小时内有效）
	PresignedURL string    `json:"presigned_url"` // 预签名 PUT 地址（直传 COS，不走鉴权域名）
	ExpiresIn    FlexInt64 `json:"expires_in"`    // 预签名地址有效期（秒）
}

// MediaImageCompleteReq 完成上传 POST /v2/media-images/complete
type MediaImageCompleteReq struct {
	UploadID string `json:"upload_id"`
	MD5      string `json:"md5"` // PUT 响应头 ETag（去引号），服务端会与存储对象校验
}

// MediaImageCompleteResp 完成上传响应
type MediaImageCompleteResp struct {
	ImageID    string    `json:"image_id"`
	URL        string    `json:"url"`         // qqbot-media://image/v1/<token> 引用链接
	TTL        FlexInt64 `json:"ttl"`         // 有效期（秒），默认 90 天
	PreviewURL string    `json:"preview_url"` // 短时预览链接，勿持久化
}

// MediaImage 素材图片
type MediaImage struct {
	ImageID    string    `json:"image_id"`
	URL        string    `json:"url"` // 引用链接
	FileName   string    `json:"file_name"`
	PreviewURL string    `json:"preview_url"` // 短时预览链接，勿持久化
	CreatedAt  FlexInt64 `json:"created_at"`  // Unix 秒
	ExpiresAt  FlexInt64 `json:"expires_at"`  // Unix 秒
}

// MediaImageList 素材列表响应 GET /v2/media-images（按创建时间倒序）
type MediaImageList struct {
	Images     []*MediaImage `json:"images"`
	NextCursor string        `json:"next_cursor"` // 为空表示已是最后一页
	Total      FlexInt64     `json:"total"`
}

// MediaImageListPager 素材列表分页参数
type MediaImageListPager struct {
	Cursor string `json:"cursor,omitempty"`
}
