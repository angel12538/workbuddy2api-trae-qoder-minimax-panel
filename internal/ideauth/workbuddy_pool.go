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

// WorkBuddyPoolFile 是 WorkBuddy 号池文件。
type WorkBuddyPoolFile struct {
	Accounts []*WorkBuddySnapshot `json:"accounts"`
}

func workBuddyPoolPath() string {
	return filepath.Join(PoolDir(), "workbuddy_accounts.json")
}

// LoadWorkBuddyPool 读取号池（不存在返回空池）。
func LoadWorkBuddyPool() (*WorkBuddyPoolFile, error) {
	p := workBuddyPoolPath()
	raw, err := os.ReadFile(p)
	if err != nil {
		if os.IsNotExist(err) {
			return &WorkBuddyPoolFile{}, nil
		}
		return nil, err
	}
	var f WorkBuddyPoolFile
	if err := json.Unmarshal(raw, &f); err != nil {
		return nil, fmt.Errorf("解析 %s 失败: %w", p, err)
	}
	return &f, nil
}

// SaveWorkBuddyPool 写号池（原子写）。
func SaveWorkBuddyPool(f *WorkBuddyPoolFile) error {
	p := workBuddyPoolPath()
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		return err
	}
	raw, err := json.MarshalIndent(f, "", "  ")
	if err != nil {
		return err
	}
	return atomicWrite(p, raw)
}

func wbAccountKey(s *WorkBuddySnapshot) string {
	if s == nil || s.Account == nil {
		return ""
	}
	return s.Account.UID + "|" + s.Account.EnterpriseID + "|" + s.Edition
}

// UpsertWorkBuddyAccount 登录并加入/更新号池，返回快照与是否为新增。
func UpsertWorkBuddyAccount(edition WorkBuddyEdition, openBrowser bool) (*WorkBuddySnapshot, bool, error) {
	snap, err := WorkBuddyLogin(edition, openBrowser)
	if err != nil {
		return nil, false, err
	}
	return SaveWorkBuddySnapshot(snap)
}

// SaveWorkBuddySnapshot 把已有快照写入号池。
func SaveWorkBuddySnapshot(snap *WorkBuddySnapshot) (*WorkBuddySnapshot, bool, error) {
	captureWorkBuddyAuthFile(snap) // 补齐真实登录会话文件原文（用于切号）
	f, err := LoadWorkBuddyPool()
	if err != nil {
		return nil, false, err
	}
	key := wbAccountKey(snap)
	added := true
	for i, a := range f.Accounts {
		if wbAccountKey(a) == key {
			f.Accounts[i] = snap
			added = false
			break
		}
	}
	if added {
		f.Accounts = append(f.Accounts, snap)
	}
	if err := SaveWorkBuddyPool(f); err != nil {
		return nil, false, err
	}
	return snap, added, nil
}

// RemoveWorkBuddyAccount 按 uid 移出号池。
func RemoveWorkBuddyAccount(uid string) error {
	f, err := LoadWorkBuddyPool()
	if err != nil {
		return err
	}
	out := f.Accounts[:0]
	for _, a := range f.Accounts {
		if a != nil && a.Account != nil && a.Account.UID == uid {
			continue
		}
		out = append(out, a)
	}
	f.Accounts = out
	return SaveWorkBuddyPool(f)
}

// FindWorkBuddyAccount 按 uid 查找号池账号。
func FindWorkBuddyAccount(uid string) (*WorkBuddySnapshot, error) {
	f, err := LoadWorkBuddyPool()
	if err != nil {
		return nil, err
	}
	for _, a := range f.Accounts {
		if a != nil && a.Account != nil && a.Account.UID == uid {
			return a, nil
		}
	}
	return nil, fmt.Errorf("未找到账号 %s", uid)
}

// ListWorkBuddyAccounts 列出号池账号（标注是否过期/当前）。
func ListWorkBuddyAccounts() ([]WorkBuddyAccountView, error) {
	f, err := LoadWorkBuddyPool()
	if err != nil {
		return nil, err
	}
	// 为尚未保存「真实登录会话文件」的账号补抓一次，供后续真切号使用。
	// 只有当前仍处于登录态的账号能抓到（auth 目录只保留当前会话）。
	patched := false
	for _, a := range f.Accounts {
		if a != nil && captureWorkBuddyAuthFile(a) {
			patched = true
		}
	}
	if patched {
		_ = SaveWorkBuddyPool(f)
	}
	curByEd := WorkBuddyCurrentUIDsByEdition()
	curSet := map[string]bool{}
	for _, uid := range curByEd {
		if uid != "" {
			curSet[uid] = true
		}
	}
	today := CheckinDates("workbuddy")
	now := time.Now().UnixMilli()
	out := make([]WorkBuddyAccountView, 0, len(f.Accounts))
	for _, a := range f.Accounts {
		if a == nil {
			continue
		}
		v := WorkBuddyAccountView{
			Edition:  a.Edition,
			Endpoint: a.Endpoint,
			SavedAt:  a.SavedAt,
		}
		// 是否已记录真实登录会话文件：决定该账号能否一键切号。
		v.SessionReady = a.AuthFileRaw != ""
		if a.Account != nil {
			v.UID = a.Account.UID
			v.Nickname = a.Account.Nickname
			v.Type = a.Account.Type
			v.EnterpriseID = a.Account.EnterpriseID
			v.EnterpriseName = a.Account.EnterpriseName
			// 跨版本判断：国内版 / 国际版各自维护登录态，两个版本可同时为「当前登录」。
			v.Current = curSet[a.Account.UID]
			v.CheckedInToday = today[a.Account.UID] == time.Now().Format("2006-01-02")
		}
		if a.Auth != nil {
			v.HasRefresh = a.Auth.RefreshToken != ""
			if a.Auth.ExpiresAt > 0 && a.Auth.ExpiresAt < now {
				v.Expired = true
			}
			// refreshToken 过期也算过期
			if a.Auth.RefreshExpiresAt > 0 && a.Auth.RefreshExpiresAt < now {
				v.Expired = true
			}
		} else {
			v.Expired = true
		}
		out = append(out, v)
	}
	// 排序：先国内版、再国际版，同版本内「当前登录」优先、然后按保存时间倒序。
	// 这样平铺展示（签到池 / 积分总览）自然形成「国内版 … 国内版 … 国际版 …」的顺序。
	edRank := func(ed string) int {
		switch ed {
		case string(WorkBuddyCN):
			return 0
		case string(WorkBuddyIntl):
			return 1
		case string(WorkBuddyIntlBuddy):
			return 2
		}
		return 3
	}
	sort.SliceStable(out, func(i, j int) bool {
		ri, rj := edRank(out[i].Edition), edRank(out[j].Edition)
		if ri != rj {
			return ri < rj
		}
		if out[i].Current != out[j].Current {
			return out[i].Current
		}
		return out[i].SavedAt > out[j].SavedAt
	})
	return out, nil
}

// SwitchWorkBuddyTo 切换到号池中指定账号：刷新凭据、把该账号的真实登录会话
// 文件整份写回客户端读取位置，并同步本地账号标记。
func SwitchWorkBuddyTo(uid string) (*WorkBuddySnapshot, error) {
	target, err := FindWorkBuddyAccount(uid)
	if err != nil {
		return nil, err
	}
	if target.Auth == nil || target.Auth.RefreshToken == "" {
		return nil, fmt.Errorf("账号 %s 缺少 refreshToken，无法切换", uid)
	}
	// 用 refreshToken 换新 token，顺带验证账号仍然有效
	if auth, err := WorkBuddyRefreshToken(target); err == nil {
		target.Auth = auth
	}
	// 拉一次账号信息确认身份
	if acc, err := WorkBuddyFetchAccount(WorkBuddyEdition(target.Edition), target.Auth); err == nil && acc.UID != "" {
		target.Account = acc
	}
	target.SavedAt = time.Now().Format(time.RFC3339)
	captureWorkBuddyAuthFile(target) // 若快照尚未保存会话文件原文，先尝试补抓
	if _, _, err := SaveWorkBuddySnapshot(target); err != nil {
		return nil, err
	}
	// 真实切号：把目标账号的登录会话文件整份写回（daemon 下次读取即生效）。
	authPath, err := RestoreWorkBuddyAuthFile(target)
	if err != nil {
		return nil, fmt.Errorf("已更新号池，但写入 WorkBuddy 登录会话失败: %w", err)
	}
	// 同步可见的本地账号标记
	if err := writeWorkBuddyLocalAccountState(target); err != nil {
		return nil, fmt.Errorf("已写入登录会话(%s)，但更新 WorkBuddy 本地账号标记失败: %w", authPath, err)
	}
	return target, nil
}

// writeWorkBuddyLocalAccountState 把账号快照写入 WorkBuddy 本地账号态目录。
// 说明：WorkBuddy 的凭据由 daemon 用 keyblob 保护，外部无法直接伪造或替换 session；
// 这里只写入可见的“账号标记”，不能据此断言客户端下次启动会自动换到该会话。
func writeWorkBuddyLocalAccountState(s *WorkBuddySnapshot) error {
	dir := WorkBuddyConfigDir()
	if dir == "" {
		return fmt.Errorf("无法定位 WorkBuddy 配置目录")
	}
	if _, err := os.Stat(dir); err != nil {
		return fmt.Errorf("WorkBuddy 配置目录不存在: %s", dir)
	}
	// 备份并写入 account-snapshot.json
	p := WorkBuddyAccountSnapshotPath()
	if _, err := os.Stat(p); err == nil {
		_ = backupFile(p)
	}
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		return err
	}
	payload := map[string]any{
		"primary": map[string]any{
			"version":    1,
			"uid":        s.Account.UID,
			"nickname":   s.Account.Nickname,
			"type":       defaultStr(s.Account.Type, "personal"),
			"editionType": "free",
			"savedAt":    time.Now().UnixMilli(),
		},
	}
	raw, err := json.MarshalIndent(payload, "", "  ")
	if err != nil {
		return err
	}
	return atomicWrite(p, raw)
}

func defaultStr(s, d string) string {
	if s == "" {
		return d
	}
	return s
}

// workBuddyAuthsDir 返回主池 auths 目录（默认 <exe目录>/auths，可用 IDEAUTH_AUTH_DIR 覆盖）。
//
// 以 exe 目录为基准而非 os.Getwd()：分发给别人后用户可能双击启动，
// 工作目录不在程序目录，用 cwd 会把账号写到别处（表现为「号池空了」）。
func workBuddyAuthsDir() string {
	if v := strings.TrimSpace(os.Getenv("IDEAUTH_AUTH_DIR")); v != "" {
		return v
	}
	return appdir.Resolve("auths")
}

// RefreshWorkBuddyAuthFilesFromClient 逐账号重新捕获客户端当前会话文件原文。
//
// 客户端在线时会随使用刷新 token，号池里备份的原文会逐渐陈旧——
// 用陈旧原文切号可能失效。定时巡检跑一遍即可让号池与客户端保持一致。
// 返回更新的账号数；无变化时不动池文件。
func RefreshWorkBuddyAuthFilesFromClient() (updated int, err error) {
	f, err := LoadWorkBuddyPool()
	if err != nil {
		return 0, err
	}
	changed := false
	for _, a := range f.Accounts {
		if a != nil && captureWorkBuddyAuthFile(a) {
			updated++
			changed = true
		}
	}
	if changed {
		if err := SaveWorkBuddyPool(f); err != nil {
			return updated, err
		}
	}
	return updated, nil
}

// SyncWorkBuddyFromPool 把主池 auths/ 里已有的 WorkBuddy 账号（workbuddy-*.json）
// 同步进 IDE 号池，避免“客户端已登录却还要重新走浏览器 OAuth”。
// 返回：新增数、扫描到的 WorkBuddy 账号总数、错误。
func SyncWorkBuddyFromPool() (added int, scanned int, err error) {
	dir := workBuddyAuthsDir()
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return 0, 0, fmt.Errorf("未找到主池账号目录 %s", dir)
		}
		return 0, 0, err
	}
	pool, err := LoadWorkBuddyPool()
	if err != nil {
		return 0, 0, err
	}
	seen := make(map[string]bool, len(pool.Accounts))
	for _, a := range pool.Accounts {
		seen[wbAccountKey(a)] = true
	}
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		name := e.Name()
		if !strings.HasPrefix(name, "workbuddy-") || !strings.HasSuffix(name, ".json") {
			continue
		}
		raw, rerr := os.ReadFile(filepath.Join(dir, name))
		if rerr != nil {
			continue
		}
		var src struct {
			Account *struct {
				UID            string `json:"uid"`
				Nickname       string `json:"nickname"`
				Type           string `json:"type"`
				EnterpriseID   string `json:"enterpriseId"`
				EnterpriseName string `json:"enterpriseName"`
			} `json:"account"`
			Auth *struct {
				AccessToken  string `json:"accessToken"`
				RefreshToken string `json:"refreshToken"`
				Domain       string `json:"domain"`
				Realm        string `json:"realm"`
				ExpiresAt    int64  `json:"expiresAt"`
			} `json:"auth"`
		}
		if json.Unmarshal(raw, &src) != nil || src.Account == nil || src.Account.UID == "" {
			continue
		}
		scanned++

		realm, domain := "", ""
		var accessToken, refreshToken string
		var srcExpires int64
		if src.Auth != nil {
			realm = strings.ToLower(strings.TrimSpace(src.Auth.Realm))
			domain = strings.ToLower(src.Auth.Domain)
			accessToken = src.Auth.AccessToken
			refreshToken = src.Auth.RefreshToken
			srcExpires = src.Auth.ExpiresAt
		}
		// realm 优先；缺失时用域名兜底判断国内/国际。
		ed := WorkBuddyCN
		switch {
		case realm == "global" || realm == "intl":
			ed = WorkBuddyIntl
		case realm == "" && (strings.Contains(domain, "copilot.tencent.com") || strings.Contains(domain, "tencentbuddy")):
			ed = WorkBuddyIntl
		}

		snap := &WorkBuddySnapshot{
			Edition:  string(ed),
			Endpoint: WorkBuddyEndpointFor(ed),
			Account: &WorkBuddyAccount{
				UID:            src.Account.UID,
				Nickname:       src.Account.Nickname,
				Type:           defaultStr(src.Account.Type, "personal"),
				EnterpriseID:   src.Account.EnterpriseID,
				EnterpriseName: src.Account.EnterpriseName,
			},
			Auth: &WorkBuddyAuth{
				AccessToken:  accessToken,
				RefreshToken: refreshToken,
				Domain:       src.Auth.Domain,
			},
			SavedAt: time.Now().Format(time.RFC3339),
		}
		if srcExpires > 0 {
			// 主池存的是秒，IDE 号池按毫秒判断过期。
			if srcExpires < 1e12 {
				srcExpires *= 1000
			}
			snap.Auth.ExpiresAt = srcExpires
		}
		key := wbAccountKey(snap)
		if seen[key] {
			continue
		}
		pool.Accounts = append(pool.Accounts, snap)
		seen[key] = true
		added++
	}
	if added > 0 {
		if err := SaveWorkBuddyPool(pool); err != nil {
			return 0, scanned, err
		}
	}
	return added, scanned, nil
}
