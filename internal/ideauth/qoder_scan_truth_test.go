package ideauth

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// 回归：Qoder 号池「今天到底签没签」的显示口径。
//
// 2026-10-24 实测踩坑（用户报的"两个号显示反了"）——三个缺陷叠在一起：
//
//  1. 前端读 checked_in_today，后端字段却叫 checked_in_local → 取不到值恒为
//     false → 真正领到 100 Credits 的号显示"可签到"；
//  2. 前端把顶层 checked_in_today（"今天整体没有可领的了"的**聚合值**）当成
//     当前登录号的状态 → 浏览器加的号显示"今日已签到"；
//  3. 后端 current.checked_in_today 用了 CheckedInToday（只比自然日），而本地
//     日志里残留着旧版写的「纯日期」记录 → 那种记录按当日 00:00 解析，
//     "今天签过"为真 → 没签到的号被说成签过。
//
// 真值必须**以上游活动列表为准**：phase=claimed 才是真签到。本地记录只在
// 「可信」（今日 10:05 之后写入的完整时间戳）时用于兜底。

// fakeQoderUpstream 假的活动列表服务：按 Authorization 里的 token 决定返回什么。
func fakeQoderUpstream(t *testing.T, campaignsByToken map[string]string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		tok := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		body, ok := campaignsByToken[tok]
		if !ok {
			body = `{"campaigns":[]}`
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	return srv
}

// todayCampaignJSON 造一份"今天已领取"的签到活动响应。
func todayCampaignJSON(status string) string {
	start := time.Now().In(qoderCST).Add(-time.Hour).Unix()
	return `{"campaigns":[{"campaignId":"c-1","actionType":"CLAIM_BENEFIT","claimStatus":"` + status +
		`","benefit":{"amount":100},"startAt":` + itoa64(start) + `}]}`
}

func itoa64(v int64) string {
	if v == 0 {
		return "0"
	}
	neg := v < 0
	if neg {
		v = -v
	}
	var b []byte
	for v > 0 {
		b = append([]byte{byte('0' + v%10)}, b...)
		v /= 10
	}
	if neg {
		return "-" + string(b)
	}
	return string(b)
}

// writeQoderPool 写一个只含指定账号的号池。
func writeQoderPool(t *testing.T, dir string, accounts ...*QoderSnapshot) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	buf, err := json.Marshal(QoderPoolFile{Accounts: accounts})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "qoder_accounts.json"), buf, 0o600); err != nil {
		t.Fatal(err)
	}
}

func snap(email, token string) *QoderSnapshot {
	return &QoderSnapshot{
		Email:  email,
		UserID: "uid-" + email,
		Name:   email,
		Auth: &QoderAuth{
			Token: token,
			User:  QoderUser{ID: "uid-" + email, Email: email, Name: email},
		},
	}
}

// TestQoderScanUpstreamTruth 真签到只认上游：claimed → 已签；pending → 未签。
func TestQoderScanUpstreamTruth(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("IDEAUTH_POOL_DIR", dir)

	srv := fakeQoderUpstream(t, map[string]string{
		"tok-signed": todayCampaignJSON("CLAIMED"),
		"tok-open":   todayCampaignJSON("CLAIMABLE"),
		"tok-none":   `{"campaigns":[]}`,
	})
	t.Setenv("QODER_OPENAPI_BASE", srv.URL)

	writeQoderPool(t, dir,
		snap("signed@x.com", "tok-signed"),
		snap("open@x.com", "tok-open"),
		snap("none@x.com", "tok-none"),
	)

	rows := QoderPoolScan(nil)
	if len(rows) != 3 {
		t.Fatalf("应有 3 行，得到 %d", len(rows))
	}
	byEmail := map[string]QoderScanAccount{}
	for _, r := range rows {
		byEmail[r.Email] = r
	}

	// 上游说今天已领取 → 真签到
	r := byEmail["signed@x.com"]
	if !r.CheckedInToday {
		t.Errorf("signed@x.com：上游 phase=claimed，应判为已签到（checked_in_today=true），实际 phase=%s", r.Phase)
	}
	if r.Source != "upstream" {
		t.Errorf("signed@x.com：source 应为 upstream，实际 %q", r.Source)
	}

	// 上游说今天可领取 → 没签
	r = byEmail["open@x.com"]
	if r.CheckedInToday {
		t.Errorf("open@x.com：上游 phase=claimable，不该判已签到")
	}
	if r.Phase != "claimable" {
		t.Errorf("open@x.com：phase 应为 claimable，实际 %s", r.Phase)
	}

	// 上游没有今日活动 → 没签
	r = byEmail["none@x.com"]
	if r.CheckedInToday {
		t.Errorf("none@x.com：上游 phase=pending，不该判已签到")
	}
}

// TestQoderScanLegacyDateRecordIsNotProof 旧格式（纯日期）本地记录不得当作已签。
//
// 这正是"显示反了"的第 3 层：浏览器加的号挂着一条旧版纯日期记录，
// CheckedInToday 按当日 00:00 解析 → true → 面板说它"今天签过了"。
func TestQoderScanLegacyDateRecordIsNotProof(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("IDEAUTH_POOL_DIR", dir)

	srv := fakeQoderUpstream(t, map[string]string{"tok-none": `{"campaigns":[]}`})
	t.Setenv("QODER_OPENAPI_BASE", srv.URL)

	writeQoderPool(t, dir, snap("none@x.com", "tok-none"))
	// 旧格式：纯日期，无时刻信息 → 不可信
	writeLog(t, dir, "qoder:none@x.com", time.Now().Format("2006-01-02"))

	rows := QoderPoolScan(nil)
	if len(rows) != 1 {
		t.Fatalf("应有 1 行，得到 %d", len(rows))
	}
	if rows[0].CheckedInToday {
		t.Error("旧格式纯日期记录不得作为「今天签过」的证据（上游 phase=pending）")
	}
	if rows[0].CheckedInLocal {
		t.Error("不可信的本地记录应被清掉（CheckedInLocal=false）")
	}
	if !strings.Contains(rows[0].Msg, "可疑签到记录") {
		t.Errorf("应提示本地记录可疑，实际 msg=%q", rows[0].Msg)
	}
}

// TestQoderScanDeviceQuotaExplained 同设备另一个号领了名额 → 本号未签且说明原因。
func TestQoderScanDeviceQuotaExplained(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("IDEAUTH_POOL_DIR", dir)

	srv := fakeQoderUpstream(t, map[string]string{
		"tok-a": todayCampaignJSON("CLAIMED"),
		"tok-b": `{"campaigns":[]}`,
	})
	t.Setenv("QODER_OPENAPI_BASE", srv.URL)

	writeQoderPool(t, dir, snap("a@x.com", "tok-a"), snap("b@x.com", "tok-b"))
	// a 号今日已领（可信记录：10:05 之后）
	writeLog(t, dir, "qoder:a@x.com", time.Now().Format("2006-01-02")+"T23:00:00+08:00")

	rows := QoderPoolScan(nil)
	byEmail := map[string]QoderScanAccount{}
	for _, r := range rows {
		byEmail[r.Email] = r
	}
	if !byEmail["a@x.com"].CheckedInToday {
		t.Error("a@x.com：上游说已领取，应判已签到")
	}
	b := byEmail["b@x.com"]
	if b.CheckedInToday {
		t.Error("b@x.com：上游没有今日活动，不该判已签到")
	}
	if !b.DeviceQuotaUsed {
		t.Error("b@x.com：同设备其它号已领名额，应标记 device_quota_used")
	}
	if !strings.Contains(b.Msg, "名额已被其他账号使用") {
		t.Errorf("b@x.com：msg 应说明名额被占，实际 %q", b.Msg)
	}
}

// TestQoderScanTrustedLocalFallback 上游看不到今日活动但本地有可信记录 → 已签（标注来源 local）。
func TestQoderScanTrustedLocalFallback(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("IDEAUTH_POOL_DIR", dir)

	srv := fakeQoderUpstream(t, map[string]string{"tok-none": `{"campaigns":[]}`})
	t.Setenv("QODER_OPENAPI_BASE", srv.URL)

	writeQoderPool(t, dir, snap("none@x.com", "tok-none"))
	writeLog(t, dir, "qoder:none@x.com", time.Now().Format("2006-01-02")+"T23:00:00+08:00")

	rows := QoderPoolScan(nil)
	if len(rows) != 1 {
		t.Fatalf("应有 1 行，得到 %d", len(rows))
	}
	if !rows[0].CheckedInToday {
		t.Error("上游看不到活动但本地有可信记录 → 应判已签到（领完即删）")
	}
	if rows[0].Source != "local" {
		t.Errorf("source 应为 local，实际 %q", rows[0].Source)
	}
	if !rows[0].RecordTrusted {
		t.Error("record_trusted 应为 true")
	}
}
