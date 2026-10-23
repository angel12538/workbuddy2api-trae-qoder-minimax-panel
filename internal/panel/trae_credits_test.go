package panel

import (
	"path/filepath"
	"testing"
)

// TestTraeRecordCredits 面板自建积分快照：**同日重复记录按账号覆盖，不累加**。
//
// 为什么要这条断言：手动「一键签到」与自动排程可能同一天都跑一遍。若按累加记，
// 同一天的数字会翻倍，趋势图直接失真。
func TestTraeRecordCredits(t *testing.T) {
	dir := t.TempDir()
	p := &Panel{cfg: Config{ConfigPath: filepath.Join(dir, "config.json")}}

	// 第一次：两个账号
	p.traeRecordCredits([]traeCreditsEntry{
		{UserID: "u1", Name: "a", Earned: 100},
		{UserID: "u2", Name: "b", Earned: 150},
	})
	// 同日第二次：u1 覆盖为 120（模拟重复签到），u3 新增
	p.traeRecordCredits([]traeCreditsEntry{
		{UserID: "u1", Name: "a", Earned: 120},
		{UserID: "u3", Name: "c", Earned: 50},
	})

	f := p.loadTraeCreditsSelf()
	if len(f.Daily) != 1 {
		t.Fatalf("应只有 1 天，got %d", len(f.Daily))
	}
	day := f.Daily[0]
	if len(day.Accounts) != 3 {
		t.Fatalf("应有 3 个账号，got %d", len(day.Accounts))
	}
	for _, e := range day.Accounts {
		if e.UserID == "u1" && e.Earned != 120 {
			t.Errorf("u1 应被覆盖为 120（不是 100+120），got %v", e.Earned)
		}
	}
	if day.Total != 320 { // 120+150+50
		t.Errorf("当日合计应为 320，got %v", day.Total)
	}
	if day.At == "" {
		t.Error("应记录写入时刻")
	}
}

// TestTraeRecordCreditsEmptyNoop 空列表不应产生任何落盘（避免无谓 IO）。
func TestTraeRecordCreditsEmptyNoop(t *testing.T) {
	dir := t.TempDir()
	p := &Panel{cfg: Config{ConfigPath: filepath.Join(dir, "config.json")}}
	p.traeRecordCredits(nil)
	if f := p.loadTraeCreditsSelf(); len(f.Daily) != 0 {
		t.Errorf("空列表不应写入任何日期，got %+v", f.Daily)
	}
}

// TestTraeCreditsSelfPathAbs 快照路径必须是**绝对路径**。
//
// 本机踩过同类坑：ConfigPath 是相对路径（"config.json"），相对路径在不同 cwd 下
// 会落到意料之外的位置（Chrome --user-data-dir 那轮污染过 Program Files）。
func TestTraeCreditsSelfPathAbs(t *testing.T) {
	p := &Panel{cfg: Config{ConfigPath: "config.json"}}
	got := p.traeCreditsPath()
	if !filepath.IsAbs(got) {
		t.Errorf("应为绝对路径，got %s", got)
	}
	if filepath.Base(got) != "trae_credits.json" {
		t.Errorf("文件名应为 trae_credits.json，got %s", got)
	}
}

// TestLoadTraeCreditsSelfMissing 文件不存在时返回空结构（不报错、不 panic）。
func TestLoadTraeCreditsSelfMissing(t *testing.T) {
	p := &Panel{cfg: Config{ConfigPath: filepath.Join(t.TempDir(), "config.json")}}
	f := p.loadTraeCreditsSelf()
	if f == nil || len(f.Daily) != 0 {
		t.Errorf("缺文件应返回空结构，got %+v", f)
	}
}
