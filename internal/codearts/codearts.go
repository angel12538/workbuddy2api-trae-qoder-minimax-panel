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
	"regexp"
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

// Account 一个 CodeArts 账号（= 一份登录 Cookie 快照）。
type Account struct {
	ID    string `json:"id"`
	Label string `json:"label"`
	// Identity 账号的**稳定身份**（domain_tag|user_tag）。
	//
	// 为什么单独存一个 Identity：认人（去重/合并）必须用跨登录不变的东西。
	// 会话类 Cookie（SessionID / cftk / JSESSIONID）每次登录都变，拿它们认人
	// 必然把同一个账号认成好几个；domain_tag+user_tag 才是华为云账号本身。
	// 2026-10-30 实测的真实故障：同一个人先后登录两次，号池里出现两条记录，
	// ID 互相还对不上（一条 ca1d671920b4、一条 423eab29ffdd），slot 还是
	// 同一个 → 两条互相覆盖，面板上看着像加了两个号，其实是一个。
	Identity string   `json:"identity,omitempty"`
	// Aliases 这个账号历史上用过的其它 ID。
	//
	// 号池合并（同一个人被拆成多条记录）时把被并掉的 ID 记到这里，
	// 这样「今天签没签」的记录不会因为合并而丢失——签到记录是按 ID 存的。
	Aliases []string `json:"aliases,omitempty"`
	Cookies  []Cookie `json:"cookies"`
	SavedAt  string   `json:"saved_at"`
	Nickname string   `json:"nickname,omitempty"`
	DomainID string   `json:"domain_id,omitempty"`
	// Slot 浏览器 profile 的名字（目录名 = Slot）。
	//
	// 注意：profile 归档**不能**用于免登录（实测见 codearts_browser.go 顶部），
	// 它现在只是「留个现场」；签到主路径是纯 HTTP。
	Slot string `json:"slot,omitempty"`
	// 积分快照（每次签到/查询后更新），面板直接展示，不必每次都打接口。
	CreditsRemain   float64 `json:"credits_remain,omitempty"`
	CreditsTotal    float64 `json:"credits_total,omitempty"`
	CreditsUsed     float64 `json:"credits_used,omitempty"`
	CreditsExpiring float64 `json:"credits_expiring,omitempty"`
	// DailyBonus 每日签到能领多少积分（实测当天活动 benefitAmount）。
	DailyBonus float64 `json:"daily_bonus,omitempty"`
	// LastGain 最近一次签到的实际增量（签到前后余额之差），LastGainAt 记录日期。
	LastGain   float64 `json:"last_gain,omitempty"`
	LastGainAt string  `json:"last_gain_at,omitempty"`
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

// IdentityOf 取账号的**跨登录稳定身份**。
//
// 判据：domain_tag（华为云域/租户 ID）+ user_tag（用户 ID）。两者合起来
// 就是"这是哪一个人"，跨登录、跨浏览器、跨机器都不变。
//
// 拿不到这两个 Cookie 时返回空串 —— 此时只能退回 Fingerprint（会话级），
// 认人精度会下降，调用方要能分辨这种情况。
func IdentityOf(cookies []Cookie) string {
	pick := map[string]string{}
	for _, c := range cookies {
		if c.Value != "" {
			pick[c.Name] = c.Value
		}
	}
	d, u := pick["domain_tag"], pick["user_tag"]
	if d == "" || u == "" {
		return ""
	}
	return d + "|" + u
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
//
// ⚠️ 只有 domain_tag 和 user_tag **同时**存在时，指纹才是跨登录稳定的。
// 少了任何一个就会掉到 masked_* 或会话票据那一档 —— 那样算出来的 id 在
// 下一次登录就会变，号池里就会出现"同一个人两条记录"。所以认人请用
// IdentityOf()，Fingerprint 只用来生成一个显示用的短 id。
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

// Upsert 保存账号（同一个人则更新，不新增）。
//
// 认人顺序（**从准到糊**）：
//  1. Identity（domain_tag|user_tag）—— 唯一可靠的跨登录身份；
//  2. ID 相等 —— 老号池没有 Identity 字段时的兼容路径。
//
// ⚠️ **绝不能用调试端口认人**（2026-10-30 用户实测踩到）：
// 浏览器窗口一关，端口立刻被回收复用（freePort 永远给第一个空闲端口，
// 上一个窗口关掉后下一个又拿到 9333）。拿端口认人 = "下一个登录的人被
// 当成上一个" → **加第二个账号会把第一个顶掉**（号池里只剩一条，
// 内容换成了新账号）。端口只用来记录"登录窗口在哪"，不参与认人。
func Upsert(p *Pool, acc *Account) (*Account, bool) {
	for _, a := range p.Accounts {
		if a == nil {
			continue
		}
		sameID := acc.ID != "" && a.ID == acc.ID
		sameIdentity := acc.Identity != "" && a.Identity != "" && acc.Identity == a.Identity
		// 老号池里没有 Identity 字段，用现存的 Cookie 反算一次再比。
		if !sameIdentity && acc.Identity != "" {
			if old := IdentityOf(a.Cookies); old != "" && old == acc.Identity {
				sameIdentity = true
			}
		}
		// 身份明确且不同，绝不合并，哪怕 ID 撞车。
		//
		// 真实故障（2026-10-30 10:49）：号池里老账号的 ID 是历史遗留
		// （旧算法算的 ca1d671920b4），而新账号的 domain_tag/user_tag
		// 算出的指纹恰好也是 ca1d671920b4 —— 仅凭 ID 就把两个不同的人
		// 合并了，老账号的会话被覆盖（面板上只剩一个号）。
		if sameIdentity == false && acc.Identity != "" && a.Identity != "" &&
			acc.Identity != a.Identity {
			continue
		}
		if !sameIdentity && !sameID {
			continue
		}
		// 命中同一个人：更新会话，**保留原 ID**（签到记录按 ID 存，改 ID 会丢历史）。
		a.Cookies = acc.Cookies
		a.SavedAt = acc.SavedAt
		if acc.Identity != "" {
			a.Identity = acc.Identity
		}
		if acc.Port > 0 {
			a.Port = acc.Port
		}
		if a.Slot == "" && acc.Slot != "" {
			// slot 一旦分配就不再变：它绑着这个账号的本地 profile 目录，
			// 换 slot 等于让两个账号共用/错开目录。
			a.Slot = acc.Slot
		}
		if acc.DomainID != "" {
			a.DomainID = acc.DomainID
		}
		if acc.Nickname != "" {
			a.Nickname, a.Label = acc.Nickname, acc.Nickname
		}
		acc.ID = a.ID
		return a, false
	}
	if acc.Label == "" {
		acc.Label = "CodeArts 账号 " + itoa(len(p.Accounts)+1)
	}
	p.Accounts = append(p.Accounts, acc)
	return acc, true
}

// AllIDs 这个账号的**全部**已知 ID（本体 + 历史别名）。
func (a *Account) AllIDs() []string {
	if a == nil {
		return nil
	}
	out := make([]string, 0, 1+len(a.Aliases))
	if a.ID != "" {
		out = append(out, a.ID)
	}
	for _, al := range a.Aliases {
		if al != "" && al != a.ID {
			out = append(out, al)
		}
	}
	return out
}

// MergePool 合并号池里的重复账号（同一个人被拆成多条时用）。
//
// 2026-10-30 实测的真实故障：同一个人先后在两个窗口登录，号池里出现两条
// 记录，domain_tag / user_tag / masked_user 全同，slot 也相同 —— 面板上看着
// 像"加了两个号"，其实是一个，而且两条 profile 互相覆盖。
//
// 合并规则：
//   - 按 Identity 分组；Identity 为空的记录不动（认不准就别乱合）；
//   - 每组保留**会话最新**的那条（SavedAt 最新，Cookie 最多者胜出）；
//   - 被并掉的 ID 记进 Aliases，保住"今天签没签"的记录；
//   - 最后给每个账号分配**互不相同**的 slot。
//
// 返回被合并掉的记录数。
func MergePool(p *Pool) int {
	if p == nil {
		return 0
	}
	groups := map[string][]*Account{}
	for _, a := range p.Accounts {
		if a == nil {
			continue
		}
		if a.Identity == "" {
			a.Identity = IdentityOf(a.Cookies)
		}
		if a.Identity == "" {
			continue
		}
		groups[a.Identity] = append(groups[a.Identity], a)
	}

	merged := 0
	kept := make([]*Account, 0, len(p.Accounts))
	for _, a := range p.Accounts {
		if a == nil {
			continue
		}
		grp := groups[a.Identity]
		if len(grp) <= 1 {
			kept = append(kept, a)
			continue
		}
		winner := pickFreshest(grp)
		if winner != a {
			merged++
			// 把被并掉这条的 ID 记成别名，历史签到记录才不会丢。
			if a.ID != "" && a.ID != winner.ID {
				winner.Aliases = appendUnique(winner.Aliases, a.ID)
			}
			continue
		}
		// 自己就是胜出者：把同组其它 ID 收成别名。
		for _, other := range grp {
			if other == a {
				continue
			}
			if other.ID != "" && other.ID != a.ID {
				a.Aliases = appendUnique(a.Aliases, other.ID)
			}
		}
		kept = append(kept, a)
	}
	p.Accounts = kept

	// ② slot 唯一化：两个账号共用一个 profile 目录 = 互相覆盖。
	used := map[string]bool{}
	for _, a := range p.Accounts {
		if a == nil {
			continue
		}
		want := a.Slot
		if want == "" {
			want = a.ID
		}
		if used[want] {
			base := a.ID
			if base == "" {
				base = "ca-" + a.Identity
			}
			base = shortHash(base)
			want = base
			n := 2
			for used[want] {
				want = base + "-" + itoa(n)
				n++
			}
		}
		a.Slot = want
		used[want] = true
	}
	return merged
}

// pickFreshest 挑一组里最新鲜的那条（SavedAt 新，Cookie 多）。
func pickFreshest(grp []*Account) *Account {
	best := grp[0]
	for _, a := range grp[1:] {
		if a.SavedAt > best.SavedAt ||
			(a.SavedAt == best.SavedAt && len(a.Cookies) > len(best.Cookies)) {
			best = a
		}
	}
	return best
}

func appendUnique(list []string, v string) []string {
	for _, x := range list {
		if x == v {
			return list
		}
	}
	return append(list, v)
}

// shortHash 取一段字符串的短哈希（12 位十六进制）。
func shortHash(s string) string {
	sum := sha1.Sum([]byte(s))
	return hex.EncodeToString(sum[:6])
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

// Seq 把序号转成字符串（导出版本给面板用，避免到处自己写 strconv）。
func Seq(n int) string { return itoa(n) }

// itoa 小工具：整数转十进制字符串。
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

// AccountKeyFromText 从控制台页面文本里取稳定账号标识。
//
// 为什么要这个：SessionID 每次登录都会变，用它做指纹会导致"每次登录多一个账号"。
// 页面顶部会渲染形如 hid__lj4oauvj421wxe 的账号标识，同一账号恒定。
func AccountKeyFromText(text string) string {
	re := regexp.MustCompile(`hid__[a-z0-9]{6,40}`)
	if m := re.FindString(text); m != "" {
		return m
	}
	return ""
}
