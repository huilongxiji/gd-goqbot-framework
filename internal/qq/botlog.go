package qq

import (
	"fmt"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"goqbot/internal/botgo/log"
)

// botLogger 实现 botgo log.Logger：逐字复刻 console 输出格式，
// 仅在 debug=false 时丢弃逐拍心跳日志，其余日志一律原样输出。
type botLogger struct{ debug bool }

var _ log.Logger = (*botLogger)(nil)

// NewBotLogger 返回过滤心跳的 botgo logger；debug=true 时逐拍心跳照常显示。
func NewBotLogger(debug bool) log.Logger { return &botLogger{debug: debug} }

// heartbeatPhrases 心跳相关日志短语（botgo websocket client 输出），debug=false 时过滤。
// 用精确短语避免与事件正文里偶含的字样冲突。
var heartbeatPhrases = []string{
	"write Heartbeat message",
	"receive HeartbeatAck message",
	"listened heartBeat",
}

// isHeartbeat 判断一条日志是否为逐拍心跳（含任一心跳短语）
func isHeartbeat(msg string) bool {
	for _, p := range heartbeatPhrases {
		if strings.Contains(msg, p) {
			return true
		}
	}
	return false
}

// emit 复刻 botgo log/console.go 的 output()：与原 consoleLogger 处于同一调用深度，
// runtime.Caller(3) 仍能正确指向真实调用点（file:line 不错位）。
func (l *botLogger) emit(level, msg string) {
	if !l.debug && isHeartbeat(msg) {
		return
	}
	pc, file, line, _ := runtime.Caller(3)
	file = filepath.Base(file)
	funcName := strings.TrimPrefix(filepath.Ext(runtime.FuncForPC(pc).Name()), ".")
	date := time.Now().Format("2006-01-02 15:04:05")
	fmt.Printf("[%s] %s %s:%d:%s %s\n", level, date, file, line, funcName, msg)
}

func (l *botLogger) Debug(v ...interface{}) { l.emit("Debug", fmt.Sprint(v...)) }
func (l *botLogger) Info(v ...interface{})  { l.emit("Info", fmt.Sprint(v...)) }
func (l *botLogger) Warn(v ...interface{})  { l.emit("Warning", fmt.Sprint(v...)) }
func (l *botLogger) Error(v ...interface{}) { l.emit("Error", fmt.Sprint(v...)) }

func (l *botLogger) Debugf(format string, v ...interface{}) { l.emit("Debug", fmt.Sprintf(format, v...)) }
func (l *botLogger) Infof(format string, v ...interface{})  { l.emit("Info", fmt.Sprintf(format, v...)) }
func (l *botLogger) Warnf(format string, v ...interface{})  { l.emit("Warning", fmt.Sprintf(format, v...)) }
func (l *botLogger) Errorf(format string, v ...interface{}) { l.emit("Error", fmt.Sprintf(format, v...)) }

func (l *botLogger) Sync() error { return nil }
