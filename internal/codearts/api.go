package codearts

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// api.go CodeArts 的 HTTP 直连实现（不依赖浏览器窗口）。
//
// 端点（2026-10-30 用 CDP 从页面真实请求中抓到的，与前端 bundle 反查一致）：
//   GET  /chat/snap-manager/v1/package/overview          积分/套餐总览
//   GET  /chat/PromptCenterService/v1/ops/delivery        活动列表（签到活动 + claimable）
//   POST /chat/PromptCenterService/v1/ops/claim           领取 {"campaignId":N,"channel":"WEB"}
//
// 鉴权：Cookie + 一组自定义头。关键是 `cftk`（取自 cookie devclouddevuibjtcftk），
// 缺了会被网关判成 "invalid channel, group not found"。

// APIBase 网页端域名（测试里会被指向 httptest 服务器）。
var APIBase = "https://devcloud.cn-north-4.huaweicloud.com"

const apiUA = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/156.0.0.0 Safari/537.36"

// 8 秒足够：积分/活动列表都是小接口，超时太久会把面板列表卡在「加载中」。
var apiClient = &http.Client{Timeout: 8 * time.Second}

// ErrSessionExpired 会话已失效。
//
// 实测特征（2026-10-30）：华为云网关对**已过期的会话**返回的是
//
//	HTTP 200 + **空 body**
//
// 而不是 401/403。所以只判 HTTP 状态码会把"会话没了"误当成"接口坏了"，
// 面板上就永远显示不出「需重新登录」，账号也会被反复当成可签到。
var ErrSessionExpired = errors.New("华为云会话已失效，请重新登录")

// do 发一次带 Cookie + cftk 的请求。
func doAPI(acc *Account, method, path string, body any) (int, []byte, error) {
	if acc == nil || len(acc.Cookies) == 0 {
		return 0, nil, fmt.Errorf("账号没有 Cookie")
	}
	var rdr io.Reader
	if body != nil {
		buf, err := json.Marshal(body)
		if err != nil {
			return 0, nil, err
		}
		rdr = bytes.NewReader(buf)
	}
	url := APIBase + path
	req, err := http.NewRequest(method, url, rdr)
	if err != nil {
		return 0, nil, err
	}

	names := map[string]string{}
	parts := make([]string, 0, len(acc.Cookies))
	for _, c := range acc.Cookies {
		if c.Name == "" {
			continue
		}
		names[c.Name] = c.Value
		parts = append(parts, c.Name+"="+c.Value)
	}
	cftk := names["devclouddevuibjtcftk"]
	if cftk == "" {
		cftk = names["cftk"]
	}

	h := req.Header
	h.Set("Cookie", strings.Join(parts, "; "))
	h.Set("User-Agent", apiUA)
	h.Set("Accept", "application/json, text/plain, */*")
	h.Set("Content-Type", "application/json")
	h.Set("Referer", APIBase+"/chat/home")
	h.Set("language", "zh-cn")
	h.Set("x-language", "zh-cn")
	h.Set("x-codearts-doer-scenario-type", "web")
	h.Set("x-requested-with", "XMLHttpRequest")
	if cftk != "" {
		h.Set("cftk", cftk)
	}

	resp, err := apiClient.Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	// 服务端可能随响应下发新 Cookie（续期），顺手收进账号里。
	absorbSetCookies(acc, resp)
	if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
		return resp.StatusCode, raw, fmt.Errorf("%w（HTTP %d）", ErrSessionExpired, resp.StatusCode)
	}
	// **会话失效的真正特征**：HTTP 200 + 空 body（见 ErrSessionExpired 注释）。
	if len(bytes.TrimSpace(raw)) == 0 {
		return resp.StatusCode, raw, ErrSessionExpired
	}
	return resp.StatusCode, raw, nil
}

// absorbSetCookies 把响应里的 Set-Cookie 合并进账号（同名覆盖）。
func absorbSetCookies(acc *Account, resp *http.Response) {
	if resp == nil || acc == nil {
		return
	}
	scs := resp.Cookies()
	if len(scs) == 0 {
		return
	}
	idx := map[string]int{}
	for i, c := range acc.Cookies {
		idx[c.Name] = i
	}
	for _, sc := range scs {
		if sc.Name == "" || sc.Value == "" {
			continue
		}
		if i, ok := idx[sc.Name]; ok {
			acc.Cookies[i].Value = sc.Value
		} else {
			acc.Cookies = append(acc.Cookies, Cookie{Name: sc.Name, Value: sc.Value, Domain: sc.Domain})
			idx[sc.Name] = len(acc.Cookies) - 1
		}
	}
}

// Keepalive 调一次轻量接口，保持服务端会话活跃，并吸收新下发的 Cookie。
//
// 返回值：ok=true 表示这次调用成功（会话还活着）；err 非空且 errors.Is(err,
// ErrSessionExpired) 表示会话已失效，需要重新登录。
func Keepalive(acc *Account) (ok bool, err error) {
	st, _, err := doAPI(acc, http.MethodGet, "/chat/snap-manager/v1/package/overview", nil)
	if err != nil {
		return false, err
	}
	return st == http.StatusOK, nil
}

// SessionAlive 会话现在还能用吗（不消耗任何权益，只是探活）。
func SessionAlive(acc *Account) bool {
	ok, _ := Keepalive(acc)
	return ok
}

// CreditInfo 积分概况。
type CreditInfo struct {
	Remain   float64 `json:"remain"`
	Total    float64 `json:"total"`
	Used     float64 `json:"used"`
	Expiring float64 `json:"expiring"`
}

// Credits 查积分余额（package/overview 的 all_credit_package）。
func Credits(acc *Account) (*CreditInfo, error) {
	st, raw, err := doAPI(acc, http.MethodGet, "/chat/snap-manager/v1/package/overview", nil)
	if err != nil {
		return nil, err
	}
	if st != http.StatusOK {
		return nil, fmt.Errorf("package/overview HTTP %d: %s", st, truncate(string(raw), 160))
	}
	var out struct {
		All struct {
			Amount   float64 `json:"package_credit_amount"`
			Used     float64 `json:"package_credit_used"`
			Remain   float64 `json:"package_credit_remain"`
			Expiring float64 `json:"expiring_credit_amount"`
		} `json:"all_credit_package"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, err
	}
	return &CreditInfo{Remain: out.All.Remain, Total: out.All.Amount,
		Used: out.All.Used, Expiring: out.All.Expiring}, nil
}

// Campaign 一条活动（签到等）。
type Campaign struct {
	CampaignID    int    `json:"campaignId"`
	Title         string `json:"title"`
	Type          string `json:"type"`
	BenefitAmount int    `json:"benefitAmount"`
	BenefitUnit   string `json:"benefitUnit"`
	Claimable     bool   `json:"claimable"`
	// Status 活动状态：CLAIMED / CONFIRM / CONFIRMED / CONSUME 表示已领过。
	// 实测：领取成功后 status 变 CLAIMED、claimable 变 false。
	Status    string `json:"status"`
	StartTime string `json:"-"`
	EndTime   string `json:"-"`
}

// 已领状态集合（实测前端 bundle 里的状态机 + 真实响应）。
var claimedStatuses = map[string]bool{
	"CLAIMED":   true,
	"CONFIRM":   true,
	"CONFIRMED": true,
	"CONSUME":   true,
}

// Claimed 已领过没有。
func (c Campaign) Claimed() bool {
	return claimedStatuses[strings.ToUpper(c.Status)]
}

// DailyType 每日签到活动的类型（实测：type=USER_LOGIN，标题「每日签到领100积分」）。
const DailyType = "USER_LOGIN"

// Campaigns 拉活动列表（channel=WEB）。
func Campaigns(acc *Account) ([]Campaign, error) {
	st, raw, err := doAPI(acc, http.MethodGet, "/chat/PromptCenterService/v1/ops/delivery?channel=WEB", nil)
	if err != nil {
		return nil, err
	}
	if st != http.StatusOK {
		return nil, fmt.Errorf("ops/delivery HTTP %d: %s", st, truncate(string(raw), 160))
	}
	var out struct {
		Code int    `json:"code"`
		Msg  string `json:"message"`
		Data struct {
			Items []struct {
				CampaignID    int    `json:"campaignId"`
				Title         string `json:"title"`
				Type          string `json:"type"`
				BenefitAmount int    `json:"benefitAmount"`
				BenefitUnit   string `json:"benefitUnit"`
				Claimable     bool   `json:"claimable"`
				Status        string `json:"status"`
				Extra         struct {
					StartTime string `json:"startTime"`
					EndTime   string `json:"endTime"`
				} `json:"extra"`
			} `json:"items"`
		} `json:"data"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, err
	}
	list := make([]Campaign, 0, len(out.Data.Items))
	for _, it := range out.Data.Items {
		list = append(list, Campaign{
			CampaignID: it.CampaignID, Title: it.Title, Type: it.Type,
			BenefitAmount: it.BenefitAmount, BenefitUnit: it.BenefitUnit,
			Claimable: it.Claimable, Status: it.Status,
			StartTime: it.Extra.StartTime, EndTime: it.Extra.EndTime,
		})
	}
	return list, nil
}

// DailyCampaign 从活动列表里挑出「每日签到」那一条。
//
// 判据（实测）：type=USER_LOGIN 且标题含「签到」。不要只看标题——活动列表里
// 还有「学生认证领积分」这类带积分字样但与签到无关的项。
func DailyCampaign(camps []Campaign) (Campaign, bool) {
	for _, c := range camps {
		if c.Type == DailyType {
			return c, true
		}
	}
	for _, c := range camps {
		if strings.Contains(c.Title, "签到") {
			return c, true
		}
	}
	return Campaign{}, false
}

// ClaimResult 领取结果。
type ClaimResult struct {
	OK      bool
	Code    int
	Message string
}

// Already 今天已经领过了（服务端拒绝再次领取）。
func (r *ClaimResult) Already() bool {
	if r == nil || r.OK {
		return false
	}
	// 40001：重复领取/幂等键命中（实测无幂等键时也返回它，文案是「并发冲突，请重试」）。
	return r.Code == 40001 || r.Code == 40002 ||
		strings.Contains(r.Message, "已领") || strings.Contains(r.Message, "已领取")
}

// Claim 领取某条活动（不带幂等键）。
func Claim(acc *Account, campaignID int) (*ClaimResult, error) {
	return ClaimWithKey(acc, campaignID, "")
}

// ClaimWithKey 领取某条活动，带幂等键。
//
// 为什么必须带 idempotentKey（实测）：不带时，同一天第二次领取会返回
// 40001「并发冲突，请重试」——文案像并发冲突，其实是没带幂等键导致服务端
// 无法区分「重试」和「重复领取」。带上按天固定的键之后，同键重复调用才会
// 被识别成同一次领取。
func ClaimWithKey(acc *Account, campaignID int, idempotentKey string) (*ClaimResult, error) {
	body := map[string]any{"campaignId": campaignID, "channel": "WEB"}
	if idempotentKey != "" {
		body["idempotentKey"] = idempotentKey
	}
	st, raw, err := doAPI(acc, http.MethodPost, "/chat/PromptCenterService/v1/ops/claim", body)
	if err != nil {
		return nil, err
	}
	if st != http.StatusOK {
		return nil, fmt.Errorf("ops/claim HTTP %d: %s", st, truncate(string(raw), 160))
	}
	var out struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, err
	}
	return &ClaimResult{OK: out.Code == 0, Code: out.Code, Message: out.Message}, nil
}

// DailyIdempotentKey 生成当天的每日签到幂等键。
//
// 同一个账号 + 同一个活动 + 同一天 → 同一个键：这样重复调用是幂等的，
// 不会把服务端搞成「并发冲突」，也不会重复发积分。
func DailyIdempotentKey(accountID string, campaignID int, day string) string {
	return fmt.Sprintf("codearts-daily-%s-%d-%s", accountID, campaignID, day)
}

// nowDay 今天的日期串（本地时区）。
func nowDay() string { return time.Now().Format("20060102") }

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
