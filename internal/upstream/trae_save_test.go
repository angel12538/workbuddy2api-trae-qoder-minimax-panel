package upstream

import (
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"
)

func TestSaveTraeAccountRefusesCorruptFile(t *testing.T) {
	for _, raw := range []string{`{broken`, `null`, `{"accounts":[null]}`, `{"accounts":"bad"}`} {
		dir := t.TempDir()
		fp := filepath.Join(dir, "data", "checkin_accounts.json")
		os.MkdirAll(filepath.Dir(fp), 0700)
		os.WriteFile(fp, []byte(raw), 0600)
		if _, err := SaveTraeAccount(dir, TraeAccount{UserID: "1", JWT: "token"}); err == nil {
			t.Fatal("corrupt file accepted")
		}
		if after, _ := os.ReadFile(fp); string(after) != raw {
			t.Fatal("corrupt file overwritten")
		}
	}
}

func TestSaveTraeAccountConcurrent(t *testing.T) {
	dir := t.TempDir()
	var wg sync.WaitGroup
	for i := range 12 {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			if _, err := SaveTraeAccount(dir, TraeAccount{UserID: fmt.Sprint(i), JWT: "token"}); err != nil {
				t.Error(err)
			}
		}(i)
	}
	wg.Wait()
	accounts, err := LoadTraeAccounts(dir)
	if err != nil || len(accounts) != 12 {
		t.Fatalf("lost concurrent updates: %d %v", len(accounts), err)
	}
}
