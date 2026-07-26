// Package filelog 提供按天滚动的本地文件日志，以及统一的单行日志格式。
package filelog

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"
)

const timeLayout = "2006-01-02 15:04:05"

var (
	mu         sync.Mutex
	enabled    bool
	dir        string
	currentDay string
	file       *os.File

	ansiRE = regexp.MustCompile(`\x1b\[[0-9;]*m`)
)

// Init 初始化文件日志；logDir 应为绝对路径。enabled=false 时不创建文件。
func Init(en bool, logDir string) error {
	mu.Lock()
	defer mu.Unlock()
	enabled = en
	dir = logDir
	if !enabled {
		return nil
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	return openTodayLocked()
}

// Enabled 是否启用文件日志。
func Enabled() bool {
	mu.Lock()
	defer mu.Unlock()
	return enabled
}

// Close 关闭当前日志文件。
func Close() error {
	mu.Lock()
	defer mu.Unlock()
	if file == nil {
		return nil
	}
	err := file.Close()
	file = nil
	currentDay = ""
	return err
}

// colorOn 仅当 stderr 是终端时给级别标签上色；重定向到文件/管道/journald 时关闭，
// 避免终端外出现 ANSI 转义符（写文件另有 StripANSI 兜底去色）。
var colorOn = func() bool {
	fi, err := os.Stderr.Stat()
	return err == nil && fi.Mode()&os.ModeCharDevice != 0
}()

// ANSI 颜色码（仅用于级别标签）
const (
	cReset  = "\x1b[0m"
	cRed    = "\x1b[31m"
	cYellow = "\x1b[33m"
)

// colorLevel 按级别给标签上色（仅终端）：Error 红、Warning 黄，其余原样。
// 写入文件时该 ANSI 会被 WriteFile 的 StripANSI 统一去掉，文件保持纯文本。
func colorLevel(level string) string {
	if !colorOn {
		return level
	}
	switch level {
	case "Error":
		return cRed + level + cReset
	case "Warning":
		return cYellow + level + cReset
	}
	return level
}

// FormatLine 生成统一单行格式：[Level] 2006-01-02 15:04:05 message
// 级别标签在终端上色，文件写入时去色。
func FormatLine(level, msg string) string {
	return fmt.Sprintf("[%s] %s %s\n", colorLevel(level), time.Now().Format(timeLayout), msg)
}

// FormatBotgo 生成 botgo 来源的统一单行日志。
func FormatBotgo(level, msg string) string {
	return FormatLine(level, "[botgo] "+msg)
}

// FormatInfo 生成 Info 级别的统一单行日志（标准 log 用）。
func FormatInfo(msg string) string {
	return FormatLine("Info", strings.TrimSuffix(msg, "\n"))
}

// emitLevel 以指定级别输出一行到 stderr + 文件，格式与标准 log 一致（[Level] date time msg）。
// 供 app 代码分级日志使用，弥补标准 log.Printf 恒为 Info 的问题。
func emitLevel(level, msg string) {
	line := FormatLine(level, strings.TrimSuffix(msg, "\n"))
	_, _ = os.Stderr.WriteString(line)
	WriteFile([]byte(line))
}

// Infof 输出 Info 级别日志（等价于标准 log.Printf，但级别显式）。
func Infof(format string, v ...interface{}) { emitLevel("Info", fmt.Sprintf(format, v...)) }

// Warnf 输出 Warning 级别日志。
func Warnf(format string, v ...interface{}) { emitLevel("Warning", fmt.Sprintf(format, v...)) }

// Errorf 输出 Error 级别日志。
func Errorf(format string, v ...interface{}) { emitLevel("Error", fmt.Sprintf(format, v...)) }

// Fatalf 输出 Error 级别日志后退出进程（等价于 log.Fatalf，但级别显式为 Error）。
func Fatalf(format string, v ...interface{}) {
	emitLevel("Error", fmt.Sprintf(format, v...))
	os.Exit(1)
}

// StripANSI 去掉 ANSI 转义序列。
func StripANSI(s string) string {
	return ansiRE.ReplaceAllString(s, "")
}

// WriteFile 写入文件（去色）；enabled=false 时为 no-op。
func WriteFile(b []byte) {
	if len(b) == 0 {
		return
	}
	mu.Lock()
	defer mu.Unlock()
	if !enabled {
		return
	}
	if err := openTodayLocked(); err != nil {
		return
	}
	_, _ = file.Write([]byte(StripANSI(string(b))))
}

// WriteFileLine 写入一行到文件（自动补换行）。
func WriteFileLine(s string) {
	if s == "" {
		return
	}
	if !strings.HasSuffix(s, "\n") {
		s += "\n"
	}
	WriteFile([]byte(s))
}

func openTodayLocked() error {
	today := time.Now().Format("2006-01-02")
	if file != nil && currentDay == today {
		return nil
	}
	if file != nil {
		_ = file.Close()
		file = nil
	}
	path := filepath.Join(dir, "goqbot-"+today+".log")
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	file = f
	currentDay = today
	return nil
}

// stdLogWriter 为标准 log 包提供统一格式输出（stderr + 可选文件）。
type stdLogWriter struct {
	buf []byte
}

// StdLogWriter 返回供 log.SetOutput 使用的 Writer。
func StdLogWriter() io.Writer {
	return &stdLogWriter{}
}

func (w *stdLogWriter) Write(p []byte) (int, error) {
	w.buf = append(w.buf, p...)
	for {
		i := strings.IndexByte(string(w.buf), '\n')
		if i < 0 {
			break
		}
		line := string(w.buf[:i])
		w.buf = w.buf[i+1:]
		out := FormatInfo(line)
		_, _ = os.Stderr.WriteString(out)
		WriteFile([]byte(out))
	}
	return len(p), nil
}
