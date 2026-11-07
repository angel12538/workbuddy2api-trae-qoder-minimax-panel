package ideauth

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func serverPool(t *testing.T) string {
	t.Helper()
	t.Setenv("QODER_SERVER_MODE", "1")
	dir := useTempPoolDir(t)
	t.Setenv("QODER_DATA_DIR", t.TempDir())
	raw := `{"future":{"keep":true},"accounts":[null,{"email":"a@example.com","user_id":"a","name":"A","scope":"cn:scope-a","future_account":7,"auth":{"token":"token-a","refreshToken":"rt-a","realm":"cn","future_auth":8,"user":{"id":"a","email":"a@example.com","future_user":9}}},{"email":"qoder:cn:b","user_id":"b","name":"B","auth":{"token":"token-b","refreshToken":"rt-b","realm":"cn","user":{"id":"b","name":"B"}}},{"email":"broken","auth":null}]}`
	if err := os.WriteFile(qoderPoolPath(), []byte(raw), 0600); err != nil {
		t.Fatal(err)
	}
	return dir
}

func TestQoderServerSwitchAndRenewal(t *testing.T) {
	dir := serverPool(t)
	before, _ := os.ReadFile(qoderPoolPath())
	if _, err := ReadQoderAuth(); err == nil {
		t.Fatal("must explicitly select a current account")
	}
	if _, err := SwitchQoderTo("A@EXAMPLE.COM", true); err != nil {
		t.Fatal(err)
	}
	a, err := ReadQoderAuth()
	if err != nil || a.Token != "token-a" {
		t.Fatalf("wrong current auth: %v", err)
	}
	if _, err := SwitchQoderTo("qoder:cn:b", false); err != nil {
		t.Fatal(err)
	}
	views, err := ListQoderAccounts()
	if err != nil {
		t.Fatal(err)
	}
	count := 0
	for _, v := range views {
		if v.Current {
			count++
			if v.UserID != "b" {
				t.Fatal("UID-only current account mismatch")
			}
		}
	}
	if count != 1 {
		t.Fatalf("current count %d", count)
	}
	after, _ := os.ReadFile(qoderPoolPath())
	if string(before) != string(after) {
		t.Fatal("selection rewrote credentials or metadata")
	}
	pointer, _ := os.ReadFile(filepath.Join(dir, "qoder_current.json"))
	if strings.Contains(string(pointer), "token") {
		t.Fatal("selection file duplicated credentials")
	}
	// A renewal completing after B was selected must update only A.
	a.Token, a.RefreshToken = "rotated-a", "rotated-rt-a"
	if err := WriteQoderAuth(a); err != nil {
		t.Fatal(err)
	}
	b, err := ReadQoderAuth()
	if err != nil || b.Token != "token-b" || b.User.ID != "b" {
		t.Fatal("renewal changed selection")
	}
	if _, err := SwitchQoderTo("a@example.com", false); err != nil {
		t.Fatal(err)
	}
	a, err = ReadQoderAuth()
	if err != nil || a.Token != "rotated-a" {
		t.Fatal("current auth must use latest pool token")
	}
	raw, _ := os.ReadFile(qoderPoolPath())
	var root map[string]any
	if err := json.Unmarshal(raw, &root); err != nil {
		t.Fatal(err)
	}
	row := root["accounts"].([]any)[1].(map[string]any)
	if root["future"] == nil || row["future_account"] != float64(7) || row["auth"].(map[string]any)["future_auth"] != float64(8) || row["auth"].(map[string]any)["user"].(map[string]any)["future_user"] != float64(9) {
		t.Fatal("unknown metadata lost")
	}
	if _, err := os.Stat(filepath.Join(QoderDataDir(), "auth.v1.dat")); !os.IsNotExist(err) {
		t.Fatal("server must not write IDE auth file")
	}
}

func TestQoderServerInvalidSelectionAndRemovedAccount(t *testing.T) {
	serverPool(t)
	if _, err := SwitchQoderTo("a@example.com", false); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"missing", "broken", ""} {
		if _, err := SwitchQoderTo(key, false); err == nil {
			t.Fatal("accepted invalid account")
		}
		a, err := ReadQoderAuth()
		if err != nil || a.User.ID != "a" {
			t.Fatal("failed selection changed current account")
		}
	}
	a, _ := ReadQoderAuth()
	if err := RemoveQoderAccount("a@example.com"); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadQoderAuth(); err == nil {
		t.Fatal("removed account remained current")
	}
	if err := WriteQoderAuth(a); err == nil {
		t.Fatal("renewal resurrected removed account")
	}
}

func TestQoderServerCurrentUsageAndRenewal(t *testing.T) {
	serverPool(t)
	seen := ""
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == qoderPathUsage {
			seen = r.Header.Get("Authorization")
			_, _ = w.Write([]byte(`{"plan_tier":"free"}`))
			return
		}
		if r.URL.Path == qoderPathDeviceRefresh {
			var body map[string]string
			_ = json.NewDecoder(r.Body).Decode(&body)
			if body["refresh_token"] != "rt-b" {
				t.Error("renewed wrong account")
			}
			_, _ = w.Write([]byte(`{"device_token":"new-b","refresh_token":"new-rt-b","expires_at":"2099-01-01T00:00:00Z"}`))
			return
		}
		http.NotFound(w, r)
	}))
	defer srv.Close()
	t.Setenv("QODER_OPENAPI_BASE", srv.URL)
	if _, err := SwitchQoderTo("a@example.com", false); err != nil {
		t.Fatal(err)
	}
	if r := QoderUsage(); r.Phase != "ok" || seen != "Bearer token-a" {
		t.Fatal("usage did not use A")
	}
	if _, err := SwitchQoderTo("qoder:cn:b", false); err != nil {
		t.Fatal(err)
	}
	if r := QoderUsage(); r.Phase != "ok" || seen != "Bearer token-b" {
		t.Fatal("usage did not use B")
	}
	if r := QoderRenew(true); r.Phase != "renewed" {
		t.Fatalf("renewal failed: %s", r.Msg)
	}
	a, err := ReadQoderAuth()
	if err != nil || a.Token != "new-b" || a.RefreshToken != "new-rt-b" {
		t.Fatal("renewal did not persist in pool")
	}
}
