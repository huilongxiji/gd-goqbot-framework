//go:build !windows

package plugins

import (
	"os/exec"
	"syscall"
)

// configureProcess 让插件运行在独立的进程组中，便于连同它派生的子进程一起结束
func configureProcess(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
}

// killProcess 结束插件所在的整个进程组（也会关闭子孙进程继承的管道）
func killProcess(cmd *exec.Cmd) {
	if cmd.Process != nil {
		_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		_ = cmd.Process.Kill()
	}
}
