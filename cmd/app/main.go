//go:build windows

// Command app 是「单文件分发包」的唯一入口。
//
// 一个 exe 同时承担两种角色，靠 --server 参数区分：
//   - 双击启动（无参数）：启动器 —— 检查 7863 端口，未运行就以 --server 拉起自己，
//     等端口就绪后打开浏览器面板；已在运行则直接开面板。
//   - 内部以 --server 拉起：服务本体 —— 直接跑网关 + 面板（原来 wb2api.exe 的全部行为）。
//
// 为什么这样做：分发给别人时只需要给一个 exe，不用再解释「哪个是服务、哪个是启动器、
// 顺序不能错」。两个进程共用同一个 exe 文件，也不会出现版本不一致。
//
// 额外参数：
//
//	--stop    停止正在运行的服务（读 data/server.pid，兜底按端口找 PID）
//	--server  内部使用，前台运行服务本体
//
// 编译：
//
//	go build -trimpath -ldflags "-s -w -H=windowsgui" -o WorkBuddy2API.exe ./cmd/app
package main

import (
	"encoding/json"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"
	"unsafe"

	"github.com/linguo2625469/workbuddy2api-panel/internal/appdir"
	"github.com/linguo2625469/workbuddy2api-panel/internal/appserver"
)

const (
	defaultAddr    = "127.0.0.1:7863"
	createNoWindow = 0x08000000 // CREATE_NO_WINDOW：子进程不弹黑窗口
	mbIconError    = 0x10
	mbIconInfo     = 0x40
	appTitle       = "WorkBuddy2API"
)

var (
	user32  = syscall.NewLazyDLL("user32.dll")
	procBox = user32.NewProc("MessageBoxW")
)

func messageBox(text string, flags uintptr) {
	t, err1 := syscall.UTF16PtrFromString(text)
	c, err2 := syscall.UTF16PtrFromString(appTitle)
	if err1 != nil || err2 != nil {
		return
	}
	procBox.Call(0, uintptr(unsafe.Pointer(t)), uintptr(unsafe.Pointer(c)), flags)
}

// listenAddrFromConfig 从 config.json 读 listen（如 "127.0.0.1:7863" / ":7863"）。
//
// 启动器必须跟着配置走：别人把端口改成别的值时，若这里还写死 7863，
// 就会出现「服务起来了但启动器一直等 7863 直到超时」。
func listenAddrFromConfig(cfgPath string) string {
	raw, err := os.ReadFile(cfgPath)
	if err == nil {
		var c struct {
			Listen string `json:"listen"`
		}
		if json.Unmarshal(raw, &c) == nil {
			if v := strings.TrimSpace(c.Listen); v != "" {
				if strings.HasPrefix(v, ":") { // ":7863" → 本机回环
					v = "127.0.0.1" + v
				}
				return v
			}
		}
	}
	return defaultAddr
}

// dialAddr 把监听地址转成用于探测/打开浏览器的地址（0.0.0.0 换成回环）。
func dialAddr(listen string) string {
	host, port, err := net.SplitHostPort(listen)
	if err != nil {
		return defaultAddr
	}
	if host == "" || host == "0.0.0.0" || host == "::" {
		host = "127.0.0.1"
	}
	return net.JoinHostPort(host, port)
}

func portReady(addr string) bool {
	conn, err := net.DialTimeout("tcp", addr, 500*time.Millisecond)
	if err != nil {
		return false
	}
	_ = conn.Close()
	return true
}

// runHidden 跑一个不弹窗的辅助命令，返回标准输出。
func runHidden(name string, args ...string) string {
	cmd := exec.Command(name, args...)
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true, CreationFlags: createNoWindow}
	out, _ := cmd.Output()
	return string(out)
}

func main() {
	args := os.Args[1:]

	// 服务本体：去掉 --server 后交给 appserver.Run（它自己解析 -config）。
	for i, a := range args {
		if a == "--server" {
			os.Args = append([]string{os.Args[0]}, append(args[:i:i], args[i+1:]...)...)
			// 服务内部仍有相对路径写法（./auths、./data/state.json），
			// 统一把工作目录对到 exe 所在目录，双击启动也不会写错地方。
			_ = appdir.Chdir()
			appserver.Run()
			return
		}
	}

	for _, a := range args {
		if a == "--stop" {
			stopServer()
			return
		}
	}

	launch()
}

// launch 启动器流程：准备目录与配置 → 端口检查 → 拉起服务 → 开面板。
func launch() {
	dir := appdir.Dir()
	_ = appdir.Chdir()

	// 首次运行：补目录 + 生成「只手动、不自动」的默认配置。
	_ = os.MkdirAll(filepath.Join(dir, "auths"), 0o755)
	_ = os.MkdirAll(filepath.Join(dir, "data"), 0o755)
	cfgPath := filepath.Join(dir, "config.json")
	if _, err := os.Stat(cfgPath); err != nil {
		if werr := appserver.WriteDistDefault(cfgPath); werr != nil {
			messageBox("无法创建配置文件：\n"+werr.Error()+"\n\n请把本程序放到有写入权限的目录（例如桌面或 D 盘新建文件夹）后重试。", mbIconError)
			return
		}
	}

	addr := dialAddr(listenAddrFromConfig(cfgPath))
	if !portReady(addr) {
		self, err := os.Executable()
		if err != nil {
			messageBox("无法定位自身路径："+err.Error(), mbIconError)
			return
		}
		cmd := exec.Command(self, "--server", "-config", "config.json")
		cmd.Dir = dir
		cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true, CreationFlags: createNoWindow}
		if logFile, openErr := os.OpenFile(filepath.Join(dir, "data", "server.log"),
			os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644); openErr == nil {
			cmd.Stdout = logFile
			cmd.Stderr = logFile
		}
		if startErr := cmd.Start(); startErr != nil {
			messageBox("启动服务失败：\n"+startErr.Error(), mbIconError)
			return
		}
		// 记下 PID，--stop 时用它精确结束（避免误杀别的进程）。
		_ = os.WriteFile(filepath.Join(dir, "data", "server.pid"),
			[]byte(strconv.Itoa(cmd.Process.Pid)), 0o644)
		_ = cmd.Process.Release()

		for i := 0; i < 120; i++ { // 最多等 30 秒（冷启动要初始化供应商）
			if portReady(addr) {
				break
			}
			time.Sleep(250 * time.Millisecond)
		}
	}

	if !portReady(addr) {
		messageBox("服务启动超时，7863 端口仍未就绪。\n\n请查看同目录下 data\\server.log 排查。", mbIconError)
		return
	}

	_ = exec.Command("rundll32", "url.dll,FileProtocolHandler", "http://"+addr+"/panel/").Start()
}

// stopServer 停止服务：优先用 pid 文件，兜底按监听端口反查 PID。
func stopServer() {
	dir := appdir.Dir()
	pidFile := filepath.Join(dir, "data", "server.pid")

	killed := false
	if raw, err := os.ReadFile(pidFile); err == nil {
		if pid, perr := strconv.Atoi(strings.TrimSpace(string(raw))); perr == nil && pid > 0 {
			runHidden("taskkill", "/PID", strconv.Itoa(pid), "/T", "/F")
			killed = true
		}
	}
	if !killed {
		// 兜底：netstat 找到监听该端口的 PID（端口跟配置走，不写死）
		_, port, _ := net.SplitHostPort(dialAddr(listenAddrFromConfig(filepath.Join(dir, "config.json"))))
		out := runHidden("netstat", "-ano", "-p", "TCP")
		for _, line := range strings.Split(out, "\n") {
			if port == "" || !strings.Contains(line, ":"+port) || !strings.Contains(line, "LISTENING") {
				continue
			}
			fields := strings.Fields(line)
			if len(fields) == 0 {
				continue
			}
			last := fields[len(fields)-1]
			if pid, perr := strconv.Atoi(last); perr == nil && pid > 0 {
				runHidden("taskkill", "/PID", strconv.Itoa(pid), "/T", "/F")
				killed = true
			}
		}
	}
	_ = os.Remove(pidFile)
	if killed {
		messageBox("已停止服务。\n\n下次需要时再双击本程序即可。", mbIconInfo)
	} else {
		messageBox("没有找到正在运行的服务（可能已经停止）。", mbIconInfo)
	}
}

// 保留 fmt 引用：调试时可临时改成 fmt.Println，避免误删导入。
var _ = fmt.Sprintf
