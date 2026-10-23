package pool

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
)

// TestModelCooldownBlocksFallbackPath 全冷却兜底路径同样受**模型级 6004 冷却**约束。
//
// 背景（审查发现）：主选号路径走 healthyForModel（内含 modelCooled），但兜底
// pickEarliestExpiryLocked 此前只判 disabled / CoolHard / floor / inFlight ——
// 于是「健康号全被积分保底拦空」时，兜底会把**正处该模型 6004 冷却**的号选出来，
// 必然再撞一次 6004，白烧一轮账号轮转。兜底是最后一道选号路径，判据漏一项就等于没挡。
func TestModelCooldownBlocksFallbackPath(t *testing.T) {
	withNoPickGap(t)
	p := New("")
	p.SetCostExploreInterval(0)
	p.SetCreditFloor(150)
	p.SetModelRateOf(rates(map[string]map[string]string{"global": {"kimi-k3": "1.62"}}))

	u := &auth.Auth{UID: "u1"}
	auth.BackfillRealmFor(u, "global")
	p.Add(u)
	p.SetCredits("u1", 1000, 0) // 余额充足：不受保底约束，只被冷却挡住

	// 账号级软冷却 → 健康候选为空，强制走兜底路径。
	p.Cooldown("u1", CoolSoft, 2*time.Minute, "test")
	// 同时该号对 kimi-k3 处于 6004 模型级冷却（不写账号级 until）。
	p.CooldownSoftForModel("u1", 600*time.Second, time.Now().Add(1*time.Hour), "kimi-k3", "6004 model rate limit")

	if a := p.PickExcludingForRealm(nil, "kimi-k3", "global"); a != nil {
		t.Fatalf("兜底不得选中对该模型处于 6004 冷却的号，got %v", a)
	}
	// 对照：换成没被冷却的模型，兜底仍应能选中 —— 证明不是把账号整个排除了。
	if a := p.PickExcludingForRealm(nil, "other-model", "global"); a == nil || a.UID != "u1" {
		t.Fatalf("未被模型冷却的其它模型不应受影响，got %v", a)
	}
}

// TestPersistFailKeepsDirty 落盘失败必须保留脏位，供 flusher 下一轮重试。
//
// 背景（审查发现）：原实现 `if p.dirty.Swap(false) { p.saveLocked() }` 先清脏再写盘，
// saveLocked 失败只记日志、脏位仍是 false → flusher 之后再也不写，
// 这一次的状态变更**永久丢失**（内存态与 state.json 静默分叉）。
func TestPersistFailKeepsDirty(t *testing.T) {
	dir := t.TempDir()
	// 造一个「父路径是普通文件」的位置：MkdirAll 失败（被忽略）→ WriteFile 必失败。
	blocker := filepath.Join(dir, "blocker")
	if err := os.WriteFile(blocker, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	p := New(filepath.Join(blocker, "state.json"))

	p.Add(&auth.Auth{UID: "u1"})
	p.dirty.Store(false) // 从干净态开始，隔离出「本次操作」的脏位

	p.Cooldown("u1", CoolSoft, time.Minute, "test")
	if !p.dirty.Load() {
		t.Fatal("状态变更后应置脏")
	}

	p.Flush() // 落盘必失败（父路径是文件）
	if !p.dirty.Load() {
		t.Fatal("落盘失败后必须保留脏位，否则这次变更永远不会被写盘（静默丢状态）")
	}
}
