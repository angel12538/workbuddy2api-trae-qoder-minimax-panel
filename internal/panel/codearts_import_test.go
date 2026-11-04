package panel

import "testing"

// TestParseCookieHeaderExpiryUnknown 粘贴导入**永远拿不到**过期信息。
//
// Cookie 请求头只有 name=value，没有 Expires/Max-Age。这里锁死"显式未观测"：
// 否则下游会把它当 0 秒过期或会话级，面板就会谎称能判断票据寿命。
func TestParseCookieHeaderExpiryUnknown(t *testing.T) {
	for _, c := range parseCookieHeader("SSOTGC=TGT; SSOJTC=JTC; cbc-sid=a") {
		if c.Expires != 0 || c.ExpiresKnown || c.Session {
			t.Errorf("导入路径必须标记未观测，得到 %+v", c)
		}
	}
}

// TestParseCookieHeader 「粘贴 Cookie 导入」的解析容错。
//
// 用户复制过来的东西形态很杂：可能连 "Cookie: " 前缀一起抄、可能有多余空格、
// 可能有空片段。这些都要能吃下，否则"导入失败"会让人以为自己的登录态有问题。
func TestParseCookieHeader(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want int
	}{
		{"常规", "SessionID=abc; devclouddevuibjtcftk=UM46-UXO9", 2},
		{"带 Cookie: 前缀", "Cookie: SessionID=abc; cbc-sid=xyz", 2},
		{"带小写前缀", "cookie: a=1; b=2; c=3", 3},
		{"杂乱片段", "  a=1 ;;  ; b=2 ; =novalue ; c= ", 2},
		{"空输入", "   ", 0},
		{"没有等号", "justtext", 0},
	}
	for _, c := range cases {
		got := parseCookieHeader(c.in)
		if len(got) != c.want {
			t.Errorf("%s：期望 %d 条，得到 %d（%+v）", c.name, c.want, len(got), got)
		}
	}
	// 值必须原样保留（华为票据里带 '-'、'_'、base64 字符很常见）。
	got := parseCookieHeader("SSOTGC=TGTX--F745676460-92023877-c4")
	if len(got) != 1 || got[0].Value != "TGTX--F745676460-92023877-c4" {
		t.Fatalf("票据值被改坏了: %+v", got)
	}
	if len(got) == 1 && got[0].Domain != ".huaweicloud.com" {
		t.Fatalf("域应统一挂 .huaweicloud.com，得到 %q", got[0].Domain)
	}
}
