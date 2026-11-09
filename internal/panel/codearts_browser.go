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
// 流程（2026-11-03 起改为「每号一个身份槽」，见 codearts_slot.go / §H）：
//  1. 「添加账号」→ 面板在槽根下的 pending 目录拉起可见浏览器，用户登录一次；
//     若指定了账号（重登），直接用**该号自己的槽** profile，保证是同一台设备；
//  2. 面板检测到登录（页面右上角不再有「登录华为云」）→ 提取 Cookie 认人
//     （用华为云稳定账号 ID domain_tag/user_tag，同一账号重复登录不新增条目）
//     → 关掉浏览器，**把登录产生的 profile 收编进该账号的身份槽**（设备身份载体）；
//  3. 补采/签到后的浏览器动作一律在该账号的槽内执行，不再新建临时 profile。
package panel

import (
	"bytes"
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
	// dirIsSlot 本次窗口用的是**账号的长期身份槽**（重登路径）。
	// 这种目录在任何关闭路径上都不能删：删了 = 该号下次动作变成"新设备"。
	dirIsSlot bool
	// accountID 重登时指定的账号 ID（"添加账号"时为空）。
	accountID string
}

// ── profile 目录 ─────────────────────────────────────────────────────────

// codeartsProfileRoot 历史 profile 根目录（config.json 同级的 data/ 下）。
//
// 2026-11-03 起新登录不再往这里写：每号身份槽在 <SlotRoot>/<SlotDir>/profile。
// 这个目录只保留给旧号池的 Slot 字段（历史数据）做只读兼容，见
// codeartsLegacyWebProfileDir。
func codeartsProfileRoot(p *Panel) string {
	return filepath.Join(traeBrowserBaseDir(p), "codearts-web")
}

// codeartsLegacyWebProfileDir 旧版（data/codearts-web/<slot>）的 profile 目录。
//
// 只用于兼容历史号池的 Slot 字段（如 slot=ca1d671920b4），新代码一律用
// (*Panel).codeartsSlotDir(账号)。
func codeartsLegacyWebProfileDir(p *Panel, slot string) string {
	if slot == "" {
		return ""
	}
	dir := filepath.Join(codeartsProfileRoot(p), slot)
	if abs, err := filepath.Abs(dir); err == nil {
		dir = abs
	}
	return dir
}

// codeartsPendingDir 登录窗口的临时 profile 目录（账号还没认出来时用）。
//
// 放在**槽根下**（<SlotRoot>/pending-<ts>），与 §H.3 的物理布局一致：
// 登录成功后整个目录会被 codeartsAdoptPendingProfile 收编进该号的槽，
// 成功即消失；残留的 pending-* 就是"登录没完成"，按 §H.6.1 属于可清理对象。
func codeartsPendingDir(p *Panel) string {
	dir := filepath.Join(codearts.SlotRoot(), fmt.Sprintf("pending-%d", time.Now().UnixNano()))
	if abs, err := filepath.Abs(dir); err == nil {
		dir = abs
	}
	return dir
}

// codeartsProfileUsable 该账号的身份槽 profile 是否还在。
func codeartsProfileUsable(p *Panel, acc *codearts.Account) bool {
	if acc == nil {
		return false
	}
	return acc.SlotExists()
}

// ── 浏览器 ──────────────────────────────────────────────────────────────

func (p *Panel) codeartsBrowserStart(w http.ResponseWriter, r *http.Request) {
	// 可选 body：{"id":"<accountID>"}。带 id = 重登某个已知号（复用它的槽 =
	// 同一台设备）；不带 = "添加账号"，账号此刻还不知道。
	var req struct {
		ID string `json:"id"`
	}
	if r.Body != nil {
		_ = json.NewDecoder(r.Body).Decode(&req)
	}

	codeartsBrowserSession.mu.Lock()
	defer codeartsBrowserSession.mu.Unlock()
	if codeartsBrowserSession.cmd != nil {
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "running": true,
			"message": "已有一个登录窗口在开着。请先在那个窗口里登录（登录成功会自动保存），" +
				"或者点「停止」关掉它再重新添加下一个账号"})
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
	// 目录选择是身份隔离的核心（§H.I2）：能确定账号就用该号的长期槽；
	// 确定不了才用 pending 占位，且登录成功后必须收编进槽。
	dir, inSlot, err := p.codeartsLoginProfileDir(req.ID)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	if !inSlot {
		if err := os.MkdirAll(dir, codeartsSlotDirMode); err != nil {
			writeErr(w, http.StatusInternalServerError, err.Error())
			return
		}
	}
	// 指纹/出口按账号槽派生：重登已知号必须是"同一台设备"的同一组启动参数，
	// 否则登录窗口这一侧就已经把设备身份换掉了。
	// 账号认不出来（添加账号）时 acc=nil，参数与引入指纹前逐字节一致。
	var acc *codearts.Account
	if id := strings.TrimSpace(req.ID); id != "" {
		if pool, perr := codearts.LoadPool(); perr == nil {
			acc = codeartsFindAccount(pool, id)
		}
	}
	args := codeartsChromeArgsForAccount(codeartsChromeArgs(port, dir, codeartsConsoleURL, false), acc)
	cmd := exec.Command(chrome, args...)
	cmd.Stdout, cmd.Stderr = io.Discard, io.Discard
	if err := cmd.Start(); err != nil {
		writeErr(w, http.StatusBadGateway, "启动浏览器失败: "+err.Error())
		return
	}
	exited := make(chan struct{})
	go func() { _ = cmd.Wait(); close(exited) }()
	codeartsBrowserSession.port = port
	codeartsBrowserSession.dir = dir
	codeartsBrowserSession.dirIsSlot = inSlot
	codeartsBrowserSession.accountID = strings.TrimSpace(req.ID)
	codeartsBrowserSession.cmd = cmd
	codeartsBrowserSession.exited = exited
	codeartsBrowserSession.startedAt = time.Now()
	log.Printf("[codearts] 浏览器已启动 (port %d, profile %s, in_slot=%v, account=%s)，等待登录",
		port, dir, inSlot, codeartsBrowserSession.accountID)

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
// keepProfile=false 时清掉**临时** profile 目录（超时未登录的 pending 目录）。
// 两种情况**绝不删目录**：
//   - keepProfile=true（登录成功，由调用方收编进槽或保留）；
//   - dirIsSlot=true（本次窗口用的就是账号的长期槽，删了等于让该号换设备）。
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
	if !keepProfile && !codeartsBrowserSession.dirIsSlot && codeartsBrowserSession.dir != "" {
		_ = os.RemoveAll(codeartsBrowserSession.dir)
	}
	codeartsBrowserSession.dir = ""
	codeartsBrowserSession.dirIsSlot = false
	codeartsBrowserSession.accountID = ""
	codeartsBrowserSession.port = 0
}

// codeartsBrowserWaitLogin 处理"还没登录"这一步：读 authui 的扫码状态机，
// 把「没扫 / 扫了没授权 / 授权了但回跳失败」三种情况区分开，并给出能照做的提示。
//
// 这是 2026-11-04 用户报「我登陆了但仍提示登录、提取不到」的正面修法。
// 根因（实测）：扫码成功后 authui 靠 302 → redirect_url（带 CAS ticket）
// 回跳控制台换会话；这一步失败时页面停在登录页，旧的文案判定永远说"未登录"，
// 面板既不报错也不提示，用户完全看不到卡在哪。
//
// 三种状态的处理：
//   - authorized + redirect_url：面板**代为跳转**并复验会话；通了就继续走保存流程。
//   - authorizing：正在授权，等下一轮。
//   - new / 427：二维码没被扫成功（最常见是扫了张过期码），明确让用户刷新重扫。
func (p *Panel) codeartsBrowserWaitLogin(w http.ResponseWriter, r *http.Request, port int, text string, cookies []codearts.Cookie) {
	msg := "还没检测到华为云登录态，请在浏览器窗口里完成登录"
	qr := codeartsQRProbe(port)
	if qr.Known && qr.Status == "authorized" && qr.RedirectURL != "" {
		// 扫码授权成功，但页面没自己跳回去（CAS ticket 回跳失败）。
		log.Printf("[codearts] 扫码已授权但页面未回跳（port %d），面板代为跳转", port)
		if err := codeartsNavigateTo(port, qr.RedirectURL); err == nil {
			time.Sleep(3 * time.Second)
			if codeartsConsoleSessionAlive(mustCDPCookies(port)) {
				log.Printf("[codearts] 代跳后控制台会话已通（port %d）", port)
				// 回跳成功 → 重跑一次完整判定（Cookie 已更新），这次会走保存流程。
				p.codeartsBrowserPoll(w, r)
				return
			}
		}
		msg = "扫码已成功，但华为云回跳控制台失败（票据交换被拒）。请在窗口里刷新二维码后重扫一次"
	} else if m := codeartsLoginMessage(qr, text); m != "" {
		msg = m
	}
	if msg == "还没检测到华为云登录态，请在浏览器窗口里完成登录" && strings.Contains(text, "登录华为云") {
		msg = "浏览器还停在登录页，请完成登录后自动保存"
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"ok": true, "running": true, "cookies": len(cookies),
		"sample": trimText(text, 200), "message": msg,
	})
}

// codeartsLoginMessage 按 authui 的扫码状态给出能照做的提示（纯函数，便于测试）。
//
// authui 的 /authui/qrcode/status 实测会回这些状态：
//
//	new        二维码刚出，没被扫过
//	authorizing 扫到了，正在授权
//	authorized  授权完成（带 redirect_url = CAS 回跳地址）
//	expired    二维码已过期（2026-11-04 实测现场就是这个）
//
// 另外 error_code=427 也是「二维码失效，需要刷新」，页面文案里同时会出现
// 「二维码已失效 刷新」。三条判据（状态 / 错误码 / 文案）取并集，避免任何
// 一条变化就漏判 —— 漏判的表现是笼统地说"还没登录"，用户完全看不到卡在哪。
//
// 返回空串 = 没有可给的具体状态（调用方用默认文案）。
func codeartsLoginMessage(qr codeartsQRState, text string) string {
	switch {
	case qr.Known && qr.Status == "authorizing":
		return "已扫到二维码，正在授权，请稍等（授权完成后会自动保存）"
	case qr.Known && (qr.Status == "expired" || qr.ErrorCode == "427" || codeartsQRExpired(text)):
		return "二维码已失效，请点一下二维码旁的「刷新」重新出码后扫码（旧的扫了也不会成功）"
	case qr.Known && qr.Status == "new":
		return "请用华为云 App 扫描窗口里的二维码（面板会自动保存，不需要手动确认）"
	}
	return ""
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
		//
		// ⚠️ 只认**我们自己起过的**端口（号池里记着 Port 的那些）。
		// 不加这个限制的话，会把用户自己日常开的 Chrome 窗口也扫进来，
		// 把它的 Cookie 当成新账号存进去 —— 相当于凭空多一个号、
		// 或者把号池搅乱。
		if ports := caDiscoverPorts(p.codeartsKnownPorts()); len(ports) > 0 {
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
		// ⚠️ 只看页面文案会漏掉**登录成功但页面没跳回来**的情况（实测 2026-11-04）：
		// 扫码后 authui 会 302 到 redirect_url（带 CAS ticket）换控制台会话，
		// 这一步失败时页面停在 authui 登录页 —— 文案判定永远说"未登录"。
		// 但此时浏览器里**已经有**控制台会话 Cookie（devclouddevuibjJ_SESSION_ID）
		// 和**认证侧**身份 Cookie（domain_tag/user_tag）。
		//
		// 所以这里补一路**实测**：拿浏览器当下的活 Cookie 直接打控制台只读接口，
		// 通 → 就是真登录了（保存）；不通 → 确实还没登录。
		if codeartsConsoleSessionAlive(cookies) {
			logged = true
			if nick == "" {
				nick = codeartsNicknameOf(cookies)
			}
			log.Printf("[codearts] 页面文案未判登录，但控制台接口实测已登录（port %d），按已登录处理", port)
		}
	}
	if !logged {
		p.codeartsBrowserWaitLogin(w, r, port, text, cookies)
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
	// 关窗口前先记住本次用的目录，并**一律保留**（keepProfile=true）：
	// 登录产生的 profile 就是这台"设备"的身份载体（Local State / Network
	// Persistent State / Local Storage），要么收编进该号的槽，要么（收编失败时）
	// 明确清掉，不能由 StopBrowser 顺手删。
	codeartsBrowserSession.mu.Lock()
	pendingDir := codeartsBrowserSession.dir
	inSlot := codeartsBrowserSession.dirIsSlot
	codeartsStopBrowserLocked(true)
	codeartsBrowserSession.mu.Unlock()

	if acc != nil && !inSlot && pendingDir != "" {
		if aerr := p.codeartsAdoptPendingProfile(acc, pendingDir); aerr != nil {
			log.Printf("[codearts] 账号 %s 的登录 profile 收编进身份槽失败: %v"+
				"（该槽将按空 profile 新建 —— 下次动作等同新设备）", acc.ID, aerr)
			_ = os.RemoveAll(pendingDir)
		}
	}

	// 会话最值钱的就是刚抓到的这一刻：当场把今天的份领掉，不等下一个排程。
	// （实测会话寿命约 7 小时，等定时任务很可能等到会话已死。）
	p.codeartsAutoClaimAfterLogin(acc)

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
// 关于 profile：2026-10-30 实测证明归档下来的 profile **恢复不了登录态**
// （浏览器一关，domain_tag / user_tag 这些会话 Cookie 一起消失，用归档目录
// 打开控制台照样显示「登录华为云」）。所以 profile 不用于免登录；
// 2026-11-03 起它有了新用途——**设备身份容器**（§H.4）：登录成功后由
// codeartsBrowserPoll 把 pending profile 收编进该号的槽，签到主路径仍是纯 HTTP。
func (p *Panel) codeartsSaveLogin(cookies []codearts.Cookie, nick, pendingDir string, port int, pageText string) (*codearts.Account, bool, error) {
	if len(cookies) == 0 {
		return nil, false, fmt.Errorf("没有 Cookie 可保存")
	}
	// 先算归一结论：登录保存与补采必须用**同一套**判定。否则重新登录这条路径
	// 只会把 TicketDeadline 算成空、TicketSource 留空 —— 这正是缺陷一里
	// "重新登录也不采集过期时间"的语义缺口（路径 B/C）。
	capture := codeartsCaptureResultFromCookies(cookies)
	identity := codearts.IdentityOf(cookies)
	if identity == "" {
		// 退一步：页面上的稳定账号标识。
		if k := codearts.AccountKeyFromText(pageText); k != "" {
			identity = "hid:" + k
		}
	}
	fp := codearts.Fingerprint(cookies)

	var saved *codearts.Account
	var added bool
	// 整个"读号池 → 认人 → 合并 → 写回"放在 UpdatePool 事务里：登录、补采、
	// 续期、保活都在并发写同一个文件，裸 Load→Save 会互相丢更新（R4.3）。
	err := codearts.UpdatePool(func(pool *codearts.Pool) error {
		// 先把历史上的重复记录合并掉（同一个人被拆成多条时用）。
		if merged := codearts.MergePool(pool); merged > 0 {
			log.Printf("[codearts] 号池合并：%d 条重复记录已并入同一个人", merged)
		}

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
				if identity != "" {
					accID = codearts.Fingerprint([]codearts.Cookie{{Name: "identity", Value: identity}})
				} else {
					// 认不出身份时只能用「指纹 + 序号」保证唯一，
					// 否则所有认不出身份的账号会拿到**同一个** ID，又会撞车。
					accID = fp + "-" + codearts.Seq(len(pool.Accounts)+1)
				}
				if accID == fp {
					accID = accID + "b"
				}
				slot = accID
			}
		}

		acc := &codearts.Account{
			ID: accID, Identity: identity, Cookies: cookies, SavedAt: codearts.Now(),
			Nickname: nick, DomainID: codearts.DomainIDOf(cookies), Slot: slot, Port: port,
			// 来源标记：这条会话是面板登录窗口拿到的（§H.2 ClientSource）。
			ClientSource: codearts.ClientSourceBrowser,
		}
		if nick != "" {
			acc.Label = nick
		}
		// 端口是**独占**的：上一个登录窗口关掉后端口立刻被回收复用，
		// 这个端口现在属于本次登录。别的账号还留着同一个端口记录的话，
		// 签到时会连到**别人的窗口**、用别人的会话替它签到（假签到）。
		// 所以这里把其它账号的同端口记录清掉。
		for _, a := range pool.Accounts {
			if a != nil && port > 0 && a.Port == port {
				a.Port = 0
			}
		}
		saved, added = codearts.Upsert(pool, acc)
		// Upsert 里命中的老账号要补上 Identity（老号池没这个字段）。
		if saved.Identity == "" && identity != "" {
			saved.Identity = identity
		}
		// 会话来自面板登录窗口 —— 覆盖旧的 ClientSource（import/paste → browser）。
		saved.ClientSource = codearts.ClientSourceBrowser
		// Upsert 内部的 RefreshTicketDeadline 只能判"有绝对票据"，判不出
		// "全是会话级"；这里用归一结论覆盖，登录后 ticket_source 才立即可见。
		codeartsApplyCaptureMeta(saved, capture, codearts.Now())
		// 再跑一遍号池整理：新账号加进来后 slot 可能和老账号撞了
		// （老账号的 slot 是历史遗留的，新账号的 slot 是按指纹派生的）。
		// slot 撞车 = 两个账号共用同一个本地目录。
		codearts.MergePool(pool)
		return nil
	})
	if err != nil {
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

// codeartsCookiesOf 只取 huaweicloud 域下的 Cookie，并**统一走归一入口**。
//
// 为什么不在这里自己判断 expires：CDP 的 `expires=-1` 表示会话 Cookie、`0`
// 表示没给（或字段缺失），两者的语义完全不同；各调用点自己解释的结果就是
// "采到了 -1 但号池里什么都没有"（旧版正是因为丢了 expires，号池里 51/55 条
// Cookie 没有一条带过期信息）。归一只有一个入口：codearts.NewObservedCookie。
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
		// 保留 expires=-1（会话 Cookie）：归一函数会把它落成
		// {Expires:-1, ExpiresKnown:true, Session:true}。
		out = append(out, codearts.NewObservedCookie(c.Name, c.Value, c.Domain, c.Path, c.Expires))
	}
	return out
}

// mustCDPCookies 读浏览器当前 Cookie；读失败返回空切片（调用方自己判空）。
func mustCDPCookies(port int) []codearts.Cookie {
	all, err := cdpAllCookies(port)
	if err != nil {
		return nil
	}
	return codeartsCookiesOf(all)
}

// codeartsNavigateTo 把登录窗口里**当前正在登录的那个页面**导航到目标 URL。
//
// 为什么只导航当前页而不是新开标签：新开标签会打断 authui 的页面上下文
//（client_key、service 参数都在原页面的 URL 上），而且用户会看到多一个标签页。
func codeartsNavigateTo(port int, target string) error {
	pages, err := caListPages(port)
	if err != nil {
		return err
	}
	for _, p := range pages {
		if !strings.Contains(p.URL, "auth.huaweicloud.com") {
			continue
		}
		ws, derr := wsDial(p.WS, 6*time.Second)
		if derr != nil {
			continue
		}
		nerr := caNavigate(ws, target)
		_ = ws.Close()
		if nerr == nil {
			return nil
		}
	}
	return fmt.Errorf("没有可导航的登录页面")
}

// codeartsConsoleSessionAlive 用**浏览器当下的活 Cookie** 打控制台只读接口，
// 判断控制台侧会话是不是真的活着。
//
// 为什么需要它（实测 2026-11-04 11:0x）：
//   - 页面文案判定只在"登录成功**且**页面跳回控制台"时才成立；
//   - 但 authui 扫码成功后的 CAS ticket 回跳（302 → redirect_url）可能失败，
//     此时页面停在 authui 登录页、文案永远说"未登录"，而浏览器里其实已经有
//     可用的控制台会话（devclouddevuibjJ_SESSION_ID + cftk 齐全）。
//   - 反过来，光看 Cookie 名字也会误判：未登录时同样有 SessionID / HWWAFSESID。
//
// 判据用**接口实测**：真打一次 /chat/snap-manager/v1/package/overview，
// 拿到非空 JSON 才算活。这是唯一不会骗人的判据。
//
// ⚠️ 只读接口：不 claim、不改号池、不刷新任何凭据。
func codeartsConsoleSessionAlive(cookies []codearts.Cookie) bool {
	if len(cookies) == 0 {
		return false
	}
	// 缺控制台侧会话 Cookie 就不用发请求了（省一次网络往返）。
	names := map[string]string{}
	for _, c := range cookies {
		names[c.Name] = c.Value
	}
	if names["devclouddevuibjJ_SESSION_ID"] == "" && names["devclouddevuibjtcftk"] == "" {
		return false
	}
	// 自己拼请求而不是复用 codearts.Credits：那个走 8 秒超时的共享 client，
	// 放在登录轮询里会让界面卡住。这里给 4 秒。
	parts := make([]string, 0, len(cookies))
	for _, c := range cookies {
		if c.Name == "" || c.Value == "" {
			continue
		}
		parts = append(parts, c.Name+"="+c.Value)
	}
	req, err := http.NewRequest(http.MethodGet,
		codearts.APIBase+"/chat/snap-manager/v1/package/overview", nil)
	if err != nil {
		return false
	}
	h := req.Header
	h.Set("Cookie", strings.Join(parts, "; "))
	h.Set("User-Agent", "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/156.0.0.0 Safari/537.36")
	h.Set("Accept", "application/json, text/plain, */*")
	h.Set("Referer", codearts.APIBase+"/chat/home")
	h.Set("language", "zh-cn")
	h.Set("x-language", "zh-cn")
	h.Set("x-codearts-doer-scenario-type", "web")
	h.Set("x-requested-with", "XMLHttpRequest")
	h.Set("Agent-Type", "PromptCenter")
	if cftk := names["devclouddevuibjtcftk"]; cftk != "" {
		h.Set("cftk", cftk)
	}
	cl := &http.Client{Timeout: 4 * time.Second}
	resp, err := cl.Do(req)
	if err != nil {
		return false
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	// 实测：会话失效的特征是 HTTP 200 + **空 body**（见 codearts.ErrSessionExpired）。
	// 有内容 = 接口真的返回了数据 = 会话活着。
	return resp.StatusCode == http.StatusOK && len(bytes.TrimSpace(raw)) > 0
}

// codeartsQRExpired 判断登录页上的二维码是否已失效。
//
// 实测文案：华为云 authui 二维码过期时页面出现「二维码已失效 刷新」。
// 这是最高频的卡点 —— 用户扫的是一张过期码，服务端 /authui/qrcode/status
// 一直是 {"status":"new"}（从未扫成功），用户却以为"我登录了"。
func codeartsQRExpired(text string) bool {
	if text == "" {
		return false
	}
	for _, kw := range []string{"二维码已失效", "二维码已过期", "扫码已失效"} {
		if strings.Contains(text, kw) {
			return true
		}
	}
	return false
}

// codeartsNicknameOf 从 Cookie 里取账号昵称（masked_user 形如 h*********uvj421wxe）。
func codeartsNicknameOf(cookies []codearts.Cookie) string {
	for _, c := range cookies {
		if c.Name == "masked_user" && c.Value != "" {
			return c.Value
		}
	}
	return ""
}

// ── 扫码登录状态（authui）────────────────────────────────────────────────

// codeartsQRState authui 扫码状态机（实测 2026-11-04 反查 app.bundle.js + 真实响应）。
//
//	{"status":"new"}        二维码刚出，还没扫
//	{"status":"authorizing"} 扫到了，正在授权
//	{"status":"authorized"}  授权成功 —— **redirect_url 里带 CAS ticket**，
//	                        前端就是 window.location.href = redirect_url 回跳控制台
//
// 为什么面板要自己读它（而不是只看页面文案）：
// 用户扫码成功后，authui 会 302 到 redirect_url 换控制台会话。这一步失败时
// 页面停在登录页、文案判定永远说"未登录"，用户看到的就是"我登录了但还是提示
// 登录、也提取不到"—— 面板既不报错也不提示，用户完全不知道卡在哪。
// 读 qrcode/status 能把「扫码没成功」「扫了没授权」「授权了但回跳失败」
// 三种状态区分开，给出准确提示。
type codeartsQRState struct {
	// Status new / authorizing / authorized / 其它
	Status string `json:"status"`
	// RedirectURL 授权成功后的回跳地址（带 ticket）
	RedirectURL string `json:"redirect_url"`
	// ErrorCode authui 的错误码（427 = 二维码失效需要刷新）
	ErrorCode string `json:"error_code"`
	// Known 页面里没有 authui 登录页（false = 不在扫码流程里）
	Known bool `json:"known"`
}

// codeartsQRProbe 在**登录窗口的 authui 页面里**读扫码状态。
//
// ⚠️ 必须用页面上下文发（fetch with credentials），因为 client_key 是页面
// 轮询时自己生成并带在 URL 上的；纯 HTTP 拿不到当前页面的 client_key。
//
// 只读：只 GET /authui/qrcode/status，不点任何按钮、不领任何东西。
func codeartsQRProbe(port int) codeartsQRState {
	pages, err := caListPages(port)
	if err != nil {
		return codeartsQRState{}
	}
	for _, p := range pages {
		if !strings.Contains(p.URL, "auth.huaweicloud.com") {
			continue
		}
		ws, derr := wsDial(p.WS, 6*time.Second)
		if derr != nil {
			continue
		}
		raw, eerr := caEval(ws, codeartsQRStatusJS)
		_ = ws.Close()
		if eerr != nil {
			continue // 页面正在跳转，下一轮再读
		}
		var st codeartsQRState
		if json.Unmarshal(raw, &st) != nil {
			continue
		}
		st.Known = true
		return st
	}
	return codeartsQRState{}
}

// codeartsQRStatusJS 在 authui 页面里取 client_key 并读扫码状态。
const codeartsQRStatusJS = `(async () => {
  const q = new URLSearchParams(location.search);
  const service = q.get('service') || '';
  // client_key 在页面自己的轮询 URL 上：从 performance 资源里反查。
  let ck = '';
  try {
    const r = performance.getEntriesByType('resource').map(e => e.name)
      .filter(n => n.indexOf('/authui/qrcode/status') >= 0);
    for (const n of r) {
      const m = n.match(/[?&]client_key=([^&]+)/);
      if (m) { ck = decodeURIComponent(m[1]); break; }
    }
  } catch (e) {}
  if (!ck) {
    // 轮询还没发出去过：从 qrcode 请求里找
    try {
      const r2 = performance.getEntriesByType('resource').map(e => e.name)
        .filter(n => n.indexOf('/authui/qrcode') >= 0);
      for (const n of r2) {
        const m = n.match(/[?&]client_key=([^&]+)/);
        if (m) { ck = decodeURIComponent(m[1]); break; }
      }
    } catch (e) {}
  }
  if (!ck) return JSON.stringify({ status: '', known: true });
  try {
    const u = '/authui/qrcode/status?client_key=' + encodeURIComponent(ck) +
      (service ? '&service=' + encodeURIComponent(service) : '');
    const r = await fetch(u, { credentials: 'same-origin' });
    const t = await r.text();
    let p = null;
    try { p = JSON.parse(t); } catch (e) {}
    if (!p) return JSON.stringify({ status: '', known: true, error_code: 'not-json' });
    return JSON.stringify({
      status: p.status || '', redirect_url: p.redirect_url || '',
      error_code: p.error_code || '', known: true
    });
  } catch (e) {
    return JSON.stringify({ status: '', known: true, error_code: String(e) });
  }
})()`

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

// codeartsClaim 优先复用「还开着的登录窗口」；窗口没了就只能靠 Cookie 直连。
func (p *Panel) codeartsClaim(acc *codearts.Account) (phase, msg, sample string, err error) {
	// ① 登录窗口还在：直接用真实鼠标点（窗口里就是活的会话）。
	//
	// ⚠️ 用之前必须核对窗口里登录的是**不是这个账号**：端口会被回收复用
	// （上一个窗口关掉，下一个登录又拿到同一个端口），端口记录可能指向
	// 别人的窗口 —— 拿别人的会话替这个号签到就是假签到。
	if acc.Port > 0 && caPortBelongsTo(acc.Port, acc) {
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

// caPortBelongsTo 核对某个调试端口上的浏览器登录的是不是这个账号。
//
// 为什么必须核对：调试端口是**独占但会回收**的——上一个登录窗口关掉后，
// 端口立刻被 freePort 重新分配给下一个登录窗口。号池里旧账号残留的 Port
// 记录就会指向别人的窗口；不核对就等于**拿别人的会话替这个号签到**
//（面板显示"签到成功"，实际领的是另一个人的积分）。
//
// 判据：窗口里的华为云 Cookie 算出的 Identity 要和账号一致；
// 拿不到稳定标识时保守地认为**不是**这个人（宁可少签，不可假签）。
func caPortBelongsTo(port int, acc *codearts.Account) bool {
	if acc == nil {
		return false
	}
	want := acc.Identity
	if want == "" {
		want = codearts.IdentityOf(acc.Cookies)
	}
	if want == "" {
		return false // 认不准就别用这个窗口
	}
	all, err := cdpAllCookies(port)
	if err != nil {
		return false
	}
	got := codearts.IdentityOf(codeartsCookiesOf(all))
	return got != "" && got == want
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
// codeartsAccountItem 组装单个账号在面板上要显示的信息（C-CA-API@1 A）。
//
// 拆成两步：codeartsAccountItemBase 只算**离线**字段（纯派生、可单测），
// 这里再补上游接口查到的积分/签到状态 —— 后者要发网络请求，线上是必需的，
// 但不该让字段契约的测试被网络拖慢。
//
// login_expired 的判据是**接口真的查不通**（会话失效），而不是"本地有没有
// profile"——实测归档 profile 恢复不了登录态，拿它判活会把好号误判成僵尸。
func (p *Panel) codeartsAccountItem(a *codearts.Account) map[string]any {
	item := codeartsAccountItemBase(a)

	// 每日能领多少积分 + 当前余额。
	daily := a.DailyBonus
	expired := false
	// 服务端说今天已领 ⇒ 直接算"已签到"。服务端是权威：
	// 面板上显示"可签到"但点了才知道其实领过，是最让人困惑的状态。
	serverClaimed := false

	/*
	 * ⚠ 2026-11-08：这两个上游请求改成**并行**。
	 *   以前是串行：Campaigns → Credits，各 8 秒超时 —— 一个死会话账号
	 *   会把整张列表拖住 16 秒（账号之间虽然并发，但一个慢号就够面板卡半天）。
	 *   并行后单号最坏 8 秒。
	 *   两个请求互不依赖（一个查活动、一个查余额），并行没有副作用问题。
	 */
	type campResult struct {
		camps []codearts.Campaign
		err   error
	}
	type creditResult struct {
		ci  *codearts.CreditInfo
		err error
	}
	chCamp := make(chan campResult, 1)
	chCredit := make(chan creditResult, 1)
	go func() {
		c, e := codearts.Campaigns(a)
		chCamp <- campResult{c, e}
	}()
	go func() {
		ci, e := codearts.Credits(a)
		chCredit <- creditResult{ci, e}
	}()

	cr := <-chCamp
	if cr.err == nil {
		if c, ok := codearts.DailyCampaign(cr.camps); ok {
			daily = float64(c.BenefitAmount)
			a.DailyBonus = daily
			serverClaimed = c.Claimed() || !c.Claimable
			item["daily_claimed"] = serverClaimed
		}
	} else if errors.Is(cr.err, codearts.ErrSessionExpired) {
		expired = true
	}
	item["daily_bonus"] = daily
	// 活动面值单独放 benefit_amount（契约澄清 #4）：daily_bonus 是既有的"今日
	// 签到面值"字段，而 credits 必须表示**真实结果**，两者不能混用。
	item["benefit_amount"] = daily
	if serverClaimed {
		item["checked_in_today"] = true
	}

	lr := <-chCredit
	if lr.err == nil && lr.ci != nil {
		ci := lr.ci
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
		if errors.Is(lr.err, codearts.ErrSessionExpired) {
			// 会话已失效（等价于 failed）：credits 必须是 0，**不能**拿旧快照
			// 冒充"当前余额"（契约澄清 #4）。旧值仍在 credits_total 等快照字段里。
			item["credits"] = 0.0
		}
		expired = true
	}
	item["login_expired"] = expired

	// 存回快照（下次接口不通时还有底）。必须走 UpdatePool：续期/补采/签到
	// 也在并发写同一个号池，裸 Load→Save 会把别人的修改冲掉。
	codeartsSaveAccountSnapshot(a)
	return item
}

// codeartsAccountItemBase 账号行里**不依赖上游接口**的全部字段。
//
// 字段名以 C-CA-API@1 A 为准，且**空值也必须出现**（前端按字段名渲染"未知"，
// 缺字段会被当成"后端没实现"）。
func codeartsAccountItemBase(a *codearts.Account) map[string]any {
	item := map[string]any{
		"id":                   a.ID,
		"label":                a.Label,
		"nickname":             a.Nickname,
		"saved_at":             a.SavedAt,
		"cookie_count":         len(a.Cookies),
		"checked_in_today":     codeartsCheckedInToday(a),
		"login_expired":        false,
		"credits":              0.0,
		"daily_bonus":          0.0,
		"benefit_amount":       0.0,
		"ticket_deadline":      a.TicketDeadline,
		"ticket_source":        a.TicketSource,
		"session_expires_at":   codearts.SessionExpiresAt(a),
		"last_ok_at":           a.LastOKAt,
		"renew_next_at":        a.RenewNextAt,
		"renew_result":         a.RenewResult,
		"renew_fail_at":        a.RenewFailAt,
		"renew_fail_msg":       a.RenewFailMsg,
		"probe_state":          a.ProbeState,
		"probe_msg":            a.ProbeMsg,
		"probe_at":             a.ProbeAt,
		"expires_known_count":  codeartsExpiresKnownCount(a.Cookies),
		"session_cookie_count": codeartsSessionCookieCount(a.Cookies),
		"can_auto_renew":       codearts.CanAutoRenew(a),
		"needs_manual":         codeartsNeedsManual(a),
	}
	if len(a.Aliases) > 0 {
		item["aliases"] = a.Aliases
	}
	if a.LastGain > 0 {
		item["today_gain"] = a.LastGain
		item["today_gain_at"] = a.LastGainAt
	}
	return item
}

// codeartsSaveAccountSnapshot 把积分/活动快照写回号池（值没变就不写）。
func codeartsSaveAccountSnapshot(a *codearts.Account) {
	if a == nil || a.ID == "" {
		return
	}
	_ = codearts.UpdatePool(func(pool *codearts.Pool) error {
		x := codeartsFindAccount(pool, a.ID)
		if x == nil {
			return nil
		}
		if x.CreditsRemain == a.CreditsRemain && x.CreditsTotal == a.CreditsTotal &&
			x.CreditsUsed == a.CreditsUsed && x.CreditsExpiring == a.CreditsExpiring &&
			x.DailyBonus == a.DailyBonus {
			return nil
		}
		x.CreditsRemain, x.CreditsTotal = a.CreditsRemain, a.CreditsTotal
		x.CreditsUsed, x.CreditsExpiring = a.CreditsUsed, a.CreditsExpiring
		x.DailyBonus = a.DailyBonus
		return nil
	})
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

	// 删除账号 + 定位槽目录放在同一个 UpdatePool 事务里：与补采/续期/保活并发
	// 时，裸 Load→Save 会把别人刚写进去的更新整条抹掉（R4.3）。
	// 回调内只改传入的 *Pool，不再调 LoadPool/SavePool（锁不可重入）。
	errAccountMissing := errors.New("账号不存在")
	var slotDir string
	err := codearts.UpdatePool(func(pool *codearts.Pool) error {
		acc := codeartsFindAccount(pool, req.ID)
		if acc == nil || !codearts.Remove(pool, req.ID) {
			return errAccountMissing
		}
		acc.EnsureSlot()
		slotDir = filepath.Join(codearts.SlotRoot(), acc.SlotDir)
		if abs, aerr := filepath.Abs(slotDir); aerr == nil {
			slotDir = abs
		}
		return nil
	})
	if err != nil {
		if errors.Is(err, errAccountMissing) {
			writeErr(w, http.StatusNotFound, "账号不存在")
			return
		}
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	// 身份槽**不直接删**：改名成 .bak-<ts>（里面是登录态，属于敏感数据，
	// 但误删不可逆；§H.I5 要求一切删除都可回滚）。
	if slotDir != "" {
		if _, aerr := codeartsSlotArchiveDir(slotDir); aerr != nil {
			log.Printf("[codearts] 账号 %s 的槽目录备份失败（账号记录已删除）: %v", req.ID, aerr)
		}
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
		// 与补采/续期/保活并发写同一份号池：必须走 UpdatePool（R4.3）。
		// 回调内只改传入的 *Pool，不再调 LoadPool/SavePool（锁不可重入）。
		_ = codearts.UpdatePool(func(pool *codearts.Pool) error {
			x := codeartsFindAccount(pool, acc.ID)
			if x == nil {
				return nil // 账号已被删除：没有可写的东西
			}
			x.CreditsRemain, x.CreditsTotal = acc.CreditsRemain, acc.CreditsTotal
			x.CreditsUsed, x.CreditsExpiring = acc.CreditsUsed, acc.CreditsExpiring
			x.DailyBonus = acc.DailyBonus
			x.LastGain, x.LastGainAt = acc.LastGain, acc.LastGainAt
			return nil
		})
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
	// Keepalive 本身不改 acc（只是探活 + 吸收服务端下发的续期 Cookie）。
	// 回写仍必须走 UpdatePool：与补采/续期/保活并发时，裸 Load→Save 会把
	// 别人刚写进去的更新整条覆盖（R4.3）。回调内只改传入的 *Pool。
	if err := codearts.UpdatePool(func(latest *codearts.Pool) error {
		x := codeartsFindAccount(latest, acc.ID)
		if x == nil {
			return nil // 并发删除：没有可写的账号
		}
		x.Cookies = append([]codearts.Cookie(nil), acc.Cookies...)
		x.SavedAt = acc.SavedAt
		x.TicketDeadline = acc.TicketDeadline
		x.TicketSource = acc.TicketSource
		return nil
	}); err != nil {
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

// codeartsKnownPorts 号池里记着的浏览器调试端口（我们自己起过的那些）。
func (p *Panel) codeartsKnownPorts() []int {
	pool, err := codearts.LoadPool()
	if err != nil {
		return nil
	}
	var out []int
	seen := map[int]bool{}
	for _, a := range pool.Accounts {
		if a == nil || a.Port <= 0 || seen[a.Port] {
			continue
		}
		seen[a.Port] = true
		out = append(out, a.Port)
	}
	return out
}

// codeartsMigratePool 启动时整理号池：合并"同一个人被拆成多条"的记录、
// 给每个账号分配互不相同的 slot。
//
// 2026-10-30 实测的现场：号池里两条记录的 domain_tag / user_tag /
// masked_user 全同（就是同一个人），slot 也相同 —— 面板上看着像加了两个号，
// 其实是一个，还互相覆盖。这里一次性收敛掉。
func (p *Panel) codeartsMigratePool() {
	// ① 先只读探测：没有需要整理的记录就**不写盘**（启动时不产生无谓写入）。
	// 这一步只是判断"要不要动"，真正的修改在下面的事务里重做，避免用过期
	// 快照盲写（R4.3：裸 Load→改→Save 会覆盖并发的补采/续期）。
	probe, err := codearts.LoadPool()
	if err != nil || probe == nil {
		return
	}
	if codearts.MergePool(probe) == 0 && codearts.DedupeLabels(probe) == 0 {
		return
	}

	// ② 有变化 → 在 UpdatePool 事务里重做，只改传入的 *Pool。
	var merged, relabeled int
	err = codearts.UpdatePool(func(pool *codearts.Pool) error {
		merged = codearts.MergePool(pool)
		relabeled = codearts.DedupeLabels(pool)
		return nil
	})
	if err != nil {
		log.Printf("[codearts] 号池整理后写回失败: %v", err)
		return
	}
	log.Printf("[codearts] 号池整理：合并 %d 条重复账号、改名 %d 个消重，现剩 %d 个",
		merged, relabeled, len(probe.Accounts))
}


// codeartsKeepalive 后台保活：定时用 Cookie 调一次轻量接口。
//
// 华为云的会话是「空闲超时」型：长时间不用才失效。定时打一次能把有效期
// 拉到服务端允许的上限，并顺带吸收服务端续期下发的新 Cookie。
//
// 频率 5 分钟（2026-11-08 从 15 分钟调快）：实测服务端只回一个会话 Cookie
// （BENSESSCC_TAG），**不给 SessionID 续期**，所以保活不是"续签"，
// 而是"别让它因闲置而过期"—— 那就更不能隔太久才碰它一次。
// 5 分钟足够把空闲计时器一直按在阈值以内（一次就是一个轻量接口的开销）。
//
// ⚠️ 这里必须比较 Cookie 的**值**而不是条数：服务端续期是**同名换值**
// （条数不变），只看条数的话新 Cookie 永远存不回号池，等于白保活。
func (p *Panel) codeartsKeepalive() {
	p.codeartsKeepaliveOnce()
	t := time.NewTicker(5 * time.Minute)
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
	// changedIDs 记录本轮保活**确实**改过 Cookie 的账号；落盘只写这些账号，
	// 不再整池回写（整池回写会把补采/续期刚写的新快照覆盖回旧值）。
	changedIDs := make([]*codearts.Account, 0, len(pool.Accounts))
	now0 := time.Now()

	/*
	 * ⚠ 2026-11-08：账号之间改成**并发**保活。
	 *   以前是逐号串行，一个死会话账号要等满 5 秒超时才轮到下一个；
	 *   号一多，一轮保活本身就可能超过新的 5 分钟间隔 —— 那等于保活没跟上，
	 *   "别让它因闲置而过期"这个目的就落空了。
	 *   面板的 keepalive 接口本来就是并发做的（codeartsKeepaliveHTTP），
	 *   这里与它保持一致。
	 *   每号改的是自己的 *Account，互不影响；落盘统一在下面做。
	 */
	var mu sync.Mutex
	var wg sync.WaitGroup
	for _, a := range pool.Accounts {
		if a == nil || len(a.Cookies) == 0 {
			continue
		}
		wg.Add(1)
		go func(a *codearts.Account) {
			defer wg.Done()
			before := codeartsCookieDigest(a.Cookies)
			ok, kerr := codearts.Keepalive(a)
			if kerr != nil {
				if errors.Is(kerr, codearts.ErrSessionExpired) {
					// 会话判死是**不可自愈**的状态：旧版每 15 分钟播一条，一天 96 条，
					// 把日志里真正有用的东西冲没了。每天只播第一条。
					if codeartsNoteOnce("codearts:dead:" + a.ID) {
						log.Printf("[codearts] 账号 %s 会话已失效，需要重新登录（今天已提醒过，后续静默；"+
							"自动续期见 panel: CodeArts 相关日志）", a.ID)
					}
				} else {
					log.Printf("[codearts] 账号 %s 保活失败: %v", a.ID, kerr)
				}
				return
			}
			if !ok {
				return
			}
			if codeartsCookieDigest(a.Cookies) != before {
				mu.Lock()
				changed = true
				changedIDs = append(changedIDs, a)
				mu.Unlock()
				log.Printf("[codearts] 账号 %s 保活时收到续期 Cookie，已更新", a.ID)
			}
		}(a)
	}
	wg.Wait()
	if changed {
		// 原子落盘（C-CA-TICKET@1 的 UpdatePool）：与补采/续期/登录并发写同一份
		// 号池时，各自 Load→改→Save 会互相覆盖；这里持包级锁读-改-写。
		// 回调内只改传入的 *Pool，不再调 LoadPool/SavePool（同包锁不可重入）。
		if err := codearts.UpdatePool(func(latest *codearts.Pool) error {
			for _, a := range changedIDs {
				x := codeartsFindAccount(latest, a.ID)
				if x == nil {
					continue
				}
				// 保活期间若补采/续期写了更新的快照，保留它（不能用本轮旧值覆盖）。
				if saved, serr := time.Parse(time.RFC3339, x.SavedAt); serr == nil && saved.After(now0) {
					continue
				}
				x.Cookies = append([]codearts.Cookie(nil), a.Cookies...)
				x.SavedAt = codearts.Now()
				x.RefreshTicketDeadline()
				if t := strings.TrimSpace(a.LastOKAt); t != "" {
					x.LastOKAt = t
				}
			}
			return nil
		}); err != nil {
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
