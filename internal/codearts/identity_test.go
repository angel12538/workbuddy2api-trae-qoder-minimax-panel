package codearts

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// ck 造一条 Cookie（定义在 codearts_test.go 里）。

// huID 是实测的那个华为云账号（domain_tag=租户，user_tag=用户）。
const (
	huDomain = "c89103cc576b49c299c773db80c8dd3d"
	huUser   = "1fc7818c8c2c414694ad40d8560205c1"
)

// ── 身份 ────────────────────────────────────────────────────────────────

// TestIdentityStableAcrossLogins 同一个人两次登录，身份必须一致。
//
// 这是「能不能加多个号」的根：以前用会话 Cookie 认人，同一个人两次登录
// 会被认成两个号，号池里出现两条记录、slot 还撞车互相覆盖。
func TestIdentityStableAcrossLogins(t *testing.T) {
	first := []Cookie{
		ck("domain_tag", huDomain), ck("user_tag", huUser),
		ck("SessionID", "11111111-2222-3333-4444-555555555555"),
		ck("devclouddevuibjtcftk", "AAAA-BBBB-CCCC"),
	}
	second := []Cookie{
		ck("domain_tag", huDomain), ck("user_tag", huUser),
		ck("SessionID", "99999999-8888-7777-6666-555555555555"),
		ck("devclouddevuibjtcftk", "DDDD-EEEE-FFFF"),
	}
	if IdentityOf(first) != IdentityOf(second) {
		t.Fatalf("同一个华为云账号两次登录身份必须一致：%q vs %q",
			IdentityOf(first), IdentityOf(second))
	}
	if IdentityOf(first) == "" {
		t.Fatal("domain_tag + user_tag 齐全时身份不能为空")
	}
}

// TestIdentityDistinguishesAccounts 不同账号身份必须不同。
func TestIdentityDistinguishesAccounts(t *testing.T) {
	a := []Cookie{ck("domain_tag", huDomain), ck("user_tag", "user-a")}
	b := []Cookie{ck("domain_tag", huDomain), ck("user_tag", "user-b")}
	if IdentityOf(a) == IdentityOf(b) {
		t.Fatal("不同用户必须算出不同身份")
	}
}

// TestIdentityEmptyWithoutStableCookies 缺稳定标识时身份为空（别瞎认人）。
func TestIdentityEmptyWithoutStableCookies(t *testing.T) {
	c := []Cookie{ck("SessionID", "abc"), ck("HWWAFSESID", "waf")}
	if IdentityOf(c) != "" {
		t.Fatalf("没有 domain_tag/user_tag 时身份应为空，实际 %q", IdentityOf(c))
	}
}

// TestUpsertByIdentityMergesSamePerson 同一个人重新登录必须**更新**而不是新增。
func TestUpsertByIdentityMergesSamePerson(t *testing.T) {
	p := &Pool{}
	acc := &Account{
		ID: "old-id", Identity: IdentityOf([]Cookie{ck("domain_tag", huDomain), ck("user_tag", huUser)}),
		Slot: "old-slot", Cookies: []Cookie{ck("domain_tag", huDomain), ck("user_tag", huUser)},
		SavedAt: Now(),
	}
	p.Accounts = append(p.Accounts, acc)

	again := &Account{
		ID: "new-id", Identity: IdentityOf([]Cookie{ck("domain_tag", huDomain), ck("user_tag", huUser)}),
		Slot: "new-slot", Port: 9333,
		Cookies: []Cookie{ck("domain_tag", huDomain), ck("user_tag", huUser), ck("SessionID", "fresh")},
		SavedAt: Now(),
	}
	saved, added := Upsert(p, again)
	if added {
		t.Fatal("同一个人的二次登录不应新增记录")
	}
	if len(p.Accounts) != 1 {
		t.Fatalf("号池里应该只有 1 条，实际 %d", len(p.Accounts))
	}
	if saved.ID != "old-id" {
		t.Fatalf("ID 必须保持稳定（签到记录按 ID 存）：%q", saved.ID)
	}
	if saved.Slot != "old-slot" {
		t.Fatalf("slot 必须保持稳定：%q", saved.Slot)
	}
	if len(saved.Cookies) != 3 {
		t.Fatalf("Cookie 必须换成新的那套，实际 %d 条", len(saved.Cookies))
	}
}

// TestMergePoolCollapsesDuplicates 同一个人在号池里有多条 → 合并成一条。
//
// 实测故障现场：两条记录的 domain_tag / user_tag / masked_user 全同，
// slot 也相同，面板上看着像"加了两个号"，其实是一个。
func TestMergePoolCollapsesDuplicates(t *testing.T) {
	id := IdentityOf([]Cookie{ck("domain_tag", huDomain), ck("user_tag", huUser)})
	p := &Pool{Accounts: []*Account{
		{ID: "aaaa11112222", Identity: id, Slot: "shared", SavedAt: "2026-10-28T13:12:06+08:00",
			Cookies: []Cookie{ck("domain_tag", huDomain), ck("user_tag", huUser)}},
		{ID: "bbbb33334444", Identity: id, Slot: "shared", SavedAt: "2026-10-30T10:08:20+08:00",
			Cookies: []Cookie{ck("domain_tag", huDomain), ck("user_tag", huUser), ck("SessionID", "fresh")}},
	}}
	merged := MergePool(p)
	if merged != 1 {
		t.Fatalf("应合并掉 1 条，实际 %d", merged)
	}
	if len(p.Accounts) != 1 {
		t.Fatalf("合并后应只剩 1 条，实际 %d", len(p.Accounts))
	}
	keep := p.Accounts[0]
	if keep.ID != "bbbb33334444" {
		t.Fatalf("应保留会话更新的那条，实际 %q", keep.ID)
	}
	// 被并掉的 ID 必须留成别名，否则"今天签没签"的记录会丢。
	found := false
	for _, al := range keep.Aliases {
		if al == "aaaa11112222" {
			found = true
		}
	}
	if !found {
		t.Fatalf("被并掉的 ID 必须记进别名，实际 %v", keep.Aliases)
	}
	if len(keep.AllIDs()) != 2 {
		t.Fatalf("AllIDs 应含本体与别名，实际 %v", keep.AllIDs())
	}
}

// TestMergePoolKeepsSlotUnique 合并后 slot 必须互不相同。
func TestMergePoolKeepsSlotUnique(t *testing.T) {
	idA := IdentityOf([]Cookie{ck("domain_tag", huDomain), ck("user_tag", "user-a")})
	idB := IdentityOf([]Cookie{ck("domain_tag", huDomain), ck("user_tag", "user-b")})
	p := &Pool{Accounts: []*Account{
		{ID: "aaaaaaaaaaaa", Identity: idA, Slot: "same", SavedAt: "2026-10-30T10:00:00+08:00", Cookies: []Cookie{ck("user_tag", "user-a")}},
		{ID: "bbbbbbbbbbbb", Identity: idB, Slot: "same", SavedAt: "2026-10-30T10:01:00+08:00", Cookies: []Cookie{ck("user_tag", "user-b")}},
	}}
	MergePool(p)
	if p.Accounts[0].Slot == p.Accounts[1].Slot {
		t.Fatalf("两个账号不能共用同一个 slot（会互相覆盖）：%q", p.Accounts[0].Slot)
	}
}

// ── 接口契约 ────────────────────────────────────────────────────────────

// TestEmptyBodyIsSessionExpired HTTP 200 + 空 body = 会话失效。
//
// 实测：华为云网关对过期会话返回的是 200 空 body，不是 401/403。
// 只看状态码会把"会话没了"当成"接口坏了"。
func TestEmptyBodyIsSessionExpired(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		// 空 body
	}))
	defer srv.Close()
	old := APIBase
	APIBase = srv.URL
	defer func() { APIBase = old }()

	acc := &Account{Cookies: []Cookie{ck("devclouddevuibjtcftk", "X")}}
	_, _, err := doAPI(acc, http.MethodGet, "/x", nil)
	if !errors.Is(err, ErrSessionExpired) {
		t.Fatalf("200 空 body 必须判成会话失效，实际 err=%v", err)
	}
	if _, err := Credits(acc); !errors.Is(err, ErrSessionExpired) {
		t.Fatalf("Credits 必须把空 body 报成会话失效，实际 %v", err)
	}
}

// TestClaimSendsIdempotentKey 领取必须带幂等键。
//
// 实测：不带时当天第二次领取返回 40001「并发冲突，请重试」。
func TestClaimSendsIdempotentKey(t *testing.T) {
	var got map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&got)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"code":0,"message":"ok"}`))
	}))
	defer srv.Close()
	old := APIBase
	APIBase = srv.URL
	defer func() { APIBase = old }()

	acc := &Account{Cookies: []Cookie{ck("devclouddevuibjtcftk", "X")}}
	key := DailyIdempotentKey("acct-1", 34, "20261030")
	if _, err := ClaimWithKey(acc, 34, key); err != nil {
		t.Fatalf("领取失败: %v", err)
	}
	if got["idempotentKey"] != key {
		t.Fatalf("请求体必须带 idempotentKey，实际 %v", got)
	}
	if got["campaignId"].(float64) != 34 {
		t.Fatalf("campaignId 不对：%v", got["campaignId"])
	}
	if got["channel"] != "WEB" {
		t.Fatalf("channel 必须是 WEB：%v", got["channel"])
	}
	// 同一天同账号必须生成同一个键（幂等）。
	if DailyIdempotentKey("acct-1", 34, "20261030") != key {
		t.Fatal("同一天必须生成同一个幂等键")
	}
	if DailyIdempotentKey("acct-1", 34, "20261031") == key {
		t.Fatal("不同日期必须生成不同的幂等键")
	}
}

// TestClaimResultAlreadyAlready 识别"今天已领"。
func TestClaimResultAlreadyAlready(t *testing.T) {
	cases := []struct {
		code int
		msg  string
		want bool
	}{
		{40001, "并发冲突，请重试", true},
		{0, "ok", false},
		{500, "系统繁忙", false},
	}
	for _, c := range cases {
		r := &ClaimResult{OK: c.code == 0, Code: c.code, Message: c.msg}
		if r.Already() != c.want {
			t.Fatalf("code=%d msg=%q → Already()=%v，期望 %v", c.code, c.msg, r.Already(), c.want)
		}
	}
}

// TestDailyCampaignPicksUserLogin 每日签到活动是 type=USER_LOGIN。
//
// 实测真实活动列表里有 4 条：USER_LOGIN（每日签到领100积分）、
// NEW_USER_REGISTER、INVITE_USER、STUDENT_CERTIFIED —— 只有第一条是签到。
func TestDailyCampaignPicksUserLogin(t *testing.T) {
	camps := []Campaign{
		{CampaignID: 4, Type: "NEW_USER_REGISTER", Title: "新用户注册送 4000 积分", BenefitAmount: 4000},
		{CampaignID: 3, Type: "INVITE_USER", Title: "推荐码上有礼", BenefitAmount: 1000},
		{CampaignID: 2, Type: "STUDENT_CERTIFIED", Title: "学生认证领积分", BenefitAmount: 4000},
		{CampaignID: 34, Type: "USER_LOGIN", Title: "每日签到领100积分", BenefitAmount: 100},
	}
	c, ok := DailyCampaign(camps)
	if !ok || c.CampaignID != 34 {
		t.Fatalf("必须挑中 USER_LOGIN 那条，实际 ok=%v c=%+v", ok, c)
	}
	if c.BenefitAmount != 100 {
		t.Fatalf("每日签到额度应为 100，实际 %d", c.BenefitAmount)
	}
}

// TestCampaignClaimedStatus 已领状态的判定。
func TestCampaignClaimedStatus(t *testing.T) {
	for _, s := range []string{"CLAIMED", "CONFIRM", "CONFIRMED", "CONSUME"} {
		if !(Campaign{Status: s}).Claimed() {
			t.Fatalf("状态 %s 应判为已领", s)
		}
	}
	for _, s := range []string{"", "ELIGIBLE", "PENDING"} {
		if (Campaign{Status: s}).Claimed() {
			t.Fatalf("状态 %q 不应判为已领", s)
		}
	}
}

// TestCampaignsParsesStatusAndBenefit 活动列表要解析出 status 和 benefitAmount。
func TestCampaignsParsesStatusAndBenefit(t *testing.T) {
	body := `{"code":0,"message":"ok","data":{"channel":"WEB","items":[
      {"campaignId":34,"title":"每日签到领100积分","type":"USER_LOGIN","benefitAmount":100,
       "benefitUnit":"CREDIT","claimable":false,"status":"CLAIMED",
       "extra":{"startTime":"2026-10-26T16:00:00Z","endTime":"2026-12-31T15:59:59Z"}}]}}`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(body))
	}))
	defer srv.Close()
	old := APIBase
	APIBase = srv.URL
	defer func() { APIBase = old }()

	acc := &Account{Cookies: []Cookie{ck("devclouddevuibjtcftk", "X")}}
	camps, err := Campaigns(acc)
	if err != nil {
		t.Fatalf("拉活动失败: %v", err)
	}
	if len(camps) != 1 {
		t.Fatalf("应有 1 条活动，实际 %d", len(camps))
	}
	c := camps[0]
	if c.Status != "CLAIMED" || !c.Claimed() {
		t.Fatalf("status 解析错了：%+v", c)
	}
	if c.BenefitAmount != 100 || c.BenefitUnit != "CREDIT" {
		t.Fatalf("benefit 解析错了：%+v", c)
	}
}

// TestKeepaliveAbsorbsSetCookie 保活要能吸收服务端下发的新 Cookie。
func TestKeepaliveAbsorbsSetCookie(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Add("Set-Cookie", "BENSESSCC_TAG=renewed-value; Path=/")
		_, _ = w.Write([]byte(`{"all_credit_package":{"package_credit_remain":5600}}`))
	}))
	defer srv.Close()
	old := APIBase
	APIBase = srv.URL
	defer func() { APIBase = old }()

	acc := &Account{Cookies: []Cookie{ck("devclouddevuibjtcftk", "X"), ck("BENSESSCC_TAG", "old-value")}}
	ok, err := Keepalive(acc)
	if !ok || err != nil {
		t.Fatalf("保活应成功：ok=%v err=%v", ok, err)
	}
	found := false
	for _, c := range acc.Cookies {
		if c.Name == "BENSESSCC_TAG" && c.Value == "renewed-value" {
			found = true
		}
	}
	if !found {
		t.Fatalf("保活必须吸收服务端下发的新 Cookie（同名换值），实际 %+v", acc.Cookies)
	}
}

// TestCookieHeaderAndIdentityDoNotLeak 身份串里不该混进会话票据。
func TestCookieHeaderAndIdentityDoNotLeak(t *testing.T) {
	cs := []Cookie{ck("domain_tag", huDomain), ck("user_tag", huUser), ck("SessionID", "secret")}
	id := IdentityOf(cs)
	if strings.Contains(id, "secret") {
		t.Fatalf("身份串里混进了会话票据：%q", id)
	}
}
