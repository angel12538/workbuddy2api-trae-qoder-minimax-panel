// Package doubao 豆包（Doubao 桌面客户端）每日签到池。
//
// 豆包的「天天领额度」签到活动页是 https://www.doubao.com/pc/agent-explore，
// 仅限客户端 UA 参与（前端检测 navigator.userAgent 含 SamanthaDoubao/），
// 签到动作会带整套 AGW 设备签名上下文 —— 纯 HTTP 直连实测被「系统错误」拦下
// （2026-10-24 实测：cookie 齐全仍返回 710010702）。因此签到走 CDP 无头浏览器
// 通道（与 Trae 凭证提取同思路）：注入 Cookie + 客户端 UA → 打开活动页 →
// 由页面自身的完整链路完成签到。
//
// 凭据 = 豆包桌面客户端（Chromium 壳）的 Cookie 库：
//
//	%LOCALAPPDATA%\Doubao\User Data\Default\Network\Cookies
//	密钥：Local State 的 os_crypt.encrypted_key → DPAPI → AES-256-GCM（v10）
//	明文头部有 32 字节校验块，跳过即得真实值（与 macOS 钥匙串方案同构）
//
// Cookie 里起作用的核心是 sessionid / sessionid_ss / ttwid / msToken 等；
// sessionid 长期有效（客户端登录态），无需轮换 —— 这是它与 MiniMax（1h token）
// 的本质区别：签到用的凭据不会自己过期，掉登录只发生在客户端侧退出账号时。
package doubao

import (
	"crypto/sha1"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/appdir"
)

// 活动页与客户端 UA（UA 必须带 SamanthaDoubao/ 才能过「仅限电脑版客户端」校验）。
const (
	ActivityURL = "https://www.doubao.com/pc/agent-explore"
	HomeURL     = "https://www.doubao.com/"
	ClientUA    = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/147.0.0.0 Safari/537.36 SamanthaDoubao/2.33.3"
)

// Account 豆包账号（Cookie 凭据）。
type Account struct {
	ID        string            `json:"id"`
	Label     string            `json:"label"`
	SessionID string            `json:"session_id"`
	Cookies   map[string]string `json:"cookies"`
	SavedAt   string            `json:"saved_at"`
	Source    string            `json:"source"` // client（客户端提取）/ manual（手动粘贴）
}

// PoolFile 号池文件。
type PoolFile struct {
	Accounts []*Account `json:"accounts"`
}

// PoolPath 号池文件路径（DOUBAO_POOL_FILE 可覆盖）。
func PoolPath() string {
	if v := strings.TrimSpace(os.Getenv("DOUBAO_POOL_FILE")); v != "" {
		return v
	}
	return appdir.Resolve(filepath.Join("data", "doubao_accounts.json"))
}

// Fingerprint 由 sessionid 派生稳定短 id。
func Fingerprint(sessionID string) string {
	sum := sha1.Sum([]byte("doubao:" + sessionID))
	return hex.EncodeToString(sum[:6])
}

// LoadPool 读取号池（不存在返回空池）。
func LoadPool() (*PoolFile, error) {
	raw, err := os.ReadFile(PoolPath())
	if err != nil {
		if os.IsNotExist(err) {
			return &PoolFile{}, nil
		}
		return nil, err
	}
	var f PoolFile
	if err := json.Unmarshal(raw, &f); err != nil {
		return nil, fmt.Errorf("号池文件损坏: %w", err)
	}
	return &f, nil
}

// SavePool 写回号池（0600，含 Cookie）。
func SavePool(f *PoolFile) error {
	if f == nil {
		return nil
	}
	sort.SliceStable(f.Accounts, func(i, j int) bool {
		return f.Accounts[i].ID < f.Accounts[j].ID
	})
	if dir := filepath.Dir(PoolPath()); dir != "" {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return err
		}
	}
	buf, err := json.MarshalIndent(f, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(PoolPath(), buf, 0o600)
}

// ParseCookieString 解析粘贴的 Cookie 文本（支持 "k=v; k2=v2" 与 "k: v" 两种形态）。
func ParseCookieString(s string) map[string]string {
	out := map[string]string{}
	s = strings.NewReplacer("\r\n", "\n", "; ", ";\n", ";", ";\n").Replace(strings.TrimSpace(s))
	for _, line := range strings.Split(s, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		line = strings.TrimPrefix(line, "Set-Cookie: ")
		line = strings.TrimPrefix(line, "Cookie: ")
		sep := strings.IndexAny(line, "=:")
		if sep <= 0 {
			continue
		}
		k := strings.TrimSpace(line[:sep])
		v := strings.TrimSpace(line[sep+1:])
		// "k=v; Path=/; HttpOnly" 形态只取第一段
		if i := strings.Index(v, ";"); i >= 0 {
			v = v[:i]
		}
		if k != "" && v != "" {
			out[k] = v
		}
	}
	return out
}

// Upsert 按 sessionid 去重写入账号（存在则更新全部 Cookie）。
func Upsert(cookies map[string]string, source string) (*Account, bool, error) {
	sid := cookies["sessionid"]
	if sid == "" {
		sid = cookies["sessionid_ss"]
	}
	if sid == "" {
		return nil, false, fmt.Errorf("Cookie 里没有 sessionid（无法定位账号）")
	}
	pool, err := LoadPool()
	if err != nil {
		return nil, false, err
	}
	id := Fingerprint(sid)
	added := true
	for _, a := range pool.Accounts {
		if a == nil {
			continue
		}
		if a.SessionID == sid || a.ID == id {
			a.Cookies = cookies
			a.SessionID = sid
			a.SavedAt = time.Now().Format(time.RFC3339)
			a.Source = source
			if a.Label == "" {
				a.Label = "豆包 " + tail(sid, 6)
			}
			if err := SavePool(pool); err != nil {
				return nil, false, err
			}
			return a, false, nil
		}
	}
	acc := &Account{
		ID:        id,
		Label:     fmt.Sprintf("豆包账号 %d", len(pool.Accounts)+1),
		SessionID: sid,
		Cookies:   cookies,
		SavedAt:   time.Now().Format(time.RFC3339),
		Source:    source,
	}
	acc.Label = "豆包 " + tail(sid, 6)
	pool.Accounts = append(pool.Accounts, acc)
	if err := SavePool(pool); err != nil {
		return nil, false, err
	}
	return acc, added, nil
}

// Remove 从号池移除账号。
func Remove(id string) error {
	pool, err := LoadPool()
	if err != nil {
		return err
	}
	out := pool.Accounts[:0]
	for _, a := range pool.Accounts {
		if a != nil && a.ID != id {
			out = append(out, a)
		}
	}
	pool.Accounts = out
	return SavePool(pool)
}

// Get 从号池取账号。
func Get(id string) *Account {
	pool, err := LoadPool()
	if err != nil {
		return nil
	}
	for _, a := range pool.Accounts {
		if a != nil && a.ID == id {
			return a
		}
	}
	return nil
}

// CookieHeader 把账号的 Cookie 拼成请求头形态（诊断用）。
func CookieHeader(cookies map[string]string) string {
	keys := make([]string, 0, len(cookies))
	for k := range cookies {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	parts := make([]string, 0, len(keys))
	for _, k := range keys {
		parts = append(parts, k+"="+cookies[k])
	}
	return strings.Join(parts, "; ")
}

// HasClient 判断本机是否安装了豆包客户端（凭据目录存在即认为安装）。
func HasClient() bool {
	_, err := os.Stat(ClientUserDataDir())
	return err == nil
}

func tail(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[len(s)-n:]
}
