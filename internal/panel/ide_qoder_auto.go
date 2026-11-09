package panel

import (
	"context"
	"log"
	"strings"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/ideauth"
)

// ── Qoder 号池后台自动续期（2026-11-08 新增）──────────────────────────
//
// 【为什么补这个】
//   以前 Qoder 只有「手动续期」按钮，而且它只续**当前登录**那一个号。
//   号池里其余号的 token 没人管：过期之后，只有等签到时撞上 401 才现续，
//   那一次签到就废了 —— 用户看到的就是「某些号老是签不上」、
//   「号池里的号换着用，有的能签有的不能」。
//
// 【节奏】
//   每 1 小时巡检一次，把「剩余有效期不足 24 小时」的号提前续上。
//   巡检本身很轻（只看本地到期时间），真正要续时才打上游。
//   1 小时一轮意味着「最坏 1 小时就能发现临期」，比签到时才发现早得多。
//
// 【写回号池】
//   必须走 ideauth.UpdateQoderPool（包级锁下的 Load→改→Save）：
//   号池有多个并发写者（签到回写 token、续期、增删账号），
//   裸 Load→改→Save 会互相覆盖 —— 后写的把先写的改动整个盖掉。
const (
	// qoderRenewInterval 续期巡检间隔。
	qoderRenewInterval = 1 * time.Hour
	// qoderRenewMargin 剩余有效期不足这个值就续（留足容错）。
	qoderRenewMargin = 24 * time.Hour
)

// StartQoderRenewAuto 启动 Qoder 号池的后台自动续期。
func (p *Panel) StartQoderRenewAuto(ctx context.Context) {
	go func() {
		// 起来后先等 60 秒：启动瞬间面板要做号池整理/缓存预热，别抢。
		select {
		case <-ctx.Done():
			return
		case <-time.After(60 * time.Second):
		}
		t := time.NewTicker(qoderRenewInterval)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				p.qoderRenewOnce()
			}
		}
	}()
}

// qoderRenewOnce 跑一轮 Qoder 号池续期。
func (p *Panel) qoderRenewOnce() {
	pool, err := ideauth.LoadQoderPool()
	if err != nil || pool == nil || len(pool.Accounts) == 0 {
		return
	}
	// 1) 先算清楚哪些号要续（只读，不碰号池）
	var list []qoderRenewItem
	for _, snap := range pool.Accounts {
		if snap == nil {
			continue
		}
		if !ideauth.QoderSnapshotNeedsRenew(snap, qoderRenewMargin) {
			continue
		}
		list = append(list, qoderRenewItem{email: snap.Email, snap: snap})
	}
	if len(list) == 0 {
		return
	}
	// 2) 逐号续期（快照是各自独立的内存对象，互不影响）
	type res struct {
		email   string
		renewed bool
		expires string
		err     error
	}
	results := make([]res, 0, len(list))
	for _, it := range list {
		renewed, exp, rerr := ideauth.QoderRenewSnapshotAuth(it.snap)
		results = append(results, res{email: it.email, renewed: renewed, expires: exp, err: rerr})
	}
	// 3) 一次性写回号池（只写续期成功的）
	if err := ideauth.UpdateQoderPool(func(f *ideauth.QoderPoolFile) error {
		for _, r := range results {
			if !r.renewed {
				continue
			}
			src := qoderRenewItemByEmail(list, r.email)
			if src == nil {
				continue
			}
			for _, snap := range f.Accounts {
				if snap == nil || snap.Email != r.email {
					continue
				}
				// 用刚续出来的那份 auth 覆盖（refresh_token 是轮换的，必须换）。
				snap.Auth.Token = src.snap.Auth.Token
				snap.Auth.RefreshToken = src.snap.Auth.RefreshToken
				if src.snap.Auth.ExpiresAt != "" {
					snap.Auth.ExpiresAt = src.snap.Auth.ExpiresAt
				}
				if src.snap.Auth.RefreshTokenExpiresAt != "" {
					snap.Auth.RefreshTokenExpiresAt = src.snap.Auth.RefreshTokenExpiresAt
				}
				snap.SavedAt = time.Now().Format(time.RFC3339)
				break
			}
		}
		return nil
	}); err != nil {
		log.Printf("[qoder] 续期后写号池失败: %v", err)
		return
	}
	for _, r := range results {
		if r.renewed {
			log.Printf("[qoder] 账号 %s 已自动续期，到期 %s", maskLogIdent(r.email), r.expires)
		} else if r.err != nil {
			log.Printf("[qoder] 账号 %s 自动续期失败: %v", maskLogIdent(r.email), r.err)
		}
	}
}

// qoderRenewItem 待续期的一个账号（email + 它的快照）。
type qoderRenewItem struct {
	email string
	snap  *ideauth.QoderSnapshot
}

// qoderRenewItemByEmail 按 email 取回待续列表里的快照（写回号池时要用续出来的那份 auth）。
func qoderRenewItemByEmail(list []qoderRenewItem, email string) *qoderRenewItem {
	for i := range list {
		if list[i].email == email {
			return &list[i]
		}
	}
	return nil
}

// maskLogIdent 日志里也不打真实邮箱。
func maskLogIdent(email string) string {
	s := strings.TrimSpace(email)
	if s == "" {
		return "(空)"
	}
	if !strings.Contains(s, "@") {
		return s
	}
	return "账号" + s[:1] + "***"
}
