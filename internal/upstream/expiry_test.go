package upstream

import (
	"encoding/base64"
	"encoding/json"
	"testing"
	"time"
)

// TestExpiresAtFromExpiresIn 过期时刻换算必须挡掉两个方向的坏值。
//
// 背景（审查发现）：登录路径（panel/login.go）此前**无条件**照写
// now+expiresIn，而刷新路径（client.go）有守卫——两条路径对同一份上游响应
// 判定不一致，登录落盘的账号可能带一个坏 ExpiresAt：
//   - expiresIn<=0 → ExpiresAt 落到 now → 立刻「需刷新」，反复刷新（刷新风暴）
//   - expiresIn 荒谬巨大 → ExpiresAt 推到荒谬未来 → NeedsRefresh 永假 →
//     token 真过期也不刷新，账号**静默失效**（严重得多）
func TestExpiresAtFromExpiresIn(t *testing.T) {
	// 正常值：应给出「now + expiresIn」量级的绝对时刻。
	if got := ExpiresAtFromExpiresIn(3600); got <= time.Now().Unix() {
		t.Errorf("正常 expiresIn=3600 应给出未来时刻，got %d", got)
	}
	// 坏值：一律返回 0，表示「不可信，调用方保留旧值/另找权威来源」。
	for _, bad := range []int64{0, -1, -3600, int64((10 * 365 * 24 * time.Hour).Seconds()), 1 << 40} {
		if got := ExpiresAtFromExpiresIn(bad); got != 0 {
			t.Errorf("坏 expiresIn=%d 应返回 0，got %d", bad, got)
		}
	}
	// 边界：恰好 10 年视为不可信（>= 上限）。
	if got := ExpiresAtFromExpiresIn(int64((10*365*24*time.Hour)/time.Second) - 1); got <= 0 {
		t.Errorf("略小于上限应可信，got %d", got)
	}
}

// TestJWTExpiry 通用 JWT exp 解析（WorkBuddy 裸 JWT 与 Trae 带前缀两种形态）。
func TestJWTExpiry(t *testing.T) {
	const want = 1793714399
	mk := func(prefix string, payload string) string {
		seg := base64.URLEncoding.EncodeToString([]byte(payload))
		return prefix + "eyJhbGciOiJSUzI1NiJ9." + seg + ".sig"
	}
	body, _ := json.Marshal(map[string]any{"exp": want, "sub": "u1"})

	// 裸 JWT（WorkBuddy accessToken 形态）。
	if exp, ok := JWTExpiry(mk("", string(body))); !ok || exp.Unix() != want {
		t.Errorf("裸 JWT 解析失败：ok=%v exp=%v", ok, exp)
	}
	// 带 Cloud-IDE-JWT 前缀（Trae 凭证形态）。
	if exp, ok := JWTExpiry(mk("Cloud-IDE-JWT ", string(body))); !ok || exp.Unix() != want {
		t.Errorf("带前缀 JWT 解析失败：ok=%v exp=%v", ok, exp)
	}
	// 非法输入一律 ok=false（调用方据此回落保守值，不得瞎填）。
	for _, bad := range []string{"", "not-a-jwt", "a.b", mk("", `{"sub":"no-exp"}`)} {
		if _, ok := JWTExpiry(bad); ok {
			t.Errorf("非法输入 %q 应 ok=false", bad)
		}
	}
}
