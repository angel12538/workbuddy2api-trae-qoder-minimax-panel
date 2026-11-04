package panel

import (
	"encoding/json"
	"testing"

	"github.com/linguo2625469/workbuddy2api-panel/internal/codearts"
)

// UI/补采采集路径的归一入口：CDP 的 -1 必须变成
// {Expires:-1, ExpiresKnown:true, Session:true}，else 票据寿命永远判不出来。
func TestCDPCookieToCodearts(t *testing.T) {
	session := cdpCookie{Name: "SSOTGC", Value: "v", Domain: ".huaweicloud.com", Path: "/", Expires: -1}.toCodearts()
	if session.Expires != -1 || !session.ExpiresKnown || !session.Session {
		t.Fatalf("CDP -1 必须落成会话三元组，得到 %+v", session)
	}

	abs := cdpCookie{Name: "SSOJTC", Value: "v", Domain: ".huaweicloud.com", Path: "/", Expires: 1767225600}.toCodearts()
	if abs.Expires != 1767225600 || !abs.ExpiresKnown || abs.Session {
		t.Fatalf("CDP >0 必须落成已知绝对时间，得到 %+v", abs)
	}

	unknown := cdpCookie{Name: "CASTGCLOW", Value: "v", Domain: ".huaweicloud.com", Path: "/", Expires: 0}.toCodearts()
	if unknown.Expires != 0 || unknown.ExpiresKnown || unknown.Session {
		t.Fatalf("CDP 0 必须按未知处理，得到 %+v", unknown)
	}
}

// 归一结果必须能真正驱动票据寿命判定（防止只改结构、不通链路）。
func TestCDPNormalizeFeedsIDPDeadline(t *testing.T) {
	got := []codearts.Cookie{
		cdpCookie{Name: "SSOTGC", Value: "tgt", Domain: ".huaweicloud.com", Path: "/", Expires: -1}.toCodearts(),
		cdpCookie{Name: "cbc-sid", Value: "a", Domain: ".huaweicloud.com", Path: "/", Expires: 1767225600}.toCodearts(),
		cdpCookie{Name: "SSOJTC", Value: "jtc", Domain: ".auth.huaweicloud.com", Path: "/", Expires: 0}.toCodearts(),
	}
	ddl, ok := codearts.IDPDeadline(got)
	if !ok || ddl.Unix() != 1767225600 {
		t.Fatalf("归一后的绝对票据必须被 IDPDeadline 识别，得到 %v/%v", ddl, ok)
	}
	if !got[0].Session || got[0].ExpiresKnown != true {
		t.Fatalf("会话票据必须是已知的 session，得到 %+v", got[0])
	}
	if got[2].ExpiresKnown || got[2].Session {
		t.Fatalf("未观测票据不得被当成已知，得到 %+v", got[2])
	}
}

// 粘贴导入路径没有过期信息：必须显式是「未观测」，JSON 里不得出现假 expires/session。
func TestImportedCookiesAreExpiryUnknown(t *testing.T) {
	got := parseCookieHeader("SSOTGC=TGT-x; SSOJTC=JTC-y; cbc-sid=z")
	if len(got) != 3 {
		t.Fatalf("应解析出 3 条，得到 %d", len(got))
	}
	for _, c := range got {
		if c.Expires != 0 || c.ExpiresKnown || c.Session {
			t.Errorf("粘贴导入必须标记为未观测，得到 %+v", c)
		}
	}
	raw, err := json.Marshal(got[0])
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatal(err)
	}
	for _, k := range []string{"expires", "expires_known", "session"} {
		if _, ok := m[k]; ok {
			t.Errorf("未观测的 Cookie 不应写出 %q（会误导成已观测）：%s", k, raw)
		}
	}
	// 未观测的 Cookie 不得产生票据截止时间（不谎称能判寿命）。
	p := &codearts.Account{Cookies: got}
	p.RefreshTicketDeadline()
	if p.TicketDeadline != "" || p.TicketSource != "" {
		t.Fatalf("未观测的票据不得推出截止时间，得到 %q/%q", p.TicketDeadline, p.TicketSource)
	}
}
