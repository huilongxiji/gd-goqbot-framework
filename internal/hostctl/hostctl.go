// Package hostctl 实现插件可调用的 goqbot 运行管理能力（host.*）：运行状态、日志、
// 读写配置、重启与更新程序。插件宿主通过 plugins.HostServices 接口使用它。
package hostctl

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"regexp"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"gopkg.in/yaml.v3"

	"goqbot/internal/config"
	"goqbot/internal/plugins"
)

// exePath 启动时的可执行文件路径。必须在启动时记下：更新后 Linux 的 /proc/self/exe 会指向改名后的旧文件。
var exePath = func() string {
	p, err := os.Executable()
	if err != nil {
		return ""
	}
	if r, err := filepath.EvalSymlinks(p); err == nil {
		return r
	}
	return p
}()

// Options 运行管理所需的信息与回调，由 main 装配
type Options struct {
	AppName        string
	Version        string
	ConfigPath     string                  // config.yml 绝对路径
	Template       []byte                  // 内置配置模板（写入前按它补齐缺失的参数）
	LogDir         string                  // 日志目录
	LogEnabled     bool                    // 是否写本地日志文件
	SelfID         func() int64            // 机器人 self_id
	QQReady        func() bool             // QQ 连接是否就绪
	QQBotName      func() string           // 机器人名字
	Connections    func() []map[string]any // 下游连接状态
	Plugins        func() []plugins.Info   // 插件状态
	IDMap          func() map[string]any   // idmap 情况（是否持久化、文件大小、消息记录条数等）
	RequestRestart func(reason string)     // 请求重启 goqbot（异步执行，先让调用方收到回复）

	// Running 启动时加载的配置，用来判断修改后哪些项要重启才生效
	Running *config.Config
	// ApplyOneBot 保存配置后立即应用 onebot 下游连接（access_token、heartbeat_interval、servers），返回应用结果
	ApplyOneBot func(ob config.OneBotConfig) any
}

// Service 运行管理能力的实现
type Service struct {
	opts    Options
	start   time.Time
	mu      sync.Mutex // 配置写入、更新互斥
	updated bool       // 已完成一次更新，等待重启
	running config.Config
}

// New 创建运行管理服务
func New(opts Options) *Service {
	s := &Service{opts: opts, start: time.Now()}
	if opts.Running != nil {
		s.running = *opts.Running
	}
	return s
}

// restartRequired 返回与运行中配置不同、且要重启才能生效的配置项
func restartRequired(running, next *config.Config) []string {
	out := []string{}
	if !reflect.DeepEqual(running.QQ, next.QQ) {
		out = append(out, "qq")
	}
	if running.OneBot.MessageFormat != next.OneBot.MessageFormat {
		out = append(out, "onebot.message_format")
	}
	if running.OneBot.Debug != next.OneBot.Debug {
		out = append(out, "onebot.debug")
	}
	if running.OneBot.DowntimeMessage != next.OneBot.DowntimeMessage {
		out = append(out, "onebot.downtime_message")
	}
	if !reflect.DeepEqual(running.IDMap, next.IDMap) {
		out = append(out, "idmap")
	}
	if !reflect.DeepEqual(running.Log, next.Log) {
		out = append(out, "log")
	}
	if !reflect.DeepEqual(running.Plugins, next.Plugins) {
		out = append(out, "plugins")
	}
	return out
}

// Info host.info
func (s *Service) Info() map[string]any {
	info := map[string]any{
		"name":           s.opts.AppName,
		"version":        s.opts.Version,
		"pid":            os.Getpid(),
		"uptime_seconds": int64(time.Since(s.start).Seconds()),
		"platform":       runtime.GOOS + "-" + runtime.GOARCH,
	}
	if s.opts.SelfID != nil {
		info["self_id"] = s.opts.SelfID()
	}
	qq := map[string]any{"ready": false, "bot_name": ""}
	if s.opts.QQReady != nil {
		qq["ready"] = s.opts.QQReady()
	}
	if s.opts.QQBotName != nil {
		qq["bot_name"] = s.opts.QQBotName()
	}
	info["qq"] = qq
	conns := []map[string]any{}
	if s.opts.Connections != nil {
		conns = s.opts.Connections()
	}
	info["connections"] = conns
	total, ready := 0, 0
	if s.opts.Plugins != nil {
		for _, p := range s.opts.Plugins() {
			total++
			if p.State == plugins.StateReady {
				ready++
			}
		}
	}
	info["plugins"] = map[string]any{"total": total, "ready": ready}
	if s.opts.IDMap != nil {
		info["idmap"] = s.opts.IDMap()
	}
	return info
}

// Logs host.logs：读取 goqbot 当天（或指定日期）日志的最后若干行
// 日志文件是 goqbot-YYYY-MM-DD.log，一天一个
var logFileName = regexp.MustCompile(`^goqbot-(\d{4}-\d{2}-\d{2})\.log$`)

func (s *Service) Logs(lines int, date string) (map[string]any, error) {
	if !s.opts.LogEnabled {
		return nil, plugins.Detail(plugins.ErrCallFailed, "未开启文件日志（log.enabled=false）")
	}
	if date == "" {
		date = time.Now().Format("2006-01-02")
	}
	files := s.logFiles()
	out, err := plugins.TailFile(filepath.Join(s.opts.LogDir, "goqbot-"+date+".log"), lines)
	if err != nil {
		if !os.IsNotExist(err) {
			return nil, plugins.Detail(plugins.ErrCallFailed, "读取日志失败")
		}
		out = []string{} // 这一天没有日志文件
	}
	return map[string]any{"date": date, "lines": out, "files": files}, nil
}

// logFiles 列出日志目录里的全部日志文件，最新的在前
func (s *Service) logFiles() []map[string]any {
	entries, err := os.ReadDir(s.opts.LogDir)
	if err != nil {
		return []map[string]any{}
	}
	files := make([]map[string]any, 0, len(entries))
	for i := len(entries) - 1; i >= 0; i-- { // ReadDir 按文件名升序，倒过来就是最新的在前
		e := entries[i]
		m := logFileName.FindStringSubmatch(e.Name())
		if m == nil || !e.Type().IsRegular() {
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		files = append(files, map[string]any{"date": m[1], "size": info.Size()})
	}
	return files
}

// ConfigGet host.config.get
func (s *Service) ConfigGet() (map[string]any, error) {
	data, err := os.ReadFile(s.opts.ConfigPath)
	if err != nil {
		return nil, plugins.Detail(plugins.ErrCallFailed, "读取配置文件失败")
	}
	var parsed any
	if err := yaml.Unmarshal(data, &parsed); err != nil {
		return nil, plugins.Detail(plugins.ErrCallFailed, "配置文件无法解析：%v", err)
	}
	if parsed == nil {
		parsed = map[string]any{}
	}
	return map[string]any{"path": s.opts.ConfigPath, "content": string(data), "data": parsed}, nil
}

// ConfigSet host.config.set：整份替换
func (s *Service) ConfigSet(content string, restart bool) (map[string]any, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.writeConfig([]byte(content), restart)
}

// ConfigUpdate host.config.update：按路径修改个别配置项，保留注释和其他内容
func (s *Service) ConfigUpdate(values map[string]json.RawMessage, restart bool) (map[string]any, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	data, err := os.ReadFile(s.opts.ConfigPath)
	if err != nil {
		return nil, plugins.Detail(plugins.ErrCallFailed, "读取配置文件失败")
	}
	var doc yaml.Node
	if err := yaml.Unmarshal(data, &doc); err != nil {
		return nil, plugins.Detail(plugins.ErrCallFailed, "现有配置文件无法解析：%v", err)
	}
	if doc.Kind == 0 || len(doc.Content) == 0 {
		doc = yaml.Node{Kind: yaml.DocumentNode, Content: []*yaml.Node{{Kind: yaml.MappingNode, Tag: "!!map"}}}
	}
	root := doc.Content[0]
	paths := make([]string, 0, len(values))
	for p := range values {
		paths = append(paths, p)
	}
	sort.Strings(paths)
	for _, p := range paths {
		segs := strings.Split(p, ".")
		for _, seg := range segs {
			if seg == "" {
				return nil, plugins.Detail(plugins.ErrInvalidArguments, "路径 %q 无效", p)
			}
		}
		val, err := jsonToNode(values[p])
		if err != nil {
			return nil, plugins.Detail(plugins.ErrInvalidArguments, "路径 %s 的值无效", p)
		}
		if err := setPath(root, segs, val); err != nil {
			return nil, plugins.Detail(plugins.ErrInvalidArguments, "路径 %s：%v", p, err)
		}
	}
	var buf bytes.Buffer
	enc := yaml.NewEncoder(&buf)
	enc.SetIndent(2)
	if err := enc.Encode(&doc); err != nil {
		return nil, plugins.Detail(plugins.ErrCallFailed, "生成配置失败")
	}
	if err := enc.Close(); err != nil {
		return nil, plugins.Detail(plugins.ErrCallFailed, "生成配置失败")
	}
	return s.writeConfig(buf.Bytes(), restart)
}

// writeConfig 校验并写入配置：先写到同目录的临时文件，按模板补齐缺失参数，再按启动时的规则加载、校验；
// 通过后把原文件备份为 .bak，再整体替换。调用方须持有 s.mu。
func (s *Service) writeConfig(content []byte, restart bool) (map[string]any, error) {
	dir := filepath.Dir(s.opts.ConfigPath)
	tmp, err := os.CreateTemp(dir, ".config.yml.check-*")
	if err != nil {
		return nil, plugins.Detail(plugins.ErrCallFailed, "无法在配置目录创建临时文件")
	}
	tmpName := tmp.Name()
	keep := false
	defer func() {
		if !keep {
			_ = os.Remove(tmpName)
		}
	}()
	if _, err := tmp.Write(content); err != nil {
		_ = tmp.Close()
		return nil, plugins.Detail(plugins.ErrCallFailed, "写入临时文件失败")
	}
	if err := tmp.Close(); err != nil {
		return nil, plugins.Detail(plugins.ErrCallFailed, "写入临时文件失败")
	}
	added, err := config.EnsureComplete(tmpName, s.opts.Template)
	if err != nil {
		return nil, plugins.Detail(plugins.ErrInvalidArguments, "配置无法解析：%v", err)
	}
	cfg, err := config.Load(tmpName)
	if err != nil {
		return nil, plugins.Detail(plugins.ErrInvalidArguments, "配置无法解析：%v", err)
	}
	if err := cfg.Validate(); err != nil {
		return nil, plugins.Detail(plugins.ErrInvalidArguments, "配置校验未通过：%v", err)
	}
	if old, err := os.ReadFile(s.opts.ConfigPath); err == nil {
		if err := os.WriteFile(s.opts.ConfigPath+".bak", old, 0o600); err != nil {
			return nil, plugins.Detail(plugins.ErrCallFailed, "备份原配置失败")
		}
	}
	if st, err := os.Stat(s.opts.ConfigPath); err == nil {
		_ = os.Chmod(tmpName, st.Mode().Perm())
	} else {
		_ = os.Chmod(tmpName, 0o644)
	}
	if err := os.Rename(tmpName, s.opts.ConfigPath); err != nil {
		return nil, plugins.Detail(plugins.ErrCallFailed, "替换配置文件失败")
	}
	keep = true
	if added == nil {
		added = []string{}
	}
	result := map[string]any{"restarting": restart, "added": added}
	if restart && s.opts.RequestRestart != nil {
		s.opts.RequestRestart("配置已修改")
		return result, nil
	}
	// 不重启：下游连接立即按新配置更新，其他改动提示要重启
	if s.opts.ApplyOneBot != nil {
		result["applied"] = s.opts.ApplyOneBot(cfg.OneBot)
		s.running.OneBot.AccessToken = cfg.OneBot.AccessToken
		s.running.OneBot.HeartbeatInterval = cfg.OneBot.HeartbeatInterval
		s.running.OneBot.Servers = cfg.OneBot.Servers
	}
	result["restart_required"] = restartRequired(&s.running, cfg)
	return result, nil
}

// Restart host.restart
func (s *Service) Restart(reason string) error {
	if s.opts.RequestRestart == nil {
		return plugins.Detail(plugins.ErrCallFailed, "当前运行方式不支持重启")
	}
	if reason == "" {
		reason = "插件请求重启"
	}
	s.opts.RequestRestart(reason)
	return nil
}

// Update host.update：校验新程序后替换当前程序并重启
func (s *Service) Update(file, wantSHA string) (map[string]any, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.updated {
		return nil, plugins.Detail(plugins.ErrBusy, "已经更新过，正在等待重启")
	}
	if exePath == "" {
		return nil, plugins.Detail(plugins.ErrCallFailed, "无法确定当前程序的位置")
	}
	src, err := os.Open(file)
	if err != nil {
		return nil, plugins.Detail(plugins.ErrInvalidArguments, "无法打开新程序文件")
	}
	defer src.Close()

	dir, base := filepath.Dir(exePath), filepath.Base(exePath)
	stem := strings.TrimSuffix(base, ".exe")
	ext := base[len(stem):]
	newPath := filepath.Join(dir, stem+".new"+ext)
	oldPath := filepath.Join(dir, stem+".old"+ext)

	// 复制到程序所在目录（同一文件系统才能原子改名），边复制边算 sha256
	dst, err := os.OpenFile(newPath, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o755)
	if err != nil {
		return nil, plugins.Detail(plugins.ErrCallFailed, "无法在程序目录写入新文件")
	}
	h := sha256.New()
	_, copyErr := io.Copy(io.MultiWriter(dst, h), src)
	closeErr := dst.Close()
	cleanup := func() { _ = os.Remove(newPath) }
	if copyErr != nil || closeErr != nil {
		cleanup()
		return nil, plugins.Detail(plugins.ErrCallFailed, "复制新程序失败")
	}
	if hex.EncodeToString(h.Sum(nil)) != wantSHA {
		cleanup()
		return nil, plugins.Detail(plugins.ErrInvalidArguments, "sha256 不一致")
	}
	_ = os.Chmod(newPath, 0o755)

	// 运行新程序 -version，确认它是 goqbot 并能在本机运行
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	out, err := exec.CommandContext(ctx, newPath, "-version").Output()
	cancel()
	fields := strings.Fields(string(out))
	if err != nil || len(fields) < 2 || fields[0] != s.opts.AppName {
		cleanup()
		return nil, plugins.Detail(plugins.ErrInvalidArguments, "新文件不是 goqbot，或无法在本机运行")
	}
	newVersion := fields[1]

	// 替换：当前程序改名为 .old 备份，新程序改名为当前程序（失败时还原）
	_ = os.Remove(oldPath)
	if err := os.Rename(exePath, oldPath); err != nil {
		cleanup()
		return nil, plugins.Detail(plugins.ErrCallFailed, "备份当前程序失败")
	}
	if err := os.Rename(newPath, exePath); err != nil {
		_ = os.Rename(oldPath, exePath)
		cleanup()
		return nil, plugins.Detail(plugins.ErrCallFailed, "替换程序失败，已还原")
	}
	s.updated = true
	if s.opts.RequestRestart != nil {
		s.opts.RequestRestart("更新到 " + newVersion)
	}
	return map[string]any{"from": s.opts.Version, "to": newVersion}, nil
}

// ExecPath 启动时的可执行文件路径（重启时运行它）
func ExecPath() string {
	return exePath
}

// ---------------- 按路径修改 YAML ----------------

// setPath 在 YAML 节点树里按路径设置值：映射里缺少的键会新建，列表只能改已有的下标
func setPath(n *yaml.Node, segs []string, val *yaml.Node) error {
	seg, last := segs[0], len(segs) == 1
	if n.Kind == yaml.ScalarNode && (n.Tag == "!!null" || n.Value == "" || n.Value == "~" || n.Value == "null") {
		// 空值（如只写了 plugins:）当作空映射
		n.Kind, n.Tag, n.Value, n.Style = yaml.MappingNode, "!!map", "", 0
	}
	switch n.Kind {
	case yaml.MappingNode:
		for i := 0; i+1 < len(n.Content); i += 2 {
			if n.Content[i].Value == seg {
				child := n.Content[i+1]
				if last {
					replaceNode(child, val)
					moveLineComment(n.Content[i], child)
					return nil
				}
				return setPath(child, segs[1:], val)
			}
		}
		key := &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: seg}
		if last {
			flowEmpty(val)
			n.Content = append(n.Content, key, val)
			return nil
		}
		child := &yaml.Node{Kind: yaml.MappingNode, Tag: "!!map"}
		n.Content = append(n.Content, key, child)
		return setPath(child, segs[1:], val)
	case yaml.SequenceNode:
		idx, err := strconv.Atoi(seg)
		if err != nil || idx < 0 || idx >= len(n.Content) {
			return fmt.Errorf("列表下标 %s 超出范围（共 %d 项）", seg, len(n.Content))
		}
		if last {
			replaceNode(n.Content[idx], val)
			return nil
		}
		return setPath(n.Content[idx], segs[1:], val)
	default:
		return fmt.Errorf("%s 的上一级不是映射或列表", seg)
	}
}

// replaceNode 用 val 替换 dst 的内容，保留 dst 上的注释
func replaceNode(dst, val *yaml.Node) {
	if mergeScalarSeq(dst, val) {
		return
	}
	head, line, foot := dst.HeadComment, dst.LineComment, dst.FootComment
	flowEmpty(val)
	*dst = *val
	dst.HeadComment, dst.LineComment, dst.FootComment = head, line, foot
}

// commentedItem 匹配被注释掉的列表项：# - "名称"   # 说明
var commentedItem = regexp.MustCompile(`^#\s*-\s*"?([^"\s#]+)"?\s*(#.*)?$`)

// mergeScalarSeq 修改纯文字列表（如 qq.text_intent）时尽量保留注释：
// 原有的项连同行尾说明原样保留；新加的项如果在注释里出现过（# - "名称" # 说明），把说明拿过来并删掉那行注释；
// 去掉的项改写成一行注释，以后手动编辑时去掉 # 就能打开。不是纯文字列表时返回 false，按普通方式替换。
func mergeScalarSeq(dst, val *yaml.Node) bool {
	if dst.Kind != yaml.SequenceNode || val.Kind != yaml.SequenceNode || len(val.Content) == 0 {
		return false
	}
	for _, n := range append(append([]*yaml.Node{}, dst.Content...), val.Content...) {
		if n.Kind != yaml.ScalarNode {
			return false
		}
	}
	var foot []string
	old := make(map[string]*yaml.Node, len(dst.Content))
	var style yaml.Style
	for _, it := range dst.Content {
		if it.FootComment != "" {
			foot = append(foot, strings.Split(it.FootComment, "\n")...)
			it.FootComment = ""
		}
		old[it.Value] = it
		style = it.Style
	}
	keep := make(map[string]bool, len(val.Content))
	items := make([]*yaml.Node, 0, len(val.Content))
	for _, v := range val.Content {
		keep[v.Value] = true
		if o, ok := old[v.Value]; ok {
			items = append(items, o)
			continue
		}
		v.Style = style
		for i, l := range foot {
			if m := commentedItem.FindStringSubmatch(strings.TrimSpace(l)); m != nil && m[1] == v.Value {
				v.LineComment = m[2]
				foot = append(foot[:i], foot[i+1:]...)
				break
			}
		}
		items = append(items, v)
	}
	for _, it := range dst.Content {
		if !keep[it.Value] {
			l := fmt.Sprintf("# - %q", it.Value)
			if it.LineComment != "" {
				l += " " + it.LineComment
			}
			foot = append(foot, l)
		}
	}
	dst.Content = items
	if len(foot) > 0 {
		items[len(items)-1].FootComment = strings.Join(foot, "\n")
	}
	return true
}

// moveLineComment 键的行尾注释（servers: # 说明）放在哪里才能被 yaml.v3 正确写出：
// 值是 [] / {} 时放在值上（写成 servers: [] # 说明，放在键上会被写到下一个键后面）；
// 值是多行的列表 / 映射时放在键上（放在值上会被丢掉）。
func moveLineComment(key, val *yaml.Node) {
	collection := val.Kind == yaml.SequenceNode || val.Kind == yaml.MappingNode
	if !collection {
		return
	}
	if val.Style&yaml.FlowStyle != 0 && len(val.Content) == 0 {
		if key.LineComment != "" && val.LineComment == "" {
			val.LineComment, key.LineComment = key.LineComment, ""
		}
	} else if val.LineComment != "" && key.LineComment == "" {
		key.LineComment, val.LineComment = val.LineComment, ""
	}
}

// flowEmpty 空列表、空映射写成同一行的 [] / {}。
// 否则键上带行尾注释时（如 servers: # 说明），yaml.v3 会把 [] 写到下一行行首，生成无法解析的文件。
func flowEmpty(n *yaml.Node) {
	if (n.Kind == yaml.SequenceNode || n.Kind == yaml.MappingNode) && len(n.Content) == 0 {
		n.Style |= yaml.FlowStyle
	}
}

// jsonToNode 把 JSON 值转成 YAML 节点（整数保持整数，不变成浮点数）
func jsonToNode(raw json.RawMessage) (*yaml.Node, error) {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil {
		return nil, err
	}
	return valueNode(v), nil
}

func valueNode(v any) *yaml.Node {
	switch x := v.(type) {
	case nil:
		return &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!null", Value: "null"}
	case bool:
		return &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!bool", Value: strconv.FormatBool(x)}
	case json.Number:
		s := x.String()
		if strings.ContainsAny(s, ".eE") {
			return &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!float", Value: s}
		}
		return &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!int", Value: s}
	case string:
		return &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: x}
	case []any:
		n := &yaml.Node{Kind: yaml.SequenceNode, Tag: "!!seq"}
		for _, item := range x {
			n.Content = append(n.Content, valueNode(item))
		}
		return n
	case map[string]any:
		n := &yaml.Node{Kind: yaml.MappingNode, Tag: "!!map"}
		keys := make([]string, 0, len(x))
		for k := range x {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			n.Content = append(n.Content, &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: k}, valueNode(x[k]))
		}
		return n
	}
	return &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!null", Value: "null"}
}
