package account

import "testing"

func TestEffectiveQueueWaitFirstToken(t *testing.T) {
	// 未设置（nil）→ 默认开
	s := &Settings{}
	if !s.EffectiveQueueWaitFirstToken() {
		t.Error("nil should default to true (ON)")
	}
	// 显式 true
	tr := true
	s.QueueWaitFirstToken = &tr
	if !s.EffectiveQueueWaitFirstToken() {
		t.Error("explicit true should be true")
	}
	// 显式 false
	fa := false
	s.QueueWaitFirstToken = &fa
	if s.EffectiveQueueWaitFirstToken() {
		t.Error("explicit false should be false")
	}
}
