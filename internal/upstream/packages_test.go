package upstream

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
)

// packagesEnvelope 把 Accounts 数组包成上游信封
// （doJSON 先解一层 apiEnvelope，CreditPackages 再从 Response.Data 开始解）。
func packagesEnvelope(accountsJSON string) string {
	return fmt.Sprintf(`{"code":0,"msg":"OK","data":{"Response":{"Data":{"Accounts":[%s]}}}}`, accountsJSON)
}

// newPackagesServer 起一个固定返回 accountsJSON 的假上游。
func newPackagesServer(t *testing.T, accountsJSON string) (*Client, func()) {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(packagesEnvelope(accountsJSON)))
	}))
	c := &Client{HTTP: srv.Client(), BillingBaseCN: srv.URL}
	return c, srv.Close
}

// TestCreditPackagesEndTimePrefersCycleEndTime 回归：上游实际下发的是 CycleEndTime。
//
// 背景：ExpiredTime 在 CN/global 实测恒为空串，PackageEndTime 在字段全集里
// 根本不存在，真实到期时间只在 CycleEndTime。旧实现只读前两者 → 面板
// 「积分构成」的到期列恒显示「—」（上游 4466a3e6 修的正是这个）。
func TestCreditPackagesEndTimePrefersCycleEndTime(t *testing.T) {
	const acct = `{"PackageName":"Free Plan","CapacitySize":100,"CapacityRemain":100,
		"CycleCapacitySize":100,"CycleCapacityRemain":0,"CycleCapacityUsed":100,
		"ExpiredTime":"","CycleStartTime":"2026-09-01 00:00:00",
		"CycleEndTime":"2026-09-30 23:59:59","CreateTime":1789125551000}`
	c, closeFn := newPackagesServer(t, acct)
	defer closeFn()

	packs, _, _, err := c.CreditPackages(&auth.Auth{AccessToken: "at", UID: "u1"})
	if err != nil {
		t.Fatalf("CreditPackages: %v", err)
	}
	if len(packs) != 1 {
		t.Fatalf("packs=%d want 1", len(packs))
	}
	if got, want := packs[0].EndTime, "2026-09-30 23:59:59"; got != want {
		t.Errorf("EndTime=%q want %q（漏读 CycleEndTime 会让面板到期列恒为 —）", got, want)
	}
}

// TestCreditPackagesEndTimeFieldPrecedence 三个候选字段的优先级：
// ExpiredTime > PackageEndTime > CycleEndTime，谁先有值用谁。
func TestCreditPackagesEndTimeFieldPrecedence(t *testing.T) {
	cases := []struct {
		name string
		acct string
		want string
	}{
		{
			name: "ExpiredTime 优先",
			acct: `{"PackageName":"p","CycleCapacitySize":1,"CycleCapacityRemain":1,
				"ExpiredTime":"2027-01-01 00:00:00","PackageEndTime":"2026-12-01 00:00:00",
				"CycleEndTime":"2026-09-30 23:59:59"}`,
			want: "2027-01-01 00:00:00",
		},
		{
			name: "ExpiredTime 空则 PackageEndTime",
			acct: `{"PackageName":"p","CycleCapacitySize":1,"CycleCapacityRemain":1,
				"ExpiredTime":"","PackageEndTime":"2026-12-01 00:00:00",
				"CycleEndTime":"2026-09-30 23:59:59"}`,
			want: "2026-12-01 00:00:00",
		},
		{
			name: "前两者都空才用 CycleEndTime",
			acct: `{"PackageName":"p","CycleCapacitySize":1,"CycleCapacityRemain":1,
				"ExpiredTime":"","PackageEndTime":"","CycleEndTime":"2026-09-30 23:59:59"}`,
			want: "2026-09-30 23:59:59",
		},
		{
			name: "三者皆空 → 留空（不伪造）",
			acct: `{"PackageName":"p","CycleCapacitySize":1,"CycleCapacityRemain":1,
				"ExpiredTime":"","PackageEndTime":"","CycleEndTime":""}`,
			want: "",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c, closeFn := newPackagesServer(t, tc.acct)
			defer closeFn()
			packs, _, _, err := c.CreditPackages(&auth.Auth{AccessToken: "at", UID: "u1"})
			if err != nil {
				t.Fatalf("CreditPackages: %v", err)
			}
			if len(packs) != 1 {
				t.Fatalf("packs=%d want 1", len(packs))
			}
			if packs[0].EndTime != tc.want {
				t.Errorf("EndTime=%q want %q", packs[0].EndTime, tc.want)
			}
		})
	}
}

// TestCreditPackagesCycleFieldsWhenCycleCapacityPresent 回归：CycleCapacitySize>0
// 时按周期字段算余量（不混用 Capacity*，否则同一个包被算两次）。
func TestCreditPackagesCycleFieldsWhenCycleCapacityPresent(t *testing.T) {
	const acct = `{"PackageName":"p","CapacitySize":999,"CapacityRemain":999,
		"CycleCapacitySize":100,"CycleCapacityRemain":30,"CycleCapacityUsed":70,
		"CycleEndTime":"2026-09-30 23:59:59"}`
	c, closeFn := newPackagesServer(t, acct)
	defer closeFn()

	packs, sumRemain, sumSize, err := c.CreditPackages(&auth.Auth{AccessToken: "at", UID: "u1"})
	if err != nil {
		t.Fatalf("CreditPackages: %v", err)
	}
	if !packs[0].Cycle {
		t.Error("Cycle=false want true（CycleCapacitySize>0 应走周期口径）")
	}
	if packs[0].Size != 100 || packs[0].Remain != 30 {
		t.Errorf("size/remain=%d/%d want 100/30（走 Capacity* 会得到 999/999）", packs[0].Size, packs[0].Remain)
	}
	if sumRemain != 30 || sumSize != 100 {
		t.Errorf("sum=%d/%d want 30/100", sumRemain, sumSize)
	}
}
