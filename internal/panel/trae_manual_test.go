package panel

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/upstream"
)

func manualTestReturn(callback, state, token string) string {
	return callback + "?" + url.Values{"scope": {"trae"}, "loginTraceID": {state}, "refreshToken": {token}, "data": {token}}.Encode()
}

func TestTraeManualAuthAndStart(t *testing.T) {
	p := newTestPanel()
	for _, path := range []string{"start", "import"} {
		r := httptest.NewRequest("POST", "/panel/api/trae/login/manual/"+path, strings.NewReader("{}"))
		w := httptest.NewRecorder()
		p.ServeHTTP(w, r)
		if w.Code != 401 {
			t.Fatalf("%s: expected authentication, got %d", path, w.Code)
		}
	}
	r := httptest.NewRequest("POST", "/panel/api/trae/login/manual/start", nil)
	r.Header.Set("Authorization", "Bearer test-key")
	w := httptest.NewRecorder()
	p.ServeHTTP(w, r)
	if w.Code != 200 || w.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("start: %d", w.Code)
	}
	var result struct {
		State string `json:"state"`
		URL   string `json:"url"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	u, err := url.Parse(result.URL)
	if err != nil {
		t.Fatal(err)
	}
	q := u.Query()
	if len(result.State) != 64 || q.Get("login_trace_id") != result.State || q.Get("login_channel") != "native_ide" || q.Get("login_version") != "1" || u.Host != "www.trae.cn" {
		t.Fatal("incorrect official login parameters")
	}
	session := p.traeManualLogins[result.State]
	if session == nil || session.callback != q.Get("auth_callback_url") {
		t.Fatal("missing login session")
	}
}

func TestTraeManualReturnValidation(t *testing.T) {
	const callback = "http://127.0.0.1:53001/authorize"
	const state = "this-login"
	const secret = "private+token/=value"
	good := manualTestReturn(callback, state, secret)
	if got, err := parseTraeManualReturn(good, state, callback); err != nil || got != secret {
		t.Fatal("valid callback rejected", err)
	}
	bad := []string{
		strings.Replace(good, "127.0.0.1", "203.0.113.7", 1),
		strings.Replace(good, ":53001", ":53002", 1),
		strings.Replace(good, "http:", "https:", 1),
		strings.Replace(good, "127.0.0.1", "user@127.0.0.1", 1),
		strings.Replace(good, "this-login", "old-login", 1),
		good + "&refreshToken=other", good + "#fragment", good + "&broken=%ZZ",
		callback + "?" + url.Values{"scope": {"trae"}, "loginTraceID": {state}, "accessToken": {"web-only"}}.Encode(),
	}
	for _, raw := range bad {
		_, err := parseTraeManualReturn(raw, state, callback)
		if err == nil {
			t.Fatal("invalid callback accepted")
		}
		if strings.Contains(err.Error(), secret) || strings.Contains(err.Error(), "private%2Btoken") {
			t.Fatal("credential leaked in error")
		}
	}
}

func TestTraeManualImportRetriesAndPreserves(t *testing.T) {
	dir := t.TempDir()
	fp := filepath.Join(dir, "data", "checkin_accounts.json")
	if err := os.MkdirAll(filepath.Dir(fp), 0700); err != nil {
		t.Fatal(err)
	}
	initial := `{"other_tool":{"keep":1234567890123456789},"accounts":[{"UserID":"12345","name":"Original","jwt":"old","device_id":"keep-device","added_at":"keep-date","custom":{"keep":true}},{"UserID":"67890","name":"Second","jwt":"other"}]}`
	if err := os.WriteFile(fp, []byte(initial), 0600); err != nil {
		t.Fatal(err)
	}
	p := New(Config{TraeDataDir: dir})
	const state = "test-login"
	const callback = "http://127.0.0.1:53002/authorize"
	p.traeManualLogins = map[string]*traeManualSession{state: {created: time.Now(), callback: callback}}
	raw := manualTestReturn(callback, state, "original-rt")
	jwt := "Cloud-IDE-JWT header." + base64.RawURLEncoding.EncodeToString([]byte(`{"data":{"id":12345},"exp":9999999999}`)) + ".signature"
	exchanges, statuses := 0, 0
	services := traeManualServices{
		exchange: func(rt string) (string, string, error) {
			exchanges++
			if rt != "original-rt" {
				t.Fatal("wrong refresh token")
			}
			return jwt, "rotated-rt", nil
		},
		status: func(a upstream.TraeAccount) (bool, float64, string, error) {
			statuses++
			if a.JWT != jwt || a.RefreshToken != "rotated-rt" {
				t.Fatal("wrong credential validation")
			}
			if statuses == 1 {
				return false, 0, "", fmt.Errorf("temporary status error with a secret")
			}
			return true, 5, "", nil
		},
	}
	if _, code, err := p.finishTraeManual(state, raw, services); err == nil || code != 502 {
		t.Fatal("unvalidated account accepted")
	}
	if b, _ := os.ReadFile(fp); string(b) != initial {
		t.Fatal("failed validation changed account file")
	}
	message, code, err := p.finishTraeManual(state, raw, services)
	if err != nil || code != 200 || !strings.Contains(message, "Original") || exchanges != 1 {
		t.Fatalf("retry failed: %d %v exchanges=%d", code, err, exchanges)
	}
	var file map[string]json.RawMessage
	b, _ := os.ReadFile(fp)
	if err := json.Unmarshal(b, &file); err != nil {
		t.Fatal(err)
	}
	var accounts []map[string]json.RawMessage
	json.Unmarshal(file["accounts"], &accounts)
	var custom map[string]bool
	if len(accounts) != 2 {
		t.Fatal("existing accounts lost")
	}
	json.Unmarshal(accounts[0]["custom"], &custom)
	if !custom["keep"] || string(accounts[0]["device_id"]) != "\"keep-device\"" || string(accounts[0]["added_at"]) != "\"keep-date\"" {
		t.Fatal("existing fields or accounts lost")
	}
	if !strings.Contains(string(file["other_tool"]), "1234567890123456789") {
		t.Fatal("top-level data changed")
	}
	if string(accounts[0]["refresh_token"]) != "\"rotated-rt\"" {
		t.Fatal("rotated token not saved")
	}
	if _, _, err := p.finishTraeManual(state, raw, services); err == nil || exchanges != 1 {
		t.Fatal("completed login replayed")
	}
}

func TestTraeManualExpiredAndBusy(t *testing.T) {
	p := New(Config{})
	p.traeManualLogins = map[string]*traeManualSession{
		"expired": {created: time.Now().Add(-traeLoginTTL - time.Second)},
		"busy":    {created: time.Now(), busy: true},
	}
	for state, want := range map[string]int{"expired": 400, "missing": 400, "busy": 409} {
		_, code, err := p.finishTraeManual(state, "", traeManualServices{})
		if err == nil || code != want {
			t.Fatalf("%s: %d %v", state, code, err)
		}
	}
}
