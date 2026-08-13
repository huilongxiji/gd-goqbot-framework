// Package config 定义适配器的 YAML 配置结构，并负责加载、填充默认值与校验。
// 配置分四段：qq（上游凭据与连接选项）、onebot（下游正/反向 WS 与行为开关）、
// idmap（id 映射的持久化）、log（本地文件日志）。
package config

import (
	"bytes"
	"fmt"
	"os"
	"strconv"

	"goqbot/internal/botgo/dto"
	"gopkg.in/yaml.v3"
)

// Config 适配器总配置
type Config struct {
	QQ     QQConfig     `yaml:"qq"`
	OneBot OneBotConfig `yaml:"onebot"`
	IDMap  IDMapConfig  `yaml:"idmap"`
	Log    LogConfig    `yaml:"log"`
}

// LogConfig 本地文件日志配置
type LogConfig struct {
	Enabled *bool  `yaml:"enabled"` // nil 或 true=开启；false=仅终端不落盘
	Path    string `yaml:"path"`    // 日志目录，默认 logs；相对路径=可执行文件同级
}

// FileEnabled 是否写入本地日志文件（默认 true）。
func (c *LogConfig) FileEnabled() bool {
	if c.Enabled == nil {
		return true
	}
	return *c.Enabled
}

// IDMapConfig idmap 持久化配置
type IDMapConfig struct {
	Disabled bool   `yaml:"disabled"` // true=纯内存不持久化（重启后 message_id/反查丢失）
	Path     string `yaml:"path"`     // 持久化文件，默认 idmap.db；相对路径=可执行文件同级目录
}

// QQConfig QQ 官方机器人凭据与连接选项
type QQConfig struct {
	AppID                 string   `yaml:"app_id"`                   // 机器人 AppID（开放平台分配）
	Uin                   int64    `yaml:"uin"`                      // 机器人 QQ 号，use_uin=true 时作 self_id
	UseUin                bool     `yaml:"use_uin"`                  // true=self_id 用 uin, false=用 app_id
	Token                 string   `yaml:"token"`                    // 机器人令牌，仅留存
	ClientSecret          string   `yaml:"client_secret"`            // = botgo AppSecret，用于鉴权换取 access_token
	Sandbox               bool     `yaml:"sandbox"`                  // true=连沙箱环境，false=正式环境
	Intents               *int64   `yaml:"intents"`                  // 可选：硬覆盖自动推导的 intent
	ShardCount            uint32   `yaml:"shard_count"`              // 分片总数，最小为 1
	ShardID               uint32   `yaml:"shard_id"`                 // 本进程分片号，从 0 开始
	TextIntent            []string `yaml:"text_intent"`              // 按 handler 名订阅事件，intent 自动推导
	AddAtGroup            bool     `yaml:"add_at_group"`             // 群消息上报时是否在首段补 at self
	SelfID                int64    `yaml:"self_id"`                  // 可选：覆盖上报用 self_id，0=按 use_uin/app_id 推导
	Disabled              bool     `yaml:"disabled"`                 // true=跳过 QQ 上游连接（仅测试 OneBot 侧时用）
	IPRejectAction        string   `yaml:"ip_reject_action"`         // IP 白名单被拒(11298)后策略：stop(默认)/retry/exit
	IPRejectRetryInterval int      `yaml:"ip_reject_retry_interval"` // 秒，仅 retry 生效；默认 60，最小 5
}

// AppSecret 返回 botgo 鉴权用的 AppSecret（优先 client_secret，回退 token）
func (c *QQConfig) AppSecret() string {
	if c.ClientSecret != "" {
		return c.ClientSecret
	}
	return c.Token
}

// OneBotConfig OneBot v11 下游连接配置
type OneBotConfig struct {
	AccessToken       string         `yaml:"access_token"`       // 下游鉴权口令，空=不校验
	HeartbeatInterval int            `yaml:"heartbeat_interval"` // 心跳间隔毫秒，默认 5000
	MessageFormat     string         `yaml:"message_format"`     // array(默认) 或 string
	Debug             bool           `yaml:"debug"`              // true 时打印收到的 action 日志
	DowntimeMessage   string         `yaml:"downtime_message"`   // 全下游掉线时回发 QQ 的兜底文本，空=关闭
	Servers           []ServerConfig `yaml:"servers"`            // 正向/反向 WS server 列表
}

// ServerConfig 单个 server 条目，ws 与 ws-reverse 二选一
type ServerConfig struct {
	WS        *WSServerConfig        `yaml:"ws"`
	WSReverse *WSReverseServerConfig `yaml:"ws-reverse"`
}

// WSServerConfig 正向 WS 服务端：本进程监听，等待下游接入
type WSServerConfig struct {
	Disabled bool   `yaml:"disabled"` // true=不启用该监听
	Address  string `yaml:"address"`  // 监听地址，如 0.0.0.0:8080
}

// WSReverseServerConfig 反向 WS 客户端：本进程主动连接下游
type WSReverseServerConfig struct {
	Disabled          bool   `yaml:"disabled"`           // true=不启用该连接
	Universal         string `yaml:"universal"`          // API+Event 合一端点；设置后优先于 api/event
	API               string `yaml:"api"`                // 仅收动作请求的端点
	Event             string `yaml:"event"`              // 仅推事件的端点
	ReconnectInterval int    `yaml:"reconnect-interval"` // 断线重连间隔毫秒，默认 3000
}

// EnsureComplete 校验配置文件是否缺少参数，缺失的键按模板补齐默认值后写回文件。
// 以内嵌模板 template（config.example.yml）为参照，仅补用户文件中缺失的键，
// 保留用户已填写的值与文件内注释（直接写回原文件，不生成备份）。
// 返回被补全的键路径列表（如 "onebot.heartbeat_interval"）。
func EnsureComplete(path string, template []byte) ([]string, error) {
	userData, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var userDoc yaml.Node
	if err := yaml.Unmarshal(userData, &userDoc); err != nil {
		return nil, fmt.Errorf("解析现有配置失败: %w", err)
	}
	var tmplDoc yaml.Node
	if err := yaml.Unmarshal(template, &tmplDoc); err != nil {
		return nil, fmt.Errorf("解析内置模板失败: %w", err)
	}
	userRoot := rootMapping(&userDoc)
	tmplRoot := rootMapping(&tmplDoc)
	// 用户文件为空或顶层不是映射时，不做自动补全，交由 Load/Validate 处理
	if userRoot == nil || tmplRoot == nil {
		return nil, nil
	}
	var added []string
	mergeMissing(userRoot, tmplRoot, "", &added)
	if len(added) == 0 {
		return nil, nil
	}

	var buf bytes.Buffer
	enc := yaml.NewEncoder(&buf)
	enc.SetIndent(2)
	if err := enc.Encode(&userDoc); err != nil {
		_ = enc.Close()
		return nil, fmt.Errorf("序列化补全后配置失败: %w", err)
	}
	if err := enc.Close(); err != nil {
		return nil, err
	}
	if err := os.WriteFile(path, buf.Bytes(), 0o644); err != nil {
		return nil, fmt.Errorf("写回配置失败: %w", err)
	}
	return added, nil
}

// rootMapping 取出文档节点下的根映射节点（非映射返回 nil）
func rootMapping(n *yaml.Node) *yaml.Node {
	if n == nil {
		return nil
	}
	if n.Kind == yaml.DocumentNode {
		if len(n.Content) == 0 {
			return nil
		}
		n = n.Content[0]
	}
	if n.Kind != yaml.MappingNode {
		return nil
	}
	return n
}

// mergeMissing 把 src 映射中 dst 缺失的键（连同默认值与注释）追加到 dst；
// 对两侧都存在且均为映射的键递归下钻，只增不改，绝不覆盖用户已有值。
//
// 对 onebot.servers 这类序列：用户缺失整个键时会按模板补上默认连接配置；
// 但只要用户已有该键，其值为序列（非映射）不会递归，用户对条目的任何增删都原样保留
// （例如只保留反向 WS 而删掉正向，不会被补回）。
func mergeMissing(dst, src *yaml.Node, prefix string, added *[]string) {
	if dst.Kind != yaml.MappingNode || src.Kind != yaml.MappingNode {
		return
	}
	dstVals := make(map[string]*yaml.Node, len(dst.Content)/2)
	for i := 0; i+1 < len(dst.Content); i += 2 {
		dstVals[dst.Content[i].Value] = dst.Content[i+1]
	}
	for i := 0; i+1 < len(src.Content); i += 2 {
		key := src.Content[i]
		val := src.Content[i+1]
		path := prefix + key.Value
		if dv, ok := dstVals[key.Value]; ok {
			if dv.Kind == yaml.MappingNode && val.Kind == yaml.MappingNode {
				mergeMissing(dv, val, path+".", added)
			}
			continue
		}
		dst.Content = append(dst.Content, key, val)
		*added = append(*added, path)
	}
}

// Load 从文件加载配置并填充默认值
func Load(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	cfg := &Config{}
	if err := yaml.Unmarshal(data, cfg); err != nil {
		return nil, err
	}
	cfg.applyDefaults()
	return cfg, nil
}

func (c *Config) applyDefaults() {
	// self_id：use_uin && uin!=0 时用 uin，否则解析 app_id
	if c.QQ.SelfID == 0 {
		if c.QQ.UseUin && c.QQ.Uin != 0 {
			c.QQ.SelfID = c.QQ.Uin
		} else if c.QQ.AppID != "" {
			if id, err := strconv.ParseInt(c.QQ.AppID, 10, 64); err == nil {
				c.QQ.SelfID = id
			}
		}
	}
	if c.QQ.ShardCount == 0 {
		c.QQ.ShardCount = 1
	}
	switch c.QQ.IPRejectAction {
	case "stop", "retry", "exit":
		// ok
	case "":
		c.QQ.IPRejectAction = "stop"
	default:
		// 非法值留给 Validate 报错
	}
	if c.QQ.IPRejectRetryInterval <= 0 {
		c.QQ.IPRejectRetryInterval = 60
	} else if c.QQ.IPRejectRetryInterval < 5 {
		c.QQ.IPRejectRetryInterval = 5
	}
	if c.OneBot.HeartbeatInterval == 0 {
		c.OneBot.HeartbeatInterval = 5000
	}
	if c.OneBot.MessageFormat == "" {
		c.OneBot.MessageFormat = "array"
	}
	if c.IDMap.Path == "" {
		c.IDMap.Path = "idmap.db"
	}
	if c.Log.Path == "" {
		c.Log.Path = "logs"
	}
	for i := range c.OneBot.Servers {
		if rev := c.OneBot.Servers[i].WSReverse; rev != nil && rev.ReconnectInterval == 0 {
			rev.ReconnectInterval = 3000
		}
	}
}

// IntentOverride 返回硬覆盖的 intent（配置了 intents 时），否则 ok=false 走自动推导
func (c *QQConfig) IntentOverride() (dto.Intent, bool) {
	if c.Intents != nil {
		return dto.Intent(*c.Intents), true
	}
	return 0, false
}

// Validate 校验配置
func (c *Config) Validate() error {
	switch c.QQ.IPRejectAction {
	case "stop", "retry", "exit":
		// ok
	default:
		return fmt.Errorf("qq.ip_reject_action 仅允许 stop / retry / exit，当前为 %q", c.QQ.IPRejectAction)
	}
	for i, s := range c.OneBot.Servers {
		hasWS := s.WS != nil && !s.WS.Disabled
		hasRev := s.WSReverse != nil && !s.WSReverse.Disabled
		if hasWS && hasRev {
			return fmt.Errorf("onebot.servers[%d]: ws 与 ws-reverse 不能同时启用", i)
		}
		if !hasWS && !hasRev {
			continue
		}
		if hasWS && s.WS.Address == "" {
			return fmt.Errorf("onebot.servers[%d].ws.address 不能为空", i)
		}
		if hasRev {
			rev := s.WSReverse
			if rev.Universal == "" && rev.API == "" && rev.Event == "" {
				return fmt.Errorf("onebot.servers[%d].ws-reverse 需配置 universal、api 或 event", i)
			}
		}
	}
	return nil
}
