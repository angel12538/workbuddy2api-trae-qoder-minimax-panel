// crypto_common.go 与平台无关的加解密/解析小工具。
//
// 这些函数原先和 Windows 专有代码（DPAPI / ShellExecuteW）混在 win.go 里，
// 而 win.go 当时没有 build tag —— Go 只认文件名的 `_GOOS` 后缀，`win.go` 不算，
// 于是 syscall.NewLazyDLL 等 Windows 专有符号会在 linux/darwin 交叉编译时直接
// 报错（`undefined: syscall.NewLazyDLL`），5 平台 CI 矩阵全部编不出来。
//
// 拆分后：纯 Go 部分放这里（无 build tag，任何平台可编），Windows 专有部分留在
// win.go（`//go:build windows`），非 Windows 桩在 win_other.go。
package ideauth

import (
	"crypto/aes"
	"crypto/cipher"
	"fmt"
	"strings"
)

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
