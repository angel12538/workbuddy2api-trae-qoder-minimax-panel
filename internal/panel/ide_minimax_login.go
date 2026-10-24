// ide_minimax_login.go 面板「MiniMax 浏览器登录」：OAuth 2.0 设备码授权
// （RFC 8628 + S256 PKCE）。
//
// 解决的问题：MiniMax 号池此前只有「捕获本机客户端登录态」和「手工粘贴
// access/refresh token」两条加号路径 —— 前者要求账号已经在本机 MiniMax Code
// 里登录过，后者要求用户去别处抠 token。设备码授权让用户在面板点一下、
// 浏览器里登录完就进池，与 WorkBuddy 的「获取授权链接」是同一套交互。
//
// 接口：
//   - POST /panel/api/ide/minimax/login/start  发起授权，返回验证码与授权链接
//   - GET  /panel/api/ide/minimax/login/poll   轮询授权结果；成功后写入号池
//
// 实测要点（2026-10-24）：
//   - 授权窗口只有 300 秒（上游 expires_in=300），面板要显示倒计时并允许重发；
//   - 轮询快于 interval 会拿到 slow_down，前端要按服务端给的间隔来轮询；
//   - 成功后拿到的 refresh_token 与客户端的同源，但**独立成链**（客户端那次
//     授权的 RT 不会被我们顶掉），所以号池账号可以自己续期，不会把客户端踢下线。
package panel

import (
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/minimax"
)

// mmLoginTTL 授权会话的最长保留时间。
//
// 上游的授权窗口只有 300 秒，这里给到 10 分钟：多留的这段时间是为了让
// 「已过期」的提示能如实显示（而不是直接报「会话不存在」），并允许用户
// 在同一条会话上重试一次轮询。
const mmLoginTTL = 10 * time.Minute

// minimaxLoginStart 发起一次设备码授权。
//
// body: {"region":"cn"|"global"}（缺省 cn）。
// 返回: state（面板会话号）、user_code（验证码）、verification_uri_complete
// （带验证码的授权页）、interval（轮询间隔秒）、expires_in（授权窗口秒）。
func (p *Panel) minimaxLoginStart(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Region string `json:"region"`
	}
	if r.Body != nil {
		_ = json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<12)).Decode(&req)
	}
	region := minimax.NormalizeRegion(req.Region)

	sess, err := minimax.StartDeviceAuth(r.Context(), p.minimaxHTTP(), region)
	if err != nil {
		writeErr(w, http.StatusBadGateway, "minimax_login_start_failed: "+err.Error())
		return
	}
	sess.State = randomHex(24)

	p.mmLoginMu.Lock()
	// 顺手回收过期会话，避免「开了弹窗就走开」的会话一直滞留。
	now := time.Now()
	for k, v := range p.mmLogins {
		if now.Sub(v.Created) > mmLoginTTL {
			delete(p.mmLogins, k)
		}
	}
	p.mmLogins[sess.State] = sess
	p.mmLoginMu.Unlock()

	log.Printf("panel: 发起 MiniMax 设备码授权 region=%s（user_code=%s）", region, sess.UserCode)
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":                      true,
		"state":                   sess.State,
		"region":                  region,
		"user_code":               sess.UserCode,
		"verification_uri":        sess.VerifyURI,
		"verification_uri_complete": sess.VerifyURICComplete,
		"interval":                sess.Interval,
		"expires_in":              int(time.Until(sess.ExpiresAt).Seconds()),
	})
}

// minimaxLoginPoll 轮询授权结果。
//
// 未完成 → {"done":false}；完成 → 写入号池并顺带签到，返回账号信息。
func (p *Panel) minimaxLoginPoll(w http.ResponseWriter, r *http.Request) {
	state := r.URL.Query().Get("state")
	if state == "" {
		writeErr(w, http.StatusBadRequest, "missing state")
		return
	}
	p.mmLoginMu.Lock()
	sess, known := p.mmLogins[state]
	if known && time.Since(sess.Created) > mmLoginTTL {
		delete(p.mmLogins, state)
		known = false
	}
	p.mmLoginMu.Unlock()
	if !known {
		writeErr(w, http.StatusNotFound, "授权会话不存在或已过期（请重新发起登录）")
		return
	}

	status, tok, err := minimax.PollDeviceAuth(r.Context(), p.minimaxHTTP(), sess)
	cli := p.minimaxClient
	if cli == nil {
		cli = minimax.New()
	}
	switch status {
	case minimax.DeviceOK:
		// 授权成功：写入号池（按 user_id 去重），并顺带签到一次。
		acc, added, aerr := p.addMinimaxDeviceAccount(r, cli, tok, sess.Region)
		if aerr != nil {
			writeErr(w, http.StatusBadGateway, "minimax_add_failed: "+aerr.Error())
			return
		}
		p.mmLoginMu.Lock()
		delete(p.mmLogins, state)
		p.mmLoginMu.Unlock()

		checkinMsg := ""
		if claim, cerr := cli.ForRegion(sess.Region).ClaimSignin(r.Context(), minimax.Credentials{
			AccessToken:  tok.AccessToken,
			RefreshToken: tok.RefreshToken,
			ExpiresAt:    tok.ExpiresAt,
		}); cerr != nil {
			checkinMsg = "签到失败: " + cerr.Error()
		} else {
			checkinMsg = claim.Msg
		}
		log.Printf("panel: MiniMax 浏览器登录成功：%s（%s）· %s", acc.Label, sess.Region, checkinMsg)
		writeJSON(w, http.StatusOK, map[string]any{
			"done":     true,
			"id":       acc.ID,
			"label":    acc.Label,
			"added":    added,
			"region":   sess.Region,
			"msg":      fmt.Sprintf("已%s：%s", map[bool]string{true: "添加", false: "更新"}[added], acc.Label),
			"checkin":  checkinMsg,
		})
	case minimax.DeviceDenied, minimax.DeviceExpired:
		p.mmLoginMu.Lock()
		delete(p.mmLogins, state)
		p.mmLoginMu.Unlock()
		msg := "授权被拒绝"
		if err != nil {
			msg = err.Error()
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"done":  false,
			"error": msg,
			"fatal": true,
		})
	default:
		// pending：继续等。带上当前间隔，前端据此调整轮询节奏（避免 slow_down）。
		out := map[string]any{
			"done":     false,
			"interval": sess.Interval,
		}
		if err != nil {
			out["message"] = err.Error()
		}
		if !sess.ExpiresAt.IsZero() {
			out["expires_in"] = int(time.Until(sess.ExpiresAt).Seconds())
		}
		writeJSON(w, http.StatusOK, out)
	}
}

// addMinimaxDeviceAccount 把浏览器授权拿到的凭证写进号池。
func (p *Panel) addMinimaxDeviceAccount(r *http.Request, cli *minimax.Client, tok *minimax.DeviceAuthResult, region string) (*minimax.Account, bool, error) {
	if cli == nil {
		cli = minimax.New()
	}
	var expMs int64
	if !tok.ExpiresAt.IsZero() {
		expMs = tok.ExpiresAt.UnixMilli()
	}
	return cli.AddManualRegion(r.Context(), tok.AccessToken, tok.RefreshToken, expMs, region)
}

// minimaxHTTP 返回 MiniMax 通道的 HTTP 客户端（可能为 nil，由 minimax 包内部回落默认）。
func (p *Panel) minimaxHTTP() *http.Client {
	if p.minimaxClient == nil {
		return nil
	}
	return p.minimaxClient.HTTP
}
