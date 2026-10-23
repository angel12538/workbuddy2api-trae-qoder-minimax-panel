// model_queue.go 面板「模型排队与冷却」数据面。
//
// 汇总三款 IDE 各自能拿到的「模型排队 / 冷却」信息：
//   - WorkBuddy（本代理上游）：池内逐（域, 模型）的模型级冷却 + 在途/可服务数
//     （来自 pool.ModelLockView，即选号实况）；
//   - Trae：v1 额度接口下发的**模型请求限额**（快速/慢速高级模型请求、高级模型、
//     自动补全、并行上限）——这是 Trae 唯一暴露的模型请求排队/额度数据面；
//     本地 Trae 客户端二次校验的「当前账号」也一并透出。
//   - Qoder：无模型级排队/冷却接口，仅有套餐额度（在其它视图展示），此处给出说明。
package panel

import (
	"net/http"
	"sync"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/upstream"
)

// traeLimitAccountView 单账号的 Trae 模型请求限额视图。
type traeLimitAccountView struct {
	Name          string                      `json:"name"`
	UserID        string                      `json:"user_id"`
	Limits        []upstream.TraeRequestLimit `json:"limits,omitempty"`
	ParallelLimit int                         `json:"parallel_limit,omitempty"`
	Expired       bool                        `json:"expired,omitempty"`
	Error         string                      `json:"error,omitempty"`
}

// modelQueue 汇总三款 IDE 的模型排队/冷却数据。
func (p *Panel) modelQueue(w http.ResponseWriter, r *http.Request) {
	// ① WorkBuddy：池内模型级冷却全清单（哪些模型不能用、锁了几个号、还要锁多久）。
	locks := p.cfg.Pool.ModelLockView()

	// ② Trae：逐账号查模型请求限额（并发限流，与 traeCredits 同口径）。
	accts, _ := p.traeLoadAccounts()
	trae := make([]traeLimitAccountView, len(accts))
	var wg sync.WaitGroup
	sem := make(chan struct{}, 3)
	for i, a := range accts {
		wg.Add(1)
		go func(i int, a upstream.TraeAccount) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			v := traeLimitAccountView{Name: a.Name, UserID: a.UserID}
			if exp, ok := upstream.TraeJWTExpiry(a.JWT); ok && time.Now().After(exp) {
				v.Expired = true
				v.Error = upstream.TraeAuthHint(a)
				trae[i] = v
				return
			}
			limits, parallel, err := upstream.TraeRequestLimits(a)
			if err != nil {
				v.Error = err.Error()
			} else {
				v.Limits, v.ParallelLimit = limits, parallel
			}
			trae[i] = v
		}(i, a)
	}
	wg.Wait()

	// ③ Trae 模型通道（trae: 前缀转发）的实时并发占用/冷却。
	// 这是**本网关自己**的中继池状态，与上面 ② 的上游额度互补：
	// 上游额度说「账号还能排多少」，此处说「此刻有几个请求正占着名额」。
	chat := map[string]any{"enabled": false}
	if p.traeChat != nil {
		st := p.traeChat.Status()
		busy, cooling := 0, 0
		for _, s := range st {
			busy += s.InFlight
			if s.Cooling {
				cooling++
			}
		}
		models := 0
		if p.traeCatalog != nil {
			if ms, err := p.traeCatalog.Models(r.Context()); err == nil {
				models = len(ms)
			}
		}
		chat = map[string]any{
			"enabled":     true,
			"in_flight":   busy,
			"cooling":     cooling,
			"accounts":    st,
			"model_count": models,
			"endpoint":    "POST /v1/chat/completions  model=\"trae:<模型名>\"",
		}
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"ok":    true,
		"locks": locks,
		"trae":  trae,
		"trae_chat": chat,
		"qoder": map[string]any{
			"has_model_queue": false,
			"note":            "Qoder 无模型级排队/冷却接口，仅提供套餐额度",
		},
	})
}

// traeLimits 单独暴露 Trae 模型请求限额（供模型页按需刷新）。
func (p *Panel) traeLimits(w http.ResponseWriter, r *http.Request) {
	accts, err := p.traeLoadAccounts()
	if err != nil {
		writeErr(w, http.StatusBadGateway, "读取 Trae 账号失败: "+err.Error())
		return
	}
	views := make([]traeLimitAccountView, len(accts))
	var wg sync.WaitGroup
	sem := make(chan struct{}, 3)
	for i, a := range accts {
		wg.Add(1)
		go func(i int, a upstream.TraeAccount) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			v := traeLimitAccountView{Name: a.Name, UserID: a.UserID}
			if exp, ok := upstream.TraeJWTExpiry(a.JWT); ok && time.Now().After(exp) {
				v.Expired = true
				v.Error = upstream.TraeAuthHint(a)
				views[i] = v
				return
			}
			limits, parallel, lerr := upstream.TraeRequestLimits(a)
			if lerr != nil {
				v.Error = lerr.Error()
			} else {
				v.Limits, v.ParallelLimit = limits, parallel
			}
			views[i] = v
		}(i, a)
	}
	wg.Wait()
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "accounts": views})
}
