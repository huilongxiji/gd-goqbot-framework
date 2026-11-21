//go:build windows

package banner

import (
	"syscall"
	"unsafe"
)

var (
	kernel32            = syscall.NewLazyDLL("kernel32.dll")
	procSetConsoleTitle = kernel32.NewProc("SetConsoleTitleW")
	procGetConsoleMode  = kernel32.NewProc("GetConsoleMode")
	procSetConsoleMode  = kernel32.NewProc("SetConsoleMode")
)

// setTitle 用 Windows 的接口设置控制台窗口标题（旧的黑窗口和 Windows Terminal 都有效）
func setTitle(title string) {
	p, err := syscall.UTF16PtrFromString(title)
	if err != nil {
		return
	}
	_, _, _ = procSetConsoleTitle.Call(uintptr(unsafe.Pointer(p)))
}

// enableColor 打开控制台的彩色支持（ENABLE_VIRTUAL_TERMINAL_PROCESSING），
// 打开后日志里的颜色也能正常显示；很旧的 Windows 不支持时返回 false，字符字就不上色
func enableColor() bool {
	h, err := syscall.GetStdHandle(syscall.STD_ERROR_HANDLE)
	if err != nil {
		return false
	}
	var mode uint32
	if r, _, _ := procGetConsoleMode.Call(uintptr(h), uintptr(unsafe.Pointer(&mode))); r == 0 {
		return false
	}
	const enableVT = 0x0004
	if mode&enableVT != 0 {
		return true
	}
	r, _, _ := procSetConsoleMode.Call(uintptr(h), uintptr(mode|enableVT))
	return r != 0
}
