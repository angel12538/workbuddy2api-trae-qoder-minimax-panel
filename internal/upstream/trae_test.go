package upstream

import (
	"encoding/base64"
	"encoding/json"
	"strings"
	"testing"
	"time"
)

// fakeTraeJWT 造一个带指定 exp 的假 JWT：解析方不校验签名，故只需正确编码 payload。
// 用 RawURLEncoding（无 padding），覆盖 TraeJWTExpiry 里「按需补 padding」的分支。
func fakeTraeJWT(exp int64) string {
	payload, _ := json.Marshal(map[string]any{
		"exp":  exp,
		"data": map[string]any{"id": "123", "source": "refresh_token"},
	})
	return "Cloud-IDE-JWT eyJhbGciOiJub25lIn0." +
		base64.RawURLEncoding.EncodeToString(payload) + ".sig"
}

func TestTraeJWTExpiry(t *testing.T) {
	want := time.Now().Add(48 * time.Hour).Unix()
	got, ok := TraeJWTExpiry(fakeTraeJWT(want))
	if !ok {
		t.Fatal("合法 JWT 应当解析成功")
	}
	if got.Unix() != want {
		t.Fatalf("exp 不符：got %d want %d", got.Unix(), want)
	}
}

func TestTraeJWTExpiryMalformed(t *testing.T) {
	// 畸形输入一律返回 ok=false，调用方据此回落展示上游原始错误。
	for _, jwt := range []string{"", "abc", "a.b", "Cloud-IDE-JWT x.y.z", "Cloud-IDE-JWT .e30."} {
		if _, ok := TraeJWTExpiry(jwt); ok {
			t.Fatalf("畸形 JWT %q 不应解析成功", jwt)
		}
	}
}

func TestTraeAuthHint(t *testing.T) {
	// 已过期 → 必须给出可操作的中文提示（这是本次修复的核心目的）。
	expired := TraeAccount{JWT: fakeTraeJWT(time.Now().Add(-time.Hour).Unix())}
	if h := TraeAuthHint(expired); h == "" {
		t.Fatal("过期凭证应当给出中文提示")
	} else if !strings.Contains(h, "重新登录") {
		t.Fatalf("提示应引导用户重新登录，got %q", h)
	}

	// 未过期 → 不给过期提示，让调用方回落上游原始错误（避免把网络故障误报成过期）。
	fresh := TraeAccount{JWT: fakeTraeJWT(time.Now().Add(time.Hour).Unix())}
	if h := TraeAuthHint(fresh); h != "" {
		t.Fatalf("未过期凭证不应给过期提示，got %q", h)
	}
}
