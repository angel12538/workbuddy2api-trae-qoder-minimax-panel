// usage_official.go 官方用量（上游真实逐请求调用记录）。
//
// 为什么需要：面板自带的 usage 记录器只统计**经过面板 API 网关**的请求。用户若用
// 官方客户端直连上游（常见），面板「用量」页会显示 0，让人误以为统计坏了。
// 本接口直接拉上游官方的逐请求用量，补上「真实用了多少」。
//
// 接口（CN / 国际版同路径，走 chatBase）：
//
//	POST /billing/meter/get-user-request-usage
//	  body {"startTime":"2026-09-01T00:00:00+08:00","endTime":"...","pageNum":1,"pageSize":50}
//	  resp {"code":0,"data":{"total":N,"data":[
//	        {"requestId","credit","model","client","requestTime","inputTrunc","input","agentPurpose"}]}}
//
// 实测要点（踩过的坑）：
//   - startTime/endTime 必须是 **RFC3339 字符串**（Go 结构体字段 startTime 为 string，
//     传数字会报 "cannot unmarshal number into ... of type string"）；写 0 或纯日期都
//     报 invalid params。
//   - 路径**不带 /v2 前缀**（/v2/billing/... 实测 404）；CN 与 global 一致。
//   - 分页字段是 pageNum / pageSize（page/page_size 不认）。
package upstream

import (
	"encoding/json"
	"net/http"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
)

// userRequestUsagePath 官方逐请求用量路径（chatBase，CN/global 同路径，无 /v2 前缀）。
const userRequestUsagePath = "/billing/meter/get-user-request-usage"

// UserRequestUsageItem 单条调用记录（字段名与上游 JSON 对齐）。
type UserRequestUsageItem struct {
	RequestID    string  `json:"requestId"`
	Credit       float64 `json:"credit"`      // 本条扣除的积分
	Model        string  `json:"model"`       // 模型名
	Client       string  `json:"client"`      // 客户端来源（WorkBuddy 等）
	RequestTime  string  `json:"requestTime"` // 本地时间 "2006-01-02 15:04:05"
	AgentPurpose string  `json:"agentPurpose,omitempty"`
}

// UserRequestUsagePage 一页用量记录。
type UserRequestUsagePage struct {
	Total int                    `json:"total"`
	Data  []UserRequestUsageItem `json:"data"`
}

// GetUserRequestUsage 拉取指定时间窗内的一页官方用量记录。
// startTime/endTime 必须是 RFC3339 带时区字符串（如 2026-09-01T00:00:00+08:00）。
func (c *Client) GetUserRequestUsage(a *auth.Auth, startTime, endTime string, pageNum, pageSize int) (*UserRequestUsagePage, error) {
	if pageNum < 1 {
		pageNum = 1
	}
	if pageSize < 1 || pageSize > 200 {
		pageSize = 100
	}
	body := map[string]any{
		"startTime": startTime,
		"endTime":   endTime,
		"pageNum":   pageNum,
		"pageSize":  pageSize,
	}
	// chatBase + BillingHeaders（与 growth 域同传输）。
	data, err := c.growthJSON(a, http.MethodPost, userRequestUsagePath, body)
	if err != nil {
		return nil, err
	}
	var page UserRequestUsagePage
	if err := json.Unmarshal(data, &page); err != nil {
		return nil, err
	}
	return &page, nil
}
