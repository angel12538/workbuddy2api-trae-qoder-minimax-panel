// Package codearts 华为云「码道 CodeArts Agent」通道。
//
// 与其它 IDE 通道的差别：
//   - 客户端（VS Code 派生）里**没有**签到接口，签到入口是控制台/网页里的
//     「活动福利 → 每日领取」，只能带网页会话去点；
//   - 客户端本地只给 STS 临时凭证（AK/SK/securityToken，约 12 小时过期），
//     拿它调不了活动接口，所以凭据用**浏览器登录后的 Cookie**。
//
// 因此本包只负责：账号池（Cookie 快照）+ 签到记录。真正的浏览器操作在
// internal/panel/codearts_browser.go（复用豆包那套 CDP）。
package codearts

import (
	"crypto/sha1"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// Cookie 一条网页 Cookie。
type Cookie struct {
	Name   string `json:"name"`
	Value  string `json:"value"`
	Domain string `json:"domain"`
	Path   string `json:"path,omitempty"`
}

// Account 一个 CodeArts 账号（= 一份登录 Cookie 快照 + 一份浏览器 profile）。
type Account struct {
	ID       string   `json:"id"`
	Label    string   `json:"label"`
	Cookies  []Cookie `json:"cookies"`
	SavedAt  string   `json:"saved_at"`
	Nickname string   `json:"nickname,omitempty"`
	DomainID string   `json:"domain_id,omitempty"`
	// Slot 浏览器 profile 的名字（目录名 = Slot）。
	//
	// 为什么必须有它：华为云的登录会话**活不过一小时**（SSOTGC/SessionID 是
	// 短时票据），把 Cookie 快照存下来第二天再注入，控制台一律判定未登录
	// （2026-10-27 实测：10-26 22:15 抓的 61 个 Cookie，10-27 09:00 注入
	// headless 后 /chat/home 仍显示「登录华为云」，/developer/ 直接跳登录页）。
	// 所以签到必须复用**登录时那个浏览器的 profile**（会话留在 profile 里），
	// 面板每天用同一 profile 拉一次无头浏览器即可。
	Slot string `json:"slot,omitempty"`
	// Port 记住「登录窗口」的调试端口：签到优先复用它（窗口里就是登录态，
	// 不用把 Cookie 注入到新的 headless 浏览器——实测注入后仍被判定未登录）。
	Port int `json:"port,omitempty"`
}

// Pool 账号池。
type Pool struct {
	Accounts []*Account `json:"accounts"`
}

// PoolPath 账号池路径。
func PoolPath() string {
	if v := strings.TrimSpace(os.Getenv("CODEARTS_POOL_FILE")); v != "" {
		return v
	}
	return filepath.Join("data", "ide_accounts", "codearts_accounts.json")
}

// LoadPool 读取（不存在 → 空池）。
func LoadPool() (*Pool, error) {
	raw, err := os.ReadFile(PoolPath())
	if err != nil {
		if os.IsNotExist(err) {
			return &Pool{}, nil
		}
		return nil, err
	}
	var p Pool
	if err := json.Unmarshal(raw, &p); err != nil {
		return nil, err
	}
	return &p, nil
}

// SavePool 写回（0600，含会话 Cookie）。
func SavePool(p *Pool) error {
	path := PoolPath()
	if dir := filepath.Dir(path); dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return err
		}
	}
	buf, err := json.MarshalIndent(p, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, buf, 0o600)
}

// Fingerprint 用账号的**稳定标识**派生 id（同一账号重复登录不会新增条目）。
//
// 优先级很重要 —— 会话类 Cookie 每次登录都会变，拿它们认人必然把同一个账号
// 认成好多个：
//   - domain_tag + user_tag：华为云账号的唯一 ID，跨登录不变（首选）。
//   - masked_user + masked_domain：脱敏账号名，稳定。
//   - SessionID / hwid 等会话票据：每次登录都变，只保证"同一份 Cookie 快照"
//     可重复识别，作为最后的退路。
//   - 全都没有时：全量 Cookie 哈希（仍稳定）。
func Fingerprint(cookies []Cookie) string {
	pick := map[string]string{}
	for _, c := range cookies {
		if c.Value != "" {
			pick[c.Name] = c.Value
		}
	}
	groups := [][]string{
		{"domain_tag", "user_tag"},
		{"masked_user", "masked_domain"},
		{"csb_user_id", "userId"},
		{"SessionID", "hwid", "hwssotoken", "hwssotoken_", "HWSID", "sessionid"},
	}
	for _, keys := range groups {
		parts := make([]string, 0, len(keys))
		for _, k := range keys {
			if v := pick[k]; v != "" {
				parts = append(parts, k+"="+v)
			}
		}
		if len(parts) > 0 {
			sum := sha1.Sum([]byte(strings.Join(parts, "&")))
			return hex.EncodeToString(sum[:6])
		}
	}
	parts := make([]string, 0, len(cookies))
	for _, c := range cookies {
		parts = append(parts, c.Name+"="+c.Value)
	}
	sort.Strings(parts)
	sum := sha1.Sum([]byte(strings.Join(parts, "&")))
	return hex.EncodeToString(sum[:6])
}

// DomainIDOf 取账号的稳定标识（华为云账号 ID），没有就返回空。
func DomainIDOf(cookies []Cookie) string {
	pick := map[string]string{}
	for _, c := range cookies {
		if c.Value != "" {
			pick[c.Name] = c.Value
		}
	}
	for _, k := range []string{"domain_tag", "user_tag"} {
		if v := pick[k]; v != "" {
			return v
		}
	}
	return ""
}

// Upsert 保存账号（同 id 则更新 Cookie）。
func Upsert(p *Pool, acc *Account) (*Account, bool) {
	for _, a := range p.Accounts {
		if a == nil {
			continue
		}
		// 同一个登录窗口（调试端口）就是同一个账号：Cookie 集合每次提取都会变
		// （WAF/埋点），只按指纹匹配会越加越多。
		samePort := acc.Port > 0 && a.Port == acc.Port
		if samePort || (acc.ID != "" && a.ID == acc.ID) {
			a.Cookies = acc.Cookies
			a.SavedAt = acc.SavedAt
			if samePort {
				acc.ID = a.ID // 沿用已有 ID，面板显示稳定
			}
			if acc.Port > 0 {
				a.Port = acc.Port // 登录窗口端口：签到要复用，漏了就会一直退回 headless
			}
			if acc.Slot != "" {
				a.Slot = acc.Slot // 浏览器 profile 名（签到靠它复用登录态）
			}
			if acc.DomainID != "" {
				a.DomainID = acc.DomainID
			}
			if acc.Nickname != "" {
				a.Nickname, a.Label = acc.Nickname, acc.Nickname
			}
			return a, false
		}
	}
	if acc.Label == "" {
		acc.Label = "CodeArts 账号 " + itoa(len(p.Accounts)+1)
	}
	p.Accounts = append(p.Accounts, acc)
	return acc, true
}

// Remove 删除账号。
func Remove(p *Pool, id string) bool {
	out := p.Accounts[:0]
	removed := false
	for _, a := range p.Accounts {
		if a != nil && a.ID == id {
			removed = true
			continue
		}
		out = append(out, a)
	}
	p.Accounts = out
	return removed
}

// CookieHeader 拼成请求头用（如后续要直接调接口）。
func CookieHeader(cookies []Cookie) string {
	parts := make([]string, 0, len(cookies))
	for _, c := range cookies {
		if c.Name != "" {
			parts = append(parts, c.Name+"="+c.Value)
		}
	}
	return strings.Join(parts, "; ")
}

// Now 统一时间格式。
func Now() string { return time.Now().Format(time.RFC3339) }

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b []byte
	for n > 0 {
		b = append([]byte{byte('0' + n%10)}, b...)
		n /= 10
	}
	return string(b)
}
