package panel

import (
	"path/filepath"
	"testing"

	"github.com/linguo2625469/workbuddy2api-panel/internal/codearts"
)

// caCk 造一条华为云 Cookie。
func caCk(name, value string) codearts.Cookie {
	return codearts.Cookie{Name: name, Value: value, Domain: ".huaweicloud.com"}
}

// TestCodeartsSaveLoginIDCollisionDoesNotMerge 新账号的指纹**恰好**等于号池里
// 另一条记录的历史 ID 时，也必须是两个账号，不能合并。
//
// 真实故障现场（2026-10-30 10:49，用户实测）：号池里老账号的 ID 是历史遗留
// （旧算法算的 ca1d671920b4），而新账号的 domain_tag/user_tag 算出的指纹
// 恰好也是 ca1d671920b4 → 仅凭 ID 就把两个不同的人合并了，老账号的会话被
// 覆盖，面板上只剩一个号。
func TestCodeartsSaveLoginIDCollisionDoesNotMerge(t *testing.T) {
	t.Setenv("CODEARTS_POOL_FILE", filepath.Join(t.TempDir(), "pool.json"))
	p := &Panel{}

	newCookies := []codearts.Cookie{
		caCk("domain_tag", "NEW-DOMAIN"), caCk("user_tag", "NEW-USER"),
		caCk("SessionID", "sess-new"), caCk("devclouddevuibjtcftk", "cftk-new"),
	}
	fp := codearts.Fingerprint(newCookies)
	oldCookies := []codearts.Cookie{
		caCk("domain_tag", "OLD-DOMAIN"), caCk("user_tag", "OLD-USER"),
		caCk("SessionID", "sess-old"), caCk("devclouddevuibjtcftk", "cftk-old"),
	}
	// 老账号的 ID 故意等于新账号的指纹
	pool := &codearts.Pool{Accounts: []*codearts.Account{{
		ID: fp, Identity: codearts.IdentityOf(oldCookies), Slot: "old-slot",
		Cookies: oldCookies, SavedAt: codearts.Now(), Label: "老账号",
	}}}
	if err := codearts.SavePool(pool); err != nil {
		t.Fatalf("写号池失败: %v", err)
	}

	b, added, err := p.codeartsSaveLogin(newCookies, "新账号", "", 9333, "")
	if err != nil {
		t.Fatalf("保存新账号失败: %v", err)
	}
	if !added {
		t.Fatal("ID 撞车也不该合并 —— 身份明确不同就是两个人")
	}
	pool2, err := codearts.LoadPool()
	if err != nil {
		t.Fatalf("读号池失败: %v", err)
	}
	if len(pool2.Accounts) != 2 {
		t.Fatalf("老账号被顶掉了！号池只剩 %d 条", len(pool2.Accounts))
	}
	ids := map[string]bool{}
	for _, x := range pool2.Accounts {
		if ids[x.ID] {
			t.Fatalf("两个账号的 ID 相同：%q", x.ID)
		}
		ids[x.ID] = true
		if x.Label == "老账号" {
			for _, c := range x.Cookies {
				if c.Name == "SessionID" && c.Value != "sess-old" {
					t.Fatalf("老账号的会话被改成 %q", c.Value)
				}
			}
		}
	}
	if b.ID == fp {
		t.Fatalf("新账号的 ID 不能继续用撞车的那个值：%q", b.ID)
	}
}

// TestCodeartsSaveLoginPortReuseDoesNotEvict 端口被复用时，**不能**把
// 上一个账号顶掉。
//
// 用户实测故障（2026-10-30）：已有账号 A 的情况下再添加账号 B，B 把 A 顶掉了。
// 真因是 freePort 永远给第一个空闲端口 —— 上一个登录窗口一关，端口立刻被
// 回收复用，B 拿到和 A 一样的端口；而号池的 Upsert 里有一条"同一个调试端口
// = 同一个人"的规则，于是 B 被当成 A，A 的会话被 B 覆盖。
func TestCodeartsSaveLoginPortReuseDoesNotEvict(t *testing.T) {
	t.Setenv("CODEARTS_POOL_FILE", filepath.Join(t.TempDir(), "pool.json"))
	p := &Panel{}

	cookiesA := []codearts.Cookie{
		caCk("domain_tag", "dom-a"), caCk("user_tag", "usr-a"),
		caCk("SessionID", "sess-a"), caCk("devclouddevuibjtcftk", "cftk-a"),
	}
	cookiesB := []codearts.Cookie{
		caCk("domain_tag", "dom-b"), caCk("user_tag", "usr-b"),
		caCk("SessionID", "sess-b"), caCk("devclouddevuibjtcftk", "cftk-b"),
	}

	// A 用 9333（窗口关掉后端口被回收）
	a1, added, err := p.codeartsSaveLogin(cookiesA, "账号A", "", 9333, "")
	if err != nil {
		t.Fatalf("保存 A 失败: %v", err)
	}
	if !added {
		t.Fatal("A 应为新增")
	}
	// B 拿到**同一个端口**（freePort 的实际行为）
	b1, added2, err := p.codeartsSaveLogin(cookiesB, "账号B", "", 9333, "")
	if err != nil {
		t.Fatalf("保存 B 失败: %v", err)
	}
	if !added2 {
		t.Fatal("B 应为新增 —— 端口相同不代表是同一个人")
	}

	pool, err := codearts.LoadPool()
	if err != nil {
		t.Fatalf("读号池失败: %v", err)
	}
	if len(pool.Accounts) != 2 {
		t.Fatalf("A 和 B 必须都在，号池里实际只有 %d 条：%+v", len(pool.Accounts), pool.Accounts)
	}
	if a1.ID == b1.ID {
		t.Fatalf("A、B 的 ID 不能相同：%q", a1.ID)
	}
	// A 的会话不能被 B 覆盖。
	for _, a := range pool.Accounts {
		if a.ID == a1.ID {
			for _, c := range a.Cookies {
				if c.Name == "SessionID" && c.Value != "sess-a" {
					t.Fatalf("账号 A 的会话被覆盖成了 %q", c.Value)
				}
			}
		}
	}
	// 端口是独占的：保存 B 之后，A 不能再声称自己占着 9333，
	// 否则签到时会连到 B 的窗口、拿 B 的会话替 A 签到（假签到）。
	for _, a := range pool.Accounts {
		if a.ID == a1.ID && a.Port == 9333 {
			t.Fatalf("账号 A 残留的端口记录 %d 必须清掉（否则会连到别人的窗口）", a.Port)
		}
	}
}

// TestCodeartsSaveLoginTwoAccounts 加两个**不同**的华为云账号，必须各占一条、
// 各有各的 ID 和 slot。
//
// 用户诉求原话：「无法添加多个账号」。实测根因是认人用了会变的会话票据，
// 同一个人的两次登录被拆成两条记录，还共用同一个 slot 互相覆盖。
func TestCodeartsSaveLoginTwoAccounts(t *testing.T) {
	t.Setenv("CODEARTS_POOL_FILE", filepath.Join(t.TempDir(), "pool.json"))
	p := &Panel{}

	cookiesA := []codearts.Cookie{
		caCk("domain_tag", "dom-a"), caCk("user_tag", "usr-a"),
		caCk("SessionID", "sess-a"), caCk("devclouddevuibjtcftk", "cftk-a"),
	}
	cookiesB := []codearts.Cookie{
		caCk("domain_tag", "dom-b"), caCk("user_tag", "usr-b"),
		caCk("SessionID", "sess-b"), caCk("devclouddevuibjtcftk", "cftk-b"),
	}

	a1, addedA, err := p.codeartsSaveLogin(cookiesA, "账号A", "", 9333, "")
	if err != nil {
		t.Fatalf("保存账号 A 失败: %v", err)
	}
	if !addedA {
		t.Fatal("账号 A 应该是新增")
	}
	b1, addedB, err := p.codeartsSaveLogin(cookiesB, "账号B", "", 9334, "")
	if err != nil {
		t.Fatalf("保存账号 B 失败: %v", err)
	}
	if !addedB {
		t.Fatal("账号 B 应该是新增")
	}

	pool, err := codearts.LoadPool()
	if err != nil {
		t.Fatalf("读号池失败: %v", err)
	}
	if len(pool.Accounts) != 2 {
		t.Fatalf("两个不同账号应各占一条，实际 %d 条", len(pool.Accounts))
	}
	if a1.ID == b1.ID {
		t.Fatalf("两个账号的 ID 不能相同：%q", a1.ID)
	}
	if a1.Slot == b1.Slot {
		t.Fatalf("两个账号的 slot 不能相同（会互相覆盖）：%q", a1.Slot)
	}
	if a1.Identity == "" || b1.Identity == "" {
		t.Fatalf("身份必须记下来：%q / %q", a1.Identity, b1.Identity)
	}
	if a1.Identity == b1.Identity {
		t.Fatal("不同账号的身份不能相同")
	}
}

// TestCodeartsSaveLoginSamePersonTwice 同一个人登录两次只能有一条记录。
//
// 实测故障现场：同一个人的两次登录在号池里变成两条（domain_tag /
// masked_user 全同），面板上看着像"加了两个号"，其实是一个，还互相覆盖。
func TestCodeartsSaveLoginSamePersonTwice(t *testing.T) {
	t.Setenv("CODEARTS_POOL_FILE", filepath.Join(t.TempDir(), "pool.json"))
	p := &Panel{}

	first := []codearts.Cookie{
		caCk("domain_tag", "dom-x"), caCk("user_tag", "usr-x"),
		caCk("SessionID", "sess-1"), caCk("devclouddevuibjtcftk", "cftk-1"),
	}
	second := []codearts.Cookie{
		caCk("domain_tag", "dom-x"), caCk("user_tag", "usr-x"),
		caCk("SessionID", "sess-2"), caCk("devclouddevuibjtcftk", "cftk-2"),
	}

	a1, _, err := p.codeartsSaveLogin(first, "我", "", 9333, "")
	if err != nil {
		t.Fatalf("首次保存失败: %v", err)
	}
	a2, added, err := p.codeartsSaveLogin(second, "我", "", 9334, "")
	if err != nil {
		t.Fatalf("二次保存失败: %v", err)
	}
	if added {
		t.Fatal("同一个人的二次登录不应新增记录")
	}
	pool, err := codearts.LoadPool()
	if err != nil {
		t.Fatalf("读号池失败: %v", err)
	}
	if len(pool.Accounts) != 1 {
		t.Fatalf("同一个人只能有一条记录，实际 %d 条", len(pool.Accounts))
	}
	if a1.ID != a2.ID {
		t.Fatalf("ID 必须保持稳定：%q → %q", a1.ID, a2.ID)
	}
	if a1.Slot != a2.Slot {
		t.Fatalf("slot 必须保持稳定：%q → %q", a1.Slot, a2.Slot)
	}
	// 会话要换成新的那套。
	if len(a2.Cookies) != 4 || a2.Cookies[2].Value != "sess-2" {
		t.Fatalf("Cookie 必须更新成新会话：%+v", a2.Cookies)
	}
}

// TestCodeartsSaveLoginFallsBackToPageKey 拿不到 domain_tag/user_tag 时，
// 用页面上的稳定账号标识（hid__xxx）认人。
func TestCodeartsSaveLoginFallsBackToPageKey(t *testing.T) {
	t.Setenv("CODEARTS_POOL_FILE", filepath.Join(t.TempDir(), "pool.json"))
	p := &Panel{}

	// 没有 domain_tag / user_tag，只有一个会话 Cookie。
	cookies := []codearts.Cookie{caCk("SessionID", "sess-only")}
	page := "当前用户：hid__lj4oauvj421wxe"

	a1, added, err := p.codeartsSaveLogin(cookies, "", "", 9333, page)
	if err != nil {
		t.Fatalf("保存失败: %v", err)
	}
	if !added {
		t.Fatal("应是新增")
	}
	if a1.Identity == "" || a1.Identity[:4] != "hid:" {
		t.Fatalf("应退回用页面标识认人，实际 identity=%q", a1.Identity)
	}

	// 同一个人再来一次：还是同一条。
	a2, added2, err := p.codeartsSaveLogin(
		[]codearts.Cookie{caCk("SessionID", "sess-other")}, "", "", 9334, page)
	if err != nil {
		t.Fatalf("二次保存失败: %v", err)
	}
	if added2 {
		t.Fatal("同一人的二次登录不应新增")
	}
	if a1.ID != a2.ID {
		t.Fatalf("ID 应保持稳定：%q → %q", a1.ID, a2.ID)
	}
}
