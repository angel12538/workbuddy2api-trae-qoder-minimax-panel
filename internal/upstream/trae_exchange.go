package upstream

import (
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"strings"
)

// TraeExchangeError contains only fixed descriptions and sanitized error codes.
// Neither upstream messages nor response bodies (which may contain tokens) are
// included, so the same error can safely be shown in the panel and server logs.
type TraeExchangeError struct {
	Reason     string
	HTTPStatus int
	APICode    string
}

func (e *TraeExchangeError) Error() string {
	detail := e.Reason
	if e.HTTPStatus != 0 {
		detail += fmt.Sprintf("，HTTP %d", e.HTTPStatus)
	}
	if e.APICode != "" {
		detail += "，code=" + e.APICode
	}
	return "ExchangeToken：" + detail
}

func traeExchangeNetworkError(err error) error {
	var network net.Error
	if errors.As(err, &network) && network.Timeout() {
		return &TraeExchangeError{Reason: "请求超时，请检查容器到 TRAE 的网络"}
	}
	return &TraeExchangeError{Reason: "网络或 TLS 连接失败，请检查容器网络"}
}

func safeTraeExchangeCode(raw json.RawMessage) string {
	var code string
	if json.Unmarshal(raw, &code) != nil {
		code = string(raw)
	}
	if code == "" || len(code) > 32 {
		return "未知"
	}
	for _, c := range code {
		if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c == '_' || c == '-') {
			return "未知"
		}
	}
	return code
}

func decodeTraeExchangeResponse(status int, raw []byte) (string, string, error) {
	if status < 200 || status >= 300 {
		return "", "", &TraeExchangeError{Reason: "接口返回 HTTP 错误", HTTPStatus: status}
	}
	var envelope map[string]json.RawMessage
	if json.Unmarshal(raw, &envelope) != nil || envelope == nil {
		return "", "", &TraeExchangeError{Reason: "接口返回了非预期的 JSON 响应", HTTPStatus: status}
	}
	var metadata struct {
		Error *struct {
			Code json.RawMessage `json:"Code"`
		} `json:"Error"`
	}
	if value, ok := envelope["ResponseMetadata"]; ok {
		if json.Unmarshal(value, &metadata) != nil {
			return "", "", &TraeExchangeError{Reason: "响应元数据格式异常", HTTPStatus: status}
		}
		if metadata.Error != nil {
			return "", "", &TraeExchangeError{Reason: "官方接口拒绝凭证，请重新获取授权链接并登录", HTTPStatus: status, APICode: safeTraeExchangeCode(metadata.Error.Code)}
		}
	}
	if value, ok := envelope["code"]; ok {
		code := safeTraeExchangeCode(value)
		if code != "0" {
			return "", "", &TraeExchangeError{Reason: "官方接口拒绝凭证，请重新授权", HTTPStatus: status, APICode: code}
		}
	}
	getString := func(fields map[string]json.RawMessage, keys ...string) string {
		for _, key := range keys {
			var value string
			if json.Unmarshal(fields[key], &value) == nil && strings.TrimSpace(value) != "" {
				return strings.TrimSpace(value)
			}
		}
		return ""
	}
	for _, key := range []string{"Result", "data"} {
		value, ok := envelope[key]
		if !ok {
			continue
		}
		var token string
		var fields map[string]json.RawMessage
		refresh := ""
		if json.Unmarshal(value, &token) != nil {
			if json.Unmarshal(value, &fields) != nil {
				continue
			}
			token = getString(fields, "AccessToken", "access_token", "Token", "token")
			refresh = getString(fields, "RefreshToken", "refresh_token", "refreshToken")
		}
		token = strings.TrimSpace(token)
		if token == "" {
			continue
		}
		if !strings.HasPrefix(token, "Cloud-IDE-JWT ") {
			token = "Cloud-IDE-JWT " + token
		}
		return token, refresh, nil
	}
	return "", "", &TraeExchangeError{Reason: "成功响应中缺少可识别的访问令牌", HTTPStatus: status}
}
