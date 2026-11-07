package panel

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/ideauth"
)

type qdTestTransport func(*http.Request) (*http.Response, error)

func (f qdTestTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }
func qdResponse(status int, body string) *http.Response {
	return &http.Response{StatusCode: status, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(body))}
}

func qdStart(t *testing.T, region string) (string, *qoderDeviceSession, map[string]any) {
	t.Helper()
	w := httptest.NewRecorder()
	(&Panel{}).qoderDeviceStart(w, httptest.NewRequest("POST", "/", strings.NewReader(`{"region":"`+region+`"}`)))
	if w.Code != 200 {
		t.Fatalf("start status %d", w.Code)
	}
	var d map[string]any
	_ = json.Unmarshal(w.Body.Bytes(), &d)
	state := d["state"].(string)
	qoderDeviceSessions.Lock()
	s := qoderDeviceSessions.by[state]
	qoderDeviceSessions.Unlock()
	t.Cleanup(func() {
		qoderDeviceSessions.Lock()
		delete(qoderDeviceSessions.by, state)
		qoderDeviceSessions.Unlock()
	})
	return state, s, d
}

func TestQoderDeviceStartPKCE(t *testing.T) {
	for _, region := range []string{"cn", "intl"} {
		_, session, data := qdStart(t, region)
		u, err := url.Parse(data["auth_url"].(string))
		if err != nil {
			t.Fatal(err)
		}
		hash := sha256.Sum256([]byte(session.verifier))
		if u.Query().Get("challenge") != base64.RawURLEncoding.EncodeToString(hash[:]) || u.Query().Get("challenge_method") != "S256" {
			t.Fatal("invalid PKCE challenge")
		}
		if strings.Contains(u.RawQuery, session.verifier) {
			t.Fatal("verifier exposed")
		}
		if region == "cn" && u.Query().Get("redirect_uri") != "qoder-work-cn://" {
			t.Fatal("CN redirect missing")
		}
		if region == "intl" && (u.Query().Has("redirect_uri") || strings.Contains(session.nonce, "-")) {
			t.Fatal("incorrect intl protocol")
		}
	}
	w := httptest.NewRecorder()
	(&Panel{}).qoderDeviceStart(w, httptest.NewRequest("POST", "/", strings.NewReader(`{"region":"other"}`)))
	if w.Code != 400 {
		t.Fatal("invalid region accepted")
	}
}

func TestQoderDevicePendingAndImport(t *testing.T) {
	poolDir := t.TempDir()
	t.Setenv("IDEAUTH_POOL_DIR", poolDir)
	state, _, _ := qdStart(t, "cn")
	original := http.DefaultTransport
	t.Cleanup(func() { http.DefaultTransport = original })
	polls, identities := 0, 0
	http.DefaultTransport = qdTestTransport(func(r *http.Request) (*http.Response, error) {
		if r.URL.Host != "openapi.qoder.com.cn" {
			t.Fatal("unexpected host")
		}
		if strings.HasSuffix(r.URL.Path, "/poll") {
			polls++
			if polls == 1 {
				return qdResponse(404, ""), nil
			}
			return qdResponse(200, `{"device_token":"test-access","refresh_token":"test-refresh","expires_in":3600000}`), nil
		}
		identities++
		if r.Header.Get("Authorization") != "Bearer test-access" {
			t.Fatal("wrong bearer")
		}
		return qdResponse(200, `{"id":"test-id","email":"person@example.com","name":"Test"}`), nil
	})
	poll := func() *httptest.ResponseRecorder {
		w := httptest.NewRecorder()
		(&Panel{}).qoderDevicePoll(w, httptest.NewRequest("GET", "/?state="+state, nil))
		return w
	}
	w := poll()
	if !strings.Contains(w.Body.String(), `"done":false`) {
		t.Fatal("404 should be pending")
	}
	if _, err := os.Stat(filepath.Join(poolDir, "qoder_accounts.json")); !os.IsNotExist(err) {
		t.Fatal("pending saved account")
	}
	var wg sync.WaitGroup
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			w := poll()
			if w.Code != 200 || !strings.Contains(w.Body.String(), `"ok":true`) {
				t.Error("import failed")
			}
			if strings.Contains(w.Body.String(), "test-access") || strings.Contains(w.Body.String(), "test-refresh") {
				t.Error("credential leaked")
			}
		}()
	}
	wg.Wait()
	if polls != 2 || identities != 1 {
		t.Fatal("duplicate poll consumed grant twice")
	}
	b, err := os.ReadFile(filepath.Join(poolDir, "qoder_accounts.json"))
	if err != nil {
		t.Fatal(err)
	}
	var pool struct{ Accounts []any }
	_ = json.Unmarshal(b, &pool)
	if len(pool.Accounts) != 1 {
		t.Fatal("wrong account count")
	}
}

func TestQoderDeviceIdentityRetryAndExpiry(t *testing.T) {
	t.Setenv("IDEAUTH_POOL_DIR", t.TempDir())
	state, session, _ := qdStart(t, "intl")
	original := http.DefaultTransport
	t.Cleanup(func() { http.DefaultTransport = original })
	polls, identities := 0, 0
	http.DefaultTransport = qdTestTransport(func(r *http.Request) (*http.Response, error) {
		if strings.HasSuffix(r.URL.Path, "/poll") {
			polls++
			return qdResponse(200, `{"token":"test-access"}`), nil
		}
		identities++
		if identities == 1 {
			return qdResponse(502, `{"error":"test-access-secret-body"}`), nil
		}
		return qdResponse(200, `{"data":{"id":"test-id","email":"retry@example.com"}}`), nil
	})
	w := httptest.NewRecorder()
	(&Panel{}).qoderDevicePoll(w, httptest.NewRequest("GET", "/?state="+state, nil))
	if w.Code != 502 || strings.Contains(w.Body.String(), "test-access") {
		t.Fatal("bad error handling")
	}
	w = httptest.NewRecorder()
	(&Panel{}).qoderDevicePoll(w, httptest.NewRequest("GET", "/?state="+state, nil))
	if w.Code != 200 || polls != 1 || identities != 2 {
		t.Fatal("issued grant was not retained for retry")
	}
	session.created = time.Now().Add(-11 * time.Minute)
	w = httptest.NewRecorder()
	(&Panel{}).qoderDevicePoll(w, httptest.NewRequest("GET", "/?state="+state, nil))
	if !strings.Contains(w.Body.String(), `"ok":false`) {
		t.Fatal("expired session accepted")
	}
}

func TestQoderDeviceVerifiedProfileWithoutEmail(t *testing.T) {
	t.Setenv("IDEAUTH_POOL_DIR", t.TempDir())
	original := http.DefaultTransport
	t.Cleanup(func() { http.DefaultTransport = original })
	http.DefaultTransport = qdTestTransport(func(r *http.Request) (*http.Response, error) {
		if strings.HasSuffix(r.URL.Path, "/poll") {
			return qdResponse(200, `{"token":"test-access","user_id":"test-id","refresh_token":"test-refresh","expires_in":3600000}`), nil
		}
		if r.Header.Get("Authorization") != "Bearer test-access" {
			t.Fatal("identity request missing authentication")
		}
		// Same observed field structure as the live CN API, with synthetic values.
		return qdResponse(200, `{"id":"test-id","name":"Test","username":"test-name","avatar":"test-avatar","organization_id":"","third_party_identities":[]}`), nil
	})
	for i := 0; i < 2; i++ {
		state, _, _ := qdStart(t, "cn")
		w := httptest.NewRecorder()
		(&Panel{}).qoderDevicePoll(w, httptest.NewRequest("GET", "/?state="+state, nil))
		if w.Code != 200 || !strings.Contains(w.Body.String(), `"ok":true`) {
			t.Fatalf("UID-only verified profile rejected: HTTP %d", w.Code)
		}
		if strings.Contains(w.Body.String(), "test-access") || strings.Contains(w.Body.String(), "test-refresh") {
			t.Fatal("credential leaked")
		}
	}
	pool, err := ideauth.LoadQoderPool()
	if err != nil {
		t.Fatal(err)
	}
	if len(pool.Accounts) != 1 || pool.Accounts[0].Auth.User.Email != "" || pool.Accounts[0].Auth.User.ID != "test-id" || pool.Accounts[0].Auth.Realm != "cn" {
		t.Fatal("UID-only profile not persisted accurately")
	}
	if pool.Accounts[0].Auth.User.AvatarURL != "test-avatar" {
		t.Fatal("observed avatar field not supported")
	}
	exp, err := time.Parse(time.RFC3339, pool.Accounts[0].Auth.ExpiresAt)
	if err != nil || time.Until(exp) < 59*time.Minute || time.Until(exp) > 61*time.Minute {
		t.Fatal("numeric JSON expiry not preserved")
	}
}

func TestQoderDeviceIdentityAliasesAndValidation(t *testing.T) {
	for _, test := range []struct {
		name, user, grant string
		ok                bool
		wantID            string
	}{
		{"integer_uid", `{"id":9007199254740993,"name":"Test"}`, `{"token":"test-access"}`, true, "9007199254740993"},
		{"userId", `{"userId":"alias-id","name":"Test"}`, `{"token":"test-access"}`, true, "alias-id"},
		{"uid", `{"uid":"alias-id","name":"Test"}`, `{"token":"test-access"}`, true, "alias-id"},
		{"grant_uid", `{"name":"Test"}`, `{"token":"test-access","user_id":"grant-id"}`, true, "grant-id"},
		{"missing_uid", `{"name":"Test"}`, `{"token":"test-access"}`, false, ""},
		{"conflicting_uid", `{"id":"one"}`, `{"token":"test-access","user_id":"two"}`, false, ""},
		{"identity_rejected", `{"id":"claimed-id","name":"Test"}`, `{"token":"test-access"}`, false, ""},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Setenv("IDEAUTH_POOL_DIR", t.TempDir())
			state, _, _ := qdStart(t, "cn")
			original := http.DefaultTransport
			t.Cleanup(func() { http.DefaultTransport = original })
			http.DefaultTransport = qdTestTransport(func(r *http.Request) (*http.Response, error) {
				if strings.HasSuffix(r.URL.Path, "/poll") {
					return qdResponse(200, test.grant), nil
				}
				status := 200
				if test.name == "identity_rejected" {
					status = 401
				}
				return qdResponse(status, test.user), nil
			})
			w := httptest.NewRecorder()
			(&Panel{}).qoderDevicePoll(w, httptest.NewRequest("GET", "/?state="+state, nil))
			pool, err := ideauth.LoadQoderPool()
			if err != nil {
				t.Fatal(err)
			}
			if test.ok {
				if w.Code != 200 || len(pool.Accounts) != 1 || pool.Accounts[0].UserID != test.wantID {
					t.Fatal("valid identity not saved exactly")
				}
			} else {
				if w.Code == 200 || len(pool.Accounts) != 0 {
					t.Fatal("unverified identity saved")
				}
			}
		})
	}
}
