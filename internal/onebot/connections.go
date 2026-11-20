package onebot

import (
	"encoding/json"
	"log"
	"sync"

	"goqbot/internal/config"
	"goqbot/internal/filelog"
)

// Connections 管理全部下游连接（正向 WS 监听、反向 WS 客户端）。
// Apply 按新配置做差异更新：没变的连接保持不动，删掉的停止，新增或改过的重新启动。
// 每条连接可以有自己的 access-token / heartbeat-interval，没写的用 onebot 下的全局值；
// 判断是否改过时按实际生效的值比较，所以改全局值只会重启沿用全局值的连接。
type Connections struct {
	selfID int64
	bus    *EventBus
	caller *Caller

	mu      sync.Mutex
	applied bool
	entries []*connEntry // 按配置顺序
}

// connEntry 配置里的一条连接（servers 的一项可以同时写 ws 和 ws-reverse，各算一条）
type connEntry struct {
	index     int    // 在 onebot.servers 里的位置
	kind      string // ws / ws-reverse
	key       string // 配置内容（不含名称），用来判断是否改过
	name      string
	disabled  bool
	address   string
	token     string // 实际生效的口令
	heartbeat int    // 实际生效的心跳间隔（毫秒），负数=不发
	err       string // 启动失败的原因
	server    *WSServer
	client    *WSClient
}

// ApplyResult Apply 的结果
type ApplyResult struct {
	Started int      `json:"started"` // 新启动的连接数
	Stopped int      `json:"stopped"` // 停止的连接数
	Kept    int      `json:"kept"`    // 没变、保持运行的连接数
	Errors  []string `json:"errors"`  // 启动失败的连接
}

// NewConnections 创建连接管理器
func NewConnections(selfID int64, bus *EventBus, caller *Caller) *Connections {
	return &Connections{selfID: selfID, bus: bus, caller: caller}
}

// entryKey 连接配置（不含名称）加上实际生效的口令和心跳，用来判断连接是否要重启
func entryKey(v any, token string, heartbeat int) string {
	b, _ := json.Marshal(struct {
		C  any    `json:"c"`
		T  string `json:"t"`
		HB int    `json:"hb"`
	}{v, token, heartbeat})
	return string(b)
}

// Apply 按 ob 更新下游连接
func (m *Connections) Apply(ob config.OneBotConfig) ApplyResult {
	m.mu.Lock()
	defer m.mu.Unlock()

	// 旧连接按配置内容分组，内容相同的可以直接沿用
	pool := map[string][]*connEntry{}
	for _, e := range m.entries {
		if e.err == "" { // 上次启动失败的不沿用，借这次保存重试
			pool[e.kind+e.key] = append(pool[e.kind+e.key], e)
		}
	}
	take := func(kind, key string) *connEntry {
		list := pool[kind+key]
		if len(list) == 0 {
			return nil
		}
		pool[kind+key] = list[1:]
		return list[0]
	}

	var next, fresh []*connEntry
	res := ApplyResult{Errors: []string{}}
	for i, srv := range ob.Servers {
		if srv.WS != nil {
			ws := *srv.WS
			named := ws
			named.Name = "" // 只改名称不重启连接
			named.AccessToken, named.HeartbeatInterval = nil, nil
			token, hb := ob.Effective(ws.AccessToken, ws.HeartbeatInterval)
			key := entryKey(named, token, hb)
			if e := take("ws", key); e != nil {
				e.index = i
				e.name = ws.Name
				next = append(next, e)
				if !e.disabled {
					res.Kept++
				}
			} else {
				e := &connEntry{index: i, kind: "ws", key: key, name: ws.Name, disabled: ws.Disabled, address: ws.Address, token: token, heartbeat: hb}
				next = append(next, e)
				fresh = append(fresh, e)
			}
		}
		if srv.WSReverse != nil {
			rev := *srv.WSReverse
			named := rev
			named.Name = ""
			named.AccessToken, named.HeartbeatInterval = nil, nil
			token, hb := ob.Effective(rev.AccessToken, rev.HeartbeatInterval)
			key := entryKey(named, token, hb)
			if e := take("ws-reverse", key); e != nil {
				e.index = i
				e.name = rev.Name
				next = append(next, e)
				if !e.disabled {
					res.Kept++
				}
			} else {
				addr := rev.Universal
				if addr == "" {
					addr = rev.Event
				}
				if addr == "" {
					addr = rev.API
				}
				e := &connEntry{index: i, kind: "ws-reverse", key: key, name: rev.Name, disabled: rev.Disabled, address: addr, token: token, heartbeat: hb}
				next = append(next, e)
				fresh = append(fresh, e)
			}
		}
	}

	// 先停掉不再需要的连接（释放端口），再启动新的
	for _, list := range pool {
		for _, e := range list {
			if m.stopEntry(e) {
				res.Stopped++
			}
		}
	}
	cfgs := ob.Servers
	for _, e := range fresh {
		if e.disabled {
			continue
		}
		srv := cfgs[e.index]
		if e.kind == "ws" {
			ws := *srv.WS
			warnSensitive(e, ws.AllowSensitivePlugins)
			s := NewWSServer(&ws, connOneBot(&ob, e), m.selfID, m.bus, m.caller)
			if err := s.Start(); err != nil {
				e.err = err.Error()
				filelog.Errorf("[onebot] 正向 WS 启动失败 %s: %v", ws.Address, err)
				res.Errors = append(res.Errors, "正向 WS "+ws.Address+"："+err.Error())
				continue
			}
			e.server = s
		} else {
			rev := *srv.WSReverse
			warnSensitive(e, rev.AllowSensitivePlugins)
			c := NewWSClient(&rev, connOneBot(&ob, e), m.selfID, m.bus, m.caller)
			c.StartHeartbeat()
			c.Run()
			e.client = c
		}
		res.Started++
	}
	m.entries = next
	if m.applied && (res.Started > 0 || res.Stopped > 0) {
		log.Printf("[onebot] 下游连接已更新：新启动 %d 条，停止 %d 条，保持 %d 条", res.Started, res.Stopped, res.Kept)
	}
	m.applied = true
	return res
}

// connOneBot 每条连接持有一份自己的配置（填入这条连接实际生效的口令和心跳），
// 之后改配置不会和正在运行的连接并发读写
func connOneBot(ob *config.OneBotConfig, e *connEntry) *config.OneBotConfig {
	c := *ob
	c.Servers = nil
	c.AccessToken = e.token
	c.HeartbeatInterval = e.heartbeat
	return &c
}

// warnSensitive 允许敏感能力但没有口令时，敏感能力不会开放给这条连接，提醒一下
func warnSensitive(e *connEntry, allow bool) {
	if allow && e.token == "" {
		label := e.name
		if label == "" {
			label = e.address
		}
		filelog.Warnf("[onebot] 连接 %s 允许调用插件的敏感能力，但没有设置 access-token，敏感能力不会开放给它", label)
	}
}

// stopEntry 停止一条连接，返回它原本是否在运行
func (m *Connections) stopEntry(e *connEntry) bool {
	running := false
	if e.server != nil {
		e.server.Stop()
		e.server = nil
		running = true
	}
	if e.client != nil {
		e.client.Stop()
		e.client = nil
		running = true
		log.Printf("[onebot] 反向 WS 已停止: %s", e.address)
	}
	return running
}

// Status 每条连接的状态，按配置顺序。index 是它在 onebot.servers 里的位置
func (m *Connections) Status() []map[string]any {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]map[string]any, 0, len(m.entries))
	for _, e := range m.entries {
		var st map[string]any
		switch {
		case e.server != nil:
			st = e.server.Status()
		case e.client != nil:
			st = e.client.Status()
		default:
			st = map[string]any{"type": e.kind, "address": e.address, "connected": false}
			if e.kind == "ws" {
				st["clients"] = 0
			}
		}
		st["index"] = e.index
		st["name"] = e.name
		st["token_set"] = e.token != ""
		st["heartbeat_interval"] = e.heartbeat
		st["disabled"] = e.disabled
		st["running"] = e.server != nil || e.client != nil
		if e.err != "" {
			st["error"] = e.err
		}
		out = append(out, st)
	}
	return out
}

// StopAll 停止全部连接（退出、重启前调用）
func (m *Connections) StopAll() {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, e := range m.entries {
		m.stopEntry(e)
	}
}
