package panel

import (
	"testing"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
	"github.com/linguo2625469/workbuddy2api-panel/internal/upstream"
)

// TestTaskAccountLockSameAccountExclusive 同一账号的任务锁互斥：第二次 tryLock 必须失败，
// 解锁后可再次获取。这是「重复点一键完成不并发重跑」的核心保障。
func TestTaskAccountLockSameAccountExclusive(t *testing.T) {
	p := &Panel{}
	uid := "u1"

	if !p.tryLockAccount(uid) {
		t.Fatal("首次加锁应成功")
	}
	if p.tryLockAccount(uid) {
		t.Fatal("同账号第二次加锁应失败（互斥）")
	}
	p.unlockAccount(uid)

	if !p.tryLockAccount(uid) {
		t.Fatal("解锁后应可再次加锁")
	}
	p.unlockAccount(uid)
}

// TestTaskAccountLockDifferentAccountsIndependent 不同账号的锁互不影响（并行照旧）。
func TestTaskAccountLockDifferentAccountsIndependent(t *testing.T) {
	p := &Panel{}
	if !p.tryLockAccount("u1") {
		t.Fatal("u1 加锁应成功")
	}
	if !p.tryLockAccount("u2") {
		t.Fatal("u2 加锁应成功（不同账号不互斥）")
	}
	p.unlockAccount("u2")
	p.unlockAccount("u1")
}

// TestTaskAccountLockCrossEntryShared 单任务 auto 与全量 auto_all 共用同一把账号锁
// （在 handler 层都走 tryLockAccount，这里验证锁命名空间一致）。
func TestTaskAccountLockCrossEntryShared(t *testing.T) {
	p := &Panel{}
	if !p.tryLockAccount("u1") {
		t.Fatal("u1 加锁应成功")
	}
	// 模拟全量入口对同一 uid 加锁——必须被挡（否则两入口可并发）。
	if p.tryLockAccount("u1") {
		t.Fatal("同 uid 跨入口加锁应失败（共用锁）")
	}
	p.unlockAccount("u1")
}

// TestAutoAllKeepsHandlerLock 「一键完成」主流程自身不抢锁、也不释放锁：
// 锁的唯一 owner 是 handler（accountTaskAutoAll），流水线在锁内运行。
// 若主流程顺手解锁，并发点击会在流水线未结束时重入（两轮同时打上游）。
func TestAutoAllKeepsHandlerLock(t *testing.T) {
	p := &Panel{}
	uid := "u-lock"
	if !p.tryLockAccount(uid) {
		t.Fatal("handler 加锁应成功")
	}

	env := newFakeAutoEnv([]upstream.Task{
		{TaskCode: "Expert_Philanthropy", Title: "真实捐款", Target: 1, Current: 0, AcceptStatus: "accepted"},
	}, nil)
	out := p.runAutoAllWithDeps(&auth.Auth{UID: uid}, env.deps())
	if len(out) != 1 {
		t.Fatalf("结果项数 = %d, want 1: %+v", len(out), out)
	}

	// 主流程不应把 handler 的锁放掉：这里仍然抢不到。
	if p.tryLockAccount(uid) {
		t.Fatal("一键完成主流程不应释放 handler 持有的账号锁")
	}
	p.unlockAccount(uid)
	if !p.tryLockAccount(uid) {
		t.Fatal("解锁后应可再次加锁")
	}
	p.unlockAccount(uid)
}

// TestAutoAllLockedAndClaimedDoNotRun 锁在流水线内同样成立：上游 locked 与
// 已领取的任务一律不执行动作（只如实回报 locked / 不入结果）。
func TestAutoAllLockedAndClaimedDoNotRun(t *testing.T) {
	p := &Panel{}
	uid := "u-locked"
	if !p.tryLockAccount(uid) {
		t.Fatal("handler 加锁应成功")
	}
	defer p.unlockAccount(uid)

	locked := fakeTask("first_buddy", 0, 1)
	locked.Locked = true
	claimed := fakeTask("chat_5", 5, 5)
	claimed.Claimed = true

	env := newFakeAutoEnv([]upstream.Task{locked, claimed}, nil)
	out := p.runAutoAllWithDeps(&auth.Auth{UID: uid}, env.deps())

	if len(env.executed) != 0 {
		t.Fatalf("locked/claimed 不应执行动作，executed=%v", env.executed)
	}
	if item := findAutoResult(out, "first_buddy"); item == nil || item["status"] != AutoStatusLocked {
		t.Fatalf("locked 任务应如实列为 locked，out=%+v", out)
	}
	if findAutoResult(out, "chat_5") != nil {
		t.Error("已领取任务不应出现在结果中")
	}
}
