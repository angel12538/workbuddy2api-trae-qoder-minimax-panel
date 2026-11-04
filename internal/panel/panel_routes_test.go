package panel

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/linguo2625469/workbuddy2api-panel/internal/pool"
)

const repairRoutesTestAPIKey = "repair-routes-test-api-key"

// newRepairRoutesTestPanel 走真实的 New() 装配（routes + registerRepairRoutes），
// 让 panel.go 的接线本身也进入测试覆盖。
//
// 所有持久化目录指向 t.TempDir()：本测试只验证路由可达与鉴权，绝不读写
// D:/WorkBuddy/data，也不会触发上游请求（空号池下各 handler 直接返回）。
func newRepairRoutesTestPanel(t *testing.T) *Panel {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("CODEARTS_POOL_FILE", filepath.Join(dir, "codearts_accounts.json"))
	t.Setenv("IDEAUTH_POOL_DIR", t.TempDir())
	t.Setenv("QODER_DATA_DIR", t.TempDir())
	t.Setenv("QODER_EGRESS_FILE", filepath.Join(dir, "qoder_egress.json"))
	t.Setenv("QODER_PROBE_FILE", "")
	return New(Config{
		APIKey:     repairRoutesTestAPIKey,
		Pool:       pool.New(""),
		ConfigPath: filepath.Join(dir, "config.json"),
	})
}

// repairRoute 是契约新增端点的一条（方法 + 路径 + 请求体）。
type repairRoute struct {
	group  string
	method string
	path   string
	body   string
}

// repairContractRoutes 恰为 registerRepairRoutes 注册的「方法+路径」。
//
// slot_forget 刻意留空 body：它要求 confirm=true 才动号池，空 body 会在
// 「缺 id / 缺确认」这一步返回 400 —— 既能证明路由已可达，又保证本测试
// 绝不会真的改写号池里的身份槽。
func repairContractRoutes() []repairRoute {
	return []repairRoute{
		{"codearts keepalive", http.MethodPost, "/panel/api/ide/codearts/keepalive", ""},
		{"codearts probe", http.MethodPost, "/panel/api/ide/codearts/probe", ""},
		{"codearts renew", http.MethodPost, "/panel/api/ide/codearts/renew", ""},
		{"codearts slots", http.MethodGet, "/panel/api/ide/codearts/slots", ""},
		{"codearts slot_forget", http.MethodPost, "/panel/api/ide/codearts/slot_forget", ``},
		{"qoder egress get", http.MethodGet, "/panel/api/ide/qoder/egress", ""},
		{"qoder egress set", http.MethodPost, "/panel/api/ide/qoder/egress", "{}"},
		{"qoder claim_history", http.MethodGet, "/panel/api/ide/qoder/claim_history", ""},
		{"tasks diag", http.MethodGet, "/panel/api/tasks/diag", ""},
	}
}

// TestRepairRoutesRegisteredWithAuth 逐路径断言：
//   - 无 Authorization → 401（withAuth 生效，同时证明路由已注册）；
//   - 带正确 Bearer → 不是 404/405（handler 可达；200/400 由业务决定）。
//
// 同一 mux 重复注册会 panic；本测试经 New() 只注册一次。若 registerRepairRoutes
// 与 routes() 中既有模式冲突，New() 会直接 panic 让测试失败——这正是要防的
// Go 1.22 ServeMux 冲突回归点。
func TestRepairRoutesRegisteredWithAuth(t *testing.T) {
	p := newRepairRoutesTestPanel(t)

	for _, rt := range repairContractRoutes() {
		t.Run(rt.group, func(t *testing.T) {
			rec := httptest.NewRecorder()
			p.ServeHTTP(rec, httptest.NewRequest(rt.method, rt.path, strings.NewReader(rt.body)))
			if rec.Code != http.StatusUnauthorized {
				t.Fatalf("%s %s 无鉴权应为 401，实际 %d（body=%s）",
					rt.method, rt.path, rec.Code, rec.Body.String())
			}
			t.Logf("无鉴权 → %d", rec.Code)

			rec = httptest.NewRecorder()
			req := httptest.NewRequest(rt.method, rt.path, strings.NewReader(rt.body))
			req.Header.Set("Authorization", "Bearer "+repairRoutesTestAPIKey)
			p.ServeHTTP(rec, req)
			if rec.Code == http.StatusNotFound || rec.Code == http.StatusMethodNotAllowed {
				t.Fatalf("%s %s 带正确鉴权仍不可达：%d（body=%s）",
					rt.method, rt.path, rec.Code, rec.Body.String())
			}
			t.Logf("%s %s 带鉴权 → %d（可达）", rt.method, rt.path, rec.Code)
		})
	}
}

// TestRepairRoutesUnknownPathsStill404 断言没有顺手放开任何未授权路径。
func TestRepairRoutesUnknownPathsStill404(t *testing.T) {
	p := newRepairRoutesTestPanel(t)
	paths := []string{
		"/panel/api/repair-not-registered",
		"/panel/api/ide/codearts/not-a-route",
		"/panel/api/ide/qoder/not-a-route",
		"/panel/api/tasks/diag_extra",
	}
	for _, path := range paths {
		t.Run(path, func(t *testing.T) {
			rec := httptest.NewRecorder()
			req := httptest.NewRequest(http.MethodGet, path, nil)
			req.Header.Set("Authorization", "Bearer "+repairRoutesTestAPIKey)
			p.ServeHTTP(rec, req)
			if rec.Code != http.StatusNotFound {
				t.Fatalf("未注册路径 %s 应为 404，实际 %d", path, rec.Code)
			}
		})
	}
}

// TestCodeArtsSlotsHTTPSemantics 身份槽两个端点的真实语义：
//   - slot_forget 不带 confirm → 400 且 code=confirm_required（不动号池）；
//   - 账号不存在 → 404；
//   - 带 confirm → 200，返回**真实**的新 slot_id 与旧槽备份路径；
//   - GET slots 能看到刚换过的新槽。
//
// 全程用 t.TempDir() 下的号池与槽根，不碰 data/ 真实号池（PREFERENCE_3）。
func TestCodeArtsSlotsHTTPSemantics(t *testing.T) {
	p := newRepairRoutesTestPanel(t) // 已把 CODEARTS_POOL_FILE 指向临时目录
	t.Setenv("CODEARTS_SLOT_ROOT", t.TempDir())

	acc := codeartsSlotTestAccount("acct-http", "DOMAIN-H", "USER-H")
	codeartsSlotTestSavePool(t, acc)
	// 先真实建一次槽，这样 SlotForget 才有旧槽可备份。
	oldProfile, err := p.codeartsSlotDir(acc)
	if err != nil {
		t.Fatalf("建槽失败: %v", err)
	}
	oldSlotID := acc.SlotID
	oldSlotDir := filepath.Dir(oldProfile)

	post := func(body string) *httptest.ResponseRecorder {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodPost, "/panel/api/ide/codearts/slot_forget", strings.NewReader(body))
		req.Header.Set("Authorization", "Bearer "+repairRoutesTestAPIKey)
		p.ServeHTTP(rec, req)
		return rec
	}

	// ① 缺 confirm：必须拒绝，且给出稳定 code。
	rec := post(`{"id":"acct-http"}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("缺 confirm 应为 400，实际 %d（body=%s）", rec.Code, rec.Body.String())
	}
	var refused struct {
		OK    bool   `json:"ok"`
		Code  string `json:"code"`
		Error string `json:"error"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &refused); err != nil {
		t.Fatalf("解析拒绝响应失败: %v\n%s", err, rec.Body.String())
	}
	if refused.Code != "confirm_required" || refused.OK || refused.Error == "" {
		t.Fatalf("缺 confirm 的响应必须是 ok=false + code=confirm_required + error，实际 %s", rec.Body.String())
	}
	t.Logf("缺 confirm → %d %s", rec.Code, refused.Code)

	// ② 不存在的账号：404（不是 400，也不是 200）。
	rec = post(`{"id":"no-such-account","confirm":true}`)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("不存在账号应为 404，实际 %d（body=%s）", rec.Code, rec.Body.String())
	}
	t.Logf("不存在账号 → %d", rec.Code)

	// ③ 正常重置：换新槽，旧槽备份到 <oldDir>.bak-*。
	rec = post(`{"id":"acct-http","confirm":true}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("带 confirm 重置应成功，实际 %d（body=%s）", rec.Code, rec.Body.String())
	}
	var done struct {
		OK        bool   `json:"ok"`
		ID        string `json:"id"`
		SlotID    string `json:"slot_id"`
		BackupDir string `json:"backup_dir"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &done); err != nil {
		t.Fatalf("解析重置响应失败: %v\n%s", err, rec.Body.String())
	}
	if !done.OK || done.ID != "acct-http" {
		t.Fatalf("重置响应应 ok=true 且 id=acct-http，实际 %s", rec.Body.String())
	}
	if done.SlotID == "" || done.SlotID == oldSlotID {
		t.Fatalf("重置后 slot_id 必须换成新值，旧=%q 新=%q", oldSlotID, done.SlotID)
	}
	if done.BackupDir == "" || !filepath.IsAbs(done.BackupDir) ||
		!strings.Contains(filepath.Base(done.BackupDir), ".bak-") {
		t.Fatalf("backup_dir 必须是真实的绝对备份路径，实际 %q", done.BackupDir)
	}
	if !codeartsDirExists(done.BackupDir) {
		t.Fatalf("backup_dir 指向的目录不存在: %s", done.BackupDir)
	}
	if codeartsDirExists(oldSlotDir) {
		t.Fatalf("旧槽目录应已改名（不应再存在）: %s", oldSlotDir)
	}
	t.Logf("重置成功：slot %s → %s，备份 %s", oldSlotID, done.SlotID, done.BackupDir)

	// ④ 列表端点到新槽：字段齐全，slot_dir 是绝对路径。
	rec = httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/panel/api/ide/codearts/slots", nil)
	req.Header.Set("Authorization", "Bearer "+repairRoutesTestAPIKey)
	p.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET slots 应 200，实际 %d（body=%s）", rec.Code, rec.Body.String())
	}
	var listed struct {
		OK    bool       `json:"ok"`
		Slots []slotInfo `json:"slots"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &listed); err != nil {
		t.Fatalf("解析 slots 响应失败: %v\n%s", err, rec.Body.String())
	}
	if !listed.OK || len(listed.Slots) != 1 {
		t.Fatalf("slots 应 ok=true 且恰 1 行，实际 %s", rec.Body.String())
	}
	got := listed.Slots[0]
	if got.ID != "acct-http" || got.SlotID != done.SlotID {
		t.Fatalf("slots 里应是重置后的新槽：%+v（want slot_id=%s）", got, done.SlotID)
	}
	if !filepath.IsAbs(got.SlotDir) || got.ProfileBytes < 0 {
		t.Fatalf("slot_dir 必须是绝对路径：%+v", got)
	}
	// 注意 exists 必须允许 false：codeartsSlotForget 只换 SlotID/SlotDir，
	// 新 profile 目录要等下次真正用这个槽启动浏览器时才生成 —— 这是"新设备
	// 尚未上线"的真实状态，不能为了好看硬建一个空目录。
	if filepath.Base(got.SlotDir) != done.SlotID {
		t.Errorf("slot_dir 应按新 slot_id 派生：%+v", got)
	}
	for name, v := range map[string]string{
		"label": got.Label, "device_guid": got.DeviceGUID,
		"person_key": got.PersonKey, "client_source": got.ClientSource,
	} {
		if v == "" {
			t.Errorf("slots 响应缺字段 %s：%+v", name, got)
		}
	}
	t.Logf("GET slots → %d 行，slot_dir=%s", len(listed.Slots), got.SlotDir)
}
