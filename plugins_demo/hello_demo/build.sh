#!/usr/bin/env bash
# 交叉编译 hello_demo 示例插件，输出到本目录下的 dist/：hello_demo-<系统>-<架构>[.exe]
# 用法：./build.sh            编译全部 6 个平台
set -euo pipefail
cd "$(dirname "$0")"
mkdir -p dist
for t in darwin/amd64 darwin/arm64 linux/amd64 linux/arm64 windows/amd64 windows/arm64; do
	os=${t%/*}; arch=${t#*/}; ext=""
	[ "$os" = windows ] && ext=".exe"
	out="dist/hello_demo-$os-$arch$ext"
	if CGO_ENABLED=0 GOOS=$os GOARCH=$arch go build -trimpath -ldflags "-s -w" -o "$out" .; then
		echo "OK   $out"
	else
		echo "FAIL $t" >&2
	fi
done
