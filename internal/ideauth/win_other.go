//go:build !windows

// win_other.go 非 Windows 平台的对应实现（Windows 版见 win.go）。
//
// 存在的理由与 crypto_common.go 相同：win.go 曾缺少 build tag，导致 linux/darwin
// 交叉编译失败。本项目主要面向 Windows，非 Windows 只需要「能编过 + 行为可预期」：
//   - OpenURL：走 xdg-open（Linux）/ open（macOS）；
//   - DPAPIUnprotect / QoderOSCryptKey：Windows 专有（DPAPI），非 Windows 直接报错；
//   - QoderDataDir：按 XDG 目录约定推导；
//   - runHidden：无需隐藏窗口，直接执行。
package ideauth

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
)

// OpenURL 用系统默认浏览器打开 URL（非 Windows：xdg-open / open）。
func OpenURL(u string) error {
	if u == "" {
		return errors.New("URL 为空")
	}
	var opener string
	switch runtime.GOOS {
	case "darwin":
		opener = "open"
	default:
		opener = "xdg-open"
	}
	cmd := exec.Command(opener, u)
	if err := cmd.Start(); err != nil {
		return err
	}
	go func() { _ = cmd.Wait() }()
	return nil
}

// DPAPIUnprotect 非 Windows 无 DPAPI，无法解密。
func DPAPIUnprotect(data []byte) ([]byte, error) {
	return nil, errors.New("DPAPI 解密仅 Windows 可用")
}

// QoderDataDir 按 XDG 目录约定推导 Qoder 数据目录（非 Windows）。
func QoderDataDir() string {
	if v := os.Getenv("QODER_DATA_DIR"); v != "" {
		return v
	}
	if v := os.Getenv("XDG_CONFIG_HOME"); v != "" {
		return filepath.Join(v, "com.qoder.app.stable")
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".config", "com.qoder.app.stable")
}

// QoderOSCryptKey 依赖 DPAPI，非 Windows 不可用。
func QoderOSCryptKey() ([]byte, error) {
	return nil, errors.New("OSCrypt key 读取依赖 Windows DPAPI")
}

// runHidden 执行命令并返回 stdout（非 Windows 无需隐藏窗口）。
func runHidden(name string, args ...string) ([]byte, error) {
	return exec.Command(name, args...).Output()
}

// hideWindow 非 Windows 无窗口隐藏概念，空实现。
func hideWindow(cmd *exec.Cmd) {}
