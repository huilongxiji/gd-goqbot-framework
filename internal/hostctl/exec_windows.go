//go:build windows

package hostctl

import (
	"errors"
	"os"
	"os/exec"
)

// ExecSelf 用同样的启动参数重新运行 goqbot。Windows 不支持替换进程：先启动新进程，再退出当前进程。
// 调用前必须已关闭插件、下游连接与数据库（新进程要重新监听端口、打开数据库）。
func ExecSelf() error {
	if exePath == "" {
		return errors.New("无法确定当前程序的位置")
	}
	cmd := exec.Command(exePath, os.Args[1:]...)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	if err := cmd.Start(); err != nil {
		return err
	}
	os.Exit(0)
	return nil
}
