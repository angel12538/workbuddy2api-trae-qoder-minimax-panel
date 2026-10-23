// trae_refresh.go Trae 令牌**自动续期**。
//
// 问题：Trae 的 access token 就是 Cloud-IDE-JWT，**有效期只有 8 小时**（实测
// iat→exp 恒为 28800s），过期后签到接口一律 401。exp 由 Trae 服务器签发，
// 客户端改不了（改了签名即失效）——所以「延长」的唯一手段是**续期**。
//
// 两条续期通道，按优先级：
//  1. refresh_token → ExchangeToken（不开浏览器、秒级完成；最理想）
//  2. 账号绑定的持久 profile → 拉起无头浏览器打开 trae.cn，让站点用长效登录态
//     自己换新 JWT，再从 localStorage 读回（兜底，覆盖「浏览器提取」的老账号）
//
// 定时循环每 4 小时巡检一遍（JWT 8h，留一半余量），只对「剩余不足 3 小时」的账号动手。
package panel

import (
	"context"
	"encoding/json"
	"log"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/ideauth"
	"github.com/linguo2625469/workbuddy2api-panel/internal/upstream"
)

const (
	// traeRefreshMargin 剩余不足这个时长就续期（JWT 8h → 提前 3h 续，够容错）。
	traeRefreshMargin = 3 * time.Hour
	// traeRefreshInterval 续期巡检间隔。
	traeRefreshInterval = 4 * time.Hour
)

// traeRefreshMu 串行化续期：浏览器通道一次只能占一个 profile / 调试端口，
// 并发跑会互相抢端口。签到用的是另一把锁（traeMu），两者互不阻塞。
var traeRefreshMu sync.Mutex

// traeRefreshResult 单账号续期结果。
type traeRefreshResult struct {
	Name      string `json:"name"`
	UserID    string `json:"user_id"`
	Status    string `json:"status"`           // refreshed | skipped | fail
	Method    string `json:"method,omitempty"` // api | browser
	Message   string `json:"message,omitempty"`
	ExpiresAt string `json:"expires_at,omitempty"`
}

// traeRefreshOne 续期单个账号。force=true 时忽略「还没到点」直接续。
func (p *Panel) traeRefreshOne(a upstream.TraeAccount, force bool) traeRefreshResult {
	res := traeRefreshResult{Name: a.Name, UserID: a.UserID}
	if !force && !upstream.TraeNeedsRefresh(a, traeRefreshMargin) {
		res.Status = "skipped"
		res.Message = "凭证仍在有效期内，无需续期"
		if exp, ok := upstream.JWTExpiry(a.JWT); ok {
			res.ExpiresAt = exp.Local().Format("2006-01-02 15:04")
		}
		return res
	}

	// ── 通道 1：refresh_token（首选）────────────────────────────────────────
	if rt := strings.TrimSpace(a.RefreshToken); rt != "" {
		jwt, newRT, err := upstream.TraeExchangeToken(rt)
		if err == nil {
			if _, serr := upstream.SaveTraeAccount(p.cfg.TraeDataDir, upstream.TraeAccount{
				UserID: a.UserID, JWT: jwt, RefreshToken: newRT,
			}); serr != nil {
				res.Status, res.Method, res.Message = "fail", "api", "续期成功但写回失败: "+serr.Error()
				return res
			}
			res.Status, res.Method, res.Message = "refreshed", "api", "已用 refresh_token 续期"
			if exp, ok := upstream.JWTExpiry(jwt); ok {
				res.ExpiresAt = exp.Local().Format("2006-01-02 15:04")
			}
			log.Printf("panel: Trae 续期成功(api) uid=%s 新到期 %s", a.UserID, res.ExpiresAt)
			return res
		}
		res.Message = "refresh_token 续期失败：" + err.Error()
		log.Printf("panel: Trae 续期(refresh_token)失败 uid=%s: %v，改试浏览器通道", a.UserID, err)
	}

	// ── 通道 2：绑定的持久 profile + 无头浏览器 ──────────────────────────────
	dir := traeProfileDirFor(p, a.UserID)
	if st, err := os.Stat(dir); err != nil || !st.IsDir() {
		res.Status = "fail"
		if res.Message == "" {
			res.Message = "该账号既无 refresh_token、也无绑定的浏览器会话，需在「添加账号」里重新登录一次"
		} else {
			res.Message += "；且无绑定的浏览器会话"
		}
		return res
	}
	jwt, err := traeBrowserRefresh(dir)
	if err != nil {
		res.Status, res.Method = "fail", "browser"
		res.Message = strings.TrimSpace(res.Message + "；浏览器续期失败：" + err.Error())
		return res
	}
	if _, serr := upstream.SaveTraeAccount(p.cfg.TraeDataDir, upstream.TraeAccount{
		UserID: a.UserID, JWT: jwt,
	}); serr != nil {
		res.Status, res.Method, res.Message = "fail", "browser", "续期成功但写回失败: "+serr.Error()
		return res
	}
	res.Status, res.Method, res.Message = "refreshed", "browser", "已用绑定的浏览器会话续期"
	if exp, ok := upstream.JWTExpiry(jwt); ok {
		res.ExpiresAt = exp.Local().Format("2006-01-02 15:04")
	}
	log.Printf("panel: Trae 续期成功(browser) uid=%s 新到期 %s", a.UserID, res.ExpiresAt)
	return res
}

// traeRefreshAll 巡检全部账号。串行处理，避免并发抢浏览器端口 / 触发风控。
func (p *Panel) traeRefreshAll(force bool) []traeRefreshResult {
	traeRefreshMu.Lock()
	defer traeRefreshMu.Unlock()

	accts, err := p.traeLoadAccounts()
	if err != nil {
		log.Printf("panel: Trae 续期读取账号失败: %v", err)
		return nil
	}
	out := make([]traeRefreshResult, 0, len(accts))
	refreshed, skipped, failed := 0, 0, 0
	for _, a := range accts {
		r := p.traeRefreshOne(a, force)
		switch r.Status {
		case "refreshed":
			refreshed++
		case "skipped":
			skipped++
		default:
			failed++
		}
		out = append(out, r)
		time.Sleep(500 * time.Millisecond)
	}
	if len(accts) > 0 {
		log.Printf("panel: Trae 续期巡检完成：续期 %d / 免续 %d / 失败 %d", refreshed, skipped, failed)
	}
	return out
}

// traeRefresh HTTP：手动触发续期。body 可给 {"user_ids":[...]} 指定账号、{"force":true} 强制。
func (p *Panel) traeRefresh(w http.ResponseWriter, r *http.Request) {
	var body struct {
		UserIDs []string `json:"user_ids"`
		Force   bool     `json:"force"`
	}
	_ = json.NewDecoder(r.Body).Decode(&body)

	if len(body.UserIDs) > 0 {
		want := map[string]bool{}
		for _, u := range body.UserIDs {
			if u != "" {
				want[u] = true
			}
		}
		accts, err := p.traeLoadAccounts()
		if err != nil {
			writeErr(w, http.StatusBadGateway, "读取 Trae 账号失败: "+err.Error())
			return
		}
		traeRefreshMu.Lock()
		defer traeRefreshMu.Unlock()
		results := make([]traeRefreshResult, 0, len(body.UserIDs))
		for _, a := range accts {
			if want[a.UserID] {
				results = append(results, p.traeRefreshOne(a, body.Force))
			}
		}
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "results": results})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "results": p.traeRefreshAll(body.Force)})
}

// StartTraeAutoRefresh 启动 Trae 令牌自动续期巡检（配置关闭时不动）。
//
// 与自动签到（StartTraeAuto）互补：签到负责「每天领积分」，续期负责「凭证别过期」。
// 两者各用一把锁，互不阻塞。
func (p *Panel) StartTraeAutoRefresh(ctx context.Context) {
	if !p.cfg.TraeEnabled {
		return
	}
	go func() {
		// 启动后先等 1 分钟：避开进程启动期的初始化高峰，也让账号列表先就绪。
		startup := time.NewTimer(time.Minute)
		select {
		case <-ctx.Done():
			startup.Stop()
			return
		case <-startup.C:
		}
		for {
			// ── 三家客户端统一提取（都在这轮巡检里做，失败静默不影响彼此）──
			// 1) Trae：storage.json 解 iCube 块 → 当前登录账号的新 JWT。
			if r := p.TraeSyncFromClient(false); r.OK {
				log.Printf("panel: Trae 客户端提取: %s", r.Message)
			}
			// 2) Qoder：客户端 auth.v1.dat（DPAPI 解密）随客户端自刷新，
			//    巡检时同步进号池——客户端登录着谁就更新谁，新账号直接入池。
			if snap, added, err := ideauth.UpsertQoderAccount(); err == nil {
				log.Printf("panel: Qoder 客户端提取: %s（新增=%v）", snap.Email, added)
			}
			// 3) WorkBuddy：把号池各账号的客户端会话文件原文对齐客户端最新
			//    （客户端在线会随使用刷新 token，切号恢复需要最新原文）。
			if n, err := ideauth.RefreshWorkBuddyAuthFilesFromClient(); err == nil && n > 0 {
				log.Printf("panel: WorkBuddy 会话文件已对齐客户端（更新 %d 个）", n)
			}
			p.traeRefreshAll(false)
			t := time.NewTimer(traeRefreshInterval)
			select {
			case <-ctx.Done():
				t.Stop()
				return
			case <-t.C:
			}
		}
	}()
	log.Printf("panel: Trae 令牌自动续期已启用（每 %s 巡检一次）", traeRefreshInterval)
}
