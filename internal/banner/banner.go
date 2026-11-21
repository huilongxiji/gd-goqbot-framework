// Package banner 启动时在终端显示 goqbot 字符字，并把终端窗口的标题设为「程序名 版本」。
// 只在输出是终端时生效：不写进日志文件，输出被重定向（服务、管道）时什么都不做。
package banner

import (
	"os"
	"strings"
)

// art goqbot 字符字（只用普通英文符号，任何终端都能正常显示）
var art = []string{
	"                      _             _",
	"  __ _   ___    __ _ | |__    ___  | |_",
	" / _` | / _ \\  / _` || '_ \\  / _ \\ | __|",
	"| (_| || (_) || (_| || |_) || (_) || |_",
	" \\__, | \\___/  \\__, ||_.__/  \\___/  \\__|",
	" |___/            |_|",
}

// Show 显示字符字并设置终端窗口标题
func Show(name, version string) {
	fi, err := os.Stderr.Stat()
	if err != nil || fi.Mode()&os.ModeCharDevice == 0 {
		return
	}
	setTitle(name + " " + version)
	color := enableColor()
	var b strings.Builder
	b.WriteString("\n")
	for _, l := range art {
		if color {
			b.WriteString("\x1b[36m" + l + "\x1b[0m\n") // 青色
		} else {
			b.WriteString(l + "\n")
		}
	}
	b.WriteString("  QQ 官方机器人 → OneBot V11\n\n")
	_, _ = os.Stderr.WriteString(b.String())
}
