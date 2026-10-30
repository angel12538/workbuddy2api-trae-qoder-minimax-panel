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
	"crypto/sha1"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/codearts"
	"github.com/linguo2625469/workbuddy2api-panel/internal/ideauth"
)

const (
	codeartsConsoleURL = "https://devcloud.cn-north-4.huaweicloud.com/chat/"
	// codeartsWelfareURL 直接打开福利面板（客户端自身用的参数）。
	codeartsWelfareURL = "https://devcloud.cn-north-4.huaweicloud.com/chat/home?openWelfare=true"
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
		// 服务重启会丢掉内存里的会话，但浏览器窗口通常还开着：先按端口找回来。
		if ports := caDiscoverPorts(); len(ports) > 0 {
			port = ports[0]
			codeartsBrowserSession.mu.Lock()
			codeartsBrowserSession.port = port
			codeartsBrowserSession.startedAt = time.Now()
			codeartsBrowserSession.mu.Unlock()
			// 局部 started 也要跟上，否则下面的 TTL 检查用的还是 0 → 误判超时并关窗口。
			started = time.Now()
			log.Printf("[codearts] 自动发现已登录窗口：port %d", port)
		}
	}
	if port == 0 {
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "running": false,
			"message": "没找到浏览器窗口：点「添加账号」重新打开"})
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
	// **关掉窗口**：登录已经完成，会话凭据存在号池里（Cookie 快照），
	// 之后靠 keepalive 续着，不必留一个窗口给用户看（留着的唯一好处是
	// 用户手动点点，代价是窗口一直开着）。
	acc, added, err := p.codeartsSaveLogin(cookies, nick, dir, port, text)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	codeartsBrowserSession.mu.Lock()
	codeartsStopBrowserLocked(false) // profile 不再归档，临时目录直接清掉
	codeartsBrowserSession.mu.Unlock()

	msg := "已保存：" + acc.Label
	if !added {
		msg = "已更新：" + acc.Label
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "running": false,
		"added": added, "id": acc.ID, "label": acc.Label, "message": msg})
}

// codeartsSaveLogin 把一次登录结果落库。
//
// 认人（这是「能不能加多个号」的关键）：
//   - 首选 Identity = domain_tag|user_tag，华为云账号本身，跨登录不变；
//   - 拿不到就用页面上的稳定账号标识（hid__xxx）；
//   - 都没有才退回指纹（会话级，精度差，只作最后手段）。
//
// 同一个 Identity 再次登录 → **更新原记录**（保留原 ID，保住签到历史），
// 不再新增一条"看起来像另一个号"的僵尸记录。
//
// 关于 profile 归档：2026-10-30 实测证明**归档下来的 profile 恢复不了登录态**
// （浏览器一关，domain_tag / user_tag 这些会话 Cookie 一起消失，用归档目录
// 打开控制台照样显示「登录华为云」）。所以不再归档——它只占磁盘（实测残留
// 1.5GB）却不带来任何免登录能力。签到主路径是纯 HTTP。
func (p *Panel) codeartsSaveLogin(cookies []codearts.Cookie, nick, pendingDir string, port int, pageText string) (*codearts.Account, bool, error) {
	pool, err := codearts.LoadPool()
	if err != nil {
		return nil, false, err
	}
	// 先把历史上的重复记录合并掉（同一个人被拆成多条时用）。
	if merged := codearts.MergePool(pool); merged > 0 {
		log.Printf("[codearts] 号池合并：%d 条重复记录已并入同一个人", merged)
	}

	identity := codearts.IdentityOf(cookies)
	if identity == "" {
		// 退一步：页面上的稳定账号标识。
		if k := codearts.AccountKeyFromText(pageText); k != "" {
			identity = "hid:" + k
		}
	}
	fp := codearts.Fingerprint(cookies)

	// 找同一个人已有的账号。
	var existing *codearts.Account
	for _, a := range pool.Accounts {
		if a == nil {
			continue
		}
		if identity != "" && a.Identity != "" && a.Identity == identity {
			existing = a
			break
		}
	}
	if existing == nil && identity != "" {
		// 老号池没有 Identity 字段，用现存的 Cookie 反算一次再比。
		for _, a := range pool.Accounts {
			if a == nil {
				continue
			}
			if codearts.IdentityOf(a.Cookies) == identity {
				existing = a
				break
			}
		}
	}

	accID := fp
	slot := fp
	if existing != nil {
		// 同一个人：沿用原 ID 与 slot（签到记录按 ID 存，不能换）。
		accID = existing.ID
		slot = existing.Slot
		if slot == "" {
			slot = existing.ID
		}
	}
	// 新账号的 ID 不能和别人的撞：撞了就用 identity 派生一个。
	if existing == nil {
		taken := false
		for _, a := range pool.Accounts {
			if a != nil && a.ID == accID {
				taken = true
				break
			}
		}
		if taken {
			accID = codearts.Fingerprint([]codearts.Cookie{{Name: "identity", Value: identity}})
			if accID == fp {
				accID = accID + "b"
			}
			slot = accID
		}
	}

	acc := &codearts.Account{
		ID: accID, Identity: identity, Cookies: cookies, SavedAt: codearts.Now(),
		Nickname: nick, DomainID: codearts.DomainIDOf(cookies), Slot: slot, Port: port,
	}
	if nick != "" {
		acc.Label = nick
	}
	saved, added := codearts.Upsert(pool, acc)
	// Upsert 里命中的老账号要补上 Identity（老号池没这个字段）。
	if saved.Identity == "" && identity != "" {
		saved.Identity = identity
	}
	if err := codearts.SavePool(pool); err != nil {
		return nil, false, err
	}
	log.Printf("[codearts] 账号已保存: id=%s identity=%s slot=%s added=%v", saved.ID, identity, saved.Slot, added)
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

// codeartsCheckin 给一批 CodeArts 账号签到。
//
// 主路径是**纯 HTTP**（Cookie + 接口），不依赖任何浏览器窗口：
//   查活动列表 → 找到每日签到活动 → 没领就领（带幂等键）→ 刷新积分。
// 浏览器只在这两种情况下才登场：① 登录窗口还开着（人就在旁边）；② 兜底提示。
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
		// 主路径：纯 HTTP（不依赖窗口）；窗口只在 HTTP 不可用时兜底。
		if ph, m, okHTTP := codeartsCheckinHTTP(acc); okHTTP {
			out.Phase, out.Msg = ph, m+"（HTTP）"
			if ph == "claimed" {
				claimed++
			} else {
				already++
			}
			_ = ideauth.MarkCheckedIn("codearts", acc.ID)
			results = append(results, out)
			continue
		}
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

// codeartsClaim 优先复用「还开着的登录窗口」；窗口没了才退回 profile。
func (p *Panel) codeartsClaim(acc *codearts.Account) (phase, msg, sample string, err error) {
	// ① 登录窗口还在：直接用真实鼠标点（窗口里就是活的会话）。
	if acc.Port > 0 {
		if ws, werr := caConnectPage(acc.Port, "chat"); werr == nil {
			defer ws.Close()
			var steps []string
			// 直接用带参 URL 打开福利面板：「活动福利」在侧边栏是图标项，
			// 没有可点击文本，按文案点经常点不到（实测 steps 为空）。
			// 客户端自己用的就是 ?openWelfare=true 这个参数。
			if err := caNavigate(ws, codeartsWelfareURL); err == nil {
				steps = append(steps, "nav:welfare")
				time.Sleep(5 * time.Second)
			}
			for _, kw := range []string{"活动福利", "限时福利", "福利中心"} {
				if ok, tx, _ := caClickByText(ws, kw, 16); ok {
					steps = append(steps, "open:"+tx)
					time.Sleep(3 * time.Second)
					break
				}
			}
			clicked := ""
			for _, kw := range []string{"每日领取", "立即签到", "立即领取"} {
				if ok, tx, _ := caClickByText(ws, kw, 16); ok {
					clicked = tx
					steps = append(steps, "click:"+tx)
					break
				}
			}
			time.Sleep(4 * time.Second)
			tasks := 0
			for i := 0; i < 3; i++ {
				if ok, _, _ := caClickByText(ws, "领取", 6); ok {
					tasks++
					time.Sleep(1200 * time.Millisecond)
				} else {
					break
				}
			}
			after, _ := caEvalExistingPage(acc.Port, "chat")
			ph, m := classifyClaim(clicked != "" || tasks > 0, after, tasks)
			return ph, m + "（登录窗口）", "steps=" + strings.Join(steps, ",") + " || " + trimText(after, 600), nil
		}
	}

	// ② 登录窗口不在了，本地 profile 也救不回来。
	//
	// 实测（2026-10-30）：归档下来的 profile **恢复不了登录态** —— 浏览器
	// 一关，domain_tag / user_tag 这些会话 Cookie 就没了，用归档目录打开
	// 控制台照样显示「登录华为云」（探针实测：SessionID 与 cftk 还在，但
	// 页面仍判未登录）。所以这条退路是死的：以前每次失败都要白白拉起一个
	// Chrome 等 15 秒，最后还是失败。
	//
	// 现在唯一的凭据是号池里的 Cookie 快照，靠 keepalive 续命；它一旦失效
	// 就只能重新登录（右上角「添加账号」）。
	return "pending", "这个账号的华为云会话已失效，请点右上角「添加账号」重新登录一次", "", nil
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
	out := make([]map[string]any, len(pool.Accounts))
	// 每个账号并发查（串行的话 N 个账号 × 2 个接口 × 8 秒超时，面板会卡死）。
	var wg sync.WaitGroup
	for i, a := range pool.Accounts {
		if a == nil {
			continue
		}
		wg.Add(1)
		go func(i int, a *codearts.Account) {
			defer wg.Done()
			out[i] = p.codeartsAccountItem(a)
		}(i, a)
	}
	wg.Wait()
	items := make([]map[string]any, 0, len(out))
	for _, it := range out {
		if it != nil {
			items = append(items, it)
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "accounts": items})
}

// codeartsAccountItem 组装单个账号在面板上要显示的信息。
//
// 「每天能领多少积分」= 每日签到活动的 benefitAmount（实测 100）；
// 「一共还剩多少积分」= package/overview 的 package_credit_remain（实测 5600）。
//
// login_expired 的判据是**接口真的查不通**（会话失效），而不是"本地有没有
// profile"——实测归档 profile 恢复不了登录态，拿它判活会把好号误判成僵尸。
func (p *Panel) codeartsAccountItem(a *codearts.Account) map[string]any {
	item := map[string]any{
		"id": a.ID, "label": a.Label, "nickname": a.Nickname,
		"saved_at": a.SavedAt, "cookie_count": len(a.Cookies),
		"checked_in_today": codeartsCheckedInToday(a),
	}
	if len(a.Aliases) > 0 {
		item["aliases"] = a.Aliases
	}

	// 每日能领多少积分 + 当前余额。
	daily := a.DailyBonus
	expired := false
	if camps, cerr := codearts.Campaigns(a); cerr == nil {
		if c, ok := codearts.DailyCampaign(camps); ok {
			daily = float64(c.BenefitAmount)
			a.DailyBonus = daily
			item["daily_claimed"] = c.Claimed() || !c.Claimable
		}
	} else if errors.Is(cerr, codearts.ErrSessionExpired) {
		expired = true
	}
	if daily > 0 {
		item["daily_bonus"] = daily
	}

	if ci, cerr := codearts.Credits(a); cerr == nil && ci != nil {
		a.CreditsRemain, a.CreditsTotal = ci.Remain, ci.Total
		a.CreditsUsed, a.CreditsExpiring = ci.Used, ci.Expiring
		item["credits"] = ci.Remain
		item["credits_total"] = ci.Total
		item["credits_used"] = ci.Used
		item["credits_expiring"] = ci.Expiring
	} else {
		// 接口查不通 = 会话没了。用上次快照兜底，但要标出来。
		if a.CreditsRemain > 0 {
			item["credits"] = a.CreditsRemain
			item["credits_total"] = a.CreditsTotal
			item["credits_used"] = a.CreditsUsed
			item["credits_expiring"] = a.CreditsExpiring
		}
		expired = true
	}
	item["login_expired"] = expired
	if a.LastGain > 0 {
		item["today_gain"] = a.LastGain
		item["today_gain_at"] = a.LastGainAt
	}
	// 存回快照（下次接口不通时还有底）。
	if pool, perr := codearts.LoadPool(); perr == nil {
		for _, x := range pool.Accounts {
			if x != nil && x.ID == a.ID {
				x.CreditsRemain, x.CreditsTotal = a.CreditsRemain, a.CreditsTotal
				x.CreditsUsed, x.CreditsExpiring = a.CreditsUsed, a.CreditsExpiring
				x.DailyBonus = a.DailyBonus
				_ = codearts.SavePool(pool)
				break
			}
		}
	}
	return item
}

// codeartsCheckedInToday 今天签没签（认全部历史别名）。
//
// 号池合并会把同一个人的多条记录并成一条，被并掉的 ID 记在 Aliases 里；
// 签到记录是按 ID 存的，不认别名的话，合并后就会把"今天签过"显示成没签。
func codeartsCheckedInToday(a *codearts.Account) bool {
	if a == nil {
		return false
	}
	for _, id := range a.AllIDs() {
		if ideauth.CheckedInToday("codearts", id) {
			return true
		}
	}
	return false
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
	acc, added, err := p.codeartsSaveLogin(cookies, nick, "", req.Port, text)
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
	// **页面显示「今日已领」就是已签**，与"有没有点到按钮"无关：
	// 签过之后按钮会变成灰色「今日已领」，点不到属于正常，之前这里直接判 pending，
	// 用户看到的就是"明明签了却说找不到入口"。
	case strings.Contains(sample, "今日已领") || strings.Contains(sample, "今日已领取") ||
		strings.Contains(sample, "明天再来") || strings.Contains(sample, "已签到"):
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

// codeartsAPIsProbe 在登录窗口里读 performance 资源，找出后端 API 路径。
//
// 目的：换成「Cookie + HTTP 直连」的方案，就先把页面自己调过的接口抓出来
//（积分/签到/福利相关），之后不依赖窗口也能调。
func codeartsAPIsProbe(port int) ([]string, error) {
	ws, err := caConnectPage(port, "huaweicloud")
	if err != nil {
		return nil, err
	}
	defer ws.Close()
	raw, err := caEval(ws, `(() => {
      const rs = performance.getEntriesByType('resource').map(e => e.name);
      const hits = rs.filter(u => /huaweicloud/.test(u) &&
        /(point|credit|welfare|sign|checkin|benefit|activity|campaign|task|user)/i.test(u));
      return JSON.stringify({ total: rs.length, hits: [...new Set(hits)].slice(0, 60) });
    })()`)
	if err != nil {
		return nil, err
	}
	var r struct {
		Total int      `json:"total"`
		Hits  []string `json:"hits"`
	}
	if err := json.Unmarshal(raw, &r); err != nil {
		return nil, err
	}
	return r.Hits, nil
}

// codeartsAPIProbe 面板诊断接口：GET /panel/api/ide/codearts/api_probe?port=9334
func (p *Panel) codeartsAPIProbe(w http.ResponseWriter, r *http.Request) {
	port := 0
	fmt.Sscanf(r.URL.Query().Get("port"), "%d", &port)
	if port <= 0 {
		writeErr(w, http.StatusBadRequest, "缺少 port")
		return
	}
	hits, err := codeartsAPIsProbe(port)
	if err != nil {
		writeErr(w, http.StatusBadGateway, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "count": len(hits), "apis": hits})
}

// codeartsAPITest 诊断：在登录窗口的页面上下文里直接 fetch 接口，返回原始响应。
// 用途：拿到 balance / claim 的真实返回结构（HTTP 直连缺网关 channel 头，
// 页面内 fetch 天然带全，先以它为准，再逐步收敛成纯 HTTP）。
func (p *Panel) codeartsAPITest(w http.ResponseWriter, r *http.Request) {
	port := 0
	fmt.Sscanf(r.URL.Query().Get("port"), "%d", &port)
	path := r.URL.Query().Get("path")
	if port <= 0 || path == "" {
		writeErr(w, http.StatusBadRequest, "需要 port 与 path")
		return
	}
	ws, err := caConnectPage(port, "huaweicloud")
	if err != nil {
		writeErr(w, http.StatusBadGateway, err.Error())
		return
	}
	defer ws.Close()
	expr := fmt.Sprintf(`(async () => {
      try {
        const r = await fetch(%q, {credentials: 'include', headers: {'X-Requested-With': 'XMLHttpRequest', 'Accept': 'application/json, text/plain, */*'}});
        const t = await r.text();
        return JSON.stringify({status: r.status, body: t.slice(0, 1200)});
      } catch (e) { return JSON.stringify({status: -1, body: String(e)}); }
    })()`, path)
	raw, err := caEval(ws, expr)
	if err != nil {
		writeErr(w, http.StatusBadGateway, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "raw": string(raw)})
}

// codeartsSniffHeaders 监听页面真实请求头（Network 域），用于补齐网关要求的
// channel / group 等自定义头。做法：enable → reload → 收集 45 秒。
func (p *Panel) codeartsSniffHeaders(w http.ResponseWriter, r *http.Request) {
	port := 0
	fmt.Sscanf(r.URL.Query().Get("port"), "%d", &port)
	if port <= 0 {
		writeErr(w, http.StatusBadRequest, "需要 port")
		return
	}
	ws, err := caConnectPage(port, "huaweicloud")
	if err != nil {
		writeErr(w, http.StatusBadGateway, err.Error())
		return
	}
	defer ws.Close()
	// 开启 Network 并刷新页面，让页面重新发一遍接口请求
	for _, m := range []map[string]any{{"id": 101, "method": "Network.enable", "params": map[string]any{}}} {
		b, _ := json.Marshal(m)
		_ = ws.writeText(b)
	}
	if b, e := json.Marshal(map[string]any{"id": 102, "method": "Page.reload", "params": map[string]any{}}); e == nil {
		_ = ws.writeText(b)
	}

	type hit struct {
		URL     string            `json:"url"`
		Headers map[string]string `json:"headers"`
	}
	var hits []hit
	seen := map[string]bool{}
	deadline := time.Now().Add(45 * time.Second)
	for time.Now().Before(deadline) {
		msg, rerr := ws.readMessage(6 * time.Second)
		if rerr != nil {
			break
		}
		var ev struct {
			Method string `json:"method"`
			Params struct {
				Request struct {
					URL     string            `json:"url"`
					Headers map[string]string `json:"headers"`
				} `json:"request"`
			} `json:"params"`
		}
		if json.Unmarshal(msg, &ev) != nil || ev.Method != "Network.requestWillBeSent" {
			continue
		}
		u := ev.Params.Request.URL
		// 抓 /chat/ 下**所有**接口：之前只过滤 benefit/cloudagent，漏掉了真正的
		// 积分查询（可能在 PromptCenter / points 等别的路径上）。
		if !strings.Contains(u, "/chat/") {
			continue
		}
		if strings.Contains(u, ".js") || strings.Contains(u, ".css") || strings.Contains(u, ".png") ||
			strings.Contains(u, ".svg") || strings.Contains(u, ".woff") || strings.Contains(u, ".ico") ||
			strings.Contains(u, ".json?") || strings.Contains(u, ".map") {
			continue
		}
		if seen[u] {
			continue
		}
		seen[u] = true
		hits = append(hits, hit{URL: u, Headers: ev.Params.Request.Headers})
		if len(hits) >= 30 {
			break
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "hits": hits})
}

// codeartsCheckinHTTP 纯 HTTP 完成签到（不依赖浏览器窗口）。
//
// 顺序：查活动列表 → 找每日签到活动 → 没领就领 → 顺带取积分余额。
// 这是主路径：窗口关着也能用，只要 Cookie 在服务端还没过期。
//
// 两个实测坑（都在这处理了）：
//  1. **到账有延迟**：领取返回成功后立刻查余额还是旧值（实测领取前后
//     同一次查询差值恒为 0，几秒后 overview 才变成 5600）。所以要**重试**
//     才能算出"今天实际加了多少"。
//  2. **必须带幂等键**：不带时当天第二次领取会返回 40001「并发冲突，请重试」。
func codeartsCheckinHTTP(acc *codearts.Account) (phase, msg string, ok bool) {
	camps, err := codearts.Campaigns(acc)
	if err != nil {
		return "", "", false
	}
	daily, found := codearts.DailyCampaign(camps)
	if !found {
		return "", "", false
	}
	// 签到前先记余额：签到后再查一次，两者之差就是"今天实际加了多少"，
	// 比直接读活动配置的 benefitAmount 可靠（可能有加成/上限）。
	before, _ := codearts.Credits(acc)

	persist := func() {
		if pool, perr := codearts.LoadPool(); perr == nil {
			for _, a := range pool.Accounts {
				if a != nil && a.ID == acc.ID {
					a.CreditsRemain, a.CreditsTotal = acc.CreditsRemain, acc.CreditsTotal
					a.CreditsUsed, a.CreditsExpiring = acc.CreditsUsed, acc.CreditsExpiring
					a.DailyBonus = acc.DailyBonus
					a.LastGain, a.LastGainAt = acc.LastGain, acc.LastGainAt
					_ = codearts.SavePool(pool)
					break
				}
			}
		}
	}

	acc.DailyBonus = float64(daily.BenefitAmount)

	// 情况一：活动状态显示已领（CLAIMED / CONFIRMED / CONSUME…）→ 今天签过了。
	if daily.Claimed() {
		codeartsRefreshCredits(acc)
		persist()
		return "already", fmt.Sprintf("今日已签到（余额 %.0f · 已用 %.0f）", acc.CreditsRemain, acc.CreditsUsed), true
	}
	// 情况二：服务端说不能再领（claimable=false 但没有明确状态）。
	if !daily.Claimable {
		codeartsRefreshCredits(acc)
		persist()
		return "already", fmt.Sprintf("今日已签到（余额 %.0f · 已用 %.0f）", acc.CreditsRemain, acc.CreditsUsed), true
	}

	// 情况三：还没领 → 领。
	day := time.Now().Format("20060102")
	key := codearts.DailyIdempotentKey(acc.ID, daily.CampaignID, day)
	r, cerr := codearts.ClaimWithKey(acc, daily.CampaignID, key)
	if cerr != nil {
		return "", "", false
	}
	if !r.OK {
		if r.Already() {
			codeartsRefreshCredits(acc)
			persist()
			return "already", fmt.Sprintf("今日已签到（余额 %.0f）", acc.CreditsRemain), true
		}
		return "", "", false
	}

	// 领取成功 → 等到账（实测不是瞬时的），再算真实增量。
	gain := float64(daily.BenefitAmount)
	if after, okAfter := codeartsCreditsAfter(acc, before); okAfter {
		gain = after.Remain - before.Remain
	}
	acc.LastGain, acc.LastGainAt = gain, codearts.Now()
	persist()
	return "claimed", fmt.Sprintf("签到成功 +%.0f 积分（余额 %.0f · 已用 %.0f）", gain, acc.CreditsRemain, acc.CreditsUsed), true
}

// codeartsRefreshCredits 刷新一次积分快照到账号上。
func codeartsRefreshCredits(acc *codearts.Account) bool {
	if ci, err := codearts.Credits(acc); err == nil && ci != nil {
		acc.CreditsRemain, acc.CreditsTotal = ci.Remain, ci.Total
		acc.CreditsUsed, acc.CreditsExpiring = ci.Used, ci.Expiring
		return true
	}
	return false
}

// codeartsCreditsAfter 领取后**重试**查余额，直到拿到新值或超时。
//
// 实测：领取接口返回成功后，余额在几秒内还是旧值 —— 立刻查差值恒为 0，
// 会把"今天加了多少"显示成 +0。这里最多等 ~6 秒。
func codeartsCreditsAfter(acc *codearts.Account, before *codearts.CreditInfo) (*codearts.CreditInfo, bool) {
	var last *codearts.CreditInfo
	for i := 0; i < 4; i++ {
		if i > 0 {
			time.Sleep(2 * time.Second)
		}
		ci, err := codearts.Credits(acc)
		if err != nil || ci == nil {
			continue
		}
		last = ci
		acc.CreditsRemain, acc.CreditsTotal = ci.Remain, ci.Total
		acc.CreditsUsed, acc.CreditsExpiring = ci.Used, ci.Expiring
		if before == nil || ci.Remain > before.Remain {
			return ci, true
		}
	}
	return last, last != nil
}

// codeartsRefresh 用账号自己的持久 profile 静默续期。
//
// 原理：华为云把 SessionID 存成会话级 Cookie（关浏览器就没），但 profile 里还有
// 持久的 cbc-sid / hwid_cas_sid。用同一 profile 打开页面，SSO 往往能凭它们
// 静默重新登录、下发新的 SessionID——这样就不必每次让用户手动登录。
func (p *Panel) codeartsRefresh(w http.ResponseWriter, r *http.Request) {
	var req struct {
		ID string `json:"id"`
	}
	_ = json.NewDecoder(r.Body).Decode(&req)
	pool, err := codearts.LoadPool()
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	var acc *codearts.Account
	for _, a := range pool.Accounts {
		if a != nil && (req.ID == "" || a.ID == req.ID) {
			acc = a
			break
		}
	}
	if acc == nil {
		writeErr(w, http.StatusNotFound, "账号不存在")
		return
	}
	// 刷新 = 打一次保活接口，把服务端续期下发的 Cookie 收下，并回报余额。
	//
	// ⚠️ 以前这里是用归档 profile 做「SSO 静默续期」，实测**不成立**：
	// 浏览器一关，domain_tag / user_tag 这些会话 Cookie 一起消失，用归档目录
	// 打开控制台照样显示「登录华为云」（2026-10-30 实测）。会话一旦过期，
	// 只能重新登录，没有别的办法。
	ok, kerr := codearts.Keepalive(acc)
	if !ok {
		msg := "会话已失效，请点右上角「添加账号」重新登录一次"
		if kerr != nil && !errors.Is(kerr, codearts.ErrSessionExpired) {
			msg = "刷新失败：" + kerr.Error()
		}
		writeJSON(w, http.StatusOK, map[string]any{"ok": false, "refreshed": false,
			"id": acc.ID, "message": msg})
		return
	}
	if err := codearts.SavePool(pool); err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	msg := "会话正常"
	if ci, cerr2 := codearts.Credits(acc); cerr2 == nil && ci != nil {
		acc.CreditsRemain, acc.CreditsTotal = ci.Remain, ci.Total
		acc.CreditsUsed, acc.CreditsExpiring = ci.Used, ci.Expiring
		msg = fmt.Sprintf("会话正常（剩余 %.0f 积分）", ci.Remain)
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "refreshed": true,
		"id": acc.ID, "message": msg})
}

// codeartsMigratePool 启动时整理号池：合并"同一个人被拆成多条"的记录、
// 给每个账号分配互不相同的 slot。
//
// 2026-10-30 实测的现场：号池里两条记录的 domain_tag / user_tag /
// masked_user 全同（就是同一个人），slot 也相同 —— 面板上看着像加了两个号，
// 其实是一个，还互相覆盖。这里一次性收敛掉。
func (p *Panel) codeartsMigratePool() {
	pool, err := codearts.LoadPool()
	if err != nil || pool == nil {
		return
	}
	merged := codearts.MergePool(pool)
	if merged == 0 {
		return
	}
	if err := codearts.SavePool(pool); err != nil {
		log.Printf("[codearts] 号池整理后写回失败: %v", err)
		return
	}
	log.Printf("[codearts] 号池整理：合并了 %d 条重复账号，现剩 %d 个", merged, len(pool.Accounts))
}

// codeartsKeepalive 后台保活：定时用 Cookie 调一次轻量接口。
//
// 华为云的会话是「空闲超时」型：长时间不用才失效。定时打一次能把有效期
// 拉到服务端允许的上限，并顺带吸收服务端续期下发的新 Cookie。
//
// 频率 15 分钟：实测服务端只回一个会话 Cookie（BENSESSCC_TAG），
// **不给 SessionID 续期**，所以保活不是"续签"，而是"别让它因闲置而过期"。
// 15 分钟足够把空闲计时器一直按在阈值以内。
//
// ⚠️ 这里必须比较 Cookie 的**值**而不是条数：服务端续期是**同名换值**
// （条数不变），只看条数的话新 Cookie 永远存不回号池，等于白保活。
func (p *Panel) codeartsKeepalive() {
	p.codeartsKeepaliveOnce()
	t := time.NewTicker(15 * time.Minute)
	defer t.Stop()
	for range t.C {
		p.codeartsKeepaliveOnce()
	}
}

// codeartsKeepaliveOnce 跑一轮保活。
func (p *Panel) codeartsKeepaliveOnce() {
	pool, err := codearts.LoadPool()
	if err != nil || len(pool.Accounts) == 0 {
		return
	}
	changed := false
	for _, a := range pool.Accounts {
		if a == nil || len(a.Cookies) == 0 {
			continue
		}
		before := codeartsCookieDigest(a.Cookies)
		ok, kerr := codearts.Keepalive(a)
		if kerr != nil {
			if errors.Is(kerr, codearts.ErrSessionExpired) {
				log.Printf("[codearts] 账号 %s 会话已失效，需要重新登录", a.ID)
			} else {
				log.Printf("[codearts] 账号 %s 保活失败: %v", a.ID, kerr)
			}
			continue
		}
		if !ok {
			continue
		}
		if codeartsCookieDigest(a.Cookies) != before {
			changed = true
			log.Printf("[codearts] 账号 %s 保活时收到续期 Cookie，已更新", a.ID)
		}
	}
	if changed {
		if err := codearts.SavePool(pool); err != nil {
			log.Printf("[codearts] 保活后写号池失败: %v", err)
		}
	}
}

// codeartsCookieDigest Cookie 集合的指纹（只看 name=value，条数相同也会不同）。
func codeartsCookieDigest(cs []codearts.Cookie) string {
	parts := make([]string, 0, len(cs))
	for _, c := range cs {
		parts = append(parts, c.Name+"="+c.Value)
	}
	sort.Strings(parts)
	sum := sha1.Sum([]byte(strings.Join(parts, ";")))
	return hex.EncodeToString(sum[:])
}
