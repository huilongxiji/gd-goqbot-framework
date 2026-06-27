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

// FormatLine 生成统一单行格式：[Level] 2006-01-02 15:04:05 message
func FormatLine(level, msg string) string {
	return fmt.Sprintf("[%s] %s %s\n", level, time.Now().Format(timeLayout), msg)
}

// FormatBotgo 生成 botgo 来源的统一单行日志。
func FormatBotgo(level, msg string) string {
	return FormatLine(level, "[botgo] "+msg)
}

// FormatInfo 生成 Info 级别的统一单行日志（标准 log 用）。
func FormatInfo(msg string) string {
	return FormatLine("Info", strings.TrimSuffix(msg, "\n"))
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
