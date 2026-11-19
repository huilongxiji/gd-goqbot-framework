package plugins

import (
	"bytes"
	"context"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"
)

// 插件种类（Info.Kind）
const (
	KindExecutable = "executable" // 可执行文件插件（含 plugin.yml 指定 command 的开发调试方式）
	KindJS         = "js"         // JS / TS 插件，由内置启动器用 Node 运行
)

// minNodeMajor / minNodeMinor 运行 JS / TS 插件需要的最低 Node 版本：22.18 起 Node 默认能直接运行 TypeScript
const (
	minNodeMajor = 22
	minNodeMinor = 18
)

// runnerSource 内置的 JS / TS 插件启动器
//
//go:embed runner/goqbot-runner.mjs
var runnerSource []byte

// jsEntryNames 没有 package.json（或它没写 main）时依次尝试的入口文件
var jsEntryNames = []string{"index.ts", "index.mts", "index.cts", "index.js", "index.mjs", "index.cjs"}

// NodeInfo 找到的 Node
type NodeInfo struct {
	Path    string // Node 程序的绝对路径
	Version string // 如 v26.1.0
	Source  string // 来源说明：plugins.node / runtime / PATH
}

// FindNode 按顺序查找运行 JS / TS 插件的 Node：
// config.yml 的 plugins.node（相对路径以 goqbot 所在目录为基准）> goqbot 同级的 runtime/node > 系统 PATH 里的 node。
// 指定了 plugins.node 但不可用时直接报错，不再往下找，避免悄悄用上别的 Node。
func FindNode(exeDir, configured string) (NodeInfo, error) {
	exe := "node"
	if runtime.GOOS == "windows" {
		exe = "node.exe"
	}
	if configured = strings.TrimSpace(configured); configured != "" {
		path := configured
		if !filepath.IsAbs(path) {
			path = filepath.Join(exeDir, path)
		}
		if st, err := os.Stat(path); err != nil || st.IsDir() {
			return NodeInfo{}, fmt.Errorf("plugins.node 指定的 %s 不存在", path)
		}
		_ = ensureExecutable(path)
		return checkNode(path, "plugins.node")
	}
	bundled := filepath.Join(exeDir, "runtime", exe)
	if st, err := os.Stat(bundled); err == nil && !st.IsDir() {
		_ = ensureExecutable(bundled)
		return checkNode(bundled, "runtime")
	}
	path, err := exec.LookPath("node")
	if err != nil {
		return NodeInfo{}, errors.New("没有找到 Node（发布包自带的 runtime/ 文件夹不存在，系统里也没有安装 node）")
	}
	if abs, err := filepath.Abs(path); err == nil {
		path = abs
	}
	return checkNode(path, "PATH")
}

// checkNode 运行 node --version 检查版本是否够用
func checkNode(path, source string) (NodeInfo, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, path, "--version").Output()
	if err != nil {
		return NodeInfo{}, fmt.Errorf("Node 无法运行（%s）", path)
	}
	version := strings.TrimSpace(string(out))
	major, minor, ok := parseNodeVersion(version)
	if !ok {
		return NodeInfo{}, fmt.Errorf("无法识别 Node 版本 %q（%s）", version, path)
	}
	if major < minNodeMajor || (major == minNodeMajor && minor < minNodeMinor) {
		return NodeInfo{}, fmt.Errorf("Node 版本 %s 太低，需要 %d.%d 及以上（%s）", version, minNodeMajor, minNodeMinor, path)
	}
	return NodeInfo{Path: path, Version: version, Source: source}, nil
}

// parseNodeVersion 解析 v26.1.0 这样的版本号
func parseNodeVersion(v string) (major, minor int, ok bool) {
	parts := strings.Split(strings.TrimPrefix(v, "v"), ".")
	if len(parts) < 2 {
		return 0, 0, false
	}
	var err1, err2 error
	major, err1 = strconv.Atoi(parts[0])
	minor, err2 = strconv.Atoi(parts[1])
	return major, minor, err1 == nil && err2 == nil
}

// WriteRunner 把内置启动器写到 dir/goqbot-runner.mjs（内容相同时不重写），返回其路径
func WriteRunner(dir string) (string, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	path := filepath.Join(dir, "goqbot-runner.mjs")
	if old, err := os.ReadFile(path); err == nil && bytes.Equal(old, runnerSource) {
		return path, nil
	}
	if err := writeFileAtomic(path, runnerSource, 0o644); err != nil {
		return "", err
	}
	return path, nil
}

// findJSEntry 找 JS / TS 插件的入口：package.json 的 main（不能指向文件夹以外），否则按 jsEntryNames 依次找。
// 不是 JS / TS 插件时返回空字符串；package.json 写错时返回错误。
func findJSEntry(dir string) (string, error) {
	data, err := os.ReadFile(filepath.Join(dir, "package.json"))
	if err == nil {
		var pkg struct {
			Main string `json:"main"`
		}
		if err := json.Unmarshal(data, &pkg); err != nil {
			return "", fmt.Errorf("%w: package.json 格式错误: %v", ErrInvalidConfig, err)
		}
		if pkg.Main != "" {
			entry := filepath.Clean(filepath.Join(dir, filepath.FromSlash(pkg.Main)))
			rel, err := filepath.Rel(dir, entry)
			if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) || filepath.IsAbs(rel) {
				return "", fmt.Errorf("%w: package.json 的 main 不能指向插件文件夹以外", ErrInvalidConfig)
			}
			if st, err := os.Stat(entry); err != nil || st.IsDir() {
				return "", fmt.Errorf("%w: package.json 的 main 指向的 %s 不存在", ErrInvalidConfig, pkg.Main)
			}
			return entry, nil
		}
	}
	for _, name := range jsEntryNames {
		entry := filepath.Join(dir, name)
		if st, err := os.Stat(entry); err == nil && !st.IsDir() {
			return entry, nil
		}
	}
	if err == nil {
		// 有 package.json 但找不到入口
		return "", fmt.Errorf("%w: 有 package.json，但没有 main，也没有 index.ts / index.js 等入口文件", ErrInvalidConfig)
	}
	return "", nil
}
