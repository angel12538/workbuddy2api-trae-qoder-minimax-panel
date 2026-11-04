package panel

import (
	"testing"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/codearts"
)

// caObs 造一条"已观测"的 Cookie（走 C-CA-COOKIE@1 的唯一归一入口）。
func caObs(name, value string, cdpExpires float64) codearts.Cookie {
	return codearts.NewObservedCookie(name, value, ".huaweicloud.com", "/", cdpExpires)
}

// caUnknown 造一条"没观测过过期信息"的 Cookie（旧号池/粘贴导入的形态）。
func caUnknown(name, value string) codearts.Cookie {
	return codearts.Cookie{Name: name, Value: value, Domain: ".huaweicloud.com"}
}

// TestCodeartsEvidenceOfFormat Evidence 是面板/日志核对票据状态的唯一凭据，
// 格式必须固定（C-CA-CAPTURE@1）：
//
//	SSOTGC=session;SSOJTC=1767225600;CASTGCLOW=unknown;expires_known=37/55;session_cookies=14
func TestCodeartsEvidenceOfFormat(t *testing.T) {
	cs := []codearts.Cookie{
		caObs("SSOTGC", "v1", -1),        // 会话级
		caObs("SSOJTC", "v2", 1767225600), // 绝对过期
		caObs("SessionID", "v3", -1),     // 会话级（非 SSO 名单，只计入末尾统计）
		caUnknown("CASTGCLOW", "v4"),     // 未知
	}
	got := codeartsEvidenceOf(cs)
	want := "SSOTGC=session;SSOJTC=1767225600;CASTGCLOW=unknown;hwid_cas_sid=unknown;" +
		"cbc-sid=unknown;expires_known=3/4;session_cookies=2"
	if got != want {
		t.Fatalf("Evidence 格式不对：\n got=%q\nwant=%q", got, want)
	}
}

// TestCodeartsEvidenceTicketState 同一张票据跨多个 domain/path 时：
// 有绝对过期就报**最早**那个（决定何时必须续期），没有才退回 session/unknown。
func TestCodeartsEvidenceTicketState(t *testing.T) {
	cs := []codearts.Cookie{
		caObs("SSOTGC", "a", -1),
		caObs("cbc-sid", "b", 1767225600),
		caObs("cbc-sid", "c", 1767000000), // 更早 → 应该被选中
	}
	got := codeartsEvidenceOf(cs)
	if !contains(got, "SSOTGC=session") {
		t.Errorf("会话级票据应报 session：%s", got)
	}
	if !contains(got, "cbc-sid=1767000000") {
		t.Errorf("同名多域应报最早时刻：%s", got)
	}
	if !contains(got, "expires_known=3/3") {
		t.Errorf("已知条数应为 3/3：%s", got)
	}
}

// TestCodeartsCaptureResultFromCookies 登录保存与补采共用的判定入口。
func TestCodeartsCaptureResultFromCookies(t *testing.T) {
	// ① 一条都没采到
	if res := codeartsCaptureResultFromCookies(nil); res.Phase != "no_cookie" || res.OK {
		t.Fatalf("空集合应判 no_cookie，实际 phase=%s ok=%v", res.Phase, res.OK)
	}

	// ② 全是会话 Cookie → session_only，且不允许编造 deadline
	allSession := []codearts.Cookie{
		caObs("SSOTGC", "a", -1), caObs("SSOJTC", "b", -1),
	}
	res := codeartsCaptureResultFromCookies(allSession)
	if res.Phase != "session_only" || !res.OK {
		t.Fatalf("全会话级应判 session_only，实际 phase=%s", res.Phase)
	}
	if res.TicketSource != "session" {
		t.Fatalf("ticket_source 应为 session，实际 %q", res.TicketSource)
	}
	if res.TicketDeadline != "" {
		t.Fatalf("会话级票据不得有 ticket_deadline，实际 %q", res.TicketDeadline)
	}
	if res.SessionCookieCount != 2 || res.ExpiresKnownCount != 2 {
		t.Fatalf("计数不对：session=%d known=%d", res.SessionCookieCount, res.ExpiresKnownCount)
	}

	// ③ 有绝对票据 → probed，deadline 取最早
	withAbs := []codearts.Cookie{
		caObs("SSOTGC", "a", -1),
		caObs("SSOJTC", "b", 1767225600),
		caObs("hwid_cas_sid", "c", 1767000000),
	}
	res = codeartsCaptureResultFromCookies(withAbs)
	if res.Phase != "probed" || res.TicketSource != "cookie" {
		t.Fatalf("有绝对票据应判 probed/cookie，实际 %s/%s", res.Phase, res.TicketSource)
	}
	want := time.Unix(1767000000, 0).Format(time.RFC3339)
	if res.TicketDeadline != want {
		t.Fatalf("ticket_deadline 应取最早时刻 %q，实际 %q", want, res.TicketDeadline)
	}
	if res.Evidence == "" {
		t.Fatal("Evidence 不能为空")
	}
}

// TestCodeartsProbeStateOf phase → ProbeState 的固定映射。
func TestCodeartsProbeStateOf(t *testing.T) {
	cases := map[string]string{
		"probed":       "ok",
		"session_only": "session_only",
		"login_dead":   "login_dead",
		"no_cookie":    "failed",
		"error":        "failed",
		"":             "",
	}
	for in, want := range cases {
		if got := codeartsProbeStateOf(in); got != want {
			t.Errorf("codeartsProbeStateOf(%q) = %q, want %q", in, got, want)
		}
	}
}

// TestCodeartsProbeThrottled 6 小时节流；失败（login_dead/failed）后 2 小时。
func TestCodeartsProbeThrottled(t *testing.T) {
	now := time.Date(2026, 11, 3, 10, 0, 0, 0, time.Local)
	at := now.Add(-1 * time.Hour).Format(time.RFC3339)

	// 成功探测后 1 小时：仍在 6 小时窗口内
	a := &codearts.Account{ProbeAt: at, ProbeState: "session_only"}
	throttled, next := codeartsProbeThrottled(a, now)
	if !throttled {
		t.Fatal("成功探测后 1 小时应被节流")
	}
	if want := now.Add(5 * time.Hour); !next.Equal(want) {
		t.Fatalf("下次可探测时刻应为 %s，实际 %s", want, next)
	}

	// 成功探测后 6 小时零 1 秒：放行
	if ok, _ := codeartsProbeThrottled(a, now.Add(6*time.Hour+time.Second)); ok {
		t.Fatal("超过 6 小时不应再节流")
	}

	// 失败（login_dead）后 3 小时：已过 2 小时窗口 → 放行
	dead := &codearts.Account{ProbeAt: now.Add(-3 * time.Hour).Format(time.RFC3339), ProbeState: "login_dead"}
	if ok, _ := codeartsProbeThrottled(dead, now); ok {
		t.Fatal("失败后超过 2 小时不应再节流")
	}

	// 失败后 1 小时：仍在 2 小时窗口内
	dead1h := &codearts.Account{ProbeAt: now.Add(-1 * time.Hour).Format(time.RFC3339), ProbeState: "login_dead"}
	if ok, _ := codeartsProbeThrottled(dead1h, now); !ok {
		t.Fatal("失败后 1 小时应被节流")
	}

	// ProbeAt 缺失 / 解析失败 → 不节流（宁可多采一次，也不能永远不再采集）
	if ok, _ := codeartsProbeThrottled(&codearts.Account{}, now); ok {
		t.Fatal("没有 ProbeAt 不应节流")
	}
	if ok, _ := codeartsProbeThrottled(&codearts.Account{ProbeAt: "not-a-time"}, now); ok {
		t.Fatal("ProbeAt 解析失败不应节流")
	}
}

// TestCodeartsShouldTakeCaptureCookies 补采覆盖旧快照的判定（契约澄清：以采集
// 时刻为准，数量不作为依据）。
func TestCodeartsShouldTakeCaptureCookies(t *testing.T) {
	now := time.Date(2026, 11, 3, 10, 0, 0, 0, time.Local)
	old := []codearts.Cookie{caObs("a", "1", -1), caObs("b", "2", -1)}

	// 旧快照更早（本次是新观测）→ 覆盖，即使条数更少
	accOld := &codearts.Account{SavedAt: now.Add(-24 * time.Hour).Format(time.RFC3339), Cookies: old}
	if !codeartsShouldTakeCaptureCookies(accOld, []codearts.Cookie{caObs("a", "1", -1)}, now) {
		t.Fatal("旧快照更早时，本次观测应覆盖（与条数无关）")
	}

	// 旧快照比本次采集更新（并发登录/续期刚写过）→ 不覆盖，即使条数更多
	accNew := &codearts.Account{SavedAt: now.Add(5 * time.Minute).Format(time.RFC3339), Cookies: old}
	more := []codearts.Cookie{caObs("a", "1", -1), caObs("b", "2", -1), caObs("c", "3", -1)}
	if codeartsShouldTakeCaptureCookies(accNew, more, now) {
		t.Fatal("旧快照更新时，条数更多也不得覆盖")
	}

	// SavedAt 相同（同一时刻采集）→ 覆盖
	accSame := &codearts.Account{SavedAt: now.Format(time.RFC3339), Cookies: old}
	if !codeartsShouldTakeCaptureCookies(accSame, []codearts.Cookie{caObs("a", "1", -1)}, now) {
		t.Fatal("同一时刻应视为本次更准确")
	}

	// SavedAt 缺失/不可解析 → 无从比较，以本次为准
	if !codeartsShouldTakeCaptureCookies(&codearts.Account{}, []codearts.Cookie{caObs("a", "1", -1)}, now) {
		t.Fatal("旧 SavedAt 缺失时应以本次采集为准")
	}

	// 空快照永远不覆盖
	if codeartsShouldTakeCaptureCookies(accOld, nil, now) {
		t.Fatal("空快照不得覆盖")
	}
}

// TestCodeartsCaptureFromPortInvalidPort 端口无效时必须 Phase=error 且带错误
// （调用方据此"不改写号池"）。
func TestCodeartsCaptureFromPortInvalidPort(t *testing.T) {
	p := &Panel{}
	res, err := p.codeartsCaptureFromPort(0)
	if err == nil {
		t.Fatal("无效端口应返回错误")
	}
	if res == nil || res.Phase != "error" {
		t.Fatalf("无效端口应 Phase=error，实际 %+v", res)
	}
	if res.Message == "" {
		t.Fatal("Phase=error 时必须带原始错误文本")
	}
}

// TestCodeartsBackfillAccountNoCookie 没有 Cookie 的账号不起浏览器、不写号池。
func TestCodeartsBackfillAccountNoCookie(t *testing.T) {
	p := &Panel{}
	res, err := p.codeartsBackfillAccount(&codearts.Account{ID: "x"})
	if err != nil {
		t.Fatalf("no_cookie 不应返回错误: %v", err)
	}
	if res.Phase != "no_cookie" {
		t.Fatalf("应判 no_cookie，实际 %s", res.Phase)
	}
}

// 说明：codeartsBackfillAccount 的真实浏览器路径（注入 → 打开 SSO 链 → 读
// Cookie）无法离线验证，这里跳过而不是造假。真实补采取证见交付报告。
func TestCodeartsBackfillBrowserSkipped(t *testing.T) {
	t.Skip("浏览器补采依赖真实 Chrome 与已登录会话，离线单测不跑；真实证据由 codeartsBackfillAccount 实跑记录")
}