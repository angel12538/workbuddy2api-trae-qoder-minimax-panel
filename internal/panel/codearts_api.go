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
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
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
	CampaignID   string `json:"campaignId"`
	Type         string `json:"type"`
	Status       string `json:"status"`
	Claimed      bool   `json:"claimed"`
	Claimable    *bool  `json:"claimable"`
	BenefitAmt   any    `json:"benefitAmount"`
	Title        string `json:"title"`
	Description  string `json:"description"`
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
