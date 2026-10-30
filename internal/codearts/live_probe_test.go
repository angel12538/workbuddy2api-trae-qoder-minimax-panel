package codearts

import (
	"os"
	"testing"
)

// 这是一个**连真实华为云**的诊断入口，默认跳过。
//
// 用法（真打上游，不是 mock）：
//
//	$env:CODEARTS_POOL_FILE='D:\WorkBuddy\data\ide_accounts\codearts_accounts.json'
//	$env:CA_LIVE='1'
//	go test ./internal/codearts/ -run TestLiveProbe -v
//
// 输出用来核对三件事：
//  1. 会话还有效吗（积分/活动接口能不能通）；
//  2. 今天的每日签到领了没（claimable / status / benefitAmount）；
//  3. 余额是多少（package/overview 的 package_credit_remain）。
//
// ⚠️ 会真的调 /ops/claim 领取权益 —— 加 CA_LIVE_CLAIM=1 才领。
func TestLiveProbe(t *testing.T) {
	if os.Getenv("CA_LIVE") != "1" {
		t.Skip("需要 CA_LIVE=1")
	}
	pool, err := LoadPool()
	if err != nil {
		t.Fatalf("读号池失败: %v", err)
	}
	for _, a := range pool.Accounts {
		if a == nil {
			continue
		}
		t.Logf("=== id=%s label=%q identity=%s slot=%s cookies=%d saved=%s",
			a.ID, a.Label, a.Identity, a.Slot, len(a.Cookies), a.SavedAt)
		t.Logf("    Fingerprint=%s  IdentityOf=%s", Fingerprint(a.Cookies), IdentityOf(a.Cookies))

		ci, cerr := Credits(a)
		if cerr != nil {
			t.Logf("    积分: 失败 → %v（会话失效？）", cerr)
		} else {
			t.Logf("    积分: 剩余 %.0f / 总额 %.0f / 已用 %.0f / 将过期 %.0f",
				ci.Remain, ci.Total, ci.Used, ci.Expiring)
		}

		camps, derr := Campaigns(a)
		if derr != nil {
			t.Logf("    活动: 失败 → %v", derr)
			continue
		}
		for _, c := range camps {
			t.Logf("    活动: id=%d type=%-18s claimable=%-5v status=%-10s benefit=%d %q",
				c.CampaignID, c.Type, c.Claimable, c.Status, c.BenefitAmount, c.Title)
		}

		daily, ok := DailyCampaign(camps)
		if !ok {
			t.Logf("    每日签到: 活动列表里没有")
			continue
		}
		t.Logf("    每日签到: 每天可领 %d 积分，今天 %s",
			daily.BenefitAmount, map[bool]string{true: "已领", false: "未领"}[daily.Claimed() || !daily.Claimable])

		if os.Getenv("CA_LIVE_CLAIM") != "1" {
			continue
		}
		if daily.Claimed() || !daily.Claimable {
			continue
		}
		key := DailyIdempotentKey(a.ID, daily.CampaignID, nowDay())
		r, cerr := ClaimWithKey(a, daily.CampaignID, key)
		if cerr != nil {
			t.Logf("    领取失败: %v", cerr)
			continue
		}
		t.Logf("    领取: ok=%v code=%d msg=%q", r.OK, r.Code, r.Message)
	}
}
