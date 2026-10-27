// ide_doubao.go 面板「豆包签到池」：多账号 Cookie 凭据 + CDP 浏览器通道签到。
//
// 为什么走 CDP 而不是直连接口：豆包签到接口（/alice/ug/campaign/landing/checkin）
// 依赖整套 AGW 设备签名上下文，纯 HTTP 带 Cookie 实测返回「系统错误」（710010702）；
// 而页面自身在浏览器里跑的就是完整链路 —— 无头浏览器注入 Cookie + 客户端 UA 后
// 打开活动页点「立即领取」，是参考实现（playwright 方案）与本项目 Trae 通道双重
// 验证过的路线。
//
// 接口：
//   - GET  /panel/api/ide/doubao/accounts     号池账号 + 本地签到记录
//   - POST /panel/api/ide/doubao/add_extract  从客户端提取当前登录 Cookie（自动重启客户端）
//   - POST /panel/api/ide/doubao/add_cookie   手动粘贴 Cookie
//   - POST /panel/api/ide/doubao/remove       移除账号 {"id"}
//   - POST /panel/api/ide/doubao/checkin      签到 {"ids":[...]}（缺省=全部）
//   - GET  /panel/api/ide/doubao/status       上游核实各账号签到状态（无头扫页面，不点）
package panel

import (
	"encoding/json"
	"fmt"
	"regexp"
	"log"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/doubao"
	"github.com/linguo2625469/workbuddy2api-panel/internal/ideauth"
)

// doubaoAccounts 号池列表 + 本地签到记录。
func (p *Panel) doubaoAccounts(w http.ResponseWriter, r *http.Request) {
	pool, err := doubao.LoadPool()
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "doubao_pool_read_failed: "+err.Error())
		return
	}
	type row struct {
		ID             string `json:"id"`
		Label          string `json:"label"`
		SavedAt        string `json:"saved_at"`
		Source         string `json:"source,omitempty"`
		SessionTail    string `json:"session_tail"`
		CookieCount    int    `json:"cookie_count"`
		CheckedInToday bool   `json:"checked_in_today"`
		CheckedInAt    string `json:"checked_in_at,omitempty"`
	}
	out := make([]row, 0, len(pool.Accounts))
	for _, a := range pool.Accounts {
		if a == nil {
			continue
		}
		it := row{
			ID: a.ID, Label: a.Label, SavedAt: a.SavedAt, Source: a.Source,
			SessionTail: tailStr(a.SessionID, 6), CookieCount: len(a.Cookies),
		}
		if ideauth.CheckinRecordTrusted("doubao", a.ID) {
			it.CheckedInToday = true
			if t, ok := ideauth.CheckedInAt("doubao", a.ID); ok {
				it.CheckedInAt = t.Format("01-02 15:04")
			}
		}
		out = append(out, it)
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":         true,
		"accounts":   out,
		"has_client": doubao.HasClient(),
	})
}

// doubaoAddExtract 从豆包客户端提取当前登录 Cookie。
//
// 客户端运行时 Cookie 库被独占锁定：先结束 Doubao.exe → 拷贝 → 解密 → 重启客户端。
// 客户端本来就常驻后台，闪断几秒对登录态无影响（Cookie 落盘后以盘上为准）。
func (p *Panel) doubaoAddExtract(w http.ResponseWriter, r *http.Request) {
	if !doubao.HasClient() {
		writeErr(w, http.StatusBadRequest, "未找到豆包客户端（%LOCALAPPDATA%\\Doubao），请先安装并登录")
		return
	}
	running := ideauth.ClientRunning("doubao")
	killNote := ""
	if running {
		killNote = ideauth.KillClient("doubao")
		if strings.Contains(killNote, "仍未退出") {
			writeErr(w, http.StatusBadGateway, "豆包客户端无法结束（"+killNote+"），请手动关闭后重试")
			return
		}
	}
	cookies, err := extractDoubaoCookies()
	launchNote := ""
	if running {
		launchNote = ideauth.LaunchClient("doubao")
	}
	if err != nil {
		writeErr(w, http.StatusBadGateway, "提取失败: "+err.Error())
		return
	}
	acc, added, err := doubao.Upsert(cookies, "client")
	if err != nil {
		writeErr(w, http.StatusBadRequest, "保存失败: "+err.Error())
		return
	}
	verb := "已添加"
	if !added {
		verb = "已更新"
	}
	log.Printf("panel: 豆包客户端提取: %s（%s，%d 个 Cookie）", acc.Label, verb, len(cookies))
	writeJSON(w, http.StatusOK, map[string]any{
		"ok": true, "added": added, "id": acc.ID, "label": acc.Label,
		"msg":     verb + "：" + acc.Label + "（" + fmt.Sprintf("%d 个 Cookie", len(cookies)) + "）",
		"note":    strings.TrimSpace(killNote + "；" + launchNote),
	})
}

// extractDoubaoCookies 拷贝 Cookie 库到临时文件再解密（原件可能被锁/正在写）。
func extractDoubaoCookies() (map[string]string, error) {
	src := doubao.ClientCookiesDB()
	raw, err := os.ReadFile(src)
	if err != nil {
		return nil, fmt.Errorf("读取 Cookie 库失败（客户端可能未退出）: %w", err)
	}
	tmp, err := os.CreateTemp("", "doubao-cookies-*.db")
	if err != nil {
		return nil, err
	}
	name := tmp.Name()
	defer os.Remove(name)
	if _, err := tmp.Write(raw); err != nil {
		tmp.Close()
		return nil, err
	}
	if err := tmp.Close(); err != nil {
		return nil, err
	}
	return doubao.ExtractFromCookiesDB(name)
}

// doubaoLaunch 启动豆包桌面客户端（可见窗口），用于「登录窗口」。
//
// 豆包的登录在客户端自己的 UI 里（扫码 / 账号密码），网页资源又是加密打包的
// （应用内有 liblark_resource_encrypt.dll），静态扫不到可复用的 OAuth 端点 —
// 所以这里的「登录窗口」= 拉起客户端本体的登录界面：你在客户端里登录完，
// 再点「从客户端提取当前账号」把登录态收进号池。
func (p *Panel) doubaoLaunch(w http.ResponseWriter, r *http.Request) {
	if !doubao.HasClient() {
		writeErr(w, http.StatusBadRequest, "未找到豆包客户端（%LOCALAPPDATA%\\Doubao），请先安装")
		return
	}
	if ideauth.ClientRunning("doubao") {
		writeJSON(w, http.StatusOK, map[string]any{
			"ok": true, "running": true,
			"msg": "豆包客户端已在运行 —— 直接在客户端里登录，登录完回面板点「从客户端提取当前账号」",
		})
		return
	}
	note := ideauth.LaunchClient("doubao")
	writeJSON(w, http.StatusOK, map[string]any{
		"ok": true, "running": true, "note": note,
		"msg": "已启动豆包客户端 —— 在客户端里登录要添加的账号，登录完回面板点「从客户端提取当前账号」",
	})
}

// doubaoAddCookie 手动粘贴 Cookie（浏览器 F12 → Network 任一请求 → Cookie 头整串）。
func (p *Panel) doubaoAddCookie(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Cookie string `json:"cookie"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || strings.TrimSpace(req.Cookie) == "" {
		writeErr(w, http.StatusBadRequest, "cookie 不能为空（粘贴 sessionid=...; ttwid=... 整串）")
		return
	}
	cookies := doubao.ParseCookieString(req.Cookie)
	if cookies["sessionid"] == "" && cookies["sessionid_ss"] == "" {
		writeErr(w, http.StatusBadRequest, "Cookie 里没有 sessionid —— 请复制包含登录态的完整 Cookie 串")
		return
	}
	acc, added, err := doubao.Upsert(cookies, "manual")
	if err != nil {
		writeErr(w, http.StatusBadRequest, "保存失败: "+err.Error())
		return
	}
	verb := "已添加"
	if !added {
		verb = "已更新"
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "added": added, "id": acc.ID, "label": acc.Label, "msg": verb + "：" + acc.Label})
}

// doubaoRemove 移除账号。
func (p *Panel) doubaoRemove(w http.ResponseWriter, r *http.Request) {
	var req struct {
		ID string `json:"id"`
	}
	_ = json.NewDecoder(r.Body).Decode(&req)
	if req.ID == "" {
		writeErr(w, http.StatusBadRequest, "missing id")
		return
	}
	if err := doubao.Remove(req.ID); err != nil {
		writeErr(w, http.StatusInternalServerError, "doubao_remove_failed: "+err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// doubaoCheckinResult 单账号签到结果。
type doubaoCheckinResult struct {
	ID     string `json:"id"`
	Label  string `json:"label"`
	Phase  string `json:"phase"` // claimed / already / claimable / login_required / error / unknown
	Msg    string `json:"msg"`
	Sample string `json:"sample,omitempty"`
}

// doubaoCheckin 签到（CDP 通道，逐账号串行 —— 每号一个无头浏览器会话）。
func (p *Panel) doubaoCheckin(w http.ResponseWriter, r *http.Request) {
	var req struct {
		IDs []string `json:"ids"`
	}
	_ = json.NewDecoder(r.Body).Decode(&req)
	pool, err := doubao.LoadPool()
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "doubao_pool_read_failed: "+err.Error())
		return
	}
	want := map[string]bool{}
	for _, id := range req.IDs {
		if id = strings.TrimSpace(id); id != "" {
			want[id] = true
		}
	}
	out := make([]doubaoCheckinResult, 0, len(pool.Accounts))
	claimed, already, failed := 0, 0, 0
	for _, a := range pool.Accounts {
		if a == nil || a.SessionID == "" {
			continue
		}
		if len(want) > 0 && !want[a.ID] {
			continue
		}
		res := p.doubaoSigninOne(a)
		switch res.Phase {
		case "claimed":
			claimed++
			_ = ideauth.MarkCheckedIn("doubao", a.ID)
		case "already":
			already++
			_ = ideauth.MarkCheckedIn("doubao", a.ID)
		case "claimable":
			// 扫到了按钮但没点成功：不标记
			failed++
		default:
			failed++
		}
		out = append(out, res)
	}
	msg := fmt.Sprintf("本次签到 %d 个 · 今日已领 %d 个 · 失败 %d 个", claimed, already, failed)
	writeJSON(w, http.StatusOK, map[string]any{
		"ok": true, "results": out,
		"claimed_accounts": claimed, "already_accounts": already, "failed_accounts": failed,
		"msg": msg,
	})
}

// doubaoSigninOne 对单个账号跑一次 CDP 签到（点击通道）。
func (p *Panel) doubaoSigninOne(a *doubao.Account) doubaoCheckinResult {
	res := doubaoCheckinResult{ID: a.ID, Label: a.Label}
	phase, msg, sample := doubaoRunInBrowser(a.Cookies, true, traeBrowserBaseDir(p))
	res.Phase, res.Msg, res.Sample = phase, msg, sample
	return res
}

// doubaoStatus 上游核实：无头浏览器扫活动页状态（不点击）。
func (p *Panel) doubaoStatus(w http.ResponseWriter, r *http.Request) {
	pool, err := doubao.LoadPool()
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "doubao_pool_read_failed: "+err.Error())
		return
	}
	out := make([]doubaoCheckinResult, 0, len(pool.Accounts))
	for _, a := range pool.Accounts {
		if a == nil || a.SessionID == "" {
			continue
		}
		phase, msg, sample := doubaoRunInBrowser(a.Cookies, false, traeBrowserBaseDir(p))
		res := doubaoCheckinResult{ID: a.ID, Label: a.Label, Phase: phase, Msg: msg, Sample: sample}
		if ideauth.CheckinRecordTrusted("doubao", a.ID) && (phase == "unknown" || phase == "error") {
			res.Phase = "already"
			res.Msg = "上游扫描未确认（" + msg + "）；本地记录今日已签"
		}
		out = append(out, res)
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "results": out})
}

// doubaoRunInBrowser CDP 主流程：注入 Cookie + 客户端 UA → 打开活动页 →
// （可选）点签到按钮 → 读取页面状态。全程无窗口，约 15-40 秒。
func doubaoRunInBrowser(cookies map[string]string, doClick bool, baseDir string) (phase, msg, sample string) {
	chrome, err := findChrome()
	if err != nil {
		return "error", "未找到 Chrome / Edge：" + err.Error(), ""
	}
	port, err := freePort(traeBrowserPorts)
	if err != nil {
		return "error", "无可用调试端口: " + err.Error(), ""
	}
	dir := filepath.Join(traeBrowserBaseDir(nil), "doubao-browser", "tmp-"+time.Now().Format("20060102-150405"))
	if abs, err := filepath.Abs(dir); err == nil {
		dir = abs
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "error", "创建浏览器目录失败: " + err.Error(), ""
	}
	defer os.RemoveAll(dir)
	cmd := exec.Command(chrome,
		"--headless=new", "--disable-gpu", "--no-first-run", "--no-default-browser-check",
		"--disable-extensions", "--window-size=1280,900",
		fmt.Sprintf("--remote-debugging-port=%d", port),
		"--user-data-dir="+dir,
		"about:blank",
	)
	if err := cmd.Start(); err != nil {
		return "error", "启动无头浏览器失败: " + err.Error(), ""
	}
	defer func() {
		traeCloseViaCDP(port)
		if cmd.Process != nil {
			traeWaitExit(cmd.Process, 4*time.Second)
		}
	}()
	// 等调试端口就绪
	deadline := time.Now().Add(20 * time.Second)
	wsURL := ""
	for time.Now().Before(deadline) {
		time.Sleep(700 * time.Millisecond)
		targets, err := traeBrowserTargets(port)
		if err == nil {
			for _, t := range targets {
				if t.WSURL != "" && (strings.HasPrefix(t.URL, "about:") || t.URL == "") {
					wsURL = t.WSURL
					break
				}
			}
			if wsURL == "" && len(targets) > 0 {
				wsURL = targets[0].WSURL
			}
		}
		if wsURL != "" {
			break
		}
	}
	if wsURL == "" {
		return "error", "浏览器调试端口未就绪", ""
	}
	ws, err := wsDial(wsURL, 5*time.Second)
	if err != nil {
		return "error", "CDP 连接失败: " + err.Error(), ""
	}
	defer ws.Close()

	doubaoCDPMu.Lock()
	defer doubaoCDPMu.Unlock()

	// 客户端 UA（活动页校验 navigator.userAgent 含 SamanthaDoubao/）。
	_, _ = doubaoCDPCall(ws, 1, "Emulation.setUserAgentOverride",
		map[string]any{"userAgent": doubao.ClientUA}, 5*time.Second)
	if _, err := doubaoCDPCall(ws, 2, "Network.enable", map[string]any{}, 5*time.Second); err != nil {
		return "error", "Network.enable 失败: " + err.Error(), ""
	}
	// 注入 Cookie（domain 统一 .doubao.com；核心是 sessionid/sessionid_ss）。
	nSet := 0
	for name, value := range cookies {
		if strings.TrimSpace(value) == "" {
			continue
		}
		params := map[string]any{
			"name": name, "value": value,
			"domain": ".doubao.com", "path": "/",
			"secure": true, "httpOnly": true,
		}
		if _, err := doubaoCDPCall(ws, 10+nSet, "Network.setCookie", params, 5*time.Second); err == nil {
			nSet++
		}
	}
	if nSet == 0 {
		return "error", "Cookie 注入失败", ""
	}
	_, _ = doubaoCDPCall(ws, 200, "Page.enable", map[string]any{}, 5*time.Second)
	// 在文档创建前注入 fetch/XHR 日志钩子：页面自己会调 landing/get（含
	// daily_checkin.checked_in_today）与 landing/checkin。读它的响应比看 DOM
	// 文案准确得多（豆包改版只改 DOM 不改接口）。
	_, _ = doubaoCDPCall(ws, 202, "Page.addScriptToEvaluateOnNewDocument", map[string]any{
		"source": doubaoNetLogJS,
	}, 5*time.Second)
	if _, err := doubaoCDPCall(ws, 201, "Page.navigate", map[string]any{"url": doubao.ActivityURL}, 15*time.Second); err != nil {
		return "error", "打开活动页失败: " + err.Error(), ""
	}
	// 等页面就绪。注意豆包活动页是渐进水合：静态壳先带出「立即领取」文案，
	// 签到卡片（每日签到 / 累计获得额度）后到 —— 只认「卡片出现」为就绪信号，
	// 孤立按钮最多多等 12 秒（防止对未挂载的壳点空）。最长 60 秒。
	var last doubaoScanState
	started := time.Now()
	deadline = time.Now().Add(60 * time.Second)
	for time.Now().Before(deadline) {
		time.Sleep(2 * time.Second)
		st, err := doubaoScanPage(ws, 300)
		if err == nil {
			last = st
			if st.Ready && (st.Already || st.Success || st.Login || st.Card != "" ||
				(st.Btn != "" && time.Since(started) > 12*time.Second)) {
				break
			}
		}
	}
	if !last.Ready {
		return "error", "活动页 45 秒内未加载完成", sampleOf(last.Sample)
	}
	if last.Login {
		return "login_required", "登录态已失效（活动页要求登录），请重新提取或更新 Cookie", sampleOf(last.Sample)
	}
	// 诊断：把页面全部请求捕获落盘（data/doubao_capture.json），定位签到状态字段来源。
	if rows := doubaoNetResults(ws, 315); len(rows) > 0 {
		if buf, err := json.MarshalIndent(rows, "", " "); err == nil {
			_ = os.WriteFile(filepath.Join(baseDir, "doubao_capture.json"), buf, 0o600)
		}
	}
	// 页面上下文内直接调一次只读状态接口（带页面自己的完整会话签名，
	// 纯站外直连会因缺设备签名被"系统错误"拦下，页内调用不受影响）。
	if ph, det, ok := doubaoPageStatusFetch(ws, 310); ok {
		if ph == "claimed" || ph == "already" {
			return ph, withTasks(det, doubaoTasksIf(doClick, ws)), sampleOf(last.Sample)
		}
		if !doClick && ph == "claimable" {
			return "claimable", "今日可领取（" + det + "）", sampleOf(last.Sample)
		}
	}
	if ph, det, ok := doubaoParseNet(doubaoNetResults(ws, 300)); ok {
		if ph == "claimed" || ph == "already" {
			// 已签时也顺手领任务奖励（用户要求：不做任务，但能领的都领掉）。
			//
			// ⚠️ 只在**点击通道**（doClick=true，即真正的签到流程）里领：
			// doubaoStatus 是只读扫描，不该去动页面上的按钮。
			return ph, withTasks(det, doubaoTasksIf(doClick, ws)), sampleOf(last.Sample)
		}
		if !doClick && ph == "claimable" {
			return "claimable", "今日可领取（" + det + "）", sampleOf(last.Sample)
		}
	}
	if !doClick {
		// 只查状态
		switch {
		case last.Success || last.Already || last.cardCheckedIn():
			return "already", "今日已签到", sampleOf(last.Sample)
		case last.Btn != "":
			hint := ""
			for _, r := range doubaoNetResults(ws, 310) {
				if strings.Contains(strings.ToLower(r.Body), "checkin") || strings.Contains(r.Body, "签到") {
					b := r.Body
					if len(b) > 3000 {
						b = b[:1500] + "……" + b[len(b)-1500:]
					}
					hint += "[" + r.URL[:min(70, len(r.URL))] + "] " + b + " ;; "
				}
			}
			return "claimable", "今日可领取（检测到按钮：" + last.Btn + "）", hint[:min(len(hint), 6000)]
		default:
			return "unknown", "未识别到签到卡片（页面可能改版）", sampleOf(last.Sample)
		}
	}
	// 点击签到
	if last.cardCheckedIn() {
		return "already", "今日已签到（签到卡片已显示已签状态）", sampleOf(last.Sample)
	}
	if last.Btn == "" && !last.Already {
		return "unknown", "页面上没有可点的签到按钮", sampleOf(last.Sample)
	}
	if last.Btn != "" {
		clicked := ""
		// 点击偶发被页面重渲染吞掉：定位+真实点击重试两轮。
		for i := 0; i < 2 && clicked == ""; i++ {
			if i > 0 {
				time.Sleep(2 * time.Second)
				if st, serr := doubaoScanPage(ws, 600); serr == nil {
					last = st
					if last.cardCheckedIn() {
						return "already", "今日已签到（重扫后确认）", sampleOf(last.Sample)
					}
				}
			}
			text, x, y, lerr := doubaoLocateSignin(ws, 400)
			if lerr != nil || text == "" {
				continue
			}
			if cerr := doubaoCDPClick(ws, 401, x, y); cerr != nil {
				continue
			}
			clicked = text
		}
		if clicked == "" {
			return "unknown", "点击签到按钮失败｜候选: " + strings.Join(last.Cands, " ;; "), sampleOf(last.Sample)
		}
		// 等结果弹窗/文案（最多 15 秒）。点击后的「今日已领取」是明确信号
		//（刚点过签到按钮，该文案只可能来自签到本身）；点击前的扫描则保持
		// 严格判据（只认已签到/明日再来），避免任务行的「已领取」干扰。
		deadline = time.Now().Add(15 * time.Second)
		for time.Now().Before(deadline) {
			time.Sleep(1500 * time.Millisecond)
			// 点击后优先读 checkin 接口响应（权威）。
			if ph, det, ok := doubaoParseNet(doubaoNetResults(ws, 500)); ok && ph != "claimable" {
				return ph, withTasks(det+"（点击后接口确认）", doubaoClaimTasks(ws)), sampleOf(last.Sample)
			}
			st2, err := doubaoScanPage(ws, 500)
			if err == nil {
				if st2.Success {
					return "claimed", withTasks("签到成功", doubaoClaimTasks(ws)), sampleOf(st2.Sample)
				}
				bodyAlready := st2.Already || st2.cardCheckedIn() ||
					strings.Contains(st2.Sample, "今日已领取") || strings.Contains(st2.Sample, "明日再来")
				if bodyAlready {
					return "already", withTasks("今日已签到（点击后页面显示已签）", doubaoClaimTasks(ws)), sampleOf(st2.Sample)
				}
			}
		}
		return "unknown", "已点击「" + clicked + "」但未确认结果，请手动核对", sampleOf(last.Sample + " ｜ 候选: " + strings.Join(last.Cands, " ;; "))
	}
	// 扫描时就是已签状态
	return "already", "今日已签到", sampleOf(last.Sample)
}

// doubaoNetLogJS 文档创建前注入：记录页面自身发起的 checkin/campaign 请求与响应。
const doubaoNetLogJS = `(() => {
  if (window.__wb2api) return;
  window.__wb2api = [];
  const want = u => true; // 全量捕获：豆包的签到状态字段来源不定，先抓全量再在服务端筛
  const rec = (url, status, body) => {
    const row = { url: String(url).slice(0, 200), status, body: String(body || '') };
    row.body = row.body.slice(0, 200000);
    try { window.__wb2api.push(row); } catch (e) {}
    // 活动接口跑在 iframe 里：镜像到顶层 window（同源），主框架读取才能看到。
    try { if (parent && parent !== window) { (parent.__wb2api = parent.__wb2api || []).push(row); } } catch (e) {}
    try { if (top && top !== window && top !== parent) { (top.__wb2api = top.__wb2api || []).push(row); } } catch (e) {}
  };
  try {
    const of = window.fetch;
    if (of) window.fetch = async function (...args) {
      const res = await of.apply(this, args);
      try {
        const url = String((args[0] && args[0].url) || args[0] || '');
        if (want(url)) { const c = res.clone(); c.text().then(t => rec(url, res.status, t)).catch(() => {}); }
      } catch (e) {}
      return res;
    };
  } catch (e) {}
  try {
    const oo = XMLHttpRequest.prototype.open, os = XMLHttpRequest.prototype.send;
    XMLHttpRequest.prototype.open = function (m, u) { this.__u = u; return oo.apply(this, arguments); };
    XMLHttpRequest.prototype.send = function () {
      this.addEventListener('load', () => {
        try {
          if (!want(this.__u)) return;
          let b = '';
          try { b = this.responseText; } catch (e) { try { b = JSON.stringify(this.response); } catch (e2) {} }
          rec(this.__u, this.status, b);
        } catch (e) {}
      });
      return os.apply(this, arguments);
    };
  } catch (e) {}
})();`

// doubaoNetRow 钩子记录的一条接口响应。
type doubaoNetRow struct {
	URL    string `json:"url"`
	Status int    `json:"status"`
	Body   string `json:"body"`
}

// doubaoNetResults 读取钩子记录的接口响应。
func doubaoNetResults(ws *wsConn, id int) []doubaoNetRow {
	raw, err := doubaoCDPCall(ws, id, "Runtime.evaluate", map[string]any{
		"expression": `JSON.stringify(window.__wb2api || [])`, "returnByValue": true,
	}, 6*time.Second)
	if err != nil {
		return nil
	}
	var out []doubaoNetRow
	if json.Unmarshal(raw, &out) != nil {
		return nil
	}
	return out
}

// doubaoCheckinRe 从 v3/detail 响应里提取 daily_checkin 状态
//（{"campaign_code":"daily_signin",...,"daily_checkin":{"checked_in_today":true,
//   "continuous_days":1,"last_checkin_date":"2026-10-24",...}}）。
var doubaoCheckinRe = regexp.MustCompile(
	`"daily_checkin":\{"checked_in_today":(true|false),"continuous_days":(\d+),"last_checkin_date":"([^"]+)"`)

// doubaoParseNet 从钩子记录里判定签到状态（phase, 说明, 是否有结论）。
// 状态来源：/alice/ug/campaign/v3/detail/ 的 tasks[].daily_checkin（页面加载即拉）
// 与点击后 /alice/ug/campaign/landing/checkin 的响应。
func doubaoParseNet(rows []doubaoNetRow) (phase, detail string, ok bool) {
	for _, r := range rows {
		lu := strings.ToLower(r.URL)
		body := r.Body
		// ① landing/checkin 的直接响应（点击后才有）
		if strings.Contains(lu, "landing/checkin") {
			lb := strings.ToLower(body)
			if strings.Contains(lb, `"checked_in":true`) || strings.Contains(lb, `"checked_in": true`) {
				return "claimed", "接口确认签到成功", true
			}
			if strings.Contains(lb, "already") || strings.Contains(body, "已领取") || strings.Contains(body, "已签到") {
				return "already", "接口返回今日已领取", true
			}
		}
		// ② v3/detail 里的 daily_checkin（页面加载即有，最可靠）
		if m := doubaoCheckinRe.FindStringSubmatch(body); m != nil {
			if m[1] == "true" {
				return "already", "签到接口确认今日已签（连续 " + m[2] + " 天，最近 " + m[3] + "）", true
			}
			return "claimable", "签到接口确认今日未签（上次 " + m[3] + "）", true
		}
	}
	return "", "", false
}

// doubaoPageStatusJS 页面内调 landing/get 查每日签到状态（只读）。
const doubaoPageStatusJS = `(async () => {
  try {
    const r = await fetch('/alice/ug/campaign/landing/get', {
      method: 'POST', credentials: 'include',
      headers: { 'content-type': 'application/json' },
      body: JSON.stringify({ scene: '', biz_params: {} })
    });
    return await r.text();
  } catch (e) { return 'FETCH_ERR:' + (e && e.message); }
})()`

// doubaoPageStatusFetch 页面内查状态，解析 daily_checkin.checked_in_today。
func doubaoPageStatusFetch(ws *wsConn, id int) (phase, detail string, ok bool) {
	raw, err := doubaoCDPCall(ws, id, "Runtime.evaluate", map[string]any{
		"expression": doubaoPageStatusJS, "returnByValue": true, "awaitPromise": true,
	}, 15*time.Second)
	if err != nil {
		return "", "", false
	}
	body := string(raw)
	lb := strings.ToLower(body)
	if strings.HasPrefix(lb, "fetch_err") {
		return "", "", false
	}
	if i := strings.Index(lb, `"checked_in_today"`); i >= 0 {
		seg := lb[i:min(len(lb), i+40)]
		if strings.Contains(seg, "true") {
			return "already", "签到接口确认今日已签", true
		}
		if strings.Contains(seg, "false") {
			return "claimable", "签到接口确认今日未签", true
		}
	}
	if strings.Contains(lb, `"code":0`) && strings.Contains(body, "daily_checkin") {
		return "", "", false // 有数据但没有 checked_in_today 字段：按改版处理，走 DOM 判据
	}
	return "", "", false
}

// doubaoScanState 页面扫描快照。
type doubaoScanState struct {
	Ready   bool   `json:"ready"`
	Btn     string `json:"btn"`
	Already bool   `json:"already"`
	Success bool   `json:"success"`
	Card    string `json:"card"`
	Login   bool   `json:"login"`
	Sample  string `json:"sample"`
	Cands   []string `json:"cands,omitempty"`
}

// cardCheckedIn 签到卡片区域显示「已签到」（今天已签的最可靠页面信号）。
func (s doubaoScanState) cardCheckedIn() bool {
	return s.Card != "" && (strings.Contains(s.Card, "已签到") || strings.Contains(s.Card, "明日再来"))
}

// doubaoScanJS 页面状态扫描（按钮文案候选与参考实现对齐，豆包改版改这里）。
const doubaoScanJS = `(() => {
  const texts = ['立即签到','每日签到','签到领额度','今日签到','立即领取','立即签到领额度'];
  // 已签判据只认「已签到 / 明日再来」：任务行的「已领取」不能当签到完成信号，
  // 否则「做任务领额度」列表里随便一个已完成的任务都会把未签误报成已签。
  const alreadyTexts = ['已签到','明日再来'];
  const successTexts = ['签到成功','领取成功','额度已到账'];
  const body = (document.body && document.body.innerText) || '';
  const has = arr => arr.some(x => body.includes(x));
  let btn = null;
  const els = [...document.querySelectorAll('button,[role=button],a,div,span')];
  const vis = e => { const r = e.getBoundingClientRect(); return r.width > 0 && r.height > 0; };
  const pick = (root) => {
    const pool = root ? [...root.querySelectorAll('button,[role=button],a,div,span')] : els;
    for (const tx of texts) {
      for (const e of pool) {
        const t = (e.innerText || '').trim();
        if (t === tx && !e.disabled && vis(e)) return tx;
      }
    }
    return null;
  };
  let cardEl = null;
  for (const e of els) {
    const t = (e.innerText || '').trim();
    if (t && t.includes('每日签到') && t.length < 200) { cardEl = e; break; }
  }
  btn = (cardEl && pick(cardEl)) || pick(null);
  // 签到卡片区域：包含「每日签到」的最小元素文本（卡片内若已是「已签到」状态，
  // 说明今天签过 —— 页面上其它卡片的「立即领取」按钮会干扰判断）。
  let card = '';
  for (const e of els) {
    const t = (e.innerText || '').trim();
    if (t && t.includes('每日签到') && t.length < 200) { card = t; break; }
  }
  const cands = [];
  for (const e of els) {
    const t = (e.innerText || '').replace(/\s+/g, ' ').trim();
    if (t && t.length <= 20 && (t.includes('签到') || t.includes('领取'))) {
      const r = e.getBoundingClientRect();
      cands.push(e.tagName + '|' + t + '|dis=' + !!e.disabled + '|vis=' + (r.width > 0 && r.height > 0) + '|len=' + t.length);
      if (cands.length >= 15) break;
    }
  }
  return JSON.stringify({
    ready: document.readyState === 'complete' || document.readyState === 'interactive',
    btn, already: has(alreadyTexts), success: has(successTexts),
    card, login: body.includes('扫码登录') || body.includes('手机号登录'),
    cands,
    sample: body.replace(/\s+/g, ' ').slice(0, 500)
  });
})()`

// doubaoLocateJS 定位签到按钮的中心坐标。返回坐标而不是直接 e.click()：
// React 按钮的合成 click 可能触发同步导航把执行上下文销毁（实测），真实鼠标
// 事件（CDP Input 域）与 playwright 同机制，最稳。
const doubaoLocateJS = `(async () => {
  // 顺序很重要：先「立即签到」，再兜底「立即领取」。
  const texts = ['立即签到','每日签到','签到领额度','今日签到','立即领取','立即签到领额度'];
  const els = [...document.querySelectorAll('button,[role=button],a,div,span')];
  const vis = e => { const r = e.getBoundingClientRect(); return r.width > 0 && r.height > 0; };
  const pick = (root) => {
    const pool = root ? [...root.querySelectorAll('button,[role=button],a,div,span')] : els;
    for (const tx of texts) {
      for (const e of pool) {
        const t = (e.innerText || '').trim();
        if (t === tx && !e.disabled && vis(e)) return { e: e, tx: tx };
      }
    }
    return null;
  };
  // 1) 优先在「每日签到」卡片内部找：页面上方常有别的活动卡也写着「立即领取」，
  //    全局取第一个会点错卡（用户看到的现象：点了但没签到）。
  let card = null;
  for (const e of els) {
    const t = (e.innerText || '').trim();
    if (t && t.includes('每日签到') && t.length < 200) { card = e; break; }
  }
  let hit = card ? pick(card) : null;
  // 2) 卡片里没找到，再退回全局
  if (!hit) hit = pick(null);
  if (!hit) return null;
  const e = hit.e;
  e.scrollIntoView({ block: 'center', inline: 'center' });
  await new Promise(r => setTimeout(r, 500));
  const r = e.getBoundingClientRect();
  return JSON.stringify({ text: hit.tx, x: Math.round(r.x + r.width / 2), y: Math.round(r.y + r.height / 2) });
})()`

// doubaoLocateSignin 找到签到按钮中心坐标（未找到返回 nil）。
func doubaoLocateSignin(ws *wsConn, id int) (text string, x, y float64, err error) {
	raw, err := doubaoCDPCall(ws, id, "Runtime.evaluate", map[string]any{
		"expression": doubaoLocateJS, "returnByValue": true, "awaitPromise": true,
	}, 12*time.Second)
	if err != nil {
		return "", 0, 0, err
	}
	var loc *struct {
		Text string  `json:"text"`
		X    float64 `json:"x"`
		Y    float64 `json:"y"`
	}
	if err := json.Unmarshal(raw, &loc); err != nil || loc == nil {
		return "", 0, 0, nil
	}
	return loc.Text, loc.X, loc.Y, nil
}

// doubaoCDPClick 用 CDP Input 域在 (x,y) 模拟一次真实鼠标点击：
// 先移入（产生 mouseover），按下后停 90ms 再释放 —— 与真人点击节奏一致，
// 页面的 React 合成事件依赖完整的事件序列。
func doubaoCDPClick(ws *wsConn, id int, x, y float64) error {
	move := map[string]any{"type": "mouseMoved", "x": x, "y": y, "button": "none"}
	if _, err := doubaoCDPCall(ws, id, "Input.dispatchMouseEvent", move, 5*time.Second); err != nil {
		return err
	}
	time.Sleep(120 * time.Millisecond)
	press := map[string]any{"type": "mousePressed", "x": x, "y": y, "button": "left", "buttons": 1, "clickCount": 1}
	if _, err := doubaoCDPCall(ws, id+1, "Input.dispatchMouseEvent", press, 5*time.Second); err != nil {
		return err
	}
	time.Sleep(90 * time.Millisecond)
	release := map[string]any{"type": "mouseReleased", "x": x, "y": y, "button": "left", "buttons": 0, "clickCount": 1}
	if _, err := doubaoCDPCall(ws, id+2, "Input.dispatchMouseEvent", release, 5*time.Second); err != nil {
		return err
	}
	return nil
}

func doubaoScanPage(ws *wsConn, id int) (doubaoScanState, error) {
	raw, err := doubaoCDPCall(ws, id, "Runtime.evaluate", map[string]any{
		"expression": doubaoScanJS, "returnByValue": true, "awaitPromise": true,
	}, 8*time.Second)
	if err != nil {
		return doubaoScanState{}, err
	}
	var out doubaoScanState
	if err := json.Unmarshal(raw, &out); err != nil {
		return doubaoScanState{}, err
	}
	return out, nil
}

// doubaoCDPMu CDP 会话内互斥（同一 ws 的消息必须串行；多账号签到本身也串行）。
var doubaoCDPMu sync.Mutex

// doubaoCDPCall 发一条 CDP 命令并等它的响应（按 id 匹配，其它事件忽略）。
func doubaoCDPCall(ws *wsConn, id int, method string, params map[string]any, timeout time.Duration) (json.RawMessage, error) {
	req := map[string]any{"id": id, "method": method, "params": params}
	buf, _ := json.Marshal(req)
	if err := ws.writeText(buf); err != nil {
		return nil, err
	}
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		msg, err := ws.readMessage(timeout)
		if err != nil {
			return nil, err
		}
		var resp struct {
			ID     int             `json:"id"`
			Method string          `json:"method"`
			Result json.RawMessage `json:"result"`
			Error  *struct {
				Message string `json:"message"`
			} `json:"error"`
		}
		if json.Unmarshal(msg, &resp) != nil {
			continue
		}
		// 豆包活动页点「立即领取」会弹原生 confirm（Runtime.evaluate 会被挂住直到
		// 弹窗被应答）。这里自动点「确定」——签到动作本身就是我们要的。
		if resp.Method == "Page.javascriptDialogOpening" {
			buf, _ := json.Marshal(map[string]any{
				"id": 999999, "method": "Page.handleJavaScriptDialog",
				"params": map[string]any{"accept": true},
			})
			_ = ws.writeText(buf)
			continue
		}
		if resp.ID != id {
			continue
		}
		if resp.Error != nil {
			return nil, fmt.Errorf("%s: %s", method, resp.Error.Message)
		}
		// Runtime.evaluate 的业务返回在 result.result.value（字符串内嵌 JSON）。
		if method == "Runtime.evaluate" {
			var inner struct {
				Result struct {
					Value string `json:"value"`
				} `json:"result"`
			}
			if json.Unmarshal(resp.Result, &inner) == nil && inner.Result.Value != "" {
				return json.RawMessage(inner.Result.Value), nil
			}
			return json.RawMessage(`{}`), nil
		}
		return resp.Result, nil
	}
	return nil, fmt.Errorf("%s: CDP 响应超时", method)
}

func sampleOf(s string) string {
	if len(s) > 300 {
		return s[:300]
	}
	return s
}

func tailStr(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[len(s)-n:]
}

// doubaoClaimTasksJS 把页面上「已完成待领」的任务奖励点一遍。
// 只点「领取 / 立即领取 / 去领取 / 一键领取」，不动「去完成」，避免误触发任务流程。
const doubaoClaimTasksJS = `(async () => {
  const sleep = ms => new Promise(r => setTimeout(r, ms));
  // 豆包活动页是渐进水合：先等任务区渲染出来再找按钮。
  await sleep(2500);
  const vis = e => { const r = e.getBoundingClientRect(); return r.width > 0 && r.height > 0; };
  const texts = ['领取','立即领取','去领取','一键领取'];
  let n = 0;
  for (let round = 0; round < 2; round++) {
    const btns = [...document.querySelectorAll('button,[role=button],div,span')].filter(e => {
      const t = (e.innerText || '').trim();
      return texts.includes(t) && !e.disabled && vis(e);
    });
    if (!btns.length) break;
    for (const b of btns.slice(0, 8)) {
      try { b.click(); n++; await sleep(1000); } catch (e) {}
    }
    await sleep(1200);
  }
  return JSON.stringify({ n: n });
})()`

// doubaoTasksIf 只在点击通道里领任务奖励；只读扫描（doClick=false）不点页面。
func doubaoTasksIf(doClick bool, ws *wsConn) int {
	if !doClick {
		return 0
	}
	return doubaoClaimTasks(ws)
}

// doubaoClaimTasks 执行任务奖励领取，返回成功点击的次数（失败按 0 处理，不阻断签到结果）。
func doubaoClaimTasks(ws *wsConn) int {
	if ws == nil {
		return 0
	}
	raw, err := doubaoCDPCall(ws, 777, "Runtime.evaluate", map[string]any{
		"expression": doubaoClaimTasksJS, "awaitPromise": true, "returnByValue": true,
	}, 45*time.Second)
	if err != nil {
		return 0
	}
	var r struct {
		N int `json:"n"`
	}
	if json.Unmarshal(raw, &r) != nil {
		return 0
	}
	return r.N
}

// withTasks 在结论文案后追加任务奖励数量。
func withTasks(base string, n int) string {
	if n > 0 {
		return fmt.Sprintf("%s（另领 %d 个任务奖励）", base, n)
	}
	return base
}
