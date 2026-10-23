package ideauth

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

// Trae IDE（Trae CN / TRAE SOLO CN）的登录态文件布局：
//   %APPDATA%\<产品名>\User\globalStorage\storage.json   主凭据（含 token / 账号）
//   %APPDATA%\<产品名>\machineid                          设备机器码（明文）
//
// 产品名按安装版本不同：Trae CN 用 "Trae CN"，Trae Solo 用 "TRAE SOLO CN"。
// 这里按优先级探测，命中即用；也可用环境变量 TRAE_DATA_DIR 显式指定。

var traeProductCandidates = []string{"Trae CN", "TRAE SOLO CN", "Trae"}

// TraeDataDir 返回 Trae IDE 的用户数据目录（不存在时返回第一个候选路径）。
func TraeDataDir() string {
	if v := os.Getenv("TRAE_DATA_DIR"); v != "" {
		return v
	}
	appdata := os.Getenv("APPDATA")
	if appdata == "" {
		home, _ := os.UserHomeDir()
		appdata = filepath.Join(home, "AppData", "Roaming")
	}
	for _, name := range traeProductCandidates {
		p := filepath.Join(appdata, name)
		if st, err := os.Stat(p); err == nil && st.IsDir() {
			return p
		}
	}
	return filepath.Join(appdata, traeProductCandidates[0])
}

// TraeStoragePath 主凭据文件路径。
func TraeStoragePath() string {
	return filepath.Join(TraeDataDir(), "User", "globalStorage", "storage.json")
}

// TraeMachineIDPath 机器码文件路径。
func TraeMachineIDPath() string {
	return filepath.Join(TraeDataDir(), "machineid")
}

// TraeAvailable 本机是否有可切换的 Trae 登录态。
func TraeAvailable() bool {
	_, err := os.Stat(TraeStoragePath())
	return err == nil
}

// TraeSnapshot Trae IDE 登录态快照。
type TraeSnapshot struct {
	UserID    string `json:"user_id"`
	Email     string `json:"email"`
	Name      string `json:"name"`
	Storage   string `json:"storage"`    // storage.json 原文
	MachineID string `json:"machine_id"` // machineid 文件内容
	SavedAt   string `json:"saved_at"`
}

// CaptureTraeSnapshot 抓取当前 Trae IDE 登录态。
func CaptureTraeSnapshot() (*TraeSnapshot, error) {
	sp := TraeStoragePath()
	raw, err := os.ReadFile(sp)
	if err != nil {
		return nil, fmt.Errorf("读取 Trae storage.json 失败（可能未登录）: %w", err)
	}
	mid, _ := os.ReadFile(TraeMachineIDPath())
	snap := &TraeSnapshot{
		Storage:   string(raw),
		MachineID: strings.TrimSpace(string(mid)),
		SavedAt:   time.Now().Format(time.RFC3339),
	}
	// 尽力从 storage.json 抽取账号标识（不同版本 key 名不一）。
	//
	// 关键：Trae 2.3+（iCube 内核）把账号身份放进了加密的 `iCubeAuthInfo://*`
	// blob（`tc` 头，客户端自己才能解），明文里已经<b>没有</b> userId / email 字段。
	// 好在有两处稳定明文可用：
	//   1) icube_gtm → {"users":{"<uid>":{...}}} 的 uid 键（数字，如 2081802063446363）
	//   2) iCubeAuthInfo://icube-dc:<id> 的键后缀（租户/设备 id）
	// 因此这里改为多源提取，并在只有 iCubeAuthInfo 键时也判定为「已登录」。
	snap.Email = firstNonEmpty(
		extractJSONString(raw, "email"),
		extractJSONString(raw, "userEmail"),
	)
	// 权威来源：解密 `iCubeAuthInfo://icube.cloudide` 里的会话 token，取 JWT 的 uid。
	// 这是唯一准确的「当前登录账号」来源。icube_gtm 只是会滞后的埋点缓存：换号登录后
	// 它仍保留旧账号键，正是此前「已登录新账号却仍显示旧账号 / 只显示仅签到凭证」的根因。
	snap.UserID = TraeCurrentUIDFromStorageRaw(raw)
	if snap.UserID == "" {
		// 解密不可用（旧版格式 / 异常）时回退到明文启发式。
		snap.UserID = firstNonEmpty(
			extractJSONString(raw, "userId"),
			extractJSONString(raw, "user_id"),
			extractJSONString(raw, "uid"),
			extractIcubicGtmUserID(raw),
			extractIcubicAuthKeySuffix(raw, "iCubeAuthInfo://icube-dc:"),
		)
	}
	snap.Name = firstNonEmpty(
		extractJSONString(raw, "userName"),
		extractJSONString(raw, "nickname"),
		extractJSONString(raw, "displayName"),
		extractJSONString(raw, "nickName"),
	)
	if snap.Email == "" && snap.UserID == "" {
		// 仍存在 iCubeAuthInfo:// 登录键 → 视为已登录，用整份 storage 的短哈希兜底做标识，
		// 保证「已登录但字段加密」的机器也能入池（这正是此前添加账号失败的根因）。
		if !hasIcubicAuth(raw) {
			return nil, fmt.Errorf("Trae storage.json 中未找到账号标识（可能未登录）")
		}
		sum := sha256.Sum256(raw)
		snap.UserID = "trae-" + hex.EncodeToString(sum[:])[:12]
	}
	return snap, nil
}

// icubicGtmUIDRe 匹配 icube_gtm → users 对象里的数字用户 id 键。
var icubicGtmUIDRe = regexp.MustCompile(`"?(\d{8,})"?\s*:`)

// extractIcubicGtmUserID 从 icube_gtm 的 {"users":{"<uid>":{...}}} 取第一个数字 userId。
func extractIcubicGtmUserID(raw []byte) string {
	s := string(raw)
	i := strings.Index(s, "icube_gtm")
	if i < 0 {
		return ""
	}
	seg := s[i:]
	j := strings.Index(seg, "users")
	if j < 0 {
		return ""
	}
	seg = seg[j:]
	if m := icubicGtmUIDRe.FindStringSubmatch(seg); m != nil {
		return m[1]
	}
	return ""
}

// extractIcubicAuthKeySuffix 从形如 "iCubeAuthInfo://icube-dc:<id>" 的键取后缀 id。
func extractIcubicAuthKeySuffix(raw []byte, prefix string) string {
	s := string(raw)
	i := strings.Index(s, prefix)
	if i < 0 {
		return ""
	}
	rest := s[i+len(prefix):]
	if end := strings.IndexAny(rest, `"')`); end >= 0 {
		rest = rest[:end]
	}
	return strings.TrimSpace(rest)
}

// hasIcubicAuth 是否存在任意 iCubeAuthInfo:// 登录键（宽松登录判定）。
func hasIcubicAuth(raw []byte) bool {
	return strings.Contains(string(raw), "iCubeAuthInfo://")
}

// RestoreTraeSnapshot 把快照写回 Trae IDE（切号）。
func RestoreTraeSnapshot(s *TraeSnapshot, restoreMachine bool) error {
	if s == nil || s.Storage == "" {
		return fmt.Errorf("快照为空")
	}
	sp := TraeStoragePath()
	if err := backupFile(sp); err != nil {
		return err
	}
	if err := atomicWrite(sp, []byte(s.Storage)); err != nil {
		return err
	}
	if restoreMachine && s.MachineID != "" {
		mp := TraeMachineIDPath()
		_ = backupFile(mp)
		_ = atomicWrite(mp, []byte(s.MachineID))
	}
	return nil
}

// ---------- Trae 号池 ----------

func traePoolPath() string { return filepath.Join(PoolDir(), "trae_ide_accounts.json") }

// TraePoolFile Trae IDE 号池。
type TraePoolFile struct {
	Accounts []*TraeSnapshot `json:"accounts"`
}

// LoadTraePool 读取 Trae 号池。
func LoadTraePool() (*TraePoolFile, error) {
	raw, err := os.ReadFile(traePoolPath())
	if err != nil {
		if os.IsNotExist(err) {
			return &TraePoolFile{}, nil
		}
		return nil, err
	}
	var f TraePoolFile
	if err := json.Unmarshal(raw, &f); err != nil {
		return nil, err
	}
	return &f, nil
}

// SaveTraePool 写回 Trae 号池。
func SaveTraePool(f *TraePoolFile) error {
	b, err := json.MarshalIndent(f, "", "  ")
	if err != nil {
		return err
	}
	return atomicWrite(traePoolPath(), b)
}

// traeSnapKey 账号唯一键（优先 email，回落 user_id）。
func traeSnapKey(s *TraeSnapshot) string {
	if s.Email != "" {
		return strings.ToLower(s.Email)
	}
	return s.UserID
}

// UpsertTraeAccount 把当前 Trae 登录态存入号池。
func UpsertTraeAccount() (*TraeSnapshot, bool, error) {
	snap, err := CaptureTraeSnapshot()
	if err != nil {
		return nil, false, err
	}
	f, err := LoadTraePool()
	if err != nil {
		return nil, false, err
	}
	key := traeSnapKey(snap)
	added := true
	for i, a := range f.Accounts {
		if traeSnapKey(a) == key {
			f.Accounts[i] = snap
			added = false
			break
		}
	}
	if added {
		f.Accounts = append(f.Accounts, snap)
	}
	if err := SaveTraePool(f); err != nil {
		return nil, false, err
	}
	return snap, added, nil
}

// RemoveTraeAccount 从号池移除（按 email 或 user_id 匹配）。
func RemoveTraeAccount(id string) error {
	f, err := LoadTraePool()
	if err != nil {
		return err
	}
	want := strings.ToLower(id)
	out := f.Accounts[:0]
	for _, a := range f.Accounts {
		if strings.ToLower(a.Email) != want && a.UserID != id {
			out = append(out, a)
		}
	}
	f.Accounts = out
	return SaveTraePool(f)
}

// SwitchTraeTo 切到号池指定账号（先保存当前登录态）。
func SwitchTraeTo(id string, restoreMachine bool) (*TraeSnapshot, error) {
	f, err := LoadTraePool()
	if err != nil {
		return nil, err
	}
	want := strings.ToLower(id)
	var target *TraeSnapshot
	for _, a := range f.Accounts {
		if strings.ToLower(a.Email) == want || a.UserID == id {
			target = a
			break
		}
	}
	if target == nil {
		return nil, fmt.Errorf("号池中没有账号 %s", id)
	}
	if cur, err := CaptureTraeSnapshot(); err == nil && traeSnapKey(cur) != traeSnapKey(target) {
		_, _, _ = UpsertTraeAccount()
	}
	if err := RestoreTraeSnapshot(target, restoreMachine); err != nil {
		return nil, err
	}
	// 写回校验：重新读取并解密，确认磁盘上的当前登录账号确实变成了目标账号。
	// 若此处不通过，说明写入被覆盖（通常是客户端没退干净），切号并不算成功。
	if raw, err := os.ReadFile(TraeStoragePath()); err == nil {
		got := TraeCurrentUIDFromStorageRaw(raw)
		if got != "" && target.UserID != "" && got != target.UserID {
			return nil, fmt.Errorf("切号未生效：写入后当前账号仍为 %s（期望 %s），请确认 Trae 已完全退出后重试", got, target.UserID)
		}
	}
	target.SavedAt = time.Now().Format(time.RFC3339)
	return target, nil
}

// TraeAccountView 面板展示视图。
type TraeAccountView struct {
	Email     string `json:"email"`
	UserID    string `json:"user_id"`
	Name      string `json:"name"`
	Current   bool   `json:"current"`
	SavedAt   string `json:"saved_at"`
	MachineID string `json:"machine_id"`
	// CheckinOnly 该账号只在 Trae 签到侧存在（无本地 IDE 登录态，不能切号）。
	CheckinOnly bool `json:"checkin_only,omitempty"`
}

// ListTraeAccounts 返回 Trae 号池视图。
func ListTraeAccounts() ([]TraeAccountView, error) {
	f, err := LoadTraePool()
	if err != nil {
		return nil, err
	}
	curKey := ""
	if cur, err := CaptureTraeSnapshot(); err == nil {
		curKey = traeSnapKey(cur)
	}
	out := make([]TraeAccountView, 0, len(f.Accounts))
	for _, a := range f.Accounts {
		out = append(out, TraeAccountView{
			Email:     a.Email,
			UserID:    a.UserID,
			Name:      a.Name,
			Current:   traeSnapKey(a) == curKey,
			SavedAt:   a.SavedAt,
			MachineID: a.MachineID,
		})
	}
	return out, nil
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}
