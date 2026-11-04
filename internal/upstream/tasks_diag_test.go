package upstream

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
)

// TestTasksListDiagMatchesResponse 诊断字段必须与真实响应逐项一致：
// http/body_bytes/top_keys/task_count，且默认口径与 mp 口径使用同一 base、
// 仅靠 X-Client-Platform 区分。
func TestTasksListDiagMatchesResponse(t *testing.T) {
	body := `{"code":0,"msg":"OK","data":{"tasks":[{"task_code":"chat_5"},{"task_code":"first_buddy"}],"total":2}}`
	var gotPlatform, gotUID string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPlatform = r.Header.Get("X-Client-Platform")
		gotUID = r.Header.Get("X-User-Id")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(body))
	}))
	defer srv.Close()

	c := &Client{HTTP: srv.Client(), ChatBaseCN: srv.URL, BillingBaseCN: srv.URL}
	a := &auth.Auth{UID: "u-diag", AccessToken: "at"}

	d := c.TasksListDiag(a, false)
	if d.HTTP != http.StatusOK {
		t.Fatalf("http=%d want 200", d.HTTP)
	}
	if d.BodyBytes != len(body) {
		t.Errorf("body_bytes=%d want %d（必须等于真实响应字节数）", d.BodyBytes, len(body))
	}
	if d.TaskCount != 2 {
		t.Errorf("task_count=%d want 2", d.TaskCount)
	}
	if got := strings.Join(d.TopKeys, ","); got != "tasks,total" {
		t.Errorf("top_keys=%q want [tasks total]（data 顶层键排序）", got)
	}
	if d.Error != "" {
		t.Errorf("error=%q want empty", d.Error)
	}
	if gotPlatform != "" {
		t.Errorf("默认口径不应带 X-Client-Platform，got %q", gotPlatform)
	}
	if gotUID != "u-diag" {
		t.Errorf("X-User-Id=%q want u-diag（与正式列表同 headers）", gotUID)
	}

	dMP := c.TasksListDiag(a, true)
	if gotPlatform != "miniprogram" {
		t.Errorf("mp 口径 X-Client-Platform=%q want miniprogram", gotPlatform)
	}
	if dMP.TaskCount != 2 || dMP.BodyBytes != len(body) {
		t.Errorf("mp 诊断应与默认口径同解析，got count=%d bytes=%d", dMP.TaskCount, dMP.BodyBytes)
	}
}

// TestTasksListDiagHTTPError 401/403 必须保留状态与真实错误，且错误文本脱敏。
func TestTasksListDiagHTTPError(t *testing.T) {
	body := `{"code":401,"msg":"invalid token","echo":"Bearer SECRET_TOKEN_VALUE"}`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(body))
	}))
	defer srv.Close()

	c := &Client{HTTP: srv.Client(), ChatBaseCN: srv.URL}
	d := c.TasksListDiag(&auth.Auth{UID: "u", AccessToken: "at"}, false)
	if d.HTTP != http.StatusUnauthorized {
		t.Errorf("http=%d want 401", d.HTTP)
	}
	if !strings.Contains(d.Error, "401") {
		t.Errorf("error=%q 应包含 HTTP 状态", d.Error)
	}
	if d.TaskCount != 0 {
		t.Errorf("task_count=%d want 0（HTTP 失败无任务数）", d.TaskCount)
	}
	if d.BodyBytes != len(body) {
		t.Errorf("body_bytes=%d want %d（失败响应也要记录字节数）", d.BodyBytes, len(body))
	}
	if strings.Contains(d.Error, "SECRET_TOKEN_VALUE") {
		t.Errorf("error 泄露 token：%q", d.Error)
	}
	if !strings.Contains(d.Error, "<redacted>") {
		t.Errorf("error=%q 应把 Bearer 凭据替换成 <redacted>", d.Error)
	}
}

// TestTasksListDiagMissingTasks data 对象存在但缺 tasks：必须报形态错误且列出顶层键。
func TestTasksListDiagMissingTasks(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"code":0,"msg":"OK","data":{"foo":1}}`))
	}))
	defer srv.Close()

	c := &Client{HTTP: srv.Client(), ChatBaseCN: srv.URL}
	d := c.TasksListDiag(&auth.Auth{UID: "u"}, false)
	if d.HTTP != http.StatusOK {
		t.Errorf("http=%d want 200", d.HTTP)
	}
	if !strings.Contains(d.Error, "缺少 tasks") {
		t.Errorf("error=%q 应指出缺少 tasks 字段", d.Error)
	}
	if got := strings.Join(d.TopKeys, ","); got != "foo" {
		t.Errorf("top_keys=%q want [foo]", got)
	}
	if d.TaskCount != 0 {
		t.Errorf("task_count=%d want 0", d.TaskCount)
	}
}

// TestTasksListDiagDataNull data:null 时把 data_type 记录到 Error（契约要求）。
func TestTasksListDiagDataNull(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"code":0,"msg":"OK","data":null}`))
	}))
	defer srv.Close()

	c := &Client{HTTP: srv.Client(), ChatBaseCN: srv.URL}
	d := c.TasksListDiag(&auth.Auth{UID: "u"}, false)
	if !strings.Contains(d.Error, "data_type=null") {
		t.Errorf("error=%q 应记录 data_type=null", d.Error)
	}
	if len(d.TopKeys) != 0 {
		t.Errorf("top_keys=%v want empty（data 非对象）", d.TopKeys)
	}
	if d.TaskCount != 0 {
		t.Errorf("task_count=%d want 0", d.TaskCount)
	}
}

// TestTasksListDiagBusinessCode 业务 code != 0 时保留 code/msg，不误报空任务。
func TestTasksListDiagBusinessCode(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"code":10001,"msg":"growth system is only available for personal users"}`))
	}))
	defer srv.Close()

	c := &Client{HTTP: srv.Client(), ChatBaseCN: srv.URL}
	d := c.TasksListDiag(&auth.Auth{UID: "u"}, false)
	if d.HTTP != http.StatusOK {
		t.Errorf("http=%d want 200", d.HTTP)
	}
	if !strings.Contains(d.Error, "code=10001") {
		t.Errorf("error=%q 应包含业务码", d.Error)
	}
}

// TestTasksListDiagNetworkError 网络错误：HTTP=0、Error 非空，不 panic。
func TestTasksListDiagNetworkError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	c := &Client{HTTP: srv.Client(), ChatBaseCN: srv.URL}
	srv.Close() // 先关闭，制造连接失败

	d := c.TasksListDiag(&auth.Auth{UID: "u"}, false)
	if d.HTTP != 0 {
		t.Errorf("http=%d want 0（网络错误无状态码）", d.HTTP)
	}
	if d.Error == "" {
		t.Error("网络错误必须写入 error")
	}
	if d.TaskCount != 0 || d.BodyBytes != 0 {
		t.Errorf("网络错误不应产生任务数/字节数，got count=%d bytes=%d", d.TaskCount, d.BodyBytes)
	}
}
