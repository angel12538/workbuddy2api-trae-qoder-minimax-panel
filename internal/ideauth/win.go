package ideauth

import (
	"crypto/aes"
	"crypto/cipher"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
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
	crypt32            = syscall.NewLazyDLL("crypt32.dll")
	procUnprotectData  = crypt32.NewProc("CryptUnprotectData")
	kernel32           = syscall.NewLazyDLL("kernel32.dll")
	procLocalFree      = kernel32.NewProc("LocalFree")
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

// ---------- AES-256-GCM（Qoder 帧：iv12 | ct | tag16） ----------

// GCMDecryptIV12 解密 Qoder 风格数据体：前 12 字节 IV，末 16 字节 tag。
func GCMDecryptIV12(key, body []byte) ([]byte, error) {
	if len(body) < 28 {
		return nil, fmt.Errorf("body too short: %d", len(body))
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	gcm, err := cipher.NewGCMWithNonceSize(block, 12)
	if err != nil {
		return nil, err
	}
	iv := body[:12]
	ct := body[12 : len(body)-16]
	tag := body[len(body)-16:]
	sealed := append(append([]byte{}, ct...), tag...)
	return gcm.Open(nil, iv, sealed, nil)
}

// GCMEncryptIV12 加密并产出 Qoder 风格数据体（iv12 | ct | tag16）。
func GCMEncryptIV12(key, plain, iv []byte) ([]byte, error) {
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	gcm, err := cipher.NewGCMWithNonceSize(block, 12)
	if err != nil {
		return nil, err
	}
	if len(iv) != 12 {
		return nil, fmt.Errorf("iv must be 12 bytes, got %d", len(iv))
	}
	sealed := gcm.Seal(nil, iv, plain, nil)
	ct := sealed[:len(sealed)-16]
	tag := sealed[len(sealed)-16:]
	out := make([]byte, 0, 12+len(ct)+16)
	out = append(out, iv...)
	out = append(out, ct...)
	out = append(out, tag...)
	return out, nil
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

// ---------- 通用小工具 ----------

// extractJSONString 从 JSON 文本里粗略抓取某个 key 的字符串值（避免引入依赖）。
func extractJSONString(data []byte, key string) string {
	s := string(data)
	needle := `"` + key + `"`
	i := strings.Index(s, needle)
	if i < 0 {
		return ""
	}
	j := strings.IndexByte(s[i+len(needle):], ':')
	if j < 0 {
		return ""
	}
	rest := s[i+len(needle)+j+1:]
	rest = strings.TrimLeft(rest, " \t\r\n")
	if len(rest) == 0 || rest[0] != '"' {
		return ""
	}
	rest = rest[1:]
	end := strings.IndexByte(rest, '"')
	if end < 0 {
		return ""
	}
	return rest[:end]
}

func base64Decode(s string) ([]byte, error) {
	// 标准 base64，含可能省略的 padding
	return base64Std.DecodeString(strings.TrimSpace(s))
}

// runHidden 执行命令并返回 stdout（Windows 下隐藏窗口）。
func runHidden(name string, args ...string) ([]byte, error) {
	cmd := exec.Command(name, args...)
	// HideWindow 只影响窗口首次显示；控制台程序仍可能闪一下黑框。
	// 服务本体已是 GUI 子系统（-H windowsgui），子进程一律再叠 CREATE_NO_WINDOW，
	// 从根上避免「隔一段时间弹一个 CMD/PowerShell 窗口」。
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true, CreationFlags: 0x08000000}
	return cmd.Output()
}
