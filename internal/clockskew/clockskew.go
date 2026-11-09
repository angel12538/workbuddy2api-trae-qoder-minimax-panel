// Package clockskew 保存「本机时钟相对上游的偏差」，供各处需要跟上游比时间的判定使用。
//
// 【为什么需要它】
//   这台机器实测钟比上游快 8 小时（RTC 里存的是 UTC，Windows 当本地时间用）。
//   凡是拿**本地时间**去和**上游给的时间**比较的判定，都会被这 8 小时带偏：
//     · CodeArts 的票据临期判定（ticket_expired）→ 提前 8 小时判死 →
//       静默续期直接被跳过 → 用户看到的就是「华为每次都要重新认证」；
//     · 每日幂等键、自然日边界、签到可信线同理。
//
//   偏差本身已经由 appserver 的时钟核对闸测出来了（用上游 HTTP Date 头），
//   这里只把它共享出来，让「会话临期」这类判定用**校准后的时间**。
//
// 【符号约定】
//   skew = 上游时间 - 本机时间。正值 = 本机慢了（要往后加）。
package clockskew

import (
	"sync/atomic"
	"time"
)

var skewNanos atomic.Int64

// SetSkew 记录一次测得的偏差（上游 - 本机）。
func SetSkew(d time.Duration) {
	skewNanos.Store(int64(d))
}

// Skew 返回最近一次记录的偏差（没记录过就是 0）。
func Skew() time.Duration {
	return time.Duration(skewNanos.Load())
}

// Now 返回**校准后**的当前时间：time.Now() + skew。
//
// 偏差没测过时（skew=0）它就等于本机时间 —— 行为与以前完全一致，
// 不会因为这个包的存在改变任何既有语义。
func Now() time.Time {
	return time.Now().Add(Skew())
}

// SkewKnown 是否已经测到过偏差（用于决定要不要在界面上提示）。
func SkewKnown() bool {
	return skewNanos.Load() != 0
}
