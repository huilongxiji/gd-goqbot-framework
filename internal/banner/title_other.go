//go:build !windows

package banner

import "os"

// setTitle 用终端控制序列设置窗口标题（macOS 终端、iTerm2、Linux 终端、Windows Terminal 都支持）
func setTitle(title string) {
	_, _ = os.Stderr.WriteString("\x1b]0;" + title + "\x07")
}

// enableColor 类 Unix 终端本来就支持颜色
func enableColor() bool { return true }
