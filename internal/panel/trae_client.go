// trae_client.go 从**本机已登录的 Trae IDE 客户端**提取当前账号令牌。
//
// 背景：签到/对话号池（checkin_accounts.json）的 JWT 有效期只有 8 小时，续期原有
// 两条通道（refresh_token API、绑定的无头浏览器 profile）都依赖登录时留下的凭证。
// 而客户端本身登录着账号、且会自己把新 JWT 写进 storage.json 的 iCube 加密 blob ——
// 这是一份**永远新鲜的令牌来源**，不需要浏览器：
//
//	Trae 客户端 storage.json（%APPDATA%\Trae CN\User\globalStorage\storage.json）
//	  → 解密 iCubeAuthInfo://icube.cloudide → {"token":"<JWT>"} → uid
//	  → upsert 进号池（SaveTraeAccount 按 UserID 原地更新，保留 device_id 等）
//
// 触发点：进程启动后首次巡检前、每次 4h 续期巡检前、对话号池遇到 401 时（自救）、
// 以及面板「从客户端提取」按钮（force）。
package panel

import (
	"encoding/json"
	"log"
	"net/http"
	"os"
	"sync"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/ideauth"
	"github.com/linguo2625469/workbuddy2api-panel/internal/upstream"
)

// traeClientSyncMinGap 非强制触发的最小间隔（401 钩子 + 定时巡检共用，防止打爆）。
const traeClientSyncMinGap = 5 * time.Minute

var (
	traeClientSyncMu   sync.Mutex
	traeClientSyncLast time.Time
)

// traeClientSyncResult 单次提取结果（面板/日志共用）。
type traeClientSyncResult struct {
	OK        bool   `json:"ok"`
	Action    string `json:"action"` // updated | fresh | unavailable
	UserID    string `json:"user_id,omitempty"`
	Name      string `json:"name,omitempty"`
	ExpiresAt string `json:"expires_at,omitempty"`
	Message   string `json:"message,omitempty"`
}

// TraeSyncFromClient 从 Trae 客户端提取当前登录账号并 upsert 进号池。
//
// force=true（面板按钮）时跳过节流与「客户端令牌不比池内新」的判断，直接落库；
// 自动触发（定时/401 钩子）只在客户端令牌确实更新时才写，避免无谓写盘。
// 客户端未登录 / storage.json 不存在 / 解密失败 → OK=false，属于正常情况（不报错）。
func (p *Panel) TraeSyncFromClient(force bool) traeClientSyncResult {
	traeClientSyncMu.Lock()
	defer traeClientSyncMu.Unlock()
	if !force && time.Since(traeClientSyncLast) < traeClientSyncMinGap {
		return traeClientSyncResult{OK: false, Action: "fresh", Message: "距上次提取不足 5 分钟，跳过"}
	}
	traeClientSyncLast = time.Now()

	if !p.cfg.TraeEnabled {
		return traeClientSyncResult{OK: false, Action: "unavailable", Message: "Trae 板块未启用"}
	}
	raw, err := os.ReadFile(ideauth.TraeStoragePath())
	if err != nil {
		return traeClientSyncResult{OK: false, Action: "unavailable", Message: "本机未找到 Trae 客户端登录态（storage.json）"}
	}
	jwt, uid := ideauth.TraeClientJWTFromStorageRaw(raw)
	if jwt == "" || uid == "" {
		return traeClientSyncResult{OK: false, Action: "unavailable", Message: "Trae 客户端未登录或版本不含 iCube 令牌"}
	}

	// 与池内同账号比较：客户端令牌不比池内新就不写（force 除外）。
	if !force {
		accts, err := p.traeLoadAccounts()
		if err == nil {
			for _, a := range accts {
				if a.UserID != uid {
					continue
				}
				curExp, okCur := upstream.JWTExpiry(a.JWT)
				newExp, okNew := upstream.JWTExpiry(jwt)
				if okCur && okNew && !newExp.After(curExp) {
					return traeClientSyncResult{
						OK: false, Action: "fresh", UserID: uid,
						ExpiresAt: curExp.Local().Format("2006-01-02 15:04"),
						Message:   "池内令牌仍比客户端新，无需提取",
					}
				}
			}
		}
	}

	msg, err := p.traeSaveFromJWT(jwt)
	if err != nil {
		return traeClientSyncResult{OK: false, Action: "unavailable", UserID: uid, Message: err.Error()}
	}
	res := traeClientSyncResult{OK: true, Action: "updated", UserID: uid, Message: msg}
	if exp, ok := upstream.JWTExpiry(jwt); ok {
		res.ExpiresAt = exp.Local().Format("2006-01-02 15:04")
	}
	if a, err := findTraeAccount(p, uid); err == nil && a != nil {
		res.Name = a.Name
	}
	log.Printf("panel: Trae 客户端令牌提取成功 uid=%s 到期=%s", uid, res.ExpiresAt)
	return res
}

// traeClientSync HTTP：手动触发提取（面板「从客户端提取」按钮）。
func (p *Panel) traeClientSync(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Force bool `json:"force"`
	}
	_ = json.NewDecoder(r.Body).Decode(&body)
	res := p.TraeSyncFromClient(body.Force)
	code := http.StatusOK
	if !res.OK && res.Action == "unavailable" {
		code = http.StatusConflict
	}
	writeJSON(w, code, res)
}

// findTraeAccount 从号池里找账号（仅用于补展示名）。
func findTraeAccount(p *Panel, uid string) (*upstream.TraeAccount, error) {
	accts, err := p.traeLoadAccounts()
	if err != nil {
		return nil, err
	}
	for i := range accts {
		if accts[i].UserID == uid {
			return &accts[i], nil
		}
	}
	return nil, nil
}
