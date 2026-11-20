package plugins

import (
	"context"
	"encoding/json"
	"sort"
	"sync"
)

// ActionResult 执行一个内置 OneBot 动作的结果
type ActionResult struct {
	OK      bool            // status == ok
	Data    json.RawMessage // 成功时的 data
	Retcode int             // 失败时的 retcode
	Message string          // 失败时的 message
	Wording string          // 失败时的 wording
}

// ActionRunner 内置 OneBot 动作的执行者（由 onebot.Caller 实现）
type ActionRunner interface {
	// BuiltinActions 全部内置动作名（含别名），插件的能力与它们重名时不注册
	BuiltinActions() []string
	// RunAction 以插件身份执行一个内置动作；params 为动作参数对象
	RunAction(plugin, action string, params json.RawMessage) ActionResult
}

// HostServices goqbot 自身的运行管理能力（host.*），由 main 装配
type HostServices interface {
	Info() map[string]any
	Logs(lines int, date string) (map[string]any, error) // {"date", "lines", "files"}
	ConfigGet() (map[string]any, error)
	ConfigSet(content string, restart bool) (map[string]any, error)
	ConfigUpdate(values map[string]json.RawMessage, restart bool) (map[string]any, error)
	Restart(reason string) error
	Update(file, sha256 string) (map[string]any, error)
}

// Options 插件宿主选项
type Options struct {
	HostVersion string       // 握手时告知插件的 goqbot 版本
	LogDir      string       // 插件 stderr 日志目录（plugin.yml 开启 log_stderr 时使用）
	SelfID      func() int64 // 机器人 self_id（握手、插件发出的事件里使用）
	// DownstreamSensitive 下游能否调用插件的敏感能力。具体到每条连接，还要这条连接开启了
	// allow-sensitive-plugins 并且设置了 access-token（由 OneBot 侧按连接判断）
	DownstreamSensitive bool
}

// claim 某个插件声明的一项能力，登记在注册表里
type claim struct {
	owner *plugin
	cap   Capability
}

// Manager 插件宿主：持有扫描到的全部插件，负责启动、查询、调用与关闭，并维护能力注册表
type Manager struct {
	opts Options

	mu      sync.Mutex
	started bool // 已调用 Start
	closed  bool // 已调用 Close

	ordered []*plugin          // 按插件名排序，用于 List 展示
	byName  map[string]*plugin // 已开启且配置有效的插件，用于 Call 查找

	actions ActionRunner         // 内置 OneBot 动作
	host    HostServices         // 运行管理能力
	emitter func(raw []byte) int // 把插件发出的事件推给下游，返回送达的下游连接数
	regMu   sync.Mutex           // 保护 builtin / claims（不要在持有 plugin.mu 时获取）
	builtin map[string]bool      // 内置动作名
	claims  map[string][]claim   // 能力名 -> 声明了它的协议 2 插件

	running   sync.WaitGroup // 各插件的守护循环
	closeOnce sync.Once
	done      chan struct{} // Close 时关闭，结束 ctx 监听
}

// New 按 Discover 得到的配置创建宿主，不启动任何进程
func New(configs []Config, opts Options) *Manager {
	m := &Manager{
		opts:    opts,
		byName:  make(map[string]*plugin),
		builtin: make(map[string]bool),
		claims:  make(map[string][]claim),
		done:    make(chan struct{}),
	}
	for _, cfg := range configs {
		cfg.Args = append([]string(nil), cfg.Args...)
		p := newPlugin(m, cfg)
		m.ordered = append(m.ordered, p)
		// 未开启或配置有误的插件只用于展示，不可调用
		if cfg.Enabled && p.info.State == StatePending {
			m.byName[cfg.Name] = p
		}
	}
	return m
}

// SetActions 注入内置 OneBot 动作的执行者（Start 之前调用）
func (m *Manager) SetActions(a ActionRunner) {
	m.regMu.Lock()
	defer m.regMu.Unlock()
	m.actions = a
	m.builtin = make(map[string]bool)
	for _, name := range a.BuiltinActions() {
		m.builtin[name] = true
	}
}

// SetHost 注入运行管理能力（Start 之前调用）
func (m *Manager) SetHost(h HostServices) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.host = h
}

// SetEmitter 注入事件发布函数：插件 events.emit 的事件经它推给下游（Start 之前调用）
func (m *Manager) SetEmitter(fn func(raw []byte) int) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.emitter = fn
}

// EnabledCount 已开启（开关为 true 且配置有效）的插件数量
func (m *Manager) EnabledCount() int {
	return len(m.byName)
}

// Start 在后台拉起所有已开启的插件，立即返回。单个插件失败不影响 goqbot 与其他插件，
// 状态可通过 List 查看；可恢复的失败会自动重启。ctx 结束时自动 Close。
func (m *Manager) Start(ctx context.Context) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.started || m.closed {
		return
	}
	m.started = true
	for _, p := range m.ordered {
		if m.byName[p.cfg.Name] != p {
			continue
		}
		m.running.Add(1)
		go func() {
			defer m.running.Done()
			p.run(ctx)
		}()
	}
	go func() {
		select {
		case <-ctx.Done():
			m.Close()
		case <-m.done:
		}
	}()
}

// List 返回全部插件（含未开启的）的状态快照
func (m *Manager) List() []Info {
	result := make([]Info, 0, len(m.ordered))
	for _, p := range m.ordered {
		info := p.snapshot()
		info.Permissions = m.permissions(p, info.Requires)
		result = append(result, info)
	}
	return result
}

// Call 调用插件的一项能力（call_plugin）。allowSensitive 表示发起请求的下游连接是否开启了 allow-sensitive-plugins。
func (m *Manager) Call(ctx context.Context, name, capability string, args json.RawMessage, allowSensitive bool) (json.RawMessage, error) {
	m.mu.Lock()
	closed := m.closed
	p, ok := m.byName[name]
	m.mu.Unlock()
	if closed {
		return nil, ErrClosed
	}
	if !ok {
		return nil, ErrNotFound
	}
	return p.call(ctx, capability, args, allowSensitive && m.opts.DownstreamSensitive)
}

// HasAction 是否有插件把 action 注册成了 OneBot 动作
func (m *Manager) HasAction(action string) bool {
	_, _, ok := m.lookup(action)
	return ok
}

// CallAction 下游直接调用插件注册的 OneBot 动作
func (m *Manager) CallAction(ctx context.Context, action string, args json.RawMessage, allowSensitive bool) (json.RawMessage, error) {
	m.mu.Lock()
	closed := m.closed
	m.mu.Unlock()
	if closed {
		return nil, ErrClosed
	}
	owner, _, ok := m.lookup(action)
	if !ok {
		return nil, ErrCapabilityNotFound
	}
	return owner.call(ctx, action, args, allowSensitive && m.opts.DownstreamSensitive)
}

// DispatchEvent 把一个 OneBot 事件推给订阅了它的协议 2 插件（事件总线回调，不计入下游送达数）
func (m *Manager) DispatchEvent(raw []byte) {
	var head struct {
		PostType      string `json:"post_type"`
		MessageType   string `json:"message_type"`
		NoticeType    string `json:"notice_type"`
		RequestType   string `json:"request_type"`
		MetaEventType string `json:"meta_event_type"`
		Plugin        string `json:"plugin"`
	}
	if json.Unmarshal(raw, &head) != nil || head.PostType == "" {
		return
	}
	sub := head.MessageType + head.NoticeType + head.RequestType + head.MetaEventType
	eventType := head.PostType + "." + sub
	heartbeat := eventType == "meta_event.heartbeat"
	for _, p := range m.ordered {
		if m.byName[p.cfg.Name] != p {
			continue
		}
		if head.NoticeType == "plugin_event" && head.Plugin == p.cfg.Name {
			continue // 插件发出的事件不推回给它自己
		}
		inst := p.currentInstance()
		if inst == nil || !p.allowed("events.subscribe", false) { // 权限被收回后立即停止推送
			continue
		}
		id := inst.matchSubscription(eventType)
		if id == "" || (heartbeat && !inst.subscribedExactly(id, eventType)) {
			continue
		}
		frame, err := json.Marshal(struct {
			JSONRPC string `json:"jsonrpc"`
			Method  string `json:"method"`
			Params  struct {
				SubscriptionID string          `json:"subscription_id"`
				Event          json.RawMessage `json:"event"`
			} `json:"params"`
		}{JSONRPC: "2.0", Method: "notifications/event", Params: struct {
			SubscriptionID string          `json:"subscription_id"`
			Event          json.RawMessage `json:"event"`
		}{id, json.RawMessage(raw)}})
		if err != nil {
			continue
		}
		if len(frame) > MaxFrameBytes {
			p.noteDropped(1)
			continue
		}
		inst.pushEvent(frame)
	}
}

// subscribedExactly 订阅 id 是否逐字写了该事件类型（心跳只推给明确订阅了的插件）
func (inst *instance) subscribedExactly(id, eventType string) bool {
	inst.mu.Lock()
	defer inst.mu.Unlock()
	for _, t := range inst.subs[id] {
		if t == eventType {
			return true
		}
	}
	return false
}

// Close 关闭全部插件并等待进程回收，可重复调用
func (m *Manager) Close() {
	m.closeOnce.Do(func() {
		m.mu.Lock()
		m.closed = true
		close(m.done)
		m.mu.Unlock()
		var wg sync.WaitGroup
		for _, p := range m.ordered {
			wg.Add(1)
			go func() {
				defer wg.Done()
				p.shutdown()
			}()
		}
		wg.Wait()
		m.running.Wait()
	})
}

// ---------------- 能力注册表 ----------------

// setClaims 登记协议 2 插件声明的能力（替换它之前的登记）
func (m *Manager) setClaims(p *plugin, caps []Capability) {
	m.regMu.Lock()
	m.removeClaimsLocked(p)
	for _, c := range caps {
		m.claims[c.Name] = append(m.claims[c.Name], claim{owner: p, cap: c})
	}
	var conflicts []string
	for _, c := range caps {
		switch {
		case m.builtin[c.Name]:
			conflicts = append(conflicts, c.Name+"（与内置动作重名）")
		case len(m.claims[c.Name]) > 1:
			others := ""
			for _, cl := range m.claims[c.Name] {
				if cl.owner != p {
					if others != "" {
						others += "、"
					}
					others += cl.owner.cfg.Name
				}
			}
			conflicts = append(conflicts, c.Name+"（与插件 "+others+" 重名）")
		}
	}
	m.regMu.Unlock()
	for _, c := range conflicts {
		logConflict(p.cfg.Name, c)
	}
}

// clearClaims 移除插件在注册表里的全部登记
func (m *Manager) clearClaims(p *plugin) {
	m.regMu.Lock()
	changed := m.removeClaimsLocked(p)
	m.regMu.Unlock()
	if changed {
		m.notifyListChanged(p)
	}
}

func (m *Manager) removeClaimsLocked(p *plugin) bool {
	changed := false
	for name, list := range m.claims {
		kept := list[:0]
		for _, cl := range list {
			if cl.owner == p {
				changed = true
				continue
			}
			kept = append(kept, cl)
		}
		if len(kept) == 0 {
			delete(m.claims, name)
		} else {
			m.claims[name] = kept
		}
	}
	return changed
}

// regStatus 插件 p 的能力 name 是否注册成了 OneBot 动作；没有时给出原因
func (m *Manager) regStatus(p *plugin, name string) (bool, string) {
	m.regMu.Lock()
	defer m.regMu.Unlock()
	if m.builtin[name] {
		return false, "conflict_builtin"
	}
	list := m.claims[name]
	if len(list) == 1 && list[0].owner == p {
		return true, ""
	}
	if len(list) > 1 {
		return false, "conflict_plugin"
	}
	return false, ""
}

// lookup 查找注册成 OneBot 动作的插件能力
func (m *Manager) lookup(name string) (*plugin, Capability, bool) {
	m.regMu.Lock()
	defer m.regMu.Unlock()
	if m.builtin[name] {
		return nil, Capability{}, false
	}
	list := m.claims[name]
	if len(list) != 1 {
		return nil, Capability{}, false
	}
	return list[0].owner, list[0].cap, true
}

// registeredPluginCaps 已注册的插件能力（按名字排序）
func (m *Manager) registeredPluginCaps() []claim {
	m.regMu.Lock()
	defer m.regMu.Unlock()
	var out []claim
	for name, list := range m.claims {
		if len(list) == 1 && !m.builtin[name] {
			out = append(out, list[0])
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].cap.Name < out[j].cap.Name })
	return out
}

// builtinNames 内置动作名（排序后）
func (m *Manager) builtinNames() []string {
	m.regMu.Lock()
	defer m.regMu.Unlock()
	names := make([]string, 0, len(m.builtin))
	for name := range m.builtin {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// notifyListChanged 注册表有变化时通知其他已就绪的协议 2 插件
func (m *Manager) notifyListChanged(except *plugin) {
	for _, p := range m.ordered {
		if p == except || m.byName[p.cfg.Name] != p {
			continue
		}
		inst := p.currentInstance()
		if inst == nil {
			continue
		}
		inst.mu.Lock()
		ok := inst.initialized && !inst.quitting
		inst.mu.Unlock()
		if ok {
			go inst.notify("notifications/capabilities/list_changed", struct{}{})
		}
	}
}
