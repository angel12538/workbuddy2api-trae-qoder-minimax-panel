//go:build windows

// win.go Windows 专有能力：ShellExecuteW 打开 URL、DPAPI 解密、Qoder 数据目录与
// OSCrypt key、隐藏窗口执行子进程。
//
// build tag 说明：本文件原先没有 build tag，而 Go 的隐式平台约束**只认文件名的
// `_GOOS` 后缀**（`win.go` 不算），于是 syscall.NewLazyDLL 等 Windows 专有符号
// 会在 linux/darwin 交叉编译时报 `undefined: syscall.NewLazyDLL`，5 平台 CI
// 矩阵全部编不出来。现补 `//go:build windows`，平台无关部分迁到 crypto_common.go，
// 非 Windows 桩在 win_other.go。
package ideauth

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"unsafe"
)

// OpenURL 用系统默认浏览器打开一个 URL。
// 通过 ShellExecuteW 实现，避免调用可被策略拦截的命令行工具。
func OpenURL(u string) error {
	if u == "" {
		return errors.New("URL 为空")
	}
	shell32 := syscall.NewLazyDLL("shell32.dll")
	procShellExecuteW := shell32.NewProc("ShellExecuteW")
	verb, _ := syscall.UTF16PtrFromString("open")
	file, _ := syscall.UTF16PtrFromString(u)
	ret, _, _ := procShellExecuteW.Call(
		0,
		uintptr(unsafe.Pointer(verb)),
		uintptr(unsafe.Pointer(file)),
		0, 0, 1, // SW_SHOWNORMAL
	)
	if ret <= 32 {
		return fmt.Errorf("ShellExecuteW 打开失败, code=%d", ret)
	}
	return nil
}

// ---------- Windows DPAPI (CryptUnprotectData) ----------

var (
	crypt32           = syscall.NewLazyDLL("crypt32.dll")
	procUnprotectData = crypt32.NewProc("CryptUnprotectData")
	kernel32          = syscall.NewLazyDLL("kernel32.dll")
	procLocalFree     = kernel32.NewProc("LocalFree")
)

type dataBlob struct {
	cbData uint32
	pbData *byte
}

func newBlob(d []byte) *dataBlob {
	if len(d) == 0 {
		return &dataBlob{}
	}
	return &dataBlob{cbData: uint32(len(d)), pbData: &d[0]}
}

func (b *dataBlob) bytes() []byte {
	if b.pbData == nil || b.cbData == 0 {
		return nil
	}
	out := make([]byte, b.cbData)
	copy(out, unsafe.Slice(b.pbData, b.cbData))
	return out
}

// DPAPIUnprotect 用当前用户凭据调用 CryptUnprotectData 解密 data（无额外熵）。
func DPAPIUnprotect(data []byte) ([]byte, error) {
	in := newBlob(data)
	var out dataBlob
	r, _, err := procUnprotectData.Call(
		uintptr(unsafe.Pointer(in)),
		0, 0, 0, 0, 0,
		uintptr(unsafe.Pointer(&out)),
	)
	if r == 0 {
		return nil, fmt.Errorf("CryptUnprotectData failed: %v", err)
	}
	defer procLocalFree.Call(uintptr(unsafe.Pointer(out.pbData)))
	return out.bytes(), nil
}

// ---------- Qoder 目录与 OSCrypt key ----------

// QoderDataDir 返回 Qoder 用户数据目录（Windows: %APPDATA%\com.qoder.app.stable）。
func QoderDataDir() string {
	if v := os.Getenv("QODER_DATA_DIR"); v != "" {
		return v
	}
	appdata := os.Getenv("APPDATA")
	if appdata == "" {
		home, _ := os.UserHomeDir()
		appdata = filepath.Join(home, "AppData", "Roaming")
	}
	return filepath.Join(appdata, "com.qoder.app.stable")
}

// QoderOSCryptKey 读取 Local State 里的 encrypted_key，经 DPAPI 解出
// 32 字节 OSCrypt key（用于加解密 auth*.dat）。
func QoderOSCryptKey() ([]byte, error) {
	lsPath := filepath.Join(QoderDataDir(), "Local State")
	raw, err := os.ReadFile(lsPath)
	if err != nil {
		return nil, fmt.Errorf("读取 Local State 失败: %w", err)
	}
	b64 := extractJSONString(raw, "encrypted_key")
	if b64 == "" {
		return nil, fmt.Errorf("Local State 中未找到 os_crypt.encrypted_key")
	}
	blob, err := base64Decode(b64)
	if err != nil {
		return nil, err
	}
	if len(blob) < 5 || string(blob[:5]) != "DPAPI" {
		return nil, fmt.Errorf("encrypted_key 不是 DPAPI 前缀")
	}
	return DPAPIUnprotect(blob[5:])
}

// runHidden 执行命令并返回 stdout（Windows 下隐藏窗口）。
func runHidden(name string, args ...string) ([]byte, error) {
	cmd := exec.Command(name, args...)
	// HideWindow 只影响窗口首次显示；控制台程序仍可能闪一下黑框。
	// 服务本体已是 GUI 子系统（-H windowsgui），子进程一律再叠 CREATE_NO_WINDOW，
	// 从根上避免「隔一段时间弹一个 CMD/PowerShell 窗口」。
	hideWindow(cmd)
	return cmd.Output()
}

// hideWindow 让子进程不弹窗（仅 Windows 有意义；其他平台是空实现）。
//
// 单独抽出来是因为调用方往往要自己组装 cmd（设 Stdin/Dir 等），
// 直接在业务文件里写 `cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: ...}`
// 会把 Windows 专有的 syscall 字段带进无 build tag 的文件 —— 那类文件在
// linux/darwin 交叉编译时会报 `unknown field HideWindow in syscall.SysProcAttr`。
func hideWindow(cmd *exec.Cmd) {
	if cmd == nil {
		return
	}
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true, CreationFlags: 0x08000000}
}
