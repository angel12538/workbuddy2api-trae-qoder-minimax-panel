package codearts

import (
	"testing"
	"time"
)

// caRenewNow 决策表统一用固定的"当前时刻"，避免测试随真实时间漂移。
var caRenewNow = time.Date(2026, 11, 3, 10, 0, 0, 0, time.UTC)

// caRFC 相对 caRenewNow 生成 RFC3339 字符串（负数表示过去）。
func caRFC(offset time.Duration) string {
	return caRenewNow.Add(offset).Format(time.RFC3339)
}

// TestDecideRenewBranches C-CA-RENEW@1 七条判定分支（顺序即契约）。
//
// 每条用例都同时钉住三件事：要不要发起续期（Attempt）、要不要先补采（Probe）、
// 下一次检查时刻（NextAt）——只看 Reason 是不够的，旧版缺陷正是"Reason 对了
// 但 Attempt 永远 false"。
func TestDecideRenewBranches(t *testing.T) {
	cases := []struct {
		name         string
		acc          *Account
		sessionAlive bool
		busy         bool
		wantAttempt  bool
		wantProbe    bool
		wantReason   string
		wantNext     time.Duration
	}{
		{
			name:        "1-busy 优先于一切",
			acc:         &Account{ProbeState: "login_dead"},
			busy:        true,
			wantReason:  "busy",
			wantNext:    AliveRecheck,
			wantAttempt: false,
			wantProbe:   false,
		},
		{
			name:         "2-会话活着不动",
			acc:          &Account{TicketDeadline: caRFC(-time.Hour)},
			sessionAlive: true,
			wantReason:   "alive",
			wantNext:     AliveRecheck,
		},
		{
			name:        "3-补采已判会话死(login_dead)",
			acc:         &Account{ProbeState: "login_dead", ProbeAt: caRFC(-time.Hour)},
			wantReason:  "needs_login",
			wantNext:    ManualRecheck,
			wantAttempt: false,
		},
		{
			name:        "3-上次续期停在人工页(needs_login)",
			acc:         &Account{RenewResult: "needs_login"},
			wantReason:  "needs_login",
			wantNext:    ManualRecheck,
			wantAttempt: false,
		},
		{
			name:        "4-绝对票据已过期",
			acc:         &Account{TicketDeadline: caRFC(-time.Minute)},
			wantReason:  "ticket_expired",
			wantNext:    ManualRecheck,
			wantAttempt: false,
		},
		{
			name:        "5-无票据时间且没补采过→先补采",
			acc:         &Account{},
			wantReason:  "probe_required",
			wantNext:    ProbeGap,
			wantAttempt: true,
			wantProbe:   true,
		},
		{
			name:        "5-无票据时间且补采已过 6h→再补采",
			acc:         &Account{ProbeAt: caRFC(-7 * time.Hour)},
			wantReason:  "probe_required",
			wantNext:    ProbeGap,
			wantAttempt: true,
			wantProbe:   true,
		},
		{
			name:        "6-2 小时内试过→节流",
			acc:         &Account{ProbeAt: caRFC(-10 * time.Minute), TicketSource: "session", RenewAttemptAt: caRFC(-30 * time.Minute)},
			wantReason:  "throttled",
			wantNext:    90 * time.Minute, // RenewAttemptAt + 2h
			wantAttempt: false,
		},
		{
			name:        "7-会话级票据且已过 2h→正常续期",
			acc:         &Account{ProbeAt: caRFC(-10 * time.Minute), TicketSource: "session", RenewAttemptAt: caRFC(-3 * time.Hour)},
			wantReason:  "renew_due",
			wantNext:    AliveRecheck,
			wantAttempt: true,
		},
		{
			name:        "7-从来没有尝试过→正常续期",
			acc:         &Account{ProbeAt: caRFC(-time.Hour), TicketSource: "session"},
			wantReason:  "renew_due",
			wantNext:    AliveRecheck,
			wantAttempt: true,
		},
		{
			name:        "7-绝对票据仍有效→正常续期",
			acc:         &Account{TicketDeadline: caRFC(48 * time.Hour), TicketSource: "cookie"},
			wantReason:  "renew_due",
			wantNext:    AliveRecheck,
			wantAttempt: true,
		},
		{
			name:        "防御-nil 账号按 busy 不执行",
			acc:         nil,
			wantReason:  "busy",
			wantNext:    AliveRecheck,
			wantAttempt: false,
		},
		{
			name:        "防御-TicketDeadline 解析失败按未知继续续期",
			acc:         &Account{TicketDeadline: "not-a-time", ProbeAt: caRFC(-time.Hour)},
			wantReason:  "renew_due",
			wantNext:    AliveRecheck,
			wantAttempt: true,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			d := DecideRenew(c.acc, caRenewNow, c.sessionAlive, c.busy)
			if d.Reason != c.wantReason {
				t.Fatalf("Reason = %q, want %q", d.Reason, c.wantReason)
			}
			if d.Attempt != c.wantAttempt {
				t.Errorf("Attempt = %v, want %v（旧缺陷就是这里永远 false）", d.Attempt, c.wantAttempt)
			}
			if d.Probe != c.wantProbe {
				t.Errorf("Probe = %v, want %v", d.Probe, c.wantProbe)
			}
			if want := caRenewNow.Add(c.wantNext); !d.NextAt.Equal(want) {
				t.Errorf("NextAt = %s, want %s", d.NextAt.Format(time.RFC3339), want.Format(time.RFC3339))
			}
		})
	}
}

// TestDecideRenewOrder 顺序断言：高优先级分支必须压过低优先级，
// 否则"会话活着"会被 needs_login 抢先、"票据过期"会被节流抢先。
func TestDecideRenewOrder(t *testing.T) {
	acc := &Account{
		ProbeState:     "login_dead",        // 第 3 条
		TicketDeadline: caRFC(-time.Hour),   // 第 4 条
		ProbeAt:        caRFC(-time.Hour),   // 第 5 条（不命中）
		RenewAttemptAt: caRFC(-time.Minute), // 第 6 条
	}
	if d := DecideRenew(acc, caRenewNow, true, false); d.Reason != "alive" {
		t.Errorf("会话活着应压过 needs_login：%q", d.Reason)
	}
	if d := DecideRenew(acc, caRenewNow, false, true); d.Reason != "busy" {
		t.Errorf("busy 应压过一切：%q", d.Reason)
	}
	if d := DecideRenew(acc, caRenewNow, false, false); d.Reason != "needs_login" {
		t.Errorf("needs_login 应压过 ticket_expired/throttled：%q", d.Reason)
	}
	// 去掉 login_dead 后应命中 ticket_expired（而不是被 2h 节流抢走）。
	acc.ProbeState = "ok"
	if d := DecideRenew(acc, caRenewNow, false, false); d.Reason != "ticket_expired" {
		t.Errorf("ticket_expired 应压过节流：%q", d.Reason)
	}
	// 票据时间未知但 6h 内补采过 → 走节流，而不是重复补采。
	acc.TicketDeadline = ""
	if d := DecideRenew(acc, caRenewNow, false, false); d.Reason != "throttled" {
		t.Errorf("补采刚做过应走节流：%q", d.Reason)
	}
}

// TestForceRenewDecision force 越过 2 小时节流**和** needs_login，真票据过期不硬试。
func TestForceRenewDecision(t *testing.T) {
	throttled := RenewDecision{Attempt: false, Reason: "throttled", NextAt: caRenewNow.Add(90 * time.Minute)}
	d := ForceRenewDecision(throttled, caRenewNow)
	if !d.Attempt || d.Probe || d.Reason != "renew_due" {
		t.Fatalf("force 应把节流升级为续期：%+v", d)
	}
	if want := caRenewNow.Add(AliveRecheck); !d.NextAt.Equal(want) {
		t.Errorf("force 后 NextAt = %s, want %s", d.NextAt, want)
	}
	// needs_login：手动「立即续期」必须**真的试一次**，不能 386ms 就甩一句
	// "只能重新登录"（那正是「手动续期没用」的根因）。
	nd := RenewDecision{Attempt: false, Reason: "needs_login", NextAt: caRenewNow.Add(ManualRecheck)}
	out := ForceRenewDecision(nd, caRenewNow)
	if !out.Attempt || out.Probe || out.Reason != "renew_due" {
		t.Errorf("force 应越过 needs_login 真正发起一次尝试：%+v", out)
	}
	// 其余分支：force 一律不得改写。
	for _, reason := range []string{"ticket_expired", "busy", "alive", "probe_required"} {
		in := RenewDecision{Attempt: false, Probe: reason == "probe_required", Reason: reason, NextAt: caRenewNow.Add(time.Hour)}
		out := ForceRenewDecision(in, caRenewNow)
		if out != in {
			t.Errorf("force 不得改写 %q 分支：got %+v", reason, out)
		}
	}
}

// TestDecideRenewFreshLogin 重新登录后，旧的 needs_login 结论必须作废。
//
// 实测 2026-11-04：用户明明重新登录成功了，面板还是显示"只能重新登录"，
// 手动「立即续期」也秒回 needs_login —— 账号被永久锁死。
func TestDecideRenewFreshLogin(t *testing.T) {
	// 上一次续期结论（needs_login）发生在 3 小时前（已在 2 小时节流窗口之外）；
	// 用户随后重新登录（SavedAt 更新）。
	acc := &Account{
		RenewResult:    "needs_login",
		RenewAttemptAt: caRFC(-3 * time.Hour),
		RenewFailAt:   caRFC(-3 * time.Hour),
		SavedAt:        caRFC(-10 * time.Minute),
		TicketDeadline: caRFC(48 * time.Hour),
		TicketSource:   "cookie",
	}
	if !acc.HasFreshLogin() {
		t.Fatal("SavedAt 晚于结论时刻时应判定为刚登录过")
	}
	d := DecideRenew(acc, caRenewNow, false, false)
	if d.Reason == "needs_login" {
		t.Errorf("重新登录后不应再被 needs_login 挡住：%+v", d)
	}
	if !d.Attempt {
		t.Errorf("重新登录后应允许续期：%+v", d)
	}
	// 反例：登录发生在结论**之前**（登录后又被判死）→ needs_login 仍成立。
	acc.SavedAt = caRFC(-4 * time.Hour)
	if acc.HasFreshLogin() {
		t.Error("结论比登录新时不应判定为刚登录过")
	}
	if d := DecideRenew(acc, caRenewNow, false, false); d.Reason != "needs_login" {
		t.Errorf("没有新证据时 needs_login 仍应成立：%q", d.Reason)
	}
}

// TestDecideRenewConstants 常量值被面板/前端文案引用，钉死防止漂移。
func TestDecideRenewConstants(t *testing.T) {
	if ProbeGap != 6*time.Hour || RenewMinGap != 2*time.Hour ||
		AliveRecheck != 15*time.Minute || ManualRecheck != 24*time.Hour {
		t.Fatalf("续期常量被改动：ProbeGap=%s RenewMinGap=%s AliveRecheck=%s ManualRecheck=%s",
			ProbeGap, RenewMinGap, AliveRecheck, ManualRecheck)
	}
}
