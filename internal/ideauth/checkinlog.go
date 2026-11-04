package ideauth

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

// 签到日期记录：面板无法可靠地向上游「查询今天是否已签到」，因此对每一次成功的签到
// （无论 claimed 还是 already）落一条「uid → 时刻」。UI 据此把已签到账号的按钮变灰。
//
// 记录只代表「本面板观察到该账号在今天完成过签到」，不代表上游的权威状态。
//
// v2 起存完整时间戳（RFC3339）而不是纯日期：Qoder 的每日活动在**上午 10:00**
// （UTC+8）才刷新，此前的「已签」只可能来自昨天的陈旧活动——重试循环与扫描
// 需要知道「这条记录是几点写的」，才能区分「10 点前的可疑记录」与「10 点后的
// 可信记录」。旧格式（纯日期字符串）仍然兼容读取。

var checkinLogMu sync.Mutex

type checkinLog struct {
	// WorkBuddy / Qoder / 豆包共用一个文件，按 "kind:key" 分桶。
	// 值为 RFC3339 时间戳；历史文件可能是纯日期（2006-01-02）。
	Entries map[string]string `json:"entries"`
	// Claims 记录**真实领取**（phase=claimed）的时刻，与 Entries 分开。
	//
	// 为什么分开：Entries 记的是「今天签过」（claimed 与 already 都算），用来
	// 展示；Claims 记的是「这一天真正把 100 积分领到手」的时刻。Qoder 的每日
	// 额度服务端按**人**给（同一人多个账号一天只有一份，见 qoder_pool_checkin.go
	// 的实测记录），所以「让哪个号先打」决定了谁拿到那 100 积分 —— 要做公平
	// 轮换，就必须知道每个号上次真正领到是什么时候。
	Claims map[string]string `json:"claims,omitempty"`
}

func checkinLogPath() string {
	return filepath.Join(PoolDir(), "checkin_log.json")
}

func loadCheckinLog() map[string]string {
	raw, err := os.ReadFile(checkinLogPath())
	if err != nil {
		return map[string]string{}
	}
	var f checkinLog
	if json.Unmarshal(raw, &f) != nil || f.Entries == nil {
		return map[string]string{}
	}
	return f.Entries
}

// loadCheckinLogFile 读整个日志文件（entries + claims）。
func loadCheckinLogFile() checkinLog {
	var f checkinLog
	raw, err := os.ReadFile(checkinLogPath())
	if err != nil {
		return checkinLog{Entries: map[string]string{}, Claims: map[string]string{}}
	}
	if json.Unmarshal(raw, &f) != nil {
		return checkinLog{Entries: map[string]string{}, Claims: map[string]string{}}
	}
	if f.Entries == nil {
		f.Entries = map[string]string{}
	}
	if f.Claims == nil {
		f.Claims = map[string]string{}
	}
	return f
}

// saveCheckinLogFile 写回整个日志文件。
func saveCheckinLogFile(f checkinLog) error {
	buf, err := json.MarshalIndent(f, "", "  ")
	if err != nil {
		return err
	}
	return atomicWrite(checkinLogPath(), buf)
}

// MarkCheckedIn 记录某签到对象今日已签到。kind ∈ {workbuddy, qoder, doubao}。
func MarkCheckedIn(kind, key string) error {
	return MarkCheckedInAt(kind, key, time.Now())
}

// MarkCheckedInAt 同 MarkCheckedIn，但由调用方给时刻（上游返回的真实领取时刻）。
//
// 为什么需要带时刻：可信判定要看"这条记录是不是今天活动下发之后写的"，用本机时钟
// 写的记录，在时钟不准的机器上会把**真领到的那次**判成旧版误报（实测这台机器钟慢
// 8 小时：12:39 真领到，本地记成 04:39 → 落在 10:05 线前 → 面板整天说没签上）。
// 上游给的时刻不受本机时钟影响。
func MarkCheckedInAt(kind, key string, at time.Time) error {
	key = strings.TrimSpace(key)
	if key == "" {
		return nil
	}
	if at.IsZero() {
		at = time.Now()
	}
	checkinLogMu.Lock()
	defer checkinLogMu.Unlock()
	f := loadCheckinLogFile()
	f.Entries[kind+":"+key] = at.Format(time.RFC3339)
	return saveCheckinLogFile(f)
}

// MarkClaimed 记录某账号**真正领到**积分的时刻（phase=claimed，不是 already）。
func MarkClaimed(kind, key string) error {
	return MarkClaimedAt(kind, key, time.Now())
}

// MarkClaimedAt 同 MarkClaimed，时刻由调用方给（上游返回的 claimedAt 优先）。
//
// 用途：Qoder 的每日额度服务端按「人」给，同一人的多个账号一天只有一份，
// 谁先打谁拿到。轮换排序与可信判定都用这个时刻，所以它必须是真实时刻。
func MarkClaimedAt(kind, key string, at time.Time) error {
	key = strings.TrimSpace(key)
	if key == "" {
		return nil
	}
	if at.IsZero() {
		at = time.Now()
	}
	checkinLogMu.Lock()
	defer checkinLogMu.Unlock()
	f := loadCheckinLogFile()
	f.Claims[kind+":"+key] = at.Format(time.RFC3339)
	return saveCheckinLogFile(f)
}

// LastClaimedAt 返回某账号最后一次**真正领到**积分的时刻（没有记录返回 false）。
//
// 契约 C-QD-LEDGER@1：Qoder 的 Claims 只记「最后一次真正领取」一条，若它缺失
// （换了机器 / 文件被清理 / 旧版没写），回落到逐日领取账本里最近一条
// phase=claimed 的记录 —— 轮换排序依赖「上次真正领到是什么时候」，不能让
// 账本里明明有的历史被忽略。
func LastClaimedAt(kind, key string) (time.Time, bool) {
	key = strings.TrimSpace(key)
	checkinLogMu.Lock()
	f := loadCheckinLogFile()
	stamp := f.Claims[kind+":"+key]
	checkinLogMu.Unlock()
	if t, ok := parseCheckinStamp(stamp); ok {
		return t, true
	}
	if strings.EqualFold(strings.TrimSpace(kind), "qoder") {
		return qoderLedgerLastClaimed(key)
	}
	return time.Time{}, false
}

// UnmarkCheckedIn 撤销某签到对象的「今日已签到」记录。
//
// 什么时候需要：服务端明确答复「同一个人今天已经领过了」时，说明**本号**今天
// 并没有签到 —— 之前若有本地记录，那是误记（例如把被服务端拒绝的领取也写进了
// 记录），必须清掉，否则面板会显示成"今日已签到"（假签到）。
func UnmarkCheckedIn(kind, key string) error {
	key = strings.TrimSpace(key)
	if key == "" {
		return nil
	}
	checkinLogMu.Lock()
	defer checkinLogMu.Unlock()
	f := loadCheckinLogFile()
	k := kind + ":" + key
	if _, ok := f.Entries[k]; !ok {
		return nil
	}
	delete(f.Entries, k)
	delete(f.Claims, k)
	return saveCheckinLogFile(f)
}

// parseCheckinStamp 解析记录值：RFC3339 时间戳优先，纯日期回落当日 00:00。
func parseCheckinStamp(v string) (time.Time, bool) {
	v = strings.TrimSpace(v)
	if v == "" {
		return time.Time{}, false
	}
	if t, err := time.Parse(time.RFC3339, v); err == nil {
		return t, true
	}
	if t, err := time.ParseInLocation("2006-01-02", v, time.Local); err == nil {
		return t, true
	}
	return time.Time{}, false
}

// CheckedInToday 报告某签到对象今日是否已签到（按平台时区基准的自然日）。
func CheckedInToday(kind, key string) bool {
	t, ok := CheckedInAt(kind, key)
	if !ok {
		return false
	}
	return sameCheckinDay(kind, t)
}

// CheckedInAt 返回最近一次签到记录的时刻。历史纯日期记录回落为当日 00:00。
func CheckedInAt(kind, key string) (time.Time, bool) {
	checkinLogMu.Lock()
	defer checkinLogMu.Unlock()
	t, ok := parseCheckinStamp(loadCheckinLog()[kind+":"+strings.TrimSpace(key)])
	return t, ok
}

// CheckinRecordTrusted 报告本地签到记录是否「可信」。
//
// 「可信」的分界线**按平台区分**（见 checkinTrustLine）：各平台活动刷新时刻
// 不同，用一条线套所有平台会误伤。
func CheckinRecordTrusted(kind, key string) bool {
	// **真领到**的记录优先：Claims 只在领取成功（phase=claimed，且领的是今天下发的
	// 活动）时写入，是硬证据；Entries 那条时刻线只是防「昨天的陈旧活动被当成今天
	// 已领」的保守判据，本机时钟一慢就会把真领到的号误杀（实测这台机器钟慢 8 小时：
	// 真领到的那次被记成 04:39，落在 10:05 线前 → 面板整天显示「没签上」）。
	if c, ok := LastClaimedAt(kind, key); ok && sameCheckinDay(kind, c) {
		return true
	}
	t, ok := CheckedInAt(kind, key)
	if !ok {
		return false
	}
	if !sameCheckinDay(kind, t) {
		return false // 非今日记录一律不可信（已过期）
	}
	return !t.Before(checkinTrustLine(kind))
}

// checkinDayLoc 该平台的「今天」用哪个时区基准。
//
// Qoder 的每日活动按 **UTC+8** 刷新（campaignId 每日本地 10:00 才下发），所以
// 「今天」与「10:05 可信线」都必须按 UTC+8 判定。原先统一用机器本地时区：在
// 本机（UTC+8）恰好正确，问题被完全掩盖；换到 UTC 机器（CI / 海外服务器）上
// 整条线会偏 8 小时——11:00 (UTC+8) 写下的**真实**记录会被判成「线前」→ 不可信。
// 其余平台按自然日刷新，继续用本地时区。
func checkinDayLoc(kind string) *time.Location {
	if strings.EqualFold(strings.TrimSpace(kind), "qoder") {
		return qoderCST
	}
	return time.Local
}

// sameCheckinDay 这条时刻是否落在该平台的「今天」（按平台时区基准的自然日）。
func sameCheckinDay(kind string, t time.Time) bool {
	loc := checkinDayLoc(kind)
	return t.In(loc).Format("2006-01-02") == time.Now().In(loc).Format("2006-01-02")
}

// checkinTrustLine 本地签到记录的可信分界线（今日本地时间的时刻）。
//
// 分平台的原因：
//   - Qoder：每日活动 10:00 (UTC+8) 刷新。10:05 前写下的记录可能只是**昨天**
//     那波还没刷新的陈旧活动（旧版把「昨天已领」当成「今天已领」写进记录），
//     所以要等到 10:05 真活动下发后才可信。带时间戳的记录让我们能把这些历史
//     假记录自动降级为「可疑」——重试循环会继续巡检，旧假记录不阻塞任何流程。
//   - 豆包 / WorkBuddy / CodeArts / MiniMax 等：每日签到按**自然日**刷新
//     （零点起算），上游接口直接给出 checked_in_today / last_checkin_date，
//     不存在「昨天的活动冒充今天」的问题。对它们套用 10:05 线会让**上午就
//     签到成功**的号在面板上一整天显示「未签」——用户看到的就是「签到失效」
//     （2026-10-27 实测：08:47 豆包两个号签到成功，号池列表仍返回
//     checked_in_today=false，按钮一直是蓝底可点）。
func checkinTrustLine(kind string) time.Time {
	loc := checkinDayLoc(kind)
	now := time.Now().In(loc)
	switch strings.ToLower(strings.TrimSpace(kind)) {
	case "qoder":
		// Qoder 的活动 10:00 (UTC+8) 刷新 → 可信线是 UTC+8 的 10:05（不是机器本地时间，
		// 否则 UTC 机器上整条线偏 8 小时，见 checkinDayLoc 注释）。
		return time.Date(now.Year(), now.Month(), now.Day(), 10, 5, 0, 0, loc)
	default:
		// 自然日刷新：今日零点起就算可信。
		return time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, loc)
	}
}

// qoderCheckinTrustLine Qoder 本地签到记录的可信分界线（今日 10:05 UTC+8）。
func qoderCheckinTrustLine() time.Time {
	return checkinTrustLine("qoder")
}

// CheckinDates 返回 kind 下全部 key → 日期 映射（用于批量列表渲染）。
func CheckinDates(kind string) map[string]string {
	checkinLogMu.Lock()
	defer checkinLogMu.Unlock()
	out := map[string]string{}
	prefix := kind + ":"
	for k, v := range loadCheckinLog() {
		if strings.HasPrefix(k, prefix) {
			if t, ok := parseCheckinStamp(v); ok {
				out[strings.TrimPrefix(k, prefix)] = t.Local().Format("2006-01-02")
			} else {
				out[strings.TrimPrefix(k, prefix)] = v
			}
		}
	}
	return out
}

// ---------------------------------------------------------------------------
// Qoder 逐日领取账本（契约 C-QD-LEDGER@1）
//
// 为什么需要独立文件：checkin_log.json 的 Entries 只记「今天签过」，Claims 只记
// 「最后一次真正领到」的一条 —— 前者会被第二天的记录盖掉，后者根本没有逐日轨迹。
// 用户问「这个号为什么今天没领到 / 下次轮到谁」时，需要的是**逐日、逐号的尝试
// 与结果**（含 blocked/pending/failed 这些没领到的尝试），而不是一行当前状态。
//
// 文件：<PoolDir>/qoder_claim_history.json，0600，原子写。
//   - 按 (email, date) 覆盖：同一天多次尝试只留一条最新结果；
//   - 粘性：当天一旦有过 phase=claimed，后续 blocked/pending/failed **不得**覆盖
//     它（只能更新 strategy/quota_taken_by/at），否则轮换依据会被后一次失败擦掉；
//   - 保留 180 天；超过 5000 条按 at 淘汰最旧。
//
// 注意：本文件与 checkin_log.json 是两套语义，互不覆盖 —— 旧文件保持原样，
// 新账本只做追加/覆盖审计，不改既有签到记录的行为。
// ---------------------------------------------------------------------------

const (
	// qoderClaimHistoryVersion 账本文件版本（契约固定为 1）。
	qoderClaimHistoryVersion = 1
	// qoderClaimHistoryKeepDays 账本保留天数（契约 180 天）。
	qoderClaimHistoryKeepDays = 180
	// qoderClaimHistoryMaxRecords 账本条数上限（超过按 at 淘汰最旧）。
	qoderClaimHistoryMaxRecords = 5000
)

// QoderClaimRecord 一条逐日领取记录（契约 C-QD-LEDGER@1）。
type QoderClaimRecord struct {
	Email        string  `json:"email"`
	Date         string  `json:"date"`  // UTC+8 的 YYYY-MM-DD
	Phase        string  `json:"phase"` // claimed|already|blocked|pending|failed|login_required|error
	Credits      float64 `json:"credits"`
	CampaignID   string  `json:"campaign_id,omitempty"`
	GrantID      string  `json:"grant_id,omitempty"`
	QuotaTakenBy string  `json:"quota_taken_by,omitempty"`
	Strategy     string  `json:"strategy,omitempty"`
	At           string  `json:"at"`
}

// qoderClaimHistoryFile 账本文件结构。
type qoderClaimHistoryFile struct {
	Version int                `json:"version"`
	Records []QoderClaimRecord `json:"records"`
}

// qoderClaimPhases 七相枚举（契约 C-QD-API@1，账本 phase 同集合）。
var qoderClaimPhases = map[string]bool{
	"claimed":        true,
	"already":        true,
	"blocked":        true,
	"pending":        true,
	"failed":         true,
	"login_required": true,
	"error":          true,
}

// qoderClaimHistoryMu 串行化账本的「读-改-写」。与 checkinLogMu 分开：
// 两个文件独立，混用一把锁会让签到路径与轮换读互相阻塞。
var qoderClaimHistoryMu sync.Mutex

// qoderClaimHistoryPath 账本文件路径（契约固定文件名）。
func qoderClaimHistoryPath() string {
	return filepath.Join(PoolDir(), "qoder_claim_history.json")
}

// qoderClaimDay 把时刻转成 UTC+8 的 YYYY-MM-DD（契约 date 口径）。
func qoderClaimDay(t time.Time) string {
	return t.In(qoderCST).Format("2006-01-02")
}

// loadQoderClaimHistoryLocked 读账本（调用方须持 qoderClaimHistoryMu）。
//
// 文件不存在 → 空账本；损坏 → 也按空账本处理（账本是审计/轮换的辅助数据，
// 不能因为一个坏掉的文件把整条签到链路打死；下一次写入会重建一个合法文件）。
func loadQoderClaimHistoryLocked() qoderClaimHistoryFile {
	empty := qoderClaimHistoryFile{
		Version: qoderClaimHistoryVersion,
		Records: []QoderClaimRecord{},
	}
	raw, err := os.ReadFile(qoderClaimHistoryPath())
	if err != nil || len(strings.TrimSpace(string(raw))) == 0 {
		return empty
	}
	var f qoderClaimHistoryFile
	if json.Unmarshal(raw, &f) != nil {
		return empty
	}
	if f.Version == 0 {
		f.Version = qoderClaimHistoryVersion
	}
	if f.Records == nil {
		f.Records = []QoderClaimRecord{}
	}
	return f
}

// saveQoderClaimHistoryLocked 原子写回账本（0600；atomicWrite 临时文件即 0600）。
func saveQoderClaimHistoryLocked(f qoderClaimHistoryFile) error {
	f.Version = qoderClaimHistoryVersion
	if f.Records == nil {
		f.Records = []QoderClaimRecord{}
	}
	b, err := json.MarshalIndent(f, "", "  ")
	if err != nil {
		return err
	}
	return atomicWrite(qoderClaimHistoryPath(), b)
}

// normalizeQoderClaimRecord 规范化并校验一条记录。
func normalizeQoderClaimRecord(rec QoderClaimRecord) (QoderClaimRecord, error) {
	rec.Email = normalizeQoderEmail(rec.Email)
	if rec.Email == "" {
		return rec, fmt.Errorf("领取记录缺少 email")
	}
	rec.Phase = strings.ToLower(strings.TrimSpace(rec.Phase))
	if !qoderClaimPhases[rec.Phase] {
		return rec, fmt.Errorf("领取记录 phase 非法: %q（只允许七相枚举）", rec.Phase)
	}
	at := strings.TrimSpace(rec.At)
	if at != "" {
		if t, err := time.Parse(time.RFC3339, at); err == nil {
			at = t.In(qoderCST).Format(time.RFC3339)
			if strings.TrimSpace(rec.Date) == "" {
				rec.Date = qoderClaimDay(t)
			}
		} else {
			return rec, fmt.Errorf("领取记录 at 非法（需要 RFC3339）: %q", rec.At)
		}
	} else {
		n := time.Now().In(qoderCST)
		at = n.Format(time.RFC3339)
		if strings.TrimSpace(rec.Date) == "" {
			rec.Date = n.Format("2006-01-02")
		}
	}
	rec.Date = strings.TrimSpace(rec.Date)
	if _, err := time.ParseInLocation("2006-01-02", rec.Date, qoderCST); err != nil {
		return rec, fmt.Errorf("领取记录 date 非法（需要 UTC+8 的 YYYY-MM-DD）: %q", rec.Date)
	}
	rec.At = at
	// 契约：只有 claimed/already 才有「本号实际入账」的积分；其余相位一律 0。
	if rec.Phase != "claimed" && rec.Phase != "already" {
		rec.Credits = 0
	}
	if rec.Credits < 0 {
		rec.Credits = 0
	}
	return rec, nil
}

// mergeQoderClaimRecord 把同一天的新结果并入旧记录。
func mergeQoderClaimRecord(old, rec QoderClaimRecord) QoderClaimRecord {
	// 粘性：当天已经真领到过，后续的失败/阻塞不得把它擦掉。
	if old.Phase == "claimed" && rec.Phase != "claimed" {
		old.Strategy = firstNonEmpty(rec.Strategy, old.Strategy)
		old.QuotaTakenBy = firstNonEmpty(rec.QuotaTakenBy, old.QuotaTakenBy)
		old.At = firstNonEmpty(rec.At, old.At)
		return old
	}
	out := rec
	// 新记录没带上的活动/奖励标识保留旧值（claimed 才有 grant/campaign）。
	if strings.TrimSpace(out.CampaignID) == "" {
		out.CampaignID = old.CampaignID
	}
	if strings.TrimSpace(out.GrantID) == "" {
		out.GrantID = old.GrantID
	}
	return out
}

// pruneQoderClaimRecords 按 180 天 / 5000 条淘汰并统一排序。
func pruneQoderClaimRecords(records []QoderClaimRecord, now time.Time) []QoderClaimRecord {
	// 保留 today-179 起（含当天共 180 天）。
	cutoff := qoderClaimDay(now.AddDate(0, 0, -(qoderClaimHistoryKeepDays - 1)))
	out := make([]QoderClaimRecord, 0, len(records))
	for _, r := range records {
		if r.Date != "" && r.Date < cutoff {
			continue
		}
		out = append(out, r)
	}
	if len(out) > qoderClaimHistoryMaxRecords {
		// 按 at 升序淘汰最旧；at 相同则保持原顺序（SliceStable）。
		sort.SliceStable(out, func(i, j int) bool { return out[i].At < out[j].At })
		out = out[len(out)-qoderClaimHistoryMaxRecords:]
	}
	// 文件按 date 升序、同日按 at 升序、再按 email，便于人读与逐日对比。
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Date != out[j].Date {
			return out[i].Date < out[j].Date
		}
		if out[i].At != out[j].At {
			return out[i].At < out[j].At
		}
		return out[i].Email < out[j].Email
	})
	return out
}

// AppendQoderClaim 追加（或按 (email,date) 覆盖）一条领取记录。原子写。
//
// 每次尝试都写（含 blocked/pending/failed/login_required/error），供审计与轮换；
// 只有 phase=claimed 才是轮换依据。
func AppendQoderClaim(rec QoderClaimRecord) error {
	rec, err := normalizeQoderClaimRecord(rec)
	if err != nil {
		return err
	}
	qoderClaimHistoryMu.Lock()
	defer qoderClaimHistoryMu.Unlock()
	f := loadQoderClaimHistoryLocked()
	replaced := false
	for i := range f.Records {
		if strings.EqualFold(f.Records[i].Email, rec.Email) && f.Records[i].Date == rec.Date {
			f.Records[i] = mergeQoderClaimRecord(f.Records[i], rec)
			replaced = true
			break
		}
	}
	if !replaced {
		f.Records = append(f.Records, rec)
	}
	f.Records = pruneQoderClaimRecords(f.Records, time.Now())
	return saveQoderClaimHistoryLocked(f)
}

// QoderClaimHistory 返回某账号最近 days 天的领取记录（date 升序）。
//
// email 为空 = 全部账号；days<=0 = 全部已保留记录（最多 180 天）。
func QoderClaimHistory(email string, days int) []QoderClaimRecord {
	ex := normalizeQoderEmail(email)
	qoderClaimHistoryMu.Lock()
	f := loadQoderClaimHistoryLocked()
	qoderClaimHistoryMu.Unlock()

	cutoff := ""
	if days > 0 {
		if days > qoderClaimHistoryKeepDays {
			days = qoderClaimHistoryKeepDays
		}
		cutoff = qoderClaimDay(time.Now().AddDate(0, 0, -(days - 1)))
	}
	out := make([]QoderClaimRecord, 0, len(f.Records))
	for _, r := range f.Records {
		if ex != "" && !strings.EqualFold(r.Email, ex) {
			continue
		}
		if cutoff != "" && r.Date < cutoff {
			continue
		}
		out = append(out, r)
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Date != out[j].Date {
			return out[i].Date < out[j].Date
		}
		if out[i].At != out[j].At {
			return out[i].At < out[j].At
		}
		return out[i].Email < out[j].Email
	})
	return out
}

// QoderClaimedDates 返回某账号 phase=claimed 的去重日期（升序）。
func QoderClaimedDates(email string, days int) []string {
	records := QoderClaimHistory(email, days)
	seen := map[string]bool{}
	out := make([]string, 0, len(records))
	for _, r := range records {
		if r.Phase != "claimed" || r.Date == "" || seen[r.Date] {
			continue
		}
		seen[r.Date] = true
		out = append(out, r.Date)
	}
	sort.Strings(out)
	return out
}

// QoderRecentClaims 返回全部账号最近 limit 条记录（at 降序），供面板 history 端点用。
// limit<=0 时按契约上限 500 条。
func QoderRecentClaims(limit int) []QoderClaimRecord {
	if limit <= 0 {
		limit = 500
	}
	records := QoderClaimHistory("", 0)
	sort.SliceStable(records, func(i, j int) bool {
		if records[i].At != records[j].At {
			return records[i].At > records[j].At
		}
		if records[i].Date != records[j].Date {
			return records[i].Date > records[j].Date
		}
		return records[i].Email < records[j].Email
	})
	if len(records) > limit {
		records = records[:limit]
	}
	return records
}

// qoderLatestClaimRecords 返回每个账号最近一条账本记录（按 date/at 取最新）。
func qoderLatestClaimRecords() map[string]QoderClaimRecord {
	records := QoderClaimHistory("", 0)
	out := make(map[string]QoderClaimRecord, len(records))
	for _, r := range records {
		key := normalizeQoderEmail(r.Email)
		old, ok := out[key]
		if !ok || r.Date > old.Date || (r.Date == old.Date && r.At > old.At) {
			out[key] = r
		}
	}
	return out
}

// qoderLedgerLastClaimed 返回账本里该账号最近一次 phase=claimed 的时刻。
func qoderLedgerLastClaimed(email string) (time.Time, bool) {
	ex := normalizeQoderEmail(email)
	if ex == "" {
		return time.Time{}, false
	}
	qoderClaimHistoryMu.Lock()
	f := loadQoderClaimHistoryLocked()
	qoderClaimHistoryMu.Unlock()
	var best time.Time
	found := false
	for _, r := range f.Records {
		if !strings.EqualFold(r.Email, ex) || r.Phase != "claimed" {
			continue
		}
		t, err := time.Parse(time.RFC3339, strings.TrimSpace(r.At))
		if err != nil {
			continue
		}
		if !found || t.After(best) {
			best, found = t, true
		}
	}
	return best, found
}

// qoderLedgerLastClaimedRFC3339 返回账本/Claims 里最近一次真领到的时刻（RFC3339，空 = 无）。
func qoderLedgerLastClaimedRFC3339(email string) string {
	if t, ok := LastClaimedAt("qoder", email); ok {
		return t.In(qoderCST).Format(time.RFC3339)
	}
	return ""
}

// qoderDailyClaimCredits Qoder 每日 CLAIM_BENEFIT 的固定面值（实测 benefit.amount=100）。
// 历史补写时若账本里没有任何 claimed 记录可参考面值，用它兜底。
const qoderDailyClaimCredits = 100.0

// QoderBackfillClaimHistoryForToday 补写「今天已领但账本缺失」的 claimed 记录。
//
// 场景（D-Q2）：某号今天确实领到了（checkin_log.json 的 Claims 里有今天的时刻），
// 但那次领取发生在 qoder_claim_history.json 建立之前（或由旧版进程完成），
// 账本当天没有 phase=claimed 记录 —— claim_history_days 因此少算一天。
// 这是**历史数据缺失**，不是统计口径错误，所以补一条真实记录而不是改统计规则。
//
// 规则：
//   - LastClaimedAt("qoder", email) 必须是今天（UTC+8），否则不补；
//   - 账本当天已有 phase=claimed 记录 → 跳过（幂等，不重复写盘）；
//   - credits 取账本里最近一次 claimed 的面值，没有则用每日固定面值；
//   - date=今天、at=Claims 里的真实领取时刻（不是 now），campaign/grant 留空。
//
// 返回是否真的写了一条记录。失败由调用方记日志，不阻断扫描/签到。
func QoderBackfillClaimHistoryForToday(email string) (bool, error) {
	ex := normalizeQoderEmail(email)
	if ex == "" {
		return false, nil
	}
	claimedAt, ok := LastClaimedAt("qoder", ex)
	if !ok || !sameCheckinDay("qoder", claimedAt) {
		return false, nil
	}
	today := qoderClaimDay(time.Now())
	for _, rec := range QoderClaimHistory(ex, 1) {
		if rec.Date == today && rec.Phase == "claimed" {
			return false, nil
		}
	}
	credits := 0.0
	for _, rec := range QoderClaimHistory(ex, 0) { // date 升序：后出现的是更近的一次
		if rec.Phase == "claimed" && rec.Credits > 0 {
			credits = rec.Credits
		}
	}
	if credits <= 0 {
		credits = qoderDailyClaimCredits
	}
	err := AppendQoderClaim(QoderClaimRecord{
		Email:   ex,
		Date:    today,
		Phase:   "claimed",
		Credits: credits,
		At:      claimedAt.In(qoderCST).Format(time.RFC3339),
	})
	if err != nil {
		return false, err
	}
	return true, nil
}
