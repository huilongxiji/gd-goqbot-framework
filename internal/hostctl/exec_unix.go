//go:build !windows

package hostctl

import (
	"errors"
	"os"
	"syscall"
)

// ExecSelf 用同样的启动参数重新运行 goqbot。Linux / macOS 原地替换当前进程（PID 不变）。
// 调用前必须已关闭插件、下游连接与数据库。
func ExecSelf() error {
	if exePath == "" {
		return errors.New("无法确定当前程序的位置")
	}
	return syscall.Exec(exePath, os.Args, os.Environ())
}
