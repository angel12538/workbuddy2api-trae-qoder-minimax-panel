package panel

import (
	"errors"
	"net/url"
	"strings"
)

var errDoubaoCookiePaste = errors.New("Cookie 格式不正确：支持完整 Cookie 值、Cookie 请求头或整段请求标头；请勿在单个凭证中间换行。未保存账号。")
var errDoubaoCookieHost = errors.New("这段请求标头的目标域名不是豆包，请选择 www.doubao.com 的请求后重新复制。origin / referer 不是请求目标域名。未保存账号。")
var errDoubaoCookieMissing = errors.New("这段请求标头没有 Cookie，请先在豆包网页完成登录，刷新后重新复制包含 Cookie 的请求标头。未保存账号。")

func copiedHeaderName(s string) bool {
	if strings.HasPrefix(s, ":") {
		s = s[1:]
	}
	if s == "" {
		return false
	}
	for _, c := range s {
		if c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || strings.ContainsRune("!#$%&'*+-.^_`|~", c) {
			continue
		}
		return false
	}
	return true
}

// DevTools copies either "name: value" or "name\nvalue", including HTTP/2
// pseudo-headers. The delimiter inside a Cookie value must never be treated as
// a request-header delimiter.
func copiedHeaderLine(line string) (name, value string, ok bool) {
	start := 0
	if strings.HasPrefix(line, ":") {
		start = 1
	}
	i := strings.IndexByte(line[start:], ':')
	if i < 0 {
		return "", "", false
	}
	i += start
	if !copiedHeaderName(line[:i]) {
		return "", "", false
	}
	return strings.ToLower(line[:i]), strings.TrimSpace(line[i+1:]), true
}

func commonCopiedHeader(name string) bool {
	name = strings.ToLower(name)
	if strings.HasPrefix(name, ":") || strings.HasPrefix(name, "sec-") || strings.HasPrefix(name, "x-") {
		return true
	}
	switch name {
	case "host", "accept", "accept-encoding", "accept-language", "authorization", "cache-control", "connection", "content-length", "content-type", "dnt", "origin", "pragma", "priority", "referer", "user-agent", "upgrade-insecure-requests", "if-modified-since", "if-none-match":
		return true
	}
	return false
}

func doubaoCookieHostAllowed(value string) bool {
	u, err := url.Parse("https://" + strings.TrimSpace(value))
	if err != nil || u.User != nil || u.Path != "" || u.RawQuery != "" || u.Fragment != "" {
		return false
	}
	host := strings.TrimSuffix(strings.ToLower(u.Hostname()), ".")
	return host == "doubao.com" || strings.HasSuffix(host, ".doubao.com")
}

func parseDoubaoCookieInput(raw string) (map[string]string, error) {
	for _, c := range raw {
		if c == 0x7f || c < 0x20 && c != '\r' && c != '\n' && c != '\t' {
			return nil, errDoubaoCookiePaste
		}
	}
	raw = strings.TrimPrefix(strings.TrimSpace(raw), "\ufeff")
	lines := strings.Split(strings.NewReplacer("\r\n", "\n", "\r", "\n").Replace(raw), "\n")
	firstCookie := -1
	requestContext := false
	for i, original := range lines {
		line := strings.TrimSpace(original)
		name, value, inline := copiedHeaderLine(line)
		if !inline && copiedHeaderName(line) {
			name = strings.ToLower(line)
		}
		if name == "cookie" && firstCookie < 0 {
			firstCookie = i
		}
		if name == "host" || name == ":authority" {
			requestContext = true
			if !inline && i+1 < len(lines) {
				value = strings.TrimSpace(lines[i+1])
			}
			if !doubaoCookieHostAllowed(value) {
				return nil, errDoubaoCookieHost
			}
		}
		if name == ":method" || name == ":path" || name == ":scheme" || copiedRequestLine(line) {
			requestContext = true
		}
	}
	var values []string
	if firstCookie < 0 {
		if requestContext {
			return nil, errDoubaoCookieMissing
		}
		values = []string{raw} // Plain Cookie values retain the strict existing parser.
	} else {
		for i := firstCookie; i < len(lines); i++ {
			line := strings.TrimSpace(lines[i])
			if line == "" {
				continue
			}
			if strings.EqualFold(line, "cookie") {
				continue
			}
			if name, value, inline := copiedHeaderLine(line); inline {
				if name != "cookie" {
					continue
				}
				line = value
				if line == "" {
					continue
				}
			} else if copiedHeaderName(line) && (requestContext || commonCopiedHeader(line)) {
				// A split-column header owns its following value, even if that
				// value looks like "u=1" or another Cookie pair.
				if i+1 >= len(lines) || strings.TrimSpace(lines[i+1]) == "" {
					return nil, errDoubaoCookiePaste
				}
				i++
				continue
			}
			values = append(values, line)
		}
	}
	if len(values) == 0 {
		return nil, errDoubaoCookieMissing
	}
	pairs, err := parseCodeartsCookieInput(strings.Join(values, "\n"))
	if err != nil {
		return nil, errDoubaoCookiePaste
	}
	cookies := make(map[string]string, len(pairs))
	for _, c := range pairs {
		cookies[c.Name] = c.Value
	}
	return cookies, nil
}

func copiedRequestLine(line string) bool {
	fields := strings.Fields(line)
	if len(fields) != 3 || !strings.HasPrefix(fields[2], "HTTP/") {
		return false
	}
	switch fields[0] {
	case "GET", "POST", "HEAD", "PUT", "PATCH", "DELETE", "OPTIONS", "CONNECT", "TRACE":
		return true
	}
	return false
}
