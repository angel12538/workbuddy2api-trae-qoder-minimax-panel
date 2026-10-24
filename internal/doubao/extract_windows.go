//go:build windows

// extract_windows.go 从豆包桌面客户端（Chromium 壳）提取登录 Cookie。
//
// 数据布局（2026-10 实测）：
//
//	%LOCALAPPDATA%\Doubao\User Data\Local State          → os_crypt.encrypted_key（DPAPI）
//	%LOCALAPPDATA%\Doubao\User Data\Default\Network\Cookies → SQLite，cookies 表
//	  encrypted_value = "v10" + nonce(12) + ct + tag(16)，AES-256-GCM
//	  解密后明文 = 32 字节校验块 + 真实 Cookie 值
//
// 注意：客户端运行时 Cookies 库被独占锁定，读之前必须先结束 Doubao.exe
// （由面板层的「提取当前账号」动作负责 kill → 拷贝 → 重启，见 ide_doubao.go）。
package doubao

import (
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/linguo2625469/workbuddy2api-panel/internal/ideauth"
	_ "modernc.org/sqlite"
)

// ClientUserDataDir 豆包客户端 Chromium 用户数据目录。
func ClientUserDataDir() string {
	if v := strings.TrimSpace(os.Getenv("DOUBAO_USER_DATA_DIR")); v != "" {
		return v
	}
	local := os.Getenv("LOCALAPPDATA")
	return filepath.Join(local, "Doubao", "User Data")
}

// ClientCookiesDB Cookie 库路径。
func ClientCookiesDB() string {
	return filepath.Join(ClientUserDataDir(), "Default", "Network", "Cookies")
}

// ExtractFromCookiesDB 从**已拷贝**的 Cookie 库文件解出全部 doubao 相关 Cookie。
// dbPath 可以是运行中的原件，也可以是面板层先拷贝出来的副本。
func ExtractFromCookiesDB(dbPath string) (map[string]string, error) {
	key, err := osCryptKey()
	if err != nil {
		return nil, err
	}
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		return nil, fmt.Errorf("打开 Cookie 库失败: %w", err)
	}
	defer db.Close()
	rows, err := db.Query(`SELECT host_key, name, encrypted_value, value FROM cookies`)
	if err != nil {
		return nil, fmt.Errorf("读取 cookies 表失败: %w", err)
	}
	defer rows.Close()
	out := map[string]string{}
	for rows.Next() {
		var host, name string
		var enc []byte
		var plain string
		if err := rows.Scan(&host, &name, &enc, &plain); err != nil {
			continue
		}
		if !strings.Contains(strings.ToLower(host), "doubao") {
			continue
		}
		if len(enc) == 0 {
			if plain != "" {
				out[name] = plain
			}
			continue
		}
		v, err := decryptCookieValue(key, enc)
		if err != nil || v == "" {
			continue
		}
		out[name] = v
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("没有解出任何豆包 Cookie（客户端可能未登录）")
	}
	return out, nil
}

// osCryptKey Local State → DPAPI → AES-256-GCM 密钥。
func osCryptKey() ([]byte, error) {
	raw, err := os.ReadFile(filepath.Join(ClientUserDataDir(), "Local State"))
	if err != nil {
		return nil, fmt.Errorf("读取 Local State 失败: %w", err)
	}
	var ls struct {
		OSCrypt struct {
			EncryptedKey string `json:"encrypted_key"`
		} `json:"os_crypt"`
	}
	if json.Unmarshal(raw, &ls) != nil || ls.OSCrypt.EncryptedKey == "" {
		return nil, fmt.Errorf("Local State 里没有 os_crypt.encrypted_key")
	}
	blob, err := base64.StdEncoding.DecodeString(ls.OSCrypt.EncryptedKey)
	if err != nil {
		return nil, fmt.Errorf("encrypted_key base64 解码失败: %w", err)
	}
	if len(blob) < 5 || string(blob[:5]) != "DPAPI" {
		return nil, fmt.Errorf("encrypted_key 不是 DPAPI 前缀")
	}
	key, err := ideauth.DPAPIUnprotect(blob[5:])
	if err != nil {
		return nil, fmt.Errorf("DPAPI 解密失败: %w", err)
	}
	return key, nil
}

// decryptCookieValue 解单个 Cookie 值：v10 前缀 + nonce(12) + ct||tag(16)。
// 明文头部的 32 字节是校验块（与 macOS 方案同构），跳过即真实值。
func decryptCookieValue(key, enc []byte) (string, error) {
	if len(enc) < 3+12+16 {
		return "", fmt.Errorf("cookie 数据过短")
	}
	prefix := string(enc[:3])
	if prefix != "v10" {
		return "", fmt.Errorf("不支持的加密前缀 %q（App-Bound 加密暂不支持）", prefix)
	}
	body := enc[3:] // iv12 | ct | tag16 —— 与 Qoder 帧同布局
	plain, err := ideauth.GCMDecryptIV12(key, body)
	if err != nil {
		return "", err
	}
	if len(plain) > 32 {
		// 32 字节校验块之后才是真实值；若整体都是可打印 ASCII 则原样返回（兼容）。
		printable := true
		for _, b := range plain {
			if b < 0x20 || b > 0x7e {
				printable = false
				break
			}
		}
		if !printable {
			plain = plain[32:]
		}
	}
	return string(plain), nil
}
