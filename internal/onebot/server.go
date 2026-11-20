package onebot

import (
	"bytes"
	"errors"
	"io"
	"log"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"
	"goqbot/internal/config"
	"goqbot/internal/filelog"
)

// upgrader 把 HTTP 请求升级为 WebSocket，放开同源校验（下游可任意来源接入）
var upgrader = websocket.Upgrader{
	CheckOrigin: func(_ *http.Request) bool { return true },
}

// errConnClosed 表示连接已关闭，写入被丢弃
var errConnClosed = errors.New("ws connection closed")

// wsConn 包装一个 websocket 连接，所有写入经由专用 writer goroutine 串行化
type wsConn struct {
	conn       *websocket.Conn
	caller     *Caller            // 处理收到的动作请求
	scope      requestScope       // 本连接发起的动作请求所拥有的权限
	writeCh    chan outboundFrame // 待写消息队列
	closeCh    chan struct{}      // 关闭信号
	closeOnce  sync.Once          // 保证只关闭一次
	onWriteErr func([]byte)       // 写失败时回调（用于反向客户端缓存待重发消息）
}

// outboundFrame 待写出的一帧
type outboundFrame struct {
	payload []byte
	replay  bool // 写失败时能否进入重连重发缓存；插件响应可能含凭证，不能跨连接重发
}

// newWSConn 创建连接包装并启动写 goroutine
func newWSConn(conn *websocket.Conn, caller *Caller, onWriteErr func([]byte), scope requestScope) *wsConn {
	c := &wsConn{
		conn:       conn,
		caller:     caller,
		scope:      scope,
		writeCh:    make(chan outboundFrame, 256),
		closeCh:    make(chan struct{}),
		onWriteErr: onWriteErr,
	}
	go c.startWriter()
	return c
}

// startWriter 串行消费写队列，写失败则回调并关闭连接
func (c *wsConn) startWriter() {
	for {
		select {
		case frame := <-c.writeCh:
			_ = c.conn.SetWriteDeadline(time.Now().Add(15 * time.Second))
			if err := c.conn.WriteMessage(websocket.TextMessage, frame.payload); err != nil {
				if frame.replay && c.onWriteErr != nil {
					c.onWriteErr(frame.payload)
				}
				_ = c.close()
				return
			}
		case <-c.closeCh:
			return
		}
	}
}

// writeText 将消息投递到写队列；连接已关闭则返回 errConnClosed
func (c *wsConn) writeText(b []byte) error {
	return c.writeFrame(outboundFrame{payload: b, replay: true})
}

// writeFrame 将一帧投递到写队列；连接已关闭则返回 errConnClosed
func (c *wsConn) writeFrame(frame outboundFrame) error {
	select {
	case <-c.closeCh:
		return errConnClosed
	default:
	}
	select {
	case <-c.closeCh:
		return errConnClosed
	case c.writeCh <- frame:
		return nil
	}
}

// close 幂等关闭连接：发关闭信号并关底层 conn
func (c *wsConn) close() error {
	c.closeOnce.Do(func() { close(c.closeCh) })
	return c.conn.Close()
}

// handleRequest 处理一条动作请求并把响应写回（带 panic 保护）
func (c *wsConn) handleRequest(payload []byte) {
	defer func() {
		if r := recover(); r != nil {
			filelog.Errorf("[onebot] 处理动作请求时发生 panic: %v", r)
			_ = c.close()
		}
	}()
	resp := c.caller.handleScopedRequest(payload, c.scope)
	// 插件结果可能包含 Cookie 等凭证，只属于本次连接，不放入重连重发缓存
	if err := c.writeFrame(outboundFrame{payload: resp, replay: !c.caller.isPluginRequest(payload)}); err != nil {
		filelog.Errorf("[onebot] 写回动作响应失败: %v", err)
	}
}

// listenAPI 读循环：把文本帧作为动作请求各起 goroutine 处理
func (c *wsConn) listenAPI() {
	defer func() { _ = c.close() }()
	for {
		t, reader, err := c.conn.NextReader()
		if err != nil {
			return
		}
		buf := bytes.NewBuffer(nil)
		if _, err = io.Copy(buf, reader); err != nil {
			return
		}
		if t == websocket.TextMessage {
			payload := append([]byte(nil), buf.Bytes()...)
			go c.handleRequest(payload)
		}
	}
}

// WSServer 正向 OneBot v11 WebSocket 服务端
type WSServer struct {
	cfg      *config.WSServerConfig // 监听配置（地址等）
	obCfg    *config.OneBotConfig   // 下游全局配置（access_token、心跳等）
	selfID   int64                  // 握手与心跳上报用 self_id
	bus      *EventBus              // 事件总线，订阅后向已连下游推事件
	caller   *Caller                // 动作调用器，处理下游 API 请求
	mu       sync.Mutex             // 保护 eventCon、conns、stopped
	eventCon []*wsConn              // 当前已建立的事件推送连接
	conns    map[*wsConn]struct{}   // 全部已建立的连接（停止时一并断开）
	stopped  bool
	unsub    func()        // 取消事件总线订阅
	listener net.Listener  // Start 成功后的监听
	stop     chan struct{} // 关闭信号
	stopOnce sync.Once
}

// NewWSServer 创建正向 WS 服务端
func NewWSServer(cfg *config.WSServerConfig, obCfg *config.OneBotConfig, selfID int64, bus *EventBus, caller *Caller) *WSServer {
	s := &WSServer{
		cfg:    cfg,
		obCfg:  obCfg,
		selfID: selfID,
		bus:    bus,
		caller: caller,
		conns:  map[*wsConn]struct{}{},
		stop:   make(chan struct{}),
	}
	s.unsub = bus.OnPush(s.onEventPush)
	return s
}

// listenAddress 去掉地址里可能带的 ws:// 前缀
func listenAddress(addr string) string {
	if after, ok := strings.CutPrefix(addr, "ws://"); ok {
		return after
	}
	if after, ok := strings.CutPrefix(addr, "wss://"); ok {
		return after
	}
	return addr
}

// Start 开始监听（不阻塞）。端口被占用等错误直接返回，此时服务已停止，不需要再调用 Stop
func (s *WSServer) Start() error {
	mux := http.NewServeMux()
	mux.HandleFunc("/event", s.handleEvent)
	mux.HandleFunc("/api", s.handleAPI)
	mux.HandleFunc("/", s.handleAny)

	listener, err := net.Listen("tcp", listenAddress(s.cfg.Address))
	if err != nil {
		s.Stop()
		return err
	}
	s.mu.Lock()
	s.listener = listener
	s.mu.Unlock()
	log.Printf("[onebot] 正向 WS 服务器已启动: %s", listener.Addr())

	go s.heartbeatLoop()
	go func() {
		err := http.Serve(listener, mux)
		select {
		case <-s.stop:
		default:
			filelog.Errorf("[onebot] 正向 WS 服务退出: %v", err)
		}
	}()
	return nil
}

// Stop 停止服务：关闭监听，断开所有已连入的下游（可重复调用）
func (s *WSServer) Stop() {
	s.stopOnce.Do(func() {
		close(s.stop)
		s.unsub()
		s.mu.Lock()
		s.stopped = true
		listener := s.listener
		conns := make([]*wsConn, 0, len(s.conns))
		for c := range s.conns {
			conns = append(conns, c)
		}
		s.conns = map[*wsConn]struct{}{}
		s.eventCon = nil
		s.mu.Unlock()
		if listener != nil {
			_ = listener.Close()
			log.Printf("[onebot] 正向 WS 服务器已停止: %s", listener.Addr())
		}
		for _, c := range conns {
			_ = c.close()
		}
	})
}

// track 登记一条新连接；服务已停止时直接断开并返回 false
func (s *WSServer) track(conn *wsConn, event bool) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.stopped {
		_ = conn.close()
		return false
	}
	s.conns[conn] = struct{}{}
	if event {
		s.eventCon = append(s.eventCon, conn)
	}
	return true
}

// untrack 连接断开后移除登记
func (s *WSServer) untrack(conn *wsConn) {
	s.mu.Lock()
	delete(s.conns, conn)
	s.mu.Unlock()
}

// heartbeatLoop 按配置间隔向所有事件连接推送 heartbeat 元事件
func (s *WSServer) heartbeatLoop() {
	if s.obCfg.HeartbeatInterval <= 0 {
		return
	}
	ticker := time.NewTicker(time.Duration(s.obCfg.HeartbeatInterval) * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case <-s.stop:
			return
		case <-ticker.C:
			s.onEventPush(&Event{Raw: Heartbeat(s.selfID, int64(s.obCfg.HeartbeatInterval))})
		}
	}
}

// checkAuth 校验下游鉴权：token 为空放行，否则比对 Authorization 头或 access_token 查询参数
func checkAuth(r *http.Request, token string) int {
	if token == "" {
		return http.StatusOK
	}
	auth := r.Header.Get("Authorization")
	if auth == "" {
		auth = r.URL.Query().Get("access_token")
	} else if _, after, ok := strings.Cut(auth, " "); ok {
		auth = after
	}
	if auth != token {
		return http.StatusUnauthorized
	}
	return http.StatusOK
}

// scope 经本监听连入的下游所拥有的权限
func (s *WSServer) scope() requestScope {
	// 敏感能力只开放给设置了口令的连接
	return requestScope{allowSensitivePlugins: s.cfg.AllowSensitivePlugins && s.obCfg.AccessToken != ""}
}

// handshake 返回连接建立时下发的 lifecycle connect 元事件
func (s *WSServer) handshake(_ bool) []byte {
	return LifecycleConnect(s.selfID)
}

// clientLabel 用握手请求的 User-Agent 标识下游（多个框架可同时接入，不能默认是 NoneBot），未携带时显示「未知」
func clientLabel(r *http.Request) string {
	ua := []rune(strings.TrimSpace(r.UserAgent()))
	if len(ua) == 0 {
		return "未知"
	}
	if len(ua) > 64 {
		ua = append(ua[:64], '…')
	}
	return string(ua)
}

// handleEvent 处理 /event 端点：升级、握手并登记为事件推送连接（只推不收）
func (s *WSServer) handleEvent(w http.ResponseWriter, r *http.Request) {
	if status := checkAuth(r, s.obCfg.AccessToken); status != http.StatusOK {
		w.WriteHeader(status)
		return
	}
	c, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		filelog.Errorf("[onebot] /event 升级为 WebSocket 失败: %v", err)
		return
	}
	if err = c.WriteMessage(websocket.TextMessage, s.handshake(false)); err != nil {
		_ = c.Close()
		return
	}
	log.Printf("[onebot] 正向 WS 连接成功: %s (/event)，客户端 %s", r.RemoteAddr, clientLabel(r))
	conn := newWSConn(c, s.caller, nil, s.scope())
	s.track(conn, true)
}

// handleAPI 处理 /api 端点：升级后只接收并处理动作请求
func (s *WSServer) handleAPI(w http.ResponseWriter, r *http.Request) {
	if status := checkAuth(r, s.obCfg.AccessToken); status != http.StatusOK {
		w.WriteHeader(status)
		return
	}
	c, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		return
	}
	log.Printf("[onebot] 正向 WS 连接成功: %s (/api)，客户端 %s", r.RemoteAddr, clientLabel(r))
	conn := newWSConn(c, s.caller, nil, s.scope())
	if !s.track(conn, false) {
		return
	}
	defer s.untrack(conn)
	conn.listenAPI()
}

// handleAny 处理 / 通用端点：收发合一，既登记为事件连接又监听动作请求
func (s *WSServer) handleAny(w http.ResponseWriter, r *http.Request) {
	if status := checkAuth(r, s.obCfg.AccessToken); status != http.StatusOK {
		w.WriteHeader(status)
		return
	}
	c, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		return
	}
	if err = c.WriteMessage(websocket.TextMessage, s.handshake(true)); err != nil {
		_ = c.Close()
		return
	}
	log.Printf("[onebot] 正向 WS 连接成功: %s (/)，客户端 %s", r.RemoteAddr, clientLabel(r))
	conn := newWSConn(c, s.caller, nil, s.scope())
	if !s.track(conn, true) {
		return
	}
	defer s.untrack(conn)
	conn.listenAPI()
}

// onEventPush 事件总线回调：向所有事件连接写事件，剔除写失败的连接并返回成功投递数
func (s *WSServer) onEventPush(ev *Event) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	j := 0
	for i := 0; i < len(s.eventCon); i++ {
		conn := s.eventCon[i]
		if err := conn.writeText(ev.Raw); err != nil {
			_ = conn.close()
			delete(s.conns, conn)
			continue
		}
		if i != j {
			s.eventCon[j] = conn
		}
		j++
	}
	s.eventCon = s.eventCon[:j]
	return j
}

// isClosed 连接是否已关闭
func (c *wsConn) isClosed() bool {
	select {
	case <-c.closeCh:
		return true
	default:
		return false
	}
}

// Status 正向 WS 的连接状态（供插件的 host.info 查询）
func (s *WSServer) Status() map[string]any {
	s.mu.Lock()
	n := 0
	for _, conn := range s.eventCon {
		if !conn.isClosed() {
			n++
		}
	}
	s.mu.Unlock()
	return map[string]any{"type": "ws", "address": s.cfg.Address, "connected": n > 0, "clients": n}
}
