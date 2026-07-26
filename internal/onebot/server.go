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
	caller     *Caller       // 处理收到的动作请求
	writeCh    chan []byte   // 待写消息队列
	closeCh    chan struct{} // 关闭信号
	closeOnce  sync.Once     // 保证只关闭一次
	onWriteErr func([]byte)  // 写失败时回调（用于反向客户端缓存待重发消息）
}

// newWSConn 创建连接包装并启动写 goroutine
func newWSConn(conn *websocket.Conn, caller *Caller, onWriteErr func([]byte)) *wsConn {
	c := &wsConn{
		conn:       conn,
		caller:     caller,
		writeCh:    make(chan []byte, 256),
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
		case b := <-c.writeCh:
			_ = c.conn.SetWriteDeadline(time.Now().Add(15 * time.Second))
			if err := c.conn.WriteMessage(websocket.TextMessage, b); err != nil {
				if c.onWriteErr != nil {
					c.onWriteErr(b)
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
	select {
	case <-c.closeCh:
		return errConnClosed
	default:
	}
	select {
	case <-c.closeCh:
		return errConnClosed
	case c.writeCh <- b:
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
			filelog.Errorf("[onebot] API handler panic: %v", r)
			_ = c.close()
		}
	}()
	resp := c.caller.HandleRequest(payload)
	if err := c.writeText(resp); err != nil {
		filelog.Errorf("[onebot] write API response failed: %v", err)
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
	mu       sync.Mutex             // 保护 eventCon
	eventCon []*wsConn              // 当前已建立的事件推送连接
	stop     chan struct{}          // 关闭信号
}

// NewWSServer 创建正向 WS 服务端
func NewWSServer(cfg *config.WSServerConfig, obCfg *config.OneBotConfig, selfID int64, bus *EventBus, caller *Caller) *WSServer {
	s := &WSServer{
		cfg:    cfg,
		obCfg:  obCfg,
		selfID: selfID,
		bus:    bus,
		caller: caller,
		stop:   make(chan struct{}),
	}
	bus.OnPush(s.onEventPush)
	return s
}

// Run 启动监听（阻塞）
func (s *WSServer) Run() error {
	mux := http.NewServeMux()
	mux.HandleFunc("/event", s.handleEvent)
	mux.HandleFunc("/api", s.handleAPI)
	mux.HandleFunc("/", s.handleAny)

	network, address := "tcp", s.cfg.Address
	if strings.HasPrefix(s.cfg.Address, "ws://") {
		address = strings.TrimPrefix(s.cfg.Address, "ws://")
	} else if strings.HasPrefix(s.cfg.Address, "wss://") {
		address = strings.TrimPrefix(s.cfg.Address, "wss://")
	}
	listener, err := net.Listen(network, address)
	if err != nil {
		return err
	}
	log.Printf("[onebot] 正向 WS 服务器已启动: %s", listener.Addr())

	go s.heartbeatLoop()

	go func() {
		<-s.stop
		_ = listener.Close()
	}()
	return http.Serve(listener, mux)
}

// Stop 停止服务
func (s *WSServer) Stop() {
	close(s.stop)
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

// handshake 返回连接建立时下发的 lifecycle connect 元事件
func (s *WSServer) handshake(_ bool) []byte {
	return LifecycleConnect(s.selfID)
}

// handleEvent 处理 /event 端点：升级、握手并登记为事件推送连接（只推不收）
func (s *WSServer) handleEvent(w http.ResponseWriter, r *http.Request) {
	if status := checkAuth(r, s.obCfg.AccessToken); status != http.StatusOK {
		w.WriteHeader(status)
		return
	}
	c, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		filelog.Errorf("[onebot] upgrade /event failed: %v", err)
		return
	}
	if err = c.WriteMessage(websocket.TextMessage, s.handshake(false)); err != nil {
		_ = c.Close()
		return
	}
	log.Printf("[onebot] 正向 WS 连接成功: NoneBot %s (/event)", r.RemoteAddr)
	conn := newWSConn(c, s.caller, nil)
	s.mu.Lock()
	s.eventCon = append(s.eventCon, conn)
	s.mu.Unlock()
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
	log.Printf("[onebot] 正向 WS 连接成功: NoneBot %s (/api)", r.RemoteAddr)
	conn := newWSConn(c, s.caller, nil)
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
	log.Printf("[onebot] 正向 WS 连接成功: NoneBot %s (/)", r.RemoteAddr)
	conn := newWSConn(c, s.caller, nil)
	s.mu.Lock()
	s.eventCon = append(s.eventCon, conn)
	s.mu.Unlock()
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
