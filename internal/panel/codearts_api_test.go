package panel

import "testing"

// 每日签到的「已领 / 可领」判据抄自 CodeArts 前端 bundle（福利弹窗里的
// claimed / claimable 计算），这里钉死，别让它悄悄漂移。
func TestCaWelfareClaimJudgement(t *testing.T) {
	tr := true
	fl := false
	cases := []struct {
		name       string
		a          caWelfareActivity
		wantClaim  bool
		wantCan    bool
	}{
		{"已领-状态CLAIMED", caWelfareActivity{Status: "CLAIMED"}, true, false},
		{"已领-状态CONFIRM", caWelfareActivity{Status: "CONFIRM"}, true, false},
		{"已领-状态CONFIRMED", caWelfareActivity{Status: "CONFIRMED"}, true, false},
		{"已领-状态CONSUME", caWelfareActivity{Status: "CONSUME"}, true, false},
		{"已领-claimed标记", caWelfareActivity{Claimed: true}, true, false},
		{"可领-ELIGIBLE", caWelfareActivity{Status: "ELIGIBLE"}, false, true},
		{"可领-无状态", caWelfareActivity{}, false, true},
		{"可领-claimable为真", caWelfareActivity{Status: "ELIGIBLE", Claimable: &tr}, false, true},
		{"不可领-claimable为假", caWelfareActivity{Status: "ELIGIBLE", Claimable: &fl}, false, false},
		{"已领优先于可领标记", caWelfareActivity{Status: "CLAIMED", Claimable: &tr}, true, false},
	}
	for _, c := range cases {
		if got := caWelfareIsClaimed(c.a); got != c.wantClaim {
			t.Errorf("%s: caWelfareIsClaimed = %v, want %v", c.name, got, c.wantClaim)
		}
		if got := caWelfareClaimable(c.a); got != c.wantCan {
			t.Errorf("%s: caWelfareClaimable = %v, want %v", c.name, got, c.wantCan)
		}
	}
}

// campaignId 会被拼进页面里执行的 JS，必须收敛成安全字面量。
func TestCaWelfareIDSafe(t *testing.T) {
	cases := map[string]string{
		"3":             "3",
		"daily_2026":    "daily_2026",
		"a-b_c9":        "a-b_c9",
		"';alert(1);//": "__alert_1____",
		"中文":           "__",
		"":              "_",
		"a\"b'c":       "a_b_c",
	}
	for in, want := range cases {
		if got := caWelfareIDSafe(in); got != want {
			t.Errorf("caWelfareIDSafe(%q) = %q, want %q", in, got, want)
		}
	}
	// 超长也要截断
	long := ""
	for i := 0; i < 300; i++ {
		long += "x"
	}
	if got := caWelfareIDSafe(long); len(got) != 120 {
		t.Errorf("超长 id 未截断：len=%d", len(got))
	}
}

// 领取脚本要把 campaignId 填进去，不能留占位符。
func TestCaWelfareClaimJSFor(t *testing.T) {
	js := caWelfareClaimJSFor("abc-123")
	if contains(js, "%CID%") {
		t.Error("占位符没被替换")
	}
	if !contains(js, `"abc-123"`) {
		t.Errorf("campaignId 没填进脚本：%s", js[:200])
	}
	// 注入尝试必须被挡住
	evil := caWelfareClaimJSFor("x'; alert(1); //")
	if contains(evil, "alert(1); //\"") {
		t.Errorf("注入没有被挡住：%s", evil[:200])
	}
}

func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
