// codearts_api.go 华为云 CodeArts「活动福利」接口通道（接口优先，UI 兜底）。
//
// 接口契约（2026-10-27 从 CodeArtsWorkWeb 126.10.3 的前端 bundle 实测反查，
// 不是猜的；base = https://devcloud.cn-north-4.huaweicloud.com）：
//
//	GET  /PromptCenterService/v1/ops/delivery?channel=WEB
//	     → 活动列表（含每日签到活动）
//	POST /PromptCenterService/v1/ops/claim
//	     body {campaignId, idempotentKey, channel:"WEB"}
//	     → 领取；前端用的幂等键形如 wb_<campaignId>_<毫秒时间戳>
//	POST /PromptCenterService/v1/ops/confirm
//	     body {campaignId}
//	     → 领取后确认
//
// 公共请求头：Agent-Type: PromptCenter、X-Language: zh-cn、
// x-codearts-doer-scenario-type: web。Cookie 由浏览器自动带（同源）。
//
// 为什么在**页面上下文内**发请求：会话 Cookie 只活约一小时，存快照再注入
// 一律被判定未登录（实测 2026-10-27）；而在页面里发 fetch，用的是浏览器
// **当下**的活会话，和前端自己发请求完全等价。
package panel

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/codearts"
)

// caWelfareListJS 在页面里拉活动列表，吐出「每日签到」活动的信息。
//
// 返回：{"ok":bool,"http":int,"err":string,"daily":{...}|null,"items":n}
const caWelfareListJS = `(async () => {
  const H = {
    'Agent-Type': 'PromptCenter',
    'X-Language': 'zh-cn',
    'x-codearts-doer-scenario-type': 'web',
    'x-auth-token': '',
    'Accept': 'application/json, text/plain, */*'
  };
  try {
    const m = document.cookie.match(/(?:^|;\s*)(devclouddevuibjtcftk|cftk)=([^;]*)/);
    if (m && m[2]) H['cftk'] = m[2];
  } catch (e) {}
  const j = async (path, opt) => {
    try {
      const r = await fetch(path, Object.assign({ credentials: 'same-origin', headers: H }, opt || {}));
      const t = await r.text();
      let p = null;
      try { p = JSON.parse(t); } catch (e) {}
      return { status: r.status, json: p, text: p ? '' : t.slice(0, 300) };
    } catch (e) { return { status: -1, err: String(e) }; }
  };
  const list = await j('/PromptCenterService/v1/ops/delivery?channel=WEB');
  if (list.status < 0) return JSON.stringify({ ok: false, http: -1, err: list.err || 'fetch failed' });
  if (!list.json) {
    // 多半是登录跳转页（HTML）——会话没了。
    return JSON.stringify({ ok: false, http: list.status, err: 'not-json',
      snippet: (list.text || '').slice(0, 160) });
  }
  const root = list.json && list.json.data !== undefined ? list.json.data : list.json;
  // 递归找 type=DAILY_CLAIM 的活动（响应结构没定死，遍历一遍最稳）。
  const found = [];
  const walk = (o, depth) => {
    if (!o || depth > 7) return;
    if (Array.isArray(o)) { for (const x of o) walk(x, depth + 1); return; }
    if (typeof o !== 'object') return;
    const t = String(o.type || o.activityType || o.activity_type || '').toUpperCase().replace(/[-_]/g, '');
    if (t === 'DAILYCLAIM') found.push(o);
    for (const k of Object.keys(o)) walk(o[k], depth + 1);
  };
  walk(root, 0);
  const pick = found[0] || null;
  const d = pick ? {
    campaignId: String(pick.campaignId ?? pick.campaign_id ?? pick.id ?? ''),
    type: pick.type || '',
    status: pick.status || '',
    claimed: !!pick.claimed,
    claimable: pick.claimable,
    benefitAmount: pick.benefitAmount ?? pick.benefit_amount ?? 0,
    title: pick.title || '',
    description: pick.description || ''
  } : null;
  return JSON.stringify({ ok: true, http: list.status, daily: d,
    foundCount: found.length,
    code: list.json.code, message: list.json.message || '' });
})()`

// caWelfareClaimJS 在页面里领「每日签到」活动的奖励。
//
// 入参 campaignId 通过 JSON 注入（只允许字母数字下划线，避免拼进 JS 字符串）。
// 返回：{"ok":bool,"claimed":bool,"code":n,"message":string,"raw":string}
const caWelfareClaimJS = `(async () => {
  const CID = %CID%;
  const H = {
    'Agent-Type': 'PromptCenter',
    'X-Language': 'zh-cn',
    'x-codearts-doer-scenario-type': 'web',
    'x-auth-token': '',
    'Accept': 'application/json, text/plain, */*',
    'Content-Type': 'application/json'
  };
  try {
    const m = document.cookie.match(/(?:^|;\s*)(devclouddevuibjtcftk|cftk)=([^;]*)/);
    if (m && m[2]) H['cftk'] = m[2];
  } catch (e) {}
  const post = async (path, body) => {
    try {
      const r = await fetch(path, { method: 'POST', credentials: 'same-origin',
        headers: H, body: JSON.stringify(body) });
      const t = await r.text();
      let p = null;
      try { p = JSON.parse(t); } catch (e) {}
      return { status: r.status, json: p, text: p ? '' : t.slice(0, 300) };
    } catch (e) { return { status: -1, err: String(e) }; }
  };
  const idem = 'wb_' + CID + '_' + Date.now();
  const r = await post('/PromptCenterService/v1/ops/claim',
    { campaignId: CID, idempotentKey: idem, channel: 'WEB' });
  if (r.status < 0) return JSON.stringify({ ok: false, claimed: false, err: r.err || 'fetch failed' });
  if (!r.json) {
    return JSON.stringify({ ok: false, claimed: false, http: r.status, err: 'not-json',
      snippet: (r.text || '').slice(0, 160) });
  }
  const ok = r.json.code === 0 || r.json.code === 200 ||
    (r.json.data !== undefined && r.json.data !== null);
  // 领完再确认一次（前端也是这么做的）。
  let conf = null;
  if (ok) {
    const c = await post('/PromptCenterService/v1/ops/confirm', { campaignId: CID });
    conf = c.json ? { code: c.json.code, message: c.json.message || '' } : { err: 'not-json' };
  }
  return JSON.stringify({ ok: ok, claimed: ok, http: r.status,
    code: r.json.code, message: r.json.message || '',
    data: r.json.data === undefined ? null : r.json.data,
    confirm: conf, raw: JSON.stringify(r.json).slice(0, 600) });
})()`

// caWelfareActivity 页面侧解析出的「每日签到」活动。
type caWelfareActivity struct {
	CampaignID  string `json:"campaignId"`
	Type        string `json:"type"`
	Status      string `json:"status"`
	Claimed     bool   `json:"claimed"`
	Claimable   *bool  `json:"claimable"`
	BenefitAmt  any    `json:"benefitAmount"`
	Title       string `json:"title"`
	Description string `json:"description"`
}

// caWelfareIsClaimed 判定这个活动今天是否已经领过。
//
// 判据抄自前端 bundle（福利弹窗里的 claimed 计算）：
// status ∈ {CLAIMED, CONFIRM, CONFIRMED, CONSUME} 或 claimed=true → 已领。
func caWelfareIsClaimed(a caWelfareActivity) bool {
	if a.Claimed {
		return true
	}
	switch strings.ToUpper(strings.TrimSpace(a.Status)) {
	case "CLAIMED", "CONFIRM", "CONFIRMED", "CONSUME", "CONSUMED":
		return true
	}
	return false
}

// caWelfareClaimable 判定今天还能不能领。
//
// 判据抄自前端 bundle 的每日签到筛选：
// claimable !== false && !claimed && status 不在已领集合里。
func caWelfareClaimable(a caWelfareActivity) bool {
	if caWelfareIsClaimed(a) {
		return false
	}
	if a.Claimable != nil && !*a.Claimable {
		return false
	}
	return true
}

// caWelfareIDSafe 把 campaignId 收敛成能安全嵌进 JS 字符串的字面量。
func caWelfareIDSafe(s string) string {
	var b strings.Builder
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '_', r == '-':
			b.WriteRune(r)
		default:
			b.WriteByte('_')
		}
	}
	out := b.String()
	if out == "" {
		return "_"
	}
	if len(out) > 120 {
		out = out[:120]
	}
	return out
}

// caWelfareClaimJSFor 把 campaignId 填进领取脚本。
func caWelfareClaimJSFor(campaignID string) string {
	return strings.Replace(caWelfareClaimJS, "%CID%", strconv.Quote(caWelfareIDSafe(campaignID)), 1)
}

// caWelfareBenefitText 把 benefitAmount 拼成「1000 积分」这样的文案。
func caWelfareBenefitText(a caWelfareActivity) string {
	switch v := a.BenefitAmt.(type) {
	case float64:
		if v == 0 {
			return ""
		}
		return strconv.FormatFloat(v, 'f', -1, 64) + " 积分"
	case string:
		if v == "" {
			return ""
		}
		return v + " 积分"
	default:
		return ""
	}
}

// codeartsClaimViaAPI 走接口领取每日签到。
//
// 返回 ok=false 表示「接口这条路走不通」（网络/会话/结构不对），调用方应当
// 退回 UI 点按钮的兜底流程；ok=true 时 phase/msg/sample 就是最终结论。
func (p *Panel) codeartsClaimViaAPI(ws *wsConn) (phase, msg, sample string, ok bool) {
	raw, err := caEval(ws, caWelfareListJS)
	if err != nil {
		return "", "", "", false
	}
	var lr struct {
		OK         bool               `json:"ok"`
		HTTP       int                `json:"http"`
		Err        string             `json:"err"`
		Snippet    string             `json:"snippet"`
		Daily      *caWelfareActivity `json:"daily"`
		FoundCount int                `json:"foundCount"`
		Code       any                `json:"code"`
		Message    string             `json:"message"`
	}
	if json.Unmarshal(raw, &lr) != nil {
		return "", "", "", false
	}
	if !lr.OK {
		// 多半是登录过期（返回登录跳转 HTML）。这种情况要说人话。
		if lr.Err == "not-json" {
			return "pending", "这个账号的华为云登录已过期，请点右上角「添加账号」重新登录一次",
				"delivery http=" + itoa(lr.HTTP) + " " + trimText(lr.Snippet, 200), true
		}
		return "", "", "", false
	}
	diag := fmt.Sprintf("delivery http=%d found=%d msg=%q", lr.HTTP, lr.FoundCount, lr.Message)
	if lr.Daily == nil {
		return "pending", "上游活动列表里没有「每日签到」活动（活动可能已下线）", diag, true
	}
	a := *lr.Daily
	diag += fmt.Sprintf(" campaign=%s type=%s status=%s claimed=%v", a.CampaignID, a.Type, a.Status, a.Claimed)

	if caWelfareIsClaimed(a) {
		txt := caWelfareBenefitText(a)
		if txt != "" {
			txt = "（" + txt + "）"
		}
		return "already", "今日已领" + txt, diag, true
	}
	if !caWelfareClaimable(a) {
		return "pending", "今天暂时不能领（上游标记为不可领）", diag, true
	}
	if a.CampaignID == "" {
		return "", "", "", false
	}

	raw2, err := caEval(ws, caWelfareClaimJSFor(a.CampaignID))
	if err != nil {
		return "", "", "", false
	}
	var cr struct {
		OK      bool   `json:"ok"`
		Claimed bool   `json:"claimed"`
		HTTP    int    `json:"http"`
		Code    any    `json:"code"`
		Message string `json:"message"`
		Err     string `json:"err"`
		Snippet string `json:"snippet"`
		Raw     string `json:"raw"`
		Confirm any    `json:"confirm"`
	}
	if json.Unmarshal(raw2, &cr) != nil {
		return "", "", "", false
	}
	diag += " | claim http=" + itoa(cr.HTTP) + " code=" + fmt.Sprint(cr.Code)
	if cr.OK && cr.Claimed {
		txt := caWelfareBenefitText(a)
		if txt == "" {
			txt = "（+1000 积分）"
		}
		return "claimed", "签到成功 " + txt, diag + " " + cr.Raw, true
	}
	// 领失败：把上游的话原样带出来（别自己编）。
	upMsg := strings.TrimSpace(cr.Message)
	if upMsg == "" {
		upMsg = strings.TrimSpace(cr.Err)
	}
	if upMsg == "" {
		upMsg = "上游没有给出原因"
	}
	return "pending", "领取失败：" + upMsg, diag + " " + cr.Raw + " " + trimText(cr.Snippet, 200), true
}

// itoa 小的整数转字符串（避免为此引 strconv 之外的东西）。
func itoa(n int) string { return strconv.Itoa(n) }

// ── 账号视图 / 验活 / 补采端点（C-CA-API@1 A/B/C） ───────────────────────
//
// 路由由 T11 注册（panel.go 不归本任务）；这里只提供 handler：
//
//	GET  /panel/api/ide/codearts/accounts   → p.codeartsAccounts（既有，本任务补齐字段）
//	POST /panel/api/ide/codearts/keepalive  → p.codeartsKeepaliveHTTP
//	POST /panel/api/ide/codearts/probe      → p.codeartsProbeHTTP

// codeartsNeedsManual 是否需要人工重新登录（C-CA-API@1 A 的派生规则）。
//
// 三个来源任一成立即为真：
//  1. 补采确认会话已死（ProbeState=login_dead）；
//  2. 续期停在人工登录页（RenewResult=needs_login）；
//  3. 已知的绝对票据时间已经过去。
//
// ⚠️ 不包含"ticket_deadline 为空"：那是**会话级票据**的正常状态（R1.1.3），
// 把它当成需人工会导致"会话级账号永远显示要重新登录"。
func codeartsNeedsManual(a *codearts.Account) bool {
	if a == nil {
		return false
	}
	if a.ProbeState == "login_dead" || a.RenewResult == "needs_login" {
		return true
	}
	if strings.TrimSpace(a.TicketDeadline) == "" {
		return false
	}
	ddl, err := time.Parse(time.RFC3339, a.TicketDeadline)
	if err != nil {
		return false // 解析不了按"未知"处理，不得 panic、也不得当成已过期
	}
	return time.Now().After(ddl)
}

// codeartsKeepaliveResult POST keepalive 的单账号结果（C-CA-API@1 B）。
type codeartsKeepaliveResult struct {
	ID                string `json:"id"`
	OK                bool   `json:"ok"`
	Phase             string `json:"phase"` // alive|expired|network|error
	Message           string `json:"message"`
	CookieCount       int    `json:"cookie_count"`
	TicketDeadline    string `json:"ticket_deadline"`
	ExpiresKnownCount int    `json:"expires_known_count"`
	Refreshed         bool   `json:"refreshed"`
}

// codeartsProbeResult POST probe 的单账号结果（C-CA-API@1 C）。
type codeartsProbeResult struct {
	ID                 string `json:"id"`
	Phase              string `json:"phase"` // 见 Contract（含 throttled）
	Message            string `json:"message"`
	TicketDeadline     string `json:"ticket_deadline"`
	TicketSource       string `json:"ticket_source"`
	ExpiresKnownCount  int    `json:"expires_known_count"`
	SessionCookieCount int    `json:"session_cookie_count"`
	Evidence           string `json:"evidence"`
	ProbeAt            string `json:"probe_at"`
	RenewNextAt        string `json:"renew_next_at"`
}

// codeartsIDsFromBody 解析 {"ids":[…],"force":bool}。
//
// 空 body / 缺省 ids 都表示"全部账号"（契约 B/C 的约定），不是错误。
func codeartsIDsFromBody(r *http.Request) (ids []string, force bool, err error) {
	if r == nil || r.Body == nil {
		return nil, false, nil
	}
	raw, rerr := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	if rerr != nil {
		return nil, false, rerr
	}
	if len(bytes.TrimSpace(raw)) == 0 {
		return nil, false, nil
	}
	var req struct {
		IDs   []string `json:"ids"`
		Force bool     `json:"force"`
	}
	if uerr := json.Unmarshal(raw, &req); uerr != nil {
		return nil, false, fmt.Errorf("请求体不是合法 JSON")
	}
	return req.IDs, req.Force, nil
}

// codeartsSelectAccounts 按 ids 挑账号（空 = 全部）。
//
// 匹配用 AllIDs（本体 + 历史别名）：号池合并会把旧 ID 记进 Aliases，而前端
// 手里的 id 可能还是合并前那个；只比本体 ID 会让"点了没反应"。
func codeartsSelectAccounts(pool *codearts.Pool, ids []string) []*codearts.Account {
	if pool == nil {
		return nil
	}
	if len(ids) == 0 {
		out := make([]*codearts.Account, 0, len(pool.Accounts))
		for _, a := range pool.Accounts {
			if a != nil {
				out = append(out, a)
			}
		}
		return out
	}
	want := make(map[string]bool, len(ids))
	for _, id := range ids {
		if id = strings.TrimSpace(id); id != "" {
			want[id] = true
		}
	}
	var out []*codearts.Account
	for _, a := range pool.Accounts {
		if a == nil {
			continue
		}
		for _, id := range a.AllIDs() {
			if want[id] {
				out = append(out, a)
				break
			}
		}
	}
	return out
}

// codeartsCloneAccount 浅拷贝账号并把 Cookie 切片复制一份。
//
// 探活会往账号里写 LastOKAt / 新 Cookie（codearts.Keepalive 内部就改传入的
// 指针），不复制就会直接改到号池内存对象 —— 随后的 UpdatePool 又从磁盘读一份
// 新对象，改了等于白改。
func codeartsCloneAccount(a *codearts.Account) *codearts.Account {
	if a == nil {
		return nil
	}
	cp := *a
	cp.Cookies = append([]codearts.Cookie(nil), a.Cookies...)
	cp.Aliases = append([]string(nil), a.Aliases...)
	return &cp
}

// codeartsKeepaliveHTTP POST /panel/api/ide/codearts/keepalive
//
// 只做"验活 + 收新 Cookie"，不决定什么时候续期（那是 T3 的事）。
func (p *Panel) codeartsKeepaliveHTTP(w http.ResponseWriter, r *http.Request) {
	ids, _, err := codeartsIDsFromBody(r)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	pool, err := codearts.LoadPool()
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "读号池失败: "+err.Error())
		return
	}
	targets := codeartsSelectAccounts(pool, ids)
	results := make([]codeartsKeepaliveResult, len(targets))
	// 每号独立探活，互不依赖 → 并发；写盘由 UpdatePool 的包级锁串行化。
	var wg sync.WaitGroup
	for i, a := range targets {
		wg.Add(1)
		go func(i int, a *codearts.Account) {
			defer wg.Done()
			results[i] = p.codeartsKeepaliveOne(a)
		}(i, a)
	}
	wg.Wait()
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "results": results})
}

// codeartsKeepaliveOne 对一个账号做一次验活并持久化结果。
func (p *Panel) codeartsKeepaliveOne(a *codearts.Account) codeartsKeepaliveResult {
	out := codeartsKeepaliveResult{Phase: "error"}
	if a == nil {
		out.Message = "账号为空"
		return out
	}
	out.ID = a.ID
	out.CookieCount = len(a.Cookies)
	out.TicketDeadline = a.TicketDeadline
	out.ExpiresKnownCount = codeartsExpiresKnownCount(a.Cookies)
	if len(a.Cookies) == 0 {
		out.Message = "账号没有 Cookie"
		return out
	}
	snap := codeartsCloneAccount(a)
	before := codeartsCookieDigest(snap.Cookies)
	ok, err := codearts.Keepalive(snap)
	out.Phase, out.Message = codeartsKeepalivePhase(ok, err)
	out.OK = out.Phase == "alive"

	if out.OK {
		// 成功必写 LastOKAt；只有 Cookie 集合真的变了才算 refreshed 并刷新 SavedAt。
		out.Refreshed = codeartsPersistKeepalive(snap, before)
		// 只有成功才把新 Cookie 落盘；失败时号池里仍是旧的这批（别在结果里
		// 显示一个"已经吸进来了"的条数，那会让人以为结果已保存）。
		out.CookieCount = len(snap.Cookies)
		out.TicketDeadline = snap.TicketDeadline
		out.ExpiresKnownCount = codeartsExpiresKnownCount(snap.Cookies)
	}
	return out
}

// codeartsPersistKeepalive 把一次成功验活写回号池。
//
// 并发保护：如果本次请求期间号池里的 Cookie 已被别人（补采/续期/登录）换过，
// 不再用我们这份更旧的快照覆盖，只更新 LastOKAt。
func codeartsPersistKeepalive(snap *codearts.Account, beforeDigest string) (refreshed bool) {
	if snap == nil || snap.ID == "" {
		return false
	}
	lastOK := strings.TrimSpace(snap.LastOKAt)
	if lastOK == "" {
		lastOK = codearts.Now()
	}
	err := codearts.UpdatePool(func(pool *codearts.Pool) error {
		x := codeartsFindAccount(pool, snap.ID)
		if x == nil {
			return nil
		}
		if codeartsCookieDigest(x.Cookies) == beforeDigest &&
			codeartsCookieDigest(snap.Cookies) != beforeDigest {
			x.Cookies = append([]codearts.Cookie(nil), snap.Cookies...)
			x.SavedAt = codearts.Now()
			x.RefreshTicketDeadline()
			refreshed = true
		}
		x.LastOKAt = lastOK
		return nil
	})
	if err != nil {
		log.Printf("[codearts] 账号 %s 验活结果写号池失败: %v", snap.ID, err)
		return refreshed
	}
	return refreshed
}

// codeartsKeepalivePhase 验活结果 → (phase, message)。
//
// phase 只允许 alive|expired|network|error（C-CA-API@1 B）：
//   - expired 必须来自 errors.Is(err, codearts.ErrSessionExpired)；
//   - 网络错误（DNS/连接拒绝/超时）必须报 network，**不得**报 expired：
//     那会把一次 DNS 抖动写成"要重新登录"，用户就去重扫一次码了
//     （实测这台机器一天出现过两次 no such host）。
func codeartsKeepalivePhase(ok bool, err error) (string, string) {
	switch {
	case ok && err == nil:
		return "alive", "会话可用"
	case err != nil && errors.Is(err, codearts.ErrSessionExpired):
		return "expired", err.Error()
	case err != nil && codeartsNetworkError(err):
		return "network", "网络错误（不是会话失效）: " + err.Error()
	case err != nil:
		return "error", err.Error()
	}
	return "error", "接口返回非 200"
}

// codeartsNetworkError 是不是网络层错误（而不是"会话失效"）。
func codeartsNetworkError(err error) bool {
	if err == nil {
		return false
	}
	var ne net.Error
	if errors.As(err, &ne) {
		return true
	}
	s := strings.ToLower(err.Error())
	for _, kw := range []string{"no such host", "connection refused", "i/o timeout",
		"tls handshake timeout", "connection reset", "broken pipe", "network is unreachable"} {
		if strings.Contains(s, kw) {
			return true
		}
	}
	return false
}

// codeartsProbeHTTP POST /panel/api/ide/codearts/probe
//
// 补采到期时间。浏览器是独占资源 → 逐个账号串行执行（不能并发起两个 Chrome）。
func (p *Panel) codeartsProbeHTTP(w http.ResponseWriter, r *http.Request) {
	ids, force, err := codeartsIDsFromBody(r)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	pool, err := codearts.LoadPool()
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "读号池失败: "+err.Error())
		return
	}
	targets := codeartsSelectAccounts(pool, ids)
	now := time.Now()
	results := make([]codeartsProbeResult, 0, len(targets))
	for _, a := range targets {
		results = append(results, p.codeartsProbeOne(a, force, now))
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "results": results})
}

// codeartsProbeOne 对一个账号做一次补采（含 6 小时节流）。
func (p *Panel) codeartsProbeOne(a *codearts.Account, force bool, now time.Time) codeartsProbeResult {
	at := now.Format(time.RFC3339)
	out := codeartsProbeResult{ProbeAt: at}
	if a == nil {
		out.Phase = "error"
		out.Message = "账号为空"
		return out
	}
	out.ID = a.ID
	out.RenewNextAt = codeartsNextProbeAt(a, now).Format(time.RFC3339)
	if len(a.Cookies) == 0 {
		out.Phase = "no_cookie"
		out.Message = "这个账号没有 Cookie，无法补采；请先重新登录或粘贴 Cookie"
		out.TicketDeadline = a.TicketDeadline
		out.TicketSource = a.TicketSource
		return out
	}
	if !force {
		if throttled, next := codeartsProbeThrottled(a, now); throttled {
			out.Phase = "throttled"
			out.Message = "补采节流中（成功探测后 6 小时、失败后 2 小时各只探测一次）：下次可探测 " +
				next.Format(time.RFC3339) + "，需要立即执行请带 force=true"
			out.ProbeAt = a.ProbeAt
			out.RenewNextAt = next.Format(time.RFC3339)
			out.TicketDeadline = a.TicketDeadline
			out.TicketSource = a.TicketSource
			out.ExpiresKnownCount = codeartsExpiresKnownCount(a.Cookies)
			out.SessionCookieCount = codeartsSessionCookieCount(a.Cookies)
			return out
		}
	}

	res, err := p.codeartsBackfillAccount(a)
	if err != nil {
		// 浏览器/端口不可用：不改写号池，如实报错。
		out.Phase = "error"
		out.Message = err.Error()
		out.TicketDeadline = a.TicketDeadline
		out.TicketSource = a.TicketSource
		out.RenewNextAt = now.Add(codeartsProbeFailGap).Format(time.RFC3339)
		return out
	}
	out.Phase = res.Phase
	out.Message = res.Message
	out.TicketDeadline = res.TicketDeadline
	out.TicketSource = res.TicketSource
	out.ExpiresKnownCount = res.ExpiresKnownCount
	out.SessionCookieCount = res.SessionCookieCount
	out.Evidence = res.Evidence
	// 以号池**实际落盘**的值为准：补采期间可能并发登录/续期改过同一账号，
	// 直接把内存里的 res 当结果会让面板显示与号池文件不一致。
	if latest, lerr := codearts.LoadPool(); lerr == nil {
		if x := codeartsFindAccount(latest, a.ID); x != nil {
			if x.ProbeAt != "" {
				out.ProbeAt = x.ProbeAt
			}
			out.TicketDeadline = x.TicketDeadline
			out.TicketSource = x.TicketSource
			out.ExpiresKnownCount = codeartsExpiresKnownCount(x.Cookies)
			out.SessionCookieCount = codeartsSessionCookieCount(x.Cookies)
		}
	}
	if res.Phase == "login_dead" || res.Phase == "no_cookie" {
		// 失败后续期间隔 2 小时（R1.3.2），不是 6 小时。
		out.RenewNextAt = now.Add(codeartsProbeFailGap).Format(time.RFC3339)
	} else {
		out.RenewNextAt = now.Add(codeartsProbeGap).Format(time.RFC3339)
	}
	return out
}

// ── 身份槽端点（C-CA-SLOT@1）────────────────────────────────────────────
//
// 路由由 registerRepairRoutes 注册（见 routes_repair.go），这里只提供 handler：
//
//	GET  /panel/api/ide/codearts/slots        → 号池里每个账号的身份槽
//	POST /panel/api/ide/codearts/slot_forget  → 重置某账号的身份（换设备）
//
// 为什么要把槽暴露给面板：槽是"这台机器用哪个华为云身份"的唯一载体
// （evidence/huawei-client-extract-recon.md §H）。只有把 slot_dir / device_guid
// 展示出来，用户才能核对「两个号确实不在同一台设备上」，而不是凭感觉。

// codeartsSlotsHTTP GET /panel/api/ide/codearts/slots
//
// 数据全部来自 p.codeartsSlotsOverview()（号池 + 槽目录现状），额外计算只在
// 该函数内部；本 handler 不做派生，避免两处口径不一致。
func (p *Panel) codeartsSlotsHTTP(w http.ResponseWriter, r *http.Request) {
	slots := p.codeartsSlotsOverview()
	// 读号池失败时 overview 返回 nil：必须回空数组而不是 null，
	// 前端 `(d.slots || [])` 对 null 也能处理，但空数组才是"没有账号"的正常表达。
	if slots == nil {
		slots = make([]slotInfo, 0)
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "slots": slots})
}

// codeartsSlotForgetRequest POST slot_forget 的请求体。
type codeartsSlotForgetRequest struct {
	ID      string `json:"id"`
	Confirm bool   `json:"confirm"`
}

// codeartsSlotForgetHTTP POST /panel/api/ide/codearts/slot_forget
//
// 语义：把该账号的旧槽目录备份成 `<slotDir>.bak-<ts>`，然后换一个新的
// SlotID/DeviceGUID —— 对华为云而言等于"这台设备不再使用该身份"。
//
// 因为这件事**不可逆地换掉设备身份**（虽然旧槽有备份，但服务端已经认了
// 新设备），所以必须显式 confirm=true；缺确认一律 400 且回明确的
// code=confirm_required，让调用方知道不是参数格式问题而是"你还没答应"。
func (p *Panel) codeartsSlotForgetHTTP(w http.ResponseWriter, r *http.Request) {
	var req codeartsSlotForgetRequest
	if r.Body != nil {
		raw, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
		if err != nil {
			writeErr(w, http.StatusBadRequest, "读请求体失败: "+err.Error())
			return
		}
		if len(bytes.TrimSpace(raw)) > 0 {
			if uerr := json.Unmarshal(raw, &req); uerr != nil {
				writeErr(w, http.StatusBadRequest, "请求体不是合法 JSON")
				return
			}
		}
	}
	id := strings.TrimSpace(req.ID)
	if id == "" {
		writeErr(w, http.StatusBadRequest, "缺少 id：需要指明要重置哪个账号的身份槽")
		return
	}
	if err := codeartsSlotForgetConfirmError(req.Confirm); err != nil {
		// code 字段是给调用方的稳定判据；error 是给人看的原因。
		writeJSON(w, http.StatusBadRequest, map[string]any{
			"ok":    false,
			"code":  "confirm_required",
			"error": err.Error(),
		})
		return
	}

	pool, err := codearts.LoadPool()
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "读号池失败: "+err.Error())
		return
	}
	acc := codeartsFindAccount(pool, id)
	if acc == nil {
		writeErr(w, http.StatusNotFound, "账号不存在: "+id)
		return
	}
	acc.EnsureSlot()
	// 备份目录按"旧槽目录 + .bak-*"回溯：codeartsSlotForget 只负责改名，
	// 不改方法签名（本任务不改 codearts_slot.go），所以由 HTTP 层从磁盘找真实结果。
	oldSlotDir := codeartsSlotAbsoluteDir(acc)

	if err := p.codeartsSlotForget(acc); err != nil {
		// 前提已确认账号存在，到这里失败基本都是槽目录被占用/IO 错误 —— 如实回 500。
		writeErr(w, http.StatusInternalServerError, "重置身份槽失败: "+err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":         true,
		"id":         acc.ID,
		"slot_id":    acc.SlotID,
		"backup_dir": codeartsSlotLatestBackup(oldSlotDir),
	})
}

// codeartsSlotAbsoluteDir 账号槽目录的绝对路径（含 EnsureSlot 的就地补全）。
//
// 与 codeartsSlotInfo 里 slot_dir 的口径一致（都是 `<SlotRoot>/<SlotDir>`），
// 保证「列表里看到的目录」和「重置时备份的目录」是同一个。
func codeartsSlotAbsoluteDir(a *codearts.Account) string {
	if a == nil {
		return ""
	}
	a.EnsureSlot()
	dir := filepath.Join(codearts.SlotRoot(), a.SlotDir)
	if abs, err := filepath.Abs(dir); err == nil {
		return abs
	}
	return dir
}

// codeartsSlotLatestBackup 在旧槽目录旁边找最近一次重置产生的备份目录。
//
// 返回空串表示"旧槽本来就不存在，没有可备份的东西"（codeartsSlotForget 的
// 正常分支），调用方照实回空串，不能编一个路径出来。
//
// 用 ReadDir + 前缀匹配而不是 filepath.Glob：Windows 路径里出现 `[`、`*`
// 等 Glob 元字符会静默匹配不到，这里只做字符串前缀判断。
func codeartsSlotLatestBackup(oldDir string) string {
	oldDir = strings.TrimSpace(oldDir)
	if oldDir == "" {
		return ""
	}
	parent, base := filepath.Dir(oldDir), filepath.Base(oldDir)
	entries, err := os.ReadDir(parent)
	if err != nil {
		return ""
	}
	prefix := base + ".bak-"
	var best string
	var bestMod time.Time
	for _, e := range entries {
		if !e.IsDir() || !strings.HasPrefix(e.Name(), prefix) {
			continue
		}
		info, ierr := e.Info()
		if ierr != nil {
			continue
		}
		if best == "" || info.ModTime().After(bestMod) {
			best, bestMod = filepath.Join(parent, e.Name()), info.ModTime()
		}
	}
	if best == "" {
		return ""
	}
	if abs, aerr := filepath.Abs(best); aerr == nil {
		return abs
	}
	return best
}
