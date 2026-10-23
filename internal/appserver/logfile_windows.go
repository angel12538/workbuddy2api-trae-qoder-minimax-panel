//go:build windows

// logfile_windows.go 让 GUI 子系统进程在没有控制台/未重定向时也能留下日志。
//
// 背景：wb2api.exe 以 `-H windowsgui` 编译，计划任务直接拉起时没有控制台，
// os.Stderr 是无效句柄，log 包所有输出都会静默丢弃——排查问题无从下手。
// 这里用 GetFileType 判断 stderr 的真实去向：
//   - FILE_TYPE_CHAR（控制台）
//   - FILE_TYPE_PIPE（父进程管道）
//   - FILE_TYPE_DISK（已被重定向到文件）
//
// 三种情况都视为「日志有去处」，不动；只有 UNKNOWN/无效句柄才自己落盘，
// 避免与启动器的 `>> server.log 2>&1` 重复写入。
package appserver

import (
	"log"
	"os"
	"path/filepath"
	"syscall"
)

func ensureLogSink(cfgPath string) {
	dir := "."
	if cfgPath != "" {
		dir = filepath.Dir(cfgPath)
	}
	if dir == "" {
		dir = "."
	}

	if h, err := syscall.GetStdHandle(syscall.STD_ERROR_HANDLE); err == nil && h != 0 && h != syscall.InvalidHandle {
		if t, terr := syscall.GetFileType(h); terr == nil {
			switch t {
			case syscall.FILE_TYPE_CHAR, syscall.FILE_TYPE_PIPE, syscall.FILE_TYPE_DISK:
				return // 控制台 / 管道 / 已重定向到文件：按原样输出
			}
		}
	}

	dataDir := filepath.Join(dir, "data")
	if err := os.MkdirAll(dataDir, 0o755); err != nil {
		return
	}
	f, err := os.OpenFile(filepath.Join(dataDir, "server.log"), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return
	}
	log.SetOutput(f)
	log.SetFlags(log.LstdFlags)
}
