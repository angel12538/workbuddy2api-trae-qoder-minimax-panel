package ideauth

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
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

// LoadQoderPool 读取号池（不存在返回空池）。
func LoadQoderPool() (*QoderPoolFile, error) {
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

// SaveQoderPool 原子写回号池。
func SaveQoderPool(f *QoderPoolFile) error {
	if f == nil {
		return fmt.Errorf("nil pool")
	}
	sort.SliceStable(f.Accounts, func(i, j int) bool {
		return f.Accounts[i].Email < f.Accounts[j].Email
	})
	b, err := json.MarshalIndent(f, "", "  ")
	if err != nil {
		return err
	}
	return atomicWrite(qoderPoolPath(), b)
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
	f, err := LoadQoderPool()
	if err != nil {
		return nil, false, err
	}
	added := true
	for i, a := range f.Accounts {
		if strings.EqualFold(a.Email, snap.Email) {
			f.Accounts[i] = snap
			added = false
			break
		}
	}
	if added {
		f.Accounts = append(f.Accounts, snap)
	}
	if err := SaveQoderPool(f); err != nil {
		return nil, false, err
	}
	return snap, added, nil
}

// RemoveQoderAccount 从号池移除指定邮箱。
func RemoveQoderAccount(email string) error {
	f, err := LoadQoderPool()
	if err != nil {
		return err
	}
	out := f.Accounts[:0]
	for _, a := range f.Accounts {
		if !strings.EqualFold(a.Email, email) {
			out = append(out, a)
		}
	}
	f.Accounts = out
	return SaveQoderPool(f)
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
	// Scope 该账号归属的额度组标识（服务端下发的 agentAccountScope）。
	// 同一 scope 的多个账号共享**同一份**每日签到额度。
	Scope string `json:"scope,omitempty"`
	// ScopeLabel scope 的短标签，给人看。
	ScopeLabel string `json:"scope_label,omitempty"`
	// QuotaGroup 该账号所在额度组里的账号数（含自己）。>1 = 与别的号共享每日额度。
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
		out = append(out, QoderAccountView{
			Email:      a.Email,
			UserID:     a.UserID,
			Name:       a.Name,
			ExpiresAt:  expAt(a),
			Expired:    exp,
			Current:    strings.EqualFold(a.Email, curEmail),
			SavedAt:    a.SavedAt,
			MachineID:  a.MachineID,
			DeviceID:   a.DeviceID,
			Scope:      scope,
			ScopeLabel: QoderQuotaScopeLabel(scope),
			QuotaGroup: sizes[strings.ToLower(strings.TrimSpace(a.Email))],
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
// 额度组（scope）
//
// 2026-11-02 实测（真打上游）：Qoder 的每日 100 Credits **不是按设备发的**，而是按
// **账号归属的 scope**（服务端认定的实名/租户）发的。同一个 scope 下的多个账号共享
// **同一份**每日额度：服务端只给其中一个下发当天的 CLAIM_BENEFIT，其余账号的活动
// 列表里连这条都没有，跨号去领会被回 SAME_PERSON_ALREADY_CLAIMED。
//
// 三条独立证据：
//  1. 伪造整套 Cosy-* 设备头（machineToken / machineCode / machineId 全换）服务端
//     照样返回同一条活动 → 设备头只是「有没有设备」的开关，不参与额度判定；
//     唯一例外是**一个 Cosy 头都不带**时活动列表整体为空。
//  2. POST /api/v1/deviceToken/refresh 的响应里没有任何 scope/设备字段 → 账号与
//     额度组的绑定发生在登录/注册阶段，不在请求头上，改设备头没用。
//  3. 两个账号各自登录后拿到的 agentAccountScope 完全一致（qoder-data.v1.json 的
//     agentAccountScope），而 user id 不同 → 关联键是 scope，不是账号、也不是设备。
//
// 结论：想要两个号**各领一份**每日额度，唯一办法是第二个号归属到**另一个 scope**
// （即用不同身份注册的账号）；同一 scope 下多号只能轮流领同一份。
// ---------------------------------------------------------------------------
// QoderQuotaScopeKey 返回账号归属的 scope（服务端下发的 agentAccountScope）。
func QoderQuotaScopeKey(a *QoderSnapshot) string {
	if a == nil {
		return ""
	}
	return strings.TrimSpace(a.Scope)
}

// QoderQuotaGroupKey 返回该账号的额度组键。
//
// scope 相同 = 同一组（共享一份每日额度）。
//
// scope 未知（老号池快照没存）时**全部未知账号归为一组**：是不是「同一个人」
// 不由我们猜，而是**让服务端回答** —— 只有它对探测请求回
// SAME_PERSON_ALREADY_CLAIMED，才认定同组共用一份额度。拆成各自成组会让
// 老号池（以及上游还没下发 scope 的情况）永远问不出这个判定。
func QoderQuotaGroupKey(a *QoderSnapshot) string {
	if a == nil {
		return ""
	}
	return qoderQuotaGroupKeyOf(QoderQuotaScopeKey(a))
}

// qoderQuotaGroupKeyOf 由 scope 算组键（scan 里只有 scope 这个字段可用）。
func qoderQuotaGroupKeyOf(scope string) string {
	if s := strings.TrimSpace(scope); s != "" {
		return "scope:" + s
	}
	return "unknown"
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
		return strings.TrimSpace(a.Auth.User.Email)
	}
	return ""
}

// QoderQuotaGroups 按额度组分组：组键 → 组内账号邮箱列表。
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

// QoderQuotaGroupSizes 返回每个账号所在的额度组大小（含自己）。
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

// QoderScopePeers 返回号池里与指定账号**同 scope**的其它账号邮箱。
//
// 用途：判定「这个号是不是独立的额度组」——比问上游更硬。上游答复还受「今天
// 谁先领」影响（还没人领时谁都看不到差别），而 scope 是服务端在登录时按账号
// 归属（实名/租户）下发的，**同 scope 就一定共享一份每日额度**。
//
// scope 为空时返回空（无法判定），调用方要按"未知"处理，不要当成"独立"。
func QoderScopePeers(email string) []string {
	pool, err := LoadQoderPool()
	if err != nil || pool == nil {
		return nil
	}
	ex := strings.ToLower(strings.TrimSpace(email))
	target := ""
	for _, a := range pool.Accounts {
		if a == nil {
			continue
		}
		if strings.ToLower(strings.TrimSpace(qoderSnapshotEmail(a))) == ex {
			target = QoderQuotaGroupKey(a)
			break
		}
	}
	if target == "" {
		return nil
	}
	var out []string
	for _, a := range pool.Accounts {
		if a == nil {
			continue
		}
		em := strings.TrimSpace(qoderSnapshotEmail(a))
		if em == "" || strings.EqualFold(em, email) {
			continue
		}
		if QoderQuotaGroupKey(a) == target {
			out = append(out, em)
		}
	}
	sort.Strings(out)
	return out
}
