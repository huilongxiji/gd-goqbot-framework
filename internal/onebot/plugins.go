package onebot

import (
	"context"
	"encoding/json"
	"errors"
	"strings"

	"github.com/tidwall/gjson"
	"goqbot/internal/plugins"
)

// requestScope 单次动作请求的来源权限，由请求所属 WS 连接的配置决定
type requestScope struct {
	allowSensitivePlugins bool // 是否允许调用插件声明为 sensitive 的能力（如获取 Cookie）
}

// isPluginAction 是否为插件相关动作（参数与结果可能含凭证，日志与重连缓存需特殊处理）
func isPluginAction(action string) bool {
	return action == "get_plugin_list" || action == "call_plugin"
}

// isPluginRequest 原始请求是否为插件相关动作（兼容 _async 后缀）
func isPluginRequest(payload []byte) bool {
	return isPluginAction(strings.TrimSuffix(gjson.GetBytes(payload, "action").Str, "_async"))
}

// SetPlugins 注入插件宿主。只挂接不启动，插件的启动时机由 main 决定。
func (c *Caller) SetPlugins(manager *plugins.Manager) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.plugins = manager
}

// pluginManager 返回已注入的插件宿主，未注入时为 nil
func (c *Caller) pluginManager() *plugins.Manager {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.plugins
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
	result, err := manager.Call(context.Background(), name.Str, capability.Str, args, scope.allowSensitivePlugins)
	if err != nil {
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
func pluginActionContent(params gjson.Result, ret map[string]any) string {
	target := pluginParamSummary(params)
	if ret["status"] == "ok" {
		return strings.TrimSpace("插件请求已完成 " + target)
	}
	if msg, _ := ret["message"].(string); plugins.IsCode(msg) {
		return strings.TrimSpace("插件请求失败：" + msg + " " + target)
	}
	return "插件请求失败"
}
