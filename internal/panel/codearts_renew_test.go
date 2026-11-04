package panel

import (
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/codearts"
)

// codearts_renew_test.go 覆盖 C-CA-RENEW@1 的**执行层**（决策本身在
// internal/codearts/renewplan_test.go）。
//
// 这里一律使用注入钩子（codeartsRenewKeepaliveHook / BackfillHook / SSOHook），
// 不打上游、不起浏览器：执行层的行为（什么时候续、失败字段怎么落盘、
// force 的边界、补采失败后的节流）必须能在离线环境里钉死。

// caRenewPoolFile 建一份临时号池并返回路径。
func caRenewPoolFile(t *testing.T, accs ...*codearts.Account) string {
	t.Helper()
	file := filepath.Join(t.TempDir(), "codearts_accounts.json")
	t.Setenv("CODEARTS_POOL_FILE", file)
	if err := codearts.SavePool(&codearts.Pool{Accounts: accs}); err != nil {
		t.Fatalf("写测试号池失败: %v", err)
	}
	return file
}

// caRenewLoad 从落盘号池读回账号（断言"真的写进文件了"）。
func caRenewLoad(t *testing.T, id string) *codearts.Account {
	t.Helper()
	pool, err := codearts.LoadPool()
	if err != nil {
		t.Fatalf("读号池失败: %v", err)
	}
	for _, a := range pool.Accounts {
		if a != nil && a.ID == id {
			return a
		}
	}
	t.Fatalf("号池里找不到账号 %s", id)
	return nil
}

// caRenewResetHooks 清空钩子与"每天一条"的记忆，避免测试互相污染。
func caRenewResetHooks(t *testing.T) {
	t.Helper()
	codeartsRenewKeepaliveHook = nil
	codeartsRenewBackfillHook = nil
	codeartsRenewSSOHook = nil
	codeartsRenewLogf = log.Printf
	codeartsNoteOnce_.Lock()
	codeartsNoteOnce_.seen = map[string]string{}
	codeartsNoteOnce_.Unlock()
	t.Cleanup(func() {
		codeartsRenewKeepaliveHook = nil
		codeartsRenewBackfillHook = nil
		codeartsRenewSSOHook = nil
		codeartsRenewLogf = log.Printf
	})
}

// caRenewExpired 固定"会话已死"的验活结果。
func caRenewExpired(*codearts.Account) (bool, error) {
	return false, codearts.ErrSessionExpired
}

// caRenewAfter 检查 RFC3339 时刻是否落在 want±tol 内。
func caRenewAfter(t *testing.T, got string, want time.Time, tol time.Duration) {
	t.Helper()
	g, err := time.Parse(time.RFC3339, strings.TrimSpace(got))
	if err != nil {
		t.Fatalf("时刻 %q 解析失败: %v", got, err)
	}
	if d := g.Sub(want); d < -tol || d > tol {
		t.Fatalf("时刻 = %s, want ≈%s（差 %s）", g.Format(time.RFC3339), want.Format(time.RFC3339), d)
	}
}

// TestCodeArtsRenewNeedsLoginNotSilent 缺陷一的回归点：
// 补采已判会话死时，必须返回 needs_login 并把结论落盘 —— 旧版在这里
// 只打一条日志就 continue，号池字段一动不动（"静默空转"）。
func TestCodeArtsRenewNeedsLoginNotSilent(t *testing.T) {
	caRenewResetHooks(t)
	acc := &codearts.Account{
		ID: "ca-needslogin", Label: "CodeArts test",
		Cookies:    []codearts.Cookie{caObs("SSOTGC", "v", -1)},
		SavedAt:    time.Now().Add(-2 * time.Hour).Format(time.RFC3339),
		ProbeState: "login_dead",
		ProbeMsg:   "注入后仍停在 auth 域",
		ProbeAt:    time.Now().Add(-time.Hour).Format(time.RFC3339),
	}
	caRenewPoolFile(t, acc)

	ssoCalls := 0
	codeartsRenewKeepaliveHook = caRenewExpired
	codeartsRenewSSOHook = func(*Panel, *codearts.Account) (string, string) {
		ssoCalls++
		return "renewed", "不该被调用"
	}

	now := time.Now()
	p := &Panel{}
	phase, msg := p.codeartsRenewAccount(acc, false)
	if phase != "needs_login" {
		t.Fatalf("phase = %q, want needs_login（msg=%s）", phase, msg)
	}
	if ssoCalls != 0 {
		t.Errorf("login_dead 时不得起浏览器续期，SSO 被调用 %d 次", ssoCalls)
	}
	if !strings.Contains(msg, "人工") {
		t.Errorf("message 必须说清楚要人工登录：%q", msg)
	}

	got := caRenewLoad(t, acc.ID)
	if got.RenewResult != "needs_login" {
		t.Errorf("RenewResult = %q, want needs_login", got.RenewResult)
	}
	// ⚠️ 本轮**没有**真的发起续期（只是判定结论）→ RenewAttemptAt 必须保持为空。
	// 旧实现把"判定"也写成"刚刚试过"，面板于是显示成"刚试过又失败"，
	// 2 小时节流也误判成"刚失败过" —— 用户看到的"续期不工作"就是这么来的。
	if got.RenewAttemptAt != "" {
		t.Errorf("没有真正发起续期时不得写 RenewAttemptAt：%q", got.RenewAttemptAt)
	}
	if got.RenewFailAt == "" {
		t.Errorf("结论必须落盘 RenewFailAt（供面板显示上次结论时刻）")
	}
	if got.RenewFailMsg == "" {
		t.Errorf("RenewFailMsg 不能为空")
	}
	caRenewAfter(t, got.RenewNextAt, now.Add(codearts.ManualRecheck), 2*time.Minute)

	// 再来一轮：结论仍然可见（不是"每次都当没发生过"）。
	phase2, _ := p.codeartsRenewAccount(acc, false)
	if phase2 != "needs_login" {
		t.Fatalf("第二轮 phase = %q, want needs_login", phase2)
	}
	if again := caRenewLoad(t, acc.ID); again.RenewResult != "needs_login" {
		t.Errorf("第二轮后 RenewResult 被改坏: %q", again.RenewResult)
	}
}

// TestCodeArtsRenewThrottledThenForce 2 小时节流 + force 只跳过节流。
func TestCodeArtsRenewThrottledThenForce(t *testing.T) {
	caRenewResetHooks(t)
	now := time.Now()
	acc := &codearts.Account{
		ID: "ca-throttle", Label: "CodeArts test",
		Cookies:        []codearts.Cookie{caObs("SSOTGC", "v", -1)},
		SavedAt:        now.Add(-2 * time.Hour).Format(time.RFC3339),
		TicketSource:   "session",
		ProbeAt:        now.Add(-time.Hour).Format(time.RFC3339),
		RenewAttemptAt: now.Add(-30 * time.Minute).Format(time.RFC3339),
		RenewResult:    "failed",
	}
	caRenewPoolFile(t, acc)

	ssoCalls := 0
	codeartsRenewKeepaliveHook = caRenewExpired
	codeartsRenewSSOHook = func(_ *Panel, a *codearts.Account) (string, string) {
		ssoCalls++
		// 模拟换回新会话：多一条 Cookie，LastOKAt 更新。
		a.Cookies = append(a.Cookies, caObs("SessionID", "new", -1))
		a.LastOKAt = codearts.Now()
		return "renewed", "已换回新会话（Cookie 2 条）"
	}

	p := &Panel{}
	phase, msg := p.codeartsRenewAccount(acc, false)
	if phase != "throttled" {
		t.Fatalf("2 小时内应节流，phase = %q（msg=%s）", phase, msg)
	}
	if ssoCalls != 0 {
		t.Fatalf("节流时不得起浏览器，SSO 被调用 %d 次", ssoCalls)
	}
	if !strings.Contains(msg, "force") {
		t.Errorf("节流提示应告诉用户可用 force：%q", msg)
	}

	// force=true：只跳过第 6 条（节流），正常发起续期。
	phase, msg = p.codeartsRenewAccount(acc, true)
	if phase != "renewed" {
		t.Fatalf("force 应续期成功，phase = %q（msg=%s）", phase, msg)
	}
	if ssoCalls != 1 {
		t.Errorf("SSO 调用次数 = %d, want 1", ssoCalls)
	}

	got := caRenewLoad(t, acc.ID)
	if len(got.Cookies) != 2 {
		t.Errorf("续期后 Cookie 条数 = %d, want 2（新会话必须落盘）", len(got.Cookies))
	}
	if got.RenewResult != "renewed" {
		t.Errorf("RenewResult = %q, want renewed", got.RenewResult)
	}
	if got.RenewFailAt != "" || got.RenewFailMsg != "" {
		t.Errorf("成功后应清掉旧失败结论：at=%q msg=%q", got.RenewFailAt, got.RenewFailMsg)
	}
	caRenewAfter(t, got.RenewNextAt, time.Now().Add(codearts.AliveRecheck), 2*time.Minute)
	if got.LastOKAt == "" {
		t.Errorf("LastOKAt 必须写")
	}
	if got.SavedAt == "" {
		t.Errorf("SavedAt 必须写")
	}
}

// TestCodeArtsRenewForceDoesNotBypassDead 死票据 force 不能硬试；
// 但 needs_login **可以**被 force 越过（手动「立即续期」必须真的试一次）。
//
// 契约变更（2026-11-04）：旧版 force 对 needs_login 也短路，于是手动点
// 「立即续期」386ms 就返回一句"只能重新登录"，什么都没做 —— 用户据此认定
// "手动续期没用"。现在 force 会真的发起一次尝试；死票据（ticket_expired）
// 仍然硬挡，因为硬试只是白起一次浏览器。
func TestCodeArtsRenewForceDoesNotBypassDead(t *testing.T) {
	caRenewResetHooks(t)
	now := time.Now()
	caRenewPoolFile(t,
		&codearts.Account{ID: "ca-force-login", Cookies: []codearts.Cookie{caObs("SSOTGC", "v", -1)},
			ProbeState: "login_dead", ProbeAt: now.Add(-time.Hour).Format(time.RFC3339)},
		&codearts.Account{ID: "ca-force-expired", Cookies: []codearts.Cookie{caObs("SSOTGC", "v", 1000)},
			TicketDeadline: now.Add(-time.Hour).Format(time.RFC3339), TicketSource: "cookie"},
	)

	ssoCalls := 0
	codeartsRenewKeepaliveHook = caRenewExpired
	codeartsRenewSSOHook = func(*Panel, *codearts.Account) (string, string) {
		ssoCalls++
		return "renewed", "不该被调用"
	}
	p := &Panel{}
	pool, _ := codearts.LoadPool()
	for _, a := range pool.Accounts {
		ph, _ := p.codeartsRenewAccount(a, true)
		if a.ID == "ca-force-expired" {
			// 死票据：force 也不许硬试。
			if ph != "ticket_expired" {
				t.Errorf("账号 %s force 后 phase = %q, want ticket_expired", a.ID, ph)
			}
			continue
		}
		// needs_login：force 必须真的发起一次尝试。
		if ph != "renewed" {
			t.Errorf("账号 %s force 后 phase = %q, want renewed（force 必须真的试一次）", a.ID, ph)
		}
	}
	if ssoCalls != 1 {
		t.Fatalf("force 只应越过 needs_login 试一次（死票据不试），SSO 被调用 %d 次", ssoCalls)
	}
	if got := caRenewLoad(t, "ca-force-expired"); got.RenewResult != "ticket_expired" {
		t.Errorf("死票据 RenewResult = %q, want ticket_expired", got.RenewResult)
	}
}

// TestCodeArtsRenewFailureFields 失败字段必须一个不少，且日志每天最多一条。
func TestCodeArtsRenewFailureFields(t *testing.T) {
	caRenewResetHooks(t)
	now := time.Now()
	acc := &codearts.Account{
		ID: "ca-fail", Label: "CodeArts test",
		Cookies:        []codearts.Cookie{caObs("SSOTGC", "v", -1)},
		TicketSource:   "session",
		ProbeAt:        now.Add(-time.Hour).Format(time.RFC3339),
		RenewAttemptAt: now.Add(-3 * time.Hour).Format(time.RFC3339),
	}
	caRenewPoolFile(t, acc)

	var logs []string
	var logMu sync.Mutex
	codeartsRenewLogf = func(format string, args ...any) {
		logMu.Lock()
		defer logMu.Unlock()
		logs = append(logs, format)
	}
	codeartsRenewKeepaliveHook = caRenewExpired
	codeartsRenewSSOHook = func(*Panel, *codearts.Account) (string, string) {
		return "failed", "回到控制台但接口仍不通: HTTP 401"
	}

	p := &Panel{}
	phase, msg := p.codeartsRenewAccount(acc, false)
	if phase != "failed" {
		t.Fatalf("phase = %q, want failed（msg=%s）", phase, msg)
	}

	got := caRenewLoad(t, acc.ID)
	if got.RenewResult != "failed" {
		t.Errorf("RenewResult = %q, want failed", got.RenewResult)
	}
	if got.RenewFailMsg != msg {
		t.Errorf("RenewFailMsg = %q, want 真实原因 %q", got.RenewFailMsg, msg)
	}
	caRenewAfter(t, got.RenewAttemptAt, now, 2*time.Minute)
	caRenewAfter(t, got.RenewFailAt, now, 2*time.Minute)
	caRenewAfter(t, got.RenewNextAt, now.Add(codearts.RenewMinGap), 2*time.Minute)

	// 第二轮（force 绕过节流）同样失败 → 同一账号同一天只允许一条日志。
	phase, _ = p.codeartsRenewAccount(acc, true)
	if phase != "failed" {
		t.Fatalf("第二轮 phase = %q, want failed", phase)
	}
	logMu.Lock()
	n := len(logs)
	logMu.Unlock()
	if n != 1 {
		t.Fatalf("失败日志条数 = %d, want 1（每账号每天最多一条）", n)
	}
}

// TestCodeArtsRenewProbeThenRenew 无票据时间 → 先补采 → 再续期。
//
// 这是旧版最致命的缺口：TicketDeadline 为空时旧版直接 continue，补采代码
// 根本不存在。现在必须真的调一次补采，拿到 session_only 后继续续期。
func TestCodeArtsRenewProbeThenRenew(t *testing.T) {
	caRenewResetHooks(t)
	now := time.Now()
	acc := &codearts.Account{
		ID: "ca-probe", Label: "CodeArts test",
		Cookies: []codearts.Cookie{caObs("SSOTGC", "v", -1)},
		SavedAt: now.Add(-time.Hour).Format(time.RFC3339),
		// TicketDeadline / ProbeAt / ProbeState 全空：旧号池的真实形态。
	}
	caRenewPoolFile(t, acc)

	backfillCalls, ssoCalls := 0, 0
	codeartsRenewKeepaliveHook = caRenewExpired
	codeartsRenewBackfillHook = func(_ *Panel, a *codearts.Account) (*codearts.CaptureResult, error) {
		backfillCalls++
		// 模拟 T2：补采成功并写入号池（ProbeAt/TicketSource/ProbeState）。
		at := now.Format(time.RFC3339)
		if err := codearts.UpdatePool(func(pool *codearts.Pool) error {
			x := codeartsFindAccount(pool, a.ID)
			if x == nil {
				return nil
			}
			x.ProbeAt = at
			x.ProbeState = "session_only"
			x.ProbeMsg = "SSO 票据全为会话 Cookie"
			x.TicketSource = "session"
			x.TicketDeadline = ""
			return nil
		}); err != nil {
			return nil, err
		}
		return &codearts.CaptureResult{
			OK: true, Phase: "session_only", TicketSource: "session",
			Message: "已登录；SSO 票据无绝对过期", Evidence: "SSOTGC=session;expires_known=1/1;session_cookies=1",
		}, nil
	}
	codeartsRenewSSOHook = func(_ *Panel, a *codearts.Account) (string, string) {
		ssoCalls++
		a.Cookies = append(a.Cookies, caObs("SessionID", "new", -1))
		a.LastOKAt = codearts.Now()
		return "renewed", "已换回新会话（Cookie 2 条）"
	}

	p := &Panel{}
	phase, msg := p.codeartsRenewAccount(acc, false)
	if phase != "renewed" {
		t.Fatalf("补采成功后应继续续期，phase = %q（msg=%s）", phase, msg)
	}
	if backfillCalls != 1 {
		t.Errorf("补采调用次数 = %d, want 1（旧版这里是 0，正是永久不续的根因）", backfillCalls)
	}
	if ssoCalls != 1 {
		t.Errorf("续期调用次数 = %d, want 1", ssoCalls)
	}
	got := caRenewLoad(t, acc.ID)
	if got.ProbeState != "session_only" {
		t.Errorf("ProbeState = %q, want session_only", got.ProbeState)
	}
	if got.RenewResult != "renewed" {
		t.Errorf("RenewResult = %q, want renewed", got.RenewResult)
	}
}

// TestCodeArtsRenewProbeLoginDead 补采直接判死 → 不续期，且 24 小时后再看。
func TestCodeArtsRenewProbeLoginDead(t *testing.T) {
	caRenewResetHooks(t)
	now := time.Now()
	acc := &codearts.Account{
		ID: "ca-probe-dead", Cookies: []codearts.Cookie{caObs("SSOTGC", "v", -1)},
		SavedAt: now.Add(-time.Hour).Format(time.RFC3339),
	}
	caRenewPoolFile(t, acc)

	ssoCalls := 0
	codeartsRenewKeepaliveHook = caRenewExpired
	codeartsRenewBackfillHook = func(_ *Panel, a *codearts.Account) (*codearts.CaptureResult, error) {
		at := now.Format(time.RFC3339)
		_ = codearts.UpdatePool(func(pool *codearts.Pool) error {
			x := codeartsFindAccount(pool, a.ID)
			if x == nil {
				return nil
			}
			x.ProbeState = "login_dead"
			x.ProbeMsg = "注入后仍停在 auth.huaweicloud.com"
			x.ProbeAt = at
			x.TicketDeadline = ""
			x.TicketSource = ""
			return nil
		})
		return &codearts.CaptureResult{Phase: "login_dead", Message: "注入后仍停在 auth.huaweicloud.com"}, nil
	}
	codeartsRenewSSOHook = func(*Panel, *codearts.Account) (string, string) {
		ssoCalls++
		return "renewed", "不该被调用"
	}

	p := &Panel{}
	phase, msg := p.codeartsRenewAccount(acc, false)
	if phase != "needs_login" {
		t.Fatalf("phase = %q, want needs_login（msg=%s）", phase, msg)
	}
	if ssoCalls != 0 {
		t.Errorf("补采判死后不得续期，SSO 被调用 %d 次", ssoCalls)
	}
	got := caRenewLoad(t, acc.ID)
	if got.RenewResult != "needs_login" {
		t.Errorf("RenewResult = %q, want needs_login", got.RenewResult)
	}
	caRenewAfter(t, got.RenewNextAt, now.Add(codearts.ManualRecheck), 2*time.Minute)
}

// TestCodeArtsRenewProbeEnvFailure 补采环境失败也必须写 ProbeState=failed，
// 否则 15 分钟巡检会反复重开浏览器（契约 2.2.5 的硬要求）。
func TestCodeArtsRenewProbeEnvFailure(t *testing.T) {
	caRenewResetHooks(t)
	now := time.Now()
	acc := &codearts.Account{
		ID: "ca-probe-err", Cookies: []codearts.Cookie{caObs("SSOTGC", "v", -1)},
		SavedAt: now.Add(-time.Hour).Format(time.RFC3339),
	}
	caRenewPoolFile(t, acc)

	ssoCalls := 0
	codeartsRenewKeepaliveHook = caRenewExpired
	codeartsRenewBackfillHook = func(*Panel, *codearts.Account) (*codearts.CaptureResult, error) {
		return &codearts.CaptureResult{Phase: "error", Message: "找不到 Chrome / Edge"}, errors.New("找不到 Chrome / Edge")
	}
	codeartsRenewSSOHook = func(*Panel, *codearts.Account) (string, string) {
		ssoCalls++
		return "renewed", "不该被调用"
	}

	p := &Panel{}
	phase, msg := p.codeartsRenewAccount(acc, false)
	if phase != "failed" {
		t.Fatalf("phase = %q, want failed（msg=%s）", phase, msg)
	}
	if ssoCalls != 0 {
		t.Errorf("补采失败不得续期，SSO 被调用 %d 次", ssoCalls)
	}
	got := caRenewLoad(t, acc.ID)
	if got.ProbeState != "failed" {
		t.Errorf("ProbeState = %q, want failed（否则 15 分钟重开一次浏览器）", got.ProbeState)
	}
	if !strings.Contains(got.ProbeMsg, "Chrome") {
		t.Errorf("ProbeMsg 应是真实原因：%q", got.ProbeMsg)
	}
	caRenewAfter(t, got.ProbeAt, now, 2*time.Minute)
	caRenewAfter(t, got.RenewAttemptAt, now, 2*time.Minute)
	if got.RenewResult != "failed" {
		t.Errorf("RenewResult = %q, want failed", got.RenewResult)
	}
	caRenewAfter(t, got.RenewNextAt, now.Add(codearts.RenewMinGap), 2*time.Minute)
}

// TestCodeArtsRenewBackoff 巡检跳过的两个来源 + "新登录作废旧结论"。
func TestCodeArtsRenewBackoff(t *testing.T) {
	now := time.Now()
	cases := []struct {
		name string
		acc  *codearts.Account
		want bool
	}{
		{"空账号", nil, true},
		{"2 小时内失败过", &codearts.Account{RenewFailAt: now.Add(-time.Hour).Format(time.RFC3339)}, true},
		{"RenewNextAt 未到", &codearts.Account{RenewNextAt: now.Add(time.Hour).Format(time.RFC3339)}, true},
		{"失败已超 2h 且 NextAt 已过", &codearts.Account{
			RenewFailAt: now.Add(-3 * time.Hour).Format(time.RFC3339),
			RenewNextAt: now.Add(-time.Minute).Format(time.RFC3339),
		}, false},
		{"失败后人工重新登录过", &codearts.Account{
			RenewFailAt: now.Add(-time.Hour).Format(time.RFC3339),
			RenewNextAt: now.Add(23 * time.Hour).Format(time.RFC3339),
			SavedAt:     now.Add(-time.Minute).Format(time.RFC3339),
		}, false},
		{"没有失败记录", &codearts.Account{}, false},
	}
	for _, c := range cases {
		if got := codeartsRenewBackoff(c.acc, now); got != c.want {
			t.Errorf("%s: codeartsRenewBackoff = %v, want %v", c.name, got, c.want)
		}
	}
}

// TestCodeArtsRenewThrottleRejectsSecondCall 连续两次调用：第二次仍在 2 小时
// 节流窗口内（HTTP 层映射为 rejected），且不得改写号池的 RenewAttemptAt。
func TestCodeArtsRenewThrottleRejectsSecondCall(t *testing.T) {
	caRenewResetHooks(t)
	now := time.Now()
	attemptAt := now.Add(-time.Minute).Format(time.RFC3339)
	acc := &codearts.Account{
		ID: "ca-reject", Label: "CodeArts test",
		Cookies:        []codearts.Cookie{caObs("SSOTGC", "v", -1)},
		TicketSource:   "session",
		ProbeAt:        now.Add(-time.Hour).Format(time.RFC3339),
		RenewAttemptAt: attemptAt,
	}
	caRenewPoolFile(t, acc)

	ssoCalls := 0
	codeartsRenewKeepaliveHook = caRenewExpired
	codeartsRenewSSOHook = func(*Panel, *codearts.Account) (string, string) {
		ssoCalls++
		return "renewed", "不该被调用"
	}
	p := &Panel{}
	for i := 1; i <= 2; i++ {
		phase, msg := p.codeartsRenewAccount(acc, false)
		if phase != "throttled" {
			t.Fatalf("第 %d 次 phase = %q, want throttled（msg=%s）", i, phase, msg)
		}
	}
	if ssoCalls != 0 {
		t.Fatalf("节流窗口内不得起浏览器，SSO 被调用 %d 次", ssoCalls)
	}
	got := caRenewLoad(t, acc.ID)
	if got.RenewAttemptAt != attemptAt {
		t.Errorf("节流不应改写 RenewAttemptAt：got %q want %q", got.RenewAttemptAt, attemptAt)
	}
}

// TestCodeArtsRenewHTTPShape 契约 D 的响应形状：throttled → rejected，字段齐全。
func TestCodeArtsRenewHTTPShape(t *testing.T) {
	caRenewResetHooks(t)
	now := time.Now()
	acc := &codearts.Account{
		ID: "ca-http", Label: "CodeArts test",
		Cookies:        []codearts.Cookie{caObs("SSOTGC", "v", -1)},
		TicketSource:   "session",
		ProbeAt:        now.Add(-time.Hour).Format(time.RFC3339),
		RenewAttemptAt: now.Add(-10 * time.Minute).Format(time.RFC3339),
		RenewNextAt:    now.Add(110 * time.Minute).Format(time.RFC3339),
		RenewResult:    "failed",
	}
	caRenewPoolFile(t, acc)
	codeartsRenewKeepaliveHook = caRenewExpired

	p := &Panel{}
	req := httptest.NewRequest("POST", "/panel/api/ide/codearts/renew",
		strings.NewReader(`{"ids":["ca-http"],"force":false}`))
	rec := httptest.NewRecorder()
	p.codeartsRenewHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("HTTP %d, want 200（body=%s）", rec.Code, rec.Body.String())
	}
	var out struct {
		OK      bool             `json:"ok"`
		Results []map[string]any `json:"results"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("响应不是合法 JSON: %v（body=%s）", err, rec.Body.String())
	}
	if !out.OK || len(out.Results) != 1 {
		t.Fatalf("响应形状不对: %s", rec.Body.String())
	}
	r := out.Results[0]
	for _, k := range []string{"id", "phase", "message", "saved_at", "cookie_count",
		"ticket_deadline", "renew_next_at", "renew_result"} {
		if _, ok := r[k]; !ok {
			t.Errorf("结果缺少字段 %q", k)
		}
	}
	if r["phase"] != "rejected" {
		t.Errorf("节流时 phase 应为 rejected（契约 D），实际 %v", r["phase"])
	}
	if r["id"] != "ca-http" {
		t.Errorf("id = %v", r["id"])
	}
}

// TestCodeArtsRenewRealPoolNeedsLogin 真实号池的一次只读续期尝试（默认跳过）。
//
// 运行（先备份号池，结果只写临时副本，不动生产文件）：
//
//	$env:CODEARTS_LIVE_POOL='D:\WorkBuddy\data\ide_accounts\codearts_accounts.json'
//	go test ./internal/panel -run TestCodeArtsRenewRealPoolNeedsLogin -v -count=1
func TestCodeArtsRenewRealPoolNeedsLogin(t *testing.T) {
	src := strings.TrimSpace(os.Getenv("CODEARTS_LIVE_POOL"))
	if src == "" {
		t.Skip("设 CODEARTS_LIVE_POOL=<号池路径> 才跑（会复制到临时目录，不改原文件）")
	}
	raw, err := os.ReadFile(src)
	if err != nil {
		t.Fatalf("读真实号池失败: %v", err)
	}
	dst := filepath.Join(t.TempDir(), "codearts_accounts.json")
	if err := os.WriteFile(dst, raw, 0o600); err != nil {
		t.Fatalf("写临时号池失败: %v", err)
	}
	t.Setenv("CODEARTS_POOL_FILE", dst)

	pool, err := codearts.LoadPool()
	if err != nil {
		t.Fatalf("解析号池失败: %v", err)
	}
	p := &Panel{}
	for _, a := range pool.Accounts {
		if a == nil || len(a.Cookies) == 0 {
			continue
		}
		// 只读不动号池：这里只验"决策 + 结论落盘"，不真起浏览器续期。
		alive, kerr := codearts.Keepalive(codeartsCloneAccount(a))
		d := codearts.DecideRenew(a, time.Now(), alive && kerr == nil,
			codeartsBrowserSession.cmd != nil)
		t.Logf("账号 %s: alive=%v keepalive_err=%v decision={attempt=%v probe=%v reason=%s next=%s}",
			a.ID, alive, kerr, d.Attempt, d.Probe, d.Reason, d.NextAt.Format(time.RFC3339))
		if d.Reason == "needs_login" || d.Reason == "ticket_expired" {
			phase, msg := p.codeartsRenewAccount(a, false)
			t.Logf("账号 %s: phase=%s msg=%s", a.ID, phase, msg)
		}
		after := caRenewLoad(t, a.ID)
		t.Logf("账号 %s 落盘字段: renew_result=%q renew_next_at=%q renew_attempt_at=%q probe_state=%q",
			a.ID, after.RenewResult, after.RenewNextAt, after.RenewAttemptAt, after.ProbeState)

		// 节流证据：同一账号在"会话级票据 + 1 分钟前刚尝试过"的形态下连续两次
		// 调用，两次都必须是 throttled（间隔 <2h），且不得起浏览器。
		// Cookie 与网络都是真实的，只有票据字段是构造的（真实号池没有 session_only 账号）。
		now := time.Now()
		throttleAcc := &codearts.Account{
			ID: a.ID, Label: a.Label,
			Cookies:        append([]codearts.Cookie(nil), a.Cookies...),
			SavedAt:        now.Add(-2 * time.Hour).Format(time.RFC3339),
			TicketSource:   "session",
			ProbeAt:        now.Add(-time.Hour).Format(time.RFC3339),
			RenewAttemptAt: now.Add(-time.Minute).Format(time.RFC3339),
		}
		for i := 1; i <= 2; i++ {
			ph, m := p.codeartsRenewAccount(throttleAcc, false)
			t.Logf("账号 %s 节流第 %d 次: phase=%s msg=%s", a.ID, i, ph, m)
			if ph != "throttled" {
				t.Errorf("节流第 %d 次 phase = %q, want throttled", i, ph)
			}
		}
	}
}

// ── 槽内执行 + 同人分桶（T21） ───────────────────────────────────────────
//
// 这两组约束都是"不能靠人眼看日志"的硬约束：
//   - 续期/补采必须在账号自己的槽 profile 里跑（每次 MkdirTemp = 每次一台新设备）；
//   - 同一个人的多个号每天只打一次上游（否则同人请求互相把额度/风控打满）。

// caRenewArgValue 取启动参数里 --key=value 的值（没有返回空串）。
func caRenewArgValue(args []string, key string) string {
	prefix := key + "="
	for _, arg := range args {
		if strings.HasPrefix(arg, prefix) {
			return strings.TrimPrefix(arg, prefix)
		}
	}
	return ""
}

// caRenewFunctionBody 截取包内源文件里某个顶层函数的函数体。
//
// 用来钉死"这个函数里不能再出现某种写法"（例如续期入口的 os.MkdirTemp）：
// 只断言启动参数还不够 —— 参数对但函数体里另建一个临时目录同样是新设备。
func caRenewFunctionBody(t *testing.T, file, signature string) string {
	t.Helper()
	raw, err := os.ReadFile(file)
	if err != nil {
		t.Fatalf("读 %s 失败: %v", file, err)
	}
	text := string(raw)
	start := strings.Index(text, signature)
	if start < 0 {
		t.Fatalf("%s 里找不到 %q", file, signature)
	}
	rest := text[start:]
	if end := strings.Index(rest, "\nfunc "); end >= 0 {
		rest = rest[:end]
	}
	return rest
}

// caRenewStripLineComments 去掉函数体里的行注释。
//
// 断言"函数里不能再出现某种写法"时必须只看代码：注释里解释"旧实现用
// os.MkdirTemp"恰恰是改造的来由，不该被当成残留代码。
func caRenewStripLineComments(body string) string {
	lines := strings.Split(body, "\n")
	out := make([]string, 0, len(lines))
	for _, line := range lines {
		if i := strings.Index(line, "//"); i >= 0 {
			line = line[:i]
		}
		out = append(out, line)
	}
	return strings.Join(out, "\n")
}

// TestCodeArtsRenewUsesAccountSlotNotTempProfile 续期/补采启动参数必须指到账号槽，
// 且续期入口不得再自建临时 profile。
func TestCodeArtsRenewUsesAccountSlotNotTempProfile(t *testing.T) {
	caRenewResetHooks(t)
	root := t.TempDir()
	t.Setenv("CODEARTS_SLOT_ROOT", root)
	t.Setenv("IDEAUTH_POOL_DIR", t.TempDir())

	acc := &codearts.Account{ID: "ca-slot-renew", Label: "slot renew"}
	acc.EnsureSlot()

	p := &Panel{}
	unlock := acquireCodeartsSlotLock(codeartsSlotLockKey(acc))
	dir, err := p.ensureCodeartsSlotDir(acc)
	unlock()
	if err != nil {
		t.Fatalf("建槽失败: %v", err)
	}
	absRoot, err := filepath.Abs(root)
	if err != nil {
		t.Fatalf("解析槽根失败: %v", err)
	}
	absDir, err := filepath.Abs(dir)
	if err != nil {
		t.Fatalf("解析槽目录失败: %v", err)
	}
	if want, _ := filepath.Abs(acc.SlotProfileDir()); absDir != want {
		t.Errorf("续期 profile = %s, want 该号槽 %s", absDir, want)
	}
	if !strings.HasPrefix(absDir, absRoot+string(os.PathSeparator)) {
		t.Errorf("续期 profile = %s, want 位于槽根 %s 之下", absDir, absRoot)
	}
	// `<槽根>/<SlotDir>/profile`：profile 子目录才是 --user-data-dir。
	if base := filepath.Base(absDir); base != "profile" {
		t.Errorf("profile 目录名 = %q, want profile（槽目录结构 <SlotRoot>/<SlotDir>/profile）", base)
	}
	if info, serr := os.Stat(absDir); serr != nil || !info.IsDir() {
		t.Fatalf("槽 profile 目录不存在: %v", serr)
	}

	// 同一个号再走一次必须是同一个槽（稳定设备身份），且目录仍然在（不能"用完即删"）。
	unlock = acquireCodeartsSlotLock(codeartsSlotLockKey(acc))
	dir2, err2 := p.ensureCodeartsSlotDir(acc)
	unlock()
	if err2 != nil {
		t.Fatalf("第二次建槽失败: %v", err2)
	}
	if dir2 != dir {
		t.Errorf("第二次解析到 %s, want 同一个槽 %s（每换一次就是一台新设备）", dir2, dir)
	}
	if _, serr := os.Stat(dir2); serr != nil {
		t.Errorf("槽目录不应被清理: %v", serr)
	}

	// 续期/补采共用的无头启动参数：--user-data-dir 必须就是这个槽，且带槽指纹。
	args := codeartsHeadlessChromeArgs(9333, dir, acc)
	if got := caRenewArgValue(args, "--user-data-dir"); got != dir {
		t.Errorf("--user-data-dir = %q, want 该号槽 %q", got, dir)
	}
	fp := codeartsFingerprintFor(acc)
	if got, want := caRenewArgValue(args, "--window-size"),
		fmt.Sprintf("%d,%d", fp.Width, fp.Height); got != want {
		t.Errorf("--window-size = %q, want %q（槽指纹，不能每次都不一样）", got, want)
	}
	if got := caRenewArgValue(args, "--lang"); got != codeartsFingerprintLang {
		t.Errorf("--lang = %q, want %q", got, codeartsFingerprintLang)
	}
	if got := caRenewArgValue(args, "--accept-lang"); got != codeartsFingerprintAcceptLanguage {
		t.Errorf("--accept-lang = %q, want %q", got, codeartsFingerprintAcceptLanguage)
	}

	// 无账号（添加账号 / 旧调用点）时参数与改造前一致：不追加任何指纹参数。
	plain := codeartsHeadlessChromeArgs(9333, dir, nil)
	if got := caRenewArgValue(plain, "--lang"); got != "" {
		t.Errorf("无账号时不应带 --lang，实际 %q（会改变普通签到的既有行为）", got)
	}
	if got := caRenewArgValue(plain, "--window-size"); got != "1600,1000" {
		t.Errorf("无账号时 --window-size = %q, want 骨架默认 1600,1000", got)
	}

	// 源码级证据：续期入口不得再自建/删除临时 profile（只看代码，注释除外）。
	renewBody := caRenewStripLineComments(
		caRenewFunctionBody(t, "codearts_auto.go", "func (p *Panel) codeartsSilentRenew("))
	for _, banned := range []string{"MkdirTemp", "ca-renew-", "os.RemoveAll"} {
		if strings.Contains(renewBody, banned) {
			t.Errorf("codeartsSilentRenew 里仍出现 %q：续期不能每次都新建临时 profile", banned)
		}
	}
	for _, required := range []string{"ensureCodeartsSlotDir", "codeartsHeadlessChromeArgs", "caSetCookiesAndFingerprint"} {
		if !strings.Contains(renewBody, required) {
			t.Errorf("codeartsSilentRenew 缺少 %q：续期没有落在账号槽 + 槽指纹上", required)
		}
	}

	// 补采同样必须使用槽 + 指纹（启动参数与 Cookie 注入两个入口）。
	backfillBody := caRenewFunctionBody(t, "codearts_backfill.go", "func (p *Panel) codeartsBackfillAccount(")
	for _, required := range []string{"ensureCodeartsSlotDir", "codeartsHeadlessChromeArgs", "caSetCookiesAndFingerprint"} {
		if !strings.Contains(backfillBody, required) {
			t.Errorf("codeartsBackfillAccount 缺少 %q：补采没有落在账号槽 + 槽指纹上", required)
		}
	}

	// 登录启动也要带账号指纹（重登同一个号 = 同一台设备）。
	loginBody := caRenewFunctionBody(t, "codearts_browser.go", "func (p *Panel) codeartsBrowserStart(")
	if !strings.Contains(loginBody, "codeartsChromeArgsForAccount") {
		t.Error("codeartsBrowserStart 未使用 codeartsChromeArgsForAccount：登录窗口没有按槽指纹启动")
	}
}

// caRenewSamePersonAccount 造一个"有绝对票据、会话已死"的同人账号。
func caRenewSamePersonAccount(id, personKey string, lastOK time.Time) *codearts.Account {
	acc := &codearts.Account{
		ID: id, Label: id, PersonKey: personKey,
		Cookies:        []codearts.Cookie{caObs("SSOTGC", id, -1)},
		SavedAt:        time.Now().Add(-24 * time.Hour).Format(time.RFC3339),
		TicketDeadline: time.Now().Add(24 * time.Hour).Format(time.RFC3339),
		TicketSource:   "absolute",
	}
	if !lastOK.IsZero() {
		acc.LastOKAt = lastOK.Format(time.RFC3339)
	}
	return acc
}

// TestCodeArtsSamePersonRenewOncePerDay 同 PersonKey 的两个号：同一天只允许一个打上游。
//
// 第一个（最久没成功的）续期成功后，第二个当天必须是 skipped_same_person，
// 且再跑一轮巡检也不能调用上游（进程重启后以号池字段为准）。
func TestCodeArtsSamePersonRenewOncePerDay(t *testing.T) {
	caRenewResetHooks(t)
	t.Setenv("IDEAUTH_POOL_DIR", t.TempDir())
	codeartsResetSamePersonDailyFailures()

	now := time.Now()
	person := "person:same-person-test"
	accA := caRenewSamePersonAccount("ca-person-a", person, now.Add(-72*time.Hour))
	accB := caRenewSamePersonAccount("ca-person-b", person, now.Add(-24*time.Hour))
	caRenewPoolFile(t, accA, accB)

	var mu sync.Mutex
	var renewals []string
	codeartsRenewKeepaliveHook = caRenewExpired
	codeartsRenewSSOHook = func(_ *Panel, a *codearts.Account) (string, string) {
		mu.Lock()
		renewals = append(renewals, a.ID)
		mu.Unlock()
		a.LastOKAt = codearts.Now()
		return "renewed", "已换回新会话"
	}

	p := &Panel{}
	p.codeartsSilentRenewOnce()

	mu.Lock()
	got := append([]string(nil), renewals...)
	mu.Unlock()
	if len(got) != 1 || got[0] != "ca-person-a" {
		t.Fatalf("本轮打上游的账号 = %v, want 只有 ca-person-a（最久没成功的那一个）", got)
	}
	if a := caRenewLoad(t, "ca-person-a"); a.RenewResult != "renewed" {
		t.Errorf("ca-person-a RenewResult = %q, want renewed", a.RenewResult)
	}
	if b := caRenewLoad(t, "ca-person-b"); b.RenewAttemptAt != "" || b.RenewResult != "" {
		t.Errorf("ca-person-b 不应被上游打过：RenewAttemptAt=%q RenewResult=%q",
			b.RenewAttemptAt, b.RenewResult)
	}

	// 当日计划必须把同组的另一个号明确标成 skipped_same_person。
	pool, err := codearts.LoadPool()
	if err != nil {
		t.Fatalf("读号池失败: %v", err)
	}
	plan := codeartsSamePersonDailyPlan(pool.Accounts, time.Now())
	if plan["ca-person-b"] != codeartsPersonDaySkippedSamePerson {
		t.Errorf("ca-person-b 当日决策 = %q, want %q",
			plan["ca-person-b"], codeartsPersonDaySkippedSamePerson)
	}
	// A 今天已经成功过 → 当天不再重复打（明天再轮换）。
	if plan["ca-person-a"] != codeartsPersonDaySkippedSamePerson {
		t.Errorf("ca-person-a 当日决策 = %q, want 当天不再重复（已成功）",
			plan["ca-person-a"])
	}

	// 第二轮巡检（模拟 15 分钟后再跑一次）：上游调用次数必须还是 1。
	p.codeartsSilentRenewOnce()
	mu.Lock()
	n := len(renewals)
	mu.Unlock()
	if n != 1 {
		t.Fatalf("第二轮巡检后上游调用次数 = %d, want 1（同人组当日只打一次）", n)
	}
}

// TestCodeArtsSamePersonDifferentKeysUnaffected 不同 PersonKey 的号互不影响。
func TestCodeArtsSamePersonDifferentKeysUnaffected(t *testing.T) {
	caRenewResetHooks(t)
	t.Setenv("IDEAUTH_POOL_DIR", t.TempDir())
	codeartsResetSamePersonDailyFailures()

	accA := caRenewSamePersonAccount("ca-diff-a", "person:diff-a", time.Time{})
	accB := caRenewSamePersonAccount("ca-diff-b", "person:diff-b", time.Time{})
	caRenewPoolFile(t, accA, accB)

	var mu sync.Mutex
	var renewals []string
	codeartsRenewKeepaliveHook = caRenewExpired
	codeartsRenewSSOHook = func(_ *Panel, a *codearts.Account) (string, string) {
		mu.Lock()
		renewals = append(renewals, a.ID)
		mu.Unlock()
		a.LastOKAt = codearts.Now()
		return "renewed", "已换回新会话"
	}

	pool, err := codearts.LoadPool()
	if err != nil {
		t.Fatalf("读号池失败: %v", err)
	}
	plan := codeartsSamePersonDailyPlan(pool.Accounts, time.Now())
	for _, id := range []string{"ca-diff-a", "ca-diff-b"} {
		if plan[id] != codeartsPersonDayExecute {
			t.Errorf("%s 当日决策 = %q, want %q（不同人不能互相影响）",
				id, plan[id], codeartsPersonDayExecute)
		}
	}

	p := &Panel{}
	p.codeartsSilentRenewOnce()
	mu.Lock()
	n := len(renewals)
	mu.Unlock()
	if n != 2 {
		t.Fatalf("不同 PersonKey 的两个号都应各打一次，实际上游调用 %d 次（%v）", n, renewals)
	}
}

// TestCodeArtsSamePersonSkipsAfterFailureToday 同人组今天已失败过一次 → 当日不再重试。
func TestCodeArtsSamePersonSkipsAfterFailureToday(t *testing.T) {
	caRenewResetHooks(t)
	t.Setenv("IDEAUTH_POOL_DIR", t.TempDir())
	codeartsResetSamePersonDailyFailures()

	now := time.Now()
	cst := now.In(codeartsAutoDayLocation)
	sameDay := time.Date(cst.Year(), cst.Month(), cst.Day(), 0, 0, 1, 0, codeartsAutoDayLocation)
	if !sameDay.Before(now) {
		sameDay = now
	}
	person := "person:failed-today"
	accA := caRenewSamePersonAccount("ca-fail-a", person, time.Time{})
	accA.RenewAttemptAt = sameDay.Format(time.RFC3339)
	accA.RenewFailAt = sameDay.Format(time.RFC3339)
	accB := caRenewSamePersonAccount("ca-fail-b", person, time.Time{})
	caRenewPoolFile(t, accA, accB)

	ssoCalls := 0
	codeartsRenewKeepaliveHook = caRenewExpired
	codeartsRenewSSOHook = func(*Panel, *codearts.Account) (string, string) {
		ssoCalls++
		return "renewed", "不应发生"
	}

	pool, err := codearts.LoadPool()
	if err != nil {
		t.Fatalf("读号池失败: %v", err)
	}
	plan := codeartsSamePersonDailyPlan(pool.Accounts, time.Now())
	for _, id := range []string{"ca-fail-a", "ca-fail-b"} {
		if plan[id] != codeartsPersonDaySkippedSamePerson {
			t.Errorf("%s 当日决策 = %q, want %q（组内今天已经失败过一次）",
				id, plan[id], codeartsPersonDaySkippedSamePerson)
		}
	}

	p := &Panel{}
	p.codeartsSilentRenewOnce()
	if ssoCalls != 0 {
		t.Fatalf("同人组今天已失败过，不应再打上游，实际调用 %d 次", ssoCalls)
	}
}
