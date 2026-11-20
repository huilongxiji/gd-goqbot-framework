// Package plugins 实现 goqbot 的进程外能力插件宿主。
//
// 插件是独立的可执行程序，不编译进 goqbot：放进插件目录（默认可执行文件同级的 plugins/）即可，
// 可以是单个可执行文件，也可以是一个文件夹（里面可同时放多个平台的版本，goqbot 自动挑出本机能用的）。
// goqbot 启动时扫描插件目录，拉起插件，通过标准输入输出上的 goqbot-plugin 协议（按行分隔的 JSON-RPC 2.0，
// 借鉴 MCP 的「发现能力 → 调用能力」方式但不兼容 MCP）完成握手：
//
//   - 协议 2：双向对称。插件声明的能力注册进能力注册表，成为 OneBot 动作，下游框架和其他插件都能直接调用；
//     插件也能调用注册表里的能力（全部 OneBot 动作、事件订阅、运行管理、其他插件的能力）。
//   - 协议 1：只能由 goqbot 调用插件，下游通过 call_plugin 调用（与 1.12.0 一致）。
//
// 插件工作目录内可选的 plugin.yml 用于停用插件、收紧权限、调整超时；不写时全部使用默认值。
package plugins

import (
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"
)

const (
	// ManifestName 插件工作目录内的可选配置文件名
	ManifestName = "plugin.yml"
	// ProtocolV1 协议 1：只能由 goqbot 调用插件
	ProtocolV1 = "1"
	// ProtocolV2 协议 2：双向对称
	ProtocolV2 = "2"
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
	// namePattern 插件实例名（= 插件文件名去掉平台后缀，或文件夹名）规则
	namePattern = regexp.MustCompile(`^[a-z0-9_]{1,64}$`)
	// capabilityPattern 协议 1 的能力名规则，如 cookie.status
	capabilityPattern = regexp.MustCompile(`^[a-z][a-z0-9_.-]{0,63}$`)
	// actionPattern 协议 2 的能力名规则：同时就是 OneBot 动作名，如 get_group_album_list
	// （内置启动器 runner/goqbot-runner.mjs 里有同样的规则，修改时两边一起改）
	actionPattern = regexp.MustCompile(`^[a-z][a-z0-9_]{0,63}$`)
	// grantPattern requires / grants 中的一项：能力名，可带 * 通配
	grantPattern = regexp.MustCompile(`^[a-z0-9_.*-]{1,128}$`)
	// eventTypePattern events.subscribe 的事件类型，如 message.group、notice.*
	eventTypePattern = regexp.MustCompile(`^[a-z0-9_.*]{1,64}$`)
)

// 插件错误码。Error() 文本是固定的英文错误码，会原样作为 OneBot 响应的 message 返回给下游，
// 因此只能是这些固定值，绝不拼接插件输出、上游响应或凭证内容。
var (
	ErrInvalidConfig      = errors.New("invalid_config")           // plugin.yml 格式错误、取值越界，或插件文件冲突
	ErrNotFound           = errors.New("plugin_not_found")         // 没有这个名字的插件，或插件未开启
	ErrNotReady           = errors.New("plugin_not_ready")         // 插件正在启动、重启中或已出错
	ErrBusy               = errors.New("plugin_busy")              // 插件同时处理的请求已达上限，可稍后重试
	ErrClosed             = errors.New("plugin_closed")            // 插件宿主已关闭
	ErrTimeout            = errors.New("plugin_timeout")           // 握手或调用超时
	ErrCanceled           = errors.New("plugin_canceled")          // 调用方取消了请求
	ErrStartFailed        = errors.New("plugin_start_failed")      // 插件程序无法启动（找不到可执行文件、无执行权限等）
	ErrProcessExited      = errors.New("plugin_process_exited")    // 插件进程意外退出
	ErrProtocol           = errors.New("plugin_protocol_error")    // 插件输出不符合 goqbot-plugin 协议
	ErrOutputTooLarge     = errors.New("plugin_output_too_large")  // 插件单帧输出超过 MaxFrameBytes
	ErrRestartRequested   = errors.New("plugin_restart_requested") // 被 plugins.restart 要求重启
	ErrInvalidArguments   = errors.New("invalid_arguments")        // 调用参数不是 JSON 对象、过大或格式不对
	ErrCapabilityNotFound = errors.New("capability_not_found")     // 没有这个能力
	ErrForbidden          = errors.New("capability_forbidden")     // 没有调用权限（敏感能力未授权、插件未声明需要等）
	ErrCallFailed         = errors.New("capability_call_failed")   // 能力提供方返回了错误（原文不透传）
)

// knownErrors 全部固定错误码，Code / Describe 按此顺序匹配
var knownErrors = []error{
	ErrInvalidConfig, ErrNotFound, ErrNotReady, ErrBusy, ErrClosed,
	ErrTimeout, ErrCanceled, ErrStartFailed, ErrProcessExited, ErrProtocol,
	ErrOutputTooLarge, ErrRestartRequested, ErrInvalidArguments, ErrCapabilityNotFound, ErrForbidden, ErrCallFailed,
}

// errorWording 错误码对应的中文说明（用于日志与 OneBot 响应的 wording）
var errorWording = map[error]string{
	ErrInvalidConfig:      "插件配置无效",
	ErrNotFound:           "插件不存在或未开启",
	ErrNotReady:           "插件未就绪（启动中、重启中或已出错）",
	ErrBusy:               "插件正在处理的请求已达上限，请稍后重试",
	ErrClosed:             "插件已关闭",
	ErrTimeout:            "插件响应超时",
	ErrCanceled:           "请求已取消",
	ErrStartFailed:        "插件程序无法启动",
	ErrProcessExited:      "插件进程已退出",
	ErrProtocol:           "插件协议错误",
	ErrOutputTooLarge:     "插件输出过大",
	ErrRestartRequested:   "插件按要求重启",
	ErrInvalidArguments:   "调用参数无效",
	ErrCapabilityNotFound: "没有该能力",
	ErrForbidden:          "无权调用该能力",
	ErrCallFailed:         "能力执行失败",
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

// ValidCapabilityName 是否为合法的能力名（协议 1 规则，包含协议 2 的全部能力名）
func ValidCapabilityName(s string) bool {
	return capabilityPattern.MatchString(s)
}

// ValidActionName 是否可能是插件注册的 OneBot 动作名（协议 2 能力名规则）
func ValidActionName(s string) bool {
	return actionPattern.MatchString(s)
}

// Config 单个插件的配置。Name / Dir / Kind / Command / DataDir 由目录扫描得到，其余字段来自可选的 plugin.yml。
type Config struct {
	Name    string // 实例名，下游和其他插件用它指定插件
	Dir     string // 插件工作目录（绝对路径）
	Kind    string // 插件种类：KindExecutable / KindJS
	Command string // 启动程序（绝对路径，或开发调试时 plugin.yml 里写的 command；JS / TS 插件为 Node）
	Entry   string // JS / TS 插件的入口文件（绝对路径）
	DataDir string // 数据目录 plugin-data/<插件名>/（绝对路径），插件更新时不会被覆盖

	Enabled          bool              // 插件开关，默认 true
	Args             []string          // 原样传给程序的参数，不做 shell 展开（仅开发调试时由 plugin.yml 指定）
	Grants           []string          // 收紧权限：nil=不限制（按插件声明授权）
	Env              map[string]string // 额外环境变量；默认只继承 PATH、HOME、LANG 等少量变量
	StartupTimeoutMS int               // 握手超时毫秒，0=默认 10000
	CallTimeoutMS    int               // 单次调用超时毫秒，0=默认 20000
	LogStderr        bool              // true=把插件 stderr 写入日志目录下 plugins/<插件名>.log

	loadErr error // 发现或解析失败的原因；非空时该插件只展示、不启动
}

// manifest plugin.yml 的内容，全部可选
type manifest struct {
	Enabled          *bool             `yaml:"enabled"`
	Command          string            `yaml:"command"`
	Args             []string          `yaml:"args"`
	Grants           *[]string         `yaml:"grants"`
	Env              map[string]string `yaml:"env"`
	StartupTimeoutMS int               `yaml:"startup_timeout_ms"`
	CallTimeoutMS    int               `yaml:"call_timeout_ms"`
	LogStderr        bool              `yaml:"log_stderr"`
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
		return fmt.Errorf("%w: 插件名 %q 只能由小写字母、数字、下划线组成（1～64 个字符）", ErrInvalidConfig, c.Name)
	}
	if strings.TrimSpace(c.Command) == "" {
		return fmt.Errorf("%w: 没有找到可执行文件", ErrInvalidConfig)
	}
	for key := range c.Env {
		if key == "" || strings.ContainsAny(key, "=\x00") {
			return fmt.Errorf("%w: env 中的变量名 %q 无效", ErrInvalidConfig, key)
		}
	}
	for _, g := range c.Grants {
		if !grantPattern.MatchString(g) {
			return fmt.Errorf("%w: grants 中的 %q 无效（只能由小写字母、数字、下划线、点、连字符和 * 组成）", ErrInvalidConfig, g)
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

// Capability 一项能力的声明（插件握手时声明，或 goqbot 提供给插件）
type Capability struct {
	Name         string          `json:"name"`                    // 能力名
	Description  string          `json:"description"`             // 能力说明
	InputSchema  json.RawMessage `json:"input_schema"`            // 参数的 JSON Schema（由提供方自行校验）
	OutputSchema json.RawMessage `json:"output_schema,omitempty"` // 返回结果的 JSON Schema，只用于展示
	Sensitive    bool            `json:"sensitive"`               // true=敏感能力（返回凭证或能改动 goqbot）
}

// CapabilityInfo get_plugin_list 中的一项能力：声明内容加上注册情况
type CapabilityInfo struct {
	Capability
	Registered bool   `json:"registered"` // 是否已注册成 OneBot 动作
	Reason     string `json:"reason"`     // 未注册的原因：conflict_builtin / conflict_plugin / protocol_v1
}

// EventDecl 插件声明可能发出的事件（只用于展示）
type EventDecl struct {
	Name        string `json:"name"`
	Description string `json:"description"`
}

// Info 插件状态快照（get_plugin_list 的返回项）
// Permission 插件声明需要的一项能力，以及它现在是否被允许
type Permission struct {
	Name      string `json:"name"`      // 能力名，或带 * 的通配写法
	Sensitive bool   `json:"sensitive"` // 是敏感能力（敏感能力不会被通配符匹配）
	Wildcard  bool   `json:"wildcard"`  // 是通配写法
	Granted   bool   `json:"granted"`   // 现在是否允许（没被 grants 收紧掉）
}

type Info struct {
	Name            string           `json:"name"`             // 实例名
	Kind            string           `json:"kind"`             // 插件种类：js / executable
	Version         string           `json:"version"`          // 插件握手时上报的版本
	ProtocolVersion string           `json:"protocol_version"` // 握手协商出的协议版本，未握手时为空
	State           string           `json:"state"`            // 状态，见 State* 常量
	Reason          string           `json:"reason"`           // 最近一次失败的固定错误码，正常时为空
	Restarts        int              `json:"restarts"`         // 自动重启次数
	Capabilities    []CapabilityInfo `json:"capabilities"`     // 已声明的能力（就绪后才有）
	Events          []EventDecl      `json:"events"`           // 声明可能发出的事件
	Requires        []string         `json:"requires"`         // 插件声明需要调用的能力
	Grants          []string         `json:"grants"`           // plugin.yml 收紧后的权限；null=不限制
	Permissions     []Permission     `json:"permissions"`      // requires 里每一项的授权情况（plugins.list 返回）
	DroppedEvents   int64            `json:"dropped_events"`   // 来不及推送而丢弃的事件数
}
