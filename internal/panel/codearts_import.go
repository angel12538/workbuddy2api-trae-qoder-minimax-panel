// codearts_import.go 「粘贴 Cookie」导入华为云网页会话。
//
// 为什么加这条路：抓会话的方式**不止一条**。面板自己拉起的登录窗口只是其中一种，
// 而用户日常就是在自己的浏览器里登华为云 —— 那个浏览器没有调试端口，面板看不见它，
// 于是出现"我网页明明登进去了，面板还说要重新登录"。
//
// 从 DevTools 把任一 `/chat/` 请求的 **Cookie 请求头整行**复制过来就能用：
//   - 浏览器不限（Chrome / Edge / Firefox / 360 都可以），也不需要调试端口；
//   - 必须从 Network 面板的请求头里复制，**不能**用控制台 `document.cookie` ——
//     SSOTGC / SessionID 这些关键票据是 HttpOnly，JS 读不到，粘过来就是残缺的。
package panel

import (
	"encoding/json"
	"errors"
	"log"
	"net"
	"net/http"
	"strings"

	"github.com/linguo2625469/workbuddy2api-panel/internal/codearts"
)

// codeartsImportCookie POST /panel/api/ide/codearts/import_cookie
//
// body: {"cookie": "name=value; name2=value2; …", "label": "可选，给自己看的名字"}
//
// 先验通再入库：打不通就不写号池，免得攒一堆"看着是账号、其实是僵尸"的记录
// （MergePool 那次踩的就是这类重复/覆盖问题）。
func (p *Panel) codeartsImportCookie(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Cookie string `json:"cookie"`
		Label  string `json:"label"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, "请求体不是合法 JSON")
		return
	}
	cookies, parseErr := parseCodeartsCookieInput(req.Cookie)
	if parseErr != nil {
		writeErr(w, http.StatusBadRequest, parseErr.Error())
		return
	}
	if len(cookies) == 0 {
		writeErr(w, http.StatusBadRequest, "没解析出 Cookie：请粘贴**整行** Cookie 请求头"+
			"（形如 name=value; name2=value2）。注意要用 F12 → Network → 任一 /chat/ 请求的"+
			" Request Headers，控制台里的 document.cookie 拿不到 HttpOnly 的关键票据。")
		return
	}
	acc := &codearts.Account{
		ID:       codearts.Fingerprint(cookies),
		Identity: codearts.IdentityOf(cookies),
		DomainID: codearts.DomainIDOf(cookies),
		Cookies:  cookies,
		SavedAt:  codearts.Now(),
		Label:    strings.TrimSpace(req.Label),
		// 来源标记：这是用户手工粘贴的 Cookie 头（§H.2 ClientSource）。
		ClientSource: codearts.ClientSourceImport,
	}
	acc.Slot = acc.ID

	// 先验通再入库，但 Keepalive 是网络调用，不能持号池锁跑网络。
	// 所以在**临时池**里先 Upsert 出这一条（含认人结果），拿它去验通；
	// 验通通过后再进 UpdatePool 按同一身份重新 Upsert 落盘。
	// 这样既保留"打不通就不写号池"的原语义，也不再需要裸 Load→Save（R4.3）。
	probePool := &codearts.Pool{}
	up, _ := codearts.Upsert(probePool, acc) // 同一个人（domain_tag|user_tag）更新原记录，不新增僵尸

	ok, kerr := codearts.Keepalive(up)
	if !ok {
		msg := codeartsImportFailure(kerr)
		writeJSON(w, http.StatusOK, map[string]any{"ok": false, "message": msg})
		return
	}

	// 回写走 UpdatePool：与补采/续期/保活并发时，各自的 Load→改→Save 会互相
	// 覆盖（R4.3）。回调内只改传入的 *Pool，不再调 LoadPool/SavePool（锁不可重入）。
	var stored *codearts.Account
	if err := codearts.UpdatePool(func(pool *codearts.Pool) error {
		x, _ := codearts.Upsert(pool, up)
		if x == nil {
			return nil
		}
		// Upsert 命中已有记录时不会复制 ClientSource，这里显式标成 import。
		x.ClientSource = codearts.ClientSourceImport
		stored = x
		return nil
	}); err != nil {
		writeErr(w, http.StatusInternalServerError, "写入号池失败: "+err.Error())
		return
	}
	if stored == nil {
		writeErr(w, http.StatusInternalServerError, "写入号池失败: 没有可保存的账号记录")
		return
	}
	log.Printf("[codearts] 粘贴 Cookie 导入成功: id=%s label=%s cookies=%d identity=%s"+
		"（该路径 expires_known=false，无票据寿命）",
		stored.ID, stored.Label, len(stored.Cookies), stored.Identity)
	// 会话最值钱的就是这一刻：当场把今天的份领掉。
	p.codeartsAutoClaimAfterLogin(stored)
	item := p.codeartsAccountItem(stored)
	// 提示必须如实：粘贴的 Cookie 头**没有**过期信息，不能让人以为面板能算票据寿命。
	msg := "已导入并验通（Cookie " + codearts.Seq(len(stored.Cookies)) + " 条）。" +
		"注意：粘贴的 Cookie 请求头不带 Expires/Max-Age，本次导入的 expires_known=false，" +
		"面板无法据此判断票据寿命；需要用浏览器登录/补采一次才能获得真实过期信息。"
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "id": stored.ID, "label": stored.Label,
		"message": msg, "expires_known": false, "ticket_source": stored.TicketSource, "account": item})
}

// parseCookieHeader 把 `name=value; name2=value2` 形式的 Cookie 头拆成条目。
//
// 统一挂在 .huaweicloud.com 域上即可：HTTP 直连路径本来就是一整行 Cookie 发出去，
// 不看 per-domain 归属（api.go 的 doAPI 就是这么拼的）。
func parseCookieHeader(raw string) []codearts.Cookie {
	cookies, _ := parseCodeartsCookieInput(raw)
	return cookies
}

var errCodeartsCookieFormat = errors.New("Cookie 格式不正确：请在网络面板中复制 Cookie 请求头的完整值。可按行粘贴完整的 name=value 项；不要在票据中间插入换行、制表符或其他控制字符。未发起验证请求，也未写入号池。")

// Normalize boundaries between complete cookie pairs, never bytes inside a token.
// A copied request-header block is restricted to its Cookie field.
func parseCodeartsCookieInput(raw string) ([]codearts.Cookie, error) {
	for _, ch := range raw {
		if ch == 0x7f || (ch < 0x20 && ch != '\r' && ch != '\n' && ch != '\t') {
			return nil, errCodeartsCookieFormat
		}
	}
	raw = strings.TrimSpace(strings.TrimPrefix(raw, "\ufeff"))
	if raw == "" {
		return nil, nil
	}
	lines := strings.Split(strings.ReplaceAll(strings.ReplaceAll(raw, "\r\n", "\n"), "\r", "\n"), "\n")
	start := 0
	explicit := false
	for i, line := range lines {
		line = strings.TrimSpace(line)
		if strings.EqualFold(line, "cookie") || strings.HasPrefix(strings.ToLower(line), "cookie:") {
			start, explicit = i, true
			break
		}
	}
	var out []codearts.Cookie
	for _, line := range lines[start:] {
		line = strings.TrimSpace(line)
		if strings.EqualFold(line, "cookie") {
			continue
		}
		if strings.HasPrefix(strings.ToLower(line), "cookie:") {
			line = strings.TrimSpace(line[len("cookie:"):])
		} else if colon := strings.IndexByte(line, ':'); explicit && colon >= 0 {
			equal := strings.IndexByte(line, '=')
			if equal < 0 || colon < equal {
				break // next request header, not part of the Cookie value
			}
		}
		for _, piece := range strings.Split(line, ";") {
			piece = strings.TrimSpace(piece)
			if piece == "" {
				continue
			}
			k, v, found := strings.Cut(piece, "=")
			if !found {
				return nil, errCodeartsCookieFormat
			}
			k = strings.TrimSpace(k)
			v = strings.TrimSpace(v)
			if k == "" || v == "" {
				continue
			}
			checkValue := v
			if len(v) >= 2 && v[0] == '"' && v[len(v)-1] == '"' {
				checkValue = v[1 : len(v)-1]
			}
			if err := (&http.Cookie{Name: k, Value: checkValue}).Valid(); err != nil {
				return nil, errCodeartsCookieFormat
			}
			out = append(out, codearts.Cookie{
				Name: k, Value: v,
				// 统一挂 .huaweicloud.com：HTTP 直连路径本来就是一整行 Cookie 发出，
				// 不看 per-domain 归属（api.go 的 doAPI 就是这么拼的）。
				Domain: ".huaweicloud.com", Path: "/",
				// 显式标记「这份数据没有过期信息」：Cookie 请求头只有 name=value，
				// 拿不到 Expires/Max-Age。默认值恰好就是 false，但写出来是为了让后来者
				// 一眼看到这是**已知事实**（未观测），不是"0 秒过期"，也不是"会话级"。
				Expires: 0, ExpiresKnown: false, Session: false,
			})
		}
	}
	return out, nil
}

func codeartsImportFailure(err error) string {
	switch {
	case errors.Is(err, codearts.ErrSessionExpired):
		return "华为云会话已失效，或 Cookie 缺少关键登录票据。请重新登录后复制完整 Cookie。没有写入号池。"
	case err != nil:
		var networkErr net.Error
		if errors.As(err, &networkErr) && networkErr.Timeout() {
			return "连接华为云超时，请检查 NAS 网络后重试。不能据此判断 Cookie 已过期。没有写入号池。"
		}
		return "连接或验证华为云接口失败，请检查 NAS 网络或稍后重试。不能据此判断 Cookie 已过期。没有写入号池。"
	default:
		return "华为云接口暂未通过会话验证，请稍后重试。没有写入号池。"
	}
}
