package ideauth

import (
	"sort"
	"strings"
)

// 号池多账号签到。
//
// 背景：Qoder 的每日签到（Campaign 领取）只需要两样东西——
//   1) 该账号的 access token（401 时用 refresh_token 续期）；
//   2) 设备指纹 Cosy-*（machineToken/machineCode 等）。
// 其中设备指纹是**设备级**的，与账号无关；token 号池里每个账号都存了。
// 所以「签到」根本不需要切号、不需要重启客户端，可以按号池逐个账号直接打接口。
//
// 之前的实现只调 QoderCheckin()（读磁盘上的当前登录态），于是面板里明明有两个号，
// 签到池也只显示/只能签当前那一个。本文件补上「按账号批量签到」的能力。

// QoderCheckinPoolResult 一次多号签到的汇总（面板直接展示）。
type QoderCheckinPoolResult struct {
	Results []QoderCheckinResult `json:"results"`
	Claimed int                  `json:"claimed_accounts"` // 本次新领取的账号数
	Already int                  `json:"already_accounts"` // 今日已领过的账号数
	Failed  int                  `json:"failed_accounts"`  // 失败账号数
	// Credits 本次**新增**的积分（只累加 claimed）；AlreadyCredits 是今日已领账号的额度，
	// 单独返回，避免把「今日已领」误报成「本次又赚了」。
	Credits        float64 `json:"credits_total"`
	AlreadyCredits float64 `json:"credits_already"`
	NoDevice       bool    `json:"no_device"` // 缺机器指纹（易被风控）
}

// QoderCheckinPool 按号池逐个账号签到。emails 为空 = 号池全部账号。
//
// 每个账号独立使用自己的 token；401 时各自续期，并把轮换后的 token 写回**号池**
// （绝不写客户端 auth.v1.dat —— 那是「当前登录号」的凭据，串写会直接把别的号顶掉）。
func QoderCheckinPool(emails []string) (*QoderCheckinPoolResult, error) {
	pool, err := LoadQoderPool()
	if err != nil {
		return nil, err
	}
	want := make(map[string]bool, len(emails))
	for _, e := range emails {
		if e = strings.ToLower(strings.TrimSpace(e)); e != "" {
			want[e] = true
		}
	}

	out := &QoderCheckinPoolResult{Results: make([]QoderCheckinResult, 0, len(pool.Accounts))}
	baseDev := QoderDeviceInfo()
	out.NoDevice = baseDev == nil || baseDev.MachineToken == ""

	changed := false
	for _, snap := range pool.Accounts {
		if snap == nil || snap.Auth == nil {
			continue
		}
		email := strings.TrimSpace(snap.Email)
		if email == "" {
			email = snap.Auth.User.Email
		}
		if len(want) > 0 && !want[strings.ToLower(email)] {
			continue
		}
		auth := cloneQoderAuth(snap.Auth)
		// 每个账号用**自己的**设备标识（不是客户端的物理设备），否则同一设备只能签一个号。
		dev := qoderDeviceForAccount(auth)
		res, at, rt := qoderCheckinAuth(auth, dev)
		res.Email = firstNonEmpty(email, res.Email)
		// token 轮换写回号池（下次切号/签到才能继续用）
		if at != "" {
			snap.Auth.Token = at
			changed = true
		}
		if rt != "" {
			snap.Auth.RefreshToken = rt
			changed = true
		}
		// 上游「领完即删」：今天已经领过的账号，activities 会变成空列表，
		// 上游报 pending。此时用本地签到记录兜底，否则面板永远显示"可签到"、
		// 点了却什么都不发生（用户看到的就是"签不了"）。
		if res.Phase == "pending" && CheckedInToday("qoder", res.Email) {
			res.Phase = "already"
			res.Msg = "今日已领取（本地记录）"
			res.CheckedInToday = true
		}
		switch res.Phase {
		case "claimed", "already":
			_ = MarkCheckedIn("qoder", res.Email)
			res.CheckedInToday = true
		}
		switch res.Phase {
		case "claimed":
			out.Claimed++
			out.Credits += res.Credits
		case "already":
			out.Already++
			out.AlreadyCredits += res.Credits
		default:
			out.Failed++
		}
		out.Results = append(out.Results, *res)
	}
	if changed {
		if err := SaveQoderPool(pool); err != nil {
			return out, err
		}
	}
	sort.SliceStable(out.Results, func(i, j int) bool { return out.Results[i].Email < out.Results[j].Email })
	return out, nil
}

// QoderPoolStatuses 号池各账号「今日是否已签到」。
// 只读本地签到日志（MarkCheckedIn 写的），不打上游——签到池进页面时不该发 N 个请求。
func QoderPoolStatuses() []map[string]any {
	pool, err := LoadQoderPool()
	if err != nil {
		return nil
	}
	out := make([]map[string]any, 0, len(pool.Accounts))
	for _, snap := range pool.Accounts {
		if snap == nil {
			continue
		}
		email := strings.TrimSpace(snap.Email)
		if email == "" && snap.Auth != nil {
			email = snap.Auth.User.Email
		}
		if email == "" {
			continue
		}
		out = append(out, map[string]any{
			"email":            email,
			"name":             snap.Name,
			"user_id":          snap.UserID,
			"checked_in_today": CheckedInToday("qoder", email),
		})
	}
	return out
}

// QoderUsageFor 查询指定账号的额度（email 为空 = 当前登录账号）。
// 号池里的账号直接用自己保存的 token 查，不需要切号。
func QoderUsageFor(email string) *QoderUsageResult {
	email = strings.TrimSpace(email)
	if email == "" {
		return QoderUsage()
	}
	pool, err := LoadQoderPool()
	if err != nil {
		return &QoderUsageResult{Phase: "error", Email: email, Msg: "读取号池失败: " + err.Error()}
	}
	for _, snap := range pool.Accounts {
		if snap == nil || snap.Auth == nil {
			continue
		}
		if strings.EqualFold(snap.Email, email) || strings.EqualFold(snap.Auth.User.Email, email) {
			return qoderUsageAuth(cloneQoderAuth(snap.Auth), QoderDeviceInfo())
		}
	}
	return &QoderUsageResult{Phase: "error", Email: email, Msg: "号池里没有该账号"}
}

// cloneQoderAuth 复制一份登录态，避免改动号池对象上的共享指针。
func cloneQoderAuth(a *QoderAuth) *QoderAuth {
	if a == nil {
		return nil
	}
	c := *a
	return &c
}
