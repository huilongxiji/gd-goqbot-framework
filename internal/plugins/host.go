package plugins

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"goqbot/internal/filelog"
)

// 返回给插件的 JSON-RPC 错误码
const (
	codeMethodNotFound       = -32601
	codeInvalidArguments     = -32602
	codeCapabilityNotFound   = -32002
	codeCapabilityForbidden  = -32003
	codeBusy                 = -32004
	codeNotReady             = -32005
	codeTimeout              = -32006
	codeNotInitialized       = -32007
	codeActionFailed         = -32010
	codeCapabilityCallFailed = -32011
)

// rpcError 返回给插件的 JSON-RPC error
type rpcError struct {
	Code    int            `json:"code"`
	Message string         `json:"message"`
	Data    map[string]any `json:"data,omitempty"`
}

// DetailError 带中文详情的错误：Err 是固定错误码，Detail 会放进返回给插件的 error.data.detail。
// 只用于 goqbot 自己生成的说明（如配置校验失败的原因），不能放凭证或插件 / 上游的原文。
type DetailError struct {
	Err    error
	Detail string
}

func (e *DetailError) Error() string { return e.Err.Error() + ": " + e.Detail }
func (e *DetailError) Unwrap() error { return e.Err }

// Detail 构造带详情的错误
func Detail(err error, format string, args ...any) error {
	return &DetailError{Err: err, Detail: fmt.Sprintf(format, args...)}
}

// newRPCError 把插件错误映射为 JSON-RPC error：message 与 data.code 为固定错误码
func newRPCError(err error) *rpcError {
	code := codeCapabilityCallFailed
	switch {
	case errors.Is(err, ErrInvalidArguments):
		code = codeInvalidArguments
	case errors.Is(err, ErrCapabilityNotFound), errors.Is(err, ErrNotFound):
		code = codeCapabilityNotFound
	case errors.Is(err, ErrForbidden):
		code = codeCapabilityForbidden
	case errors.Is(err, ErrBusy):
		code = codeBusy
	case errors.Is(err, ErrNotReady), errors.Is(err, ErrClosed):
		code = codeNotReady
	case errors.Is(err, ErrTimeout), errors.Is(err, ErrCanceled):
		code = codeTimeout
	}
	data := map[string]any{"code": Code(err), "wording": Describe(err)}
	var de *DetailError
	if errors.As(err, &de) {
		data["detail"] = de.Detail
	}
	return &rpcError{Code: code, Message: Code(err), Data: data}
}

// hostCapability goqbot 提供给插件的管理能力
type hostCapability struct {
	Capability
	handle func(m *Manager, ctx context.Context, p *plugin, args json.RawMessage) (any, error)
}

// hostCatalog 全部管理能力（events.* / host.* / plugins.*），按名字查找
var hostCatalog = map[string]hostCapability{}

// hostOrder 管理能力的展示顺序
var hostOrder []string

func defineHost(name, description string, sensitive bool, schema string,
	handle func(m *Manager, ctx context.Context, p *plugin, args json.RawMessage) (any, error)) {
	hostCatalog[name] = hostCapability{
		Capability: Capability{Name: name, Description: description, InputSchema: json.RawMessage(schema), Sensitive: sensitive},
		handle:     handle,
	}
	hostOrder = append(hostOrder, name)
}

func init() {
	defineHost("events.subscribe", "订阅 OneBot 事件，之后通过 notifications/event 推送", false,
		`{"type":"object","properties":{"types":{"type":"array","items":{"type":"string"}}},"additionalProperties":false}`,
		(*Manager).hostSubscribe)
	defineHost("events.unsubscribe", "取消事件订阅", false,
		`{"type":"object","properties":{"subscription_id":{"type":"string"}},"required":["subscription_id"],"additionalProperties":false}`,
		(*Manager).hostUnsubscribe)
	defineHost("events.emit", "向所有下游框架和订阅了的其他插件发出自定义事件", false,
		`{"type":"object","properties":{"event":{"type":"string"},"data":{"type":"object"}},"required":["event"],"additionalProperties":false}`,
		(*Manager).hostEmit)
	defineHost("host.info", "goqbot 版本、运行时长、QQ 连接状态、下游连接、插件概况", false,
		`{"type":"object","additionalProperties":false}`,
		func(m *Manager, _ context.Context, _ *plugin, args json.RawMessage) (any, error) {
			if err := decodeArgs(args, &struct{}{}); err != nil {
				return nil, err
			}
			return m.host.Info(), nil
		})
	defineHost("host.logs", "读取 goqbot 日志的最后若干行", true,
		`{"type":"object","properties":{"lines":{"type":"integer","minimum":1,"maximum":2000},"date":{"type":"string"}},"additionalProperties":false}`,
		func(m *Manager, _ context.Context, _ *plugin, args json.RawMessage) (any, error) {
			var a struct {
				Lines int    `json:"lines"`
				Date  string `json:"date"`
			}
			if err := decodeArgs(args, &a); err != nil {
				return nil, err
			}
			lines, err := checkLines(a.Lines)
			if err != nil {
				return nil, err
			}
			if a.Date != "" && !datePattern.MatchString(a.Date) {
				return nil, Detail(ErrInvalidArguments, "date 格式应为 YYYY-MM-DD")
			}
			out, err := m.host.Logs(lines, a.Date)
			if err != nil {
				return nil, err
			}
			return map[string]any{"lines": out}, nil
		})
	defineHost("host.config.get", "读取 config.yml：content 为原文（含注释），data 为解析后的对象", true,
		`{"type":"object","additionalProperties":false}`,
		func(m *Manager, _ context.Context, _ *plugin, args json.RawMessage) (any, error) {
			if err := decodeArgs(args, &struct{}{}); err != nil {
				return nil, err
			}
			return m.host.ConfigGet()
		})
	defineHost("host.config.set", "整份替换 config.yml（校验通过才写入，原文件备份为 config.yml.bak）", true,
		`{"type":"object","properties":{"content":{"type":"string"},"restart":{"type":"boolean"}},"required":["content"],"additionalProperties":false}`,
		func(m *Manager, _ context.Context, _ *plugin, args json.RawMessage) (any, error) {
			var a struct {
				Content *string `json:"content"`
				Restart bool    `json:"restart"`
			}
			if err := decodeArgs(args, &a); err != nil {
				return nil, err
			}
			if a.Content == nil {
				return nil, Detail(ErrInvalidArguments, "缺少 content")
			}
			return m.host.ConfigSet(*a.Content, a.Restart)
		})
	defineHost("host.config.update", "按路径修改个别配置项，保留注释和其他内容", true,
		`{"type":"object","properties":{"values":{"type":"object"},"restart":{"type":"boolean"}},"required":["values"],"additionalProperties":false}`,
		func(m *Manager, _ context.Context, _ *plugin, args json.RawMessage) (any, error) {
			var a struct {
				Values  map[string]json.RawMessage `json:"values"`
				Restart bool                       `json:"restart"`
			}
			if err := decodeArgs(args, &a); err != nil {
				return nil, err
			}
			if len(a.Values) == 0 {
				return nil, Detail(ErrInvalidArguments, "values 不能为空")
			}
			return m.host.ConfigUpdate(a.Values, a.Restart)
		})
	defineHost("host.restart", "重启 goqbot（所有插件也会重新启动）", true,
		`{"type":"object","properties":{"reason":{"type":"string"}},"additionalProperties":false}`,
		func(m *Manager, _ context.Context, p *plugin, args json.RawMessage) (any, error) {
			var a struct {
				Reason string `json:"reason"`
			}
			if err := decodeArgs(args, &a); err != nil {
				return nil, err
			}
			reason := sanitizeLog(a.Reason)
			if len([]rune(reason)) > 200 {
				reason = string([]rune(reason)[:200])
			}
			filelog.Warnf("[plugins] %s 请求重启 goqbot：%s", p.cfg.Name, reason)
			if err := m.host.Restart(reason); err != nil {
				return nil, err
			}
			return map[string]any{}, nil
		})
	defineHost("host.update", "用新程序替换当前程序并重启（goqbot 不联网下载，由插件提供文件）", true,
		`{"type":"object","properties":{"file":{"type":"string"},"sha256":{"type":"string"}},"required":["file","sha256"],"additionalProperties":false}`,
		func(m *Manager, _ context.Context, p *plugin, args json.RawMessage) (any, error) {
			var a struct {
				File   string `json:"file"`
				SHA256 string `json:"sha256"`
			}
			if err := decodeArgs(args, &a); err != nil {
				return nil, err
			}
			if a.File == "" || !sha256Pattern.MatchString(strings.ToLower(a.SHA256)) {
				return nil, Detail(ErrInvalidArguments, "需要 file 和 64 位十六进制的 sha256")
			}
			file := a.File
			if !filepath.IsAbs(file) {
				file = filepath.Join(p.cfg.Dir, file)
			}
			filelog.Warnf("[plugins] %s 请求更新 goqbot：%s", p.cfg.Name, file)
			return m.host.Update(file, strings.ToLower(a.SHA256))
		})
	defineHost("plugins.list", "列出全部插件（同 get_plugin_list）", false,
		`{"type":"object","additionalProperties":false}`,
		func(m *Manager, _ context.Context, _ *plugin, args json.RawMessage) (any, error) {
			if err := decodeArgs(args, &struct{}{}); err != nil {
				return nil, err
			}
			return map[string]any{"plugins": m.List()}, nil
		})
	defineHost("plugins.set_enabled", "修改插件的 enabled 开关（写入它的 plugin.yml），重启 goqbot 后生效", true,
		`{"type":"object","properties":{"plugin":{"type":"string"},"enabled":{"type":"boolean"}},"required":["plugin","enabled"],"additionalProperties":false}`,
		func(m *Manager, _ context.Context, p *plugin, args json.RawMessage) (any, error) {
			var a struct {
				Plugin  string `json:"plugin"`
				Enabled *bool  `json:"enabled"`
			}
			if err := decodeArgs(args, &a); err != nil {
				return nil, err
			}
			if a.Enabled == nil {
				return nil, Detail(ErrInvalidArguments, "缺少 enabled")
			}
			target := m.findAny(a.Plugin)
			if target == nil {
				return nil, ErrNotFound
			}
			if target == p {
				return nil, Detail(ErrForbidden, "不能修改自己的开关")
			}
			if err := setManifestEnabled(target.cfg.Dir, *a.Enabled); err != nil {
				return nil, Detail(ErrCallFailed, "写入 %s 失败：%v", ManifestName, err)
			}
			log.Printf("[plugins] %s 把插件 %s 的开关改为 %v（重启 goqbot 后生效）", p.cfg.Name, target.cfg.Name, *a.Enabled)
			return map[string]any{"restart_required": true}, nil
		})
	defineHost("plugins.restart", "立即重启某个已开启的插件", true,
		`{"type":"object","properties":{"plugin":{"type":"string"}},"required":["plugin"],"additionalProperties":false}`,
		func(m *Manager, _ context.Context, p *plugin, args json.RawMessage) (any, error) {
			var a struct {
				Plugin string `json:"plugin"`
			}
			if err := decodeArgs(args, &a); err != nil {
				return nil, err
			}
			m.mu.Lock()
			target := m.byName[a.Plugin]
			m.mu.Unlock()
			if target == nil {
				return nil, ErrNotFound
			}
			if target == p {
				return nil, Detail(ErrForbidden, "不能重启自己")
			}
			if err := target.requestRestart(); err != nil {
				return nil, err
			}
			return map[string]any{}, nil
		})
	defineHost("plugins.logs", "读取插件的 stderr 日志（需要该插件开启 log_stderr）", true,
		`{"type":"object","properties":{"plugin":{"type":"string"},"lines":{"type":"integer","minimum":1,"maximum":2000}},"required":["plugin"],"additionalProperties":false}`,
		func(m *Manager, _ context.Context, _ *plugin, args json.RawMessage) (any, error) {
			var a struct {
				Plugin string `json:"plugin"`
				Lines  int    `json:"lines"`
			}
			if err := decodeArgs(args, &a); err != nil {
				return nil, err
			}
			lines, err := checkLines(a.Lines)
			if err != nil {
				return nil, err
			}
			if m.findAny(a.Plugin) == nil {
				return nil, ErrNotFound
			}
			out, err := TailFile(filepath.Join(m.opts.LogDir, a.Plugin+".log"), lines)
			if err != nil {
				if os.IsNotExist(err) {
					return nil, Detail(ErrCallFailed, "没有日志（插件未开启 log_stderr）")
				}
				return nil, Detail(ErrCallFailed, "读取日志失败")
			}
			return map[string]any{"lines": out}, nil
		})
}

var (
	datePattern   = regexp.MustCompile(`^\d{4}-\d{2}-\d{2}$`)
	sha256Pattern = regexp.MustCompile(`^[0-9a-f]{64}$`)
)

// checkLines 校验 lines 参数：默认 200，最大 2000
func checkLines(n int) (int, error) {
	if n == 0 {
		return 200, nil
	}
	if n < 1 || n > 2000 {
		return 0, Detail(ErrInvalidArguments, "lines 必须在 1～2000 之间")
	}
	return n, nil
}

// decodeArgs 严格解码能力参数，失败统一为 invalid_arguments
func decodeArgs(args json.RawMessage, dest any) error {
	if len(args) == 0 {
		args = json.RawMessage(`{}`)
	}
	if err := strictDecode(args, dest); err != nil {
		return ErrInvalidArguments
	}
	return nil
}

// TailFile 读取文本文件的最后 n 行（最多读末尾 4 MiB）
func TailFile(path string, n int) ([]string, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return nil, err
	}
	const maxRead = 4 << 20
	offset := st.Size() - maxRead
	if offset < 0 {
		offset = 0
	}
	if _, err := f.Seek(offset, 0); err != nil {
		return nil, err
	}
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 64*1024), maxRead)
	var lines []string
	first := offset > 0
	for sc.Scan() {
		if first {
			first = false // 从中间开始读时第一行可能不完整
			continue
		}
		lines = append(lines, filelog.StripANSI(sc.Text()))
		if len(lines) > n {
			lines = lines[1:]
		}
	}
	if lines == nil {
		lines = []string{}
	}
	return lines, sc.Err()
}

// findAny 按名字查找插件（包括未开启、配置有误的）
func (m *Manager) findAny(name string) *plugin {
	for _, p := range m.ordered {
		if p.cfg.Name == name {
			return p
		}
	}
	return nil
}

// logConflict 记录能力重名
func logConflict(pluginName, what string) {
	filelog.Errorf("[plugins] %s 的能力 %s，未注册为动作", pluginName, what)
}

// ---------------- 权限 ----------------

// patternAllows 一组 requires / grants 是否允许调用 name：敏感能力必须逐字写出，通配符不匹配它们
func patternAllows(patterns []string, name string, sensitive bool) bool {
	for _, pat := range patterns {
		if sensitive {
			if pat == name {
				return true
			}
		} else if globMatch(pat, name) {
			return true
		}
	}
	return false
}

// allowed 插件 p 能否调用 name：requires 声明过，且没被 plugin.yml 的 grants 收紧掉
func (p *plugin) allowed(name string, sensitive bool) bool {
	p.mu.Lock()
	requires, grants := p.requires, p.cfg.Grants
	p.mu.Unlock()
	if !patternAllows(requires, name, sensitive) {
		return false
	}
	return grants == nil || patternAllows(grants, name, sensitive)
}

// effectiveGrants 日志用：插件实际获得的权限写法（requires 与 grants 的交集，grants 未设置时即 requires）
func (p *plugin) effectiveGrants() []string {
	p.mu.Lock()
	requires, grants := append([]string{}, p.requires...), p.cfg.Grants
	p.mu.Unlock()
	if grants == nil {
		return requires
	}
	// 取两边写法里更窄的那个：requires 写 *、grants 写 send_* 时显示 send_*
	var out []string
	seen := map[string]bool{}
	add := func(s string) {
		if !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}
	for _, r := range requires {
		for _, g := range grants {
			switch {
			case r == g, globMatch(g, r):
				add(r)
			case globMatch(r, g):
				add(g)
			}
		}
	}
	return out
}

// sensitiveGranted 插件获得的敏感能力（管理能力与其他插件的敏感能力），用于启动日志警告
func (m *Manager) sensitiveGranted(p *plugin) []string {
	var out []string
	for _, name := range hostOrder {
		if hc := hostCatalog[name]; hc.Sensitive && p.allowed(name, true) {
			out = append(out, name)
		}
	}
	for _, cl := range m.registeredPluginCaps() {
		if cl.owner != p && cl.cap.Sensitive && p.allowed(cl.cap.Name, true) {
			out = append(out, cl.cap.Name)
		}
	}
	return out
}

// capabilitiesFor 插件 p 能调用的全部能力（capabilities/list 与 notifications/initialized 使用）
func (m *Manager) capabilitiesFor(p *plugin) []Capability {
	out := []Capability{}
	if m.actions != nil {
		for _, name := range m.builtinNames() {
			if p.allowed(name, false) {
				out = append(out, Capability{
					Name:        name,
					Description: "OneBot 动作 " + name + "，参数与返回见 goqbot API 文档",
					InputSchema: json.RawMessage(`{"type":"object"}`),
				})
			}
		}
	}
	for _, cl := range m.registeredPluginCaps() {
		if cl.owner != p && p.allowed(cl.cap.Name, cl.cap.Sensitive) {
			out = append(out, cl.cap)
		}
	}
	m.mu.Lock()
	hasHost := m.host != nil
	m.mu.Unlock()
	for _, name := range hostOrder {
		hc := hostCatalog[name]
		if !hasHost && strings.HasPrefix(name, "host.") {
			continue
		}
		if p.allowed(name, hc.Sensitive) {
			out = append(out, hc.Capability)
		}
	}
	return out
}

// ---------------- 处理插件发来的请求 ----------------

// serve 处理协议 2 插件发来的请求
func (m *Manager) serve(ctx context.Context, p *plugin, method string, params json.RawMessage) (json.RawMessage, *rpcError) {
	switch method {
	case "ping":
		return json.RawMessage(`{}`), nil
	case "capabilities/list":
		if err := decodeArgs(params, &struct{}{}); err != nil {
			return nil, newRPCError(err)
		}
		return marshalResult(map[string]any{"capabilities": m.capabilitiesFor(p)})
	case "capabilities/call":
		var req struct {
			Name      string          `json:"name"`
			Arguments json.RawMessage `json:"arguments"`
		}
		if strictDecode(params, &req) != nil || req.Name == "" {
			return nil, newRPCError(ErrInvalidArguments)
		}
		args := req.Arguments
		if len(args) == 0 || string(bytes.TrimSpace(args)) == "null" {
			args = json.RawMessage(`{}`)
		}
		if !isObject(args) {
			return nil, newRPCError(ErrInvalidArguments)
		}
		return m.callFor(ctx, p, req.Name, args)
	}
	return nil, &rpcError{Code: codeMethodNotFound, Message: "method_not_found", Data: map[string]any{"code": "method_not_found"}}
}

// callFor 插件 p 调用注册表里的一项能力
func (m *Manager) callFor(ctx context.Context, p *plugin, name string, args json.RawMessage) (json.RawMessage, *rpcError) {
	// 1. 管理能力
	if hc, ok := hostCatalog[name]; ok {
		m.mu.Lock()
		hasHost := m.host != nil
		m.mu.Unlock()
		if !hasHost && strings.HasPrefix(name, "host.") {
			return nil, newRPCError(ErrCapabilityNotFound)
		}
		if !p.allowed(name, hc.Sensitive) {
			return nil, newRPCError(ErrForbidden)
		}
		result, err := hc.handle(m, ctx, p, args)
		if err != nil {
			return nil, newRPCError(err)
		}
		return marshalResult(result)
	}
	// 2. 内置 OneBot 动作
	m.regMu.Lock()
	builtin, actions := m.builtin[name], m.actions
	m.regMu.Unlock()
	if builtin && actions != nil {
		if !p.allowed(name, false) {
			return nil, newRPCError(ErrForbidden)
		}
		r := actions.RunAction(p.cfg.Name, name, args)
		if !r.OK {
			return nil, &rpcError{Code: codeActionFailed, Message: "action_failed", Data: map[string]any{
				"code": "action_failed", "retcode": r.Retcode, "message": r.Message, "wording": r.Wording,
			}}
		}
		return normalizeData(r.Data), nil
	}
	// 3. 其他插件注册的能力
	owner, capability, ok := m.lookup(name)
	if !ok {
		return nil, newRPCError(ErrCapabilityNotFound)
	}
	if owner == p {
		return nil, newRPCError(Detail(ErrForbidden, "不能通过 goqbot 调用自己的能力"))
	}
	if !p.allowed(name, capability.Sensitive) {
		return nil, newRPCError(ErrForbidden)
	}
	result, err := owner.call(ctx, name, args, true)
	if err != nil {
		return nil, newRPCError(err)
	}
	return result, nil
}

// normalizeData 把 OneBot 动作的 data 转成 JSON 对象：对象原样，空值为 {}，其他包成 {"value": ...}
func normalizeData(data json.RawMessage) json.RawMessage {
	trimmed := bytes.TrimSpace(data)
	if len(trimmed) == 0 || string(trimmed) == "null" {
		return json.RawMessage(`{}`)
	}
	if trimmed[0] == '{' {
		return trimmed
	}
	b, err := json.Marshal(map[string]json.RawMessage{"value": trimmed})
	if err != nil {
		return json.RawMessage(`{}`)
	}
	return b
}

// marshalResult 编码能力结果（必须是对象）
func marshalResult(v any) (json.RawMessage, *rpcError) {
	b, err := json.Marshal(v)
	if err != nil || !isObject(b) {
		return nil, newRPCError(ErrCallFailed)
	}
	return b, nil
}

// ---------------- events.* ----------------

func (m *Manager) hostSubscribe(_ context.Context, p *plugin, args json.RawMessage) (any, error) {
	var a struct {
		Types []string `json:"types"`
	}
	if err := decodeArgs(args, &a); err != nil {
		return nil, err
	}
	if len(a.Types) > 64 {
		return nil, Detail(ErrInvalidArguments, "types 最多 64 项")
	}
	for _, t := range a.Types {
		if !eventTypePattern.MatchString(t) {
			return nil, Detail(ErrInvalidArguments, "事件类型 %q 无效，应写成 post_type.子类型，如 message.group、notice.*", t)
		}
	}
	types := a.Types
	if len(types) == 0 {
		types = []string{"*"}
	}
	inst := p.currentInstance()
	if inst == nil {
		return nil, ErrNotReady
	}
	id, err := inst.subscribe(types)
	if err != nil {
		return nil, Detail(ErrInvalidArguments, "订阅数已达上限 64")
	}
	return map[string]any{"subscription_id": id}, nil
}

func (m *Manager) hostUnsubscribe(_ context.Context, p *plugin, args json.RawMessage) (any, error) {
	var a struct {
		SubscriptionID string `json:"subscription_id"`
	}
	if err := decodeArgs(args, &a); err != nil {
		return nil, err
	}
	inst := p.currentInstance()
	if inst == nil {
		return nil, ErrNotReady
	}
	if !inst.unsubscribe(a.SubscriptionID) {
		return nil, Detail(ErrInvalidArguments, "没有这个订阅号")
	}
	return map[string]any{}, nil
}

func (m *Manager) hostEmit(_ context.Context, p *plugin, args json.RawMessage) (any, error) {
	var a struct {
		Event string          `json:"event"`
		Data  json.RawMessage `json:"data"`
	}
	if err := decodeArgs(args, &a); err != nil {
		return nil, err
	}
	if !actionPattern.MatchString(a.Event) {
		return nil, Detail(ErrInvalidArguments, "event 只能由小写字母、数字、下划线组成，以字母开头")
	}
	data := a.Data
	if len(data) == 0 || string(bytes.TrimSpace(data)) == "null" {
		data = json.RawMessage(`{}`)
	}
	if !isObject(data) || len(data) > 512*1024 {
		return nil, Detail(ErrInvalidArguments, "data 必须是对象，且不超过 512 KiB")
	}
	self := int64(0)
	if m.opts.SelfID != nil {
		self = m.opts.SelfID()
	}
	raw, err := json.Marshal(map[string]any{
		"time":        time.Now().Unix(),
		"self_id":     self,
		"post_type":   "notice",
		"notice_type": "plugin_event",
		"sub_type":    a.Event,
		"plugin":      p.cfg.Name,
		"data":        data,
	})
	if err != nil {
		return nil, ErrCallFailed
	}
	m.mu.Lock()
	emit := m.emitter
	m.mu.Unlock()
	delivered := 0
	if emit != nil {
		delivered = emit(raw)
	} else {
		m.DispatchEvent(raw)
	}
	return map[string]any{"delivered": delivered}, nil
}
