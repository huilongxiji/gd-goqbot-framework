package dto

import (
	"strconv"
	"strings"
)

// 富媒体分片上传相关结构（对齐 QQ 群/单聊 upload_prepare / upload_part_finish / files 接口）。
// 用于本地大文件（简单 base64/URL 上传体积受限时）的分片直传：
//   prepare 拿 upload_id 与预签名分片 → 逐片 PUT 到 presigned_url → part_finish 确认 → files(upload_id) 换 file_info。

// FlexInt64 兼容 QQ 响应里以字符串或数字两种形式返回的整数（如 block_size 常以 "8388608" 字符串下发）。
type FlexInt64 int64

// UnmarshalJSON 同时接受 123 与 "123"
func (f *FlexInt64) UnmarshalJSON(b []byte) error {
	s := strings.TrimSpace(string(b))
	if s == "" || s == "null" {
		*f = 0
		return nil
	}
	s = strings.Trim(s, `"`)
	if s == "" {
		*f = 0
		return nil
	}
	n, err := strconv.ParseInt(s, 10, 64)
	if err != nil {
		return err
	}
	*f = FlexInt64(n)
	return nil
}

// RichMediaPrepareReq 分片上传-准备请求
type RichMediaPrepareReq struct {
	FileType uint64 `json:"file_type"` // 1图片 2视频 3语音 4文件
	FileName string `json:"file_name"` // 文件名（含扩展名），file_type=4 必填
	FileSize int64  `json:"file_size"` // 文件总字节数
	MD5      string `json:"md5"`       // 全文件 md5（hex）
	SHA1     string `json:"sha1"`      // 全文件 sha1（hex）
	MD510M   string `json:"md5_10m"`   // 前 10MB 的 md5（hex）
}

// UploadPartItem 单个分片的上传目标
type UploadPartItem struct {
	Index        FlexInt64 `json:"index"`         // 分片序号
	PresignedURL string    `json:"presigned_url"` // 该分片的预签名 PUT 地址（直传腾讯 OSS，不走鉴权域名）
}

// UploadConfig 分片上传的并发/重试参数
type UploadConfig struct {
	Concurrency  FlexInt64 `json:"concurrency"`   // 建议并发数
	RetryTimeout FlexInt64 `json:"retry_timeout"` // 重试超时（秒）
	RetryDelay   FlexInt64 `json:"retry_delay"`   // 重试间隔（秒）
}

// RichMediaPrepareResp 分片上传-准备响应
type RichMediaPrepareResp struct {
	UploadID     string           `json:"upload_id"`
	BlockSize    FlexInt64        `json:"block_size"` // 每片字节数（最后一片可小于该值）；QQ 常以字符串下发
	Parts        []UploadPartItem `json:"parts"`
	UploadConfig UploadConfig     `json:"upload_config"`
}

// RichMediaPartFinishReq 分片上传-单片完成确认
type RichMediaPartFinishReq struct {
	UploadID  string `json:"upload_id"`
	PartIndex int    `json:"part_index"`
	BlockSize int64  `json:"block_size"` // 该片实际字节数
	MD5       string `json:"md5"`        // 该片 md5（hex）
}

// RichMediaByUploadIDReq 分片全部完成后，用 upload_id 换取 file_info
type RichMediaByUploadIDReq struct {
	UploadID string `json:"upload_id"`
}
