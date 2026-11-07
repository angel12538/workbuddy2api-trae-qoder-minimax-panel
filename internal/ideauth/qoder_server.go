package ideauth

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
)

// QoderServerMode uses the saved account pool on hosts without a Windows IDE.
// The current selection contains only a pool key; credentials stay in the pool
// so automatic token rotation is immediately visible to current-account calls.
func QoderServerMode() bool {
	return runtime.GOOS != "windows" || os.Getenv("QODER_SERVER_MODE") == "1"
}

func qoderCurrentPath() string { return filepath.Join(PoolDir(), "qoder_current.json") }

func readQoderServerSnapshot() (*QoderSnapshot, error) {
	qoderPoolMu.Lock()
	defer qoderPoolMu.Unlock()
	var cur struct {
		AccountKey string `json:"account_key"`
	}
	raw, err := os.ReadFile(qoderCurrentPath())
	if err != nil {
		if os.IsNotExist(err) {
			return nil, fmt.Errorf("尚未选择 Qoder 当前账号，请在账号池中点击切换")
		}
		return nil, err
	}
	if err := json.Unmarshal(raw, &cur); err != nil {
		return nil, fmt.Errorf("读取 Qoder 当前账号失败: %w", err)
	}
	f, err := loadQoderPool()
	if err != nil {
		return nil, err
	}
	for _, a := range f.Accounts {
		if a != nil && cur.AccountKey != "" && strings.EqualFold(a.Email, cur.AccountKey) {
			if a.Auth == nil || strings.TrimSpace(a.Auth.Token) == "" {
				return nil, fmt.Errorf("账号缺少登录凭证，请重新授权")
			}
			a.Auth.AccountKey = a.Email
			return a, nil
		}
	}
	return nil, fmt.Errorf("当前 Qoder 账号已不在号池中，请重新选择")
}

func switchQoderServerTo(key string) (*QoderSnapshot, error) {
	qoderPoolMu.Lock()
	defer qoderPoolMu.Unlock()
	f, err := loadQoderPool()
	if err != nil {
		return nil, err
	}
	for _, a := range f.Accounts {
		if a == nil || strings.TrimSpace(key) == "" || !strings.EqualFold(a.Email, strings.TrimSpace(key)) {
			continue
		}
		if a.Auth == nil || strings.TrimSpace(a.Auth.Token) == "" {
			return nil, fmt.Errorf("账号缺少登录凭证，请重新授权")
		}
		b, err := json.Marshal(struct {
			AccountKey string `json:"account_key"`
		}{a.Email})
		if err != nil {
			return nil, err
		}
		if err := atomicWrite(qoderCurrentPath(), b); err != nil {
			return nil, err
		}
		return a, nil
	}
	return nil, fmt.Errorf("号池中没有账号 %s", key)
}

// writeQoderServerAuth updates the original account without changing the
// current selection. A renewal started before a switch must not switch back.
// Merge raw JSON to retain future pool and account metadata.
func writeQoderServerAuth(a *QoderAuth) error {
	if a == nil || strings.TrimSpace(a.Token) == "" {
		return fmt.Errorf("账号凭证为空")
	}
	qoderPoolMu.Lock()
	defer qoderPoolMu.Unlock()
	raw, err := os.ReadFile(qoderPoolPath())
	if err != nil {
		return err
	}
	var root map[string]json.RawMessage
	if err := json.Unmarshal(raw, &root); err != nil {
		return err
	}
	var rows []map[string]json.RawMessage
	if err := json.Unmarshal(root["accounts"], &rows); err != nil {
		return err
	}
	for _, row := range rows {
		var key string
		_ = json.Unmarshal(row["email"], &key)
		if key == "" || !strings.EqualFold(key, QoderIdentityKey(a)) {
			continue
		}
		var old map[string]json.RawMessage
		if err := json.Unmarshal(row["auth"], &old); err != nil {
			return err
		}
		if old == nil {
			return fmt.Errorf("账号登录态为空")
		}
		var saved QoderAuth
		if err := json.Unmarshal(row["auth"], &saved); err != nil {
			return err
		}
		if saved.User.ID != a.User.ID || saved.Realm != a.Realm {
			return fmt.Errorf("账号身份不一致，未写入凭证")
		}
		// Only credentials change here; profile and unknown fields remain intact.
		for k, v := range map[string]string{"token": a.Token, "refreshToken": a.RefreshToken, "expiresAt": a.ExpiresAt, "refreshTokenExpiresAt": a.RefreshTokenExpiresAt} {
			old[k], _ = json.Marshal(v)
		}
		row["auth"], _ = json.Marshal(old)
		root["accounts"], _ = json.Marshal(rows)
		out, err := json.MarshalIndent(root, "", "  ")
		if err != nil {
			return err
		}
		return atomicWrite(qoderPoolPath(), out)
	}
	return fmt.Errorf("账号已从号池移除，未写入凭证")
}
