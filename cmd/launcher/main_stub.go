//go:build !windows

// 非 Windows 平台的启动器占位实现。
//
// 真正的启动器（cmd/launcher/main.go）依赖 Windows 专有 API（syscall.NewLazyDLL、
// SysProcAttr.HideWindow、rundll32 拉起浏览器），无法在 Linux/macOS 上编译。
// 本文件只保证 `go build ./...` 与 `go test ./...`（GitHub Actions 在 ubuntu 上跑）
// 能通过；本仓库对外发布的二进制只有服务端（cmd/server），不含启动器。
package main

import "fmt"

func main() {
	fmt.Println("WorkBuddy2API 启动器仅支持 Windows；服务端请直接运行 wb2api。")
}
