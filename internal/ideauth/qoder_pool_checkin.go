package ideauth

import (
	"fmt"
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
// token 号池里每个账号都存了；设备指纹是**设备级**的，所有账号共用同一份。
// 所以「签到」根本不需要切号、不需要重启客户端，可以按号池逐个账号直接打接口。
//
// 之前的实现只调 QoderCheckin()（读磁盘上的当前登录态），于是面板里明明有两个号，
// 签到池也只显示/只能签当前那一个。本文件补上「按账号批量签到」的能力。
//
// ⚠️ 每日额度不是每个号一份（2026-11-02 实测）：服务端按**账号归属的 scope**
// （同一实名/租户）发，同 scope 的多个账号共享**一份**。详见 QoderQuotaGroupKey。

// QoderCheckinPoolResult 一次多号签到的汇总（面板直接展示）。
type QoderCheckinPoolResult struct {
	Results []QoderCheckinResult `json:"results"`
	Claimed int                  `json:"claimed_accounts"` // 本次新领取的账号数
	Already int                  `json:"already_accounts"` // 今日已领过的账号数
	Failed  int                  `json:"failed_accounts"`  // 失败账号数
	// Blocked 今天**没签上**的账号数：服务端判定「同一个人另一账号已领走每日额度」。
	// 这不是失败（重试也没用），也不是"已签到"（本号根本没领到）—— 单列一栏。
	Blocked int `json:"blocked_accounts"`
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
	return QoderCheckinPoolOrdered(emails, "")
}

// QoderCheckinPoolOrdered 同 QoderCheckinPool，但 priority 指定的账号先打。
//
// 「先打先得」是 Qoder 的硬规则：每日 100 Credits 按**人**给，同一个人的多个
// 账号一天只有一份（实测服务端返回 failureCode=SAME_PERSON_ALREADY_CLAIMED）。
// priority 让用户能指定「今天这 100 给哪个号」—— 一键依然是把**所有**号都打一遍，
// 只是顺序可指定，其余号照常尝试并如实回报结果。
func QoderCheckinPoolOrdered(emails []string, priority string) (*QoderCheckinPoolResult, error) {
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

	// 收集要处理的账号（号池顺序），再把 priority 提到最前。
	type poolItem struct {
		snap  *QoderSnapshot
		email string
	}
	var items []poolItem
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
		items = append(items, poolItem{snap: snap, email: email})
	}
	if p := strings.TrimSpace(priority); p != "" {
		for i := range items {
			if strings.EqualFold(items[i].email, p) {
				it := items[i]
				items = append([]poolItem{it}, append(append([]poolItem{}, items[:i]...), items[i+1:]...)...)
				break
			}
		}
	} else if len(items) > 1 {
		// 自动路径（没人指定今天这份给谁）：**最久没真正领到的号先打**。
		//
		// 为什么必须排：每日额度按**人**给，一天只有一份，先打的那个号拿到，
		// 其余的号服务端一律回 SAME_PERSON_ALREADY_CLAIMED（2026-10-24 实测；
		// 2026-11-01 换整套伪造设备指纹复测，结论一字不变）。此前自动签到调的是
		// 不带 priority 的 QoderCheckinPool(nil) → 顺序 = 号池文件里的顺序 →
		// 排第一的号天天赢，第二个号一年也轮不到一次。Claims/LastClaimedAt
		// 这两个字段当初就是为轮换存的，但一直没有接上（全仓零调用点）。
		//
		// 先把各号的上次领取时刻取一遍再排：LastClaimedAt 每次都要读一遍签到
		// 日志文件，放进比较函数就是 O(n log n) 次磁盘读。
		last := make(map[string]time.Time, len(items))
		for _, it := range items {
			if t, ok := LastClaimedAt("qoder", it.email); ok {
				last[strings.ToLower(it.email)] = t
			}
		}
		sort.SliceStable(items, func(i, j int) bool {
			return last[strings.ToLower(items[i].email)].Before(last[strings.ToLower(items[j].email)])
		})
	}

	out := &QoderCheckinPoolResult{Results: make([]QoderCheckinResult, 0, len(items))}
	baseDev := QoderDeviceInfo()
	out.NoDevice = baseDev == nil || baseDev.MachineToken == ""

	// 额度组：同一 scope 的账号共享**同一份**每日额度（2026-11-02 实测）。
	// 组内只有一个人能领到今天的 100 Credits；组与组之间互不影响。
	groupOf := map[string]string{} // 邮箱(小写) → 组键
	groupSize := map[string]int{}  // 邮箱(小写) → 组内账号数
	for _, a := range pool.Accounts {
		em := strings.ToLower(strings.TrimSpace(qoderSnapshotEmail(a)))
		if em == "" {
			continue
		}
		groupOf[em] = QoderQuotaGroupKey(a)
	}
	for _, ems := range QoderQuotaGroups(pool.Accounts) {
		for _, em := range ems {
			groupSize[strings.ToLower(em)] = len(ems)
		}
	}

	changed := false
	// 本次运行里已知的活动实例 id（账号 → cid）：先领到的号会贡献自己的 cid，
	// 后面的号就拿它去探「同一人是否已领」。
	knownCID := map[string]string{}
	// 需要二次探测的账号（自己的列表里没有今天的活动）。
	type pendingProbe struct {
		snap  *QoderSnapshot
		email string
		res   *QoderCheckinResult
		auth  *QoderAuth
		dev   *QoderDevice
		group string
	}
	var probes []pendingProbe

	for _, it := range items {
		snap := it.snap
		email := it.email
		auth := cloneQoderAuth(snap.Auth)
		// 每个账号用**自己的**设备标识（不是客户端的物理设备），否则同一设备只能签一个号。
		dev := qoderDeviceForAccount(auth)
		res, at, rt := qoderCheckinAuth(auth, dev, QoderQuotaScopeKey(snap))
		res.Email = firstNonEmpty(email, res.Email)
		res.Scope = QoderQuotaScopeKey(snap)
		res.QuotaGroup = groupSize[strings.ToLower(strings.TrimSpace(email))]
		// token 轮换写回号池（下次切号/签到才能继续用）
		if at != "" {
			snap.Auth.Token = at
			changed = true
		}
		if rt != "" {
			snap.Auth.RefreshToken = rt
			changed = true
		}
		if res.ClaimCID != "" {
			knownCID[strings.ToLower(res.Email)] = res.ClaimCID
			if snap.ClaimCID != res.ClaimCID {
				snap.ClaimCID = res.ClaimCID
				changed = true
			}
		}
		if res.Phase == "pending" {
			// 自己的列表里没有今天的活动：交给下面的二次探测（那里才是这类账号的
			// 归宿 —— 要么拿**同额度组**兄弟号的实例 id 问出 SAME_PERSON 判定，
			// 要么用本地可信记录兜底成「今日已领取」）。
			probes = append(probes, pendingProbe{
				snap: snap, email: email, res: res, auth: auth, dev: dev,
				group: groupOf[strings.ToLower(strings.TrimSpace(email))],
			})
			continue
		}
		switch res.Phase {
		case "claimed":
			out.Claimed++
			out.Credits += res.Credits
			if res.ClaimedToday {
				res.CheckedInToday = true
				// 用服务端给的 claimedAt 落时间戳：本机时钟不准时（实测这台机器慢
				// 8 小时）用 time.Now() 会把真领到的那次记到可信线之前，面板于是
				// 整天显示「没签上」，重试循环也白跑。
				_ = MarkClaimedAt("qoder", res.Email, claimStamp(res))
			}
			_ = MarkCheckedIn("qoder", res.Email)
		case "already":
			out.Already++
			out.AlreadyCredits += res.Credits
			res.CheckedInToday = true
			_ = MarkCheckedIn("qoder", res.Email)
		default:
			out.Failed++
		}
		out.Results = append(out.Results, *res)
	}

	// 二次探测：自己的列表里没有今天的活动 → 拿**同一额度组**另一个账号的活动实例
	// id 去领一次，向服务端要一个真实判定（而不是含糊地说"暂无活动"）。
	//
	// 实测（2026-10-24）：同组另一个账号领过之后，服务端回
	// status=BLOCKED / failureCode=SAME_PERSON_ALREADY_CLAIMED —— 说明这个号
	// 今天的每日额度已经被「同一个人」的另一个账号领走了，本号确实无需再领。
	// 若这次反而领成功（服务端放行），那就是真·领到，按 claimed 计。
	//
	// ⚠️ 只能取**同额度组**的 cid（2026-11-02 修正）。额度组 = 服务端 scope，
	// 同组共享一份每日额度；不同组的账号各有各的额度，拿别的组的实例 id 去领，
	// 服务端回的就不再是 SAME_PERSON 判定，而本号可能**自己就能领** —— 拿错
	// 会把一个本来能领的号误判成「额度已被领走」。
	for i := range probes {
		p := &probes[i]
		cid := ""
		// 同组候选按邮箱排序，保证每次挑到的是同一个（map 遍历顺序随机）。
		var candidates []string
		for em := range knownCID {
			if strings.EqualFold(em, p.email) {
				continue
			}
			if groupOf[strings.ToLower(em)] != p.group {
				continue
			}
			candidates = append(candidates, em)
		}
		sort.Strings(candidates)
		if len(candidates) > 0 {
			cid = knownCID[strings.ToLower(candidates[0])]
		}
		if cid == "" {
			// 号池快照里可能已存着别的号的实例 id（上次扫描记下的）。
			for _, o := range pool.Accounts {
				if o == nil || o.ClaimCID == "" {
					continue
				}
				if strings.EqualFold(o.Email, p.email) {
					continue
				}
				if groupOf[strings.ToLower(strings.TrimSpace(o.Email))] != p.group {
					continue
				}
				cid = o.ClaimCID
				break
			}
		}
		if cid == "" {
			// 同组里没有别的号的活动实例可比 → 只能按"活动未下发"如实说。
			if CheckinRecordTrusted("qoder", p.email) {
				p.res.Phase = "already"
				p.res.Msg = "今日已领取（本地记录）"
				p.res.CheckedInToday = true
				out.Already++
				out.AlreadyCredits += p.res.Credits
				_ = MarkCheckedIn("qoder", p.email)
			} else {
				out.Failed++
			}
			out.Results = append(out.Results, *p.res)
			continue
		}
		ph, amt, detail, claimedAt := qoderClaimInstance(p.auth, p.dev, cid, QoderQuotaScopeKey(p.snap))
		switch ph {
		case "claimed":
			p.res.Phase = "claimed"
			p.res.Msg = fmt.Sprintf("签到成功 +%.0f Credits", amt)
			p.res.Credits = amt
			p.res.Claimed = 1
			p.res.CheckedInToday = true
			// 用兄弟号的实例 id 反而领成功了 = 服务端放行了，本号今天真领到。
			p.res.ClaimedAt = claimedAt
			p.res.ClaimedToday = true
			out.Claimed++
			out.Credits += amt
			_ = MarkClaimedAt("qoder", p.email, claimStamp(p.res))
			_ = MarkCheckedIn("qoder", p.email)
		case "blocked":
			// 服务端明确答复：**同一个人**今天已经领过了（同组另一账号领走），
			// 也就是说**本号今天并没有签到**，今天的 100 积分也没进它的账。
			//
			// ⚠️ 这里绝不能写 MarkCheckedIn —— 那是"假签到"：本号压根没领到。
			// 也不能算进 Already（那也是"今天签过了"的意思）。单列 blocked，
			// 并把之前可能存在的误记清掉（Unmark）。
			p.res.Phase = "blocked"
			p.res.Msg = "今天没签上：同一额度组（同一实名）另一账号已领走每日额度（服务端判定 " + detail + "）"
			p.res.CheckedInToday = false
			out.Blocked++
			_ = UnmarkCheckedIn("qoder", p.email)
		default:
			if CheckinRecordTrusted("qoder", p.email) {
				p.res.Phase = "already"
				p.res.Msg = "今日已领取（本地记录）"
				p.res.CheckedInToday = true
				out.Already++
				_ = MarkCheckedIn("qoder", p.email)
			} else {
				out.Failed++
			}
		}
		out.Results = append(out.Results, *p.res)
	}

	// 额度组归属：标出每组里今天**谁**领到了那份每日额度，让面板能说清
	// 「这个号为什么没签上」而不是只丢一句"被同一人领走了"。
	groupHolder := map[string]string{}
	for _, r := range out.Results {
		if r.Phase != "claimed" || !r.ClaimedToday {
			continue
		}
		g := groupOf[strings.ToLower(strings.TrimSpace(r.Email))]
		if _, ok := groupHolder[g]; !ok {
			groupHolder[g] = r.Email
		}
	}
	for i := range out.Results {
		r := &out.Results[i]
		g := groupOf[strings.ToLower(strings.TrimSpace(r.Email))]
		if h, ok := groupHolder[g]; ok && !strings.EqualFold(h, r.Email) {
			r.QuotaTakenBy = h
			if r.Phase == "blocked" {
				r.Msg = r.Msg + "；今日额度由同组账号 " + h + " 领走"
			}
		}
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

// QuotaHolderInGroup 返回**同一额度组**里今天已领到每日额度的账号邮箱。
//
// 2026-11-02 修正：每日额度按账号归属的 scope（服务端认定的实名/租户）发，**不是
// 按设备**（实测伪造整套 Cosy-* 设备头也不影响判定，刷新 token 的响应里更是没有
// 任何设备字段）。不同 scope 的账号各有各的每日额度 —— 拿**别的组**的领取记录
// 去解释本号「没签上」是错的，会把一个本来能领的号说成额度被占。
//
// 同组多号时按邮箱排序取第一个，保证结果稳定。
func QuotaHolderInGroup(excludeEmail string) (string, bool) {
	pool, err := LoadQoderPool()
	if err != nil || pool == nil {
		return "", false
	}
	ex := strings.ToLower(strings.TrimSpace(excludeEmail))
	group := ""
	for _, a := range pool.Accounts {
		if a == nil {
			continue
		}
		if strings.ToLower(strings.TrimSpace(qoderSnapshotEmail(a))) == ex {
			group = QoderQuotaGroupKey(a)
			break
		}
	}
	if group == "" {
		return "", false
	}
	var holders []string
	for _, a := range pool.Accounts {
		if a == nil {
			continue
		}
		em := strings.TrimSpace(qoderSnapshotEmail(a))
		if em == "" || strings.EqualFold(em, excludeEmail) {
			continue
		}
		if QoderQuotaGroupKey(a) != group {
			continue
		}
		if CheckedInToday("qoder", em) && CheckinRecordTrusted("qoder", em) {
			holders = append(holders, em)
		}
	}
	if len(holders) == 0 {
		return "", false
	}
	sort.Strings(holders)
	return holders[0], true
}

// UsedDeviceQuotaToday 报告**同一额度组**里今天是否已有其它账号写入可信的 Qoder
// 签到记录（用于向看不到今日活动的账号解释原因）。
//
// 名字保留是为了不破坏既有调用点，语义已从「同设备」改成「同额度组」。
func UsedDeviceQuotaToday(excludeEmail string) bool {
	_, ok := QuotaHolderInGroup(excludeEmail)
	return ok
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
	// DeviceQuotaUsed 本账号看不到今日活动，是因为**同一额度组**（同一 scope /
	// 同一实名）的另一账号已领走了当天唯一一份每日额度（服务端按人给，先打先得）。
	DeviceQuotaUsed bool `json:"same_person_taken,omitempty"`
	// Scope 本账号归属的额度组标识（服务端下发的 agentAccountScope）。
	Scope string `json:"scope,omitempty"`
	// QuotaGroup 本账号所在额度组里的账号数（含自己）。>1 = 与别的号共享每日额度。
	QuotaGroup int `json:"quota_group,omitempty"`
	// QuotaTakenBy 同组里今天领到每日额度的账号（空 = 组里还没人领）。
	QuotaTakenBy string `json:"quota_taken_by,omitempty"`
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
	// 额度组信息：同一 scope 的账号共享同一份每日额度，扫描时要按组解释。
	groupSize := QoderQuotaGroupSizes(pool.Accounts)
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
				Scope:          QoderQuotaScopeKey(snap),
				QuotaGroup:     groupSize[strings.ToLower(strings.TrimSpace(email))],
			}
			if t, ok := CheckedInAt("qoder", email); ok {
				row.CheckedInAt = t.Format(time.RFC3339)
			}
		auth := cloneQoderAuth(snap.Auth)
		oc := qoderScanAuth(auth, qoderDeviceForAccount(auth), QoderQuotaScopeKey(snap))
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
		// 记下今天这条活动实例 id：号池里其它号（看不到这条活动的）要靠它去
		// 向服务端要一个真实判定。
		if res.ClaimCID != "" && snap.ClaimCID != res.ClaimCID {
			snap.ClaimCID = res.ClaimCID
			_ = SaveQoderPool(pool)
		}
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
		// 每日额度按**账号归属的 scope**（同一实名/租户）给，不按设备
		// （实测 2026-11-02：伪造整套 Cosy-* 设备头也不改变判定；同组另一个账号
		// 领过后，服务端回 failureCode=SAME_PERSON_ALREADY_CLAIMED）。
		// **同一额度组**里今天已有其它账号的可信领取记录、而本账号看不到签到活动时，
		// 如实说明并点名是谁领的 —— 这与笼统的"活动未下发"是两回事，用户需要分得清。
		if res.Phase == "pending" && !trusted {
			if holder, ok := QuotaHolderInGroup(email); ok {
				row.DeviceQuotaUsed = true
				row.QuotaTakenBy = holder
				row.Msg = "今日每日额度已被同额度组（同一实名）的账号 " + holder +
					" 领取（服务端按账号归属的实名给，同组一天只有一份、先打先得）"
			}
		}
		out[i] = row
		}(i, j)
	}
	wg.Wait()
	// 额度组归属：标出每组里今天**谁**领到了那份每日额度（同组其它号的
	// QuotaTakenBy 一并补上，面板就能说清「这个号为什么没签上」）。
	holder := map[string]string{}
	for _, r := range out {
		if r.Phase != "claimed" || !r.CheckedInToday {
			continue
		}
		g := qoderQuotaGroupKeyOf(r.Scope)
		if _, ok := holder[g]; !ok {
			holder[g] = r.Email
		}
	}
	for i := range out {
		r := &out[i]
		if r.QuotaTakenBy != "" {
			continue
		}
		g := qoderQuotaGroupKeyOf(r.Scope)
		if h, ok := holder[g]; ok && !strings.EqualFold(h, r.Email) {
			r.QuotaTakenBy = h
		}
	}
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
			return qoderUsageAuth(cloneQoderAuth(snap.Auth), QoderDeviceInfo(), QoderQuotaScopeKey(snap))
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

// claimStamp 本次领取的真实时刻：优先用上游返回的 claimedAt，拿不到才回落本机时钟。
//
// 为什么在意这个：签到记录的可信判定要比较"这条记录晚于今天活动下发"，两边都用
// 本机时钟时，机器钟一慢（实测慢 8 小时）真领到的记录就被判成旧版误报，面板显示
// "没签上"、重试循环整天空跑。上游给的时刻不受本机时钟影响。
func claimStamp(res *QoderCheckinResult) time.Time {
	if res != nil {
		if t, err := time.Parse(time.RFC3339, strings.TrimSpace(res.ClaimedAt)); err == nil {
			return t
		}
	}
	return time.Now()
}
