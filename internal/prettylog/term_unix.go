//go:build unix

package prettylog

import (
	"os"

	"golang.org/x/sys/unix"
)

// ioctlCols 通过 ioctl 读取 stderr 所在终端的列数，失败时返回 0
func ioctlCols() int {
	ws, err := unix.IoctlGetWinsize(int(os.Stderr.Fd()), unix.TIOCGWINSZ)
	if err != nil || ws == nil || ws.Col == 0 {
		return 0
	}
	return int(ws.Col)
}
