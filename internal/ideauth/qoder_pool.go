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
	Email       string `json:"email"`
	UserID      string `json:"user_id"`
	Name        string `json:"name"`
	ExpiresAt   string `json:"expires_at"`
	Expired     bool   `json:"expired"`
	Current     bool   `json:"current"`
	SavedAt     string `json:"saved_at"`
	MachineID   string `json:"machine_id"`
	DeviceID    string `json:"device_id"`
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
	out := make([]QoderAccountView, 0, len(f.Accounts))
	for _, a := range f.Accounts {
		exp := false
		if a.Auth != nil && a.Auth.ExpiresAt != "" {
			if t, err := time.Parse(time.RFC3339, a.Auth.ExpiresAt); err == nil && t.Before(now) {
				exp = true
			}
		}
		out = append(out, QoderAccountView{
			Email:     a.Email,
			UserID:    a.UserID,
			Name:      a.Name,
			ExpiresAt: expAt(a),
			Expired:   exp,
			Current:   strings.EqualFold(a.Email, curEmail),
			SavedAt:   a.SavedAt,
			MachineID: a.MachineID,
			DeviceID:  a.DeviceID,
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
