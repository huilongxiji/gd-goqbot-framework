package qq

import (
	"fmt"
	"os"
	"strings"

	"goqbot/internal/botgo/log"
	"goqbot/internal/filelog"
)

// botLogger 实现 botgo log.Logger：统一单行格式，
// debug=false 时终端不显示 Debug 级日志与逐拍心跳（与项目自身调试信息一致），文件始终完整写入。
type botLogger struct{ debug bool }

var _ log.Logger = (*botLogger)(nil)

// NewBotLogger 返回遵循 debug 开关的 botgo logger；debug=true 时终端显示全部日志（含 Debug 级与心跳）。
func NewBotLogger(debug bool) log.Logger { return &botLogger{debug: debug} }

// heartbeatPhrases 心跳相关日志短语（botgo websocket client 输出），debug=false 时终端过滤。
var heartbeatPhrases = []string{
	"write Heartbeat message",
	"receive HeartbeatAck message",
	"listened heartBeat",
}

// isHeartbeat 判断一条 botgo 日志是否为逐拍心跳
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
	if l.debug || (level != "Debug" && !isHeartbeat(msg)) {
		_, _ = os.Stderr.WriteString(line)
	}
	if filelog.Enabled() {
		filelog.WriteFile([]byte(line))
	}
}

// Debug、Info、Warn、Error 及对应的 f 格式化版本实现 botgo log.Logger 接口，统一经 emit 输出
func (l *botLogger) Debug(v ...interface{}) { l.emit("Debug", fmt.Sprint(v...)) }
func (l *botLogger) Info(v ...interface{})  { l.emit("Info", fmt.Sprint(v...)) }
func (l *botLogger) Warn(v ...interface{})  { l.emit("Warning", fmt.Sprint(v...)) }
func (l *botLogger) Error(v ...interface{}) { l.emit("Error", fmt.Sprint(v...)) }
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

// Sync 实现 botgo log.Logger 接口；输出无缓冲，无需刷新
func (l *botLogger) Sync() error { return nil }
