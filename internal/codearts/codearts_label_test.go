package codearts

import "testing"

// TestDedupeLabels 钉死"两个真人被叫成同一个名字"这个显示 bug。
//
// 实测现场（2026-11-01）：号池两条记录的脱敏手机号分别是 177****2169 / 156****8342
// （确实是两个人），标签却都是「CodeArts 账号 2」—— 因为老逻辑用
// "CodeArts 账号 " + (len(Accounts)+1)，插第二条时 len 已经是 1，两条算出同一个名。
// 面板上看着就像"同一个华为账号登了两次"。
func TestDedupeLabels(t *testing.T) {
	mk := func(maskedUser, maskedPhone string) []Cookie {
		return []Cookie{
			{Name: "masked_user", Value: maskedUser},
			{Name: "masked_phone", Value: maskedPhone},
			{Name: "domain_tag", Value: "d-" + maskedUser},
		}
	}
	p := &Pool{Accounts: []*Account{
		{ID: "ca1d671920b4", Label: "CodeArts 账号 2", Cookies: mk("h*********uvj421wxe", "177****2169")},
		{ID: "423eab29ffdd", Label: "CodeArts 账号 2", Cookies: mk("G*******88921257", "156****8342")},
		{ID: "nick", Label: "CodeArts 账号 2", Nickname: "张三的号", Cookies: mk("z*********9", "138****0000")},
	}}

	// 三条都要收敛：两条自动命名的换成脱敏名，有 Nickname 的那条 Label 同步成 Nickname。
	if n := DedupeLabels(p); n != 3 {
		t.Fatalf("应改 3 条（2 条脱敏名 + 1 条同步 Nickname），得到 %d", n)
	}
	if p.Accounts[2].Label != "张三的号" {
		t.Errorf("有 Nickname 的账号，Label 必须就是 Nickname，得到 %q", p.Accounts[2].Label)
	}
	a, b := p.Accounts[0].Label, p.Accounts[1].Label
	if a == b || a == "CodeArts 账号 2" || b == "CodeArts 账号 2" {
		t.Fatalf("两条应改成可区分的名字，得到 %q / %q", a, b)
	}
	t.Logf("改名后： %q / %q", a, b)

	// 幂等：再整理一次不该继续改名。
	if n := DedupeLabels(p); n != 0 {
		t.Fatalf("第二次整理不该再改名，得到 %d（%q / %q）", n, p.Accounts[0].Label, p.Accounts[1].Label)
	}
}

// TestLabelOf 没有脱敏标识时必须回空串，让调用方走编号兜底（不能拼出 "CodeArts " 这种半截名）。
func TestLabelOf(t *testing.T) {
	if got := LabelOf([]Cookie{{Name: "SessionID", Value: "x"}}); got != "" {
		t.Fatalf("无脱敏标识应返回空，得到 %q", got)
	}
	if got := LabelOf([]Cookie{{Name: "masked_phone", Value: "177****2169"}}); got == "" {
		t.Fatal("只有脱敏手机号时也该拼出可区分的名字")
	}
}
