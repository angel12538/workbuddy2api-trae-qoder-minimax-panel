// trae_credits.go 面板**自建**的 Trae 积分快照（每次签到后记录，独立于助手）。
//
// 为什么自建：上游没有「查余额」接口（余额只在聊天流的 cn_credits_remain_info 里
// 推送），而助手留下的 credits_daily.json 在签到搬到面板后就不再更新——趋势图会
// 永远停在最后一次使用助手的日期。
//
// 口径：**只记录面板能准确知道的事** —— 签到成功时的「可得额度」（status 接口的
// credits 字段，助手 auto_checkin.py 注释原文即「签到可获得的积分额度」）。
// 余额与消耗上游不提供，这里**不做推测**，避免给出看着像真数据的假数字。
package panel

import (
	"encoding/json"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

// traeCreditsEntry 某天某账号的签到所得。
type traeCreditsEntry struct {
	UserID string  `json:"user_id"`
	Name   string  `json:"name,omitempty"`
	Earned float64 `json:"earned"`
}

// traeCreditsDay 某一天的签到汇总（面板口径）。
type traeCreditsDay struct {
	Date     string             `json:"date"`
	At       string             `json:"at"`    // 最近一次记录时刻
	Total    float64            `json:"total"` // 当日签到所得合计
	Accounts []traeCreditsEntry `json:"accounts"`
}

// traeCreditsSelfFile 面板自建快照的落盘结构。
type traeCreditsSelfFile struct {
	Daily []traeCreditsDay `json:"daily"`
}

// traeCreditsKeepDays 快照保留天数（超出即裁掉最旧的）。
const traeCreditsKeepDays = 90

// traeCreditsMu 串行化快照读改写（手动签到与自动排程可能同时落盘）。
var traeCreditsMu sync.Mutex

// traeCreditsPath 面板自建快照的路径：config.json 同级的 data/ 下。
//
// ⚠️ 必须转绝对路径：ConfigPath 常是相对路径（"config.json"），
// 相对路径在不同 cwd 下会落到意料之外的位置（本机在 Chrome 那轮踩过同类坑）。
func (p *Panel) traeCreditsPath() string {
	base := strings.TrimSpace(p.cfg.ConfigPath)
	if base == "" {
		return filepath.Join(os.TempDir(), "wb2api", "trae_credits.json")
	}
	dir := filepath.Dir(base)
	if abs, err := filepath.Abs(dir); err == nil {
		dir = abs
	}
	return filepath.Join(dir, "data", "trae_credits.json")
}

// loadTraeCreditsSelf 读面板自建快照；文件不存在/损坏返回空结构（不报错）。
func (p *Panel) loadTraeCreditsSelf() *traeCreditsSelfFile {
	fp := p.traeCreditsPath()
	raw, err := os.ReadFile(fp)
	if err != nil {
		return &traeCreditsSelfFile{}
	}
	var f traeCreditsSelfFile
	if err := json.Unmarshal(raw, &f); err != nil {
		log.Printf("panel: Trae 自建积分快照解析失败（按空处理）: %v", err)
		return &traeCreditsSelfFile{}
	}
	return &f
}

// saveTraeCreditsSelfLocked 原子落盘（tmp + rename），并裁剪超期数据。调用方持锁。
func (p *Panel) saveTraeCreditsSelfLocked(f *traeCreditsSelfFile) {
	// 按日期排序 + 裁剪最旧的
	sort.Slice(f.Daily, func(i, j int) bool { return f.Daily[i].Date < f.Daily[j].Date })
	if len(f.Daily) > traeCreditsKeepDays {
		f.Daily = f.Daily[len(f.Daily)-traeCreditsKeepDays:]
	}
	fp := p.traeCreditsPath()
	if err := os.MkdirAll(filepath.Dir(fp), 0o755); err != nil {
		log.Printf("panel: Trae 自建积分快照建目录失败: %v", err)
		return
	}
	raw, err := json.MarshalIndent(f, "", "  ")
	if err != nil {
		log.Printf("panel: Trae 自建积分快照序列化失败: %v", err)
		return
	}
	tmp := fp + ".tmp"
	if err := os.WriteFile(tmp, raw, 0o600); err != nil {
		log.Printf("panel: Trae 自建积分快照写入失败: %v", err)
		return
	}
	if err := os.Rename(tmp, fp); err != nil {
		log.Printf("panel: Trae 自建积分快照落盘失败: %v", err)
	}
}

// traeRecordCredits 把一次签到中「成功获得」的账号写进当天快照。
//
// 同日多次签到（手动 + 自动排程）按**账号**覆盖，不累加——否则同一天重复签到会把
// 数字翻倍。已签到（already）的账号不计入：那次的额度属于更早的那一次签到。
func (p *Panel) traeRecordCredits(earned []traeCreditsEntry) {
	if len(earned) == 0 {
		return
	}
	traeCreditsMu.Lock()
	defer traeCreditsMu.Unlock()

	f := p.loadTraeCreditsSelf()
	today := time.Now().Format("2006-01-02")
	nowStr := time.Now().Format("2006-01-02T15:04:05")

	var day *traeCreditsDay
	for i := range f.Daily {
		if f.Daily[i].Date == today {
			day = &f.Daily[i]
			break
		}
	}
	if day == nil {
		f.Daily = append(f.Daily, traeCreditsDay{Date: today})
		day = &f.Daily[len(f.Daily)-1]
	}
	// 按 user_id 覆盖合并
	idx := map[string]int{}
	for i, e := range day.Accounts {
		idx[e.UserID] = i
	}
	for _, e := range earned {
		if i, ok := idx[e.UserID]; ok {
			day.Accounts[i] = e
		} else {
			day.Accounts = append(day.Accounts, e)
			idx[e.UserID] = len(day.Accounts) - 1
		}
	}
	day.Total = 0
	for _, e := range day.Accounts {
		day.Total += e.Earned
	}
	day.At = nowStr
	p.saveTraeCreditsSelfLocked(f)
	log.Printf("panel: Trae 积分快照已记录 %s：%d 个账号，合计 %g", today, len(day.Accounts), day.Total)
}
