// Command launcher 是 WorkBuddy2API 面板的一键启动器。
//
// 双击后：检查 7863 端口 -> 未运行则在后台拉起 wb2api.exe（无控制台窗口）
// -> 等端口就绪 -> 自动打开浏览器面板。已在运行时直接开面板，不重复起服务。
//
// 编译（隐藏控制台窗口）：
//
//	go build -trimpath -ldflags "-s -w -H=windowsgui" -o 打开面板.exe ./cmd/launcher
package main

import (
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"time"
	"unsafe"
)

const (
	host           = "127.0.0.1"
	port           = "7863"
	panelURL       = "http://127.0.0.1:7863/panel/"
	createNoWindow = 0x08000000 // CREATE_NO_WINDOW：让子进程不弹黑窗口
	mbIconError    = 0x10       // MB_ICONERROR
	appTitle       = "WorkBuddy2API 面板"
)

var (
	user32  = syscall.NewLazyDLL("user32.dll")
	procBox = user32.NewProc("MessageBoxW")
)

// messageBox 弹一个原生提示框（启动器是 GUI 子系统，没有控制台可打印）。
func messageBox(text string, flags uintptr) {
	t, err1 := syscall.UTF16PtrFromString(text)
	c, err2 := syscall.UTF16PtrFromString(appTitle)
	if err1 != nil || err2 != nil {
		return
	}
	procBox.Call(0, uintptr(unsafe.Pointer(t)), uintptr(unsafe.Pointer(c)), flags)
}

func portReady() bool {
	conn, err := net.DialTimeout("tcp", net.JoinHostPort(host, port), 500*time.Millisecond)
	if err != nil {
		return false
	}
	_ = conn.Close()
	return true
}

func main() {
	self, err := os.Executable()
	if err != nil {
		return
	}
	dir := filepath.Dir(self)

	if !portReady() {
		server := filepath.Join(dir, "wb2api.exe")
		if _, statErr := os.Stat(server); statErr != nil {
			messageBox("同目录下找不到 wb2api.exe。\n\n请把本启动器与 wb2api.exe 放在同一个文件夹中。", mbIconError)
			return
		}

		// 运行目录：缺失则补建（服务自身也会建，这里先备好）
		_ = os.MkdirAll(filepath.Join(dir, "auths"), 0o755)
		_ = os.MkdirAll(filepath.Join(dir, "data"), 0o755)

		cmd := exec.Command(server, "-config", "config.json")
		cmd.Dir = dir
		cmd.SysProcAttr = &syscall.SysProcAttr{
			HideWindow:    true,
			CreationFlags: createNoWindow,
		}

		// 服务日志落盘，方便出问题时排查
		if logFile, openErr := os.OpenFile(filepath.Join(dir, "data", "server.log"),
			os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644); openErr == nil {
			cmd.Stdout = logFile
			cmd.Stderr = logFile
		}

		if startErr := cmd.Start(); startErr != nil {
			messageBox("启动 wb2api.exe 失败：\n"+startErr.Error(), mbIconError)
			return
		}
		// 释放句柄，让服务独立于本启动器运行
		_ = cmd.Process.Release()

		// 最多等 30 秒
		for i := 0; i < 120; i++ {
			if portReady() {
				break
			}
			time.Sleep(250 * time.Millisecond)
		}
	}

	if !portReady() {
		messageBox("服务启动超时，7863 端口仍未就绪。\n\n请查看 data\\server.log 排查。", mbIconError)
		return
	}

	_ = exec.Command("rundll32", "url.dll,FileProtocolHandler", panelURL).Start()
}
