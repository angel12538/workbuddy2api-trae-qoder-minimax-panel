package codearts

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"
)

// =============================================================================
// C-CA-COOKIE@1：过期三元组、CDP 归一、Set-Cookie 合并、票据读取
// =============================================================================

// 验收点①：CDP expires=-1 必须落成 {Expires:-1, ExpiresKnown:true, Session:true}，
// 并且 JSON 里三个字段都要真的出现（omitempty 不能把 -1 吃掉）。
func TestNewObservedCookieTriplet(t *testing.T) {
	session := NewObservedCookie("SSOTGC", "TGT-x", ".huaweicloud.com", "/", -1)
	if session.Expires != -1 || !session.ExpiresKnown || !session.Session {
		t.Fatalf("CDP -1 必须是已知的会话 Cookie，得到 %+v", session)
	}
	raw, err := json.Marshal(session)
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatal(err)
	}
	if m["expires"] != float64(-1) {
		t.Fatalf("JSON 必须保留 expires=-1，得到 %s", raw)
	}
	if m["expires_known"] != true || m["session"] != true {
		t.Fatalf("JSON 必须带 expires_known/session，得到 %s", raw)
	}

	abs := NewObservedCookie("SSOJTC", "v", ".huaweicloud.com", "/", 1767225600)
	if abs.Expires != 1767225600 || !abs.ExpiresKnown || abs.Session {
		t.Fatalf("CDP >0 必须是已知绝对时间且非会话，得到 %+v", abs)
	}

	for _, v := range []float64{0, -2} {
		unknown := NewObservedCookie("CASTGCLOW", "v", ".huaweicloud.com", "/", v)
		if unknown.ExpiresKnown || unknown.Session || unknown.Expires != 0 {
			t.Fatalf("CDP expires=%v 必须按未知处理，得到 %+v", v, unknown)
		}
	}
}

// 验收点②：Set-Cookie 的 Max-Age=3600 → now+3600，且视为已知绝对时间。
func TestAbsorbSetCookiesMaxAge(t *testing.T) {
	now := time.Unix(1764000000, 0)
	a := &Account{}

	scs := []*http.Cookie{{Name: "SSOTGC", Value: "new", Domain: ".huaweicloud.com", Path: "/", MaxAge: 3600}}
	if got := a.AbsorbSetCookies(scs, now); got != 1 {
		t.Fatalf("新增一条应返回 changed=1，得到 %d", got)
	}
	if len(a.Cookies) != 1 {
		t.Fatalf("应只有一条 Cookie，得到 %d", len(a.Cookies))
	}
	c := a.Cookies[0]
	if !c.ExpiresKnown || c.Expires != float64(now.Unix()+3600) || c.Session {
		t.Fatalf("Max-Age=3600 应落成 known now+3600，得到 %+v", c)
	}
}

// Max-Age<=0（Go 解析为 MaxAge<0）与 Expires 过去时刻都必须删除该键。
func TestAbsorbSetCookiesDeletes(t *testing.T) {
	now := time.Unix(1764000000, 0)
	base := []Cookie{
		NewObservedCookie("old-maxage", "v", ".huaweicloud.com", "/", 1764000001),
		NewObservedCookie("old-expires", "v", ".huaweicloud.com", "/", 1764000001),
		NewObservedCookie("keep", "v", ".huaweicloud.com", "/", 1764000001),
	}
	a := &Account{Cookies: base}

	scs := []*http.Cookie{
		{Name: "old-maxage", Value: "", Domain: ".huaweicloud.com", Path: "/", MaxAge: -1},
		{Name: "old-expires", Value: "", Domain: ".huaweicloud.com", Path: "/", Expires: now.Add(-time.Hour)},
	}
	if got := a.AbsorbSetCookies(scs, now); got != 2 {
		t.Fatalf("删除两条应返回 changed=2，得到 %d", got)
	}
	if len(a.Cookies) != 1 || a.Cookies[0].Name != "keep" {
		t.Fatalf("应只剩 keep，得到 %+v", a.Cookies)
	}
	// 删除不存在的键不算变更。
	if got := a.AbsorbSetCookies(scs, now); got != 0 {
		t.Fatalf("重复删除不应产生变更，得到 %d", got)
	}
}

// 新值没有过期信息时：只更新 Value，**保留**已观测的过期三元组。
func TestAbsorbSetCookiesKeepsObservedTriplet(t *testing.T) {
	now := time.Unix(1764000000, 0)
	a := &Account{Cookies: []Cookie{
		NewObservedCookie("SSOTGC", "old", ".huaweicloud.com", "/", -1),
	}}
	scs := []*http.Cookie{{Name: "SSOTGC", Value: "new", Domain: ".huaweicloud.com", Path: "/"}}
	if got := a.AbsorbSetCookies(scs, now); got != 1 {
		t.Fatalf("Value 变化应返回 changed=1，得到 %d", got)
	}
	c := a.Cookies[0]
	if c.Value != "new" || c.Expires != -1 || !c.ExpiresKnown || !c.Session {
		t.Fatalf("未观测的新值不得擦掉已观测三元组，得到 %+v", c)
	}
	// 同一值再次下发：无变更，调用方不必写盘。
	if got := a.AbsorbSetCookies(scs, now); got != 0 {
		t.Fatalf("值未变不应返回变更，得到 %d", got)
	}
}

// 验收点③：两个域下的同名 cbc-sid 必须各保留一条（旧实现按 Name 单键会互相覆盖）。
func TestAbsorbSetCookiesSameNameDifferentDomain(t *testing.T) {
	now := time.Unix(1764000000, 0)
	a := &Account{Cookies: []Cookie{
		NewObservedCookie("cbc-sid", "dev", ".devcloud.huaweicloud.com", "/", -1),
		NewObservedCookie("cbc-sid", "main", ".huaweicloud.com", "/", -1),
	}}
	scs := []*http.Cookie{{
		Name: "cbc-sid", Value: "dev-new",
		Domain: ".devcloud.huaweicloud.com", Path: "/", MaxAge: 7200,
	}}
	if got := a.AbsorbSetCookies(scs, now); got != 1 {
		t.Fatalf("只应更新一个域的那条，得到 changed=%d", got)
	}
	if len(a.Cookies) != 2 {
		t.Fatalf("不同域同名 Cookie 必须各留一条，得到 %d 条：%+v", len(a.Cookies), a.Cookies)
	}
	if a.Cookies[0].Value != "dev-new" || !a.Cookies[0].ExpiresKnown {
		t.Fatalf("devcloud 域必须被更新，得到 %+v", a.Cookies[0])
	}
	if a.Cookies[1].Value != "main" || a.Cookies[1].Expires != -1 {
		t.Fatalf("huaweicloud 域那条不能被改，得到 %+v", a.Cookies[1])
	}
}

// Set-Cookie 省略 Domain/Path 时沿用同名唯一条目的域/路径，避免同一份登录态被拆条。
func TestAbsorbSetCookiesFillsScopeFromExisting(t *testing.T) {
	now := time.Unix(1764000000, 0)
	a := &Account{Cookies: []Cookie{
		{Name: "cftk", Value: "old", Domain: ".huaweicloud.com", Path: "/chat"},
	}}
	if got := a.AbsorbSetCookies([]*http.Cookie{
		{Name: "cftk", Value: "new", MaxAge: 600},
	}, now); got != 1 {
		t.Fatalf("应认到既有条目，changed=%d", got)
	}
	if len(a.Cookies) != 1 {
		t.Fatalf("不得产生重复条目，得到 %+v", a.Cookies)
	}
	if a.Cookies[0].Domain != ".huaweicloud.com" || a.Cookies[0].Path != "/chat" {
		t.Fatalf("应沿用既有域/路径，得到 %+v", a.Cookies[0])
	}
	if !a.Cookies[0].ExpiresKnown {
		t.Fatalf("Max-Age 应视为已知，得到 %+v", a.Cookies[0])
	}
}

// 验收点⑤：IDPDeadline 只认 ExpiresKnown && Expires>0 的票据，-1/0/未知全部忽略。
func TestIDPDeadlineIgnoresUnknownAndSession(t *testing.T) {
	cookies := []Cookie{
		NewObservedCookie("SSOTGC", "v", ".huaweicloud.com", "/", -1),  // 会话级
		NewObservedCookie("SSOJTC", "v", ".huaweicloud.com", "/", 0),   // 未知
		{Name: "CASTGCLOW", Value: "v", Expires: 100},                  // 有数字但未观测
		{Name: "cbc-sid", Value: "v", Expires: -1, ExpiresKnown: true}, // 会话级
		NewObservedCookie("hwid_cas_sid", "v", ".huaweicloud.com", "/", 1767225600),
		NewObservedCookie("SSOTGC", "v2", ".huaweicloud.com", "/", 1767225600+60),
	}
	got, ok := IDPDeadline(cookies)
	if !ok {
		t.Fatal("有一张已知且带绝对时间的票据，应该命中")
	}
	if got.Unix() != 1767225600 {
		t.Fatalf("应取最早时刻 1767225600，得到 %d", got.Unix())
	}

	onlySession := []Cookie{NewObservedCookie("SSOTGC", "v", ".huaweicloud.com", "/", -1)}
	if _, ok := IDPDeadline(onlySession); ok {
		t.Fatal("全是会话级票据时必须返回 false（没有绝对截止时间）")
	}
}

// =============================================================================
// C-CA-TICKET@1：票据来源、派生函数、持久化
// =============================================================================

func TestRefreshTicketDeadlineSources(t *testing.T) {
	abs := NewObservedCookie("SSOTGC", "v", ".huaweicloud.com", "/", 1767225600)
	want := time.Unix(1767225600, 0).Format(time.RFC3339)

	a := &Account{Cookies: []Cookie{abs}}
	a.RefreshTicketDeadline()
	if a.TicketDeadline != want || a.TicketSource != "cookie" {
		t.Fatalf("命中绝对票据应写 deadline+cookie，得到 %q/%q", a.TicketDeadline, a.TicketSource)
	}

	// 命中后票据被服务端改成会话级：来源是 cookie，必须清空（旧值已失效）。
	a.Cookies = []Cookie{NewObservedCookie("SSOTGC", "v", ".huaweicloud.com", "/", -1)}
	a.RefreshTicketDeadline()
	if a.TicketDeadline != "" || a.TicketSource != "" {
		t.Fatalf("来源为 cookie 时未命中必须清空，得到 %q/%q", a.TicketDeadline, a.TicketSource)
	}

	// 探测结论不能被一次"没采到"擦掉。
	for _, src := range []string{"session", "probe", "estimate"} {
		b := &Account{TicketDeadline: "", TicketSource: src}
		b.RefreshTicketDeadline()
		if b.TicketSource != src {
			t.Fatalf("来源 %s 必须保留，得到 %q", src, b.TicketSource)
		}
	}
}

func TestCanAutoRenew(t *testing.T) {
	future := time.Now().Add(2 * time.Hour).Format(time.RFC3339)
	past := time.Now().Add(-2 * time.Hour).Format(time.RFC3339)
	cases := []struct {
		name string
		a    *Account
		want bool
	}{
		{"无票据时间也可续期", &Account{TicketSource: "session"}, true},
		{"estimate 不阻止续期", &Account{TicketSource: "estimate"}, true},
		{"票据未过期", &Account{TicketDeadline: future, TicketSource: "cookie"}, true},
		{"票据已过期", &Account{TicketDeadline: past, TicketSource: "cookie"}, false},
		{"会话已死", &Account{ProbeState: "login_dead"}, false},
		{"续期停在登录页", &Account{RenewResult: "needs_login"}, false},
		{"deadline 不可解析按未知", &Account{TicketDeadline: "not-a-time"}, true},
		{"nil 账号", nil, false},
	}
	for _, c := range cases {
		if got := CanAutoRenew(c.a); got != c.want {
			t.Errorf("%s：期望 %v，得到 %v", c.name, c.want, got)
		}
	}
}

func TestSessionExpiresAt(t *testing.T) {
	last := time.Date(2026, 11, 2, 14, 30, 0, 0, time.Local)
	a := &Account{LastOKAt: last.Format(time.RFC3339)}
	want := last.Add(7 * time.Hour).Format(time.RFC3339)
	if got := SessionExpiresAt(a); got != want {
		t.Fatalf("应是 last_ok_at+7h，期望 %q 得到 %q", want, got)
	}
	if got := SessionExpiresAt(&Account{}); got != "" {
		t.Fatalf("没有 last_ok_at 必须返回空，得到 %q", got)
	}
	if got := SessionExpiresAt(&Account{LastOKAt: "bad"}); got != "" {
		t.Fatalf("解析失败必须返回空，得到 %q", got)
	}
	if got := SessionExpiresAt(nil); got != "" {
		t.Fatalf("nil 必须返回空，得到 %q", got)
	}
}

// 契约 JSON 名固定，写错一个字母下游面板就炸；这里锁死字段名。
func TestAccountAndCaptureResultJSONFields(t *testing.T) {
	raw, err := json.Marshal(&Account{
		ID: "x", TicketDeadline: "2026-11-03T10:00:00+08:00", TicketSource: "session",
		ProbeState: "session_only", ProbeMsg: "m", ProbeAt: "t",
		RenewAttemptAt: "t", RenewNextAt: "t", RenewResult: "renewed",
		RenewFailAt: "t", RenewFailMsg: "m",
	})
	if err != nil {
		t.Fatal(err)
	}
	var acc map[string]any
	if err := json.Unmarshal(raw, &acc); err != nil {
		t.Fatal(err)
	}
	for _, k := range []string{
		"ticket_deadline", "ticket_source", "probe_state", "probe_msg", "probe_at",
		"renew_attempt_at", "renew_next_at", "renew_result", "renew_fail_at", "renew_fail_msg",
	} {
		if _, ok := acc[k]; !ok {
			t.Errorf("Account JSON 缺字段 %q：%s", k, raw)
		}
	}

	craw, err := json.Marshal(&CaptureResult{
		OK: true, Phase: CapturePhaseProbed, Cookies: []Cookie{{Name: "x"}},
		TicketDeadline: "d", TicketSource: "cookie", ExpiresKnownCount: 1,
		SessionCookieCount: 2, Message: "m", Evidence: "e",
	})
	if err != nil {
		t.Fatal(err)
	}
	var cr map[string]any
	if err := json.Unmarshal(craw, &cr); err != nil {
		t.Fatal(err)
	}
	for _, k := range []string{
		"ok", "phase", "cookies", "ticket_deadline", "ticket_source",
		"expires_known_count", "session_cookie_count", "message", "evidence",
	} {
		if _, ok := cr[k]; !ok {
			t.Errorf("CaptureResult JSON 缺字段 %q：%s", k, craw)
		}
	}
}

// =============================================================================
// 持久化：UpdatePool 原子性 + 旧号池兼容
// =============================================================================

func useTempPool(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "codearts_accounts.json")
	t.Setenv("CODEARTS_POOL_FILE", path)
	return path
}

// 验收点⑥：fn 返回错误时不得写盘（宁可丢弃内存改动，也不落坏数据）。
func TestUpdatePoolFnErrorKeepsFile(t *testing.T) {
	path := useTempPool(t)
	before := []byte(`{"accounts":[]}` + "\n")
	if err := os.WriteFile(path, before, 0o600); err != nil {
		t.Fatal(err)
	}
	boom := errors.New("boom")
	err := UpdatePool(func(p *Pool) error {
		p.Accounts = append(p.Accounts, &Account{ID: "should-not-land"})
		return boom
	})
	if !errors.Is(err, boom) {
		t.Fatalf("应原样返回 fn 的错误，得到 %v", err)
	}
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(after) != string(before) {
		t.Fatalf("fn 失败不得写盘：before=%q after=%q", before, after)
	}

	// 成功路径必须真的落盘。
	if err := UpdatePool(func(p *Pool) error {
		p.Accounts = append(p.Accounts, &Account{ID: "ok-id"})
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	p, err := LoadPool()
	if err != nil {
		t.Fatal(err)
	}
	if len(p.Accounts) != 1 || p.Accounts[0].ID != "ok-id" {
		t.Fatalf("成功更新应落盘，得到 %+v", p.Accounts)
	}
}

// 验收点⑦：旧号池（无 expires 字段）可读；补上观测值后保存，新字段出现且 ID/Aliases 不丢。
func TestPoolRoundTripKeepsIDAndNewFields(t *testing.T) {
	path := useTempPool(t)
	old := `{
  "accounts": [
    {
      "id": "ca1d671920b4",
      "label": "CodeArts huvj421wxe",
      "identity": "dom|user",
      "aliases": ["423eab29ffdd"],
      "cookies": [
        {"name": "SSOTGC", "value": "TGT-x", "domain": ".huaweicloud.com", "path": "/"}
      ],
      "saved_at": "2026-11-02T18:15:08+08:00"
    }
  ]
}
`
	if err := os.WriteFile(path, []byte(old), 0o600); err != nil {
		t.Fatal(err)
	}
	p, err := LoadPool()
	if err != nil {
		t.Fatalf("旧号池必须可读：%v", err)
	}
	if len(p.Accounts) != 1 || p.Accounts[0].ID != "ca1d671920b4" {
		t.Fatalf("旧号池 ID 丢失：%+v", p.Accounts)
	}
	legacy := p.Accounts[0].Cookies[0]
	if legacy.ExpiresKnown || legacy.Session || legacy.Expires != 0 {
		t.Fatalf("旧 Cookie 必须按未知处理，得到 %+v", legacy)
	}

	// 模拟一次 CDP 采集：会话票据的 -1 被真实观测到。
	p.Accounts[0].Cookies = []Cookie{
		NewObservedCookie("SSOTGC", "TGT-x", ".huaweicloud.com", "/", -1),
		NewObservedCookie("SSOJTC", "JTC-x", ".huaweicloud.com", "/", 1767225600),
	}
	p.Accounts[0].RefreshTicketDeadline()
	if err := SavePool(p); err != nil {
		t.Fatal(err)
	}

	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var doc struct {
		Accounts []map[string]any `json:"accounts"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatal(err)
	}
	if len(doc.Accounts) != 1 {
		t.Fatalf("账号数不应变化，得到 %d", len(doc.Accounts))
	}
	acc := doc.Accounts[0]
	if acc["id"] != "ca1d671920b4" {
		t.Fatalf("ID 不得丢：%v", acc["id"])
	}
	aliases, _ := acc["aliases"].([]any)
	if len(aliases) != 1 || aliases[0] != "423eab29ffdd" {
		t.Fatalf("Aliases 不得丢：%v", acc["aliases"])
	}
	if acc["ticket_source"] != "cookie" || acc["ticket_deadline"] == "" {
		t.Fatalf("保存后应出现票据来源与截止时间：%v/%v", acc["ticket_source"], acc["ticket_deadline"])
	}
	cookies, _ := acc["cookies"].([]any)
	if len(cookies) != 2 {
		t.Fatalf("Cookie 条数异常：%d", len(cookies))
	}
	c0, _ := cookies[0].(map[string]any)
	if c0["expires"] != float64(-1) || c0["expires_known"] != true || c0["session"] != true {
		t.Fatalf("会话 Cookie 的新字段必须落盘：%v", c0)
	}

	// 权限：0600（文件）。Windows 没有 Unix 权限位（Stat 恒为 0666），只在类 Unix 上断言。
	if runtime.GOOS != "windows" {
		info, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		if info.Mode().Perm() != 0o600 {
			t.Fatalf("号池文件必须是 0600，得到 %v", info.Mode().Perm())
		}
	}
}

// =============================================================================
// R4.3：SavePool 原子写（temp+fsync+rename、0600、失败不破坏原文件）
// =============================================================================

// 保存成功后：文件存在、内容是完整 JSON、槽字段已补全、目录里没有临时文件残留。
// Windows 没有 Unix 权限位（Stat 恒为 0666），只在类 Unix 上断言 0600。
//
// ⚠️ 号池文件要放在**尚不存在**的子目录里（2026-11-04 修）：断言的是
// 「**被测代码**把号池目录建成 0700」，所以目录必须由 writePoolFileAtomically
// 的 MkdirAll(dir, 0700) 亲手创建。之前直接用 t.TempDir() 的叶子目录 ——
// 那是 testing 自己用 0777 建的（Linux 上落地成 0755），与被测代码无关，
// 在 CI（ubuntu）上必然失败。
func TestSavePoolAtomicWriteFileModeAndContent(t *testing.T) {
	base := t.TempDir()
	path := filepath.Join(base, "pool", "codearts_accounts.json")
	t.Setenv("CODEARTS_POOL_FILE", path)
	t.Setenv("CODEARTS_SLOT_ROOT", filepath.Join(t.TempDir(), "slots"))

	if err := SavePool(&Pool{Accounts: []*Account{{ID: "atomic-1", Label: "CodeArts 原子"}}}); err != nil {
		t.Fatalf("SavePool 失败：%v", err)
	}

	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("保存后文件必须存在：%v", err)
	}
	var got Pool
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("落盘内容必须是完整 JSON（不能是半截写）：%v\n%s", err, raw)
	}
	if len(got.Accounts) != 1 || got.Accounts[0].ID != "atomic-1" {
		t.Fatalf("落盘内容不对：%+v", got.Accounts)
	}
	if got.Accounts[0].SlotID != "atomic-1" {
		t.Fatalf("保存时应补全槽字段，得到 slot_id=%q", got.Accounts[0].SlotID)
	}

	// 成功路径 rename 已消费掉临时文件，目录里不得残留。
	leftovers, err := filepath.Glob(filepath.Join(filepath.Dir(path), ".*.tmp-*"))
	if err != nil {
		t.Fatal(err)
	}
	if len(leftovers) != 0 {
		t.Fatalf("成功后不得残留临时文件：%v", leftovers)
	}

	if runtime.GOOS != "windows" {
		info, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		if info.Mode().Perm() != 0o600 {
			t.Fatalf("号池文件必须是 0600，得到 %v", info.Mode().Perm())
		}
		dirInfo, err := os.Stat(filepath.Dir(path))
		if err != nil {
			t.Fatal(err)
		}
		if dirInfo.Mode().Perm() != 0o700 {
			t.Fatalf("号池目录必须是 0700，得到 %v", dirInfo.Mode().Perm())
		}
	}
}

// 写入失败时原文件必须**逐字节不变**，且不留下临时文件。
//
// 两个平台各覆盖一个失败点（都是真实可复现的）：
//   - Windows：目标文件只读 → os.Rename/MoveFileEx(REPLACE_EXISTING) 被拒
//     （已实测 "Access is denied"），正好卡在「临时文件已写完、即将覆盖」的窗口；
//   - 类 Unix：目录不可写 → 连临时文件都创建不出来，覆盖「temp 写失败」。
func TestSavePoolFailureKeepsOriginalFile(t *testing.T) {
	path := useTempPool(t)
	dir := filepath.Dir(path)
	before := []byte("{\"accounts\":[{\"id\":\"kept\",\"label\":\"Kept\"}]}\n")
	if err := os.WriteFile(path, before, 0o600); err != nil {
		t.Fatal(err)
	}

	if runtime.GOOS == "windows" {
		if err := os.Chmod(path, 0o444); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = os.Chmod(path, 0o600) })
	} else {
		if os.Geteuid() == 0 {
			t.Skip("root 无视目录权限，无法模拟写失败")
		}
		if err := os.Chmod(dir, 0o500); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = os.Chmod(dir, 0o700) })
	}

	err := SavePool(&Pool{Accounts: []*Account{{ID: "must-not-land"}}})
	if err == nil {
		t.Fatal("写入失败必须返回错误（不得静默丢数据）")
	}

	after, readErr := os.ReadFile(path)
	if readErr != nil {
		t.Fatalf("原文件必须仍可读：%v", readErr)
	}
	if !bytes.Equal(after, before) {
		t.Fatalf("写入失败不得破坏原文件：before=%q after=%q", before, after)
	}

	leftovers, globErr := filepath.Glob(filepath.Join(dir, ".*.tmp-*"))
	if globErr != nil {
		t.Fatal(globErr)
	}
	if len(leftovers) != 0 {
		t.Fatalf("失败后不得残留临时文件：%v", leftovers)
	}
}

// 并发 20 次 UpdatePool：每次加一个账号，最后一次读回必须 20 个都在。
//
// 这是 R4.3 的第二个验收点（并发不丢其他账号）。旧实现 SavePool 直接截断写，
// 并发时会出现「读到半截 JSON → 解析失败」或「后写者覆盖前者」。
func TestUpdatePoolConcurrentKeepsAllAccounts(t *testing.T) {
	useTempPool(t)
	t.Setenv("CODEARTS_SLOT_ROOT", filepath.Join(t.TempDir(), "slots"))

	const writers = 20
	var wg sync.WaitGroup
	errCh := make(chan error, writers)
	for i := 0; i < writers; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			id := fmt.Sprintf("concurrent-%02d", idx)
			if err := UpdatePool(func(p *Pool) error {
				p.Accounts = append(p.Accounts, &Account{ID: id, Label: "L-" + id})
				return nil
			}); err != nil {
				errCh <- fmt.Errorf("%s: %w", id, err)
			}
		}(i)
	}
	wg.Wait()
	close(errCh)
	for err := range errCh {
		t.Fatalf("并发 UpdatePool 出错：%v", err)
	}

	p, err := LoadPool()
	if err != nil {
		t.Fatalf("并发写后号池必须可解析：%v", err)
	}
	seen := make(map[string]bool, len(p.Accounts))
	for _, a := range p.Accounts {
		if a == nil {
			continue
		}
		if seen[a.ID] {
			t.Fatalf("账号重复：%s", a.ID)
		}
		seen[a.ID] = true
	}
	if len(seen) != writers {
		ids := make([]string, 0, len(seen))
		for id := range seen {
			ids = append(ids, id)
		}
		sort.Strings(ids)
		t.Fatalf("并发写丢了账号：want=%d got=%d ids=%v", writers, len(seen), ids)
	}
	// 槽字段必须一并落盘（fillAccountSlots 在 savePoolLocked 里做）。
	if p.Accounts[0].DeviceGUID == "" || !strings.HasPrefix(p.Accounts[0].PersonKey, "person:") {
		t.Fatalf("保存后槽字段必须完整：%+v", p.Accounts[0])
	}
}
