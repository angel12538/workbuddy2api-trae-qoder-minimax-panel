package ideauth

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
)

// useTempPoolDir 把号池指向临时目录，避免测试写真实 data/ide_accounts。
func useTempPoolDir(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("IDEAUTH_POOL_DIR", dir)
	return dir
}

func qoderPoolFileIn(t *testing.T, dir string) string {
	t.Helper()
	return filepath.Join(dir, "qoder_accounts.json")
}

// 验收④：20 个并发 UpdateQoderPool 各改不同账号，不得互相覆盖。
func TestUpdateQoderPoolConcurrent(t *testing.T) {
	useTempPoolDir(t)
	const n = 20

	seed := &QoderPoolFile{Accounts: make([]*QoderSnapshot, 0, n)}
	for i := 0; i < n; i++ {
		seed.Accounts = append(seed.Accounts, &QoderSnapshot{
			Email: fmt.Sprintf("user%02d@example.com", i),
			Name:  "before",
		})
	}
	if err := SaveQoderPool(seed); err != nil {
		t.Fatalf("SaveQoderPool: %v", err)
	}

	var wg sync.WaitGroup
	errCh := make(chan error, n)
	for i := 0; i < n; i++ {
		i := i
		wg.Add(1)
		go func() {
			defer wg.Done()
			email := fmt.Sprintf("user%02d@example.com", i)
			err := UpdateQoderPool(func(f *QoderPoolFile) error {
				for _, a := range f.Accounts {
					if a != nil && strings.EqualFold(a.Email, email) {
						a.Name = fmt.Sprintf("after-%02d", i)
						a.ClaimCID = "cid-" + email
						return nil
					}
				}
				return fmt.Errorf("账号 %s 不在池中", email)
			})
			if err != nil {
				errCh <- err
			}
		}()
	}
	wg.Wait()
	close(errCh)
	for err := range errCh {
		t.Fatalf("UpdateQoderPool 失败（说明并发下有更新丢失/报错）: %v", err)
	}

	got, err := LoadQoderPool()
	if err != nil {
		t.Fatalf("LoadQoderPool: %v", err)
	}
	if len(got.Accounts) != n {
		t.Fatalf("账号数 = %d，期望 %d", len(got.Accounts), n)
	}
	for i := 0; i < n; i++ {
		email := fmt.Sprintf("user%02d@example.com", i)
		want := fmt.Sprintf("after-%02d", i)
		found := false
		for _, a := range got.Accounts {
			if a == nil || !strings.EqualFold(a.Email, email) {
				continue
			}
			found = true
			if a.Name != want {
				t.Errorf("%s 的 Name = %q，期望 %q（更新被覆盖）", email, a.Name, want)
			}
			if a.ClaimCID != "cid-"+email {
				t.Errorf("%s 的 ClaimCID = %q，期望 %q", email, a.ClaimCID, "cid-"+email)
			}
		}
		if !found {
			t.Errorf("%s 在号池里消失了", email)
		}
	}
}

// fn 返回错误时不得写盘（原有内容必须原样保留）。
func TestUpdateQoderPoolFnErrorNoWrite(t *testing.T) {
	dir := useTempPoolDir(t)
	if err := SaveQoderPool(&QoderPoolFile{Accounts: []*QoderSnapshot{{Email: "keep@example.com", Name: "keep"}}}); err != nil {
		t.Fatalf("SaveQoderPool: %v", err)
	}
	path := qoderPoolFileIn(t, dir)
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("读取号池: %v", err)
	}

	boom := errors.New("boom")
	err = UpdateQoderPool(func(f *QoderPoolFile) error {
		f.Accounts = append(f.Accounts, &QoderSnapshot{Email: "should-not-exist@example.com"})
		return boom
	})
	if !errors.Is(err, boom) {
		t.Fatalf("UpdateQoderPool 应返回 fn 的错误，实际 %v", err)
	}
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("读取号池: %v", err)
	}
	if string(before) != string(after) {
		t.Fatalf("fn 出错后号池被改写了\n before=%s\n after=%s", before, after)
	}
}

// SaveQoderPool 保持按 Email 排序 + 原子写（0600）。
func TestSaveQoderPoolSorted(t *testing.T) {
	dir := useTempPoolDir(t)
	in := &QoderPoolFile{Accounts: []*QoderSnapshot{
		{Email: "c@example.com"},
		{Email: "a@example.com"},
		{Email: "b@example.com"},
	}}
	if err := SaveQoderPool(in); err != nil {
		t.Fatalf("SaveQoderPool: %v", err)
	}
	got, err := LoadQoderPool()
	if err != nil {
		t.Fatalf("LoadQoderPool: %v", err)
	}
	if len(got.Accounts) != 3 {
		t.Fatalf("账号数 = %d，期望 3", len(got.Accounts))
	}
	want := []string{"a@example.com", "b@example.com", "c@example.com"}
	for i, w := range want {
		if got.Accounts[i].Email != w {
			t.Errorf("第 %d 个账号 = %q，期望 %q（必须按 Email 排序）", i, got.Accounts[i].Email, w)
		}
	}
	// 临时文件不能残留。
	if _, err := os.Stat(qoderPoolFileIn(t, dir) + ".tmp"); !os.IsNotExist(err) {
		t.Errorf("原子写不应残留 .tmp 文件（stat err=%v）", err)
	}
	if runtime.GOOS != "windows" {
		st, err := os.Stat(qoderPoolFileIn(t, dir))
		if err != nil {
			t.Fatalf("stat: %v", err)
		}
		if perm := st.Mode().Perm(); perm != 0o600 {
			t.Errorf("号池文件权限 = %o，期望 600", perm)
		}
	}
}

// UpdateQoderPool 只改目标账号，不得动其他账号的字段。
func TestUpdateQoderPoolKeepsOtherAccounts(t *testing.T) {
	useTempPoolDir(t)
	if err := SaveQoderPool(&QoderPoolFile{Accounts: []*QoderSnapshot{
		{Email: "a@example.com", Name: "A", UserID: "uid-a", MachineID: "mid-a"},
		{Email: "b@example.com", Name: "B", UserID: "uid-b", MachineID: "mid-b"},
	}}); err != nil {
		t.Fatalf("SaveQoderPool: %v", err)
	}
	if err := UpdateQoderPool(func(f *QoderPoolFile) error {
		for _, a := range f.Accounts {
			if a != nil && a.Email == "a@example.com" {
				a.Name = "A2"
			}
		}
		return nil
	}); err != nil {
		t.Fatalf("UpdateQoderPool: %v", err)
	}
	got, err := LoadQoderPool()
	if err != nil {
		t.Fatalf("LoadQoderPool: %v", err)
	}
	for _, a := range got.Accounts {
		switch a.Email {
		case "a@example.com":
			if a.Name != "A2" {
				t.Errorf("a 的 Name = %q，期望 A2", a.Name)
			}
			if a.UserID != "uid-a" || a.MachineID != "mid-a" {
				t.Errorf("更新 a 时丢了其他字段: %+v", a)
			}
		case "b@example.com":
			if a.Name != "B" || a.UserID != "uid-b" || a.MachineID != "mid-b" {
				t.Errorf("b 被误改: %+v", a)
			}
		default:
			t.Errorf("出现意外账号: %+v", a)
		}
	}
}
