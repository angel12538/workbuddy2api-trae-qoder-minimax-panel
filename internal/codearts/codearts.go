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
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"
)

// Cookie 一条网页 Cookie。
//
// 过期信息是**三元组**，三者必须一起看，不能只看 Expires：
//   - Expires 的数字含义：>0 = Unix 秒；-1 = 会话 Cookie；0 = 未知；
//   - ExpiresKnown 表示这个数字**是不是真的观测到的**（CDP 返回值 / Set-Cookie），
//     false 时 Expires 一律不可信（旧号池、粘贴导入都是这种）；
//   - Session 表示服务端明确声明"关上浏览器就没了"（CDP expires=-1）。
//
// 为什么不能只用 Expires 一个数字：旧号池里 0 既可能是"没采到"也可能是"会话级"，
// 把两者混为一谈，票据寿命就永远判不出来（IDPDeadline 只能跳过它）。
type Cookie struct {
	Name   string `json:"name"`
	Value  string `json:"value"`
	Domain string `json:"domain"`
	Path   string `json:"path,omitempty"`
	// Expires 过期时刻：>0 Unix 秒；-1 会话 Cookie；0 未知。
	Expires float64 `json:"expires,omitempty"`
	// ExpiresKnown 是否真实观测过过期信息（CDP Storage.getCookies 或 Set-Cookie）。
	ExpiresKnown bool `json:"expires_known,omitempty"`
	// Session 服务端明确声明为会话级（此时 Expires=-1 且 ExpiresKnown=true）。
	Session bool `json:"session,omitempty"`
}

// NewObservedCookie 把**观测到**的过期信息归一成号池 Cookie（唯一入口）。
//
// cdpExpires 为 CDP Storage.getCookies 的 expires 原值：
//   - >0  ：带绝对过期时刻（Unix 秒），原样保存；
//   - -1  ：CDP 对"没有 expires 字段"的会话 Cookie 返回 -1 —— 这是**已知事实**，
//     必须落成 {Expires:-1, ExpiresKnown:true, Session:true}，否则会话票据会被
//     当成"未知"，票据寿命永远算不出来；
//   - 0/缺失：未知，三元组全不置位（不得当成会话级，也不得当成 0 秒）。
//
// 所有采集路径（浏览器 CDP、补采、Set-Cookie）都必须走这里，禁止各调用点
// 自行解释数字。
func NewObservedCookie(name, value, domain, path string, cdpExpires float64) Cookie {
	c := Cookie{Name: name, Value: value, Domain: domain, Path: path}
	switch {
	case cdpExpires > 0:
		c.Expires = cdpExpires
		c.ExpiresKnown = true
	case cdpExpires == -1:
		c.Expires = -1
		c.ExpiresKnown = true
		c.Session = true
	}
	return c
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
	Identity string `json:"identity,omitempty"`
	// Aliases 这个账号历史上用过的其它 ID。
	//
	// 号池合并（同一个人被拆成多条记录）时把被并掉的 ID 记到这里，
	// 这样「今天签没签」的记录不会因为合并而丢失——签到记录是按 ID 存的。
	Aliases  []string `json:"aliases,omitempty"`
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
	// LastOKAt 最近一次接口调用成功（那一刻会话还活着）的时刻。
	LastOKAt string `json:"last_ok_at,omitempty"`
	// TicketDeadline 华为 ID/CAS 侧**持久票据**里最早的过期时刻（RFC3339）。
	//
	// 这是"能不能自动续期"的唯一判据之一：应用会话（约 7 小时）死后，只要票据还没
	// 过期，走一遍 authui 的 SSO 链就有机会静默换回新会话；票据也过期了就只能人工扫码。
	// 空 = 没有绝对截止时间（会话级票据 / 未观测），**不猜**，交给 TicketSource 说明来源。
	TicketDeadline string `json:"ticket_deadline,omitempty"`
	// TicketSource ticket_deadline 的来源（固定枚举）：
	//   cookie   —— 来自带绝对过期时间的票据，此时 TicketDeadline 非空；
	//   session  —— 采集/探测确认票据全是会话级，TicketDeadline 必须为空，但**允许续期**；
	//   probe    —— 探测确认会话票据可用，但没有绝对截止时间；
	//   estimate —— 无票据证据，仅按 last_ok_at+7h 估算，**仅供展示**，不得阻止续期；
	//   ""       —— 未知。
	TicketSource string `json:"ticket_source,omitempty"`
	// ProbeState 最近一次补采/探测的结论：
	// pending|ok|session_only|login_dead|failed|""。
	ProbeState string `json:"probe_state,omitempty"`
	ProbeMsg   string `json:"probe_msg,omitempty"`
	// ProbeAt 最近一次探测的时刻（RFC3339）。
	ProbeAt string `json:"probe_at,omitempty"`
	// RenewAttemptAt / RenewNextAt 最近一次静默续期的尝试时刻与下次允许时刻（RFC3339）。
	RenewAttemptAt string `json:"renew_attempt_at,omitempty"`
	RenewNextAt    string `json:"renew_next_at,omitempty"`
	// RenewResult 最近一次续期结论：alive|renewed|ticket_expired|needs_login|failed|""。
	RenewResult string `json:"renew_result,omitempty"`
	// RenewFailAt / RenewFailMsg 最近一次静默续期的时刻与原因。
	// 有这两个字段才能把"每 15 分钟刷一条失败日志"收敛成每天一条。
	RenewFailAt  string `json:"renew_fail_at,omitempty"`
	RenewFailMsg string `json:"renew_fail_msg,omitempty"`
	// SlotID 身份槽 ID：一个账号 = 一个槽，默认等于 ID。
	//
	// 为什么要有槽：华为云 web 侧不下发可清理的设备指纹 Cookie（见
	// evidence/huawei-client-extract-recon.md §H.4），设备身份完全由浏览器
	// profile 容器承载。两号共用一份 profile，服务端看到的永远是同一台设备。
	// 槽把「一个账号一份 profile + 一份设备 ID」变成可持久化、可校验的事实。
	//
	// 槽字段全部 omitempty：旧号池没有它们也能读，LoadPool 会在内存里补齐。
	SlotID string `json:"slot_id,omitempty"`
	// SlotDir 相对槽根的 profile 目录名（默认等于 SlotID）。
	//
	// 不用绝对路径：号池可能在别的机器上被读，绝对路径会把人带到错误位置。
	// 绝对路径属于跨机器噪音，权威路径由 SlotRoot()+SlotDir+profile 现算。
	SlotDir string `json:"slot_dir,omitempty"`
	// DeviceGUID 这个槽的稳定设备 GUID（UUIDv5，由 SlotID 确定性派生）。
	//
	// 只在本地记录、不上报；作用是把「同号跨进程/跨重启 = 同一台设备」
	// 变成可比较的值（新槽若与旧槽 GUID 相同，说明槽目录被复用了）。
	DeviceGUID string `json:"device_guid,omitempty"`
	// PersonKey 按人分桶键（person:<hash>），由 masked_phone/masked_user/
	// masked_domain 或 label 派生。只存哈希，不落明文手机号。
	//
	// 用途（留给后续按人轮换）：同一个自然人只保留 1 个活跃槽，避免自相残杀；
	// 不同 PersonKey 各自独立槽，满足「不同号 = 不同身份」。
	PersonKey string `json:"person_key,omitempty"`
	// ClientSource 登录态来源：browser|client|import。旧数据默认 import。
	ClientSource string `json:"client_source,omitempty"`
}

// ClientSource 的固定枚举（客户端只会是这三个之一）。
const (
	ClientSourceBrowser = "browser"
	ClientSourceClient  = "client"
	ClientSourceImport  = "import"
)

// ssoTicketNames 华为云**跨会话**票据（能用来静默换回应用会话的那几个）。
//
// 2026-11-01 实测：拿号池里 54 条 Cookie（含这几个）走
// `auth.huaweicloud.com/authui/login?service=…`，登录已死 2 天的账号最终停在
// 人工登录页（手机号/密码/验证码）—— 说明票据本身也有寿命，必须在它活着时用。
var ssoTicketNames = []string{"SSOTGC", "SSOJTC", "CASTGCLOW", "hwid_cas_sid", "cbc-sid"}

// IDPDeadline 这批 Cookie 里**最早的**持久票据过期时刻（没有带绝对过期时间的票据 → false）。
//
// 只统计“真实观测过并且带绝对过期时间”的票据：
//   - ExpiresKnown=false（粘贴导入/旧快照）→ 跳过，不能把未知当成 0 秒；
//   - Expires<=0（-1 会话级、0 未知）→ 跳过，会话票据没有绝对截止时间；
//   - 只认 ssoTicketNames 里的跨会话票据（应用会话 Cookie 的寿命代表不了票据寿命）。
func IDPDeadline(cookies []Cookie) (time.Time, bool) {
	wanted := map[string]bool{}
	for _, n := range ssoTicketNames {
		wanted[n] = true
	}
	var best time.Time
	for _, c := range cookies {
		if !wanted[c.Name] || !c.ExpiresKnown || c.Expires <= 0 {
			continue
		}
		t := time.Unix(int64(c.Expires), 0)
		if best.IsZero() || t.Before(best) {
			best = t
		}
	}
	if best.IsZero() {
		return time.Time{}, false
	}
	return best, true
}

// sessionLifetime 应用会话的大致寿命（实测约 7 小时，仅在「没有票据证据」时用于展示）。
const sessionLifetime = 7 * time.Hour

// SessionExpiresAt 按 last_ok_at+7h 估算会话到期时刻（RFC3339）；没有 last_ok_at 返回 ""。
//
// 这是**派生值，不落盘**：落盘会让"会话还活着"时刻变化时出现两个真相。
// estimate 只用于面板展示，不得用来阻止续期（见 CanAutoRenew）。
func SessionExpiresAt(a *Account) string {
	if a == nil {
		return ""
	}
	t, err := time.Parse(time.RFC3339, strings.TrimSpace(a.LastOKAt))
	if err != nil {
		return ""
	}
	return t.Add(sessionLifetime).Format(time.RFC3339)
}

// CanAutoRenew 现在能不能自动续期（纯函数，规则顺序固定）。
//
//  1. 探测确认会话已死（ProbeState=login_dead）或续期停在登录页（RenewResult=needs_login）
//     → 不能，只能人工重新登录；
//  2. TicketDeadline 可解析且已过期 → 不能（票据都死了，静默链走不通）；
//  3. 其余一律可以 —— **包括 TicketDeadline 为空、TicketSource=session/estimate**。
//     这一条是缺陷一的核心：过去把"没有票据时间"当成"不可续期"，于是永远不续。
//     TicketDeadline 解析失败按"未知"处理，不 panic、也不阻止续期。
func CanAutoRenew(a *Account) bool {
	if a == nil {
		return false
	}
	// 结论（login_dead / needs_login）只在**没有新证据**时算数。
	// 用户重新登录过一次 → SavedAt 更新到结论之后 → 旧结论作废，重新允许续期。
	if !a.HasFreshLogin() {
		if a.ProbeState == "login_dead" || a.RenewResult == "needs_login" {
			return false
		}
	}
	if d, err := time.Parse(time.RFC3339, strings.TrimSpace(a.TicketDeadline)); err == nil {
		if d.Before(time.Now()) {
			return false
		}
	}
	return true
}

// HasFreshLogin 这个账号**在最近一次结论之后**是否又登录过。
//
// 为什么需要它（实测 2026-11-04）：
//
//	号池里 renew_result=needs_login 是**上一次尝试的结论**，不是账号的固有属性。
//	但旧实现把它当成固有属性，于是：用户明明重新登录成功了，面板还是显示
//	「只能重新登录」，手动「立即续期」也秒回 needs_login —— 账号被永久锁死。
//
// 判据：SavedAt（最后一次登录/采集落盘时刻）晚于最近一次结论时刻
// （RenewAttemptAt / RenewFailAt / ProbeAt）⇒ 结论已过期，作废。
func (a *Account) HasFreshLogin() bool {
	if a == nil {
		return false
	}
	saved, err := time.Parse(time.RFC3339, strings.TrimSpace(a.SavedAt))
	if err != nil {
		return false
	}
	for _, s := range []string{a.RenewAttemptAt, a.RenewFailAt, a.ProbeAt} {
		if t, e := time.Parse(time.RFC3339, strings.TrimSpace(s)); e == nil && saved.After(t) {
			return true
		}
	}
	return false
}

// RefreshTicketDeadline 按账号当前的 Cookie 重算票据到期时刻与来源。
//
// 每次换了 Cookie（登录、保活收到新票据、静默续期）都要调一次 —— 不然
// TicketDeadline 会停在老值，自动续期就按错的寿命判断跑。
//
// 来源规则（不能把一次"没采到"擦成"不可续期"）：
//   - 命中带绝对时间的票据 → TicketDeadline=最早时刻、TicketSource=cookie；
//   - 未命中且当前来源是 cookie / 空 → 清空两者（旧值已失效）；
//   - 当前来源是 session/probe/estimate → 保留，探测结论不能被覆盖写擦掉。
func (a *Account) RefreshTicketDeadline() {
	if a == nil {
		return
	}
	if t, ok := IDPDeadline(a.Cookies); ok {
		a.TicketDeadline = t.Format(time.RFC3339)
		a.TicketSource = "cookie"
		return
	}
	switch a.TicketSource {
	case "", "cookie":
		a.TicketDeadline = ""
		a.TicketSource = ""
	}
}

// Pool 账号池。
type Pool struct {
	Accounts []*Account `json:"accounts"`
}

// cookieMergeKey Cookie 的合并键：name + "\x00" + lower(domain) + "\x00" + path。
//
// 为什么不能只用 name：华为云同一份登录态里 cbc-sid / JSESSIONID 等会在多个域下
// 各存一份（旧号池实测 cbc-sid 有 2 条），按 name 单键合并会互相覆盖，丢票据。
func cookieMergeKey(name, domain, path string) string {
	return name + "\x00" + strings.ToLower(domain) + "\x00" + path
}

// AbsorbSetCookies 把一批 Set-Cookie 合并进账号，返回实际变更的键数。
//
// 合并规则（C-CA-COOKIE@1）：
//  1. 键 = (name, lower(domain), path)，不得只用 name；
//  2. Max-Age<=0（Go 解析为 MaxAge<0）或 Expires 为过去时刻 → 删除该键；
//  3. 新值没有过期信息（ExpiresKnown=false）→ **只更新 Value**，保留已观测的三元组；
//  4. 新值有过期信息 → 覆盖三元组；
//  5. Max-Age 优先于 Expires（RFC 6265 §5.3）：服务端续期常用 Max-Age 下发新寿命。
//
// changed 按"受影响的键"计数（同一键的 Value 与过期时间同时变也只算 1）。
// 无变更时调用方不必写盘。
func (a *Account) AbsorbSetCookies(scs []*http.Cookie, now time.Time) (changed int) {
	if a == nil {
		return 0
	}
	index := make(map[string]int, len(a.Cookies))
	for i, c := range a.Cookies {
		index[cookieMergeKey(c.Name, c.Domain, c.Path)] = i
	}
	for _, sc := range scs {
		if sc == nil || sc.Name == "" {
			continue
		}
		domain, path := a.resolveSetCookieScope(sc)
		key := cookieMergeKey(sc.Name, domain, path)
		idx, exists := index[key]

		// Max-Age 优先；MaxAge<0 是 Go 对 "Max-Age: 0" 的表示，等价于立即删除。
		// Set-Cookie 没有"会话级"声明：既无 Expires 也无 Max-Age 时语义是未知，
		// 所以这里的 Session 恒为 false，绝不把 -1 猜给服务端没声明过的 Cookie。
		var (
			expires float64
			known   bool
			remove  bool
		)
		switch {
		case sc.MaxAge < 0:
			remove = true
		case sc.MaxAge > 0:
			expires = float64(now.Unix() + int64(sc.MaxAge))
			known = true
		case !sc.Expires.IsZero():
			if !sc.Expires.After(now) {
				remove = true
			} else {
				expires = float64(sc.Expires.Unix())
				known = true
			}
		}

		if remove {
			if exists {
				a.Cookies = append(a.Cookies[:idx], a.Cookies[idx+1:]...)
				// 删掉后索引整体前移，重建映射（Set-Cookie 一般只有个位数，代价可忽略）。
				index = make(map[string]int, len(a.Cookies))
				for i, c := range a.Cookies {
					index[cookieMergeKey(c.Name, c.Domain, c.Path)] = i
				}
				changed++
			}
			continue
		}
		if !exists {
			if sc.Value == "" {
				continue // 空值又不删除：没有可用的 Cookie 内容，不制造空条目。
			}
			a.Cookies = append(a.Cookies, Cookie{
				Name: sc.Name, Value: sc.Value, Domain: domain, Path: path,
				Expires: expires, ExpiresKnown: known,
			})
			index[key] = len(a.Cookies) - 1
			changed++
			continue
		}
		cur := a.Cookies[idx]
		next := cur
		if sc.Value != "" {
			next.Value = sc.Value
		}
		if known {
			next.Expires, next.ExpiresKnown, next.Session = expires, true, false
		}
		if next != cur {
			a.Cookies[idx] = next
			changed++
		}
	}
	return changed
}

// resolveSetCookieScope 补全 Set-Cookie 里省略的 Domain / Path。
//
// Set-Cookie 省略 Domain（host-only）或 Path 时，标准做法是由请求 URL 推导；
// 这里拿不到请求 URL，就退一步沿用**同名且唯一**的已有条目的域/路径，避免同一份
// 登录态被拆成 domain="" 的重复条目。找不到唯一候选时保留空域（path 回落 "/"）。
func (a *Account) resolveSetCookieScope(sc *http.Cookie) (domain, path string) {
	domain, path = sc.Domain, sc.Path
	if domain != "" && path != "" {
		return domain, path
	}
	var sameName []Cookie
	for _, c := range a.Cookies {
		if c.Name == sc.Name {
			sameName = append(sameName, c)
		}
	}
	if d, p, ok := singleScopeOf(sameName); ok {
		if domain == "" {
			domain = d
		}
		if path == "" {
			path = p
		}
	}
	if path == "" {
		path = "/"
	}
	return domain, path
}

// singleScopeOf 这一组 Cookie 是否共用同一个 (domain, path)。
func singleScopeOf(list []Cookie) (string, string, bool) {
	if len(list) == 0 {
		return "", "", false
	}
	d, p := list[0].Domain, list[0].Path
	for _, c := range list[1:] {
		if !strings.EqualFold(c.Domain, d) || c.Path != p {
			return "", "", false
		}
	}
	return d, p, true
}

// =============================================================================
// 身份槽（Identity Slot）：一个账号一份 profile + 一份设备 ID
// =============================================================================
//
// 设计依据：evidence/huawei-client-extract-recon.md §H。
// 不变量：槽与账号 1:1 绑定；不同账号绝不复用同一 SlotDir；DeviceGUID 只允许
// 由本槽自己的 SlotID 派生，绝不从别的槽复制（复制 = 把 A 的会话带进 B 槽）。

// slotDeviceNamespace 派生 DeviceGUID 用的固定 UUID 命名空间。
//
// 取 RFC 4122 附录 C 的 DNS 命名空间（常量，永不变更）；改成别的值会让所有
// 已存在的槽算出新 GUID，等于宣称「换了设备」，因此禁止调整。
var slotDeviceNamespace = [16]byte{
	0x6b, 0xa7, 0xb8, 0x10, 0x9d, 0xad, 0x11, 0xd1,
	0x80, 0xb4, 0x00, 0xc0, 0x4f, 0xd4, 0x30, 0xc8,
}

// SlotRoot 身份槽根目录。
//
// 优先级：
//  1. 环境变量 CODEARTS_SLOT_ROOT（测试与自定义部署用，必须为绝对路径或完整相对路径）；
//  2. 跟随号池文件所在目录：<号池目录>/codearts-slots。
//
// 为什么跟随号池而不是写死 data/codearts-slots：号池可以用
// CODEARTS_POOL_FILE 指到别处（测试、多实例），槽必须跟着号池走，否则
// 「槽在哪」与「号池在哪」会出现两个真相。
func SlotRoot() string {
	if v := strings.TrimSpace(os.Getenv("CODEARTS_SLOT_ROOT")); v != "" {
		return filepath.Clean(v)
	}
	return filepath.Join(filepath.Dir(PoolPath()), "codearts-slots")
}

// EnsureSlot 就地补全账号的身份槽字段（幂等，不碰 ID/Aliases/其它字段）。
//
// 补全规则（全部确定性派生，同号跨进程/跨重启稳定）：
//   - SlotID 空 → ID；ID 也空 → "ca-"+hash(IdentityOf) → label → Cookie 指纹；
//   - SlotDir 空 → SlotID（并规范化：拒绝绝对路径与 ".." 逃逸）；
//   - DeviceGUID 空 → UUIDv5(namespace, SlotID)，同 SlotID 永远同一 GUID；
//   - PersonKey 空 → "person:"+hash(masked_phone|masked_user|masked_domain|label|ID)；
//   - ClientSource 空 → "import"（旧数据没有来源信息，只能按导入处理）。
//
// LoadPool 与 SavePool 都会调用：读进来的旧号池在内存里即可用槽字段，
// 下一次保存就把它们落盘，无需单独的迁移步骤。
func (a *Account) EnsureSlot() {
	if a == nil {
		return
	}
	if a.SlotID == "" {
		a.SlotID = a.deriveSlotID()
	}
	a.SlotDir = normalizeSlotDir(a.SlotDir, a.SlotID)
	if a.DeviceGUID == "" {
		a.DeviceGUID = deriveDeviceGUID(a.SlotID)
	}
	if a.PersonKey == "" {
		a.PersonKey = derivePersonKey(a)
	}
	if a.ClientSource == "" {
		a.ClientSource = ClientSourceImport
	}
}

// SlotProfileDir 这个槽的 Chromium profile 目录（绝对路径）。
//
// 结构：<槽根>/<SlotDir>/profile —— profile 子目录才是 --user-data-dir，
// 槽目录本身还放 slot.json / 备份 / 诊断快照，不能整目录交给浏览器。
func (a *Account) SlotProfileDir() string {
	if a == nil {
		return ""
	}
	a.EnsureSlot()
	return filepath.Join(SlotRoot(), a.SlotDir, "profile")
}

// SlotExists 槽的 profile 目录是否真的存在（存在 ≠ 可免登录，见 Account.Slot 注释）。
func (a *Account) SlotExists() bool {
	if a == nil {
		return false
	}
	info, err := os.Stat(a.SlotProfileDir())
	return err == nil && info.IsDir()
}

// deriveSlotID 账号没有 ID 时派生一个稳定槽 ID。
//
// 顺序从准到糊：IdentityOf（跨登录稳定）→ Label（用户可见名）→ Cookie 指纹。
// 三者都拿不到时 Fingerprint 仍会给出一个稳定值（空快照的哈希），
// 保证 EnsureSlot 不会产出空 SlotID（空 ID 会让两个号共用槽）。
func (a *Account) deriveSlotID() string {
	if id := strings.TrimSpace(a.ID); id != "" {
		return id
	}
	if identity := IdentityOf(a.Cookies); identity != "" {
		return "ca-" + shortHash("identity:"+identity)
	}
	if label := strings.TrimSpace(a.Label); label != "" {
		return "ca-" + shortHash("label:"+label)
	}
	return "ca-" + Fingerprint(a.Cookies)
}

// normalizeSlotDir 把槽目录名收敛成「槽根下的相对目录」。
//
// raw 为空 → fallback（=SlotID）。非空但为绝对路径、带盘符、Clean 后变成 ".."
// 或以 "../" 开头的值 → fallback。这样即使号池被手工改坏，也无法把 profile
// 写到槽根之外（旧字段不可信，但新字段必须自证安全）。
func normalizeSlotDir(raw, fallback string) string {
	name := strings.TrimSpace(raw)
	if name == "" {
		return fallback
	}
	if filepath.IsAbs(name) || filepath.VolumeName(name) != "" {
		return fallback
	}
	clean := filepath.Clean(name)
	if clean == "." || clean == ".." || strings.HasPrefix(clean, ".."+string(filepath.Separator)) {
		return fallback
	}
	return clean
}

// deriveDeviceGUID 由 SlotID 确定性派生 UUIDv5（RFC 4122 §4.3）。
//
// 为什么不用随机 UUIDv4：槽字段会被反复补全（Load 时内存补、Save 时再补），
// 随机值每次都不一样，号池里就永远存不稳，也无法判断「槽目录是否被复用」。
func deriveDeviceGUID(slotID string) string {
	h := sha1.New()
	_, _ = h.Write(slotDeviceNamespace[:])
	_, _ = h.Write([]byte("codearts-identity-slot\x00"))
	_, _ = h.Write([]byte(slotID))
	sum := h.Sum(nil)

	var b [16]byte
	copy(b[:], sum[:16])
	b[6] = (b[6] & 0x0f) | 0x50 // 版本 5（SHA-1 名字型）
	b[8] = (b[8] & 0x3f) | 0x80 // RFC 4122 变体
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}

// derivePersonKey 按人分桶键（"person:"+短哈希），只存哈希不落明文。
//
// 取键顺序：masked_phone（两个人最可靠的区分）→ masked_user → masked_domain
// → label → ID → Cookie 指纹。全部命中不到时仍给出稳定值，不留空串
// （空 PersonKey 会让「同一个人」的分桶全部塌到同一个键）。
func derivePersonKey(a *Account) string {
	pick := func(name string) string {
		for _, c := range a.Cookies {
			if c.Name == name && c.Value != "" {
				return c.Value
			}
		}
		return ""
	}
	for _, name := range []string{"masked_phone", "masked_user", "masked_domain"} {
		if v := pick(name); v != "" {
			return "person:" + shortHash("cookie:"+name+":"+v)
		}
	}
	if label := strings.TrimSpace(a.Label); label != "" {
		return "person:" + shortHash("label:"+label)
	}
	if id := strings.TrimSpace(a.ID); id != "" {
		return "person:" + shortHash("id:"+id)
	}
	return "person:" + shortHash("cookies:"+Fingerprint(a.Cookies))
}

// fillAccountSlots 给池里所有账号补全槽字段（内存操作，不写盘）。
func fillAccountSlots(p *Pool) {
	if p == nil {
		return
	}
	for _, a := range p.Accounts {
		a.EnsureSlot()
	}
}

// PoolPath 账号池路径。
func PoolPath() string {
	if v := strings.TrimSpace(os.Getenv("CODEARTS_POOL_FILE")); v != "" {
		return v
	}
	return filepath.Join("data", "ide_accounts", "codearts_accounts.json")
}

// poolMu 号池文件的包级写锁。
//
// 补采、Keepalive、续期、签到可能**并发**改号池；各自 Load→改→Save 会互相覆盖
// （后写者拿的是旧快照，把前者的更新整条抹掉）。所有写盘必须走 UpdatePool。
//
// SavePool 也持这把锁：外部即使裸调 SavePool，也不会与 UpdatePool 的
// 「读-改-写」交错。UpdatePool 内部走 savePoolLocked，**不能**再调 SavePool ——
// sync.Mutex 不可重入，持锁再调会自锁。
var poolMu sync.Mutex

// LoadPool 读取（不存在 → 空池）。
//
// 读到的旧号池会在内存里补全身分槽字段（不写盘）：调用方拿到的账号立刻
// 可以用 SlotProfileDir，但也必须知道「文件里还没有这些字段」，落盘由
// 下一次 SavePool 完成。
//
// 读不需要 poolMu：写盘是 rename 覆盖，读端要么看到旧文件、要么看到新文件，
// 不会读到写了一半的 JSON。
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
	fillAccountSlots(&p)
	return &p, nil
}

// SavePool 原子写回（0600，含会话 Cookie）。
//
// 原子性是 R4.3 的硬要求：过去用 os.WriteFile 直接截断写，写到一半崩溃/断电
// 就留下半截 JSON，下一次 LoadPool 解析失败 = **整个号池丢失**。
// 现在的顺序是：同目录临时文件（0600）→ 写入 → fsync → rename 覆盖。
// rename 是原子的：要么是完整旧文件，要么是完整新文件。
//
// 失败语义：任一步失败都会删掉临时文件并返回错误，**原文件保持原样**。
//
// 并发：本函数自己持 poolMu；UpdatePool 走 savePoolLocked（同一把锁但已持有），
// 因此调用方**不得**在 UpdatePool 的 fn 内部再调 SavePool（会自锁）。
func SavePool(p *Pool) error {
	poolMu.Lock()
	defer poolMu.Unlock()
	return savePoolLocked(p)
}

// savePoolLocked 已经持有 poolMu 时的写盘实现。
//
// 保存前再补一次槽字段：调用方可能直接 new Account 塞进池里（没走 LoadPool），
// 不补就会把空槽字段写进文件，下次读出来两个账号共用空 SlotDir。
func savePoolLocked(p *Pool) error {
	if p == nil {
		p = &Pool{}
	}
	fillAccountSlots(p)
	buf, err := json.MarshalIndent(p, "", "  ")
	if err != nil {
		return err
	}
	return writePoolFileAtomically(PoolPath(), buf)
}

// writePoolFileAtomically 同目录临时文件 → 写入 → fsync → rename 覆盖。
//
// 细节与理由：
//   - 父目录 MkdirAll 0700：号池里有会话 Cookie，目录不能让其他用户读；
//   - CreateTemp 生成**唯一**临时名（不是固定的 "<path>.tmp"）：即使有
//     绕过 poolMu 的外部写入，也不会两个写者踩同一个临时文件；
//   - 临时文件 Chmod 0600：Unix 上 CreateTemp 已是 0600，显式设一次是为了
//     语义明确；Windows 没有 Unix 权限位，Chmod 只清只读位，不报错；
//   - Sync 把内容刷到磁盘再 rename，避免「rename 成功但内容还在页缓存里」；
//   - 任一步失败都删除临时文件：残留的 .tmp 不参与 LoadPool，但会污染目录。
func writePoolFileAtomically(path string, data []byte) error {
	dir := filepath.Dir(path)
	if dir == "" {
		dir = "."
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}

	tmp, err := os.CreateTemp(dir, "."+filepath.Base(path)+".tmp-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()

	// closeAndRemove 只在失败路径用；rename 成功后临时文件已不存在，不能删。
	closeAndRemove := func() {
		_ = tmp.Close()
		_ = os.Remove(tmpName)
	}

	if err := tmp.Chmod(0o600); err != nil {
		closeAndRemove()
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		closeAndRemove()
		return err
	}
	if err := tmp.Sync(); err != nil {
		closeAndRemove()
		return err
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(tmpName)
		return err
	}
	if err := os.Rename(tmpName, path); err != nil {
		_ = os.Remove(tmpName)
		return err
	}
	return nil
}

// UpdatePool 原子的「读-改-写」：持锁执行 LoadPool → fn → SavePool。
//
// fn 返回错误时**不写盘**（坏数据宁可留在内存里丢弃，也不落盘）；
// 只有 fn 成功返回才落盘，写盘仍是 0600 + 目录 0700。
func UpdatePool(fn func(*Pool) error) error {
	poolMu.Lock()
	defer poolMu.Unlock()
	p, err := LoadPool()
	if err != nil {
		return err
	}
	if p == nil {
		p = &Pool{}
	}
	if fn != nil {
		if err := fn(p); err != nil {
			return err
		}
	}
	return savePoolLocked(p)
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

// LabelOf 从 Cookie 里拼一个**能区分账号**的名字。
//
// 为什么不能再用「CodeArts 账号 N」（N=len+1）：号池里两个真人（脱敏手机号分别是
// 177\*\*\*\*2169 / 156\*\*\*\*8342）都被叫成「CodeArts 账号 2」—— 插第二条时
// len(Accounts) 已经是 1，两条算出同一个名字，面板上看着就像同一个号被登了两次。
func LabelOf(cookies []Cookie) string {
	pick := map[string]string{}
	for _, c := range cookies {
		if c.Value != "" && pick[c.Name] == "" {
			pick[c.Name] = c.Value
		}
	}
	for _, k := range []string{"masked_user", "masked_domain", "masked_phone"} {
		if t := shortTail(pick[k]); t != "" {
			return "CodeArts " + t
		}
	}
	return ""
}

// shortTail 取脱敏标识里**没打码**的那一段（h*********uvj421wxe → huvj421wxe）。
func shortTail(s string) string {
	t := strings.ReplaceAll(strings.TrimSpace(s), "*", "")
	if len(t) < 3 {
		return ""
	}
	if r := []rune(t); len(r) > 12 {
		t = string(r[len(r)-12:])
	}
	return t
}

// DedupeLabels 把重名的账号改成可区分的名字（启动时整理号池顺带做）。
func DedupeLabels(p *Pool) int {
	if p == nil {
		return 0
	}
	fixed := 0
	// ① 有 Nickname 的账号：Label 必须就是 Nickname（Upsert 平时也是这么写的，
	//    历史数据里可能留下没同步的编号名）。
	for _, a := range p.Accounts {
		if a == nil || a.Nickname == "" {
			continue
		}
		if a.Label != a.Nickname {
			a.Label = a.Nickname
			fixed++
		}
	}
	// ② 统计重名（只算自动命名的那些，用户自己起的名字不掺和）。
	seen := map[string]int{}
	for _, a := range p.Accounts {
		if a != nil && a.Nickname == "" && a.Label != "" {
			seen[a.Label]++
		}
	}
	// ③ 撞名的改成脱敏账号名；改完还撞就补序号。
	used := map[string]bool{}
	for _, a := range p.Accounts {
		if a == nil {
			continue
		}
		if a.Nickname != "" {
			used[a.Label] = true
			continue
		}
		if a.Label != "" && !used[a.Label] && seen[a.Label] <= 1 {
			used[a.Label] = true
			continue
		}
		want := LabelOf(a.Cookies)
		if want == "" {
			want = "CodeArts " + a.ID
		}
		base := want
		for n := 2; used[want]; n++ {
			want = base + "-" + itoa(n)
		}
		if want != a.Label {
			a.Label = want
			fixed++
		}
		used[want] = true
	}
	return fixed
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
		a.RefreshTicketDeadline()
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
		// 优先用脱敏账号名当标签。用「CodeArts 账号 N」（N=len+1）时两个真人会撞名
		// —— 实测号池里两条不同手机号都叫「CodeArts 账号 2」，面板上看着就像
		// 同一个号被登了两次。
		if l := LabelOf(acc.Cookies); l != "" {
			acc.Label = l
		} else {
			acc.Label = "CodeArts 账号 " + itoa(len(p.Accounts)+1)
		}
	}
	acc.RefreshTicketDeadline()
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
