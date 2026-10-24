//go:build !windows

// extract_other.go 非 Windows 平台的占位：豆包客户端凭据提取仅支持 Windows
// （面板与签到主体在其它平台仍可编译运行，只是不能从客户端提取，可手动粘贴 Cookie）。
package doubao

import "errors"

// ClientUserDataDir 非 Windows 无客户端目录。
func ClientUserDataDir() string { return "" }

// ClientCookiesDB 非 Windows 无 Cookie 库。
func ClientCookiesDB() string { return "" }

// ExtractFromCookiesDB 非 Windows 不支持（chromium DPAPI 凭据绑定 Windows 用户）。
func ExtractFromCookiesDB(dbPath string) (map[string]string, error) {
	return nil, errors.New("豆包客户端凭据提取仅支持 Windows")
}
