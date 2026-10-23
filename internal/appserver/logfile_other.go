//go:build !windows

// logfile_other.go 非 Windows 平台的日志落盘兜底（Windows 版见 logfile_windows.go）。
//
// 背景同上：GUI 子系统进程在计划任务/无终端下运行时 stderr 可能是无效句柄，
// log 输出会静默丢弃。非 Windows 没有 GetFileType 那套判断，直接判定 stderr 是否
// 有效：有效就按原样输出（由启动器的 `>> server.log 2>&1` 接管），无效才自己落盘。
package appserver

import (
	"log"
	"os"
	"path/filepath"
)

func ensureLogSink(cfgPath string) {
	dir := "."
	if cfgPath != "" {
		dir = filepath.Dir(cfgPath)
	}
	if dir == "" {
		dir = "."
	}

	// stderr 指向终端/管道/文件时按原样输出，不重复落盘。
	if fi, err := os.Stderr.Stat(); err == nil && (fi.Mode()&os.ModeCharDevice != 0 || fi.Mode()&os.ModeNamedPipe != 0) {
		return
	} else if err == nil && fi.Mode().IsRegular() {
		return // 已被重定向到文件
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
