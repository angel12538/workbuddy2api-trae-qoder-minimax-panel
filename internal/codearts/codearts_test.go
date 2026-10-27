package codearts

import "testing"

// 回归：CodeArts 账号身份必须**跨登录稳定**。
//
// 背景：华为云网页登录态只有约一小时寿命，Cookie 快照第二天就失效，所以
// 签到必须复用「登录时那个浏览器的 profile」。而 profile 要挂在账号名下，
// 账号身份（ID）就必须在**每次登录后都算出同一个值**。
//
// 用华为云稳定账号 ID（domain_tag / user_tag）做指纹，而不是会话 Cookie
// （SessionID 每次登录都变 —— 拿它认人，同一个账号会被认成好多个）。

func ck(name, value string) Cookie { return Cookie{Name: name, Value: value, Domain: ".huaweicloud.com"} }

func TestFingerprintStableAcrossLogins(t *testing.T) {
	// 第一次登录：会话票据是 A
	first := []Cookie{
		ck("domain_tag", "c89103cc576b49c299c773db80c8dd3d"),
		ck("user_tag", "1fc7818c8c2c414694ad40d8560205c1"),
		ck("SessionID", "11111111-2222-3333-4444-555555555555"),
		ck("HWWAFSESID", "waf-a"),
	}
	// 第二次登录：同一个账号，会话票据全变了（多出 WAF / 埋点 Cookie）
	second := []Cookie{
		ck("domain_tag", "c89103cc576b49c299c773db80c8dd3d"),
		ck("user_tag", "1fc7818c8c2c414694ad40d8560205c1"),
		ck("SessionID", "99999999-8888-7777-6666-555555555555"),
		ck("HWWAFSESID", "waf-b"),
		ck("_fr_ssid", "fr-1"),
	}
	if Fingerprint(first) != Fingerprint(second) {
		t.Fatalf("同一个华为云账号两次登录必须算出同一个 id：first=%s second=%s",
			Fingerprint(first), Fingerprint(second))
	}
	if DomainIDOf(first) == "" || DomainIDOf(second) != DomainIDOf(first) {
		t.Fatal("DomainIDOf 应取出稳定的华为云账号 ID")
	}
}

func TestFingerprintDistinguishesAccounts(t *testing.T) {
	a := []Cookie{ck("domain_tag", "account-a"), ck("user_tag", "user-a")}
	b := []Cookie{ck("domain_tag", "account-b"), ck("user_tag", "user-b")}
	if Fingerprint(a) == Fingerprint(b) {
		t.Fatal("不同账号必须算出不同的 id")
	}
}

func TestFingerprintFallsBackToSession(t *testing.T) {
	// 没有稳定账号标识时退回会话票据：同一份 Cookie 快照仍要可重复识别。
	c := []Cookie{ck("SessionID", "abc"), ck("hwid", "hw-1")}
	if Fingerprint(c) != Fingerprint(c) {
		t.Fatal("同一份 Cookie 快照的指纹必须稳定")
	}
	if Fingerprint(c) == Fingerprint([]Cookie{ck("SessionID", "xyz"), ck("hwid", "hw-1")}) {
		t.Fatal("不同会话不应算出相同指纹")
	}
}

func TestUpsertKeepsSlotAndID(t *testing.T) {
	p := &Pool{}
	acc := &Account{ID: "abc123", Slot: "abc123", Cookies: []Cookie{ck("domain_tag", "x")}, SavedAt: Now()}
	saved, added := Upsert(p, acc)
	if !added || saved.Slot != "abc123" {
		t.Fatalf("新账号应入库并带 slot：added=%v slot=%q", added, saved.Slot)
	}
	// 同一账号再次登录（指纹相同 → ID 相同）：更新而不是新增，且 slot 保持不变。
	again := &Account{ID: "abc123", Slot: "abc123", Cookies: []Cookie{ck("domain_tag", "x"), ck("SessionID", "new")}, SavedAt: Now()}
	saved2, added2 := Upsert(p, again)
	if added2 || len(p.Accounts) != 1 {
		t.Fatalf("同一账号重复登录不应新增条目：added=%v n=%d", added2, len(p.Accounts))
	}
	if saved2.Slot != "abc123" {
		t.Fatalf("slot 必须保持稳定：%q", saved2.Slot)
	}
}
