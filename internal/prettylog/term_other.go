//go:build !unix

package prettylog

func ioctlCols() int { return 0 }
