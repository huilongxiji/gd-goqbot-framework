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
	"time"

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

// 重启与关闭参数
const (
	restartMinDelay       = time.Second            // 第一次自动重启前的等待
	restartMaxDelay       = time.Minute            // 重启等待的上限（每次失败翻倍）
	stableAfter           = time.Minute            // 就绪持续这么久后再失败，重启等待重新从 restartMinDelay 计
	shutdownTimeout       = time.Second            // 关闭时等待插件响应 shutdown / 进程退出的时长
	stderrLogLimit  int64 = 10 << 20               // 单个插件 stderr 日志文件上限，超出后轮转为 .old
	waitDelay             = 250 * time.Millisecond // 插件进程退出后，等待其子进程释放 stdout/stderr 管道的最长时间
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

// pendingCall 正在等待响应的请求（同一实例同一时刻最多一个）
type pendingCall struct {
	id    uint64
	reply chan reply
}

// plugin 一个插件实例：持有配置与状态，并由 run 负责拉起、握手、异常后自动重启
type plugin struct {
	cfg         Config
	hostVersion string // 握手时告知插件的 goqbot 版本
	logDir      string // stderr 日志目录（log_stderr=true 时使用）

	mu      sync.Mutex
	info    Info      // 对外展示的状态快照
	inst    *instance // 当前运行中的进程；nil=没有运行
	closing bool      // 已开始关闭，不再重启

	gate    chan struct{} // 容量 1：握手与调用互斥，同一时刻只处理一个请求
	closeCh chan struct{} // 关闭信号：通知 run 退出
	done    chan struct{} // run 已退出
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

	mu      sync.Mutex
	stopErr error        // 停止原因
	nextID  uint64       // 下一个请求 id
	pending *pendingCall // 等待响应的请求
}

// newPlugin 按配置创建插件（不启动进程）
func newPlugin(cfg Config, opts Options) *plugin {
	state := StatePending
	reason := ""
	if err := cfg.Validate(); err != nil {
		state, reason = StateError, Code(err)
	} else if !cfg.Enabled {
		state = StateDisabled
	}
	return &plugin{
		cfg:         cfg,
		hostVersion: opts.HostVersion,
		logDir:      opts.LogDir,
		info:        Info{Name: cfg.Name, State: state, Reason: reason, Capabilities: []Capability{}},
		gate:        make(chan struct{}, 1),
		closeCh:     make(chan struct{}),
		done:        make(chan struct{}),
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
			filelog.Errorf("[plugins] %s 启动失败，不再自动重启: %s", p.cfg.Name, describeErr(err))
			return
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
	params := struct {
		ProtocolVersion string `json:"protocol_version"`
		Host            struct {
			Name    string `json:"name"`
			Version string `json:"version"`
		} `json:"host"`
	}{ProtocolVersion: ProtocolVersion}
	params.Host.Name, params.Host.Version = "goqbot", p.hostVersion
	data, err := inst.exchange(initCtx, "initialize", params)
	if err != nil {
		return nil, err
	}
	var hello struct {
		ProtocolVersion string       `json:"protocol_version"`
		Name            string       `json:"name"`
		Version         string       `json:"version"`
		Capabilities    []Capability `json:"capabilities"`
	}
	if strictDecode(data, &hello) != nil || hello.ProtocolVersion != ProtocolVersion ||
		!namePattern.MatchString(hello.Name) || hello.Version == "" || len(hello.Version) > 64 ||
		hello.Capabilities == nil || len(hello.Capabilities) > 128 || !validCapabilities(hello.Capabilities) {
		// 握手内容不合规是插件实现问题，重启也不会好转
		inst.stop(ErrProtocol)
		return nil, permanentError{ErrProtocol}
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
	p.info.Version = hello.Version
	p.info.Capabilities = cloneCapabilities(hello.Capabilities)
	p.info.State = StateReady
	p.info.Reason = ""
	p.mu.Unlock()
	log.Printf("[plugins] %s 已就绪: %s v%s，能力 %d 个", p.cfg.Name, hello.Name, hello.Version, len(hello.Capabilities))
	return inst, nil
}

// validCapabilities 校验握手声明的能力列表：名称合规且不重复、说明与 schema 有长度上限、schema 为对象
func validCapabilities(caps []Capability) bool {
	seen := make(map[string]bool, len(caps))
	for _, c := range caps {
		if !capabilityPattern.MatchString(c.Name) || seen[c.Name] || len(c.Description) > 1024 ||
			len(c.InputSchema) > 128*1024 || !isObject(c.InputSchema) {
			return false
		}
		seen[c.Name] = true
	}
	return true
}

// spawn 启动插件进程并登记为当前实例（调用方须持有 gate）
func (p *plugin) spawn() (*instance, error) {
	command, err := resolveCommand(p.cfg.Dir, p.cfg.Command)
	if err != nil {
		return nil, permanentError{fmt.Errorf("%w: %v", ErrStartFailed, err)}
	}
	cmd := exec.Command(command, p.cfg.Args...)
	cmd.Dir = p.cfg.Dir
	cmd.Env = childEnvironment(p.cfg.Env)
	cmd.WaitDelay = waitDelay
	configureProcess(cmd)
	inst := &instance{owner: p, cmd: cmd, waitDone: make(chan struct{}), stopCh: make(chan struct{})}
	// stderr 默认丢弃（nil=接到空设备）；开启 log_stderr 时写入有上限的日志文件
	if p.cfg.LogStderr {
		if w, err := openStderrLog(p.logDir, p.cfg.Name); err != nil {
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
	p.info.Capabilities = []Capability{}
	p.mu.Unlock()

	go func() {
		_ = cmd.Wait()
		closeStderr()
		close(inst.waitDone)
		inst.stop(ErrProcessExited)
	}()
	go inst.readLoop()
	return inst, nil
}

// call 调用一项能力。插件同一时刻只处理一个请求：正忙时立即返回 ErrBusy，不排队。
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
	select {
	case p.gate <- struct{}{}:
		defer func() { <-p.gate }()
	default:
		return nil, ErrBusy
	}
	// 拿到 gate 后再确认一次：等待期间插件可能已重启或关闭
	p.mu.Lock()
	inst, state := p.inst, p.info.State
	p.mu.Unlock()
	if state != StateReady || inst == nil {
		return nil, stateError(state)
	}
	callCtx, cancel := context.WithTimeout(ctx, duration(p.cfg.CallTimeoutMS, DefaultCallTimeoutMS))
	defer cancel()
	return inst.exchange(callCtx, "capabilities/call", struct {
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
	for _, c := range p.info.Capabilities {
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

// snapshot 返回状态快照（能力列表深拷贝，调用方修改不影响插件）
func (p *plugin) snapshot() Info {
	p.mu.Lock()
	defer p.mu.Unlock()
	info := p.info
	info.Capabilities = cloneCapabilities(info.Capabilities)
	return info
}

// shutdown 关闭插件：空闲时先发 shutdown 请求给插件留出清理时间，随后结束并回收进程。
// 正在握手或处理请求的插件直接结束。可重复调用。
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
			ctx, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
			_, _ = inst.exchange(ctx, "shutdown", struct{}{})
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

// exchange 发送一个请求并等待对应响应。超时或取消会结束整个实例（随后由 run 自动重启），
// 因为无法确认插件是否还会输出迟到的响应。
func (inst *instance) exchange(ctx context.Context, method string, params any) (json.RawMessage, error) {
	inst.mu.Lock()
	select {
	case <-inst.stopCh:
		inst.mu.Unlock()
		return nil, inst.stoppedError()
	default:
	}
	inst.nextID++
	call := &pendingCall{id: inst.nextID, reply: make(chan reply, 1)}
	inst.pending = call
	inst.mu.Unlock()

	frame, err := json.Marshal(struct {
		JSONRPC string `json:"jsonrpc"`
		ID      uint64 `json:"id"`
		Method  string `json:"method"`
		Params  any    `json:"params"`
	}{JSONRPC: "2.0", ID: call.id, Method: method, Params: params})
	if err != nil || len(frame) > MaxFrameBytes {
		inst.stop(ErrProtocol)
		return nil, ErrProtocol
	}
	// 写入放在 goroutine 里，插件不读 stdin 导致管道写满时也能按超时退出
	writeDone := make(chan error, 1)
	go func() {
		_, err := inst.stdin.Write(append(frame, '\n'))
		writeDone <- err
	}()
	select {
	case err := <-writeDone:
		if err != nil {
			inst.stop(ErrProcessExited)
			return nil, inst.stoppedError()
		}
	case <-ctx.Done():
		err := contextError(ctx)
		inst.stop(err)
		return nil, err
	case <-inst.stopCh:
		return nil, inst.stoppedError()
	}
	select {
	case r := <-call.reply:
		return r.result, r.err
	case <-ctx.Done():
		err := contextError(ctx)
		inst.stop(err)
		return nil, err
	case <-inst.stopCh:
		return nil, inst.stoppedError()
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

// stop 结束实例：记录首个停止原因，结束整个进程组并关闭管道。可重复调用。
func (inst *instance) stop(reason error) {
	inst.stopOnce.Do(func() {
		inst.mu.Lock()
		inst.stopErr = reason
		inst.mu.Unlock()
		close(inst.stopCh)
		killProcess(inst.cmd)
		_ = inst.stdin.Close()
		_ = inst.stdout.Close()
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
		if err := inst.receive(bytes.TrimSuffix(frame, []byte{'\n'})); err != nil {
			inst.stop(err)
			return
		}
	}
}

// receive 校验一帧响应并交给等待中的请求；任何不合规（字段重复、id 不匹配、result 非对象等）都视为协议错误
func (inst *instance) receive(frame []byte) error {
	var msg struct {
		JSONRPC string          `json:"jsonrpc"`
		ID      json.RawMessage `json:"id"`
		Result  json.RawMessage `json:"result"`
		Error   json.RawMessage `json:"error"`
	}
	if strictDecode(frame, &msg) != nil || msg.JSONRPC != "2.0" ||
		(len(msg.Result) == 0) == (len(msg.Error) == 0) {
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
	defer inst.mu.Unlock()
	if inst.pending == nil || inst.pending.id != id {
		return ErrProtocol
	}
	inst.pending.reply <- response
	inst.pending = nil
	return nil
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
