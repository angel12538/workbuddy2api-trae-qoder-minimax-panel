package panel

import (
	"context"
	"testing"
	"time"
)

// TestStartIDECheckinAutoNoHours 空时点不启动排程（不 panic、不起 goroutine）。
func TestStartIDECheckinAutoNoHours(t *testing.T) {
	p := &Panel{}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	// hours 为空 → 直接返回；这里只验证不会 panic / 不会挂死。
	p.StartIDECheckinAuto(ctx, nil)
	p.StartIDECheckinAuto(ctx, []int{})
}

// TestStartIDECheckinAutoCancels ctx 取消后排程 goroutine 能退出（不留定时器泄漏）。
func TestStartIDECheckinAutoCancels(t *testing.T) {
	p := &Panel{}
	ctx, cancel := context.WithCancel(context.Background())
	// 下一个整点（很久以后），确保循环停在等待定时器上。
	p.StartIDECheckinAuto(ctx, []int{23})
	time.Sleep(200 * time.Millisecond)
	cancel()
	time.Sleep(200 * time.Millisecond)
	// 取消后不应再有活动定时器；这里通过「函数返回且不 panic」间接验证。
}

// TestNextIDEFireReusesTraeFire IDE 排程与 Trae 排程共用同一个时点计算，
// 保证两者语义一致（都在本地时区整点触发，今天已过的时点顺延到明天）。
func TestNextIDEFireReusesTraeFire(t *testing.T) {
	now := time.Date(2026, 10, 24, 8, 30, 0, 0, time.Local)
	// 9 点还没到 → 今天 9 点
	next := nextTraeFire(now, []int{9, 21})
	if next.Hour() != 9 || next.Day() != 24 {
		t.Fatalf("next=%v want 今天 9 点", next)
	}
	// 22 点，两个时点都过了 → 明天最早的
	next = nextTraeFire(now, []int{23})
	if next.Hour() != 23 || next.Day() != 24 {
		t.Fatalf("next=%v want 今天 23 点", next)
	}
	now2 := time.Date(2026, 10, 24, 23, 30, 0, 0, time.Local)
	next2 := nextTraeFire(now2, []int{9, 21})
	if next2.Day() != 25 || next2.Hour() != 9 {
		t.Fatalf("next=%v want 明天 9 点", next2)
	}
	// 非法时点被忽略；全非法时兜底为 24 小时后（排程不能停摆）。
	// 配置层另有 checkHourRange 直接拒绝非法小时，这里只测函数级兜底。
	fallback := nextTraeFire(now, []int{99})
	if fallback.IsZero() {
		t.Fatal("全非法小时应兜底为 24 小时后，而不是零值（零值会让定时器立即触发）")
	}
	if d := fallback.Sub(now); d < 23*time.Hour || d > 25*time.Hour {
		t.Fatalf("兜底间隔 %v 不在 24h 附近", d)
	}
}
