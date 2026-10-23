package upstream

import (
	"encoding/base64"
	"strconv"
	"strings"
	"testing"
	"time"
)

// TestTraeExchangeTokenRejectsEmpty 空 refresh_token 必须在**本地**就挡掉：
// 不发无谓的上游请求，并给出用户能看懂的原因（而不是把上游的英文报错透出去）。
func TestTraeExchangeTokenRejectsEmpty(t *testing.T) {
	for _, rt := range []string{"", "   ", "\t\n"} {
		_, _, err := TraeExchangeToken(rt)
		if err == nil {
			t.Errorf("空 refresh_token %q 应报错", rt)
			continue
		}
		if !strings.Contains(err.Error(), "refresh_token") {
			t.Errorf("错误信息应提到 refresh_token，got %v", err)
		}
	}
}

// TestTraeNeedsRefresh 续期判定。
//
// 背景：Trae 的 JWT 只有 8 小时有效期，续期巡检靠这个判据决定「要不要动手」。
// 两个方向都错不得：
//   - 判「不需要」太早 → 到期后签到失败（用户看到的就是「登录失效」）
//   - 判「需要」太频繁 → 每次巡检都白跑一次浏览器/接口
func TestTraeNeedsRefresh(t *testing.T) {
	mk := func(exp int64) TraeAccount {
		seg := base64.URLEncoding.EncodeToString([]byte(
			`{"data":{"id":123},"exp":` + strconv.FormatInt(exp, 10) + `}`))
		return TraeAccount{JWT: "Cloud-IDE-JWT eyJhbGciOiJSUzI1NiJ9." + seg + ".sig"}
	}
	now := time.Now()
	const margin = 3 * time.Hour

	// 无法解析 exp（老账号/坏 JWT）→ 一律判为需要续期：续期失败无害，只多一条日志。
	if !TraeNeedsRefresh(TraeAccount{JWT: ""}, margin) {
		t.Error("空 JWT 应判为需要续期")
	}
	if !TraeNeedsRefresh(TraeAccount{JWT: "not-a-jwt"}, margin) {
		t.Error("不可解析 JWT 应判为需要续期")
	}
	// 已过期
	if !TraeNeedsRefresh(mk(now.Add(-time.Hour).Unix()), margin) {
		t.Error("已过期应判为需要续期")
	}
	// 剩余 1 小时 < margin
	if !TraeNeedsRefresh(mk(now.Add(time.Hour).Unix()), margin) {
		t.Error("剩余不足 margin 应判为需要续期")
	}
	// 剩余 6 小时 > margin
	if TraeNeedsRefresh(mk(now.Add(6*time.Hour).Unix()), margin) {
		t.Error("余量充足不应判为需要续期")
	}
}
