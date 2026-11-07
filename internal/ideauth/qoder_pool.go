package ideauth

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/appdir"
)

// QoderPoolFile 是号池文件（存放多个 Qoder 账号快照）。
type QoderPoolFile struct {
	Accounts []*QoderSnapshot `json:"accounts"`
}

// PoolDir 返回号池保存目录（默认 <exe目录>/data/ide_accounts）。
//
// 必须锚定 exe 目录：分发后用户可能从任意工作目录启动，
// 用 cwd 会让号池文件散落在各处，表现为「刚才存的号不见了」。
func PoolDir() string {
	if v := os.Getenv("IDEAUTH_POOL_DIR"); v != "" {
		return v
	}
	return appdir.Resolve(filepath.Join("data", "ide_accounts"))
}

func qoderPoolPath() string {
	return filepath.Join(PoolDir(), "qoder_accounts.json")
}

// qoderPoolMu 串行化号池文件的「读-改-写」。
//
// 号池有多个并发写者（签到回写 token、ClaimCID 更新、增删账号），裸
// Load → 改 → Save 会互相覆盖：两个 goroutine 各自读到同一份快照，后写的
// 那份把先写的账号改动整个盖掉。所有读改写路径必须走 UpdateQoderPool。
var qoderPoolMu sync.Mutex

// LoadQoderPool 读取号池（不存在返回空池）。
func LoadQoderPool() (*QoderPoolFile, error) {
	qoderPoolMu.Lock()
	defer qoderPoolMu.Unlock()
	return loadQoderPool()
}

// loadQoderPool 读取号池（调用方须已持锁）。
func loadQoderPool() (*QoderPoolFile, error) {
	p := qoderPoolPath()
	raw, err := os.ReadFile(p)
	if err != nil {
		if os.IsNotExist(err) {
			return &QoderPoolFile{}, nil
		}
		return nil, err
	}
	var f QoderPoolFile
	if err := json.Unmarshal(raw, &f); err != nil {
		return nil, fmt.Errorf("解析 %s 失败: %w", p, err)
	}
	return &f, nil
}

// SaveQoderPool 原子写回号池（按 Email 排序）。
func SaveQoderPool(f *QoderPoolFile) error {
	qoderPoolMu.Lock()
	defer qoderPoolMu.Unlock()
	return saveQoderPool(f)
}

// saveQoderPool 原子写回号池（调用方须已持锁）。
func saveQoderPool(f *QoderPoolFile) error {
	if f == nil {
		return fmt.Errorf("nil pool")
	}
	sort.SliceStable(f.Accounts, func(i, j int) bool {
		if f.Accounts[i] == nil {
			return false
		}
		if f.Accounts[j] == nil {
			return true
		}
		return f.Accounts[i].Email < f.Accounts[j].Email
	})
	b, err := json.MarshalIndent(f, "", "  ")
	if err != nil {
		return err
	}
	return atomicWrite(qoderPoolPath(), b)
}

// UpdateQoderPool 在包级互斥锁内执行 Load → fn → Save（契约 C-QD-POOL@1）。
//
// fn 返回错误时不写盘；fn 内**禁止**再调用 LoadQoderPool/SaveQoderPool/
// QoderScopePeers 等会再次拿这把锁的函数（互斥锁不可重入，会死锁）。
func UpdateQoderPool(fn func(*QoderPoolFile) error) error {
	if fn == nil {
		return fmt.Errorf("nil update func")
	}
	qoderPoolMu.Lock()
	defer qoderPoolMu.Unlock()
	f, err := loadQoderPool()
	if err != nil {
		return err
	}
	if err := fn(f); err != nil {
		return err
	}
	return saveQoderPool(f)
}

// UpsertQoderAccount 把当前登录态存入号池（按 Email 去重）。
// 返回是否新增账号。
func UpsertQoderAccount() (*QoderSnapshot, bool, error) {
	snap, err := CaptureQoderSnapshot()
	if err != nil {
		return nil, false, err
	}
	if strings.TrimSpace(snap.Email) == "" {
		return nil, false, fmt.Errorf("当前 Qoder 未登录或邮箱为空")
	}

	added := true
	err = UpdateQoderPool(func(f *QoderPoolFile) error {
		for i, a := range f.Accounts {
			if a != nil && strings.EqualFold(a.Email, snap.Email) {
				f.Accounts[i] = snap
				added = false
				return nil
			}
		}
		f.Accounts = append(f.Accounts, snap)
		return nil
	})
	if err != nil {
		return nil, false, err
	}
	return snap, added, nil
}

// RemoveQoderAccount 从号池移除指定邮箱。
func RemoveQoderAccount(email string) error {
	return UpdateQoderPool(func(f *QoderPoolFile) error {
		out := f.Accounts[:0]
		for _, a := range f.Accounts {
			if a == nil || !strings.EqualFold(a.Email, email) {
				out = append(out, a)
			}
		}
		f.Accounts = out
		return nil
	})
}

// SwitchQoderTo 把号池中指定邮箱的登录态写入 Qoder（切号核心）。
// 保存当前登录态到号池后再切换，避免丢失当前账号。
func SwitchQoderTo(email string, restoreMachineID bool) (*QoderSnapshot, error) {
	f, err := LoadQoderPool()
	if err != nil {
		return nil, err
	}
	var target *QoderSnapshot
	for _, a := range f.Accounts {
		if strings.EqualFold(a.Email, email) {
			target = a
			break
		}
	}
	if target == nil {
		return nil, fmt.Errorf("号池中没有账号 %s", email)
	}
	// 先保存当前账号（若已登录且与目标不同）
	if cur, err := CaptureQoderSnapshot(); err == nil && cur.Email != "" &&
		!strings.EqualFold(cur.Email, email) {
		_, _, _ = UpsertQoderAccount()
	}
	if err := RestoreQoderSnapshot(target, restoreMachineID); err != nil {
		return nil, err
	}
	target.SavedAt = time.Now().Format(time.RFC3339)
	return target, nil
}

// QoderAccountView 是给面板用的脱敏视图。
type QoderAccountView struct {
	Email     string `json:"email"`
	UserID    string `json:"user_id"`
	Name      string `json:"name"`
	ExpiresAt string `json:"expires_at"`
	Expired   bool   `json:"expired"`
	Current   bool   `json:"current"`
	SavedAt   string `json:"saved_at"`
	MachineID string `json:"machine_id"`
	DeviceID  string `json:"device_id"`
	// Scope 该账号的 agentAccountScope（服务端在登录时下发的 agent 租户位标识）。
	// ⚠️ 它**不是**每日额度的分组键（实测：两个 scope 不同的账号照样共享一份
	// 每日额度，见 QoderQuotaGroupKey 的注释）。
	Scope string `json:"scope,omitempty"`
	// ScopeLabel scope 的短标签，给人看。
	ScopeLabel    string `json:"scope_label,omitempty"`
	IdentityLabel string `json:"identity_label,omitempty"`
	// QuotaGroup 该账号所在**同一人组**里的账号数（含自己）。
	// >1 = 服务端已判定它与别的号是同一个人，每天只有一份每日额度。
	QuotaGroup int `json:"quota_group,omitempty"`
}

// ListQoderAccounts 返回号池账号视图，标记当前登录账号。
func ListQoderAccounts() ([]QoderAccountView, error) {
	f, err := LoadQoderPool()
	if err != nil {
		return nil, err
	}
	curEmail := ""
	if cur, err := ReadQoderAuth(); err == nil {
		curEmail = cur.User.Email
	}
	now := time.Now().UTC()
	sizes := QoderQuotaGroupSizes(f.Accounts)
	out := make([]QoderAccountView, 0, len(f.Accounts))
	for _, a := range f.Accounts {
		exp := false
		if a.Auth != nil && a.Auth.ExpiresAt != "" {
			if t, err := time.Parse(time.RFC3339, a.Auth.ExpiresAt); err == nil && t.Before(now) {
				exp = true
			}
		}
		scope := QoderQuotaScopeKey(a)
		identityLabel := a.Email
		if a.Auth != nil {
			if a.Auth.User.Email != "" {
				identityLabel = a.Auth.User.Email
			} else if a.UserID != "" {
				identityLabel = "UID " + a.UserID
			}
		}
		out = append(out, QoderAccountView{
			IdentityLabel: identityLabel,
			Email:         a.Email,
			UserID:        a.UserID,
			Name:          a.Name,
			ExpiresAt:     expAt(a),
			Expired:       exp,
			Current:       strings.EqualFold(a.Email, curEmail),
			SavedAt:       a.SavedAt,
			MachineID:     a.MachineID,
			DeviceID:      a.DeviceID,
			Scope:         scope,
			ScopeLabel:    QoderQuotaScopeLabel(scope),
			QuotaGroup:    sizes[strings.ToLower(strings.TrimSpace(a.Email))],
		})
	}
	return out, nil
}

func expAt(a *QoderSnapshot) string {
	if a.Auth == nil {
		return ""
	}
	return a.Auth.ExpiresAt
}

// ---------------------------------------------------------------------------
// 每日额度分组：**按人**，不按 scope / 不按设备 / 不按账号
//
// 2026-11-02 实测（真打上游，见 qoder_person.go 的完整证据）：
//
//	服务端把当天的每日 100 Credits 发给**一个人**，而不是一个账号。
//	两个账号拿到的是同一条活动实例（campaignId 相同），先领的那份记在
//	grantId 上；另一个账号去领/去查，服务端明确回
//	failureCode=SAME_PERSON_ALREADY_CLAIMED（还会给它单独建一条 grant）。
//
//	三个**不是**分组键的维度（都实测过）：
//	  1. 设备：两个号的 machine_id / device_id 都不同，照样共享一份；
//	     伪造整套 Cosy-* 设备头也不改变判定。
//	  2. 账号：两个号 uid 不同，照样共享一份。
//	  3. agentAccountScope：**两个号的 scope 其实不同**（qq=7c3816b4…、
//	     gmail=9ecb3ca9…，用客户端数据里的 serverRuntimeId 与服务端
//	     /api/v1/agents 交叉验证得到），却照样共享同一份额度 —— 所以
//	     拿 scope 分组是错的（上一版就是错的，会把「不是同一人」的号也说成被占）。
//
// 结论：只能问服务端本人（只读接口 GET /sash/api/v1/me/campaigns/{cid}/reward），
// 判定为「同一人」的账号归为一组；判不出来的就归为**未知**（不是「独立」，
// 也不是「被占用」）。
// ---------------------------------------------------------------------------

// QoderQuotaScopeKey 返回账号的 agentAccountScope（服务端在登录时下发的
// agent 租户位标识）。
//
// ⚠️ 它**不是**每日额度的分组键（见上面的实测结论）：两个 scope 不同的账号
// 可能共享同一份每日额度。
func QoderQuotaScopeKey(a *QoderSnapshot) string {
	if a == nil {
		return ""
	}
	return strings.TrimSpace(a.Scope)
}

// QoderQuotaGroupKey 返回该账号的**同一人组**键。
//
// 组键来自「服务端判定过同一人」的关系（见 qoder_person.go）；没有判定记录
// 的账号各自成组（键 = 自己的邮箱），**不猜**。
func QoderQuotaGroupKey(a *QoderSnapshot) string {
	if a == nil {
		return ""
	}
	em := strings.ToLower(strings.TrimSpace(qoderSnapshotEmail(a)))
	if em == "" {
		return "unknown"
	}
	peers := QoderSamePersonPeers(em)
	if len(peers) == 0 {
		return "person:" + em
	}
	group := []string{em}
	group = append(group, peers...)
	sort.Strings(group)
	return "person:" + strings.Join(group, "|")
}

// qoderSnapshotEmail 取快照的账号邮箱。
func qoderSnapshotEmail(a *QoderSnapshot) string {
	if a == nil {
		return ""
	}
	if em := strings.TrimSpace(a.Email); em != "" {
		return em
	}
	if a.Auth != nil {
		return QoderIdentityKey(a.Auth)
	}
	return ""
}

// QoderQuotaGroups 按**同一人组**分组：组键 → 组内账号邮箱列表。
//
// 组内账号都是服务端判定过「同一个人」的（见 qoder_person.go）；没判定过的
// 账号各自成组 —— **不猜**。
func QoderQuotaGroups(accounts []*QoderSnapshot) map[string][]string {
	out := map[string][]string{}
	for _, a := range accounts {
		em := qoderSnapshotEmail(a)
		if em == "" {
			continue
		}
		k := QoderQuotaGroupKey(a)
		out[k] = append(out[k], em)
	}
	return out
}

// QoderQuotaGroupSizes 返回每个账号所在的**同一人组**大小（含自己）。
func QoderQuotaGroupSizes(accounts []*QoderSnapshot) map[string]int {
	groups := QoderQuotaGroups(accounts)
	out := map[string]int{}
	for _, ems := range groups {
		for _, em := range ems {
			out[strings.ToLower(em)] = len(ems)
		}
	}
	return out
}

// QoderQuotaScopeLabel 返回给人看的 scope 短标签（末 8 位哈希）。
func QoderQuotaScopeLabel(scope string) string {
	s := strings.TrimSpace(scope)
	if s == "" {
		return ""
	}
	if i := strings.LastIndex(s, ":"); i >= 0 && i+1 < len(s) {
		s = s[i+1:]
	}
	if len(s) > 8 {
		s = s[:8]
	}
	return s
}

// QoderScopePeers 返回号池里与指定账号**同一个人**的其它账号邮箱。
//
// ⚠️ 语义已改（2026-11-02）：以前按 agentAccountScope 分组，那是**错的** ——
// 实测两个号的 scope 不同却共享同一份每日额度。现在只认**服务端判定过**
// 的同一人关系（见 qoder_person.go）。
//
// 没有判定记录时返回空（无法判定），调用方要按"未知"处理，不要当成"独立"。
func QoderScopePeers(email string) []string {
	ex := strings.ToLower(strings.TrimSpace(email))
	if ex == "" {
		return nil
	}
	peers := QoderSamePersonPeers(ex)
	if len(peers) == 0 {
		return nil
	}
	pool, err := LoadQoderPool()
	if err != nil || pool == nil {
		return nil
	}
	inPool := map[string]bool{}
	for _, a := range pool.Accounts {
		if a == nil {
			continue
		}
		inPool[strings.ToLower(strings.TrimSpace(qoderSnapshotEmail(a)))] = true
	}
	var out []string
	for _, p := range peers {
		if inPool[p] {
			out = append(out, p)
		}
	}
	sort.Strings(out)
	return out
}
