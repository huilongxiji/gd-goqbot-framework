package plugins

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode"

	"goqbot/internal/filelog"
)

// 插件状态（Info.State）
const (
	StateDisabled   = "disabled"   // 开关未打开
	StatePending    = "pending"    // 已开启，等待 goqbot 启动它
	StateStarting   = "starting"   // 进程已拉起，正在握手
	StateReady      = "ready"      // 握手完成，可以调用
	StateRestarting = "restarting" // 进程异常退出，等待自动重启
	StateError      = "error"      // 无法自动恢复（配置错误、程序无法启动、协议不兼容等），修正后需重启 goqbot
	StateClosing    = "closing"    // 正在关闭
	StateClosed     = "closed"     // 已关闭
)

// 重启、关闭与协议参数
const (
	restartMinDelay         = time.Second            // 第一次自动重启前的等待
	restartMaxDelay         = time.Minute            // 重启等待的上限（每次失败翻倍）
	stableAfter             = time.Minute            // 就绪持续这么久后再失败，重启等待重新从 restartMinDelay 计
	shutdownTimeout         = time.Second            // 关闭时等待插件响应 shutdown / 进程退出的时长
	stderrLogLimit    int64 = 10 << 20               // 单个插件 stderr 日志文件上限，超出后轮转为 .old
	waitDelay               = 250 * time.Millisecond // 插件进程退出后，等待其子进程释放 stdout/stderr 管道的最长时间
	maxConcurrencyV2        = 64                     // 协议 2：插件声明的并发上限的上限
	maxPluginRequests       = 64                     // 协议 2：插件同时发给 goqbot 的请求上限
	ctrlQueueSize           = 256                    // 待写给插件的请求 / 响应 / 通知队列长度
	eventQueueSize          = 1024                   // 待推送给插件的事件队列长度，满了丢弃最早的
	logRatePerSecond        = 20                     // notifications/log 每秒最多条数
	logMaxRunes             = 4096                   // notifications/log 单条最长字符数
	maxCapabilities         = 128                    // 插件最多声明的能力数
	maxRequires             = 256                    // 插件 requires 最多条数
	maxEvents               = 64                     // 插件最多声明的事件数
)

// permanentError 标记不应自动重启的失败（重启也只会得到同样的结果）
type permanentError struct{ error }

// Unwrap 返回被包装的原始错误，供 errors.Is / errors.As 识别错误码
func (e permanentError) Unwrap() error { return e.error }

// isPermanent 失败是否无需自动重启
func isPermanent(err error) bool {
	var p permanentError
	return errors.As(err, &p)
}

// reply 一次 JSON-RPC 调用的结果
type reply struct {
	result json.RawMessage
	err    error
}

// plugin 一个插件实例：持有配置与状态，并由 run 负责拉起、握手、异常后自动重启
type plugin struct {
	m   *Manager
	cfg Config

	mu       sync.Mutex
	info     Info          // 对外展示的状态快照（Capabilities 在 snapshot 时按注册表补全）
	caps     []Capability  // 握手声明的能力
	requires []string      // 协议 2：插件声明需要调用的能力
	protocol string        // 握手协商出的协议版本
	inst     *instance     // 当前运行中的进程；nil=没有运行
	slots    chan struct{} // 协议 2：goqbot 调用插件的并发名额（每次握手按 max_concurrency 重建）
	closing  bool          // 已开始关闭，不再重启
	preferV1 bool          // 上次按协议 2 握手失败，下次先按协议 1 握手

	gate       chan struct{} // 容量 1：握手、关闭，以及协议 1 的调用互斥
	closeCh    chan struct{} // 关闭信号：通知 run 退出
	done       chan struct{} // run 已退出
	dropped    atomic.Int64  // 丢弃的事件数
	lastDropAt atomic.Int64  // 上次记录丢弃日志的时间（Unix 秒），避免刷屏
}

// instance 插件的一次进程生命周期（每次重启都是新的 instance）
type instance struct {
	owner    *plugin
	cmd      *exec.Cmd
	stdin    io.WriteCloser
	stdout   io.ReadCloser
	stderr   io.Closer     // stderr 日志文件，可为 nil
	waitDone chan struct{} // 进程已被回收
	stopCh   chan struct{} // 实例已停止（被结束或自行退出）
	stopOnce sync.Once

	ctrl     chan []byte   // 待写出的请求 / 响应 / 通知（不丢弃）
	evSignal chan struct{} // 有新事件待写

	mu          sync.Mutex
	stopErr     error                 // 停止原因
	nextID      uint64                // 下一个请求 id
	pending     map[uint64]chan reply // goqbot 发出、等待响应的请求
	peer        bool                  // 允许插件发请求和通知（已按协议 2 握手或正在尝试协议 2）
	v2          bool                  // 已确定按协议 2 运行
	initialized bool                  // 已发 notifications/initialized，可以接受插件的请求、推送事件
	quitting    bool                  // 已发 shutdown，之后插件的请求返回 not_ready
	inflight    int                   // 插件发来、正在处理的请求数
	cancels     map[int64]func()      // 插件发来、正在处理的请求的取消函数
	subs        map[string][]string   // 事件订阅：订阅号 -> 事件类型
	subOrder    []string              // 订阅号创建顺序（匹配时按此顺序取第一个）
	subSeq      int                   // 下一个订阅号
	events      [][]byte              // 待推送的事件帧
	logWindow   time.Time             // notifications/log 限流窗口起点
	logCount    int                   // 窗口内已写日志条数
}

// newPlugin 按配置创建插件（不启动进程）
func newPlugin(m *Manager, cfg Config) *plugin {
	state := StatePending
	reason := ""
	if err := cfg.Validate(); err != nil {
		state, reason = StateError, Code(err)
	} else if !cfg.Enabled {
		state = StateDisabled
	}
	return &plugin{
		m:       m,
		cfg:     cfg,
		info:    Info{Name: cfg.Name, Kind: cfg.Kind, State: state, Reason: reason, Grants: cfg.Grants},
		gate:    make(chan struct{}, 1),
		closeCh: make(chan struct{}),
		done:    make(chan struct{}),
	}
}

// run 插件守护循环：拉起并握手，就绪后等待进程结束；可恢复的失败按 1s、2s、4s…（最多 60s）间隔自动重启
func (p *plugin) run(ctx context.Context) {
	defer close(p.done)
	delay := restartMinDelay
	for {
		inst, err := p.launch(ctx)
		if err == nil {
			readyAt := time.Now()
			select {
			case <-inst.stopCh:
				err = inst.stoppedError()
			case <-p.closeCh:
				return
			case <-ctx.Done():
				return
			}
			if time.Since(readyAt) >= stableAfter {
				delay = restartMinDelay
			}
		}
		if p.isClosing() || ctx.Err() != nil {
			return
		}
		if isPermanent(err) {
			p.setState(StateError, err)
			p.m.clearClaims(p)
			filelog.Errorf("[plugins] %s 启动失败，不再自动重启: %s", p.cfg.Name, describeErr(err))
			return
		}
		if errors.Is(err, ErrRestartRequested) {
			// plugins.restart：立即重启，不计入异常重启
			p.setState(StateRestarting, err)
			log.Printf("[plugins] %s 按要求重启", p.cfg.Name)
			delay = restartMinDelay
			continue
		}
		p.mu.Lock()
		p.info.State = StateRestarting
		p.info.Reason = Code(err)
		p.info.Restarts++
		p.mu.Unlock()
		filelog.Warnf("[plugins] %s 已停止（%s），%d 秒后自动重启", p.cfg.Name, describeErr(err), int(delay/time.Second))
		select {
		case <-time.After(delay):
		case <-p.closeCh:
			return
		case <-ctx.Done():
			return
		}
		delay = min(delay*2, restartMaxDelay)
	}
}

// describeErr 日志用的错误说明：配置 / 启动类错误带详情（不含凭证），其余只给中文说明与错误码
func describeErr(err error) string {
	if errors.Is(err, ErrInvalidConfig) || errors.Is(err, ErrStartFailed) {
		return err.Error()
	}
	return fmt.Sprintf("%s，%s", Describe(err), Code(err))
}

// hello 插件对 initialize 的回复（两个协议版本的字段合集）
type hello struct {
	ProtocolVersion string       `json:"protocol_version"`
	Name            string       `json:"name"`
	Version         string       `json:"version"`
	Capabilities    []Capability `json:"capabilities"`
	MaxConcurrency  int          `json:"max_concurrency"`
	Requires        []string     `json:"requires"`
	Events          []EventDecl  `json:"events"`
}

// launch 拉起一个新进程并完成握手；成功时插件进入 ready 状态
func (p *plugin) launch(ctx context.Context) (*instance, error) {
	select {
	case p.gate <- struct{}{}:
	case <-p.closeCh:
		return nil, ErrClosed
	}
	defer func() { <-p.gate }()
	if err := p.cfg.Validate(); err != nil {
		return nil, permanentError{err}
	}
	if ctx.Err() != nil {
		return nil, ErrCanceled
	}
	inst, err := p.spawn()
	if err != nil {
		return nil, err
	}
	initCtx, cancel := context.WithTimeout(ctx, duration(p.cfg.StartupTimeoutMS, DefaultStartupTimeoutMS))
	defer cancel()

	js := p.cfg.Kind == KindJS
	p.mu.Lock()
	tryV1 := p.preferV1 && !js
	p.mu.Unlock()
	h, err := p.handshake(initCtx, inst, !tryV1)
	switch {
	case err != nil && js && errors.Is(err, ErrCallFailed):
		// JS / TS 插件载入失败（语法错误、插件对象写错等），内置启动器已把原因写进日志；重启也不会好转
		err = permanentError{fmt.Errorf("%w: 插件载入失败，原因见日志里的 [plugin:%s]", ErrInvalidConfig, p.cfg.Name)}
	case err != nil && !tryV1 && !js && errors.Is(err, ErrCallFailed):
		// 插件拒绝了协议 2 的 initialize（例如只认协议 1 的严格实现），在同一进程上改用协议 1 再握手一次
		h, err = p.handshake(initCtx, inst, false)
	}
	if err != nil {
		inst.stop(err)
		if !isPermanent(err) && !js {
			// 下次换一个协议版本先试：只认协议 1 的插件可能在收到协议 2 的参数时直接退出
			p.mu.Lock()
			p.preferV1 = !tryV1
			p.mu.Unlock()
		}
		return nil, err
	}

	v2 := h.ProtocolVersion == ProtocolV2
	conc := h.MaxConcurrency
	if conc == 0 {
		conc = 1
	}
	p.mu.Lock()
	if p.closing {
		p.mu.Unlock()
		inst.stop(ErrClosed)
		return nil, ErrClosed
	}
	if p.inst != inst {
		// 握手刚完成进程就退出了
		p.mu.Unlock()
		return nil, inst.stoppedError()
	}
	p.protocol = h.ProtocolVersion
	p.caps = cloneCapabilities(h.Capabilities)
	p.requires = append([]string{}, h.Requires...)
	p.slots = make(chan struct{}, conc)
	p.preferV1 = !v2
	p.info.Version = h.Version
	p.info.ProtocolVersion = h.ProtocolVersion
	p.info.Events = append([]EventDecl{}, h.Events...)
	p.info.Requires = append([]string{}, h.Requires...)
	p.info.State = StateReady
	p.info.Reason = ""
	p.mu.Unlock()

	if !v2 {
		p.m.clearClaims(p)
		log.Printf("[plugins] %s 已就绪: %s v%s（协议 1），能力 %d 个，下游通过 call_plugin 调用", p.cfg.Name, h.Name, h.Version, len(h.Capabilities))
		return inst, nil
	}

	inst.mu.Lock()
	inst.v2 = true
	inst.mu.Unlock()
	p.m.setClaims(p, h.Capabilities)
	p.logReady(h)
	// 注册完成后才允许插件调用 goqbot；initialized 通知里带上授权给插件的能力
	inst.mu.Lock()
	inst.initialized = true
	inst.mu.Unlock()
	inst.notify("notifications/initialized", map[string]any{"capabilities": p.m.capabilitiesFor(p)})
	p.m.notifyListChanged(p)
	return inst, nil
}

// handshake 发送 initialize 并校验插件的回复。v2=true 时按协议 2 发起（插件可回协议 1）。
func (p *plugin) handshake(ctx context.Context, inst *instance, v2 bool) (hello, error) {
	var params any
	if v2 {
		self := int64(0)
		if p.m.opts.SelfID != nil {
			self = p.m.opts.SelfID()
		}
		params = map[string]any{
			"protocol_version": ProtocolV2,
			"host":             map[string]any{"name": "goqbot", "version": p.m.opts.HostVersion, "self_id": self},
			"plugin":           map[string]any{"name": p.cfg.Name, "dir": p.cfg.Dir, "data_dir": p.cfg.DataDir},
		}
	} else {
		params = map[string]any{
			"protocol_version": ProtocolV1,
			"host":             map[string]any{"name": "goqbot", "version": p.m.opts.HostVersion},
		}
	}
	inst.mu.Lock()
	inst.peer = v2
	inst.mu.Unlock()
	data, err := inst.request(ctx, "initialize", params)
	if err != nil {
		return hello{}, err
	}
	var head struct {
		ProtocolVersion string `json:"protocol_version"`
	}
	_ = json.Unmarshal(data, &head)
	var h hello
	switch {
	case head.ProtocolVersion == ProtocolV1:
		var h1 struct {
			ProtocolVersion string       `json:"protocol_version"`
			Name            string       `json:"name"`
			Version         string       `json:"version"`
			Capabilities    []Capability `json:"capabilities"`
		}
		if strictDecode(data, &h1) != nil || !validHello(h1.Name, h1.Version) ||
			h1.Capabilities == nil || len(h1.Capabilities) > maxCapabilities || !validCapabilities(h1.Capabilities, capabilityPattern) {
			return hello{}, permanentError{ErrProtocol}
		}
		h = hello{ProtocolVersion: ProtocolV1, Name: h1.Name, Version: h1.Version, Capabilities: h1.Capabilities}
		inst.mu.Lock()
		inst.peer = false
		inst.mu.Unlock()
	case head.ProtocolVersion == ProtocolV2 && v2:
		if strictDecode(data, &h) != nil || !validHello(h.Name, h.Version) ||
			h.Capabilities == nil || len(h.Capabilities) > maxCapabilities || !validCapabilities(h.Capabilities, actionPattern) ||
			h.MaxConcurrency < 0 || h.MaxConcurrency > maxConcurrencyV2 || !validRequires(h.Requires) || !validEvents(h.Events) {
			// 握手内容不合规是插件实现问题，重启也不会好转
			return hello{}, permanentError{ErrProtocol}
		}
	default:
		return hello{}, permanentError{ErrProtocol}
	}
	if h.Requires == nil {
		h.Requires = []string{}
	}
	if h.Events == nil {
		h.Events = []EventDecl{}
	}
	return h, nil
}

// logReady 协议 2 插件就绪日志：注册的动作、可调用的能力，敏感能力单独用警告级别列出
func (p *plugin) logReady(h hello) {
	var actions []string
	for _, c := range h.Capabilities {
		if ok, _ := p.m.regStatus(p, c.Name); ok {
			actions = append(actions, c.Name)
		}
	}
	actionText := "无"
	if len(actions) > 0 {
		actionText = strings.Join(actions, ", ")
	}
	allowedText := "无"
	if allowed := p.effectiveGrants(); len(allowed) > 0 {
		allowedText = strings.Join(allowed, ", ")
	}
	kind := "协议 2"
	if p.cfg.Kind == KindJS {
		kind = "JS / TS 插件"
	}
	log.Printf("[plugins] %s 已就绪: %s v%s（%s），注册动作 %s；可调用 %s", p.cfg.Name, h.Name, h.Version, kind, actionText, allowedText)
	if sensitive := p.m.sensitiveGranted(p); len(sensitive) > 0 {
		filelog.Warnf("[plugins] %s 获得敏感能力：%s", p.cfg.Name, strings.Join(sensitive, ", "))
	}
}

// validHello 程序名与版本号是否合规
func validHello(name, version string) bool {
	return namePattern.MatchString(name) && version != "" && len(version) <= 64
}

// validCapabilities 校验握手声明的能力列表：名称合规且不重复、说明与 schema 有长度上限、schema 为对象
func validCapabilities(caps []Capability, pattern interface{ MatchString(string) bool }) bool {
	seen := make(map[string]bool, len(caps))
	for _, c := range caps {
		if !pattern.MatchString(c.Name) || seen[c.Name] || len(c.Description) > 1024 ||
			len(c.InputSchema) > 128*1024 || !isObject(c.InputSchema) ||
			(len(c.OutputSchema) > 0 && (len(c.OutputSchema) > 128*1024 || !isObject(c.OutputSchema))) {
			return false
		}
		seen[c.Name] = true
	}
	return true
}

// validRequires 校验 requires：每项都是能力名或带 * 的通配
func validRequires(list []string) bool {
	if len(list) > maxRequires {
		return false
	}
	for _, r := range list {
		if !grantPattern.MatchString(r) {
			return false
		}
	}
	return true
}

// validEvents 校验插件声明的事件
func validEvents(list []EventDecl) bool {
	if len(list) > maxEvents {
		return false
	}
	seen := map[string]bool{}
	for _, e := range list {
		if !actionPattern.MatchString(e.Name) || seen[e.Name] || len(e.Description) > 1024 {
			return false
		}
		seen[e.Name] = true
	}
	return true
}

// spawn 启动插件进程并登记为当前实例（调用方须持有 gate）
func (p *plugin) spawn() (*instance, error) {
	command, err := resolveCommand(p.cfg.Dir, p.cfg.Command)
	if err != nil {
		return nil, permanentError{fmt.Errorf("%w: %v", ErrStartFailed, err)}
	}
	if p.cfg.DataDir != "" {
		if err := os.MkdirAll(p.cfg.DataDir, 0o755); err != nil {
			return nil, permanentError{fmt.Errorf("%w: 创建数据目录 %s 失败: %v", ErrStartFailed, p.cfg.DataDir, err)}
		}
	}
	cmd := exec.Command(command, p.cfg.Args...)
	cmd.Dir = p.cfg.Dir
	cmd.Env = childEnvironment(p.cfg.Env)
	cmd.WaitDelay = waitDelay
	configureProcess(cmd)
	inst := &instance{
		owner:    p,
		cmd:      cmd,
		waitDone: make(chan struct{}),
		stopCh:   make(chan struct{}),
		ctrl:     make(chan []byte, ctrlQueueSize),
		evSignal: make(chan struct{}, 1),
		pending:  make(map[uint64]chan reply),
		cancels:  make(map[int64]func()),
		subs:     make(map[string][]string),
	}
	// stderr 默认丢弃（nil=接到空设备）；开启 log_stderr 时写入有上限的日志文件
	if p.cfg.LogStderr {
		if w, err := openStderrLog(p.m.opts.LogDir, p.cfg.Name); err != nil {
			filelog.Warnf("[plugins] %s 打开 stderr 日志失败，插件输出将被丢弃: %v", p.cfg.Name, err)
		} else {
			cmd.Stderr, inst.stderr = w, w
		}
	}
	closeStderr := func() {
		if inst.stderr != nil {
			_ = inst.stderr.Close()
		}
	}
	if inst.stdin, err = cmd.StdinPipe(); err != nil {
		closeStderr()
		return nil, permanentError{fmt.Errorf("%w: %v", ErrStartFailed, err)}
	}
	if inst.stdout, err = cmd.StdoutPipe(); err != nil {
		_ = inst.stdin.Close()
		closeStderr()
		return nil, permanentError{fmt.Errorf("%w: %v", ErrStartFailed, err)}
	}

	p.mu.Lock()
	if p.closing {
		p.mu.Unlock()
		_ = inst.stdin.Close()
		_ = inst.stdout.Close()
		closeStderr()
		return nil, ErrClosed
	}
	if err := cmd.Start(); err != nil {
		p.mu.Unlock()
		closeStderr()
		return nil, permanentError{fmt.Errorf("%w: %v", ErrStartFailed, err)}
	}
	p.inst = inst
	p.info.State = StateStarting
	p.caps = nil
	p.mu.Unlock()

	go func() {
		_ = cmd.Wait()
		closeStderr()
		close(inst.waitDone)
		inst.stop(ErrProcessExited)
	}()
	go inst.writeLoop()
	go inst.readLoop()
	return inst, nil
}

// call 调用插件的一项能力。allowSensitive=false 时不能调用敏感能力。
// 协议 1：同一时刻只处理一个请求；协议 2：同时最多 max_concurrency 个。超出时立即返回 ErrBusy，不排队。
func (p *plugin) call(ctx context.Context, capability string, args json.RawMessage, allowSensitive bool) (json.RawMessage, error) {
	if ctx.Err() != nil {
		return nil, contextError(ctx)
	}
	if len(args) == 0 {
		args = json.RawMessage(`{}`)
	}
	if len(args) > MaxFrameBytes-1024 || !isObject(args) {
		return nil, ErrInvalidArguments
	}
	if err := p.checkCallable(capability, allowSensitive); err != nil {
		return nil, err
	}
	p.mu.Lock()
	v2, slots := p.protocol == ProtocolV2, p.slots
	p.mu.Unlock()
	if v2 {
		select {
		case slots <- struct{}{}:
			defer func() { <-slots }()
		default:
			return nil, ErrBusy
		}
	} else {
		select {
		case p.gate <- struct{}{}:
			defer func() { <-p.gate }()
		default:
			return nil, ErrBusy
		}
	}
	// 拿到名额后再确认一次：等待期间插件可能已重启或关闭
	p.mu.Lock()
	inst, state := p.inst, p.info.State
	p.mu.Unlock()
	if state != StateReady || inst == nil {
		return nil, stateError(state)
	}
	callCtx, cancel := context.WithTimeout(ctx, duration(p.cfg.CallTimeoutMS, DefaultCallTimeoutMS))
	defer cancel()
	return inst.request(callCtx, "capabilities/call", struct {
		Name      string          `json:"name"`
		Arguments json.RawMessage `json:"arguments"`
	}{Name: capability, Arguments: args})
}

// checkCallable 检查插件状态、能力是否已声明，以及敏感能力的调用权限
func (p *plugin) checkCallable(capability string, allowSensitive bool) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.info.State != StateReady {
		return stateError(p.info.State)
	}
	for _, c := range p.caps {
		if c.Name == capability {
			if c.Sensitive && !allowSensitive {
				return ErrForbidden
			}
			return nil
		}
	}
	return ErrCapabilityNotFound
}

// stateError 非就绪状态对应的调用错误
func stateError(state string) error {
	if state == StateClosing || state == StateClosed {
		return ErrClosed
	}
	return ErrNotReady
}

// setState 设置状态与失败原因
func (p *plugin) setState(state string, reason error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.info.State = state
	p.info.Reason = ""
	if reason != nil {
		p.info.Reason = Code(reason)
	}
}

// isClosing 是否已开始关闭
func (p *plugin) isClosing() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.closing
}

// currentInstance 当前进程实例，未运行时为 nil
func (p *plugin) currentInstance() *instance {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.inst
}

// requestRestart 结束当前进程并立即重启（plugins.restart）
func (p *plugin) requestRestart() error {
	p.mu.Lock()
	inst, state := p.inst, p.info.State
	p.mu.Unlock()
	if inst == nil || (state != StateReady && state != StateStarting) {
		return ErrNotReady
	}
	inst.stop(ErrRestartRequested)
	return nil
}

// onInstanceStopped 实例停止时回调：就绪中的插件立即转为 restarting，避免新请求打到已停止的进程
func (p *plugin) onInstanceStopped(inst *instance, reason error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.inst != inst {
		return
	}
	p.inst = nil
	if !p.closing && (p.info.State == StateReady || p.info.State == StateStarting) {
		p.info.State = StateRestarting
		p.info.Reason = Code(reason)
	}
}

// snapshot 返回状态快照（深拷贝，调用方修改不影响插件），能力列表附上注册情况
func (p *plugin) snapshot() Info {
	p.mu.Lock()
	info := p.info
	caps := cloneCapabilities(p.caps)
	protocol := p.protocol
	info.Events = append([]EventDecl{}, info.Events...)
	info.Requires = append([]string{}, info.Requires...)
	if info.Grants != nil {
		info.Grants = append([]string{}, info.Grants...)
	}
	p.mu.Unlock()
	info.DroppedEvents = p.dropped.Load()
	info.Capabilities = make([]CapabilityInfo, 0, len(caps))
	for _, c := range caps {
		ci := CapabilityInfo{Capability: c}
		if protocol == ProtocolV2 {
			ci.Registered, ci.Reason = p.m.regStatus(p, c.Name)
		} else {
			ci.Reason = "protocol_v1"
		}
		info.Capabilities = append(info.Capabilities, ci)
	}
	return info
}

// shutdown 关闭插件：空闲时先发 shutdown 请求给插件留出清理时间，随后结束并回收进程。
// 正在握手的插件（协议 1 还包括正在处理请求的）直接结束。可重复调用。
func (p *plugin) shutdown() {
	p.mu.Lock()
	if p.closing {
		p.mu.Unlock()
		return
	}
	p.closing = true
	close(p.closeCh)
	p.mu.Unlock()

	select {
	case p.gate <- struct{}{}:
		p.mu.Lock()
		inst, ready := p.inst, p.info.State == StateReady
		if ready {
			p.info.State = StateClosing
		}
		p.mu.Unlock()
		if ready && inst != nil {
			inst.mu.Lock()
			inst.quitting = true
			inst.mu.Unlock()
			ctx, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
			_, _ = inst.request(ctx, "shutdown", struct{}{})
			cancel()
		}
		<-p.gate
	default:
	}

	p.mu.Lock()
	inst := p.inst
	p.mu.Unlock()
	if inst != nil {
		inst.stop(ErrClosed)
		select {
		case <-inst.waitDone:
		case <-time.After(shutdownTimeout):
		}
	}
	// 未开启的插件保持 disabled；其余一律标记为已关闭（失败原因仍保留在 Reason 中）
	p.mu.Lock()
	if p.info.State != StateDisabled {
		p.info.State = StateClosed
	}
	p.mu.Unlock()
}

// ---------------- 实例的读写 ----------------

// request 发送一个请求并等待对应响应。
// 协议 1（以及握手阶段）：超时或取消会结束整个实例，因为无法确认插件是否还会输出迟到的响应；
// 协议 2：超时只通知插件取消，迟到的响应被忽略，插件继续运行。
func (inst *instance) request(ctx context.Context, method string, params any) (json.RawMessage, error) {
	inst.mu.Lock()
	select {
	case <-inst.stopCh:
		inst.mu.Unlock()
		return nil, inst.stoppedError()
	default:
	}
	inst.nextID++
	id := inst.nextID
	ch := make(chan reply, 1)
	inst.pending[id] = ch
	v2 := inst.v2
	inst.mu.Unlock()
	defer func() {
		inst.mu.Lock()
		delete(inst.pending, id)
		inst.mu.Unlock()
	}()

	frame, err := json.Marshal(struct {
		JSONRPC string `json:"jsonrpc"`
		ID      uint64 `json:"id"`
		Method  string `json:"method"`
		Params  any    `json:"params"`
	}{JSONRPC: "2.0", ID: id, Method: method, Params: params})
	if err != nil || len(frame) > MaxFrameBytes {
		if !v2 {
			inst.stop(ErrProtocol)
			return nil, ErrProtocol
		}
		return nil, ErrInvalidArguments
	}
	onTimeout := func(err error) error {
		if v2 {
			inst.notify("notifications/cancelled", map[string]any{"request_id": id})
		} else {
			inst.stop(err)
		}
		return err
	}
	select {
	case inst.ctrl <- frame:
	case <-ctx.Done():
		return nil, onTimeout(contextError(ctx))
	case <-inst.stopCh:
		return nil, inst.stoppedError()
	}
	select {
	case r := <-ch:
		return r.result, r.err
	case <-ctx.Done():
		return nil, onTimeout(contextError(ctx))
	case <-inst.stopCh:
		return nil, inst.stoppedError()
	}
}

// notify 发送一条通知。队列满时最多等 1 秒，仍写不进就放弃（通知可以丢）。
func (inst *instance) notify(method string, params any) {
	frame, err := json.Marshal(struct {
		JSONRPC string `json:"jsonrpc"`
		Method  string `json:"method"`
		Params  any    `json:"params"`
	}{JSONRPC: "2.0", Method: method, Params: params})
	if err != nil || len(frame) > MaxFrameBytes {
		return
	}
	select {
	case inst.ctrl <- frame:
	case <-inst.stopCh:
	case <-time.After(time.Second):
	}
}

// respond 回复插件发来的请求
func (inst *instance) respond(id int64, result json.RawMessage, rpcErr *rpcError) {
	msg := map[string]any{"jsonrpc": "2.0", "id": id}
	if rpcErr != nil {
		msg["error"] = rpcErr
	} else {
		msg["result"] = result
	}
	frame, err := json.Marshal(msg)
	if err != nil {
		return
	}
	if len(frame) > MaxFrameBytes {
		frame, _ = json.Marshal(map[string]any{"jsonrpc": "2.0", "id": id, "error": newRPCError(ErrOutputTooLarge)})
	}
	select {
	case inst.ctrl <- frame:
	case <-inst.stopCh:
	}
}

// pushEvent 把一个事件帧放进待推送队列；队列满时丢弃最早的事件
func (inst *instance) pushEvent(frame []byte) {
	inst.mu.Lock()
	if !inst.initialized || inst.quitting {
		inst.mu.Unlock()
		return
	}
	dropped := false
	if len(inst.events) >= eventQueueSize {
		inst.events = inst.events[1:]
		dropped = true
	}
	inst.events = append(inst.events, frame)
	inst.mu.Unlock()
	if dropped {
		inst.owner.noteDropped(1)
	}
	select {
	case inst.evSignal <- struct{}{}:
	default:
	}
}

// noteDropped 记录丢弃的事件数，日志最多每分钟一条
func (p *plugin) noteDropped(n int64) {
	total := p.dropped.Add(n)
	now := time.Now().Unix()
	last := p.lastDropAt.Load()
	if now-last >= 60 && p.lastDropAt.CompareAndSwap(last, now) {
		filelog.Warnf("[plugins] %s 来不及读取事件，已累计丢弃 %d 个", p.cfg.Name, total)
	}
}

// popEvent 取出一个待推送的事件帧
func (inst *instance) popEvent() []byte {
	inst.mu.Lock()
	defer inst.mu.Unlock()
	if len(inst.events) == 0 {
		return nil
	}
	frame := inst.events[0]
	inst.events[0] = nil
	inst.events = inst.events[1:]
	return frame
}

// writeLoop 串行写出：优先写请求 / 响应 / 通知，空闲时再写事件。写失败说明插件已退出。
func (inst *instance) writeLoop() {
	write := func(frame []byte) bool {
		if _, err := inst.stdin.Write(append(frame, '\n')); err != nil {
			inst.stop(ErrProcessExited)
			return false
		}
		return true
	}
	for {
		select {
		case frame := <-inst.ctrl:
			if !write(frame) {
				return
			}
			continue
		case <-inst.stopCh:
			return
		default:
		}
		if frame := inst.popEvent(); frame != nil {
			if !write(frame) {
				return
			}
			continue
		}
		select {
		case frame := <-inst.ctrl:
			if !write(frame) {
				return
			}
		case <-inst.evSignal:
		case <-inst.stopCh:
			return
		}
	}
}

// stoppedError 实例停止的原因
func (inst *instance) stoppedError() error {
	inst.mu.Lock()
	defer inst.mu.Unlock()
	if inst.stopErr != nil {
		return inst.stopErr
	}
	return ErrClosed
}

// stop 结束实例：记录首个停止原因，结束整个进程组并关闭管道，取消插件发来的未完成请求。可重复调用。
func (inst *instance) stop(reason error) {
	inst.stopOnce.Do(func() {
		inst.mu.Lock()
		inst.stopErr = reason
		cancels := make([]func(), 0, len(inst.cancels))
		for _, c := range inst.cancels {
			cancels = append(cancels, c)
		}
		inst.mu.Unlock()
		close(inst.stopCh)
		killProcess(inst.cmd)
		_ = inst.stdin.Close()
		_ = inst.stdout.Close()
		for _, c := range cancels {
			c()
		}
		inst.owner.onInstanceStopped(inst, reason)
	})
}

// readLoop 逐行读取插件 stdout；单帧超过 MaxFrameBytes 立即结束实例，不等到超时
func (inst *instance) readLoop() {
	r := bufio.NewReaderSize(inst.stdout, 32*1024)
	for {
		var frame []byte
		for {
			// ReadSlice 要读满缓冲区才返回；帧长恰好到上限时只再允许读 1 个字节（必须是换行），
			// 避免不换行的输出把半帧一直挂到请求超时
			if len(frame) == MaxFrameBytes {
				next, err := r.ReadByte()
				if err != nil {
					inst.stop(ErrProcessExited)
					return
				}
				if next != '\n' {
					inst.stop(ErrOutputTooLarge)
					return
				}
				frame = append(frame, next)
				break
			}
			piece, err := r.ReadSlice('\n')
			if len(frame)+len(piece) > MaxFrameBytes+1 {
				inst.stop(ErrOutputTooLarge)
				return
			}
			frame = append(frame, piece...)
			if err == bufio.ErrBufferFull {
				continue
			}
			if err != nil {
				inst.stop(ErrProcessExited)
				return
			}
			break
		}
		if err := inst.receive(bytes.TrimSuffix(bytes.TrimSuffix(frame, []byte{'\n'}), []byte{'\r'})); err != nil {
			inst.stop(err)
			return
		}
	}
}

// receive 处理插件发来的一帧：响应交给等待中的请求；协议 2 下还可能是插件的请求或通知。
// 任何不合规（字段重复、未知字段、id 不对、result 非对象等）都视为协议错误。
func (inst *instance) receive(frame []byte) error {
	var msg struct {
		JSONRPC string          `json:"jsonrpc"`
		ID      json.RawMessage `json:"id"`
		Method  *string         `json:"method"`
		Params  json.RawMessage `json:"params"`
		Result  json.RawMessage `json:"result"`
		Error   json.RawMessage `json:"error"`
	}
	if strictDecode(frame, &msg) != nil || msg.JSONRPC != "2.0" {
		return ErrProtocol
	}
	if msg.Method != nil {
		return inst.receivePeer(msg.ID, *msg.Method, msg.Params, len(msg.Result) > 0 || len(msg.Error) > 0)
	}
	if len(msg.Params) > 0 || (len(msg.Result) == 0) == (len(msg.Error) == 0) {
		return ErrProtocol
	}
	id, err := strconv.ParseUint(string(msg.ID), 10, 64)
	if err != nil {
		return ErrProtocol
	}
	response := reply{result: msg.Result}
	if len(msg.Error) > 0 {
		var rpcErr struct {
			Code    *int            `json:"code"`
			Message *string         `json:"message"`
			Data    json.RawMessage `json:"data"`
		}
		if !isObject(msg.Error) || strictDecode(msg.Error, &rpcErr) != nil || rpcErr.Code == nil || rpcErr.Message == nil {
			return ErrProtocol
		}
		// 插件的错误原文可能含凭证或上游响应，只返回固定错误码
		response = reply{err: ErrCallFailed}
	} else if !isObject(msg.Result) {
		return ErrProtocol
	}
	inst.mu.Lock()
	ch, ok := inst.pending[id]
	delete(inst.pending, id)
	issued, v2 := inst.nextID, inst.v2
	inst.mu.Unlock()
	if !ok {
		// 协议 2：超时或取消后迟到的响应直接忽略；从未发出过的 id 才是协议错误
		if v2 && id >= 1 && id <= issued {
			return nil
		}
		return ErrProtocol
	}
	ch <- response
	return nil
}

// receivePeer 处理插件发来的请求（带 id）或通知（不带 id），只在协议 2 下允许
func (inst *instance) receivePeer(rawID json.RawMessage, method string, params json.RawMessage, hasResult bool) error {
	inst.mu.Lock()
	peer := inst.peer
	inst.mu.Unlock()
	if !peer || hasResult || method == "" || len(method) > 128 {
		return ErrProtocol
	}
	if len(params) == 0 {
		params = json.RawMessage(`{}`)
	} else if !isObject(params) {
		return ErrProtocol
	}
	if len(rawID) == 0 {
		inst.handleNotification(method, params)
		return nil
	}
	id, err := strconv.ParseInt(string(rawID), 10, 64)
	if err != nil {
		return ErrProtocol
	}
	inst.mu.Lock()
	if _, dup := inst.cancels[id]; dup {
		inst.mu.Unlock()
		return ErrProtocol
	}
	if inst.inflight >= maxPluginRequests {
		inst.mu.Unlock()
		go inst.respond(id, nil, &rpcError{Code: codeBusy, Message: "busy", Data: map[string]any{"code": "busy"}})
		return nil
	}
	ctx, cancel := context.WithCancel(context.Background())
	inst.inflight++
	inst.cancels[id] = cancel
	initialized, quitting := inst.initialized, inst.quitting
	inst.mu.Unlock()

	go func() {
		defer func() {
			inst.mu.Lock()
			inst.inflight--
			delete(inst.cancels, id)
			inst.mu.Unlock()
			cancel()
		}()
		var result json.RawMessage
		var rerr *rpcError
		switch {
		case quitting:
			rerr = newRPCError(ErrNotReady)
		case !initialized:
			rerr = &rpcError{Code: codeNotInitialized, Message: "not_initialized", Data: map[string]any{"code": "not_initialized"}}
		default:
			result, rerr = inst.owner.m.serve(ctx, inst.owner, method, params)
		}
		inst.respond(id, result, rerr)
	}()
	return nil
}

// handleNotification 处理插件发来的通知；不认识的通知忽略，便于以后扩展
func (inst *instance) handleNotification(method string, params json.RawMessage) {
	switch method {
	case "notifications/log":
		var arg struct {
			Level   string `json:"level"`
			Message string `json:"message"`
		}
		if strictDecode(params, &arg) != nil {
			return
		}
		inst.mu.Lock()
		now := time.Now()
		if now.Sub(inst.logWindow) >= time.Second {
			inst.logWindow, inst.logCount = now, 0
		}
		inst.logCount++
		allowed := inst.logCount <= logRatePerSecond
		inst.mu.Unlock()
		if !allowed {
			return
		}
		text := fmt.Sprintf("[plugin:%s] %s", inst.owner.cfg.Name, sanitizeLog(arg.Message))
		switch arg.Level {
		case "error":
			filelog.Errorf("%s", text)
		case "warning", "warn":
			filelog.Warnf("%s", text)
		default:
			filelog.Infof("%s", text)
		}
	case "notifications/cancelled":
		var arg struct {
			RequestID int64 `json:"request_id"`
		}
		if strictDecode(params, &arg) != nil {
			return
		}
		inst.mu.Lock()
		cancel := inst.cancels[arg.RequestID]
		inst.mu.Unlock()
		if cancel != nil {
			cancel()
		}
	}
}

// sanitizeLog 插件日志：去掉控制字符（保留换行和制表符），限制长度
func sanitizeLog(s string) string {
	var b strings.Builder
	n := 0
	for _, r := range s {
		if n >= logMaxRunes {
			b.WriteString("…")
			break
		}
		if r != '\n' && r != '\t' && unicode.IsControl(r) {
			continue
		}
		b.WriteRune(r)
		n++
	}
	return b.String()
}

// subscribe 新建一个事件订阅，返回订阅号
func (inst *instance) subscribe(types []string) (string, error) {
	inst.mu.Lock()
	defer inst.mu.Unlock()
	if len(inst.subs) >= 64 {
		return "", ErrInvalidArguments
	}
	inst.subSeq++
	id := "s" + strconv.Itoa(inst.subSeq)
	inst.subs[id] = append([]string{}, types...)
	inst.subOrder = append(inst.subOrder, id)
	return id, nil
}

// unsubscribe 取消订阅；订阅号不存在时返回 false
func (inst *instance) unsubscribe(id string) bool {
	inst.mu.Lock()
	defer inst.mu.Unlock()
	if _, ok := inst.subs[id]; !ok {
		return false
	}
	delete(inst.subs, id)
	for i, s := range inst.subOrder {
		if s == id {
			inst.subOrder = append(inst.subOrder[:i], inst.subOrder[i+1:]...)
			break
		}
	}
	return true
}

// matchSubscription 返回第一个匹配该事件类型的订阅号，没有匹配时为空
func (inst *instance) matchSubscription(eventType string) string {
	inst.mu.Lock()
	defer inst.mu.Unlock()
	if !inst.initialized || inst.quitting {
		return ""
	}
	for _, id := range inst.subOrder {
		for _, pattern := range inst.subs[id] {
			if globMatch(pattern, eventType) {
				return id
			}
		}
	}
	return ""
}

// ---------------- 通用工具 ----------------

// globMatch 简单通配匹配：* 匹配任意字符（包括点），其余字符逐个比较
func globMatch(pattern, s string) bool {
	if !strings.Contains(pattern, "*") {
		return pattern == s
	}
	parts := strings.Split(pattern, "*")
	if !strings.HasPrefix(s, parts[0]) {
		return false
	}
	s = s[len(parts[0]):]
	last := parts[len(parts)-1]
	for _, part := range parts[1 : len(parts)-1] {
		i := strings.Index(s, part)
		if i < 0 {
			return false
		}
		s = s[i+len(part):]
	}
	return strings.HasSuffix(s, last)
}

// resolveCommand 解析启动程序：绝对路径原样使用；含路径分隔符的相对路径以插件目录为基准；
// 裸命令名（如 python3）按 PATH 查找，不会在当前目录里找
func resolveCommand(dir, command string) (string, error) {
	command = strings.TrimSpace(command)
	if filepath.IsAbs(command) {
		return command, nil
	}
	if strings.ContainsAny(command, `/\`) {
		return filepath.Join(dir, command), nil
	}
	return exec.LookPath(command)
}

// childEnvironment 插件进程的环境变量：只继承运行所需的少量宿主变量（不透传整个宿主环境），
// 再叠加 plugin.yml 的 env
func childEnvironment(extra map[string]string) []string {
	keys := []string{"PATH", "HOME", "LANG", "TMPDIR", "SYSTEMROOT", "WINDIR", "TEMP", "TMP"}
	env := make([]string, 0, len(keys)+len(extra))
	for _, key := range keys {
		if _, override := extra[key]; override {
			continue
		}
		if value, ok := os.LookupEnv(key); ok {
			env = append(env, key+"="+value)
		}
	}
	for key, value := range extra {
		env = append(env, key+"="+value)
	}
	return env
}

// duration 毫秒配置转 time.Duration，0 用默认值
func duration(ms, fallback int) time.Duration {
	if ms == 0 {
		ms = fallback
	}
	return time.Duration(ms) * time.Millisecond
}

// contextError 把 context 的结束原因映射为插件错误码
func contextError(ctx context.Context) error {
	if errors.Is(ctx.Err(), context.DeadlineExceeded) {
		return ErrTimeout
	}
	return ErrCanceled
}

// isObject 是否为合法的 JSON 对象
func isObject(data []byte) bool {
	data = bytes.TrimSpace(data)
	return len(data) >= 2 && data[0] == '{' && json.Valid(data)
}

// strictDecode 严格解码 JSON 对象：拒绝重复字段、未知字段与尾随内容。
// encoding/json 默认对重复字段取最后一个值，会让 id、版本号等校验在不同实现间产生歧义。
func strictDecode(data []byte, dest any) error {
	keys := json.NewDecoder(bytes.NewReader(data))
	first, err := keys.Token()
	if err != nil || first != json.Delim('{') {
		return ErrProtocol
	}
	seen := make(map[string]bool)
	for keys.More() {
		token, err := keys.Token()
		key, ok := token.(string)
		if err != nil || !ok || seen[key] {
			return ErrProtocol
		}
		seen[key] = true
		if err := keys.Decode(new(json.RawMessage)); err != nil {
			return ErrProtocol
		}
	}
	d := json.NewDecoder(bytes.NewReader(data))
	d.DisallowUnknownFields()
	if err := d.Decode(dest); err != nil {
		return err
	}
	if err := d.Decode(new(any)); err != io.EOF {
		return ErrProtocol
	}
	return nil
}

// cloneCapabilities 深拷贝能力列表
func cloneCapabilities(caps []Capability) []Capability {
	out := make([]Capability, len(caps))
	for i, c := range caps {
		out[i] = c
		out[i].InputSchema = append(json.RawMessage(nil), c.InputSchema...)
		if c.OutputSchema != nil {
			out[i].OutputSchema = append(json.RawMessage(nil), c.OutputSchema...)
		}
	}
	return out
}

// stderrLog 插件 stderr 日志：写满 stderrLogLimit 后丢弃后续输出，避免插件刷屏占满磁盘
type stderrLog struct {
	mu      sync.Mutex
	f       *os.File
	written int64
	full    bool
}

// openStderrLog 以追加方式打开 <dir>/<name>.log；文件已超过上限时先轮转为 .log.old
func openStderrLog(dir, name string) (*stderrLog, error) {
	if dir == "" {
		return nil, errors.New("未配置日志目录")
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	path := filepath.Join(dir, name+".log")
	var size int64
	if st, err := os.Stat(path); err == nil {
		size = st.Size()
		if size >= stderrLogLimit {
			_ = os.Rename(path, path+".old")
			size = 0
		}
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return nil, err
	}
	_, _ = fmt.Fprintf(f, "==== %s 插件进程启动 ====\n", time.Now().Format("2006-01-02 15:04:05"))
	return &stderrLog{f: f, written: size}, nil
}

// Write 实现 io.Writer；超出上限后静默丢弃并返回成功，避免插件因 stderr 写失败而退出
func (w *stderrLog) Write(b []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.full {
		return len(b), nil
	}
	if w.written+int64(len(b)) > stderrLogLimit {
		_, _ = w.f.WriteString("\n==== 日志超过上限，后续输出已丢弃 ====\n")
		w.full = true
		return len(b), nil
	}
	n, err := w.f.Write(b)
	w.written += int64(n)
	if err != nil {
		return len(b), nil
	}
	return n, nil
}

// Close 关闭日志文件
func (w *stderrLog) Close() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.f.Close()
}
