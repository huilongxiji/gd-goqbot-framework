package onebot

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/tidwall/gjson"
	"goqbot/internal/filelog"
	"goqbot/internal/plugins"
)

// builtinActionNames 全部内置动作名（含别名），必须与 Caller.dispatch 的 case 保持一致。
// 插件声明的能力与它们重名时不注册；插件也通过这份名单调用内置动作。
var builtinActionNames = []string{
	"get_login_info", "get_status", "get_version_info",
	"send_group_msg", "send_private_msg", "send_msg", "send_group_active_msg", "send_private_active_msg",
	"put_interaction", "send_interaction_ack", "delete_msg", "get_msg", "get_forward_msg", "get_openid",
	"get_gateway", "get_gateway_bot", "_get_gateway", "get_session_limit",
	"get_group_info", "get_group_bot_state", "get_group_member_info", "get_group_member_list",
	"get_group_join_requests", "get_group_system_msg", "set_group_add_request", "set_group_ban",
	"get_group_ban", "get_group_mute", "set_group_whole_ban", "get_c2c_msg_state", "get_friend_msg_state",
	"get_menu", "set_menu", "get_command_panels", "get_command_panel", "create_command_panel",
	"set_command_panel", "delete_command_panel", "set_command_panel_target",
	"upload_media_image", "get_media_image", "get_media_images", "delete_media_image",
	"get_plugin_list", "call_plugin",
}

// requestScope 单次动作请求的来源与权限
type requestScope struct {
	allowSensitivePlugins bool   // 是否允许调用插件声明为 sensitive 的能力（如获取 Cookie），由下游连接的配置决定
	plugin                string // 非空=由该插件经 goqbot 发起
}

// isPluginAction 是否为插件相关动作：get_plugin_list、call_plugin，以及插件注册的动作。
// 这些动作的参数与结果可能含凭证，日志只记录名字，响应不进入重连重发缓存。
func (c *Caller) isPluginAction(action string) bool {
	if action == "get_plugin_list" || action == "call_plugin" {
		return true
	}
	if m := c.pluginManager(); m != nil {
		return m.HasAction(action)
	}
	return false
}

// isPluginRequest 原始请求是否为插件相关动作（兼容 _async 后缀）
func (c *Caller) isPluginRequest(payload []byte) bool {
	return c.isPluginAction(strings.TrimSuffix(gjson.GetBytes(payload, "action").Str, "_async"))
}

// SetPlugins 注入插件宿主，并把自己作为内置动作的执行者交给插件宿主。只挂接不启动，插件的启动时机由 main 决定。
func (c *Caller) SetPlugins(manager *plugins.Manager) {
	c.mu.Lock()
	c.plugins = manager
	c.mu.Unlock()
	if manager != nil {
		manager.SetActions(c)
	}
}

// pluginManager 返回已注入的插件宿主，未注入时为 nil
func (c *Caller) pluginManager() *plugins.Manager {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.plugins
}

// BuiltinActions 实现 plugins.ActionRunner：全部内置动作名
func (c *Caller) BuiltinActions() []string {
	return append([]string(nil), builtinActionNames...)
}

// RunAction 实现 plugins.ActionRunner：以插件身份执行一个内置动作。
// 插件发起的 call_plugin 不能调用敏感能力（插件调用其他插件的敏感能力要走注册表的授权）。
func (c *Caller) RunAction(plugin, action string, params json.RawMessage) plugins.ActionResult {
	if filelog.Enabled() && !c.isPluginAction(action) {
		filelog.WriteFile([]byte(filelog.FormatInfo(fmt.Sprintf(
			"[onebot] action=%s params=%s source=plugin:%s", action, string(params), plugin,
		))))
	}
	ret := c.callScoped(action, gjson.ParseBytes(params), requestScope{plugin: plugin})
	if ret["status"] == "ok" {
		data, err := json.Marshal(ret["data"])
		if err != nil {
			return plugins.ActionResult{OK: false, Retcode: 1500, Message: "结果编码失败", Wording: "结果编码失败"}
		}
		return plugins.ActionResult{OK: true, Data: data}
	}
	r := plugins.ActionResult{}
	switch v := ret["retcode"].(type) {
	case int:
		r.Retcode = v
	case int64:
		r.Retcode = int(v)
	case float64:
		r.Retcode = int(v)
	}
	r.Message, _ = ret["message"].(string)
	r.Wording, _ = ret["wording"].(string)
	if r.Wording == "" {
		r.Wording = r.Message
	}
	return r
}

// getPluginList 列出插件目录中的全部插件（含未开启的）及其状态、能力
func (c *Caller) getPluginList() map[string]any {
	if manager := c.pluginManager(); manager != nil {
		return ok(map[string]any{"plugins": manager.List()})
	}
	return ok(map[string]any{"plugins": []any{}})
}

// callPlugin 调用插件能力。params 只允许 plugin、capability、arguments 三个字段，arguments 须为对象。
// 失败时 message 为固定错误码（如 plugin_busy），wording 为中文说明，插件的错误原文一律不透传。
func (c *Caller) callPlugin(params gjson.Result, scope requestScope) map[string]any {
	if !params.IsObject() {
		return failed(1400, plugins.ErrInvalidArguments.Error())
	}
	valid := true
	params.ForEach(func(key, _ gjson.Result) bool {
		valid = key.Str == "plugin" || key.Str == "capability" || key.Str == "arguments"
		return valid
	})
	name, capability, arguments := params.Get("plugin"), params.Get("capability"), params.Get("arguments")
	if !valid || name.Type != gjson.String || capability.Type != gjson.String ||
		name.Str == "" || capability.Str == "" || (arguments.Exists() && !arguments.IsObject()) {
		return failed(1400, plugins.ErrInvalidArguments.Error())
	}
	args := json.RawMessage("{}")
	if arguments.Exists() {
		args = json.RawMessage(arguments.Raw)
	}
	manager := c.pluginManager()
	if manager == nil {
		return pluginFailed(plugins.ErrNotFound)
	}
	result, err := manager.Call(context.Background(), name.Str, capability.Str, args, scope.allowSensitivePlugins && scope.plugin == "")
	if err != nil {
		return pluginFailed(err)
	}
	return map[string]any{"status": "ok", "retcode": 0, "data": result}
}

// callPluginAction 下游直接调用插件注册的动作：参数就是插件能力的 arguments
func (c *Caller) callPluginAction(action string, params gjson.Result, scope requestScope) map[string]any {
	manager := c.pluginManager()
	if manager == nil {
		return failed(1404, "unsupported action: "+action)
	}
	args := json.RawMessage("{}")
	if params.Exists() && params.Raw != "" && params.Raw != "null" {
		if !params.IsObject() {
			return pluginFailed(plugins.ErrInvalidArguments)
		}
		args = json.RawMessage(params.Raw)
	}
	result, err := manager.CallAction(context.Background(), action, args, scope.allowSensitivePlugins && scope.plugin == "")
	if err != nil {
		if errors.Is(err, plugins.ErrCapabilityNotFound) {
			return failed(1404, "unsupported action: "+action)
		}
		return pluginFailed(err)
	}
	return map[string]any{"status": "ok", "retcode": 0, "data": result}
}

// pluginFailed 把插件错误映射为 OneBot 失败响应：
// 1400 参数错误、1403 无权调用敏感能力、1404 插件或能力不存在、1503 插件暂不可用或执行失败
func pluginFailed(err error) map[string]any {
	retcode := 1503
	switch {
	case errors.Is(err, plugins.ErrInvalidArguments):
		retcode = 1400
	case errors.Is(err, plugins.ErrForbidden):
		retcode = 1403
	case errors.Is(err, plugins.ErrNotFound), errors.Is(err, plugins.ErrCapabilityNotFound):
		retcode = 1404
	}
	ret := failed(retcode, plugins.Code(err))
	ret["wording"] = plugins.Describe(err)
	return ret
}

// pluginParamSummary 插件动作的日志参数摘要：只展示格式合法的插件名与能力名，arguments 一律省略
func pluginParamSummary(params gjson.Result) string {
	var parts []string
	if name := params.Get("plugin").Str; plugins.ValidName(name) {
		parts = append(parts, "plugin="+name)
	}
	if capability := params.Get("capability").Str; plugins.ValidCapabilityName(capability) {
		parts = append(parts, "capability="+capability)
	}
	if params.Get("arguments").Exists() {
		parts = append(parts, "arguments=[已省略]")
	}
	return strings.Join(parts, " ")
}

// pluginActionContent 插件动作日志的「内容」行：成功不展示结果，失败只展示固定错误码
func pluginActionContent(action string, params gjson.Result, ret map[string]any) string {
	target := ""
	if action == "call_plugin" || action == "get_plugin_list" {
		target = pluginParamSummary(params)
	}
	if ret["status"] == "ok" {
		return strings.TrimSpace("插件请求已完成 " + target)
	}
	if msg, _ := ret["message"].(string); plugins.IsCode(msg) {
		return strings.TrimSpace("插件请求失败：" + msg + " " + target)
	}
	if action != "call_plugin" && action != "get_plugin_list" {
		if msg, _ := ret["message"].(string); strings.HasPrefix(msg, "unsupported action") {
			return msg
		}
	}
	return "插件请求失败"
}
