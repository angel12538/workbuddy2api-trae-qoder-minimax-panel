// usage_official.go 面板「官方用量」接口：拉上游真实逐请求用量并聚合。
//
// 与 panel.go 的 usage（面板自身网关统计）互补：那个只看经过面板的请求，用户用官方
// 客户端时恒为 0。本接口并发拉全部账号的官方用量，聚合出「真实用了多少」。
package panel

import (
	"fmt"
	"log"
	"net/http"
	"sort"
	"strconv"
	"sync"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
)

// usageOfficialMaxPages 单账号单次最多翻页数（pageSize=100 → 上限 1000 条/账号），
// 防止超长窗口把上游打爆；超出部分截断（返回 truncated 标记）。
const usageOfficialMaxPages = 10

// usageOfficialAccount 单账号官方用量聚合。
type usageOfficialAccount struct {
	UID       string  `json:"uid"`
	Nickname  string  `json:"nickname"`
	Realm     string  `json:"realm"`
	Requests  int     `json:"requests"`
	Credits   float64 `json:"credits"`
	Truncated bool    `json:"truncated,omitempty"`
}

// usageOfficialModel 按模型聚合。
type usageOfficialModel struct {
	Model    string  `json:"model"`
	Requests int     `json:"requests"`
	Credits  float64 `json:"credits"`
}

// usageOfficialDay 按日聚合（series 用）。
type usageOfficialDay struct {
	Day      string  `json:"t"`
	Requests int     `json:"requests"`
	Credits  float64 `json:"credits"`
}

// usageOfficialErr 单账号拉取失败（不影响其他账号）。
type usageOfficialErr struct {
	UID      string `json:"uid"`
	Nickname string `json:"nickname"`
	Error    string `json:"error"`
}

// usageOfficial 拉取全部账号的官方逐请求用量并聚合。
//
// 查询参数 hours：统计窗口小时数（默认 72=近 3 天，上限 720=30 天）。
// 返回 totals / by_account / by_model / series（按日）/ errors。
func (p *Panel) usageOfficial(w http.ResponseWriter, r *http.Request) {
	hours := 72
	if v := r.URL.Query().Get("hours"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			hours = n
		}
	}
	if hours > 720 {
		hours = 720
	}
	if hours < 1 {
		hours = 1
	}
	now := time.Now()
	start := now.Add(-time.Duration(hours) * time.Hour)
	// 上游要求 RFC3339 带时区（本地时区即可，实测 +08:00 生效）。
	startStr := start.Format(time.RFC3339)
	endStr := now.Format(time.RFC3339)

	states := p.cfg.Pool.List()
	type acct struct {
		a *auth.Auth
	}
	var accts []acct
	for _, st := range states {
		if st.Disabled {
			continue
		}
		if a := p.cfg.Pool.AuthByUID(st.UID); a != nil {
			accts = append(accts, acct{a: a})
		}
	}

	var (
		mu      sync.Mutex
		byAcct  []usageOfficialAccount
		errs    []usageOfficialErr
		byModel = map[string]*usageOfficialModel{}
		byDay   = map[string]*usageOfficialDay{}
	)
	var wg sync.WaitGroup
	for _, ac := range accts {
		wg.Add(1)
		go func(a *auth.Auth) {
			defer wg.Done()
			row := usageOfficialAccount{
				UID: a.UID, Nickname: a.Nickname, Realm: a.Realm(),
			}
			for page := 1; page <= usageOfficialMaxPages; page++ {
				res, err := p.cfg.Upstream.GetUserRequestUsage(a, startStr, endStr, page, 100)
				if err != nil {
					// 第 2 页起失败**也要记**：只记第 1 页会让该账号「统计了前半段」却
					// 既无 error 也无截断标记 —— 用量/积分被静默少算（计费视图最忌此）。
					mu.Lock()
					errs = append(errs, usageOfficialErr{UID: a.UID, Nickname: a.Nickname,
						Error: fmt.Sprintf("第 %d 页拉取失败：%v", page, err)})
					mu.Unlock()
					row.Truncated = true
					return
				}
				mu.Lock()
				for _, it := range res.Data {
					row.Requests++
					row.Credits += it.Credit
					m := it.Model
					if m == "" {
						m = "未知"
					}
					mm := byModel[m]
					if mm == nil {
						mm = &usageOfficialModel{Model: m}
						byModel[m] = mm
					}
					mm.Requests++
					mm.Credits += it.Credit
					day := it.RequestTime
					if len(day) >= 10 {
						day = day[:10]
					}
					dd := byDay[day]
					if dd == nil {
						dd = &usageOfficialDay{Day: day}
						byDay[day] = dd
					}
					dd.Requests++
					dd.Credits += it.Credit
				}
				mu.Unlock()
				// 分页终止判据：以「本页条数 < pageSize」为主（上游部分域 Total 返回 0，
				// 只看 Total 会首页就 break，结果只取 100 条还没有任何截断提示）；
				// Total 仅作辅助加速。
				if len(res.Data) < 100 {
					break
				}
				if res.Total > 0 && page*100 >= res.Total {
					break
				}
				if page == usageOfficialMaxPages {
					row.Truncated = true
				}
			}
			mu.Lock()
			byAcct = append(byAcct, row)
			mu.Unlock()
		}(ac.a)
	}
	wg.Wait()

	sort.Slice(byAcct, func(i, j int) bool { return byAcct[i].Credits > byAcct[j].Credits })
	models := make([]usageOfficialModel, 0, len(byModel))
	for _, m := range byModel {
		models = append(models, *m)
	}
	sort.Slice(models, func(i, j int) bool { return models[i].Credits > models[j].Credits })
	days := make([]usageOfficialDay, 0, len(byDay))
	for _, d := range byDay {
		days = append(days, *d)
	}
	sort.Slice(days, func(i, j int) bool { return days[i].Day < days[j].Day })

	totalReq, totalCredit := 0, 0.0
	active := 0
	for _, a := range byAcct {
		totalReq += a.Requests
		totalCredit += a.Credits
		if a.Requests > 0 {
			active++
		}
	}
	log.Printf("panel: 官方用量 uid 数=%d 窗口=%dh 请求=%d 积分=%.2f 失败=%d",
		len(byAcct), hours, totalReq, totalCredit, len(errs))

	writeJSON(w, http.StatusOK, map[string]any{
		"ok":           true,
		"window_hours": hours,
		"start":        startStr,
		"end":          endStr,
		"totals": map[string]any{
			"requests":    totalReq,
			"credits":     totalCredit,
			"accounts":    len(byAcct),
			"active":      active,
			"active_days": len(days),
		},
		"by_account": byAcct,
		"by_model":   models,
		"series":     days,
		"errors":     errs,
	})
}
