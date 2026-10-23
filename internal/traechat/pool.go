package traechat

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

// Account Trae 账号（网关转发用）。
type Account struct {
	Name     string `json:"name"`
	UserID   string `json:"UserID"`
	JWT      string `json:"jwt"`
	DeviceID string `json:"device_id,omitempty"`
}

// accountsFileName 与 Trae Work 助手 / 面板签到共用同一份账号文件。
const accountsFileName = "checkin_accounts.json"

type acctState struct {
	Account
	inFlight      int
	cooldownUntil time.Time
	lastErr       string
	failures      int
}

// Pool Trae 账号池：负责选号、并发门控（上游 solo_agent_parallel_limit）与失败退避。
//
// 账号来源与面板签到、令牌续期**同一份文件**（checkin_accounts.json），所以面板
// 每 4 小时自动续期 JWT 后，这里下一轮读文件即可拿到新凭证，无需额外通道。
type Pool struct {
	client *Client
	// DataDir Trae 数据目录（含 data/checkin_accounts.json）；空 = %APPDATA%\TraeWorkAssistant。
	DataDir string
	// MaxPerAccount 单账号并发上限（上游实测 parallel_limit=2；缺省 2）。
	MaxPerAccount int
	// CooldownOnLimit 触发并行超限后的退避时长（缺省 20s）。
	CooldownOnLimit time.Duration
	// CooldownOnAuth 凭证失效退避（缺省 10min）。
	CooldownOnAuth time.Duration
	// OnUnauthorized 账号凭证失效（上游 401）时的回调（异步触发，传 userID）。
	// 面板用它触发「从 Trae 客户端提取新令牌」自救；可为 nil。
	OnUnauthorized func(userID string)

	mu       sync.Mutex
	state    []*acctState
	mtime    time.Time
	loadedAt time.Time
	rr       int
}

// NewPool 构造账号池。
func NewPool(c *Client, dataDir string) *Pool {
	if c == nil {
		c = NewClient(nil)
	}
	return &Pool{
		client:          c,
		DataDir:         dataDir,
		MaxPerAccount:   2,
		CooldownOnLimit: 20 * time.Second,
		CooldownOnAuth:  10 * time.Minute,
	}
}

// Client 暴露底层协议客户端。
func (p *Pool) Client() *Client { return p.client }

func (p *Pool) accountsPath() (string, error) {
	dir := strings.TrimSpace(p.DataDir)
	if dir == "" {
		appData := os.Getenv("APPDATA")
		if appData == "" {
			return "", os.ErrNotExist
		}
		dir = filepath.Join(appData, "TraeWorkAssistant")
	}
	return filepath.Join(dir, "data", accountsFileName), nil
}

// refresh 按需重载账号文件（mtime 变了或距上次加载超过 60s）。
func (p *Pool) refresh() {
	fp, err := p.accountsPath()
	if err != nil {
		return
	}
	st, err := os.Stat(fp)
	if err != nil {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if st.ModTime().Equal(p.mtime) && time.Since(p.loadedAt) < 60*time.Second && len(p.state) > 0 {
		return
	}
	raw, err := os.ReadFile(fp)
	if err != nil {
		return
	}
	var f struct {
		Accounts []Account `json:"accounts"`
	}
	if err := json.Unmarshal(raw, &f); err != nil {
		return
	}
	prev := map[string]*acctState{}
	for _, s := range p.state {
		prev[s.UserID] = s
	}
	next := make([]*acctState, 0, len(f.Accounts))
	for _, a := range f.Accounts {
		if strings.TrimSpace(a.JWT) == "" {
			continue
		}
		if a.UserID == "" {
			a.UserID = userIDFromJWT(a.JWT)
		}
		s := prev[a.UserID]
		if s == nil {
			s = &acctState{}
		}
		s.Account = a
		next = append(next, s)
	}
	p.state = next
	p.mtime = st.ModTime()
	p.loadedAt = time.Now()
}

// Accounts 返回当前账号快照（只读副本）。
func (p *Pool) Accounts() []Account {
	p.refresh()
	p.mu.Lock()
	defer p.mu.Unlock()
	out := make([]Account, 0, len(p.state))
	for _, s := range p.state {
		out = append(out, s.Account)
	}
	return out
}

// Ready 池中是否有可用账号（不含冷却判定）。
func (p *Pool) Ready() bool { return len(p.Accounts()) > 0 }

// AccountStatus 单个账号的运行态（供面板/状态接口透出）。
type AccountStatus struct {
	Name          string `json:"name"`
	UserID        string `json:"user_id"`
	InFlight      int    `json:"in_flight"`
	MaxInFlight   int    `json:"max_in_flight"`
	Cooling       bool   `json:"cooling"`
	CooldownUntil string `json:"cooldown_until,omitempty"`
	LastError     string `json:"last_error,omitempty"`
	Failures      int    `json:"failures"`
}

// Status 返回全部账号运行态。
func (p *Pool) Status() []AccountStatus {
	p.refresh()
	p.mu.Lock()
	defer p.mu.Unlock()
	out := make([]AccountStatus, 0, len(p.state))
	for _, s := range p.state {
		as := AccountStatus{
			Name: s.Name, UserID: s.UserID,
			InFlight: s.inFlight, MaxInFlight: p.maxPer(),
			Cooling: time.Now().Before(s.cooldownUntil),
			LastError: s.lastErr, Failures: s.failures,
		}
		if as.Cooling {
			as.CooldownUntil = s.cooldownUntil.Format(time.RFC3339)
		}
		out = append(out, as)
	}
	return out
}

func (p *Pool) maxPer() int {
	if p.MaxPerAccount <= 0 {
		return 2
	}
	return p.MaxPerAccount
}

// Acquire 取一个可用账号（按 in-flight 少的优先 + 轮转打散），并占用一个并发名额。
// 全部账号都在冷却/满载时按 waitInterval 轮询等待，直到 ctx 结束。
func (p *Pool) Acquire(ctx context.Context) (*Account, error) {
	const waitInterval = 250 * time.Millisecond
	for {
		if a := p.tryAcquire(); a != nil {
			return a, nil
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(waitInterval):
		}
	}
}

func (p *Pool) tryAcquire() *Account {
	p.refresh()
	p.mu.Lock()
	defer p.mu.Unlock()
	now := time.Now()
	n := len(p.state)
	if n == 0 {
		return nil
	}
	// 从轮转指针开始扫一圈，选 in-flight 最小且未冷却的账号。
	best := -1
	bestLoad := 1 << 30
	for i := 0; i < n; i++ {
		idx := (p.rr + i) % n
		s := p.state[idx]
		if now.Before(s.cooldownUntil) {
			continue
		}
		if s.inFlight >= p.maxPer() {
			continue
		}
		if s.inFlight < bestLoad {
			bestLoad = s.inFlight
			best = idx
		}
	}
	if best < 0 {
		return nil
	}
	s := p.state[best]
	s.inFlight++
	p.rr = (best + 1) % n
	cp := s.Account
	return &cp
}

// Release 归还并发名额。
func (p *Pool) Release(a *Account) {
	if a == nil || a.UserID == "" {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, s := range p.state {
		if s.UserID == a.UserID && s.inFlight > 0 {
			s.inFlight--
			return
		}
	}
}

// MarkSuccess 清空该账号的失败计数。
func (p *Pool) MarkSuccess(a *Account) {
	if a == nil {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, s := range p.state {
		if s.UserID == a.UserID {
			s.failures = 0
			s.lastErr = ""
			return
		}
	}
}

// MarkFailure 记录一次失败并按类型退避。
func (p *Pool) MarkFailure(a *Account, err error) {
	if a == nil {
		return
	}
	p.mu.Lock()
	unauthorized := false
	for _, s := range p.state {
		if s.UserID != a.UserID {
			continue
		}
		s.failures++
		if err != nil {
			s.lastErr = err.Error()
			if len(s.lastErr) > 200 {
				s.lastErr = s.lastErr[:200]
			}
		}
		switch {
		case IsUnauthorized(err):
			s.cooldownUntil = time.Now().Add(p.cooldownAuth())
			unauthorized = true
		case IsParallelLimit(err):
			s.cooldownUntil = time.Now().Add(p.cooldownLimit())
		}
		break
	}
	hook := p.OnUnauthorized
	p.mu.Unlock()
	// 401 = JWT 过期。回调给面板尝试「从 Trae 客户端提取新令牌」自救
	// （客户端在线时会自己刷新 JWT）。异步触发，不阻塞失败路径。
	if unauthorized && hook != nil {
		go hook(a.UserID)
	}
}

func (p *Pool) cooldownLimit() time.Duration {
	if p.CooldownOnLimit <= 0 {
		return 20 * time.Second
	}
	return p.CooldownOnLimit
}

func (p *Pool) cooldownAuth() time.Duration {
	if p.CooldownOnAuth <= 0 {
		return 10 * time.Minute
	}
	return p.CooldownOnAuth
}

// ── 模型目录缓存 ─────────────────────────────────────────────────────────

// CatalogCache 带 TTL 的模型目录缓存（避免 /v1/models 每次打上游）。
type CatalogCache struct {
	pool *Pool
	ttl  time.Duration

	mu       sync.Mutex
	models   []ModelInfo
	fetched  time.Time
	lastErr  string
}

// NewCatalogCache 构造缓存；ttl<=0 时缺省 30min。
func NewCatalogCache(p *Pool, ttl time.Duration) *CatalogCache {
	if ttl <= 0 {
		ttl = 30 * time.Minute
	}
	return &CatalogCache{pool: p, ttl: ttl}
}

// Models 返回目录（命中缓存直接返回；失败时回落到上一次成功结果）。
//
// 冷启动（无缓存）时最多尝试 attempts 次并轮换账号：实测进程刚起来后的第一次
// 探测偶发失败（TLS 预热 / 账号文件刚被面板续期改写），重试即可成功；不重试会
// 让「重启后的第一次 /v1/models」返回空 Trae 名单。
func (c *CatalogCache) Models(ctx context.Context) ([]ModelInfo, error) {
	c.mu.Lock()
	if len(c.models) > 0 && time.Since(c.fetched) < c.ttl {
		out := c.dupLocked()
		c.mu.Unlock()
		return out, nil
	}
	c.mu.Unlock()

	attempts := 1
	c.mu.Lock()
	if len(c.models) == 0 {
		attempts = 3
	}
	c.mu.Unlock()

	var lastErr error
	for i := 0; i < attempts; i++ {
		if i > 0 {
			select {
			case <-ctx.Done():
				return c.stale()
			case <-time.After(time.Duration(i) * 500 * time.Millisecond):
			}
		}
		acct, err := c.pool.Acquire(ctx)
		if err != nil {
			return c.stale()
		}
		agent, work, err := c.pool.Client().Models(acct.JWT)
		c.pool.Release(acct)
		if err != nil {
			c.pool.MarkFailure(acct, err)
			lastErr = err
			continue
		}
		c.pool.MarkSuccess(acct)
		if merged := c.build(agent, work); len(merged) > 0 {
			c.mu.Lock()
			c.models = merged
			c.fetched = time.Now()
			c.lastErr = ""
			out := c.dupLocked()
			c.mu.Unlock()
			return out, nil
		}
		lastErr = os.ErrNotExist
	}
	_ = lastErr
	return c.stale()
}

// build 按「Agent 组优先」合并模型目录并做可用性过滤。
func (c *CatalogCache) build(agent, work []ModelInfo) []ModelInfo {
	// 模型目录取「Agent 执行器」分组（能力最全、1M 档位）；该分组为空时才回落到
	// Work 分组。两个分组的模型名高度重叠，混在一起会把 Work 组里的历史/自定义
	// 条目（如 "ai"、"glm-5"）也带出来，污染网关的模型面。
	base := agent
	if len(base) == 0 {
		base = work
	}
	seen := map[string]bool{}
	merged := make([]ModelInfo, 0, len(base))
	for _, m := range base {
		if !validModelName(m.Name) || seen[m.Name] {
			continue
		}
		// 只暴露上游明确给出**消耗倍率**的条目：倍率是唯一能做「太贵的不暴露」
		// 过滤的依据，没有倍率的（历史/自定义条目）既无法过滤也无法保证可用性。
		if !m.HasConsumptionRate {
			continue
		}
		seen[m.Name] = true
		merged = append(merged, m)
	}
	sort.SliceStable(merged, func(i, j int) bool { return merged[i].Name < merged[j].Name })
	return merged
}

func (c *CatalogCache) dupLocked() []ModelInfo {
	out := make([]ModelInfo, len(c.models))
	copy(out, c.models)
	return out
}

// validModelName 过滤明显不是模型 id 的条目（如 "ai"、带空格的展示名）。
func validModelName(name string) bool {
	s := strings.TrimSpace(name)
	if len(s) < 3 || len(s) > 96 {
		return false
	}
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
		case r == '-' || r == '_' || r == '.' || r == ':':
		default:
			return false
		}
	}
	return true
}

func (c *CatalogCache) stale() ([]ModelInfo, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.models) > 0 {
		return c.dupLocked(), nil
	}
	return nil, os.ErrNotExist
}

// Find 在目录里按名字（大小写不敏感，容忍 "trae:" 前缀）查模型。
func Find(models []ModelInfo, name string) (ModelInfo, bool) {
	want := strings.ToLower(strings.TrimPrefix(strings.TrimSpace(name), "trae:"))
	for _, m := range models {
		if strings.ToLower(m.Name) == want {
			return m, true
		}
	}
	// 容忍 display_name 命中
	for _, m := range models {
		if strings.ToLower(m.DisplayName) == want {
			return m, true
		}
	}
	return ModelInfo{}, false
}

// userIDFromJWT 从 JWT payload 取 user id（不校验签名）。
func userIDFromJWT(jwt string) string {
	tok := JWTToken(jwt)
	parts := strings.Split(tok, ".")
	if len(parts) < 2 {
		return ""
	}
	seg := parts[1]
	if pad := len(seg) % 4; pad != 0 {
		seg += strings.Repeat("=", 4-pad)
	}
	raw, err := base64.URLEncoding.DecodeString(seg)
	if err != nil {
		return ""
	}
	var v struct {
		Data struct {
			ID string `json:"id"`
		} `json:"data"`
		AuthID string `json:"auth_id"`
		Sub    string `json:"sub"`
	}
	if err := json.Unmarshal(raw, &v); err != nil {
		return ""
	}
	if v.Data.ID != "" {
		return v.Data.ID
	}
	if v.AuthID != "" {
		return v.AuthID
	}
	return v.Sub
}
