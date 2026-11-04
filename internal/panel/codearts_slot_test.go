// codearts_slot_test.go 身份槽生命周期与「槽内执行」的单测。
//
// 全部隔离：CODEARTS_SLOT_ROOT / CODEARTS_POOL_FILE 都指到 t.TempDir()，
// 不读写真实 data/，不启动浏览器、不打上游接口（PREFERENCE_3）。
package panel

import (
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/linguo2625469/workbuddy2api-panel/internal/codearts"
)

// codeartsSlotTestEnv 造一个完全隔离的槽根 + 号池文件。
func codeartsSlotTestEnv(t *testing.T) string {
	t.Helper()
	slotRoot := t.TempDir()
	t.Setenv("CODEARTS_SLOT_ROOT", slotRoot)
	t.Setenv("CODEARTS_POOL_FILE", filepath.Join(t.TempDir(), "codearts_accounts.json"))
	t.Setenv("IDEAUTH_POOL_DIR", t.TempDir())
	return slotRoot
}

// codeartsSlotTestAccount 造一个最小的 CodeArts 账号（存量字段 + 会话 Cookie）。
func codeartsSlotTestAccount(id, domain, user string) *codearts.Account {
	return &codearts.Account{
		ID:       id,
		Label:    "CodeArts " + id,
		Identity: domain + "|" + user,
		Cookies: []codearts.Cookie{
			caCk("domain_tag", domain),
			caCk("user_tag", user),
			caCk("SessionID", "sess-"+id),
		},
		SavedAt: codearts.Now(),
	}
}

// codeartsSlotTestSavePool 把账号写进临时号池（SlotForget / 登录路径都要先有记录）。
func codeartsSlotTestSavePool(t *testing.T, accounts ...*codearts.Account) {
	t.Helper()
	if err := codearts.SavePool(&codearts.Pool{Accounts: accounts}); err != nil {
		t.Fatalf("写临时号池失败: %v", err)
	}
}

// TestCodeArtsSlotDirPerAccountIdempotent 两个号路径必须不同；同一号两次调用
// 路径相同且幂等（目录已存在也不报错，slot.json 不被重写）。
func TestCodeArtsSlotDirPerAccountIdempotent(t *testing.T) {
	slotRoot := codeartsSlotTestEnv(t)
	p := &Panel{}

	first := codeartsSlotTestAccount("acct-alpha", "DOMAIN-A", "USER-A")
	second := codeartsSlotTestAccount("acct-beta", "DOMAIN-B", "USER-B")

	dirFirst, err := p.codeartsSlotDir(first)
	if err != nil {
		t.Fatalf("首个账号建槽失败: %v", err)
	}
	dirSecond, err := p.codeartsSlotDir(second)
	if err != nil {
		t.Fatalf("第二个账号建槽失败: %v", err)
	}
	if dirFirst == dirSecond {
		t.Fatalf("两个账号拿到了同一个槽目录: %s", dirFirst)
	}
	if !filepath.IsAbs(dirFirst) {
		t.Fatalf("槽目录必须是绝对路径，实际 %q", dirFirst)
	}
	if !codeartsDirExists(dirFirst) {
		t.Fatalf("首次调用必须创建槽目录，实际不存在: %s", dirFirst)
	}
	if !p.codeartsSlotExists(first) {
		t.Fatal("建槽后 codeartsSlotExists 必须为 true")
	}
	if p.codeartsSlotExists(&codearts.Account{ID: "never-created"}) {
		t.Fatal("没建过的账号 codeartsSlotExists 必须为 false")
	}
	if !strings.HasPrefix(dirFirst, slotRoot) {
		t.Fatalf("槽目录必须落在 CODEARTS_SLOT_ROOT 下（%s），实际 %s", slotRoot, dirFirst)
	}

	// 幂等：第二次调用路径相同，slot.json 不被覆盖（DeviceGUID 与号池一致）。
	again, err := p.codeartsSlotDir(first)
	if err != nil {
		t.Fatalf("重复调用建槽必须成功: %v", err)
	}
	if again != dirFirst {
		t.Fatalf("同号两次调用路径不一致: %q != %q", again, dirFirst)
	}
	metaPath := filepath.Join(filepath.Dir(dirFirst), "slot.json")
	raw, err := os.ReadFile(metaPath)
	if err != nil {
		t.Fatalf("槽元数据 slot.json 未生成: %v", err)
	}
	if !strings.Contains(string(raw), first.DeviceGUID) {
		t.Fatalf("slot.json 里的 device_guid 与账号不一致: %s", raw)
	}

	// 概览：两个账号各一行，slot_dir 是绝对路径、exists 为真。
	overview := p.codeartsSlotsOverview()
	if len(overview) != 0 {
		// 两个账号还没进号池 → 概览只列举号池里的账号，这里应为空。
		t.Fatalf("号池为空时概览应为空，实际 %d 行", len(overview))
	}
	codeartsSlotTestSavePool(t, first, second)
	// 给第一个槽写一个"设备身份"文件：概览的 profile_bytes 要真的统计到内容。
	if werr := os.WriteFile(filepath.Join(dirFirst, "Local State"),
		[]byte(`{"os_crypt":{"encrypted_key":"device-key"}}`), 0o600); werr != nil {
		t.Fatalf("写槽内文件失败: %v", werr)
	}
	overview = p.codeartsSlotsOverview()
	if len(overview) != 2 {
		t.Fatalf("概览应有 2 行，实际 %d", len(overview))
	}
	seen := map[string]bool{}
	for _, info := range overview {
		if !info.Exists {
			t.Fatalf("账号 %s 的槽应已存在", info.ID)
		}
		if !filepath.IsAbs(info.SlotDir) {
			t.Fatalf("概览里的 slot_dir 必须是绝对槽目录，实际 %q", info.SlotDir)
		}

		if info.DeviceGUID == "" || info.PersonKey == "" || info.ClientSource == "" {
			t.Fatalf("概览缺字段（device_guid/person_key/client_source）: %+v", info)
		}
		// 有内容的那条必须统计到大小（空 profile 为 0 是正常现象）。
		if info.ID == first.ID && info.ProfileBytes <= 0 {
			t.Fatalf("账号 %s 的槽里已写入设备文件，profile_bytes 必须 > 0", info.ID)
		}
		seen[info.SlotDir] = true
	}
	if len(seen) != 2 {
		t.Fatalf("概览里两个账号的槽目录必须不同，实际 %v", seen)
	}
}

// TestCodeArtsSlotForgetArchivesAndRotatesGUID 重置身份：旧槽改名 .bak-*、
// 新槽 GUID 必须变化、账号本身（ID/Cookie）保持不变。
func TestCodeArtsSlotForgetArchivesAndRotatesGUID(t *testing.T) {
	codeartsSlotTestEnv(t)
	p := &Panel{}

	acc := codeartsSlotTestAccount("acct-forget", "DOMAIN-F", "USER-F")
	codeartsSlotTestSavePool(t, acc)

	dir, err := p.codeartsSlotDir(acc)
	if err != nil {
		t.Fatalf("建槽失败: %v", err)
	}
	// 模拟"用过的槽"：写一个设备身份文件进去，确认备份里还在。
	deviceFile := filepath.Join(dir, "Local State")
	if werr := os.WriteFile(deviceFile, []byte(`{"os_crypt":{"encrypted_key":"x"}}`), 0o600); werr != nil {
		t.Fatalf("写槽内文件失败: %v", werr)
	}
	oldSlotDir := filepath.Dir(dir)
	oldSlotID := acc.SlotID
	oldGUID := acc.DeviceGUID
	cookiesBefore := len(acc.Cookies)

	if err := p.codeartsSlotForget(acc); err != nil {
		t.Fatalf("重置身份槽失败: %v", err)
	}

	if codeartsDirExists(oldSlotDir) {
		t.Fatalf("旧槽目录应已改名，实际仍存在: %s", oldSlotDir)
	}
	backups := codeartsFindBackups(t, filepath.Dir(oldSlotDir), filepath.Base(oldSlotDir)+".bak-")
	if len(backups) != 1 {
		t.Fatalf("应有一个 .bak-* 备份，实际 %d 个: %v", len(backups), backups)
	}
	backupDevice := filepath.Join(backups[0], "profile", "Local State")
	if _, serr := os.Stat(backupDevice); serr != nil {
		t.Fatalf("备份里必须保留原 profile 内容（可回滚），实际读不到 %s: %v", backupDevice, serr)
	}

	if acc.DeviceGUID == oldGUID {
		t.Fatalf("重置后 DeviceGUID 必须变化（否则仍是同一台设备），实际还是 %s", oldGUID)
	}
	if acc.SlotID == oldSlotID {
		t.Fatalf("重置后 SlotID 必须变化，实际还是 %s", oldSlotID)
	}
	if acc.ID != "acct-forget" || len(acc.Cookies) != cookiesBefore {
		t.Fatalf("重置身份不能动 ID/Cookie（那是删账号，不是换设备）: id=%s cookies=%d",
			acc.ID, len(acc.Cookies))
	}

	// 号池里同步成新槽。
	pool, lerr := codearts.LoadPool()
	if lerr != nil {
		t.Fatalf("读号池失败: %v", lerr)
	}
	stored := codeartsFindAccount(pool, acc.ID)
	if stored == nil {
		t.Fatal("重置后账号不能从号池消失")
	}
	if stored.SlotID != acc.SlotID || stored.DeviceGUID != acc.DeviceGUID {
		t.Fatalf("号池记录的槽字段未同步: slot_id=%s guid=%s", stored.SlotID, stored.DeviceGUID)
	}
	if stored.SlotDir != acc.SlotDir {
		t.Fatalf("号池记录的 SlotDir 未同步: %q != %q", stored.SlotDir, acc.SlotDir)
	}

	// 新槽按需重建，且与旧目录不同。
	newDir, derr := p.codeartsSlotDir(acc)
	if derr != nil {
		t.Fatalf("重置后建新槽失败: %v", derr)
	}
	if newDir == dir {
		t.Fatalf("重置后仍指回旧目录: %s", newDir)
	}
	if !codeartsDirExists(newDir) {
		t.Fatalf("新槽目录未创建: %s", newDir)
	}
}

// codeartsFindBackups 在 dir 下找前缀匹配的 .bak-* 目录（不用 Glob：
// Windows 上反斜杠与通配符语义容易踩坑）。
func codeartsFindBackups(t *testing.T, dir, prefix string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("读目录 %s 失败: %v", dir, err)
	}
	var out []string
	for _, e := range entries {
		if e.IsDir() && strings.HasPrefix(e.Name(), prefix) {
			out = append(out, filepath.Join(dir, e.Name()))
		}
	}
	return out
}

// TestCodeArtsSlotConcurrentEnsureSingleDirectory 20 个 goroutine 并发建同一个
// 账号的槽：不 panic、路径一致、槽根下只出现一个目录。
func TestCodeArtsSlotConcurrentEnsureSingleDirectory(t *testing.T) {
	slotRoot := codeartsSlotTestEnv(t)
	p := &Panel{}
	acc := codeartsSlotTestAccount("acct-race", "DOMAIN-R", "USER-R")

	const workers = 20
	dirs := make([]string, workers)
	errs := make([]error, workers)
	var wg sync.WaitGroup
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			dirs[idx], errs[idx] = p.codeartsSlotDir(acc)
		}(i)
	}
	wg.Wait()

	for i := 0; i < workers; i++ {
		if errs[i] != nil {
			t.Fatalf("第 %d 个并发调用失败: %v", i, errs[i])
		}
		if dirs[i] != dirs[0] {
			t.Fatalf("并发调用路径不一致: %q != %q", dirs[i], dirs[0])
		}
	}

	entries, err := os.ReadDir(slotRoot)
	if err != nil {
		t.Fatalf("读槽根失败: %v", err)
	}
	var dirCount int
	for _, e := range entries {
		if e.IsDir() {
			dirCount++
		}
	}
	if dirCount != 1 {
		t.Fatalf("并发建槽应只创建一个目录，实际 %d 个", dirCount)
	}
}

// TestCodeArtsSlotChromeArgsUseSlotProfile 登录窗口与补采的 Chrome 参数必须带
// --user-data-dir=<该号的槽目录>，且不得回落到 data/codearts-web 或 pending-*。
func TestCodeArtsSlotChromeArgsUseSlotProfile(t *testing.T) {
	codeartsSlotTestEnv(t)
	p := &Panel{}
	acc := codeartsSlotTestAccount("acct-args", "DOMAIN-C", "USER-C")
	codeartsSlotTestSavePool(t, acc)

	// 补采走的就是这个入口（ensureCodeartsSlotDir，无锁版本；外面由 backfill 持槽锁）。
	slotDir, err := p.ensureCodeartsSlotDir(acc)
	if err != nil {
		t.Fatalf("取补采 profile 目录失败: %v", err)
	}

	cases := []struct {
		name     string
		headless bool
	}{
		{name: "登录窗口（可见浏览器）", headless: false},
		{name: "补采（无头浏览器）", headless: true},
	}
	for _, tc := range cases {
		args := codeartsChromeArgs(9333, slotDir, codeartsConsoleURL, tc.headless)
		if !codeartsArgsHave(args, "--user-data-dir="+slotDir) {
			t.Fatalf("%s 的 Chrome 参数必须带 --user-data-dir=%s，实际 %v", tc.name, slotDir, args)
		}
		for _, arg := range args {
			if strings.Contains(arg, "codearts-web") {
				t.Fatalf("%s 不得回落到历史 data/codearts-web 目录，实际参数 %v", tc.name, args)
			}
			if strings.Contains(arg, "pending-") {
				t.Fatalf("%s 不得使用 pending-* 临时目录，实际参数 %v", tc.name, args)
			}
		}
		if tc.headless && !codeartsArgsHave(args, "--headless=new") {
			t.Fatalf("补采必须是无头模式，实际参数 %v", args)
		}
	}

	// 槽目录本身就是该号的 profile（不是槽根、也不是别的号）。
	if filepath.Base(slotDir) != "profile" {
		t.Fatalf("槽 profile 目录名必须是 profile，实际 %s", slotDir)
	}
	if slotDir != acc.SlotProfileDir() {
		t.Fatalf("补采目录必须等于该号的槽 profile: %q != %q", slotDir, acc.SlotProfileDir())
	}
}

// codeartsArgsHave 参数列表里是否有这一项（精确匹配，不做子串误判）。
func codeartsArgsHave(args []string, want string) bool {
	for _, arg := range args {
		if arg == want {
			return true
		}
	}
	return false
}

// TestCodeArtsLoginProfileDirSelection 重登（带 id）走该号自己的槽；"添加账号"
// （不带 id）走槽根下的 pending-* 占位目录。
func TestCodeArtsLoginProfileDirSelection(t *testing.T) {
	slotRoot := codeartsSlotTestEnv(t)
	p := &Panel{}
	acc := codeartsSlotTestAccount("acct-login", "DOMAIN-L", "USER-L")
	codeartsSlotTestSavePool(t, acc)

	// 添加账号：不知道该号 → pending 占位，inSlot=false。
	pendingDir, inSlot, err := p.codeartsLoginProfileDir("")
	if err != nil {
		t.Fatalf("选待登录目录失败: %v", err)
	}
	if inSlot {
		t.Fatal("添加账号时不应标记为长期槽")
	}
	if !strings.HasPrefix(pendingDir, slotRoot) || !strings.Contains(filepath.Base(pendingDir), "pending-") {
		t.Fatalf("pending 目录必须落在槽根下，实际 %s", pendingDir)
	}

	// 重登已知号：必须复用该号的槽（同设备），inSlot=true。
	slotDir, inSlot, err := p.codeartsLoginProfileDir(acc.ID)
	if err != nil {
		t.Fatalf("选重登目录失败: %v", err)
	}
	if !inSlot {
		t.Fatal("重登已知号必须标记为长期槽（关闭窗口时不能删）")
	}
	if slotDir != acc.SlotProfileDir() {
		t.Fatalf("重登必须使用该号自己的槽: %q != %q", slotDir, acc.SlotProfileDir())
	}
	if !codeartsDirExists(slotDir) {
		t.Fatalf("重登路径应已创建槽目录: %s", slotDir)
	}

	// 不存在的账号：明确报错，不要默默建一个无主槽。
	if _, _, err := p.codeartsLoginProfileDir("no-such-account"); err == nil {
		t.Fatal("重登不存在的账号必须返回错误")
	}
}

// TestCodeArtsSlotAdoptPendingProfile 登录成功后的 pending profile 必须被
// 收编进该号槽（设备身份不丢），pending 目录清空。
func TestCodeArtsSlotAdoptPendingProfile(t *testing.T) {
	codeartsSlotTestEnv(t)
	p := &Panel{}
	acc := codeartsSlotTestAccount("acct-adopt", "DOMAIN-P", "USER-P")
	codeartsSlotTestSavePool(t, acc)

	pendingDir := filepath.Join(codearts.SlotRoot(), "pending-test")
	if err := os.MkdirAll(filepath.Join(pendingDir, "Network"), 0o700); err != nil {
		t.Fatalf("建 pending 目录失败: %v", err)
	}
	body := []byte(`{"os_crypt":{"encrypted_key":"device-key"}}`)
	if err := os.WriteFile(filepath.Join(pendingDir, "Local State"), body, 0o600); err != nil {
		t.Fatalf("写 pending 文件失败: %v", err)
	}

	if err := p.codeartsAdoptPendingProfile(acc, pendingDir); err != nil {
		t.Fatalf("收编登录 profile 失败: %v", err)
	}
	if codeartsDirExists(pendingDir) {
		t.Fatalf("收编后 pending 目录必须消失: %s", pendingDir)
	}
	got, err := os.ReadFile(filepath.Join(acc.SlotProfileDir(), "Local State"))
	if err != nil {
		t.Fatalf("槽内应保留登录 profile 的设备身份文件: %v", err)
	}
	if string(got) != string(body) {
		t.Fatalf("收编后的 profile 内容被改动: %s", got)
	}
}

// TestCodeArtsSlotForgetRequiresConfirm 重置身份是破坏性动作（换设备），
// handler 层必须显式确认；本方法不做 UI，但配套校验函数要说清楚。
func TestCodeArtsSlotForgetRequiresConfirm(t *testing.T) {
	if err := codeartsSlotForgetConfirmError(false); err == nil {
		t.Fatal("没有 confirm=true 时必须拒绝重置身份槽")
	}
	if err := codeartsSlotForgetConfirmError(true); err != nil {
		t.Fatalf("confirm=true 时应允许重置: %v", err)
	}
}

// TestCodeArtsSlotAdoptKeepsExistingDeviceIdentity 老号已有设备身份的槽不能被
// 新登录的 pending profile 覆盖（否则每次重登都换一次设备）。
func TestCodeArtsSlotAdoptKeepsExistingDeviceIdentity(t *testing.T) {
	codeartsSlotTestEnv(t)
	p := &Panel{}
	acc := codeartsSlotTestAccount("acct-keep", "DOMAIN-K", "USER-K")
	codeartsSlotTestSavePool(t, acc)

	slotDir, err := p.codeartsSlotDir(acc)
	if err != nil {
		t.Fatalf("建槽失败: %v", err)
	}
	oldIdentity := []byte(`{"os_crypt":{"encrypted_key":"old-device"}}`)
	if werr := os.WriteFile(filepath.Join(slotDir, "Local State"), oldIdentity, 0o600); werr != nil {
		t.Fatalf("写槽内设备身份失败: %v", werr)
	}

	pendingDir := filepath.Join(codearts.SlotRoot(), "pending-second")
	if merr := os.MkdirAll(pendingDir, 0o700); merr != nil {
		t.Fatalf("建 pending 目录失败: %v", merr)
	}
	if werr := os.WriteFile(filepath.Join(pendingDir, "Local State"),
		[]byte(`{"os_crypt":{"encrypted_key":"new-device"}}`), 0o600); werr != nil {
		t.Fatalf("写 pending 设备身份失败: %v", werr)
	}

	if aerr := p.codeartsAdoptPendingProfile(acc, pendingDir); aerr != nil {
		t.Fatalf("收编登录 profile 失败: %v", aerr)
	}
	got, rerr := os.ReadFile(filepath.Join(slotDir, "Local State"))
	if rerr != nil {
		t.Fatalf("原槽设备身份文件不应消失: %v", rerr)
	}
	if string(got) != string(oldIdentity) {
		t.Fatalf("已有设备身份的槽被覆盖了（等于换设备）: %s", got)
	}
	if codeartsDirExists(pendingDir) {
		t.Fatalf("未被采用的 pending 目录必须清理: %s", pendingDir)
	}
}
