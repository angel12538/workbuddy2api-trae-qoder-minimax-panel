// codearts_browser.go 华为云「码道 CodeArts」浏览器登录 + 每日领取。
//
// 为什么必须走浏览器：CodeArts Agent 客户端里没有签到接口，签到入口是控制台/
// 网页的「活动福利 → 每日领取」，凭据是网页会话（客户端本地只有 STS 临时凭证，
// 调不了活动接口）。
//
// 为什么必须用**持久浏览器 profile** 而不是 Cookie 快照（2026-10-27 实测）：
// 华为云的登录会话活不过一小时（SSOTGC / SessionID 是短时票据）。10-26 22:15
// 抓的 61 个 Cookie，10-27 09:00 注入 headless 后：
//   - /chat/home 仍然显示「登录华为云」（应用自己的登录态判断）；
//   - /developer/ 直接 302 到 auth.huaweicloud.com 登录页；
//   - 纯 HTTP 带同一批 Cookie 与不带 Cookie，响应完全一致。
// ⇒ Cookie 快照只能当**身份标识**，签到必须复用登录时那个浏览器的 profile。
//
// 流程：
//  1. 「添加账号」→ 面板用一个**新 profile 目录**拉起可见浏览器，用户登录一次；
//  2. 面板检测到登录（页面右上角不再有「登录华为云」）→ 提取 Cookie 认人
//     （用华为云稳定账号 ID domain_tag/user_tag，同一账号重复登录不新增条目）
//     → 关掉浏览器，**把 profile 目录归档到该账号名下**；
//  3. 每天签到：用该账号的 profile 拉一个无头浏览器，打开控制台点「每日领取」。
package panel

import (
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/codearts"
	"github.com/linguo2625469/workbuddy2api-panel/internal/ideauth"
)

const (
	codeartsConsoleURL = "https://devcloud.cn-north-4.huaweicloud.com/chat/"
	codeartsTTL        = 30 * time.Minute
)

var codeartsBrowserSession struct {
	mu        sync.Mutex
	port      int
	dir       string
	cmd       *exec.Cmd
	exited    chan struct{}
	startedAt time.Time
}

// ── profile 目录 ─────────────────────────────────────────────────────────

// codeartsProfileRoot 浏览器 profile 根目录（config.json 同级的 data/ 下）。
func codeartsProfileRoot(p *Panel) string {
	return filepath.Join(traeBrowserBaseDir(p), "codearts-web")
}

// codeartsSlotDir 某个账号的 profile 目录。
func codeartsSlotDir(p *Panel, slot string) string {
	if slot == "" {
		return ""
	}
	dir := filepath.Join(codeartsProfileRoot(p), slot)
	if abs, err := filepath.Abs(dir); err == nil {
		dir = abs
	}
	return dir
}

// codeartsPendingDir 新登录用的临时 profile 目录（登录成功后归档到账号名下）。
func codeartsPendingDir(p *Panel) string {
	dir := filepath.Join(codeartsProfileRoot(p), fmt.Sprintf("pending-%d", time.Now().UnixNano()))
	if abs, err := filepath.Abs(dir); err == nil {
		dir = abs
	}
	return dir
}

// codeartsProfileUsable 该账号的 profile 目录是否还在（会话还在里面）。
func codeartsProfileUsable(p *Panel, acc *codearts.Account) bool {
	if acc == nil {
		return false
	}
	slot := acc.Slot
	if slot == "" {
		slot = acc.ID // 老账号：没有 Slot 就按 ID 找
	}
	dir := codeartsSlotDir(p, slot)
	if dir == "" {
		return false
	}
	st, err := os.Stat(dir)
	return err == nil && st.IsDir()
}

// ── 浏览器 ──────────────────────────────────────────────────────────────

func (p *Panel) codeartsBrowserStart(w http.ResponseWriter, r *http.Request) {
	codeartsBrowserSession.mu.Lock()
	defer codeartsBrowserSession.mu.Unlock()
	if codeartsBrowserSession.cmd != nil {
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "running": true,
			"message": "浏览器已在运行，请在窗口里登录华为云账号"})
		return
	}
	chrome, err := findChrome()
	if err != nil {
		writeErr(w, http.StatusBadGateway, err.Error())
		return
	}
	port, err := freePort(traeBrowserPorts)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	// 每次「添加账号」用**全新**的 profile 目录：不同账号必须彼此隔离
	// （共用 profile 会串号：后登录的账号覆盖前一个）。
	dir := codeartsPendingDir(p)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	cmd := exec.Command(chrome,
		fmt.Sprintf("--remote-debugging-port=%d", port),
		"--user-data-dir="+dir,
		"--no-first-run", "--no-default-browser-check",
		codeartsConsoleURL,
	)
	cmd.Stdout, cmd.Stderr = io.Discard, io.Discard
	if err := cmd.Start(); err != nil {
		writeErr(w, http.StatusBadGateway, "启动浏览器失败: "+err.Error())
		return
	}
	exited := make(chan struct{})
	go func() { _ = cmd.Wait(); close(exited) }()
	codeartsBrowserSession.port = port
	codeartsBrowserSession.dir = dir
	codeartsBrowserSession.cmd = cmd
	codeartsBrowserSession.exited = exited
	codeartsBrowserSession.startedAt = time.Now()
	log.Printf("[codearts] 浏览器已启动 (port %d, profile %s)，等待登录", port, dir)

	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "running": true,
		"message": "浏览器已打开，请登录华为云账号；登录成功后会自动保存"})
}

func (p *Panel) codeartsBrowserStop(w http.ResponseWriter, r *http.Request) {
	codeartsBrowserSession.mu.Lock()
	defer codeartsBrowserSession.mu.Unlock()
	codeartsStopBrowserLocked(true)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// codeartsStopBrowserLocked 关掉登录窗口。
//
// keepProfile=false 时连 profile 目录一起删（超时未登录的临时目录要清掉）；
// keepProfile=true 时保留目录 —— 登录成功的 profile 要归档给账号长期用。
func codeartsStopBrowserLocked(keepProfile bool) {
	if codeartsBrowserSession.cmd != nil {
		_ = codeartsBrowserSession.cmd.Process.Kill()
		// 等进程真正退出，否则目录还被 Chrome 占着，搬不动。
		if codeartsBrowserSession.exited != nil {
			select {
			case <-codeartsBrowserSession.exited:
			case <-time.After(8 * time.Second):
			}
		}
		codeartsBrowserSession.cmd = nil
		codeartsBrowserSession.exited = nil
	}
	if !keepProfile && codeartsBrowserSession.dir != "" {
		_ = os.RemoveAll(codeartsBrowserSession.dir)
	}
	codeartsBrowserSession.dir = ""
	codeartsBrowserSession.port = 0
}

// codeartsBrowserPoll 检测登录并把账号写入号池。
func (p *Panel) codeartsBrowserPoll(w http.ResponseWriter, r *http.Request) {
	codeartsBrowserSession.mu.Lock()
	port := codeartsBrowserSession.port
	dir := codeartsBrowserSession.dir
	started := codeartsBrowserSession.startedAt
	codeartsBrowserSession.mu.Unlock()
	if port == 0 {
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "running": false,
			"message": "浏览器未启动"})
		return
	}
	if time.Since(started) > codeartsTTL {
		codeartsBrowserSession.mu.Lock()
		codeartsStopBrowserLocked(false)
		codeartsBrowserSession.mu.Unlock()
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "running": false,
			"message": "浏览器超时已关闭，请重新开始"})
		return
	}

	all, err := cdpAllCookies(port)
	if err != nil {
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "running": true,
			"message": "浏览器已开，等待登录（" + err.Error() + "）"})
		return
	}
	cookies := codeartsCookiesOf(all)
	// 光看 Cookie 名判断不了登录：未登录时也会有 SessionID / HWWAFSESID 等。
	// 用**页面可见文案**判定：未登录时右上角是「登录华为云」，登录后这个按钮
	// 消失、换成「活动福利 / 新建任务 / 工作台」等模块（实测页面文案）。
	text, nick, logged := codeartsLoginProbe(port)
	if !logged {
		msg := "还没检测到华为云登录态，请在浏览器窗口里完成登录"
		if strings.Contains(text, "登录华为云") {
			msg = "浏览器还停在登录页，请完成登录后自动保存"
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"ok": true, "running": true, "cookies": len(cookies),
			"sample": trimText(text, 200), "message": msg,
		})
		return
	}
	acc, added, err := p.codeartsSaveLogin(cookies, nick, dir, port)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	codeartsBrowserSession.mu.Lock()
	codeartsStopBrowserLocked(true) // profile 目录要留给账号用
	codeartsBrowserSession.mu.Unlock()

	msg := "已保存：" + acc.Label
	if !added {
		msg = "已更新：" + acc.Label
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "running": false,
		"added": added, "id": acc.ID, "label": acc.Label, "message": msg})
}

// codeartsSaveLogin 把一次登录结果落库，并归档 profile 目录。
//
// 归档逻辑：登录用的是临时目录（pending-*），登录成功后：
//   - 号池里已有同一账号（按稳定账号 ID 认人）→ 把临时目录搬到该账号的
//     slot 目录（覆盖旧的），账号 ID / 标签保持不变；
//   - 没有 → 新建账号，slot = 新分配的目录名，临时目录直接改名为 slot 目录。
func (p *Panel) codeartsSaveLogin(cookies []codearts.Cookie, nick, pendingDir string, port int) (*codearts.Account, bool, error) {
	pool, err := codearts.LoadPool()
	if err != nil {
		return nil, false, err
	}
	fp := codearts.Fingerprint(cookies)
	domainID := codearts.DomainIDOf(cookies)

	// 找同一个人已有的账号。
	//
	// 两个判据都要看：
	//   1. ID 相同（新算法：华为云稳定账号 ID 派生的指纹）；
	//   2. 华为云账号 ID 相同 —— 老号池里的账号是**旧算法**（拿会话 Cookie
	//      派生的指纹）算出来的，ID 对不上，但它们存的 Cookie 里仍有
	//      domain_tag，能认出是同一个人。不做这一步，老号会变成"多出来一个
	//      永远需要重新登录的僵尸账号"。
	did := codearts.DomainIDOf(cookies)
	var existing *codearts.Account
	for _, a := range pool.Accounts {
		if a == nil {
			continue
		}
		if a.ID == fp {
			existing = a
			break
		}
		if did != "" && a.DomainID == did {
			existing = a
			break
		}
		if did != "" && codearts.DomainIDOf(a.Cookies) == did {
			existing = a
			break
		}
	}
	slot := fp
	if existing != nil {
		slot = existing.Slot
		if slot == "" {
			slot = existing.ID
		}
		// 老账号的 ID 是旧算法算的：沿用它（面板上显示的标识不变），
		// 但指纹要用新的（认人才准）。
		fp = existing.ID
	}

	// 归档 profile：先关着的浏览器已经退出（调用方保证），再搬目录。
	target := codeartsSlotDir(p, slot)
	if target != "" && pendingDir != "" {
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			return nil, false, err
		}
		if err := os.RemoveAll(target); err != nil {
			return nil, false, fmt.Errorf("清理旧 profile 失败: %w", err)
		}
		if err := os.Rename(pendingDir, target); err != nil {
			// 跨卷/占用失败就退回复制，保证登录态不丢。
			if cpErr := copyDir(pendingDir, target); cpErr != nil {
				return nil, false, fmt.Errorf("归档浏览器 profile 失败: %w", err)
			}
		}
		log.Printf("[codearts] 账号 profile 已归档到 %s", target)
	}

	acc := &codearts.Account{
		ID: fp, Cookies: cookies, SavedAt: codearts.Now(),
		Nickname: nick, DomainID: domainID, Slot: slot, Port: port,
	}
	if nick != "" {
		acc.Label = nick
	}
	saved, added := codearts.Upsert(pool, acc)
	if err := codearts.SavePool(pool); err != nil {
		return nil, false, err
	}
	return saved, added, nil
}

// copyDir 递归复制目录（profile 归档跨卷时的退路）。
func copyDir(src, dst string) error {
	return filepath.Walk(src, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(src, path)
		if err != nil {
			return err
		}
		target := filepath.Join(dst, rel)
		if info.IsDir() {
			return os.MkdirAll(target, 0o755)
		}
		buf, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		return os.WriteFile(target, buf, info.Mode())
	})
}

// codeartsCookiesOf 只取 huaweicloud 域下的 Cookie。
func codeartsCookiesOf(all []cdpCookie) []codearts.Cookie {
	var out []codearts.Cookie
	for _, c := range all {
		if c.Name == "" || c.Value == "" {
			continue
		}
		d := strings.ToLower(c.Domain)
		if !strings.Contains(d, "huaweicloud") && !strings.Contains(d, "huawei") {
			continue
		}
		out = append(out, codearts.Cookie{Name: c.Name, Value: c.Value, Domain: c.Domain, Path: c.Path})
	}
	return out
}

// codeartsLoginProbe 读**已打开**的页面，判定是否已登录。
//
// ⚠️ 绝不新建标签页：登录窗口就是用户正在操作的那一个，轮询时每次新开一个
// 标签，用户那边看到的现象就是「窗口一直刷新」，而且会攒出一堆标签页
// （实测踩过：4 秒一轮询 → 无限刷新）。这里只读**已有**的页面。
//
// 华为云登录页常以独立窗口/标签弹出（用户点「登录」后出现二维码或手机号
// 登录），登录成功后会跳回 /chat/… 并带上会话。所以这里把**所有**页面都
// 看一遍：任一页面出现登录后的强信号（没有「登录华为云」按钮、且有
// 「活动福利 / 新建任务 / 工作台」等模块）即算已登录。
func codeartsLoginProbe(port int) (text, nickname string, logged bool) {
	pages, err := caListPages(port)
	if err != nil || len(pages) == 0 {
		return "", "", false
	}
	var firstText, firstNick string
	for _, p := range pages {
		ws, derr := wsDial(p.WS, 6*time.Second)
		if derr != nil {
			continue
		}
		raw, eerr := caEval(ws, codeartsProbeJS)
		_ = ws.Close()
		if eerr != nil {
			continue // 页面正在跳转（登录中）——这一轮跳过，下一轮再读
		}
		var r struct {
			Text     string `json:"text"`
			Nickname string `json:"nickname"`
			Logged   bool   `json:"logged"`
		}
		if json.Unmarshal(raw, &r) != nil {
			continue
		}
		if r.Logged {
			return r.Text, r.Nickname, true
		}
		if firstText == "" {
			firstText, firstNick = r.Text, r.Nickname
		}
	}
	return firstText, firstNick, false
}

// codeartsProbeJS 读页面文案 + 判登录。
//
// 判据（实测页面文案）：未登录时右上角有「登录华为云」按钮；登录后这个按钮
// 消失，出现「活动福利 / 新建任务 / 工作台 / 退出登录」等模块。
const codeartsProbeJS = `(() => {
  const norm = e => ((e.innerText || e.textContent || '')).replace(/\s+/g, ' ').trim();
  const vis = e => { const r = e.getBoundingClientRect(); return r.width > 0 && r.height > 0; };
  const text = ((document.body && document.body.innerText) || '').replace(/\s+/g, ' ').slice(0, 2000);
  const els = [...document.querySelectorAll('a,button,div,span,li,p')];
  // 精确等于「登录华为云」的可点元素 = 未登录的强信号
  let hasLoginBtn = false;
  for (const e of els) {
    const t = norm(e);
    if ((t === '登录华为云' || t === '登录' || t === '登录/注册') && vis(e)) { hasLoginBtn = true; break; }
  }
  const logged = !hasLoginBtn && (
    text.includes('活动福利') || text.includes('新建任务') || text.includes('工作台') ||
    text.includes('退出登录') || text.includes('我的积分') || text.includes('积分余额'));
  // 昵称：优先「您好，xxx」
  let nickname = '';
  const i = text.indexOf('您好');
  if (i >= 0) {
    let seg = text.slice(i + 2).replace(/^[\s，,、]+/, '');
    const m = seg.match(/^[^\s，,。；;（(]{1,24}/);
    if (m) nickname = m[0];
  }
  return JSON.stringify({ text, nickname, logged });
})()`

// ── 签到 ────────────────────────────────────────────────────────────────

// codeartsCheckin 用账号的浏览器 profile 打开控制台，点「活动福利 → 每日领取」。
func (p *Panel) codeartsCheckin(w http.ResponseWriter, r *http.Request) {
	var req struct {
		IDs []string `json:"ids"`
	}
	_ = json.NewDecoder(r.Body).Decode(&req)
	pool, err := codearts.LoadPool()
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	want := map[string]bool{}
	for _, id := range req.IDs {
		if id = strings.TrimSpace(id); id != "" {
			want[id] = true
		}
	}

	type res struct {
		ID     string `json:"id"`
		Label  string `json:"label"`
		Phase  string `json:"phase"`
		Msg    string `json:"msg"`
		Sample string `json:"sample,omitempty"`
	}
	results := make([]res, 0, len(pool.Accounts))
	claimed, already, failed := 0, 0, 0
	for _, acc := range pool.Accounts {
		if acc == nil || len(acc.Cookies) == 0 {
			continue
		}
		if len(want) > 0 && !want[acc.ID] {
			continue
		}
		out := res{ID: acc.ID, Label: acc.Label}
		phase, msg, sample, err := p.codeartsClaim(acc)
		if err != nil {
			out.Phase, out.Msg = "error", err.Error()
			failed++
		} else {
			out.Phase, out.Msg, out.Sample = phase, msg, sample
			switch phase {
			case "claimed":
				claimed++
			case "already":
				already++
			default:
				failed++
			}
		}
		if out.Phase == "claimed" || out.Phase == "already" {
			// 写本地签到记录，面板的「今日已签」才有依据。
			_ = ideauth.MarkCheckedIn("codearts", acc.ID)
		}
		results = append(results, out)
	}
	msg := fmt.Sprintf("本次签到 %d 个 · 今日已领 %d 个 · 失败 %d 个", claimed, already, failed)
	writeJSON(w, http.StatusOK, map[string]any{
		"ok": true, "results": results, "msg": msg,
		"claimed_accounts": claimed, "already_accounts": already, "failed_accounts": failed,
	})
}

// codeartsClaim 用账号的持久 profile 拉无头浏览器，点「每日领取」。
func (p *Panel) codeartsClaim(acc *codearts.Account) (phase, msg, sample string, err error) {
	dir := codeartsSlotDir(p, acc.Slot)
	if dir == "" {
		dir = codeartsSlotDir(p, acc.ID)
	}
	if dir == "" {
		return "pending", "这个账号没有本地登录会话，请点右上角「添加账号」重新登录一次", "", nil
	}
	if st, serr := os.Stat(dir); serr != nil || !st.IsDir() {
		return "pending", "这个账号的浏览器登录会话已丢失，请点右上角「添加账号」重新登录一次", "", nil
	}

	chrome, err := findChrome()
	if err != nil {
		return "", "", "", err
	}
	port, err := freePort(traeBrowserPorts)
	if err != nil {
		return "", "", "", err
	}
	cmd := exec.Command(chrome,
		fmt.Sprintf("--remote-debugging-port=%d", port),
		"--user-data-dir="+dir,
		"--no-first-run", "--no-default-browser-check",
		"--headless=new", "--disable-gpu", "--window-size=1600,1000",
		codeartsConsoleURL)
	cmd.Stdout, cmd.Stderr = io.Discard, io.Discard
	if err := cmd.Start(); err != nil {
		return "", "", "", err
	}
	defer func() { _ = cmd.Process.Kill() }()

	var ws *wsConn
	for i := 0; i < 30; i++ {
		time.Sleep(500 * time.Millisecond)
		ws, err = caOpenTab(port, codeartsConsoleURL)
		if err == nil {
			break
		}
	}
	if ws == nil {
		return "", "", "", fmt.Errorf("连接浏览器失败: %w", err)
	}
	defer ws.Close()

	// 等 SPA 渲染完
	time.Sleep(8 * time.Second)

	// 先确认这个 profile 里的登录态还在：profile 里没有 Cookie 了就等于
	// 登录过期（华为云会话只有约一小时寿命）。
	raw, err := caEval(ws, codeartsProbeJS)
	if err != nil {
		return "", "", "", err
	}
	var pr struct {
		Text   string `json:"text"`
		Logged bool   `json:"logged"`
	}
	if json.Unmarshal(raw, &pr) != nil {
		return "", "", "", fmt.Errorf("读取页面状态失败")
	}
	if !pr.Logged {
		return "pending", "这个账号的华为云登录已过期，请点右上角「添加账号」重新登录一次",
			trimText(pr.Text, 300), nil
	}

	// ── 接口优先 ─────────────────────────────────────────────────────────
	// 契约见 codearts_api.go（从前端 bundle 实测反查，不是猜的）。
	// 在页面里发 fetch，用的是浏览器**当下**的活会话 —— 与前端自己发请求
	// 完全等价，比点按钮稳得多。接口走不通才退回下面的 UI 点按钮。
	if ph, msg, sample, ok := p.codeartsClaimViaAPI(ws); ok {
		return ph, msg, sample, nil
	}

	// ── 接口走不通 → 退回 UI 点按钮 ───────────────────────────────────
	// ① 打开福利面板（用**真实鼠标**点：React 应用合成 click 不触发展开）
	var steps []string
	opened := ""
	for _, kw := range []string{"活动福利", "限时福利", "福利中心", "码道有礼"} {
		if ok, tx, _ := caClickByText(ws, kw, 16); ok {
			opened = tx
			steps = append(steps, "open:"+tx)
			time.Sleep(3 * time.Second)
			break
		}
	}

	// 面板若显示「登录参与 / 立即登录 / 解锁更多福利」，说明这份 profile 的
	// 登录态已经失效（实测未登录时点开活动福利就是这个界面）。
	hints := caCollectHints(ws)
	if opened != "" && caPanelNeedsLogin(hints) {
		return "pending", "这个账号的华为云登录已过期，请点右上角「添加账号」重新登录一次",
			"steps=" + strings.Join(steps, ",") + " hints=" + strings.Join(hints, "|"), nil
	}

	// ② 点「每日领取」
	clicked := ""
	for _, kw := range []string{"每日领取", "立即签到", "去签到", "立即领取"} {
		if ok, tx, _ := caClickByText(ws, kw, 16); ok {
			clicked = tx
			steps = append(steps, "click:"+tx)
			break
		}
	}
	if clicked != "" {
		time.Sleep(4 * time.Second)
	}

	// ③ 顺手领已完成任务的奖励（**文案完全相等**才算，避免把「每日领取」
	//    当成任务按钮再点一遍；不碰「去完成」）
	tasks := 0
	for i := 0; i < 3; i++ {
		ok, _, err := caClickExactText(ws, []string{"领取", "立即领取", "去领取", "一键领取"})
		if err != nil || !ok {
			break
		}
		tasks++
		time.Sleep(1200 * time.Millisecond)
	}

	after, _ := caEvalExistingPage(port, "huaweicloud")
	if after == "" {
		after = pr.Text
	}
	hints = caCollectHints(ws)
	ph, m := classifyClaim(clicked != "" || tasks > 0, after, tasks)
	diag := "steps=" + strings.Join(steps, ",") + " hints=" + strings.Join(hints, "|")
	if opened == "" && clicked == "" {
		m = "没找到「活动福利 / 每日领取」入口（页面可能改版了），请点右上角「添加账号」重新登录并核对入口"
	}
	return ph, m, diag + " || " + trimText(after, 1600), nil
}

// caPanelNeedsLogin 判断活动福利面板是不是停在「未登录」状态。
//
// 实测（未登录时点开「活动福利」）：面板底部出现
// 「解锁更多福利 / 登录即可解锁更多限时福利。/ 立即登录」，
// 而「立即参与」会变成「登录参与」。登录后才解锁「每日领取」。
func caPanelNeedsLogin(hints []string) bool {
	joined := strings.Join(hints, "|")
	for _, s := range []string{"立即登录", "登录参与", "解锁更多福利"} {
		if strings.Contains(joined, s) {
			return true
		}
	}
	return false
}

// ── 号池接口 ────────────────────────────────────────────────────────────

func (p *Panel) codeartsAccounts(w http.ResponseWriter, r *http.Request) {
	pool, err := codearts.LoadPool()
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	out := make([]map[string]any, 0, len(pool.Accounts))
	for _, a := range pool.Accounts {
		if a == nil {
			continue
		}
		out = append(out, map[string]any{
			"id": a.ID, "label": a.Label, "nickname": a.Nickname,
			"saved_at": a.SavedAt, "cookie_count": len(a.Cookies),
			"has_session": codeartsProfileUsable(p, a),
			"checked_in_today": ideauth.CheckedInToday("codearts", a.ID),
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "accounts": out})
}

func (p *Panel) codeartsRemove(w http.ResponseWriter, r *http.Request) {
	var req struct {
		ID string `json:"id"`
	}
	_ = json.NewDecoder(r.Body).Decode(&req)
	pool, err := codearts.LoadPool()
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	var target *codearts.Account
	for _, a := range pool.Accounts {
		if a != nil && a.ID == req.ID {
			target = a
			break
		}
	}
	if target == nil {
		writeErr(w, http.StatusNotFound, "账号不存在")
		return
	}
	if !codearts.Remove(pool, req.ID) {
		writeErr(w, http.StatusNotFound, "账号不存在")
		return
	}
	if err := codearts.SavePool(pool); err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	// 连 profile 一起清掉（里面是登录态，属于敏感数据）。
	if slot := target.Slot; slot != "" {
		_ = os.RemoveAll(codeartsSlotDir(p, slot))
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// codeartsAddFromPort 从**已经在跑的**调试端口提取登录态。
// 用途：服务重启会丢浏览器句柄（窗口还在但面板连不上），用这个把账号救回来；
// 也方便高级用户直接指定端口。
func (p *Panel) codeartsAddFromPort(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Port int `json:"port"`
	}
	_ = json.NewDecoder(r.Body).Decode(&req)
	if req.Port <= 0 {
		writeErr(w, http.StatusBadRequest, "缺少 port")
		return
	}
	all, err := cdpAllCookies(req.Port)
	if err != nil {
		writeErr(w, http.StatusBadGateway, "连不上该端口的浏览器: "+err.Error())
		return
	}
	cookies := codeartsCookiesOf(all)
	if len(cookies) == 0 {
		writeErr(w, http.StatusBadRequest, "该浏览器里没有华为云 Cookie")
		return
	}
	text, nick, logged := codeartsLoginProbe(req.Port)
	if !logged {
		writeJSON(w, http.StatusOK, map[string]any{"ok": false, "logged_in": false,
			"cookies": len(cookies), "sample": trimText(text, 300),
			"message": "该页面看起来还没登录"})
		return
	}
	acc, added, err := p.codeartsSaveLogin(cookies, nick, "", req.Port)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	msg := "已保存：" + acc.Label
	if !added {
		msg = "已更新：" + acc.Label
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "logged_in": true,
		"added": added, "id": acc.ID, "label": acc.Label, "message": msg,
		"sample": trimText(text, 200)})
}

// classifyClaim 把页面结果翻译成结论。
func classifyClaim(ok bool, sample string, tasks int) (phase, msg string) {
	switch {
	case ok && strings.Contains(sample, "领取成功"):
		msg = "签到成功（+1000 积分）"
		phase = "claimed"
	// 注意文案差异：CodeArts 的签到卡片写的是「今日已领」（没有"取"字），
	// 豆包/其它平台才是「今日已领取」——只认一种会漏判成 unknown。
	case ok && (strings.Contains(sample, "今日已领") || strings.Contains(sample, "明天再来") ||
		strings.Contains(sample, "已签到") || strings.Contains(sample, "领取成功")):
		msg = "今日已签到"
		phase = "already"
	case ok:
		msg = "已点「每日领取」，请核对页面是否到账"
		phase = "unknown"
	default:
		msg = "没找到「每日领取」入口（可能要重新登录）"
		phase = "pending"
	}
	if tasks > 0 {
		msg += fmt.Sprintf("（另领 %d 个任务奖励）", tasks)
	}
	return
}

// trimText 截断文本用于回传诊断。
func trimText(s string, n int) string {
	if len([]rune(s)) <= n {
		return s
	}
	return string([]rune(s)[:n]) + "…"
}
