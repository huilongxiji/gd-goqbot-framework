package plugins

import (
	"bytes"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"
)

// knownOS / knownArch 插件文件名里可识别的平台后缀，与 goqbot 的发布文件一致
var (
	knownOS   = map[string]bool{"linux": true, "windows": true, "darwin": true}
	knownArch = map[string]bool{"amd64": true, "arm64": true}
)

// reservedNames 不能作为插件名的文件名（例如误把 goqbot 本体拷进插件目录）
var reservedNames = map[string]bool{"goqbot": true}

// Platform 本机平台后缀，如 linux-amd64
func Platform() string {
	return runtime.GOOS + "-" + runtime.GOARCH
}

// exeSuffix 本机可执行文件后缀
func exeSuffix() string {
	if runtime.GOOS == "windows" {
		return ".exe"
	}
	return ""
}

// parsedFile 插件目录里的一个文件名解析结果
type parsedFile struct {
	base     string // 去掉平台后缀和 .exe 后的名字
	platform string // 平台后缀，如 linux-amd64；没有后缀时为空
}

// parseExecName 解析 <名字>[-<系统>-<架构>][.exe]；名字不合规时 ok=false
func parseExecName(file string) (parsedFile, bool) {
	name := file
	if strings.HasSuffix(strings.ToLower(name), ".exe") {
		name = name[:len(name)-4]
	}
	pf := parsedFile{base: name}
	if parts := strings.Split(name, "-"); len(parts) >= 3 {
		osName, arch := parts[len(parts)-2], parts[len(parts)-1]
		if knownOS[osName] && knownArch[arch] {
			pf.base = strings.Join(parts[:len(parts)-2], "-")
			pf.platform = osName + "-" + arch
		}
	}
	if !namePattern.MatchString(pf.base) {
		return pf, false
	}
	return pf, true
}

// isNativeExecutable 文件是否为本机能运行的程序：按文件头识别 ELF / Mach-O / PE，或以 #! 开头的脚本
func isNativeExecutable(path string) bool {
	f, err := os.Open(path)
	if err != nil {
		return false
	}
	defer f.Close()
	head := make([]byte, 4)
	if _, err := io.ReadFull(f, head); err != nil {
		return false
	}
	switch runtime.GOOS {
	case "windows":
		return head[0] == 'M' && head[1] == 'Z' && strings.HasSuffix(strings.ToLower(path), ".exe")
	case "darwin":
		if bytes.HasPrefix(head, []byte("#!")) {
			return true
		}
		for _, magic := range [][]byte{
			{0xcf, 0xfa, 0xed, 0xfe}, {0xce, 0xfa, 0xed, 0xfe}, // Mach-O 64 / 32（小端）
			{0xfe, 0xed, 0xfa, 0xcf}, {0xfe, 0xed, 0xfa, 0xce}, // Mach-O（大端）
			{0xca, 0xfe, 0xba, 0xbe}, // 通用二进制
		} {
			if bytes.Equal(head, magic) {
				return true
			}
		}
		return false
	default:
		return bytes.Equal(head, []byte{0x7f, 'E', 'L', 'F'}) || bytes.HasPrefix(head, []byte("#!"))
	}
}

// findExecutable 在 dir 中为插件 name 找本机可执行文件：先找 <name>-<平台>[.exe]，再找 <name>[.exe]。
// 同时返回是否存在其他平台的版本，用于提示「没有适合本机的版本」。
func findExecutable(dir, name string) (path string, otherPlatforms bool) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return "", false
	}
	var generic string
	for _, e := range entries {
		if e.IsDir() || strings.HasPrefix(e.Name(), ".") {
			continue
		}
		pf, ok := parseExecName(e.Name())
		if !ok || pf.base != name {
			continue
		}
		full := filepath.Join(dir, e.Name())
		switch pf.platform {
		case Platform():
			if isNativeExecutable(full) {
				return full, false
			}
		case "":
			if generic == "" && isNativeExecutable(full) {
				generic = full
			}
		default:
			otherPlatforms = true
		}
	}
	return generic, otherPlatforms && generic == ""
}

// ensureExecutable Linux / macOS 上给插件程序补上执行权限（从网上下载的文件通常没有）
func ensureExecutable(path string) error {
	if runtime.GOOS == "windows" {
		return nil
	}
	st, err := os.Stat(path)
	if err != nil {
		return err
	}
	if st.Mode()&0o100 != 0 {
		return nil
	}
	return os.Chmod(path, st.Mode()|0o100)
}

// DiscoverOptions 扫描插件目录时需要的环境信息
type DiscoverOptions struct {
	Node       string // 运行 JS / TS 插件的 Node 绝对路径；空=不可用
	NodeErr    string // Node 不可用的原因（Node 为空时用于提示）
	RunnerPath string // 内置启动器的路径
	DataRoot   string // 插件数据目录的根（plugin-data/），每个插件的数据目录为 DataRoot/<插件名>
}

// Discover 扫描插件目录，按插件名排序返回。目录不存在时自动创建并返回空列表。
//
// 识别顺序（先满足哪条按哪条）：
//  1. 文件夹里的 plugin.yml 写了 command：按它启动（开发调试）；
//  2. 文件夹里有本机平台的可执行文件 <名字>-<平台>[.exe] / <名字>[.exe]：可执行文件插件；
//  3. 文件夹里有 package.json（按 main 找入口）或 index.ts / index.js 等：JS / TS 插件，用 Node 运行内置启动器；
//  4. 插件目录下的单个可执行文件 <名字>[-<平台>][.exe]：可执行文件插件，工作目录为自动创建的 plugins/<名字>/。
//
// 识别不出来的条目被跳过，原因通过 skipped 返回供启动日志提示；配置写错或文件冲突的插件
// 仍会返回（带错误状态），以便在 get_plugin_list 中看到。
func Discover(dir string, opts DiscoverOptions) (configs []Config, skipped []string, err error) {
	abs, err := filepath.Abs(dir)
	if err != nil {
		return nil, nil, err
	}
	if err := os.MkdirAll(abs, 0o755); err != nil {
		return nil, nil, fmt.Errorf("创建插件目录失败: %w", err)
	}
	entries, err := os.ReadDir(abs)
	if err != nil {
		return nil, nil, fmt.Errorf("读取插件目录失败: %w", err)
	}

	type candidate struct {
		file      string // 插件目录下的单个可执行文件（绝对路径）
		otherOnly bool   // 只有其他平台的单个文件
		dir       bool   // 有同名文件夹
	}
	cands := map[string]*candidate{}
	get := func(name string) *candidate {
		c := cands[name]
		if c == nil {
			c = &candidate{}
			cands[name] = c
		}
		return c
	}
	for _, e := range entries {
		name := e.Name()
		if strings.HasPrefix(name, ".") {
			continue
		}
		if e.IsDir() {
			get(name).dir = true
			continue
		}
		pf, ok := parseExecName(name)
		if !ok {
			continue
		}
		full := filepath.Join(abs, name)
		switch pf.platform {
		case Platform():
			if isNativeExecutable(full) {
				get(pf.base).file = full
			}
		case "":
			if isNativeExecutable(full) {
				if c := get(pf.base); c.file == "" {
					c.file = full
				}
			}
		default:
			get(pf.base).otherOnly = true
		}
	}

	names := make([]string, 0, len(cands))
	for name := range cands {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		c := cands[name]
		if reservedNames[name] {
			skipped = append(skipped, fmt.Sprintf("跳过 %s: 这是 goqbot 本体的文件名，不能作为插件", name))
			continue
		}
		workDir := filepath.Join(abs, name)
		cfg := Config{Name: name, Dir: workDir, Kind: KindExecutable, Enabled: true}
		if opts.DataRoot != "" {
			cfg.DataDir = filepath.Join(opts.DataRoot, name)
		}
		var m manifest
		hasManifest := false
		if c.dir {
			mf := filepath.Join(workDir, ManifestName)
			if _, statErr := os.Stat(mf); statErr == nil {
				hasManifest = true
				if m, err = loadManifest(mf); err != nil {
					cfg.loadErr = err
				}
			}
		}

		// 确定启动程序：plugin.yml 的 command（开发调试）> 文件夹内的可执行文件 > JS / TS 入口 > 插件目录下的单个文件
		var inner, jsEntry string
		var innerOther bool
		var jsErr error
		if c.dir && m.Command == "" {
			inner, innerOther = findExecutable(workDir, name)
			if inner == "" {
				jsEntry, jsErr = findJSEntry(workDir)
			}
		}
		switch {
		case cfg.loadErr != nil:
		case m.Command != "":
			if c.file != "" {
				cfg.loadErr = fmt.Errorf("%w: plugin.yml 指定了 command，但插件目录下还有同名的可执行文件 %s，请移走其中一个", ErrInvalidConfig, filepath.Base(c.file))
			}
			cfg.Command = m.Command
		case inner != "" && c.file != "":
			cfg.loadErr = fmt.Errorf("%w: 文件夹 %s/ 和插件目录下的 %s 都是这个插件的程序，请移走其中一个", ErrInvalidConfig, name, filepath.Base(c.file))
		case inner != "":
			cfg.Command = inner
		case jsErr != nil && c.file == "":
			cfg.Kind = KindJS
			cfg.loadErr = jsErr
		case jsEntry != "" && c.file != "":
			cfg.loadErr = fmt.Errorf("%w: 文件夹 %s/ 是 JS / TS 插件，插件目录下又有同名的可执行文件 %s，请移走其中一个", ErrInvalidConfig, name, filepath.Base(c.file))
		case jsEntry != "":
			if opts.Node == "" {
				skipped = append(skipped, fmt.Sprintf("跳过 %s: 这是 JS / TS 插件，无法运行：%s", name, opts.NodeErr))
				continue
			}
			cfg.Kind = KindJS
			cfg.Command = opts.Node
			cfg.Entry = jsEntry
		case c.file != "":
			cfg.Command = c.file
		default:
			if !c.dir && !c.otherOnly {
				continue
			}
			if c.otherOnly || innerOther {
				skipped = append(skipped, fmt.Sprintf("跳过 %s: 没有适合 %s 的版本", name, Platform()))
			} else if !hasManifest {
				skipped = append(skipped, fmt.Sprintf("跳过 %s: 文件夹里既没有可执行文件（应为 %s-%s%s 或 %s%s），也没有 package.json / index.ts / index.js",
					name, name, Platform(), exeSuffix(), name, exeSuffix()))
			} else {
				skipped = append(skipped, fmt.Sprintf("跳过 %s: 文件夹里没有可执行文件或 JS / TS 入口文件", name))
			}
			continue
		}

		if cfg.loadErr == nil {
			applyManifest(&cfg, m)
		}
		if cfg.Kind == KindJS && cfg.loadErr == nil {
			// JS / TS 插件：node <内置启动器> <入口文件>（plugin.yml 的 args 只配合 command 使用）
			if opts.RunnerPath == "" {
				cfg.loadErr = fmt.Errorf("%w: 内置启动器不可用", ErrInvalidConfig)
			}
			cfg.Args = []string{opts.RunnerPath, cfg.Entry}
		}
		if cfg.loadErr != nil {
			cfg.Enabled = false
		}
		if cfg.Enabled && cfg.loadErr == nil {
			if c.file != "" && !c.dir {
				// 单个文件的插件：自动创建同名文件夹作为工作目录
				if mkErr := os.MkdirAll(workDir, 0o755); mkErr != nil {
					cfg.loadErr = fmt.Errorf("%w: 创建工作目录 %s 失败: %v", ErrInvalidConfig, workDir, mkErr)
					cfg.Enabled = false
				}
			}
			if cfg.Kind == KindExecutable && cfg.Command != "" && filepath.IsAbs(cfg.Command) && m.Command == "" {
				if chErr := ensureExecutable(cfg.Command); chErr != nil {
					skipped = append(skipped, fmt.Sprintf("%s: 补执行权限失败: %v", name, chErr))
				}
			}
		}
		configs = append(configs, cfg)
	}
	return configs, skipped, nil
}

// applyManifest 把 plugin.yml 的设置合并进配置
func applyManifest(cfg *Config, m manifest) {
	if m.Enabled != nil {
		cfg.Enabled = *m.Enabled
	}
	cfg.Args = append([]string(nil), m.Args...)
	if m.Grants != nil {
		cfg.Grants = append([]string{}, (*m.Grants)...)
	}
	cfg.Env = m.Env
	cfg.StartupTimeoutMS = m.StartupTimeoutMS
	cfg.CallTimeoutMS = m.CallTimeoutMS
	cfg.LogStderr = m.LogStderr
}

// loadManifest 解析 plugin.yml；不认识的字段视为错误，避免把 enable 之类的笔误静默忽略。空文件视为全部默认。
func loadManifest(path string) (manifest, error) {
	var m manifest
	data, err := os.ReadFile(path)
	if err != nil {
		return m, fmt.Errorf("%w: 读取 %s 失败: %v", ErrInvalidConfig, ManifestName, err)
	}
	if len(bytes.TrimSpace(data)) == 0 {
		return m, nil
	}
	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true)
	if err := dec.Decode(&m); err != nil && err != io.EOF {
		return manifest{}, fmt.Errorf("%w: 解析 %s 失败: %v", ErrInvalidConfig, ManifestName, err)
	}
	return m, nil
}

// setManifestEnabled 修改插件工作目录里 plugin.yml 的 enabled（保留注释与其他内容；文件不存在时创建）
func setManifestEnabled(dir string, enabled bool) error {
	path := filepath.Join(dir, ManifestName)
	var doc yaml.Node
	data, err := os.ReadFile(path)
	switch {
	case err == nil && len(bytes.TrimSpace(data)) > 0:
		if err := yaml.Unmarshal(data, &doc); err != nil {
			return fmt.Errorf("解析 %s 失败: %v", ManifestName, err)
		}
	case err == nil || os.IsNotExist(err):
		doc = yaml.Node{Kind: yaml.DocumentNode, Content: []*yaml.Node{{Kind: yaml.MappingNode, Tag: "!!map"}}}
	default:
		return err
	}
	if len(doc.Content) == 0 || doc.Content[0].Kind != yaml.MappingNode {
		return fmt.Errorf("%s 的顶层不是映射", ManifestName)
	}
	root := doc.Content[0]
	value := "false"
	if enabled {
		value = "true"
	}
	found := false
	for i := 0; i+1 < len(root.Content); i += 2 {
		if root.Content[i].Value == "enabled" {
			v := root.Content[i+1]
			v.Kind, v.Tag, v.Value, v.Style = yaml.ScalarNode, "!!bool", value, 0
			found = true
			break
		}
	}
	if !found {
		root.Content = append([]*yaml.Node{
			{Kind: yaml.ScalarNode, Tag: "!!str", Value: "enabled", HeadComment: "插件开关：false=重启 goqbot 后不再载入"},
			{Kind: yaml.ScalarNode, Tag: "!!bool", Value: value},
		}, root.Content...)
	}
	var buf bytes.Buffer
	enc := yaml.NewEncoder(&buf)
	enc.SetIndent(2)
	if err := enc.Encode(&doc); err != nil {
		return err
	}
	if err := enc.Close(); err != nil {
		return err
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	return writeFileAtomic(path, buf.Bytes(), 0o644)
}

// writeFileAtomic 先写临时文件再改名替换，避免写出半份文件
func writeFileAtomic(path string, data []byte, perm os.FileMode) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+".tmp-*")
	if err != nil {
		return err
	}
	name := tmp.Name()
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		_ = os.Remove(name)
		return err
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(name)
		return err
	}
	_ = os.Chmod(name, perm)
	if err := os.Rename(name, path); err != nil {
		_ = os.Remove(name)
		return err
	}
	return nil
}
