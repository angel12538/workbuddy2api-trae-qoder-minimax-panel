package appserver

import (
	"context"
	"errors"
	"log"
	"net/http"
	"sort"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/clockskew"
)

// 本机时钟核对闸。
//
// 为什么必须单独做这一件事：这套面板里所有「每天」的判定都是按**本机时钟**算的 ——
// 签到时点（9/10/21 点）、Qoder 签到记录的 10:05 可信线（ideauth.checkinTrustLine）、
// CodeArts 的每日幂等键（codearts.DailyIdempotentKey 用本地 yyyymmdd）、
// 「今日已签」的自然日比较（ideauth.CheckedInToday）。本机钟只要偏几小时，
// 这些判定就会成片错位：真领到的签到被记成「10:05 前的旧版误报」而整天显示没签上、
// 自动签到在错误的时刻触发、跨天的领取被判成重复。
//
// 实测（2026-11-01）：这台机器钟慢整 8 小时 —— Cloudflare 与 auth.huaweicloud.com
// 都报 `Date: 01 Nov 2026 13:48 GMT`（真实北京 21:48），本机却显示
// `13:48 +0800`、自报 UTC=`05:48`。RTC 里存的是 UTC 而 Windows 把它当本地时间用
// （双系统/WSL 常见口径问题），于是显示出来的"北京时间"比真北京慢 8 小时。
// 上游的 HTTP Date 就是现成的权威时间源，开机和每隔一段时间对一次，偏了就大声报警。
const (
	// clockSkewTolerance 超过这个差值就报警。5 分钟：够躲开网络抖动与秒级偏差，
	// 又远小于任何"自然日/时刻线"判定能承受的误差。
	clockSkewTolerance = 5 * time.Minute
	// clockSkewEvery 复查间隔。系统的 NTP 客户端可能把时间修回来，也可能被别的
	// 程序改错，所以不能只查一次。
	clockSkewEvery = 6 * time.Hour
)

// clockSkewSources 用来核对时间的外部端点（只需要响应头里的 Date，不需要登录态）。
//
// 取三个不同归属的源做中位数：单一服务器不准、或它自己时钟飘了，都不该让面板
// 喊"你的钟坏了"。401/404 也照用 —— Date 头跟业务码无关。
var clockSkewSources = []string{
	"https://openapi.qoder.sh/sash/api/v1/me/campaigns",
	"https://auth.huaweicloud.com/authui/login.html",
	"https://www.cloudflare.com/cdn-cgi/trace",
}

// StartClockSkewGuard 开机核对一次，并定期复查。
func StartClockSkewGuard(ctx context.Context) {
	go func() {
		checkClockSkew(ctx)
		t := time.NewTicker(clockSkewEvery)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				checkClockSkew(ctx)
			}
		}
	}()
}

// checkClockSkew 对一次表：偏差在容差内就报"正常"（好让人核对历史），
// 超容差就把后果讲清楚，而不是只丢一句"时间不对"。
func checkClockSkew(ctx context.Context) {
	skew, n, err := measureClockSkew(ctx)
	if err != nil || n == 0 {
		log.Printf("时钟核对跳过：拿不到任何上游时间源（%v）", err)
		return
	}
	/* 把偏差共享出去：CodeArts 的票据临期判定要用校准后的时间，
	   否则钟快 8 小时会提前把票据判死 → 静默续期被跳过 → 每次都要重新登录。 */
	clockskew.SetSkew(skew)
	abs := skew
	if abs < 0 {
		abs = -abs
	}
	if abs <= clockSkewTolerance {
		log.Printf("时钟核对：本机与上游时间一致（%d 个源，最大偏差 %v）", n, abs.Round(time.Second))
		return
	}
	dir := "快"
	if skew > 0 {
		dir = "慢" // 上游时间 > 本机时间 = 本机慢（skew 是 server-local）
	}
	log.Printf("⚠️⚠️ 本机时钟比上游%s %v（%d 个源核对）——"+
		"「每天几点」的判定会成片错位：签到可信线（今日 10:05）、每日幂等键（本地自然日）、"+
		"自动签到时点、以及会话临期判定都会算错；真领到的签到可能被显示成「没签上」。"+
		"请修正 Windows 系统时间/时区（RTC 口径：双系统或 WSL 把 UTC 写进了硬件时钟），"+
		"否则请把面板时点挪到与真实 UTC+8 对齐的位置。", dir, abs.Round(time.Minute), n)
}

// skewSample 一个源测出来的一次偏差（signed = 上游 - 本机；abs 用于排序取中位）。
type skewSample struct {
	signed time.Duration
	abs    time.Duration
}

// measureClockSkew 返回各源测得偏差的**中位数**（带符号：正=本机慢了）、成功取样的源数、错误。
//
// 每个源算两次（请求前、请求后）取中点当本机时刻，避免把"请求耗时"算成时钟偏差。
func measureClockSkew(ctx context.Context) (time.Duration, int, error) {
	var samples []skewSample
	client := &http.Client{Timeout: 12 * time.Second}
	lastErr := ""
	for _, u := range clockSkewSources {
		select {
		case <-ctx.Done():
			return 0, len(samples), ctx.Err()
		default:
		}
		before := time.Now()
		req, err := http.NewRequest(http.MethodHead, u, nil)
		if err != nil {
			lastErr = err.Error()
			continue
		}
		req.Header.Set("User-Agent", "wb2api-clockguard/1")
		resp, err := client.Do(req)
		if err != nil {
			lastErr = err.Error()
			continue
		}
		after := time.Now()
		_ = resp.Body.Close()
		server, perr := http.ParseTime(resp.Header.Get("Date"))
		if perr != nil {
			lastErr = "上游没给可用的 Date 头: " + u
			continue
		}
		mid := before.Add(after.Sub(before) / 2)
		d := server.Sub(mid)
		abs := d
		if abs < 0 {
			abs = -abs
		}
		samples = append(samples, skewSample{signed: d, abs: abs})
	}
	if len(samples) == 0 {
		if lastErr == "" {
			lastErr = "无可用时间源"
		}
		return 0, 0, errors.New(lastErr)
	}
	// 按绝对值排序取中位：个别源自己时钟飘了不该把结论带偏。
	sort.Slice(samples, func(i, j int) bool { return samples[i].abs < samples[j].abs })
	return samples[len(samples)/2].signed, len(samples), nil
}
