package ideauth

import (
	"sort"
	"strings"
	"sync"
	"time"
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
		// 上游报 pending。此时用**可信的**本地签到记录兜底（10:05 前写入的记录
		// 是旧版误报，不能用来把 pending 翻成 already），否则面板永远显示"可签到"。
		if res.Phase == "pending" && CheckinRecordTrusted("qoder", res.Email) {
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
		row := map[string]any{
			"email":            email,
			"name":             snap.Name,
			"user_id":          snap.UserID,
			"checked_in_today": CheckedInToday("qoder", email),
		}
		if t, ok := CheckedInAt("qoder", email); ok {
			row["checked_in_at"] = t.Format(time.RFC3339)
		}
		out = append(out, row)
	}
	return out
}

// UsedDeviceQuotaToday 报告本机今天是否已有**其他**账号写入了可信的 Qoder
// 签到记录（每设备每日限领一次；用于向被隐藏活动的账号解释原因）。
func UsedDeviceQuotaToday(excludeEmail string) bool {
	today := time.Now().Format("2006-01-02")
	for email := range CheckinDates("qoder") {
		if strings.EqualFold(email, strings.TrimSpace(excludeEmail)) {
			continue
		}
		if CheckedInToday("qoder", email) && CheckinRecordTrusted("qoder", email) {
			_ = today
			return true
		}
	}
	return false
}

// QoderScanAccount 号池单账号的上游核实结果。
type QoderScanAccount struct {
	Email          string `json:"email"`
	Name           string `json:"name,omitempty"`
	UID            string `json:"user_id,omitempty"`
	Phase          string `json:"phase"` // claimed / claimable / pending / login_required / error
	Msg            string `json:"msg,omitempty"`
	CheckedInLocal bool   `json:"checked_in_local"`
	CheckedInAt    string `json:"checked_in_at,omitempty"`
	Refreshed      bool   `json:"refreshed,omitempty"`
	// CheckedInToday **该账号今天是否真的签到了** —— 以**上游核实结果**为准
	// （phase=claimed/already），不采信本地记录。
	//
	// 为什么必须与 CheckedInLocal 分开：本地日志里可能残留旧版写的「纯日期」记录
	// （无时刻信息），按自然日解析会落在当日 00:00，于是"今天签过"为真 —— 而
	// 上游活动列表里根本没有今天的 CLAIM_BENEFIT。把它当"已签到"展示，就是
	// 面板凭空说假话。2026-10-24 实测踩坑：浏览器加的号（gmail）挂着这种旧记录，
	// 面板显示"今日已签到"，而真正领到 100 Credits 的另一个号反而显示"可签到"。
	CheckedInToday bool `json:"checked_in_today"`
	// RecordTrusted 本地记录是否可信（今日 10:05 之后写入的完整时间戳）。
	RecordTrusted bool `json:"record_trusted,omitempty"`
	// Source 判定来源：upstream = 上游活动列表确认；local = 上游看不到今日活动
	// （领完即删），但本地有可信记录。
	Source string `json:"source,omitempty"`
	// DeviceQuotaUsed 本账号看不到今日活动，是因为同设备已有别的账号领走了
	// 当天的唯一名额（服务端每设备每日限领一个号）。
	DeviceQuotaUsed bool `json:"device_quota_used,omitempty"`
}

// QoderPoolScan 逐账号向**上游**核实签到状态（并发 2，只查不领）。
//
// 为什么必须有这一步：此前「扫描」只读本地记录，用户永远无法确认面板说的「已签」
// 是不是真的（实测 09:00 的假已签一直挂到中午才被发现）。现在扫描直接问上游：
// claimable = 可领、claimed = 今天真领过、pending = 活动未下发（含只有昨天陈旧
// 活动的情形）。上游不可达时回落本地记录并在 msg 里说明。
func QoderPoolScan(emails []string) []QoderScanAccount {
	pool, err := LoadQoderPool()
	if err != nil {
		return nil
	}
	want := make(map[string]bool, len(emails))
	for _, e := range emails {
		if e = strings.ToLower(strings.TrimSpace(e)); e != "" {
			want[e] = true
		}
	}
	type job struct {
		snap *QoderSnapshot
	}
	var jobs []job
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
		jobs = append(jobs, job{snap: snap})
	}
	out := make([]QoderScanAccount, len(jobs))
	sem := make(chan struct{}, 2)
	var wg sync.WaitGroup
	for i, j := range jobs {
		wg.Add(1)
		go func(i int, j job) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			snap := j.snap
			email := strings.TrimSpace(snap.Email)
			if email == "" {
				email = snap.Auth.User.Email
			}
			row := QoderScanAccount{
				Email:          email,
				Name:           snap.Name,
				UID:            snap.UserID,
				CheckedInLocal: CheckedInToday("qoder", email),
			}
			if t, ok := CheckedInAt("qoder", email); ok {
				row.CheckedInAt = t.Format(time.RFC3339)
			}
		auth := cloneQoderAuth(snap.Auth)
		oc := qoderScanAuth(auth, qoderDeviceForAccount(auth))
		res := oc.result
		// 续期出的新 token 写回号池（绝不写客户端登录态）。
		if oc.newAccess != "" || oc.newRefresh != "" {
			if oc.newAccess != "" {
				snap.Auth.Token = oc.newAccess
			}
			if oc.newRefresh != "" {
				snap.Auth.RefreshToken = oc.newRefresh
			}
			_ = SaveQoderPool(pool)
			row.Refreshed = true
		}
		row.Phase = res.Phase
		row.Msg = res.Msg
		// 本地记录的可信判定：10:05 前写入的今日记录是旧版误报（把昨天的陈旧
		// 已领活动当成了今天的），一律忽略并明确提示。
		trusted := CheckinRecordTrusted("qoder", email)
		row.RecordTrusted = trusted
		if row.CheckedInLocal && !trusted {
			row.CheckedInLocal = false
			row.Msg = strings.TrimSpace(row.Msg + "；本地存在 10:05 前的可疑签到记录（旧版误报），已忽略，以上游为准")
		}
		// 「今天到底签没签」的真值判定：**以上游活动列表为准**。
		//
		//   claimed   → 上游今天的 CLAIM_BENEFIT 已是 CLAIMED → 真签到
		//   claimable → 今天还没领（可领）
		//   pending   → 上游看不到今天的活动（设备名额被占 / 未下发 / 领完即删）
		//               此时只有「本地可信记录」才能判已签，否则一律未签
		//   其它      → 上游不可达，本地可信记录兜底
		//
		// 关键：绝不能让「本地记录说签过」盖过「上游说没签」。旧版正是这么反的 ——
		// 浏览器加的号挂着一条旧格式（纯日期）记录，面板显示"今日已签到"，而真正
		// 领到 100 Credits 的另一个号反而显示"可签到"，两个号整个反了。
		switch res.Phase {
		case "claimed":
			row.CheckedInToday = true
			row.Source = "upstream"
		case "claimable":
			row.CheckedInToday = false
		case "pending":
			if trusted {
				row.CheckedInToday = true
				row.Source = "local"
				row.Msg = "今日已领取（本地记录 " + row.CheckedInAt + "；上游活动列表未见今日活动）"
			} else {
				row.CheckedInToday = false
			}
		default: // login_required / error
			row.CheckedInToday = false
			if trusted {
				row.Phase = "pending"
				row.CheckedInToday = true
				row.Source = "local"
				row.Msg = "上游核实失败（" + res.Msg + "）；本地记录今日已签 " + row.CheckedInAt
			}
		}
		// 设备名额：每台设备每日限领一个账号（服务端规则，2026-10-24 实测：
		// 第一个号领取后，同设备其余账号的签到活动被隐藏）。本机今天已有其他
		// 账号可信领取记录、而本账号看不到签到活动时，如实说明而不是笼统的
		// "活动未下发" —— 这两种情况处理方式完全不同，用户需要分得清。
		if res.Phase == "pending" && !trusted && UsedDeviceQuotaToday(email) {
			row.DeviceQuotaUsed = true
			row.Msg = "本设备今日签到名额已被其他账号使用（每设备每日限领一次，服务端规则）"
		}
		out[i] = row
		}(i, j)
	}
	wg.Wait()
	sort.SliceStable(out, func(i, j int) bool { return out[i].Email < out[j].Email })
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
