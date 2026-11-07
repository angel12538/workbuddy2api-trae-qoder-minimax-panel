package panel

import (
	"encoding/json"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/linguo2625469/workbuddy2api-panel/internal/doubao"
)

func TestDoubaoCookiePasteFormats(t *testing.T) {
	file := filepath.Join(t.TempDir(), "pool.json")
	t.Setenv("DOUBAO_POOL_FILE", file)
	for _, cookie := range []string{"Cookie:\r\nsessionid=TEST_ONLY;\r\n ttwid=two==", "sessionid=TEST_ONLY;ttwid=two==", "Host: www.doubao.com\nCookie: sessionid=TEST_ONLY;ttwid=two==\nUser-Agent: TEST_ONLY"} {
		b, _ := json.Marshal(map[string]string{"cookie": cookie})
		w := httptest.NewRecorder()
		(&Panel{}).doubaoAddCookie(w, httptest.NewRequest("POST", "/", strings.NewReader(string(b))))
		if w.Code != 200 {
			t.Fatalf("paste failed: HTTP %d", w.Code)
		}
		if strings.Contains(w.Body.String(), "TEST_ONLY") {
			t.Fatal("credential leaked")
		}
	}
	pool, err := doubao.LoadPool()
	if err != nil {
		t.Fatal(err)
	}
	if len(pool.Accounts) != 1 || len(pool.Accounts[0].Cookies) != 2 || pool.Accounts[0].Cookies["ttwid"] != "two==" {
		t.Fatal("cookie values or deduplication lost")
	}
	before, _ := os.ReadFile(file)
	for _, cookie := range []string{"ttwid=test", "sessionid=split\nvalue", "sessionid=bad\x00value"} {
		b, _ := json.Marshal(map[string]string{"cookie": cookie})
		w := httptest.NewRecorder()
		(&Panel{}).doubaoAddCookie(w, httptest.NewRequest("POST", "/", strings.NewReader(string(b))))
		if w.Code != 400 {
			t.Fatal("invalid cookie accepted")
		}
	}
	after, _ := os.ReadFile(file)
	if string(before) != string(after) {
		t.Fatal("invalid import changed pool")
	}
}

func TestDoubaoFullCopiedHeaders(t *testing.T) {
	const chrome = ":authority\nwww.doubao.com\n:method\nPOST\n:path\n/alice/slot/input/list?version_code=TEST_ONLY\n:scheme\nhttps\naccept\n*/*\ncontent-type\napplication/json\ncookie\nsessionid=TEST_ONLY; ttwid=two==; priority=COOKIE_VALUE\ndnt\n1\norigin\nhttps://www.doubao.com\npriority\nu=1, i\nreferer\nhttps://www.doubao.com/\nsec-ch-ua\n\"Chromium\";v=\"156\"\nsec-fetch-site\nsame-origin\nx-custom-header\nnot_a_cookie=TEST_ONLY\nuser-agent\nTEST_ONLY"
	for _, input := range []string{
		chrome, strings.ReplaceAll(chrome, "\n", "\r\n"), "\ufeff" + chrome,
		"POST /alice/slot/input/list HTTP/2\nHost: www.doubao.com\nCookie: sessionid=TEST_ONLY; ttwid=two==; priority=COOKIE_VALUE\nPriority: u=1\nAuthorization: Bearer TEST_ONLY",
		":authority: www.doubao.com\n:method: POST\ncookie: sessionid=TEST_ONLY; ttwid=two==\nCookie: priority=COOKIE_VALUE\nDnt: 1\nUser-Agent: TEST_ONLY",
		"Host\nwww.doubao.com\nCookie\nsessionid=TEST_ONLY\nttwid=two==\nDnt\n1\nCookie\npriority=COOKIE_VALUE\nPriority\nu=1",
	} {
		cookies, err := parseDoubaoCookieInput(input)
		if err != nil {
			t.Fatal("valid copied request headers rejected")
		}
		want := map[string]string{"sessionid": "TEST_ONLY", "ttwid": "two==", "priority": "COOKIE_VALUE"}
		if !reflect.DeepEqual(cookies, want) {
			t.Fatal("non-Cookie headers were captured or Cookie values changed")
		}
	}
	file := filepath.Join(t.TempDir(), "pool.json")
	t.Setenv("DOUBAO_POOL_FILE", file)
	for i := 0; i < 2; i++ {
		b, _ := json.Marshal(map[string]string{"cookie": chrome})
		w := httptest.NewRecorder()
		(&Panel{}).doubaoAddCookie(w, httptest.NewRequest("POST", "/", strings.NewReader(string(b))))
		if w.Code != 200 || strings.Contains(w.Body.String(), "TEST_ONLY") {
			t.Fatal("header import failed or exposed credentials")
		}
	}
	pool, err := doubao.LoadPool()
	if err != nil {
		t.Fatal(err)
	}
	if len(pool.Accounts) != 1 || len(pool.Accounts[0].Cookies) != 3 {
		t.Fatal("header import did not save exactly the Cookie field or deduplicate")
	}
}

func TestDoubaoCopiedHeadersRejectWrongHostOrMissingLogin(t *testing.T) {
	file := filepath.Join(t.TempDir(), "pool.json")
	t.Setenv("DOUBAO_POOL_FILE", file)
	for _, tt := range []struct{ input, expected string }{
		{":authority\nmssdk.bytedance.com\ncookie\nttwid=TEST_ONLY\norigin\nhttps://www.doubao.com", "目标域名不是豆包"},
		{"Host: doubao.com.example.invalid\nCookie: sessionid=TEST_ONLY", "目标域名不是豆包"},
		{"Host: notdoubao.com\nCookie: sessionid=TEST_ONLY", "目标域名不是豆包"},
		{"Host: www.doubao.com\n:authority: example.invalid\nCookie: sessionid=TEST_ONLY", "目标域名不是豆包"},
		{"Host: example.invalid\nCookie: sessionid=TEST_ONLY\nOrigin: https://www.doubao.com", "目标域名不是豆包"},
		{":authority\nwww.doubao.com\naccept\n*/*", "没有 Cookie"},
		{"Host: www.doubao.com\nCookie: ttwid=TEST_ONLY", "缺少豆包登录字段"},
		{"Cookie\nsessionid=split\nbroken-token-fragment", "Cookie 格式不正确"},
		{"Cookie: sessionid=TEST_ONLY\tinside", "Cookie 格式不正确"},
		{"Cookie: sessionid=TEST_ONLY\x00inside", "Cookie 格式不正确"},
	} {
		b, _ := json.Marshal(map[string]string{"cookie": tt.input})
		w := httptest.NewRecorder()
		(&Panel{}).doubaoAddCookie(w, httptest.NewRequest("POST", "/", strings.NewReader(string(b))))
		if w.Code != 400 || !strings.Contains(w.Body.String(), tt.expected) || strings.Contains(w.Body.String(), "TEST_ONLY") {
			t.Fatal("invalid input did not produce a safe actionable error")
		}
	}
	if _, err := os.Stat(file); !os.IsNotExist(err) {
		t.Fatal("invalid input changed account pool")
	}
}

func TestDoubaoHeaderHostRules(t *testing.T) {
	for _, host := range []string{"www.doubao.com", "doubao.com", "api.doubao.com", "WWW.DOUBAO.COM:443", "www.doubao.com."} {
		if !doubaoCookieHostAllowed(host) {
			t.Fatal("valid Doubao request host rejected")
		}
	}
	for _, host := range []string{"www.doubao.com@other.invalid", "doubao.com.other.invalid", "notdoubao.com", "www.doubao.com/path", "www.doubao.com?secret=TEST_ONLY", "www.doubao.com#ignored", ""} {
		if doubaoCookieHostAllowed(host) {
			t.Fatal("invalid request host accepted")
		}
	}
}
