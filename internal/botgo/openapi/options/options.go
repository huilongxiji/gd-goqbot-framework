// Package options 定义 openapi 客户端的可配置项（如超时、debug 等）。
package options

// Options are openapi options
type Options struct {
	URL     string
	HideTip bool // 撤回消息隐藏小灰条可选参数, true: 隐藏小灰条
}

// Option sets client options.
type Option func(*Options)

// WithURL replace default send URL
func WithURL(url string) Option {
	return func(o *Options) {
		o.URL = url
	}
}

// WithHideTip hide tip
func WithHideTip() Option {
	return func(o *Options) {
		o.HideTip = true
	}
}
