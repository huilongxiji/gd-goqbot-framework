// hello_demo：goqbot-plugin/2 最小示例插件（Go 编写，编译后是单个可执行文件，放进 plugins/ 即可运行）。
//
// 演示内容：
//  1. 提供能力 hello_echo：载入后成为 OneBot 动作，下游可直接 call_api("hello_echo", text="你好")。
//  2. 订阅群消息：有人发 /hello 时，调用 goqbot 的 send_group_msg 引用回复。
//  3. 回复后调用 events.emit 发出 hello_replied 事件，所有下游框架都会收到。
//
// 协议要点：标准输入输出、每行一条 JSON-RPC 2.0 消息；双方都能发请求、响应和通知。
// stdout 只能输出协议消息，日志写 stderr 或用 notifications/log。
//
// 编译：./build.sh（输出 hello_demo-<系统>-<架构>，与 goqbot 的命名方式一致）。
package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"sync"
	"time"
)

const (
	protocolVersion = "2"
	pluginName      = "hello_demo"
	pluginVersion   = "0.1.0"
	maxConcurrency  = 4
	callTimeout     = 30 * time.Second // 本插件调用 goqbot 时自己掌握的超时
	maxFrameBytes   = 2 << 20          // 单条消息上限 2 MiB
)

// capabilities 本插件提供的能力（握手时告诉 goqbot）
var capabilities = []map[string]any{{
	"name":        "hello_echo",
	"description": "原样返回传入的文字，用来测试插件能力调用",
	"input_schema": map[string]any{
		"type":                 "object",
		"properties":           map[string]any{"text": map[string]any{"type": "string"}},
		"required":             []string{"text"},
		"additionalProperties": false,
	},
	"output_schema": map[string]any{
		"type": "object",
		"properties": map[string]any{
			"text": map[string]any{"type": "string"},
			"from": map[string]any{"type": "string"},
		},
	},
	"sensitive": false,
}}

// requires 本插件需要调用的 goqbot 能力（握手时声明，goqbot 自动授权并写进启动日志）
var requires = []string{"send_group_msg", "events.subscribe", "events.emit"}

// events 本插件可能发出的事件（只用于展示）
var events = []map[string]any{{"name": "hello_replied", "description": "插件回复了一次 /hello"}}

// rpcError 能力调用失败：作为 JSON-RPC error 返回，或在调用 goqbot 失败时返回给调用方
type rpcError struct {
	Code    int             `json:"code"`
	Message string          `json:"message"`
	Data    json.RawMessage `json:"data,omitempty"`
}

func (e *rpcError) Error() string { return fmt.Sprintf("%d %s %s", e.Code, e.Message, e.Data) }

// frame 一条 JSON-RPC 消息（请求、响应、通知共用）
type frame struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      *int64          `json:"id,omitempty"`
	Method  string          `json:"method,omitempty"`
	Params  json.RawMessage `json:"params,omitempty"`
	Result  json.RawMessage `json:"result,omitempty"`
	Error   *rpcError       `json:"error,omitempty"`
}

// peer 双向 JSON-RPC 连接：处理 goqbot 发来的请求和通知，也能主动调用 goqbot
type peer struct {
	writeMu sync.Mutex
	out     *bufio.Writer

	mu      sync.Mutex
	nextID  int64
	pending map[int64]chan frame // 本插件发出、尚未收到响应的请求

	slots chan struct{} // 限制同时处理的 goqbot 请求数
}

func newPeer() *peer {
	return &peer{
		out:     bufio.NewWriter(os.Stdout),
		pending: make(map[int64]chan frame),
		slots:   make(chan struct{}, maxConcurrency),
	}
}

func logf(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "[hello_demo] "+format+"\n", args...)
}

// ---------- 发送 ----------

func (p *peer) send(f frame) {
	f.JSONRPC = "2.0"
	b, err := json.Marshal(f)
	if err != nil {
		logf("编码消息失败: %v", err)
		return
	}
	p.writeMu.Lock()
	defer p.writeMu.Unlock()
	_, _ = p.out.Write(append(b, '\n'))
	_ = p.out.Flush()
}

func raw(v any) json.RawMessage {
	b, _ := json.Marshal(v)
	return b
}

func (p *peer) notify(method string, params any) {
	p.send(frame{Method: method, Params: raw(params)})
}

// request 向 goqbot 发请求并等待结果
func (p *peer) request(method string, params any) (json.RawMessage, error) {
	p.mu.Lock()
	p.nextID++
	id := p.nextID
	ch := make(chan frame, 1)
	p.pending[id] = ch
	p.mu.Unlock()
	defer func() {
		p.mu.Lock()
		delete(p.pending, id)
		p.mu.Unlock()
	}()

	p.send(frame{ID: &id, Method: method, Params: raw(params)})
	select {
	case resp := <-ch:
		if resp.Error != nil {
			return nil, resp.Error
		}
		return resp.Result, nil
	case <-time.After(callTimeout):
		// 不等了：告诉 goqbot 不再需要这个结果
		p.notify("notifications/cancelled", map[string]any{"request_id": id})
		return nil, &rpcError{Code: -32006, Message: "timeout"}
	}
}

// call 调用 goqbot 注册表里的一项能力（OneBot 动作、其他插件的能力、events.* 等）
func (p *peer) call(name string, arguments any) (json.RawMessage, error) {
	return p.request("capabilities/call", map[string]any{"name": name, "arguments": arguments})
}

func (p *peer) log(level, message string) {
	p.notify("notifications/log", map[string]any{"level": level, "message": message})
}

// ---------- 接收 ----------

func (p *peer) serve() {
	sc := bufio.NewScanner(os.Stdin)
	sc.Buffer(make([]byte, 64*1024), maxFrameBytes+1)
	for sc.Scan() {
		line := sc.Bytes()
		if len(strings.TrimSpace(string(line))) == 0 {
			continue
		}
		var f frame
		if err := json.Unmarshal(line, &f); err != nil {
			logf("收到无法解析的消息: %v", err)
			continue
		}
		switch {
		case f.Method != "" && f.ID != nil: // goqbot 发来的请求：另起 goroutine，处理时可能还要调用 goqbot
			go p.handleRequest(f)
		case f.Method != "": // 通知
			go p.handleNotification(f)
		case f.ID != nil: // 响应
			p.mu.Lock()
			ch := p.pending[*f.ID]
			p.mu.Unlock()
			if ch != nil {
				ch <- f // 已超时或取消的请求查不到，迟到的响应直接忽略
			}
		}
	}
	// stdin 关闭：goqbot 已退出，插件也退出
}

func (p *peer) handleRequest(f frame) {
	select {
	case p.slots <- struct{}{}:
		defer func() { <-p.slots }()
	default:
		p.send(frame{ID: f.ID, Error: &rpcError{Code: -32004, Message: "busy"}})
		return
	}
	result, err := p.dispatch(f.Method, f.Params)
	if err != nil {
		e, ok := err.(*rpcError)
		if !ok { // 未预料的错误只写 stderr，不把原文发出去
			logf("处理 %s 出错: %v", f.Method, err)
			e = &rpcError{Code: -32000, Message: "internal_error"}
		}
		p.send(frame{ID: f.ID, Error: e})
		return
	}
	p.send(frame{ID: f.ID, Result: raw(result)})
}

func (p *peer) handleNotification(f frame) {
	switch f.Method {
	case "notifications/initialized":
		onReady(p)
	case "notifications/event":
		var params struct {
			Event json.RawMessage `json:"event"`
		}
		if json.Unmarshal(f.Params, &params) == nil {
			onEvent(p, params.Event)
		}
		// notifications/cancelled、notifications/capabilities/list_changed 本示例不需要处理
	}
}

// dispatch 处理 goqbot 发来的请求
func (p *peer) dispatch(method string, params json.RawMessage) (any, error) {
	switch method {
	case "initialize":
		return map[string]any{
			"protocol_version": protocolVersion,
			"name":             pluginName,
			"version":          pluginVersion,
			"max_concurrency":  maxConcurrency,
			"capabilities":     capabilities,
			"requires":         requires,
			"events":           events,
		}, nil
	case "capabilities/list":
		return map[string]any{"capabilities": capabilities}, nil
	case "capabilities/call":
		var req struct {
			Name      string          `json:"name"`
			Arguments json.RawMessage `json:"arguments"`
		}
		if err := json.Unmarshal(params, &req); err != nil {
			return nil, &rpcError{Code: -32602, Message: "invalid_arguments"}
		}
		return callCapability(req.Name, req.Arguments)
	case "ping", "shutdown":
		return map[string]any{}, nil
	}
	return nil, &rpcError{Code: -32601, Message: "method_not_found"}
}

// ================= 业务逻辑 =================

// callCapability 本插件提供的能力被调用（来自下游框架或其他插件，经 goqbot 转发）
func callCapability(name string, arguments json.RawMessage) (any, error) {
	switch name {
	case "hello_echo":
		var args struct {
			Text *string `json:"text"`
		}
		dec := json.NewDecoder(strings.NewReader(string(arguments)))
		dec.DisallowUnknownFields()
		if err := dec.Decode(&args); err != nil || args.Text == nil {
			return nil, &rpcError{Code: -32602, Message: "invalid_arguments"}
		}
		return map[string]any{"text": *args.Text, "from": pluginName}, nil
	}
	return nil, &rpcError{Code: -32002, Message: "capability_not_found"}
}

// onReady 握手完成后：订阅群消息
func onReady(p *peer) {
	res, err := p.call("events.subscribe", map[string]any{"types": []string{"message.group"}})
	if err != nil {
		logf("订阅群消息失败: %v", err)
		return
	}
	var sub struct {
		SubscriptionID string `json:"subscription_id"`
	}
	_ = json.Unmarshal(res, &sub)
	p.log("info", "已订阅群消息，订阅号 "+sub.SubscriptionID)
}

// onEvent 收到订阅的事件
func onEvent(p *peer, data json.RawMessage) {
	var ev struct {
		PostType    string `json:"post_type"`
		MessageType string `json:"message_type"`
		GroupID     int64  `json:"group_id"`
		MessageID   int64  `json:"message_id"`
		RawMessage  string `json:"raw_message"`
	}
	if json.Unmarshal(data, &ev) != nil || ev.PostType != "message" || ev.MessageType != "group" {
		return
	}
	if strings.TrimSpace(ev.RawMessage) != "/hello" {
		return
	}
	// 引用对方的消息回复：goqbot 会自动使用这条消息的被动回复凭证
	res, err := p.call("send_group_msg", map[string]any{
		"group_id": ev.GroupID,
		"message": []map[string]any{
			{"type": "reply", "data": map[string]any{"id": fmt.Sprint(ev.MessageID)}},
			{"type": "text", "data": map[string]any{"text": "你好，我是 hello_demo 插件"}},
		},
	})
	if err != nil {
		p.log("warning", "回复 /hello 失败："+err.Error())
		return
	}
	var sent struct {
		MessageID int64 `json:"message_id"`
	}
	_ = json.Unmarshal(res, &sent)
	// 通知所有下游框架
	if _, err := p.call("events.emit", map[string]any{
		"event": "hello_replied",
		"data":  map[string]any{"group_id": ev.GroupID, "message_id": sent.MessageID},
	}); err != nil {
		p.log("warning", "发出 hello_replied 事件失败："+err.Error())
	}
}

func main() {
	newPeer().serve()
}
