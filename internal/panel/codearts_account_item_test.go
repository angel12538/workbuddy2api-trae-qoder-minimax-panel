package panel

import (
	"errors"
	"fmt"
	"path/filepath"
	"testing"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/codearts"
)

// caItemFields C-CA-API@1 A 规定的账号行字段（少一个前端就会显示成"未实现"）。
var caItemFields = []string{
	"id", "label", "nickname", "saved_at", "cookie_count",
	"checked_in_today", "login_expired", "credits", "daily_bonus", "benefit_amount",
	"ticket_deadline", "ticket_source", "session_expires_at", "last_ok_at",
	"renew_next_at", "renew_result", "renew_fail_at", "renew_fail_msg",
	"probe_state", "probe_msg", "probe_at",
	"expires_known_count", "session_cookie_count",
	"can_auto_renew", "needs_manual",
}

// TestCodeartsAccountItemBaseFields 账号项必须返回契约里的**全部**字段。
func TestCodeartsAccountItemBaseFields(t *testing.T) {
	t.Setenv("IDEAUTH_POOL_DIR", t.TempDir())
	t.Setenv("CODEARTS_POOL_FILE", filepath.Join(t.TempDir(), "pool.json"))

	acc := &codearts.Account{
		ID: "ca1d671920b4", Label: "CodeArts huvj421wxe",
		SavedAt:        "2026-11-02T18:15:08+08:00",
		LastOKAt:       "2026-11-02T14:30:00+08:00",
		TicketSource:   "session",
		RenewNextAt:    "2026-11-03T10:00:00+08:00",
		RenewResult:    "failed",
		RenewFailAt:    "2026-11-03T09:00:00+08:00",
		RenewFailMsg:   "票据换不回会话",
		ProbeState:     "session_only",
		ProbeMsg:       "SSOTGC 等全部为会话 Cookie",
		ProbeAt:        "2026-11-03T09:00:00+08:00",
		Cookies:        []codearts.Cookie{caObs("SSOTGC", "a", -1), caObs("SSOJTC", "b", -1), caUnknown("cbc-sid", "c")},
	}
	item := codeartsAccountItemBase(acc)

	for _, k := range caItemFields {
		if _, ok := item[k]; !ok {
			t.Errorf("账号项缺少字段 %q", k)
		}
	}
	if item["id"] != "ca1d671920b4" {
		t.Errorf("id 不对: %v", item["id"])
	}
	if item["cookie_count"] != 3 {
		t.Errorf("cookie_count 应为 3，实际 %v", item["cookie_count"])
	}
	if item["ticket_source"] != "session" {
		t.Errorf("ticket_source 应为 session，实际 %v", item["ticket_source"])
	}
	if item["expires_known_count"] != 2 {
		t.Errorf("expires_known_count 应为 2，实际 %v", item["expires_known_count"])
	}
	if item["session_cookie_count"] != 2 {
		t.Errorf("session_cookie_count 应为 2，实际 %v", item["session_cookie_count"])
	}
	// ticket_deadline 为空（会话级）时仍允许续期。
	if item["can_auto_renew"] != true {
		t.Errorf("会话级票据应 can_auto_renew=true，实际 %v", item["can_auto_renew"])
	}
	if item["needs_manual"] != false {
		t.Errorf("会话级票据不应需人工，实际 %v", item["needs_manual"])
	}
	// 空值也必须在（credits/daily_bonus/login_expired 的默认值）。
	if item["credits"] != 0.0 || item["daily_bonus"] != 0.0 || item["login_expired"] != false {
		t.Errorf("默认值不对: credits=%v daily=%v expired=%v",
			item["credits"], item["daily_bonus"], item["login_expired"])
	}
}

// TestCodeartsAccountItemBaseSparse 全新/空账号也不会少字段（前端按字段名渲染）。
func TestCodeartsAccountItemBaseSparse(t *testing.T) {
	t.Setenv("IDEAUTH_POOL_DIR", t.TempDir())

	acc := &codearts.Account{ID: "x"}
	item := codeartsAccountItemBase(acc)
	for _, k := range caItemFields {
		if _, ok := item[k]; !ok {
			t.Errorf("空账号缺少字段 %q", k)
		}
	}
	if item["ticket_deadline"] != "" || item["ticket_source"] != "" {
		t.Errorf("空账号票据字段应为空串: %v / %v", item["ticket_deadline"], item["ticket_source"])
	}
	if item["session_expires_at"] != "" {
		t.Errorf("没有 last_ok_at 时会话估算应为空，实际 %v", item["session_expires_at"])
	}
	if item["can_auto_renew"] != true {
		t.Errorf("无票据时间也应允许续期（R1.1.3），实际 %v", item["can_auto_renew"])
	}
}

// TestCodeartsNeedsManual needs_manual 的三个来源（C-CA-API@1 A）。
func TestCodeartsNeedsManual(t *testing.T) {
	future := time.Now().Add(24 * time.Hour).Format(time.RFC3339)
	past := time.Now().Add(-24 * time.Hour).Format(time.RFC3339)

	cases := []struct {
		name string
		a    *codearts.Account
		want bool
	}{
		{"补采确认会话已死", &codearts.Account{ProbeState: "login_dead"}, true},
		{"续期停在登录页", &codearts.Account{RenewResult: "needs_login"}, true},
		{"票据已过期", &codearts.Account{TicketDeadline: past}, true},
		{"票据未过期", &codearts.Account{TicketDeadline: future}, false},
		{"会话级无截止时间", &codearts.Account{TicketSource: "session"}, false},
		{"时间解析失败按未知处理", &codearts.Account{TicketDeadline: "not-a-time"}, false},
		{"探测成功", &codearts.Account{ProbeState: "ok"}, false},
		{"空账号", &codearts.Account{}, false},
		{"nil", nil, false},
	}
	for _, c := range cases {
		if got := codeartsNeedsManual(c.a); got != c.want {
			t.Errorf("%s: needs_manual = %v, want %v", c.name, got, c.want)
		}
	}
}

// TestCodeartsSelectAccounts ids 匹配要认历史别名（MergePool 后的旧 ID）。
func TestCodeartsSelectAccounts(t *testing.T) {
	pool := &codearts.Pool{Accounts: []*codearts.Account{
		{ID: "a1", Aliases: []string{"old-a1"}},
		{ID: "b2"},
		nil,
	}}

	// 空 ids = 全部（跳过 nil）
	if got := codeartsSelectAccounts(pool, nil); len(got) != 2 {
		t.Fatalf("空 ids 应返回全部 2 个，实际 %d", len(got))
	}
	// 用别名也能选中
	if got := codeartsSelectAccounts(pool, []string{"old-a1"}); len(got) != 1 || got[0].ID != "a1" {
		t.Fatalf("别名未命中: %+v", got)
	}
	// 不存在的 id → 不返回
	if got := codeartsSelectAccounts(pool, []string{"nope"}); len(got) != 0 {
		t.Fatalf("不存在的 id 不应命中，实际 %d", len(got))
	}
}
// TestCodeartsKeepalivePhase 验活的四个 phase 与"网络错误不得报 expired"。
func TestCodeartsKeepalivePhase(t *testing.T) {
	cases := []struct {
		name      string
		ok        bool
		err       error
		wantPhase string
	}{
		{"成功", true, nil, "alive"},
		{"会话失效", false, codearts.ErrSessionExpired, "expired"},
		{"会话失效（包裹）", false, fmt.Errorf("%w（HTTP 401）", codearts.ErrSessionExpired), "expired"},
		{"DNS 失败", false, errors.New("dial tcp: lookup devcloud.cn-north-4.huaweicloud.com: no such host"), "network"},
		{"连接被拒", false, errors.New("dial tcp 127.0.0.1:443: connectex: connection refused"), "network"},
		{"其它错误", false, errors.New("unexpected EOF"), "error"},
		{"非 200 且无错误", false, nil, "error"},
	}
	for _, c := range cases {
		phase, msg := codeartsKeepalivePhase(c.ok, c.err)
		if phase != c.wantPhase {
			t.Errorf("%s: phase = %q, want %q (msg=%q)", c.name, phase, c.wantPhase, msg)
		}
		if c.wantPhase != "alive" && msg == "" {
			t.Errorf("%s: 非成功时必须带原因", c.name)
		}
	}
}

// TestCodeartsKeepalivePhaseNilErrNotExpired nil 错误 + ok=false 不得报 expired。
func TestCodeartsKeepalivePhaseNilErrNotExpired(t *testing.T) {
	if phase, _ := codeartsKeepalivePhase(false, nil); phase == "expired" {
		t.Fatal("没有错误信息时不得报 expired")
	}
}
