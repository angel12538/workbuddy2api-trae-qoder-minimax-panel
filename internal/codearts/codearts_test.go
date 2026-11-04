package codearts

import (
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"testing"
	"time"
)

// 回归：CodeArts 账号身份必须**跨登录稳定**。
//
// 背景：华为云网页登录态只有约一小时寿命，Cookie 快照第二天就失效，所以
// 签到必须复用「登录时那个浏览器的 profile」。而 profile 要挂在账号名下，
// 账号身份（ID）就必须在**每次登录后都算出同一个值**。
//
// 用华为云稳定账号 ID（domain_tag / user_tag）做指纹，而不是会话 Cookie
// （SessionID 每次登录都变 —— 拿它认人，同一个账号会被认成好多个）。

func ck(name, value string) Cookie {
	return Cookie{Name: name, Value: value, Domain: ".huaweicloud.com"}
}

func TestFingerprintStableAcrossLogins(t *testing.T) {
	// 第一次登录：会话票据是 A
	first := []Cookie{
		ck("domain_tag", "c89103cc576b49c299c773db80c8dd3d"),
		ck("user_tag", "1fc7818c8c2c414694ad40d8560205c1"),
		ck("SessionID", "11111111-2222-3333-4444-555555555555"),
		ck("HWWAFSESID", "waf-a"),
	}
	// 第二次登录：同一个账号，会话票据全变了（多出 WAF / 埋点 Cookie）
	second := []Cookie{
		ck("domain_tag", "c89103cc576b49c299c773db80c8dd3d"),
		ck("user_tag", "1fc7818c8c2c414694ad40d8560205c1"),
		ck("SessionID", "99999999-8888-7777-6666-555555555555"),
		ck("HWWAFSESID", "waf-b"),
		ck("_fr_ssid", "fr-1"),
	}
	if Fingerprint(first) != Fingerprint(second) {
		t.Fatalf("同一个华为云账号两次登录必须算出同一个 id：first=%s second=%s",
			Fingerprint(first), Fingerprint(second))
	}
	if DomainIDOf(first) == "" || DomainIDOf(second) != DomainIDOf(first) {
		t.Fatal("DomainIDOf 应取出稳定的华为云账号 ID")
	}
}

func TestFingerprintDistinguishesAccounts(t *testing.T) {
	a := []Cookie{ck("domain_tag", "account-a"), ck("user_tag", "user-a")}
	b := []Cookie{ck("domain_tag", "account-b"), ck("user_tag", "user-b")}
	if Fingerprint(a) == Fingerprint(b) {
		t.Fatal("不同账号必须算出不同的 id")
	}
}

func TestFingerprintFallsBackToSession(t *testing.T) {
	// 没有稳定账号标识时退回会话票据：同一份 Cookie 快照仍要可重复识别。
	c := []Cookie{ck("SessionID", "abc"), ck("hwid", "hw-1")}
	if Fingerprint(c) != Fingerprint(c) {
		t.Fatal("同一份 Cookie 快照的指纹必须稳定")
	}
	if Fingerprint(c) == Fingerprint([]Cookie{ck("SessionID", "xyz"), ck("hwid", "hw-1")}) {
		t.Fatal("不同会话不应算出相同指纹")
	}
}

func TestUpsertKeepsSlotAndID(t *testing.T) {
	p := &Pool{}
	acc := &Account{ID: "abc123", Slot: "abc123", Cookies: []Cookie{ck("domain_tag", "x")}, SavedAt: Now()}
	saved, added := Upsert(p, acc)
	if !added || saved.Slot != "abc123" {
		t.Fatalf("新账号应入库并带 slot：added=%v slot=%q", added, saved.Slot)
	}
	// 同一账号再次登录（指纹相同 → ID 相同）：更新而不是新增，且 slot 保持不变。
	again := &Account{ID: "abc123", Slot: "abc123", Cookies: []Cookie{ck("domain_tag", "x"), ck("SessionID", "new")}, SavedAt: Now()}
	saved2, added2 := Upsert(p, again)
	if added2 || len(p.Accounts) != 1 {
		t.Fatalf("同一账号重复登录不应新增条目：added=%v n=%d", added2, len(p.Accounts))
	}
	if saved2.Slot != "abc123" {
		t.Fatalf("slot 必须保持稳定：%q", saved2.Slot)
	}
}

// 登录保存路径（Upsert）必须按新 Cookie 重算票据来源，且不能擦掉探测结论。
//
// 这是缺陷一的落脚点：保存了 Cookie 却不重算 TicketSource，续期决策就会拿着
// 过期/空来源跑；反过来把 session 结论被一次"没有绝对票据"的登录擦掉，同样会
// 退化成"永远不可续期"。
func TestUpsertRefreshesTicketSource(t *testing.T) {
	p := &Pool{}
	sessionOnly := &Account{ID: "id1", Cookies: []Cookie{
		NewObservedCookie("SSOTGC", "v", ".huaweicloud.com", "/", -1),
	}}
	saved, _ := Upsert(p, sessionOnly)
	if saved.TicketDeadline != "" || saved.TicketSource != "" {
		t.Fatalf("只有会话票据时不得凭空给出截止时间，得到 %q/%q", saved.TicketDeadline, saved.TicketSource)
	}
	// 模拟 T2 探测结论：确认票据全是会话级。
	saved.TicketSource = "session"
	saved.ProbeState = "session_only"

	// 再次登录，这次拿到带绝对时间的票据 → 来源必须翻成 cookie。
	withAbs := &Account{ID: "id1", Cookies: []Cookie{
		NewObservedCookie("SSOTGC", "v2", ".huaweicloud.com", "/", -1),
		NewObservedCookie("SSOJTC", "j", ".huaweicloud.com", "/", 1767225600),
	}}
	saved, added := Upsert(p, withAbs)
	if added || len(p.Accounts) != 1 {
		t.Fatalf("同一账号必须更新而不是新增：added=%v n=%d", added, len(p.Accounts))
	}
	want := time.Unix(1767225600, 0).Format(time.RFC3339)
	if saved.TicketSource != "cookie" || saved.TicketDeadline != want {
		t.Fatalf("有绝对票据时应写 cookie/%q，得到 %q/%q", want, saved.TicketSource, saved.TicketDeadline)
	}

	// 又一次登录只有会话票据，但探测结论是 session → 必须保留，不能被擦成空。
	saved.ProbeState = "session_only"
	saved.TicketSource = "session"
	again := &Account{ID: "id1", Cookies: []Cookie{
		NewObservedCookie("SSOTGC", "v3", ".huaweicloud.com", "/", -1),
	}}
	saved, _ = Upsert(p, again)
	if saved.TicketSource != "session" || saved.ProbeState != "session_only" {
		t.Fatalf("探测结论不得被一次未命中擦掉，得到 source=%q probe=%q", saved.TicketSource, saved.ProbeState)
	}
}

// =============================================================================
// 身份槽（Identity Slot）：字段名、确定性派生、路径安全、旧号池兼容
// =============================================================================

// 两个真实账号的脱敏样本（号池里 177****2169 / 156****8342 两条）。
// 只保留派生需要的键；测试不依赖具体值，只断言「稳定且互不相同」。
func slotSampleAccountA() *Account {
	return &Account{
		ID:       "ca1d671920b4",
		Label:    "CodeArts huvj421wxe",
		Identity: "01a1c0ffeeaa|user-tag-a",
		Cookies: []Cookie{
			ck("domain_tag", "01a1c0ffeeaa"),
			ck("user_tag", "user-tag-a"),
			ck("masked_phone", "177****2169"),
			ck("masked_user", "h****uvj421wxe"),
			ck("SessionID", "session-a"),
		},
	}
}

func slotSampleAccountB() *Account {
	return &Account{
		ID:       "423eab29ffdd",
		Label:    "CodeArts uvj421wxe",
		Identity: "c89103cc576b|user-tag-b",
		Cookies: []Cookie{
			ck("domain_tag", "c89103cc576b"),
			ck("user_tag", "user-tag-b"),
			ck("masked_phone", "156****8342"),
			ck("masked_user", "G****uvj421wxe"),
			ck("SessionID", "session-b"),
		},
	}
}

// 槽字段的 JSON 名是固定契约（面板/后续任务按名消费），写错一个就断链。
// 同时锁死 omitempty：旧账号没补全时不得凭空写出空字段。
func TestAccountSlotJSONFieldNames(t *testing.T) {
	a := slotSampleAccountA()
	a.EnsureSlot()
	raw, err := json.Marshal(a)
	if err != nil {
		t.Fatal(err)
	}
	var filled map[string]any
	if err := json.Unmarshal(raw, &filled); err != nil {
		t.Fatal(err)
	}
	for _, k := range []string{"slot_id", "slot_dir", "device_guid", "person_key", "client_source"} {
		if v, ok := filled[k]; !ok || v == "" {
			t.Errorf("补全后 Account JSON 缺非空字段 %q：%s", k, raw)
		}
	}

	rawOld, err := json.Marshal(&Account{ID: "x"})
	if err != nil {
		t.Fatal(err)
	}
	var old map[string]any
	if err := json.Unmarshal(rawOld, &old); err != nil {
		t.Fatal(err)
	}
	for _, k := range []string{"slot_id", "slot_dir", "device_guid", "person_key", "client_source"} {
		if _, ok := old[k]; ok {
			t.Errorf("omitempty 失效，未补全账号写出了空字段 %q：%s", k, rawOld)
		}
	}
}

// uuidV5Pattern 固定格式：8-4-4-4-12，版本位 5，变体位 8/9/a/b。
var uuidV5Pattern = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-5[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`)

// 派生规则：默认值、幂等、跨登录稳定、两个号互不相同。
func TestEnsureSlotDerivesStableSlotIdentity(t *testing.T) {
	a := slotSampleAccountA()
	a.EnsureSlot()

	if a.SlotID != a.ID {
		t.Fatalf("SlotID 默认必须等于 ID：slot_id=%q id=%q", a.SlotID, a.ID)
	}
	if a.SlotDir != a.SlotID {
		t.Fatalf("SlotDir 默认必须等于 SlotID：slot_dir=%q slot_id=%q", a.SlotDir, a.SlotID)
	}
	if a.ClientSource != ClientSourceImport {
		t.Fatalf("旧数据 ClientSource 默认必须是 import，得到 %q", a.ClientSource)
	}
	if !strings.HasPrefix(a.PersonKey, "person:") || len(a.PersonKey) <= len("person:") {
		t.Fatalf("PersonKey 必须是 person:<hash>，得到 %q", a.PersonKey)
	}
	if !uuidV5Pattern.MatchString(a.DeviceGUID) {
		t.Fatalf("DeviceGUID 必须是 UUIDv5 格式，得到 %q", a.DeviceGUID)
	}

	// 幂等：再补一次必须一个字节都不变（Load 补完、Save 又补一遍是常态）。
	first := *a
	a.EnsureSlot()
	if a.SlotID != first.SlotID || a.SlotDir != first.SlotDir ||
		a.DeviceGUID != first.DeviceGUID || a.PersonKey != first.PersonKey ||
		a.ClientSource != first.ClientSource {
		t.Fatalf("EnsureSlot 必须幂等：first=%+v again=%+v", first, *a)
	}

	// 同一个号重新登录（会话 Cookie 变了）：设备 GUID 必须仍然相同。
	relogin := slotSampleAccountA()
	relogin.Cookies = append(relogin.Cookies, ck("SessionID", "session-a-2"), ck("HWWAFSESID", "waf-new"))
	relogin.EnsureSlot()
	if relogin.DeviceGUID != first.DeviceGUID {
		t.Fatalf("同号跨登录 DeviceGUID 必须稳定：%q vs %q", relogin.DeviceGUID, first.DeviceGUID)
	}
	if relogin.PersonKey != first.PersonKey {
		t.Fatalf("同号跨登录 PersonKey 必须稳定：%q vs %q", relogin.PersonKey, first.PersonKey)
	}

	// 两个不同的人：槽、设备、分桶三个维度都必须不同。
	b := slotSampleAccountB()
	b.EnsureSlot()
	if b.SlotID == a.SlotID || b.SlotDir == a.SlotDir {
		t.Fatalf("不同账号不得共用槽：a=%q/%q b=%q/%q", a.SlotID, a.SlotDir, b.SlotID, b.SlotDir)
	}
	if b.DeviceGUID == a.DeviceGUID {
		t.Fatalf("不同槽不得共用设备 GUID：%q", a.DeviceGUID)
	}
	if b.PersonKey == a.PersonKey {
		t.Fatalf("不同人不得共用 PersonKey：%q", a.PersonKey)
	}
}

// ID 为空时的回退链：IdentityOf → label → Cookie 指纹，全部必须稳定非空。
func TestEnsureSlotFallsBackWithoutID(t *testing.T) {
	// ① 有 Identity：由 IdentityOf 派生，同样输入必须同样输出。
	byIdentity := &Account{
		Cookies: []Cookie{ck("domain_tag", "dom-1"), ck("user_tag", "user-1")},
	}
	byIdentity.EnsureSlot()
	if byIdentity.SlotID == "" || !strings.HasPrefix(byIdentity.SlotID, "ca-") {
		t.Fatalf("有 Identity 时应派生 ca-* 槽 ID，得到 %q", byIdentity.SlotID)
	}
	sameIdentity := &Account{
		Cookies: []Cookie{ck("domain_tag", "dom-1"), ck("user_tag", "user-1")},
	}
	sameIdentity.EnsureSlot()
	if sameIdentity.SlotID != byIdentity.SlotID || sameIdentity.DeviceGUID != byIdentity.DeviceGUID {
		t.Fatalf("同一 Identity 必须派生同一槽/设备：%q/%q vs %q/%q",
			byIdentity.SlotID, byIdentity.DeviceGUID, sameIdentity.SlotID, sameIdentity.DeviceGUID)
	}

	// ② 只有 label：仍然稳定。
	byLabel := &Account{Label: "CodeArts 1772169"}
	byLabel.EnsureSlot()
	if byLabel.SlotID == "" {
		t.Fatal("只有 label 时不得产出空 SlotID")
	}
	sameLabel := &Account{Label: "CodeArts 1772169"}
	sameLabel.EnsureSlot()
	if sameLabel.SlotID != byLabel.SlotID {
		t.Fatalf("同一 label 必须派生同一槽：%q vs %q", byLabel.SlotID, sameLabel.SlotID)
	}

	// ③ 全空：仍不得产出空槽（空槽 ID 会让两个号共用目录）。
	empty := &Account{}
	empty.EnsureSlot()
	if empty.SlotID == "" || empty.SlotDir != empty.SlotID || empty.DeviceGUID == "" {
		t.Fatalf("空账号也必须补出完整槽：%+v", empty)
	}
}

// SlotDir 必须收敛成槽根下的相对目录：绝对路径 / 盘符 / ".." 逃逸一律回退。
func TestEnsureSlotSanitizesSlotDir(t *testing.T) {
	root := filepath.Join(t.TempDir(), "slots")
	t.Setenv("CODEARTS_SLOT_ROOT", root)

	const fallback = "slot-safe"
	cases := []struct{ name, raw, want string }{
		{"空值回退 SlotID", "", fallback},
		{"普通相对名保留", "custom-dir", "custom-dir"},
		{"多层相对名保留", filepath.Join("nested", "child"), filepath.Join("nested", "child")},
		{"点回退", ".", fallback},
		{"父目录回退", "..", fallback},
		{"逃逸回退", ".." + string(filepath.Separator) + "escape", fallback},
		{"深层逃逸回退", filepath.Join("a", "..", "..", "escape"), fallback},
	}
	if runtime.GOOS == "windows" {
		cases = append(cases, struct{ name, raw, want string }{"盘符回退", `C:\Windows\Temp`, fallback})
	} else {
		cases = append(cases, struct{ name, raw, want string }{"绝对路径回退", "/etc/passwd", fallback})
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			a := &Account{ID: fallback, SlotDir: tc.raw}
			dir := a.SlotProfileDir()
			want := filepath.Join(root, tc.want, "profile")
			if dir != want {
				t.Fatalf("SlotProfileDir=%q，want %q（raw=%q）", dir, want, tc.raw)
			}
			rel, err := filepath.Rel(root, dir)
			if err != nil {
				t.Fatal(err)
			}
			if rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
				t.Fatalf("profile 目录逃出了槽根：root=%q dir=%q", root, dir)
			}
		})
	}
}

// 槽根：默认跟随号池文件所在目录，CODEARTS_SLOT_ROOT 可整体覆盖。
func TestSlotRootFollowsPoolFileAndEnv(t *testing.T) {
	poolPath := filepath.Join(t.TempDir(), "custom-pool", "codearts_accounts.json")
	t.Setenv("CODEARTS_POOL_FILE", poolPath)
	t.Setenv("CODEARTS_SLOT_ROOT", "")
	if got, want := SlotRoot(), filepath.Join(filepath.Dir(poolPath), "codearts-slots"); got != want {
		t.Fatalf("默认槽根必须跟随号池目录：got=%q want=%q", got, want)
	}

	override := filepath.Join(t.TempDir(), "slots-override")
	t.Setenv("CODEARTS_SLOT_ROOT", "  "+override+"  ")
	if got := SlotRoot(); got != filepath.Clean(override) {
		t.Fatalf("CODEARTS_SLOT_ROOT 必须覆盖默认槽根：got=%q want=%q", got, filepath.Clean(override))
	}
}

// SlotProfileDir / SlotExists：路径结构正确，且只认「真的存在的目录」。
func TestSlotProfileDirAndExists(t *testing.T) {
	root := filepath.Join(t.TempDir(), "slots")
	t.Setenv("CODEARTS_SLOT_ROOT", root)

	a := slotSampleAccountA()
	a.EnsureSlot()
	want := filepath.Join(root, a.SlotDir, "profile")
	if got := a.SlotProfileDir(); got != want {
		t.Fatalf("SlotProfileDir=%q want %q", got, want)
	}
	if a.SlotExists() {
		t.Fatal("profile 目录不存在时 SlotExists 必须为 false")
	}
	if err := os.MkdirAll(want, 0o700); err != nil {
		t.Fatal(err)
	}
	if !a.SlotExists() {
		t.Fatal("profile 目录存在时 SlotExists 必须为 true")
	}

	var nilAccount *Account
	if nilAccount.SlotProfileDir() != "" || nilAccount.SlotExists() {
		t.Fatal("nil 账号必须安全返回空/false")
	}
}

// 旧号池兼容：文件里没有槽字段 → LoadPool 在内存补全且**不写盘**；
// 下一次 SavePool 自动带上槽字段，同时 ID/Aliases/旧 Slot 原样保留。
func TestLoadPoolFillsSlotsWithoutRewritingFile(t *testing.T) {
	poolPath := filepath.Join(t.TempDir(), "codearts_accounts.json")
	t.Setenv("CODEARTS_POOL_FILE", poolPath)
	t.Setenv("CODEARTS_SLOT_ROOT", filepath.Join(t.TempDir(), "slots"))

	legacy := `{
  "accounts": [
    {
      "id": "ca1d671920b4",
      "label": "CodeArts huvj421wxe",
      "identity": "01a1c0ffeeaa|user-tag-a",
      "aliases": ["423eab29ffdd"],
      "slot": "ca1d671920b4",
      "cookies": [
        {"name": "masked_phone", "value": "177****2169", "domain": ".huaweicloud.com"},
        {"name": "domain_tag", "value": "01a1c0ffeeaa", "domain": ".huaweicloud.com"},
        {"name": "user_tag", "value": "user-tag-a", "domain": ".huaweicloud.com"}
      ],
      "saved_at": "2026-11-02T18:15:08+08:00"
    }
  ]
}
`
	if err := os.WriteFile(poolPath, []byte(legacy), 0o600); err != nil {
		t.Fatal(err)
	}

	p, err := LoadPool()
	if err != nil {
		t.Fatalf("无槽字段的旧号池必须可读：%v", err)
	}
	if len(p.Accounts) != 1 {
		t.Fatalf("账号数异常：%d", len(p.Accounts))
	}
	a := p.Accounts[0]
	if a.SlotID != "ca1d671920b4" || a.SlotDir != "ca1d671920b4" {
		t.Fatalf("LoadPool 应在内存补全槽：slot_id=%q slot_dir=%q", a.SlotID, a.SlotDir)
	}
	if a.DeviceGUID == "" || !uuidV5Pattern.MatchString(a.DeviceGUID) {
		t.Fatalf("LoadPool 应补出设备 GUID，得到 %q", a.DeviceGUID)
	}
	if !strings.HasPrefix(a.PersonKey, "person:") || a.ClientSource != ClientSourceImport {
		t.Fatalf("LoadPool 应补出 PersonKey/ClientSource：%q/%q", a.PersonKey, a.ClientSource)
	}
	// 既有字段一个都不许动。
	if a.ID != "ca1d671920b4" || a.Slot != "ca1d671920b4" ||
		len(a.Aliases) != 1 || a.Aliases[0] != "423eab29ffdd" ||
		a.Identity != "01a1c0ffeeaa|user-tag-a" || a.Label != "CodeArts huvj421wxe" {
		t.Fatalf("补全槽字段不得改动既有字段：%+v", a)
	}

	afterLoad, err := os.ReadFile(poolPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(afterLoad) != legacy {
		t.Fatal("LoadPool 只做内存补全，不得写盘")
	}

	// 下一次保存：槽字段落盘，旧字段仍在。
	if err := SavePool(p); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(poolPath)
	if err != nil {
		t.Fatal(err)
	}
	var doc struct {
		Accounts []map[string]any `json:"accounts"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("保存后必须仍是完整 JSON：%v", err)
	}
	if len(doc.Accounts) != 1 {
		t.Fatalf("账号数不应变化：%d", len(doc.Accounts))
	}
	saved := doc.Accounts[0]
	for _, k := range []string{"slot_id", "slot_dir", "device_guid", "person_key", "client_source"} {
		if v, ok := saved[k]; !ok || v == "" {
			t.Errorf("保存后应带上槽字段 %q：%v", k, saved)
		}
	}
	if saved["id"] != "ca1d671920b4" || saved["slot"] != "ca1d671920b4" {
		t.Fatalf("旧 ID/Slot 不得丢：%v", saved)
	}
	if aliases, _ := saved["aliases"].([]any); len(aliases) != 1 || aliases[0] != "423eab29ffdd" {
		t.Fatalf("Aliases 不得丢：%v", saved["aliases"])
	}
}
