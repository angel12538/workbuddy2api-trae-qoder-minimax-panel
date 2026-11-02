package bridge

import (
	"testing"
	"time"
)

func TestWaitLogRingBuffer(t *testing.T) {
	w := &waitLog{cap: 3}
	for i := 0; i < 5; i++ {
		w.record(WaitEpisode{Model: "m", Tries: i})
	}
	snap := w.snapshot()
	if len(snap) != 3 {
		t.Fatalf("len = %d, want 3 (ring capacity)", len(snap))
	}
	// 最新在前，且淘汰了最旧两条（Tries 0、1），保留 2、3、4
	if snap[0].Tries != 4 || snap[1].Tries != 3 || snap[2].Tries != 2 {
		t.Errorf("order wrong: got %d,%d,%d want 4,3,2 (newest first)", snap[0].Tries, snap[1].Tries, snap[2].Tries)
	}
}

func TestRecordWaitAndRecent(t *testing.T) {
	RecordWait("smodel", "acct", "global", WaitOutcomeSuccess, 3, 45*time.Second)
	got := RecentWaitLog()
	if len(got) == 0 {
		t.Fatal("RecentWaitLog empty after RecordWait")
	}
	e := got[0]
	if e.Model != "smodel" || e.Account != "acct" || e.Region != "global" ||
		e.Tries != 3 || e.TotalWaitSec != 45 || e.Outcome != WaitOutcomeSuccess {
		t.Errorf("entry = %+v", e)
	}
	if e.Time.IsZero() {
		t.Error("Time should be auto-filled")
	}
}
