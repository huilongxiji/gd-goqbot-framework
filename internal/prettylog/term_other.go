//go:build !unix

package prettylog

// ioctlCols 非 Unix 平台无法通过 ioctl 获取终端宽度，返回 0 表示未知
func ioctlCols() int { return 0 }
