package upstream

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
)

func TestTraeExchangeCurrentAndLegacyResponses(t *testing.T) {
	for _, sample := range []struct{ name, body, token, refresh string }{
		{"official-result-string", `{"ResponseMetadata":{"Error":null},"Result":"jwt.payload.signature"}`, "Cloud-IDE-JWT jwt.payload.signature", ""},
		{"result-object", `{"ResponseMetadata":{},"Result":{"Token":"jwt.payload.signature","RefreshToken":"rotated"}}`, "Cloud-IDE-JWT jwt.payload.signature", "rotated"},
		{"legacy", `{"code":0,"data":{"access_token":"jwt.payload.signature","refresh_token":"rotated"}}`, "Cloud-IDE-JWT jwt.payload.signature", "rotated"},
		{"legacy-prefix", `{"code":0,"data":{"token":"Cloud-IDE-JWT jwt.payload.signature"}}`, "Cloud-IDE-JWT jwt.payload.signature", ""},
	} {
		t.Run(sample.name, func(t *testing.T) {
			token, refresh, err := decodeTraeExchangeResponse(200, []byte(sample.body))
			if err != nil || token != sample.token || refresh != sample.refresh {
				t.Fatalf("decode mismatch: %v", err)
			}
		})
	}
}

func TestTraeExchangeErrorsNeverExposeCredentials(t *testing.T) {
	const secret = "CREDENTIAL_MUST_NOT_APPEAR"
	for _, sample := range []struct {
		name       string
		status     int
		body, code string
	}{
		{"official-invalid-token", 200, `{"ResponseMetadata":{"Error":{"Code":"20101","Message":"` + secret + `","Data":{"token":"` + secret + `"}}}}`, "20101"},
		{"error-overrides-result", 200, `{"ResponseMetadata":{"Error":{"Code":"20101"}},"Result":"` + secret + `"}`, "20101"},
		{"legacy-error", 200, `{"code":123,"message":"` + secret + `","data":{"token":"` + secret + `"}}`, "123"},
		{"html", 200, `<html>` + secret + `</html>`, ""},
		{"http-error", 401, secret, ""},
		{"unknown-format", 200, `{"secret":"` + secret + `"}`, ""},
		{"empty-result", 200, `{"Result":null}`, ""},
	} {
		t.Run(sample.name, func(t *testing.T) {
			_, _, err := decodeTraeExchangeResponse(sample.status, []byte(sample.body))
			if err == nil {
				t.Fatal("error response accepted")
			}
			if strings.Contains(err.Error(), secret) {
				t.Fatal("credential exposed")
			}
			if sample.code != "" && !strings.Contains(err.Error(), "code="+sample.code) {
				t.Fatal("diagnostic code missing")
			}
		})
	}
}

type traeExchangeTestTransport func(*http.Request) (*http.Response, error)

func (f traeExchangeTestTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestTraeExchangeTokenWireFormat(t *testing.T) {
	client := &http.Client{Transport: traeExchangeTestTransport(func(r *http.Request) (*http.Response, error) {
		if r.URL.String() != traeOAuthExchangeURL || r.Method != "POST" {
			t.Fatal("incorrect exchange endpoint")
		}
		var body map[string]string
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatal(err)
		}
		if body["ClientID"] != traeOAuthClientID || body["RefreshToken"] != "test-refresh" || body["ClientSecret"] != "-" {
			t.Fatal("incorrect exchange request")
		}
		return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(`{"ResponseMetadata":{},"Result":"a.b.c"}`)), Header: make(http.Header)}, nil
	})}
	token, _, err := traeExchangeTokenWithClient("test-refresh", client)
	if err != nil || token != "Cloud-IDE-JWT a.b.c" {
		t.Fatalf("official response failed through HTTP path: %v", err)
	}
}

func TestTraeExchangeNetworkErrorSanitized(t *testing.T) {
	client := &http.Client{Transport: traeExchangeTestTransport(func(*http.Request) (*http.Response, error) {
		return nil, fmt.Errorf("private token in low-level error")
	})}
	_, _, err := traeExchangeTokenWithClient("private-refresh", client)
	if err == nil || strings.Contains(err.Error(), "private") {
		t.Fatal("network diagnostics exposed unsafe details")
	}
}
