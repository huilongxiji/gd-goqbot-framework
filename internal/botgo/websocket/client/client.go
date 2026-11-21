// Package client 默认的 websocket client 实现。
package client

import (
	"encoding/json"
	"fmt"
	"os"
	"os/signal"
	"regexp"
	"sync/atomic"
	"syscall"
	"time"

	wss "github.com/gorilla/websocket" // 是一个流行的 websocket 客户端，服务端实现
	"goqbot/internal/botgo/dto"
	"goqbot/internal/botgo/errs"
	"goqbot/internal/botgo/event"
	"goqbot/internal/botgo/log"
	"goqbot/internal/botgo/websocket"
)

// DefaultQueueSize 监听队列的缓冲长度
const DefaultQueueSize = 10000

// heartbeatMissLimit 连续这么多个心跳周期收不到服务端任何消息（含心跳回包），即判定连接已失效并主动断开重连。
// 不依赖系统 TCP 超时：网络被静默断开时，系统层面往往要好几分钟才报错，期间的消息全部丢失。
const heartbeatMissLimit = 2

// tokenFieldRE 匹配鉴权报文里的 token 字段，用于日志脱敏
var tokenFieldRE = regexp.MustCompile(`"token":"([^"]{0,10})[^"]*"`)

// Setup 依赖注册
func Setup() {
	websocket.Register(&Client{})
}

// New 新建一个连接对象
func (c *Client) New(session dto.Session) websocket.WebSocket {
	return &Client{
		messageQueue:    make(messageChan, DefaultQueueSize),
		session:         &session,
		closeChan:       make(closeErrorChan, 10),
		heartBeatTicker: time.NewTicker(60 * time.Second), // 先给一个默认 ticker，在收到 hello 包之后，会 reset
	}
}

// Client websocket 连接客户端
type Client struct {
	version         int
	conn            *wss.Conn
	messageQueue    messageChan
	session         *dto.Session
	user            *dto.WSUser
	closeChan       closeErrorChan
	heartBeatTicker *time.Ticker // 用于维持定时心跳

	heartbeatInterval atomic.Int64 // hello 下发的心跳间隔（纳秒），0=尚未收到 hello
	lastRecv          atomic.Int64 // 最近一次收到服务端消息的时间（UnixNano）
}

type messageChan chan *dto.WSPayload
type closeErrorChan chan error

// Connect 连接到 websocket
func (c *Client) Connect() error {
	if c.session.URL == "" {
		return errs.ErrURLInvalid
	}

	var err error
	c.conn, _, err = wss.DefaultDialer.Dial(c.session.URL, nil)
	if err != nil {
		log.Errorf("%s, connect err: %v", c.session, err)
		return err
	}
	log.Infof("%s, url %s, connected", c.session, c.session.URL)

	return nil
}

// Listening 开始监听，会阻塞进程，内部会从事件队列不断的读取事件，解析后投递到注册的 event handler，如果读取消息过程中发生错误，会循环
// 定时心跳也在这里维护
func (c *Client) Listening() error {
	defer c.Close()
	// reading message
	go c.readMessageToQueue()
	// read message from queue and handle,in goroutine to avoid business logic block closeChan and heartBeatTicker
	go c.listenMessageAndHandle()

	// 接收 resume signal
	resumeSignal := make(chan os.Signal, 1)
	if websocket.ResumeSignal >= syscall.SIGHUP {
		signal.Notify(resumeSignal, websocket.ResumeSignal)
	}

	// handler message
	for {
		select {
		case <-resumeSignal: // 使用信号量控制连接立即重连
			log.Infof("%s, received resumeSignal signal", c.session)
			return errs.ErrNeedReConnect
		case err := <-c.closeChan:
			// 关闭连接的错误码 https://bot.q.qq.com/wiki/develop/api/gateway/error/error.html
			log.Errorf("%s Listening stop. err is %v", c.session, err)
			// 不能够 identify 的错误
			if wss.IsCloseError(err, errs.WSCodeBackendBotOffline, errs.WSCodeBackendBotBanned) {
				err = errs.New(errs.CodeConnCloseCantIdentify, err.Error())
			}
			// accessToken过期
			if wss.IsCloseError(err, errs.WSCodeBackendAuthenticationFail) {
				_, _ = c.session.TokenSource.Token()
			}
			// 这里用 UnexpectedCloseError，如果有需要排除在外的 close error code，可以补充在第二个参数上
			// 4009: session time out, 发了 reconnect 之后马上关闭连接时候的错误码，这个是允许 resumeSignal 的
			if wss.IsUnexpectedCloseError(err, errs.WSCodeBackendSessionTimeOut) {
				err = errs.New(errs.CodeConnCloseCantResume, err.Error())
			}
			if event.DefaultHandlers.ErrorNotify != nil {
				// 通知到使用方错误
				event.DefaultHandlers.ErrorNotify(err)
			}
			return err
		case <-c.heartBeatTicker.C:
			if c.heartbeatTimedOut() {
				// 关闭底层连接，读协程随即报错并经 closeChan 走正常的断线重连（Resume）流程
				log.Warnf("%s %d 个心跳周期内未收到服务端任何消息，判定连接已失效，主动断开重连", c.session, heartbeatMissLimit)
				_ = c.conn.Close()
				continue
			}
			log.Debugf("%s listened heartBeat", c.session)
			heartBeatEvent := &dto.WSPayload{
				WSPayloadBase: dto.WSPayloadBase{
					OPCode: dto.WSHeartbeat,
				},
				Data: c.session.LastSeq,
			}
			// 不处理错误，Write 内部会处理，如果发生发包异常，会通知主协程退出
			_ = c.Write(heartBeatEvent)
		}
	}
}

// heartbeatTimedOut 是否已连续 heartbeatMissLimit 个心跳周期没有收到服务端任何消息
func (c *Client) heartbeatTimedOut() bool {
	interval := time.Duration(c.heartbeatInterval.Load())
	last := c.lastRecv.Load()
	if interval <= 0 || last == 0 {
		return false
	}
	return time.Since(time.Unix(0, last)) > interval*heartbeatMissLimit+5*time.Second
}

// maskToken 日志脱敏：鉴权报文里的 token 只保留前 10 个字符（含 "QQBot " 前缀）
func maskToken(s string) string {
	return tokenFieldRE.ReplaceAllString(s, `"token":"${1}***"`)
}

// Write 往 ws 写入数据
func (c *Client) Write(message *dto.WSPayload) error {
	m, _ := json.Marshal(message)
	log.Infof("%s write %s message, %v", c.session, dto.OPMeans(message.OPCode), maskToken(string(m)))

	if err := c.conn.WriteMessage(wss.TextMessage, m); err != nil {
		log.Errorf("%s WriteMessage failed, %v", c.session, err)
		c.closeChan <- err
		return err
	}
	return nil
}

// Resume 重连
func (c *Client) Resume() error {
	token, err := c.session.TokenSource.Token()
	if err != nil {
		log.Errorf("[resume] get access token failed:%s", err)
		return err
	}
	payload := &dto.WSPayload{
		Data: &dto.WSResumeData{
			// 与 Identify 一致须带 "QQBot " 前缀，缺少前缀会被服务端以 4004 Authentication fail 拒绝，
			// 导致每次断线都无法续传，只能重新 Identify（丢失断线期间的事件并多耗一次建连额度）
			Token:     fmt.Sprintf("%s %s", token.TokenType, token.AccessToken),
			SessionID: c.session.ID,
			Seq:       c.session.LastSeq,
		},
	}
	payload.OPCode = dto.WSResume // 内嵌结构体字段，单独赋值
	return c.Write(payload)
}

// Identify 对一个连接进行鉴权，并声明监听的 shard 信息
func (c *Client) Identify() error {
	// 避免传错 intent
	if c.session.Intent == 0 {
		c.session.Intent = dto.IntentGuilds
	}
	tk, err := c.session.TokenSource.Token()
	if err != nil {
		log.Errorf("[resume] get access token failed:%s", err)
		return err
	}
	payload := &dto.WSPayload{
		Data: &dto.WSIdentityData{
			Token:   fmt.Sprintf("%s %s", tk.TokenType, tk.AccessToken),
			Intents: c.session.Intent,
			Shard: []uint32{
				c.session.Shards.ShardID,
				c.session.Shards.ShardCount,
			},
		},
	}
	payload.OPCode = dto.WSIdentity
	return c.Write(payload)
}

// Close 关闭连接
func (c *Client) Close() {
	if err := c.conn.Close(); err != nil {
		log.Errorf("%s, close conn err: %v", c.session, err)
	}
	c.heartBeatTicker.Stop()
}

// Session 获取client的session信息
func (c *Client) Session() *dto.Session {
	return c.session
}

func (c *Client) readMessageToQueue() {
	for {
		_, message, err := c.conn.ReadMessage()
		if err == nil {
			c.lastRecv.Store(time.Now().UnixNano())
		}
		if err != nil {
			log.Errorf("%s read message failed, %v, message %s", c.session, err, string(message))
			close(c.messageQueue)
			// accessToken过期
			if wss.IsCloseError(err, errs.WSCodeBackendAuthenticationFail) {
				_, _ = c.session.TokenSource.Token()
			}
			c.closeChan <- err
			return
		}
		payload := &dto.WSPayload{}
		if err := json.Unmarshal(message, payload); err != nil {
			log.Errorf("%s json failed, %v", c.session, err)
			continue
		}
		payload.RawMessage = message
		payload.Session = c.session
		log.Infof("%s receive %s message, %s", c.session, dto.OPMeans(payload.OPCode), string(message))
		// 处理内置的一些事件，如果处理成功，则这个事件不再投递给业务
		if c.isHandleBuildIn(payload) {
			continue
		}
		c.messageQueue <- payload
	}
}

func (c *Client) listenMessageAndHandle() {
	defer func() {
		// panic，一般是由于业务自己实现的 handle 不完善导致
		// 打印日志后，关闭这个连接，进入重连流程
		if err := recover(); err != nil {
			websocket.PanicHandler(err, c.session)
			c.closeChan <- fmt.Errorf("panic: %v", err)
		}
	}()
	for payload := range c.messageQueue {
		c.saveSeq(payload.Seq)
		// ready 事件需要特殊处理
		if payload.Type == "READY" {
			c.readyHandler(payload)
			continue
		}
		// 解析具体事件，并投递给业务注册的 handler
		if err := c.handleOne(payload); err != nil {
			log.Errorf("%s parseAndHandle failed, %v", c.session, err)
		}
	}
	log.Infof("%s message queue is closed", c.session)
}

// handleOne 处理一条事件；处理出错崩溃时只跳过这一条，不断开连接（goqbot 修改）
func (c *Client) handleOne(payload *dto.WSPayload) (err error) {
	defer func() {
		if r := recover(); r != nil {
			websocket.PanicHandler(r, c.session)
			err = fmt.Errorf("处理事件 %s 时发生 panic，已跳过这条事件: %v", payload.Type, r)
		}
	}()
	return event.ParseAndHandle(payload)
}

func (c *Client) saveSeq(seq uint32) {
	if seq > 0 {
		c.session.LastSeq = seq
	}
}

// isHandleBuildIn 内置的事件处理，处理那些不需要业务方处理的事件
// return true 的时候说明事件已经被处理了
func (c *Client) isHandleBuildIn(payload *dto.WSPayload) bool {
	switch payload.OPCode {
	case dto.WSHello: // 接收到 hello 后需要开始发心跳
		c.startHeartBeatTicker(payload.RawMessage)
	case dto.WSHeartbeatAck: // 心跳 ack 不需要业务处理
	case dto.WSReconnect: // 达到连接时长，需要重新连接，此时可以通过 resume 续传原连接上的事件
		c.closeChan <- errs.ErrNeedReConnect
	case dto.WSInvalidSession: // 无效的 sessionLog，需要重新鉴权
		c.closeChan <- errs.ErrInvalidSession
	default:
		return false
	}
	return true
}

// startHeartBeatTicker 启动定时心跳
func (c *Client) startHeartBeatTicker(message []byte) {
	helloData := &dto.WSHelloData{}
	if err := event.ParseData(message, helloData); err != nil {
		log.Errorf("%s hello data parse failed, %v, message %v", c.session, err, message)
	}
	// 根据 hello 的回包，重新设置心跳的定时器时间
	interval := time.Duration(helloData.HeartbeatInterval) * time.Millisecond
	if interval <= 0 {
		return
	}
	c.heartbeatInterval.Store(int64(interval))
	c.heartBeatTicker.Reset(interval)
}

// readyHandler 针对ready返回的处理，需要记录 sessionID 等相关信息
func (c *Client) readyHandler(payload *dto.WSPayload) {
	readyData := &dto.WSReadyData{}
	if err := event.ParseData(payload.RawMessage, readyData); err != nil {
		log.Errorf("%s parseReadyData failed, %v, message %v", c.session, err, payload.RawMessage)
	}
	c.version = readyData.Version
	// 基于 ready 事件，更新 session 信息
	c.session.ID = readyData.SessionID
	c.session.Shards.ShardID = readyData.Shard[0]
	c.session.Shards.ShardCount = readyData.Shard[1]
	c.user = &dto.WSUser{
		ID:       readyData.User.ID,
		Username: readyData.User.Username,
		Bot:      readyData.User.Bot,
	}
	// 调用自定义的 ready 回调
	if event.DefaultHandlers.Ready != nil {
		event.DefaultHandlers.Ready(payload, readyData)
	}
}
