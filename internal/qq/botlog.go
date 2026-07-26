package qq

import (
	"fmt"
	"os"
	"strings"

	"goqbot/internal/botgo/log"
	"goqbot/internal/filelog"
)

// botLogger 实现 botgo log.Logger：统一单行格式，
// debug=false 时终端丢弃逐拍心跳，文件始终写入（含心跳）。
type botLogger struct{ debug bool }

var _ log.Logger = (*botLogger)(nil)

// NewBotLogger 返回过滤心跳的 botgo logger；debug=true 时终端逐拍心跳照常显示。
func NewBotLogger(debug bool) log.Logger { return &botLogger{debug: debug} }

// heartbeatPhrases 心跳相关日志短语（botgo websocket client 输出），debug=false 时终端过滤。
var heartbeatPhrases = []string{
	"write Heartbeat message",
	"receive HeartbeatAck message",
	"listened heartBeat",
}

func isHeartbeat(msg string) bool {
	for _, p := range heartbeatPhrases {
		if strings.Contains(msg, p) {
			return true
		}
	}
	return false
}

// emit 输出统一格式：[Level] 2006-01-02 15:04:05 [botgo] msg
// 统一写到 stderr（与 app 标准 log、prettylog 保持同一输出流，避免 stdout/stderr 混用）。
func (l *botLogger) emit(level, msg string) {
	line := filelog.FormatBotgo(level, msg)
	if l.debug || !isHeartbeat(msg) {
		_, _ = os.Stderr.WriteString(line)
	}
	if filelog.Enabled() {
		filelog.WriteFile([]byte(line))
	}
}

func (l *botLogger) Debug(v ...interface{})  { l.emit("Debug", fmt.Sprint(v...)) }
func (l *botLogger) Info(v ...interface{})   { l.emit("Info", fmt.Sprint(v...)) }
func (l *botLogger) Warn(v ...interface{})   { l.emit("Warning", fmt.Sprint(v...)) }
func (l *botLogger) Error(v ...interface{})  { l.emit("Error", fmt.Sprint(v...)) }
func (l *botLogger) Debugf(format string, v ...interface{}) {
	l.emit("Debug", fmt.Sprintf(format, v...))
}
func (l *botLogger) Infof(format string, v ...interface{}) {
	l.emit("Info", fmt.Sprintf(format, v...))
}
func (l *botLogger) Warnf(format string, v ...interface{}) {
	l.emit("Warning", fmt.Sprintf(format, v...))
}
func (l *botLogger) Errorf(format string, v ...interface{}) {
	l.emit("Error", fmt.Sprintf(format, v...))
}

func (l *botLogger) Sync() error { return nil }
