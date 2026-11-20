// Command goqbot 是一个 OneBot v11 的 QQ 官方机器人适配器。
//
// 它把 QQ 官方机器人（基于内化的 botgo）上游事件转换为 OneBot v11 事件，
// 通过正向/反向 WebSocket 对接 NoneBot 等下游；并把下游的 OneBot 动作翻译为
// QQ 官方接口调用。main 负责装配各组件：加载配置、打开 idmap、创建事件总线/
// 转换器/动作调用器、载入插件、启动 OneBot 正反向 WS 与 QQ 上游连接，并处理优雅退出与插件请求的重启。
package main

import (
	"bufio"
	"context"
	_ "embed"
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"goqbot/internal/botgo"
	"goqbot/internal/config"
	"goqbot/internal/filelog"
	"goqbot/internal/hostctl"
	"goqbot/internal/idmap"
	"goqbot/internal/onebot"
	"goqbot/internal/plugins"
	"goqbot/internal/qq"
)

// 项目名称与版本（上报的 get_version_info 及启动日志均取此值）
// 版本格式：大版本.版本号.迭代次数；何时改哪一位见 CLAUDE.md
const (
	AppName    = "goqbot"
	AppVersion = "1.14.7"
)

// configTemplate 为缺省配置模板，缺失 config.yml 时按此生成
//
//go:embed config.example.yml
var configTemplate []byte

func main() {
	configPath := flag.String("config", "config.yml", "配置文件路径")
	showVersion := flag.Bool("version", false, "输出程序名与版本号后退出")
	flag.Parse()
	if *showVersion {
		fmt.Printf("%s %s\n", AppName, AppVersion)
		return
	}

	// 未显式指定 -config 时，默认加载可执行文件同级目录下的 config.yml（不受当前工作目录影响）
	explicit := false
	flag.Visit(func(f *flag.Flag) {
		if f.Name == "config" {
			explicit = true
		}
	})
	exeDir := "."
	if exe, err := os.Executable(); err == nil {
		exeDir = filepath.Dir(exe)
	}
	if !explicit {
		*configPath = filepath.Join(exeDir, "config.yml")
	}

	// 配置文件不存在则按模板生成，提示用户填写后重启
	if _, statErr := os.Stat(*configPath); os.IsNotExist(statErr) {
		if werr := os.WriteFile(*configPath, configTemplate, 0o644); werr != nil {
			filelog.Fatalf("未找到配置文件且自动创建失败: %v", werr)
		}
		fmt.Printf("未找到配置文件，已在同级目录生成模板：\n  %s\n请填写 app_id / client_secret 等字段后重新启动本程序。\n按回车键退出...", *configPath)
		_, _ = bufio.NewReader(os.Stdin).ReadString('\n')
		os.Exit(0)
	}

	// 启动时校验配置文件是否缺少参数，缺失项按模板补齐默认值并写回用户配置文件（保留已有值与注释）。
	// 若发生补全，暂停运行，提示用户编辑配置后手动重启。
	if added, cerr := config.EnsureComplete(*configPath, configTemplate); cerr != nil {
		fmt.Printf("配置自动补全失败（将按现有配置继续）：%v\n", cerr)
	} else if len(added) > 0 {
		fmt.Printf("检测到配置缺少 %d 个参数，已按默认值补充至配置文件：\n  %s\n新增参数：%v\n请检查并按需修改后重新启动本程序。\n按回车键退出...", len(added), *configPath, added)
		_, _ = bufio.NewReader(os.Stdin).ReadString('\n')
		os.Exit(0)
	}

	if abs, err := filepath.Abs(*configPath); err == nil {
		*configPath = abs
	}
	if run(*configPath, exeDir) {
		// 插件请求了重启（host.restart / 修改配置 / 更新程序）：资源已全部释放，用同样的参数重新运行
		if err := hostctl.ExecSelf(); err != nil {
			fmt.Fprintf(os.Stderr, "重启失败: %v\n", err)
			os.Exit(1)
		}
	}
}

// run 装配并运行全部组件，直到收到退出信号或重启请求；返回 true 表示需要重启。
// 所有资源（插件、下游连接、数据库、日志文件）在返回前释放。
func run(configPath, exeDir string) (restart bool) {
	cfg, err := config.Load(configPath)
	if err != nil {
		filelog.Fatalf("加载配置失败: %v", err)
	}
	if err := cfg.Validate(); err != nil {
		filelog.Fatalf("配置无效: %v", err)
	}

	logDir := cfg.Log.Path
	if !filepath.IsAbs(logDir) {
		logDir = filepath.Join(exeDir, logDir)
	}
	if err := filelog.Init(cfg.Log.FileEnabled(), logDir); err != nil {
		filelog.Fatalf("初始化文件日志失败: %v", err)
	}
	defer func() {
		if err := filelog.Close(); err != nil {
			filelog.Fatalf("关闭文件日志失败: %v", err)
		}
	}()
	log.SetFlags(0)
	log.SetOutput(filelog.StdLogWriter())

	selfID := cfg.QQ.SelfID
	appID := cfg.QQ.AppID

	// 共享组件
	var ids *idmap.Store
	if cfg.IDMap.Disabled {
		ids = idmap.New()
		log.Println("idmap: 纯内存模式（不持久化，重启后 message_id/反查丢失）")
	} else {
		p := cfg.IDMap.Path
		if !filepath.IsAbs(p) {
			// 相对路径=可执行文件同级目录，与 config 解析一致
			if exe, e := os.Executable(); e == nil {
				p = filepath.Join(filepath.Dir(exe), p)
			}
		}
		ids, err = idmap.Open(p)
		if err != nil {
			filelog.Fatalf("打开 idmap 失败: %v", err)
		}
		log.Printf("idmap: 持久化已启用 -> %s", p)
	}
	defer func() {
		if err := ids.Close(); err != nil {
			filelog.Fatalf("关闭 idmap 失败: %v", err)
		}
	}()
	bus := onebot.NewEventBus()
	caller := onebot.NewCaller(selfID, appID, ids, cfg.OneBot.Debug, AppName, AppVersion)
	conv := onebot.NewConverter(ids, bus, func() int64 { return selfID }, cfg.QQ.ResolvedMentionMode(), cfg.OneBot.MessageFormat, cfg.OneBot.DowntimeMessage, caller)
	conv.SetDeveloperOpenID(cfg.QQ.DeveloperOpenID)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// 重启请求：稍等片刻再执行，让发起请求的插件先收到回复
	restartCh := make(chan string, 1)
	requestRestart := func(reason string) {
		go func() {
			time.Sleep(300 * time.Millisecond)
			select {
			case restartCh <- reason:
			default:
			}
		}()
	}

	// 插件：扫描插件目录（相对路径=可执行文件同级）。插件是可执行文件，单个文件或文件夹均可，plugin.yml 可选
	pluginDir := cfg.Plugins.Path
	if !filepath.IsAbs(pluginDir) {
		pluginDir = filepath.Join(exeDir, pluginDir)
	}
	// JS / TS 插件：数据目录 plugin-data/ 在可执行文件同级；内置启动器写到 plugin-data/.goqbot/；Node 按 plugins.node > runtime/ > PATH 查找
	dataRoot := filepath.Join(exeDir, "plugin-data")
	discoverOpts := plugins.DiscoverOptions{DataRoot: dataRoot}
	nodeVersion := ""
	if runner, werr := plugins.WriteRunner(filepath.Join(dataRoot, ".goqbot")); werr != nil {
		discoverOpts.NodeErr = fmt.Sprintf("写出内置启动器失败: %v", werr)
	} else if node, nerr := plugins.FindNode(exeDir, cfg.Plugins.Node); nerr != nil {
		discoverOpts.NodeErr = nerr.Error()
	} else {
		discoverOpts.Node, discoverOpts.RunnerPath, nodeVersion = node.Path, runner, node.Version
	}
	pluginConfigs, skipped, perr := plugins.Discover(pluginDir, discoverOpts)
	for _, pc := range pluginConfigs {
		if pc.Kind == plugins.KindJS && pc.Enabled && discoverOpts.Node != "" {
			log.Printf("[plugins] JS / TS 插件使用 Node %s（%s）", nodeVersion, discoverOpts.Node)
			break
		}
	}
	if perr != nil {
		filelog.Errorf("[plugins] %v", perr)
	}
	for _, msg := range skipped {
		filelog.Warnf("[plugins] %s", msg)
	}
	for _, pc := range pluginConfigs {
		if err := pc.Validate(); err != nil {
			filelog.Errorf("[plugins] %s 配置无效，不会加载: %v", pc.Name, err)
		}
	}
	pluginManager := plugins.New(pluginConfigs, plugins.Options{
		HostVersion:         AppVersion,
		LogDir:              filepath.Join(logDir, "plugins"),
		SelfID:              func() int64 { return selfID },
		DownstreamSensitive: true, // 每条连接是否开放敏感能力由 allow-sensitive-plugins 和它的 access-token 决定
	})
	caller.SetPlugins(pluginManager)
	// 事件总线上的事件同时推给订阅了的插件（不计入下游送达数，不影响 downtime_message 的判断）；
	// 插件 events.emit 发出的事件经总线推给全部下游
	bus.OnPush(func(ev *onebot.Event) int {
		pluginManager.DispatchEvent(ev.Raw)
		return 0
	})
	pluginManager.SetEmitter(bus.PushRaw)

	// 启动 OneBot 下游连接。之后改了配置里的下游连接会立即按差异更新，不用重启
	conns := onebot.NewConnections(selfID, bus, caller)
	conns.Apply(cfg.OneBot)
	if !cfg.OneBot.HasEnabledServer() {
		log.Printf("[onebot] 没有开启的下游连接：可以在 WebUI 的「连接」页开启或添加，或修改 config.yml 的 onebot.servers")
	}

	// 替换 botgo logger：过滤逐拍心跳（debug=true 时心跳照常显示），其余日志原样
	botgo.SetLogger(qq.NewBotLogger(cfg.OneBot.Debug))

	// 启动 QQ 上游连接
	qqClient := qq.NewClient(&cfg.QQ, ids, conv, caller, cfg.OneBot.Debug)
	if err := qqClient.Start(ctx); err != nil {
		filelog.Fatalf("启动 QQ 客户端失败: %v", err)
	}
	// 插件可调用的运行管理能力（host.*）
	pluginManager.SetHost(hostctl.New(hostctl.Options{
		AppName:        AppName,
		Version:        AppVersion,
		ConfigPath:     configPath,
		Template:       configTemplate,
		LogDir:         logDir,
		LogEnabled:     cfg.Log.FileEnabled(),
		SelfID:         func() int64 { return selfID },
		QQReady:        qqClient.Ready,
		QQBotName:      qqClient.BotName,
		Connections:    conns.Status,
		Plugins:        pluginManager.List,
		RequestRestart: requestRestart,
		Running:        cfg,
		ApplyOneBot: func(ob config.OneBotConfig) any {
			return conns.Apply(ob)
		},
	}))

	// 上游启动成功后才拉起插件，避免 Fatalf 跳过 defer 留下子进程。
	// 插件在后台握手，单个插件失败不影响 OneBot 与 QQ 连接；状态可用 get_plugin_list 查询。
	defer pluginManager.Close()
	if n := pluginManager.EnabledCount(); n > 0 {
		pluginManager.Start(ctx)
		log.Printf("[plugins] 插件目录 %s: 共 %d 个插件，已开启 %d 个，正在后台启动", pluginDir, len(pluginConfigs), n)
	}

	log.Printf("%s %s 已启动，等待连接...", AppName, AppVersion)

	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)
	defer signal.Stop(sig)
	select {
	case <-sig:
		log.Println("收到退出信号，正在关闭...")
	case <-qqClient.Fatal():
		log.Println("IP 被拒且策略为 exit，正在关闭...")
	case reason := <-restartCh:
		log.Printf("正在重启 goqbot（%s）...", reason)
		restart = true
		// 先关插件，再断开下游与 QQ 连接、释放监听端口，新进程才能重新监听、打开数据库
		pluginManager.Close()
		conns.StopAll()
		qqClient.Stop()
	}
	cancel()
	return restart
}
