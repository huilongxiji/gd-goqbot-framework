// Package plugins 实现 goqbot 的进程外能力插件宿主。
//
// 插件是独立的可执行程序，不编译进 goqbot：每个插件占插件目录（默认可执行文件同级的 plugins/）
// 下的一个子目录，目录内的 plugin.yml 描述开关与启动方式。goqbot 启动时扫描插件目录，
// 拉起开关已打开的插件，通过标准输入输出上的 goqbot-plugin/1 协议（按行分隔的 JSON-RPC 2.0，
// 借鉴 MCP 的发现/调用方式，但不兼容 MCP）完成握手与能力调用。
// 安装 = 放入目录并打开开关；停用 = 关闭开关或把目录移出插件目录；改动在重启 goqbot 后生效。
package plugins

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"
)

const (
	// ManifestName 插件目录内的清单文件名
	ManifestName = "plugin.yml"
	// ProtocolVersion goqbot-plugin 协议版本，握手时双方必须一致
	ProtocolVersion = "1"
	// DefaultStartupTimeoutMS 握手超时默认值（毫秒）
	DefaultStartupTimeoutMS = 10000
	// DefaultCallTimeoutMS 单次能力调用超时默认值（毫秒）
	DefaultCallTimeoutMS = 20000
	// MinTimeoutMS 超时配置下限（毫秒）
	MinTimeoutMS = 1000
	// MaxTimeoutMS 超时配置上限（毫秒）
	MaxTimeoutMS = 120000
	// MaxFrameBytes 单个协议帧（一行 JSON）的最大字节数
	MaxFrameBytes = 2 * 1024 * 1024
)

var (
	// namePattern 插件实例名（= 插件目录名）规则
	namePattern = regexp.MustCompile(`^[a-z0-9_]{1,64}$`)
	// capabilityPattern 能力名规则，如 cookie.status
	capabilityPattern = regexp.MustCompile(`^[a-z][a-z0-9_.-]{0,63}$`)
)

// 插件错误码。Error() 文本是固定的英文错误码，会原样作为 OneBot 响应的 message 返回给下游，
// 因此只能是这些固定值，绝不拼接插件输出、上游响应或凭证内容。
var (
	ErrInvalidConfig      = errors.New("invalid_config")          // plugin.yml 缺失字段、格式错误或取值越界
	ErrAuthRequired       = errors.New("access_token_required")   // 开启了插件但未设置 onebot.access_token
	ErrNotFound           = errors.New("plugin_not_found")        // 没有这个名字的插件，或插件未开启
	ErrNotReady           = errors.New("plugin_not_ready")        // 插件正在启动、重启中或已出错
	ErrBusy               = errors.New("plugin_busy")             // 插件正在处理另一个请求，可稍后重试
	ErrClosed             = errors.New("plugin_closed")           // 插件宿主已关闭
	ErrTimeout            = errors.New("plugin_timeout")          // 握手或调用超时（插件进程会被结束并自动重启）
	ErrCanceled           = errors.New("plugin_canceled")         // 调用方取消了请求
	ErrStartFailed        = errors.New("plugin_start_failed")     // 插件程序无法启动（找不到命令、无执行权限等）
	ErrProcessExited      = errors.New("plugin_process_exited")   // 插件进程意外退出
	ErrProtocol           = errors.New("plugin_protocol_error")   // 插件输出不符合 goqbot-plugin/1 协议
	ErrOutputTooLarge     = errors.New("plugin_output_too_large") // 插件单帧输出超过 MaxFrameBytes
	ErrInvalidArguments   = errors.New("invalid_arguments")       // 调用参数不是 JSON 对象或过大
	ErrCapabilityNotFound = errors.New("capability_not_found")    // 插件没有声明该能力
	ErrForbidden          = errors.New("capability_forbidden")    // 敏感能力，但当前连接未开启 allow-sensitive-plugins
	ErrCallFailed         = errors.New("capability_call_failed")  // 插件返回了 JSON-RPC 错误（原文不透传）
)

// knownErrors 全部固定错误码，Code / Describe 按此顺序匹配
var knownErrors = []error{
	ErrInvalidConfig, ErrAuthRequired, ErrNotFound, ErrNotReady, ErrBusy, ErrClosed,
	ErrTimeout, ErrCanceled, ErrStartFailed, ErrProcessExited, ErrProtocol,
	ErrOutputTooLarge, ErrInvalidArguments, ErrCapabilityNotFound, ErrForbidden, ErrCallFailed,
}

// errorWording 错误码对应的中文说明（用于日志与 OneBot 响应的 wording）
var errorWording = map[error]string{
	ErrInvalidConfig:      "插件配置无效",
	ErrAuthRequired:       "未设置 onebot.access_token，插件未启动",
	ErrNotFound:           "插件不存在或未开启",
	ErrNotReady:           "插件未就绪（启动中、重启中或已出错）",
	ErrBusy:               "插件正在处理其他请求，请稍后重试",
	ErrClosed:             "插件已关闭",
	ErrTimeout:            "插件响应超时",
	ErrCanceled:           "请求已取消",
	ErrStartFailed:        "插件程序无法启动",
	ErrProcessExited:      "插件进程已退出",
	ErrProtocol:           "插件协议错误",
	ErrOutputTooLarge:     "插件输出过大",
	ErrInvalidArguments:   "调用参数无效",
	ErrCapabilityNotFound: "插件没有该能力",
	ErrForbidden:          "当前连接无权调用该敏感能力",
	ErrCallFailed:         "插件执行失败",
}

// Code 返回 err 对应的固定错误码；不是插件错误时返回 plugin_error
func Code(err error) string {
	for _, known := range knownErrors {
		if errors.Is(err, known) {
			return known.Error()
		}
	}
	return "plugin_error"
}

// Describe 返回 err 对应的中文说明
func Describe(err error) string {
	for _, known := range knownErrors {
		if errors.Is(err, known) {
			return errorWording[known]
		}
	}
	return "插件错误"
}

// IsCode 判断 s 是否为固定错误码之一（日志里只展示固定错误码，不展示任意文本）
func IsCode(s string) bool {
	for _, known := range knownErrors {
		if known.Error() == s {
			return true
		}
	}
	return false
}

// ValidName 是否为合法的插件实例名（小写字母、数字、下划线，1～64 个字符）
func ValidName(s string) bool {
	return namePattern.MatchString(s)
}

// ValidCapabilityName 是否为合法的能力名
func ValidCapabilityName(s string) bool {
	return capabilityPattern.MatchString(s)
}

// Config 单个插件的配置。Name / Dir 由目录扫描得到，其余字段来自插件目录下的 plugin.yml。
type Config struct {
	Name string `yaml:"-"` // 实例名 = 插件目录名，OneBot 调用时用它指定插件
	Dir  string `yaml:"-"` // 插件目录绝对路径：插件的工作目录，也是相对 command 的基准

	Enabled          bool              `yaml:"enabled"`            // 插件开关：true=goqbot 启动时加载
	Command          string            `yaml:"command"`            // 启动程序：裸命令名（如 python3）按 PATH 查找；带路径的相对路径以插件目录为基准
	Args             []string          `yaml:"args"`               // 原样传给程序的参数，不做 shell 展开
	Env              map[string]string `yaml:"env"`                // 额外环境变量；默认只继承 PATH、HOME、LANG 等少量变量
	StartupTimeoutMS int               `yaml:"startup_timeout_ms"` // 握手超时毫秒，0=默认 10000
	CallTimeoutMS    int               `yaml:"call_timeout_ms"`    // 单次调用超时毫秒，0=默认 20000
	LogStderr        bool              `yaml:"log_stderr"`         // true=把插件 stderr 写入日志目录下 plugins/<插件名>.log

	loadErr error // 读取或解析 plugin.yml 失败的原因；非空时该插件只展示、不启动
}

// Validate 校验插件配置。未开启的插件不校验（关掉开关即可绕过写错的配置）。
// 返回的错误都包装了 ErrInvalidConfig，文本为面向用户的中文说明。
func (c Config) Validate() error {
	if c.loadErr != nil {
		return c.loadErr
	}
	if !c.Enabled {
		return nil
	}
	if !namePattern.MatchString(c.Name) {
		return fmt.Errorf("%w: 插件目录名 %q 只能由小写字母、数字、下划线组成（1～64 个字符）", ErrInvalidConfig, c.Name)
	}
	if strings.TrimSpace(c.Command) == "" {
		return fmt.Errorf("%w: command 不能为空", ErrInvalidConfig)
	}
	for key := range c.Env {
		if key == "" || strings.ContainsAny(key, "=\x00") {
			return fmt.Errorf("%w: env 中的变量名 %q 无效", ErrInvalidConfig, key)
		}
	}
	for _, t := range []struct {
		name string
		ms   int
	}{{"startup_timeout_ms", c.StartupTimeoutMS}, {"call_timeout_ms", c.CallTimeoutMS}} {
		if t.ms != 0 && (t.ms < MinTimeoutMS || t.ms > MaxTimeoutMS) {
			return fmt.Errorf("%w: %s 必须在 %d～%d 之间（0 表示使用默认值），当前为 %d",
				ErrInvalidConfig, t.name, MinTimeoutMS, MaxTimeoutMS, t.ms)
		}
	}
	return nil
}

// Discover 扫描插件目录：每个含 plugin.yml 的子目录是一个插件，按目录名排序返回。
// 目录不存在时自动创建并返回空列表。以 . 开头的条目、普通文件与缺少 plugin.yml 的子目录会被跳过，
// 跳过原因通过 skipped 返回供启动日志提示。plugin.yml 写错的插件仍会返回（带错误状态），
// 以便在 get_plugin_list 中看到。
func Discover(dir string) (configs []Config, skipped []string, err error) {
	abs, err := filepath.Abs(dir)
	if err != nil {
		return nil, nil, err
	}
	if err := os.MkdirAll(abs, 0o755); err != nil {
		return nil, nil, fmt.Errorf("创建插件目录失败: %w", err)
	}
	entries, err := os.ReadDir(abs)
	if err != nil {
		return nil, nil, fmt.Errorf("读取插件目录失败: %w", err)
	}
	for _, e := range entries {
		name := e.Name()
		if strings.HasPrefix(name, ".") || !e.IsDir() {
			continue
		}
		pluginDir := filepath.Join(abs, name)
		manifest := filepath.Join(pluginDir, ManifestName)
		if _, statErr := os.Stat(manifest); statErr != nil {
			skipped = append(skipped, fmt.Sprintf("跳过 %s: 目录内没有 %s", name, ManifestName))
			continue
		}
		cfg, loadErr := loadManifest(manifest)
		cfg.Name, cfg.Dir = name, pluginDir
		if loadErr != nil {
			cfg.Enabled = false
			cfg.loadErr = loadErr
		}
		configs = append(configs, cfg)
	}
	sort.Slice(configs, func(i, j int) bool { return configs[i].Name < configs[j].Name })
	return configs, skipped, nil
}

// loadManifest 解析 plugin.yml；不认识的字段视为错误，避免把 enable 之类的笔误静默忽略
func loadManifest(path string) (Config, error) {
	var cfg Config
	data, err := os.ReadFile(path)
	if err != nil {
		return cfg, fmt.Errorf("%w: 读取 %s 失败: %v", ErrInvalidConfig, ManifestName, err)
	}
	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true)
	if err := dec.Decode(&cfg); err != nil {
		return Config{}, fmt.Errorf("%w: 解析 %s 失败: %v", ErrInvalidConfig, ManifestName, err)
	}
	return cfg, nil
}

// Capability 插件在握手时声明的一项能力
type Capability struct {
	Name        string          `json:"name"`         // 能力名，如 cookie.status
	Description string          `json:"description"`  // 能力说明
	InputSchema json.RawMessage `json:"input_schema"` // 参数的 JSON Schema（由插件自行校验）
	Sensitive   bool            `json:"sensitive"`    // true=返回凭证等敏感数据，仅允许开启了 allow-sensitive-plugins 的连接调用
}

// Info 插件状态快照（get_plugin_list 的返回项）
type Info struct {
	Name         string       `json:"name"`         // 实例名（插件目录名）
	Version      string       `json:"version"`      // 插件握手时上报的版本
	State        string       `json:"state"`        // 状态，见 State* 常量
	Reason       string       `json:"reason"`       // 最近一次失败的固定错误码，正常时为空
	Restarts     int          `json:"restarts"`     // 自动重启次数
	Capabilities []Capability `json:"capabilities"` // 已声明的能力（就绪后才有）
}
