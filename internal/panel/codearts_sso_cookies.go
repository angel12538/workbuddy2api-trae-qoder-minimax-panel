package panel

import (
	"fmt"
	"log"
	"strings"

	"github.com/linguo2625469/workbuddy2api-panel/internal/codearts"
)

// codeartsSSOSessionCookies 华为云 authui 域上的 **SSO 会话** Cookie。
//
// 为什么它们是「每次都要重新认证」的元凶之一：
//
//  1. SSOTGC / SSOJTC 的 Cookie 本身是**会话级**的（expires = -1，浏览器退出即失效），
//     号池里存的那条是几天前扫码登录时抓到的快照；
//  2. 服务端的 SSO 会话早已作废（这批 Cookie 抓完没过几天就会在 authui 吐登录页）；
//  3. 但**长期票据还在**：同一个号池里还躺着 `hwid_cas_sid`（.id1.cloud.huawei.com，
//     expires 2027-12）、`cbc-sid`（.huaweicloud.com，expires 2027-10）、
//     `HW_idvc_*` / `HW_id_*`（.auth.huaweicloud.com，expires 2027-08）——
//     这些是华为统一身份认证（ID/CAS）的持久会话，有效期以**年**计；
//  4. 带着一条已死的 SSOTGC 去访问 `authui/login?service=…`，authui 看到
//     "SSO 会话存在但无效"，就直接吐登录页，**根本不会再用那批还活着的 ID 票据
//     重新走一次 SSO**。于是每次续期都得到 needs_login，用户只能重新扫码。
//
// 所以续期改成两步：第一次**不注入**这两条 Cookie，让 authui 走
// "没有 SSO 会话 → 用 ID 票据重新换 CAS ticket" 的分支；确实不行再带全量试一次。
var codeartsSSOSessionCookies = map[string]bool{
	"SSOTGC": true,
	"SSOJTC": true,
}

// codeartsCookiesWithoutSSOSession 去掉已死的 SSO 会话 Cookie，返回其余 Cookie。
//
// 注意：返回的是**新切片**（不复用底层数组），调用方可以安全地再改。
func codeartsCookiesWithoutSSOSession(cookies []codearts.Cookie) []codearts.Cookie {
	out := make([]codearts.Cookie, 0, len(cookies))
	for _, c := range cookies {
		if codeartsSSOSessionCookies[strings.ToUpper(strings.TrimSpace(c.Name))] {
			continue
		}
		out = append(out, c)
	}
	return out
}

// codeartsCookiesMergedWithBrowser 第二次尝试用的 Cookie 集合：号池里的全量 Cookie，
// 但 SSOTGC / SSOJTC 取**浏览器当前的值**（如果浏览器里有一条不一样的）。
//
// 为什么：第一次尝试时 authui 可能已经签发了**新的** SSOTGC。若这时把号池里
// 那条早就作废的旧值再写回去，会把刚拿到的新会话凭据覆盖掉 —— 越试越死。
func codeartsCookiesMergedWithBrowser(cookies []codearts.Cookie, port int) []codearts.Cookie {
	out := make([]codearts.Cookie, 0, len(cookies))
	live := map[string]string{}
	if cur, err := cdpAllCookies(port); err == nil {
		for _, c := range cur {
			live[strings.ToUpper(strings.TrimSpace(c.Name))] = c.Value
		}
	}
	for _, c := range cookies {
		name := strings.ToUpper(strings.TrimSpace(c.Name))
		if codeartsSSOSessionCookies[name] {
			if v, ok := live[name]; ok && v != "" {
				c.Value = v // 用浏览器里新签发的那条
			}
		}
		out = append(out, c)
	}
	return out
}

// caSetCookiesAndFingerprintWith 注入**指定**的 Cookie 集合 + 该账号槽的指纹。
//
// caSetCookiesAndFingerprint 的变体：续期要分两次尝试（先不带 SSO 会话 Cookie），
// 所以 Cookie 集合必须能由调用方指定，不能固定用 acc.Cookies。
func caSetCookiesAndFingerprintWith(port int, pageWS *wsConn, acc *codearts.Account, cookies []codearts.Cookie) error {
	if acc == nil {
		return fmt.Errorf("账号为空")
	}
	if len(cookies) == 0 {
		return fmt.Errorf("该账号没有 Cookie")
	}
	if err := caSetCookies(port, cookies); err != nil {
		return err
	}
	fp := codeartsFingerprintFor(acc)
	if fp.empty() || pageWS == nil {
		return nil
	}
	if err := codeartsApplyFingerprint(pageWS, fp); err != nil {
		log.Printf("[codearts] 槽 %s 指纹应用失败（Cookie 已注入，继续导航）: %v",
			codeartsSlotIdentityOf(acc), err)
	}
	return nil
}
