package ideauth

import (
	"encoding/json"
	"os"
	"testing"
)

func TestImportQoderDeviceAuthPreservesPool(t *testing.T) {
	t.Setenv("IDEAUTH_POOL_DIR", t.TempDir())
	original := `{"future_root":42,"accounts":[{"email":"person@example.com","machine_id":"existing-machine","scope":"existing-scope","future_account":"keep","auth":{"future_auth":true,"token":"old"}},{"email":"other@example.com","future_other":[1,2]}]}`
	if err := os.WriteFile(qoderPoolPath(), []byte(original), 0600); err != nil {
		t.Fatal(err)
	}
	a := &QoderAuth{Token: "test-access", RefreshToken: "test-refresh", User: QoderUser{Email: "person@example.com", ID: "test-user", Name: "Test"}}
	for i := 0; i < 2; i++ {
		if err := ImportQoderDeviceAuth(a, "new-machine"); err != nil {
			t.Fatal(err)
		}
	}
	b, _ := os.ReadFile(qoderPoolPath())
	var pool map[string]any
	if err := json.Unmarshal(b, &pool); err != nil {
		t.Fatal(err)
	}
	rows := pool["accounts"].([]any)
	if len(rows) != 2 || pool["future_root"] != float64(42) {
		t.Fatal("pool structure lost")
	}
	row := rows[0].(map[string]any)
	if row["future_account"] != "keep" || row["scope"] != "existing-scope" || row["machine_id"] != "existing-machine" {
		t.Fatal("account fields lost")
	}
	auth := row["auth"].(map[string]any)
	if auth["future_auth"] != true || auth["token"] != "test-access" {
		t.Fatal("auth merge failed")
	}
	before := string(b)
	if ImportQoderDeviceAuth(&QoderAuth{}, "") == nil {
		t.Fatal("incomplete identity accepted")
	}
	b, _ = os.ReadFile(qoderPoolPath())
	if string(b) != before {
		t.Fatal("invalid import changed pool")
	}
}

func TestImportQoderDeviceAuthDoesNotOverwriteCorruptPool(t *testing.T) {
	t.Setenv("IDEAUTH_POOL_DIR", t.TempDir())
	original := []byte(`{"accounts":"corrupt"}`)
	if err := os.WriteFile(qoderPoolPath(), original, 0600); err != nil {
		t.Fatal(err)
	}
	a := &QoderAuth{Token: "test", User: QoderUser{Email: "person@example.com", ID: "test"}}
	if ImportQoderDeviceAuth(a, "") == nil {
		t.Fatal("corrupt pool accepted")
	}
	after, _ := os.ReadFile(qoderPoolPath())
	if string(after) != string(original) {
		t.Fatal("corrupt pool overwritten")
	}
}

func TestImportQoderDeviceAuthWithoutEmail(t *testing.T) {
	t.Setenv("IDEAUTH_POOL_DIR", t.TempDir())
	a := &QoderAuth{Realm: "cn", Token: "test-access", RefreshToken: "test-refresh", User: QoderUser{ID: "test-user", Name: "Test"}}
	for i := 0; i < 2; i++ {
		if err := ImportQoderDeviceAuth(a, "test-machine"); err != nil {
			t.Fatal(err)
		}
	}
	pool, err := LoadQoderPool()
	if err != nil {
		t.Fatal(err)
	}
	if len(pool.Accounts) != 1 {
		t.Fatal("UID import must deduplicate")
	}
	snap := pool.Accounts[0]
	if snap.Email != "qoder:cn:test-user" || snap.Auth.User.Email != "" || snap.Auth.AccountKey != snap.Email || QoderIdentityKey(snap.Auth) != snap.Email {
		t.Fatal("missing email was fabricated or routing key is unstable")
	}
	views, err := ListQoderAccounts()
	if err != nil {
		t.Fatal(err)
	}
	if len(views) != 1 || views[0].IdentityLabel != "UID test-user" {
		t.Fatal("UID identity is not presented truthfully")
	}
	if len(qoderPoolItems(pool, nil)) != 1 || qoderPoolAuth(pool.Accounts, snap.Email) == nil {
		t.Fatal("UID-only account cannot participate in pool operations")
	}
}

func TestImportQoderDeviceAuthMatchesUIDAndPreservesLegacyKeys(t *testing.T) {
	t.Setenv("IDEAUTH_POOL_DIR", t.TempDir())
	original := `{"future":true,"accounts":[{"email":"legacy@example.com","user_id":"test-user","name":"Known","scope":"preserved","future_account":17,"auth":{"token":"old","user":{"id":"test-user","email":"legacy@example.com","name":"Known","future_user":42}}},{"email":"other@example.com","user_id":"other-user","future_other":true}]}`
	if err := os.WriteFile(qoderPoolPath(), []byte(original), 0600); err != nil {
		t.Fatal(err)
	}
	a := &QoderAuth{Realm: "cn", Token: "new", User: QoderUser{ID: "test-user"}}
	if err := ImportQoderDeviceAuth(a, "machine"); err != nil {
		t.Fatal(err)
	}
	if a.AccountKey != "legacy@example.com" || a.User.Email != "legacy@example.com" || a.User.Name != "Known" {
		t.Fatal("known identity metadata lost")
	}
	b, _ := os.ReadFile(qoderPoolPath())
	var root map[string]any
	_ = json.Unmarshal(b, &root)
	rows := root["accounts"].([]any)
	row := rows[0].(map[string]any)
	if len(rows) != 2 || row["scope"] != "preserved" || row["future_account"] != float64(17) || row["auth"].(map[string]any)["user"].(map[string]any)["future_user"] != float64(42) {
		t.Fatal("existing account fields lost")
	}
}

func TestImportQoderDeviceAuthDoesNotMergeDifferentUIDOrRealm(t *testing.T) {
	t.Setenv("IDEAUTH_POOL_DIR", t.TempDir())
	for _, a := range []*QoderAuth{
		{Realm: "cn", Token: "one", User: QoderUser{ID: "one", Email: "same@example.com"}},
		{Realm: "cn", Token: "two", User: QoderUser{ID: "two", Email: "same@example.com"}},
		{Realm: "intl", Token: "three", User: QoderUser{ID: "one", Email: "same@example.com"}},
	} {
		if err := ImportQoderDeviceAuth(a, "machine"); err != nil {
			t.Fatal(err)
		}
	}
	pool, err := LoadQoderPool()
	if err != nil {
		t.Fatal(err)
	}
	if len(pool.Accounts) != 3 {
		t.Fatal("different authoritative identities merged")
	}
	seen := map[string]bool{}
	for _, a := range pool.Accounts {
		if seen[a.Email] {
			t.Fatal("routing key collision")
		}
		seen[a.Email] = true
	}
}
