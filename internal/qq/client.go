// Package qq 负责与 QQ 官方机器人上游的连接：基于内化的 botgo 完成鉴权、
// 按配置订阅事件并维持分片 WebSocket（client.go），把上游事件交给 onebot
// 转换器；同时实现 OneBot 动作所需的消息发送/撤回/上传等出站能力（sender.go）。
package qq

import (
	"context"
	"log"
	"sync"
	"time"

	"goqbot/internal/botgo"
	"goqbot/internal/botgo/dto"
	"goqbot/internal/botgo/event"
	"goqbot/internal/botgo/openapi"
	"goqbot/internal/botgo/sessions/manager"
	"goqbot/internal/botgo/token"
	"goqbot/internal/botgo/websocket"
	"golang.org/x/oauth2"

	"goqbot/internal/config"
	"goqbot/internal/filelog"
	"goqbot/internal/idmap"
	"goqbot/internal/onebot"
)

// Client 上游 QQ 官方机器人连接
type Client struct {
	cfg    *config.QQConfig   // QQ 凭据与连接选项
	debug  bool               // true=打印原始报文等调试日志
	ids    *idmap.Store       // openid<->数字 id 映射，传给转换器/发送器
	conv   *onebot.Converter  // 上游事件 -> OneBot 事件的转换器
	caller *onebot.Caller     // OneBot 动作调用器，启动时注入 sender/网关查询器/昵称
	api    openapi.OpenAPI    // QQ 官方 openapi 客户端，用于互动事件自动回执
	cancel context.CancelFunc // 取消运行上下文以停止连接

	mu      sync.RWMutex // 保护 botName/ready
	botName string       // READY 后的机器人用户名
	ready   bool         // 是否已完成 READY
}

// NewClient 创建 QQ 客户端
func NewClient(cfg *config.QQConfig, ids *idmap.Store, conv *onebot.Converter, caller *onebot.Caller, debug bool) *Client {
	return &Client{cfg: cfg, ids: ids, conv: conv, caller: caller, debug: debug}
}

// Start 启动 QQ 连接（非阻塞）
func (c *Client) Start(ctx context.Context) error {
	if c.cfg.Disabled || c.cfg.AppID == "" || c.cfg.AppSecret() == "" {
		log.Println("[qq] 未配置 app_id/client_secret 或已 disabled，跳过 QQ 连接")
		return nil
	}

	credentials := &token.QQBotCredentials{
		AppID:     c.cfg.AppID,
		AppSecret: c.cfg.AppSecret(),
	}
	tokenSource := token.NewQQBotTokenSource(credentials)
	runCtx, cancel := context.WithCancel(ctx)
	c.cancel = cancel

	if err := token.StartRefreshAccessToken(runCtx, tokenSource); err != nil {
		cancel()
		return err
	}

	var api openapi.OpenAPI
	if c.cfg.Sandbox {
		api = botgo.NewSandboxOpenAPI(c.cfg.AppID, tokenSource)
	} else {
		api = botgo.NewOpenAPI(c.cfg.AppID, tokenSource)
	}
	// 默认 5s 对富媒体上传（base64 大体积 body + 服务端转存）过短，会 context deadline exceeded；
	// 调大到 60s。文本等小请求正常仍毫秒级返回，不受影响。
	api = api.WithTimeout(60 * time.Second)

	// 保存 openapi，供互动事件自动回执使用
	c.api = api

	// 注入发送通道
	c.caller.SetSender(newSender(api, c.ids))

	// 注入 /gateway/bot 查询器（供 get_gateway 动作查询 ws 剩余连接次数）
	c.caller.SetGatewayQuerier(func(ctx context.Context) (*onebot.GatewayInfo, error) {
		ap, err := api.WS(ctx, nil, "")
		if err != nil {
			return nil, err
		}
		return &onebot.GatewayInfo{
			URL:            ap.URL,
			Shards:         ap.Shards,
			Total:          ap.SessionStartLimit.Total,
			Remaining:      ap.SessionStartLimit.Remaining,
			ResetAfter:     ap.SessionStartLimit.ResetAfter,
			MaxConcurrency: ap.SessionStartLimit.MaxConcurrency,
		}, nil
	})

	intent := c.registerHandlers()
	log.Printf("[qq] 订阅 intent=%d (shard %d/%d)", intent, c.cfg.ShardID, c.cfg.ShardCount)

	go c.runShard(runCtx, api, tokenSource, intent)
	return nil
}

// Stop 停止 QQ 连接
func (c *Client) Stop() {
	if c.cancel != nil {
		c.cancel()
	}
}

// autoAckInteraction 异步回执互动事件（code=0 表示已收到），
// 避免 QQ 客户端因未回应而一直 loading 直至超时。插件仍可再用 put_interaction 覆盖回执码。
func (c *Client) autoAckInteraction(interactionID string) {
	if c.api == nil || interactionID == "" {
		return
	}
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := c.api.PutInteraction(ctx, interactionID, `{"code":0}`); err != nil {
			filelog.Warnf("[qq] 互动事件自动回执失败 id=%s: %v", interactionID, err)
		}
	}()
}

// registerHandlers 按 text_intent 注册 botgo handler 并推导 intent
func (c *Client) registerHandlers() dto.Intent {
	ready := event.ReadyHandler(func(_ *dto.WSPayload, data *dto.WSReadyData) {
		c.mu.Lock()
		c.botName = data.User.Username
		c.ready = true
		c.mu.Unlock()
		c.caller.SetNickname(data.User.Username)
		log.Printf("[qq] ====== QQ 连接成功 ======")
		log.Printf("[qq] 机器人: %s  self_id: %d  openid: %s", data.User.Username, c.cfg.SelfID, data.User.ID)
		log.Printf("[qq] 分片: [%d/%d]  session_id: %s", c.cfg.ShardID, c.cfg.ShardCount, data.SessionID)
		log.Printf("[qq] =========================")
	})
	errNotify := event.ErrorNotifyHandler(func(err error) {
		filelog.Errorf("[qq] WebSocket 错误: %v", err)
	})

	// Ready / ErrorNotify 始终注册（不影响 intent）
	handlers := []interface{}{ready, errNotify}

	for _, name := range c.cfg.TextIntent {
		switch name {
		case "ReadyHandler", "ErrorNotifyHandler":
			// 已默认注册
		case "GroupATMessageEventHandler":
			handlers = append(handlers, event.GroupATMessageEventHandler(
				func(p *dto.WSPayload, data *dto.WSGroupATMessageData) error {
					if c.debug && p != nil {
						log.Printf("[qq] 群@原始报文: %s", string(p.RawMessage))
					}
					c.conv.Group((*dto.Message)(data))
					return nil
				}))
		case "GroupMessageEventHandler":
			handlers = append(handlers, event.GroupMessageEventHandler(
				func(p *dto.WSPayload, data *dto.WSGroupMessageData) error {
					if c.debug && p != nil {
						log.Printf("[qq] 群全量原始报文: %s", string(p.RawMessage))
					}
					c.conv.GroupAll((*dto.Message)(data))
					return nil
				}))
		case "C2CMessageEventHandler":
			handlers = append(handlers, event.C2CMessageEventHandler(
				func(_ *dto.WSPayload, data *dto.WSC2CMessageData) error {
					c.conv.C2C((*dto.Message)(data))
					return nil
				}))
		case "C2CFriendEventHandler":
			handlers = append(handlers, event.C2CFriendEventHandler(
				func(p *dto.WSPayload, data *dto.WSC2CFriendData) error {
					if p != nil && p.Type == dto.EventC2CFriendAdd {
						c.conv.FriendAdd(data)
					} else {
						c.conv.FriendDel(data)
					}
					return nil
				}))
		case "GroupAddRobotEventHandler":
			handlers = append(handlers, event.GroupAddRobotEventHandler(
				func(_ *dto.WSPayload, data *dto.WSGroupRobotData) error {
					c.conv.GroupAddRobot(data)
					return nil
				}))
		case "GroupDelRobotEventHandler":
			handlers = append(handlers, event.GroupDelRobotEventHandler(
				func(_ *dto.WSPayload, data *dto.WSGroupRobotData) error {
					c.conv.GroupDelRobot(data)
					return nil
				}))
		case "GroupMsgRejectEventHandler":
			handlers = append(handlers, event.GroupMsgRejectEventHandler(
				func(_ *dto.WSPayload, data *dto.WSGroupRobotData) error {
					c.conv.GroupMsgReject(data)
					return nil
				}))
		case "GroupMsgReceiveEventHandler":
			handlers = append(handlers, event.GroupMsgReceiveEventHandler(
				func(_ *dto.WSPayload, data *dto.WSGroupRobotData) error {
					c.conv.GroupMsgReceive(data)
					return nil
				}))
		case "GroupMemberEventHandler":
			handlers = append(handlers, event.GroupMemberEventHandler(
				func(p *dto.WSPayload, data *dto.WSGroupMemberData) error {
					if c.debug && p != nil {
						log.Printf("[qq] 群成员进退原始报文: %s", string(p.RawMessage))
					}
					if p != nil && p.Type == dto.EventGroupMemberAdd {
						c.conv.GroupMemberAdd(data)
					} else {
						c.conv.GroupMemberRemove(data)
					}
					return nil
				}))
		case "C2CMsgRejectEventHandler":
			handlers = append(handlers, event.C2CMsgRejectEventHandler(
				func(_ *dto.WSPayload, data *dto.WSC2CManageData) error {
					c.conv.C2CMsgReject(data)
					return nil
				}))
		case "C2CMsgReceiveEventHandler":
			handlers = append(handlers, event.C2CMsgReceiveEventHandler(
				func(_ *dto.WSPayload, data *dto.WSC2CManageData) error {
					c.conv.C2CMsgReceive(data)
					return nil
				}))
		case "InteractionHandler", "InteractionEventHandler":
			handlers = append(handlers, event.InteractionEventHandler(
				func(p *dto.WSPayload, data *dto.WSInteractionData) error {
					// 外层 payload.id 才是发被动消息用的 event_id；内层 data.ID 仅用于 PUT 回执
					if p != nil && data.EventID == "" {
						data.EventID = p.EventID
					}
					// 自动回执互动事件，否则 QQ 客户端会一直转圈直到超时（请求第三方失败）
					c.autoAckInteraction(data.ID)
					c.conv.Interaction(data)
					return nil
				}))
		case "ATMessageEventHandler":
			handlers = append(handlers, event.ATMessageEventHandler(
				func(_ *dto.WSPayload, data *dto.WSATMessageData) error {
					log.Printf("[qq] 频道@消息(本期不转换): id=%s", data.ID)
					return nil
				}))
		case "DirectMessageEventHandler":
			handlers = append(handlers, event.DirectMessageEventHandler(
				func(_ *dto.WSPayload, data *dto.WSDirectMessageData) error {
					log.Printf("[qq] 频道私信(本期不转换): id=%s", data.ID)
					return nil
				}))
		default:
			filelog.Warnf("[qq] 未知 text_intent 条目，忽略: %s", name)
		}
	}

	intent := event.RegisterHandlers(handlers...)
	if override, ok := c.cfg.IntentOverride(); ok {
		log.Printf("[qq] 使用 intents 硬覆盖: %d", override)
		return override
	}
	return intent
}

// runShard 单分片启动器：拿接入点后按 shard 配置连接，断线重连
func (c *Client) runShard(ctx context.Context, api openapi.OpenAPI, ts oauth2.TokenSource, intent dto.Intent) {
	wsAP, err := api.WS(ctx, nil, "")
	if err != nil {
		filelog.Errorf("[qq] 获取 WebSocket 接入点失败: %v", err)
		return
	}
	log.Printf("[qq] WebSocket 接入点: url=%s shards=%d", wsAP.URL, wsAP.Shards)
	log.Printf("[qq] WS 连接频控(/gateway/bot): 剩余 %d/%d 次, %d 秒后重置, 最大并发 %d",
		wsAP.SessionStartLimit.Remaining, wsAP.SessionStartLimit.Total,
		wsAP.SessionStartLimit.ResetAfter, wsAP.SessionStartLimit.MaxConcurrency)

	session := dto.Session{
		URL:         wsAP.URL,
		TokenSource: ts,
		Intent:      intent,
		LastSeq:     0,
		Shards: dto.ShardConfig{
			ShardID:    c.cfg.ShardID,
			ShardCount: c.cfg.ShardCount,
		},
	}

	for {
		select {
		case <-ctx.Done():
			return
		default:
		}
		c.connectOnce(&session)
		select {
		case <-ctx.Done():
			return
		case <-time.After(3 * time.Second):
		}
	}
}

// connectOnce 执行一次连接生命周期，结束后由 runShard 决定是否重连
func (c *Client) connectOnce(session *dto.Session) {
	defer func() {
		if r := recover(); r != nil {
			filelog.Errorf("[qq] 连接 panic: %v", r)
		}
	}()
	wsClient := websocket.ClientImpl.New(*session)
	if err := wsClient.Connect(); err != nil {
		filelog.Errorf("[qq] 连接失败: %v", err)
		return
	}
	var err error
	if session.ID != "" {
		err = wsClient.Resume()
	} else {
		err = wsClient.Identify()
	}
	if err != nil {
		filelog.Errorf("[qq] Identify/Resume 失败: %v", err)
		return
	}
	if err = wsClient.Listening(); err != nil {
		filelog.Warnf("[qq] Listening 退出: %v", err)
		cur := wsClient.Session()
		if manager.CanNotResume(err) {
			cur.ID = ""
			cur.LastSeq = 0
		}
		if manager.CanNotIdentify(err) {
			filelog.Errorf("[qq] 无法重新鉴权（可能被封禁/下架），停止该分片重连")
			cur.ID = ""
			cur.LastSeq = 0
		}
		*session = *cur
	}
}

// BotName 返回机器人用户名
func (c *Client) BotName() string {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.botName
}

// Ready 是否已完成 READY
func (c *Client) Ready() bool {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.ready
}
