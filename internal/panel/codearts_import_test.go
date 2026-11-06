package panel

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/linguo2625469/workbuddy2api-panel/internal/codearts"
)

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

func TestCodeartsCookieInputCopiedFormats(t *testing.T) {
	for _, raw := range []string{
		"SessionID=TEST_ONLY; cftk=two==",
		"Cookie: SessionID=TEST_ONLY; cftk=two==",
		"Cookie:\r\nSessionID=TEST_ONLY;\r\n\tcftk=two==",
		"Cookie\r\nSessionID=TEST_ONLY\r\ncftk=two==",
		"SessionID=TEST_ONLY\ncftk=two==",
		"Cookie: SessionID=TEST_ONLY\ncookie: cftk=two==",
		"Host: example.invalid\nCookie:\nSessionID=TEST_ONLY; cftk=two==\nUser-Agent: TEST_ONLY",
	} {
		got, err := parseCodeartsCookieInput(raw)
		if err != nil {
			t.Fatal(err)
		}
		if len(got) != 2 || got[0].Name != "SessionID" || got[0].Value != "TEST_ONLY" ||
			got[1].Name != "cftk" || got[1].Value != "two==" {
			t.Fatal("copied cookie pairs were not preserved")
		}
	}
}

func TestCodeartsCookieInputRejectsAmbiguousOrInvalidBytes(t *testing.T) {
	for _, raw := range []string{
		"SessionID=TEST_ONLY\nbroken-token-fragment",
		"SessionID=TEST_ONLY\tinside",
		"SessionID=TEST_ONLY\x00inside",
		"SessionID=TEST_ONLY\x7f",
		"SessionID=TEST_ONLY\v",
		"invalid name=TEST_ONLY",
		"SessionID=TEST_ONLY\nUser-Agent: other",
	} {
		got, err := parseCodeartsCookieInput(raw)
		if err == nil || len(got) != 0 {
			t.Fatal("invalid input must fail entirely")
		}
		if strings.Contains(err.Error(), "TEST_ONLY") {
			t.Fatal("format error leaked input")
		}
	}
}

// Reproduce the original transport error, then exercise the corrected parser
// through the actual Keepalive request path (including the separate cftk header).
func TestCodeartsCookieInputHTTPTransportRegression(t *testing.T) {
	calls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if r.Header.Get("Cookie") != "SessionID=TEST_ONLY; cftk=two==" || r.Header.Get("cftk") != "two==" {
			t.Error("cookie request headers did not match normalized pairs")
		}
		w.Write([]byte("{}"))
	}))
	defer server.Close()
	before := codearts.APIBase
	codearts.APIBase = server.URL
	defer func() { codearts.APIBase = before }()
	_, err := codearts.Keepalive(&codearts.Account{Cookies: []codearts.Cookie{
		{Name: "SessionID", Value: "TEST_ONLY\r\ncftk=two=="},
	}})
	if err == nil || !strings.Contains(err.Error(), "invalid header field value") || calls != 0 {
		t.Fatal("original invalid-header transport failure was not reproduced")
	}
	cookies, err := parseCodeartsCookieInput("SessionID=TEST_ONLY\r\ncftk=two==")
	if err != nil {
		t.Fatal(err)
	}
	ok, err := codearts.Keepalive(&codearts.Account{Cookies: cookies})
	if err != nil || !ok || calls != 1 {
		t.Fatalf("normalized Keepalive did not complete: ok=%v err=%v calls=%d", ok, err, calls)
	}
}

func TestCodeartsImportFormatErrorBeforeNetwork(t *testing.T) {
	calls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { calls++ }))
	defer server.Close()
	before := codearts.APIBase
	codearts.APIBase = server.URL
	defer func() { codearts.APIBase = before }()
	raw, _ := json.Marshal(map[string]string{"cookie": "SessionID=TEST_ONLY\x00invalid"})
	rec := httptest.NewRecorder()
	(&Panel{}).codeartsImportCookie(rec, httptest.NewRequest(http.MethodPost, "/", strings.NewReader(string(raw))))
	if rec.Code != http.StatusBadRequest || calls != 0 || strings.Contains(rec.Body.String(), "TEST_ONLY") {
		t.Fatal("malformed import must be rejected before networking without echoing credentials")
	}
}

func TestCodeartsImportFailureClassification(t *testing.T) {
	for _, tc := range []struct {
		err  error
		want string
	}{
		{fmt.Errorf("%w: TEST_ONLY", codearts.ErrSessionExpired), "会话已失效"},
		{fmt.Errorf("%w: TEST_ONLY", context.DeadlineExceeded), "连接华为云超时"},
		{errors.New("TEST_ONLY"), "不能据此判断 Cookie 已过期"},
		{nil, "暂未通过会话验证"},
	} {
		got := codeartsImportFailure(tc.err)
		if !strings.Contains(got, tc.want) || strings.Contains(got, "TEST_ONLY") {
			t.Fatal("failure was misclassified or leaked underlying error text")
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
