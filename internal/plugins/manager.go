package plugins

import (
	"context"
	"encoding/json"
	"sync"
)

// Options 插件宿主选项
type Options struct {
	HostVersion string // 握手时告知插件的 goqbot 版本
	LogDir      string // 插件 stderr 日志目录（plugin.yml 开启 log_stderr 时使用）
}

// Manager 插件宿主：持有扫描到的全部插件，负责启动、查询、调用与关闭
type Manager struct {
	mu      sync.Mutex
	started bool // 已调用 Start
	closed  bool // 已调用 Close

	ordered []*plugin          // 按目录名排序，用于 List 展示
	byName  map[string]*plugin // 已开启且配置有效的插件，用于 Call 查找

	running   sync.WaitGroup // 各插件的守护循环
	closeOnce sync.Once
	done      chan struct{} // Close 时关闭，结束 ctx 监听
}

// New 按 Discover 得到的配置创建宿主，不启动任何进程
func New(configs []Config, opts Options) *Manager {
	m := &Manager{byName: make(map[string]*plugin), done: make(chan struct{})}
	for _, cfg := range configs {
		cfg.Args = append([]string(nil), cfg.Args...)
		p := newPlugin(cfg, opts)
		m.ordered = append(m.ordered, p)
		// 未开启或配置有误的插件只用于展示，不可调用
		if cfg.Enabled && p.info.State == StatePending {
			m.byName[cfg.Name] = p
		}
	}
	return m
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

// Refuse 不启动已开启的插件，并把它们统一标记为 error（如未设置 onebot.access_token 时）
func (m *Manager) Refuse(reason error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.started || m.closed {
		return
	}
	m.started = true
	for _, p := range m.byName {
		p.setState(StateError, reason)
	}
	m.byName = map[string]*plugin{}
}

// List 返回全部插件（含未开启的）的状态快照
func (m *Manager) List() []Info {
	result := make([]Info, 0, len(m.ordered))
	for _, p := range m.ordered {
		result = append(result, p.snapshot())
	}
	return result
}

// Call 调用插件的一项能力。allowSensitive 表示发起请求的连接是否允许调用敏感能力。
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
	return p.call(ctx, capability, args, allowSensitive)
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
