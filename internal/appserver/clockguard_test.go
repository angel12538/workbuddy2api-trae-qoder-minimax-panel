package appserver

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// TestMeasureClockSkewDetectsOffset 证明这道闸真的测得出偏差。
//
// 假上游把 Date 头写成"比本机快 3 小时"，measureClockSkew 必须报出 +3h（正=本机慢了）。
// 这台机器实测慢 8 小时（Cloudflare/华为 auth 都在 13:48 GMT 时本机显示 13:48+0800），
// 靠的就是这个 Date 头比对。
func TestMeasureClockSkewDetectsOffset(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Date", time.Now().Add(3*time.Hour).UTC().Format(http.TimeFormat))
		w.WriteHeader(http.StatusUnauthorized) // 业务码无关，Date 照样有效
	}))
	defer srv.Close()

	old := clockSkewSources
	clockSkewSources = []string{srv.URL + "/me"}
	defer func() { clockSkewSources = old }()

	skew, n, err := measureClockSkew(context.Background())
	if err != nil {
		t.Fatalf("measureClockSkew: %v", err)
	}
	if n != 1 {
		t.Fatalf("应取到 1 个源，得到 %d", n)
	}
	if d := skew - 3*time.Hour; d < -time.Minute || d > time.Minute {
		t.Fatalf("应测出本机慢约 3 小时（signed≈+3h），得到 %v", skew)
	}
}

// TestMeasureClockSkewSurvivesOneBadSource 单个源乱指不该把结论带偏（取中位数）。
func TestMeasureClockSkewSurvivesOneBadSource(t *testing.T) {
	good := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Date", time.Now().UTC().Format(http.TimeFormat))
		w.WriteHeader(http.StatusOK)
	}))
	defer good.Close()
	bad := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Date", "完全不是时间")
		w.WriteHeader(http.StatusOK)
	}))
	defer bad.Close()
	alsoGood := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Date", time.Now().UTC().Format(http.TimeFormat))
		w.WriteHeader(http.StatusOK)
	}))
	defer alsoGood.Close()

	old := clockSkewSources
	clockSkewSources = []string{good.URL, bad.URL, alsoGood.URL}
	defer func() { clockSkewSources = old }()

	skew, n, err := measureClockSkew(context.Background())
	if err != nil {
		t.Fatalf("measureClockSkew: %v", err)
	}
	if n != 2 {
		t.Fatalf("坏的那个源不该计数，得到 n=%d", n)
	}
	if skew > time.Minute || skew < -time.Minute {
		t.Fatalf("两个正常源应判基本一致，得到 %v", skew)
	}
}
