//go:build windows

package plugins

import "os/exec"

// configureProcess Windows 下无需额外设置
func configureProcess(cmd *exec.Cmd) {}

// killProcess 结束插件进程。Windows 下只能结束直接子进程，插件自行派生的后台程序需插件自己清理。
func killProcess(cmd *exec.Cmd) {
	if cmd.Process != nil {
		_ = cmd.Process.Kill()
	}
}
