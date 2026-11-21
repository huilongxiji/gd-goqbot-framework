package onebot

import (
	"bytes"
	"io"
	"log"
	"net/http"
	"strconv"
	"sync"
	"time"

	"github.com/gorilla/websocket"
	"goqbot/internal/config"
	"goqbot/internal/filelog"
)

// WSClient 反向 OneBot v11 WebSocket 客户端
type WSClient struct {
	cfg       *config.WSReverseServerConfig // 本连接配置（universal/api/event 端点与重连间隔）
	obCfg     *config.OneBotConfig          // 下游全局配置（access_token、心跳等）
	selfID    int64                         // 握手与心跳上报用 self_id
	bus       *EventBus                     // 事件总线，订阅后向下游推事件
	caller    *Caller                       // 动作调用器，处理下游 API 请求
	mu        sync.Mutex                    // 保护 universal/event 连接指针
	universal *wsConn                       // universal 端点连接（收发合一）
	event     *wsConn                       // event 端点连接（仅推事件）
	api       *wsConn                       // api 端点连接（仅收动作请求）
	stopped   bool                          // 已调用 Stop（受 mu 保护）
	stop      chan struct{}                 // 关闭信号，触发各 goroutine 退出
	stopOnce  sync.Once
	unsub     func() // 取消事件总线订阅

	failMu       sync.Mutex
	sendFailures [][]byte // 写失败缓存，跨重连重发
}

// cacheFailure 缓存一条发送失败的消息，待下次连接成功后重发
func (c *WSClient) cacheFailure(b []byte) {
	c.failMu.Lock()
	defer c.failMu.Unlock()
	c.sendFailures = append(c.sendFailures, append([]byte(nil), b...))
}

// flushFailures 将缓存的失败消息逐条投递到新连接
func (c *WSClient) flushFailures(conn *wsConn) {
	c.failMu.Lock()
	pending := c.sendFailures
	c.sendFailures = nil
	c.failMu.Unlock()
	for _, b := range pending {
		if err := conn.writeText(b); err != nil {
			return
		}
	}
}

// NewWSClient 创建反向 WS 客户端
func NewWSClient(cfg *config.WSReverseServerConfig, obCfg *config.OneBotConfig, selfID int64, bus *EventBus, caller *Caller) *WSClient {
	c := &WSClient{
		cfg:    cfg,
		obCfg:  obCfg,
		selfID: selfID,
		bus:    bus,
		caller: caller,
		stop:   make(chan struct{}),
	}
	c.unsub = bus.OnPush(c.onEventPush)
	return c
}

// Run 启动连接（非阻塞，内部 goroutine 维护重连）
func (c *WSClient) Run() {
	if c.cfg.Universal != "" {
		go c.connectLoop("Universal", c.cfg.Universal, &c.universal, true)
		return
	}
	if c.cfg.API != "" {
		go c.connectLoop("API", c.cfg.API, &c.api, false)
	}
	if c.cfg.Event != "" {
		go c.connectLoop("Event", c.cfg.Event, &c.event, true)
	}
}

// Stop 停止客户端：断开所有端点，不再重连（可重复调用）
func (c *WSClient) Stop() {
	c.stopOnce.Do(func() {
		close(c.stop)
		c.unsub()
		c.mu.Lock()
		defer c.mu.Unlock()
		c.stopped = true
		for _, conn := range []*wsConn{c.universal, c.event, c.api} {
			if conn != nil {
				_ = conn.close()
			}
		}
	})
}

// reconnectInterval 返回重连等待间隔，未配置时默认 5s
func (c *WSClient) reconnectInterval() time.Duration {
	if c.cfg.ReconnectInterval <= 0 {
		return 5 * time.Second
	}
	return time.Duration(c.cfg.ReconnectInterval) * time.Millisecond
}

// connectLoop 维护单个端点的连接：断开后按 reconnectInterval 重连，直到收到 stop
// 连不上时只在第一次和之后每 5 分钟提醒一次，其余失败只在调试模式记录，避免每隔几秒刷屏
func (c *WSClient) connectLoop(typ, addr string, conptr **wsConn, sendHandshake bool) {
	fails := 0
	var lastWarn time.Time
	for {
		select {
		case <-c.stop:
			return
		default:
		}
		if err := c.connect(typ, addr, conptr, sendHandshake, fails); err != nil {
			fails++
			switch {
			case fails == 1:
				filelog.Warnf("[onebot] 反向 WS %s 连不上 %s：%v（每 %s 自动重试，框架启动后会自动连上）", typ, addr, err, c.reconnectInterval())
				lastWarn = time.Now()
			case time.Since(lastWarn) >= 5*time.Minute:
				filelog.Warnf("[onebot] 反向 WS %s 仍然连不上 %s（已重试 %d 次）：%v", typ, addr, fails, err)
				lastWarn = time.Now()
			default:
				filelog.Debugf("[onebot] 反向 WS %s 连接失败 %s: %v", typ, addr, err)
			}
		} else {
			fails = 0
		}
		select {
		case <-c.stop:
			return
		case <-time.After(c.reconnectInterval()):
		}
	}
}

// connect 建立一次连接并阻塞监听：带鉴权头握手，可选发 lifecycle，重发失败缓存，
// 按端点类型分流到 listenAPI/listenEvent
func (c *WSClient) connect(typ, addr string, conptr **wsConn, sendHandshake bool, prevFails int) error {
	filelog.Debugf("[onebot] 连接反向 WebSocket %s: %s", typ, addr)
	header := http.Header{
		"X-Client-Role": []string{typ},
		"X-Self-ID":     []string{strconv.FormatInt(c.selfID, 10)},
		"User-Agent":    []string{c.caller.appName + "/" + c.caller.appVersion},
	}
	if c.obCfg.AccessToken != "" {
		header["Authorization"] = []string{"Token " + c.obCfg.AccessToken}
	}
	conn, _, err := websocket.DefaultDialer.Dial(addr, header)
	if err != nil {
		return err
	}
	if sendHandshake {
		if err = conn.WriteMessage(websocket.TextMessage, LifecycleConnect(c.selfID)); err != nil {
			_ = conn.Close()
			return err
		}
	}
	if prevFails > 0 {
		log.Printf("[onebot] 反向 WS %s 已连接 %s（之前失败 %d 次）", typ, addr, prevFails)
	} else {
		log.Printf("[onebot] 反向 WS %s 已连接 %s", typ, addr)
	}

	wrapped := newWSConn(conn, c.caller, c.cacheFailure, requestScope{allowSensitivePlugins: c.cfg.AllowSensitivePlugins && c.obCfg.AccessToken != ""})
	c.mu.Lock()
	if c.stopped {
		c.mu.Unlock()
		_ = wrapped.close()
		return nil
	}
	if *conptr != nil {
		_ = (*conptr).close()
	}
	*conptr = wrapped
	c.mu.Unlock()
	c.flushFailures(wrapped)

	if typ != "Event" {
		c.listenAPI(typ, wrapped)
	} else {
		c.listenEvent(wrapped)
	}
	return nil
}

// listenEvent 仅事件端点：读循环只为感知断开，收到的内容丢弃
func (c *WSClient) listenEvent(conn *wsConn) {
	defer func() { _ = conn.close() }()
	for {
		if _, _, err := conn.conn.ReadMessage(); err != nil {
			return
		}
	}
}

// listenAPI API/universal 端点：读取动作请求并各起 goroutine 处理
func (c *WSClient) listenAPI(typ string, conn *wsConn) {
	defer func() { _ = conn.close() }()
	for {
		select {
		case <-c.stop:
			return
		default:
		}
		t, reader, err := conn.conn.NextReader()
		if err != nil {
			select {
			case <-c.stop: // 主动停止，不算故障
			default:
				filelog.Warnf("[onebot] 反向 WS %s 读取失败: %v", typ, err)
			}
			return
		}
		buf := bytes.NewBuffer(nil)
		if _, err = io.Copy(buf, reader); err != nil {
			return
		}
		if t == websocket.TextMessage {
			payload := append([]byte(nil), buf.Bytes()...)
			go conn.handleRequest(payload)
		}
	}
}

// onEventPush 事件总线回调：向 universal/event 连接写事件，返回成功投递数
func (c *WSClient) onEventPush(ev *Event) int {
	c.mu.Lock()
	defer c.mu.Unlock()
	delivered := 0
	push := func(conn *wsConn, typ string) {
		if conn == nil {
			return
		}
		if err := conn.tryWriteText(ev.Raw); err != nil {
			filelog.Warnf("[onebot] 向反向 WS %s 推送事件失败: %v，已断开（会自动重连）", typ, err)
			_ = conn.close()
			return
		}
		delivered++
	}
	push(c.universal, "Universal")
	push(c.event, "Event")
	return delivered
}

// StartHeartbeat 启动反向连接心跳推送
func (c *WSClient) StartHeartbeat() {
	if c.obCfg.HeartbeatInterval <= 0 {
		return
	}
	go func() {
		ticker := time.NewTicker(time.Duration(c.obCfg.HeartbeatInterval) * time.Millisecond)
		defer ticker.Stop()
		for {
			select {
			case <-c.stop:
				return
			case <-ticker.C:
				c.onEventPush(&Event{Raw: Heartbeat(c.selfID, int64(c.obCfg.HeartbeatInterval))})
			}
		}
	}()
}

// Status 反向 WS 的连接状态（供插件的 host.info 查询）。只配置了 api 端点时以事件端点为准，无法判断时为 false。
func (c *WSClient) Status() map[string]any {
	addr := c.cfg.Universal
	if addr == "" {
		addr = c.cfg.Event
	}
	if addr == "" {
		addr = c.cfg.API
	}
	c.mu.Lock()
	connected := (c.universal != nil && !c.universal.isClosed()) || (c.event != nil && !c.event.isClosed())
	c.mu.Unlock()
	return map[string]any{"type": "ws-reverse", "address": addr, "connected": connected}
}
