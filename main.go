// Command goqbot 是一个 OneBot v11 的 QQ 官方机器人适配器。
//
// 它把 QQ 官方机器人（基于内化的 botgo）上游事件转换为 OneBot v11 事件，
// 通过正向/反向 WebSocket 对接 NoneBot 等下游；并把下游的 OneBot 动作翻译为
// QQ 官方接口调用。main 负责装配各组件：加载配置、打开 idmap、创建事件总线/
// 转换器/动作调用器、启动 OneBot 正反向 WS 与 QQ 上游连接，并处理优雅退出。
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
	"sync"
	"syscall"

	"goqbot/internal/botgo"
	"goqbot/internal/config"
	"goqbot/internal/filelog"
	"goqbot/internal/idmap"
	"goqbot/internal/onebot"
	"goqbot/internal/qq"
)

// 项目名称与版本，随时在此修改（上报给 NoneBot 的 get_version_info 及启动日志均取此值）
const (
	AppName    = "goqbot"
	AppVersion = "1.5.0"
)

// configTemplate 为缺省配置模板，缺失 config.yml 时按此生成
//
//go:embed config.example.yml
var configTemplate []byte

func main() {
	configPath := flag.String("config", "config.yml", "配置文件路径")
	flag.Parse()

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

	cfg, err := config.Load(*configPath)
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
	defer filelog.Close()
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
		defer ids.Close()
		log.Printf("idmap: 持久化已启用 -> %s", p)
	}
	bus := onebot.NewEventBus()
	caller := onebot.NewCaller(selfID, appID, ids, cfg.OneBot.Debug, AppName, AppVersion)
	conv := onebot.NewConverter(ids, bus, func() int64 { return selfID }, cfg.QQ.AddAtGroup, cfg.OneBot.MessageFormat, cfg.OneBot.DowntimeMessage, caller)

	var wg sync.WaitGroup
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// 启动 OneBot 下游连接
	for _, srv := range cfg.OneBot.Servers {
		if ws := srv.WS; ws != nil && !ws.Disabled {
			s := onebot.NewWSServer(ws, &cfg.OneBot, selfID, bus, caller)
			wg.Add(1)
			go func() {
				defer wg.Done()
				if err := s.Run(); err != nil {
					filelog.Errorf("[onebot] 正向 WS 服务退出: %v", err)
				}
			}()
		}
		if rev := srv.WSReverse; rev != nil && !rev.Disabled {
			c := onebot.NewWSClient(rev, &cfg.OneBot, selfID, bus, caller)
			c.StartHeartbeat()
			c.Run()
		}
	}

	// 替换 botgo logger：过滤逐拍心跳（debug=true 时心跳照常显示），其余日志原样
	botgo.SetLogger(qq.NewBotLogger(cfg.OneBot.Debug))

	// 启动 QQ 上游连接
	qqClient := qq.NewClient(&cfg.QQ, ids, conv, caller, cfg.OneBot.Debug)
	if err := qqClient.Start(ctx); err != nil {
		filelog.Fatalf("启动 QQ 客户端失败: %v", err)
	}

	log.Printf("%s %s 已启动，等待连接...", AppName, AppVersion)

	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)
	<-sig
	log.Println("收到退出信号，正在关闭...")
	cancel()
}
