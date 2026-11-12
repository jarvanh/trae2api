package server

import (
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"trae2api/internal/auth"
	"trae2api/internal/pool"
	"trae2api/internal/upstream"
)

// startPendingLogin 直接调用 adminLoginStart 建一条 pending，返回 TRAE 回调
// 会带回的 loginTraceID（= machineTraceID(machine,device)）。
func startPendingLogin(t *testing.T, h *Handler) string {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/admin/api/login", strings.NewReader(`{}`))
	rec := httptest.NewRecorder()
	h.adminLoginStart(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("start login code=%d body=%s", rec.Code, rec.Body)
	}
	var resp loginStartResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("parse login response: %v", err)
	}
	u, err := url.Parse(resp.LoginURL)
	if err != nil {
		t.Fatalf("parse login url: %v", err)
	}
	machine := u.Query().Get("machine_id")
	device := u.Query().Get("device_id")
	if machine == "" || device == "" {
		t.Fatalf("login url missing machine/device: %s", resp.LoginURL)
	}
	return machineTraceID(machine, device)
}

// assertNoFiles 断言 root 下没有任何普通文件（可穿越写盘的直接证据）。
func assertNoFiles(t *testing.T, root string) {
	t.Helper()
	var files []string
	_ = filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err == nil && !d.IsDir() {
			files = append(files, p)
		}
		return nil
	})
	if len(files) > 0 {
		t.Fatalf("unexpected files written: %v", files)
	}
}

// assertNoFilesOutside 断言 root 下所有普通文件都位于 allowedDir 内。
func assertNoFilesOutside(t *testing.T, root, allowedDir string) {
	t.Helper()
	prefix := allowedDir + string(os.PathSeparator)
	_ = filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err == nil && !d.IsDir() && !strings.HasPrefix(p, prefix) {
			t.Errorf("file written outside auth dir: %s", p)
		}
		return nil
	})
}

// pathUpstream 返回按请求路径分流的 fake upstream（登录闭环需要
// ExchangeToken / GetUserInfo 两个端点分别响应）。
func pathUpstream(fn func(r *http.Request) (status int, body string)) *upstream.Client {
	rt := roundTripFunc(func(r *http.Request) (*http.Response, error) {
		status, body := fn(r)
		return &http.Response{
			StatusCode: status,
			Header:     http.Header{"Content-Type": []string{"application/json"}},
			Body:       io.NopCloser(strings.NewReader(body)),
		}, nil
	})
	return &upstream.Client{
		HTTP:       &http.Client{Transport: rt},
		StreamHTTP: &http.Client{Transport: rt},
		AgentHost:  "https://fake.example",
		UgHost:     "https://fake.example",
		OAuthHost:  "https://fake.example",
		ClientID:   upstream.ClientID,
	}
}

func newSecurityHandler(t *testing.T, authDir string, up *upstream.Client, apiKey string) *Handler {
	t.Helper()
	return NewHandler(Config{
		Pool:     pool.New(""),
		Upstream: up,
		AuthDir:  authDir,
		APIKey:   apiKey,
		WorkMode: upstream.WorkModeDisabled,
	})
}

// TestAdminReadEndpointsRequireAuth 覆盖 T3：配置 key 后所有管理读接口
// 必须与写接口使用同一鉴权中间件，不能留只读后门。
func TestAdminReadEndpointsRequireAuth(t *testing.T) {
	authDir := t.TempDir()
	p := testPoolWith(&auth.Auth{UID: "u1", Nickname: "n", AccessToken: "at", ExpiresAt: 9999999999})
	up := newFakeUpstream(t, func(string) (int, string, bool) { return 200, `{}`, false })
	h := NewHandler(Config{
		Pool: p, Upstream: up, AuthDir: authDir, APIKey: "secret",
		WorkMode: upstream.WorkModeDisabled,
	})

	endpoints := []string{
		"/admin/api/credits",
		"/admin/api/models",
		"/admin/api/usage",
		"/admin/api/order",
		"/admin/api/accounts",
		"/admin/api/accounts/u1/json",
		"/admin/api/login/result?pending_id=nope",
	}
	for _, path := range endpoints {
		req := httptest.NewRequest(http.MethodGet, path, nil)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != http.StatusUnauthorized {
			t.Errorf("GET %s without key: code=%d want 401 body=%s", path, rec.Code, rec.Body)
		}
	}
	for _, path := range endpoints {
		req := httptest.NewRequest(http.MethodGet, path, nil)
		req.Header.Set("Authorization", "Bearer secret")
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code == http.StatusUnauthorized {
			t.Errorf("GET %s with key: unexpected 401 body=%s", path, rec.Body)
		}
	}
}

// TestAuthorizeRejectsWithoutPending 覆盖 T1：没有已发起的 pending 登录时，
// /authorize 必须拒绝，且绝不能落盘任何凭证文件（包括路径穿越目标）。
func TestAuthorizeRejectsWithoutPending(t *testing.T) {
	parent := t.TempDir()
	authDir := filepath.Join(parent, "auths")
	if err := os.MkdirAll(authDir, 0o755); err != nil {
		t.Fatal(err)
	}
	var upstreamCalls int32
	up := pathUpstream(func(r *http.Request) (int, string) {
		atomic.AddInt32(&upstreamCalls, 1)
		return 200, `{"Result":{"UserID":"u1","ScreenName":"n"}}`
	})
	h := newSecurityHandler(t, authDir, up, "key")

	q := url.Values{}
	q.Set("refreshToken", "rt")
	q.Set("userInfo", `{"UserID":"../../evil"}`)
	q.Set("loginTraceID", "deadbeefdeadbeef")
	req := httptest.NewRequest(http.MethodGet, "/authorize?"+q.Encode(), nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code == http.StatusOK {
		t.Fatalf("authorize without pending must not succeed: %s", rec.Body)
	}
	if got := atomic.LoadInt32(&upstreamCalls); got != 0 {
		t.Errorf("authorize without pending must not call upstream, calls=%d", got)
	}
	assertNoFiles(t, parent)
}

// TestAuthorizeFlowBindsPendingAndEscapesHTML 覆盖 T1/T5：合法 pending 流程
// 可以落盘，但 UID 只信 GetUserInfo 且必须合法；页面文本必须 HTML 转义；
// 同一 pending 不能被重放。
func TestAuthorizeFlowBindsPendingAndEscapesHTML(t *testing.T) {
	parent := t.TempDir()
	authDir := filepath.Join(parent, "auths")
	if err := os.MkdirAll(authDir, 0o755); err != nil {
		t.Fatal(err)
	}
	const evilNick = `<img src=x onerror=alert(1)>`
	up := pathUpstream(func(r *http.Request) (int, string) {
		switch r.URL.Path {
		case upstream.EpExchange:
			return 200, `{"Result":{"Token":"new-at","RefreshToken":"new-rt","TokenExpireAt":1786805537}}`
		case upstream.EpUserInfo:
			// 回调 query 里的 userInfo.UserID 是伪造值；最终 UID 只信这里。
			return 200, `{"Result":{"UserID":"user_1","ScreenName":"` + evilNick + `"}}`
		default:
			return 404, `{}`
		}
	})
	h := newSecurityHandler(t, authDir, up, "key")

	trace := startPendingLogin(t, h)
	q := url.Values{}
	q.Set("refreshToken", "rt")
	q.Set("userInfo", `{"UserID":"forged_uid"}`)
	q.Set("loginTraceID", trace)
	req := httptest.NewRequest(http.MethodGet, "/authorize?"+q.Encode(), nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("authorize code=%d body=%s", rec.Code, rec.Body)
	}
	body := rec.Body.String()
	if strings.Contains(body, evilNick) || strings.Contains(body, "<img") {
		t.Errorf("response contains unescaped HTML: %s", body)
	}
	if !strings.Contains(body, "&lt;img") {
		t.Errorf("expected escaped nickname in response: %s", body)
	}
	if _, err := os.Stat(filepath.Join(authDir, "trae-user_1.json")); err != nil {
		t.Fatalf("expected credential file for GetUserInfo uid: %v", err)
	}
	assertNoFilesOutside(t, parent, authDir)

	// 重放：同一 pending 已被消费，必须拒绝且不再落盘。
	req = httptest.NewRequest(http.MethodGet, "/authorize?"+q.Encode(), nil)
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code == http.StatusOK {
		t.Errorf("replayed callback must be rejected: %s", rec.Body)
	}
}

// TestAuthorizeRejectsInvalidUIDFromGetUserInfo 覆盖 T1：上游返回的 UID
// 也必须过白名单（防"合法登录流程 + 恶意上游响应"写出 auths 目录）。
func TestAuthorizeRejectsInvalidUIDFromGetUserInfo(t *testing.T) {
	parent := t.TempDir()
	authDir := filepath.Join(parent, "auths")
	if err := os.MkdirAll(authDir, 0o755); err != nil {
		t.Fatal(err)
	}
	up := pathUpstream(func(r *http.Request) (int, string) {
		switch r.URL.Path {
		case upstream.EpExchange:
			return 200, `{"Result":{"Token":"new-at","RefreshToken":"new-rt","TokenExpireAt":1786805537}}`
		case upstream.EpUserInfo:
			return 200, `{"Result":{"UserID":"../../evil","ScreenName":"n"}}`
		default:
			return 404, `{}`
		}
	})
	h := newSecurityHandler(t, authDir, up, "key")
	trace := startPendingLogin(t, h)

	q := url.Values{}
	q.Set("refreshToken", "rt")
	q.Set("loginTraceID", trace)
	req := httptest.NewRequest(http.MethodGet, "/authorize?"+q.Encode(), nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("invalid uid: code=%d want 400 body=%s", rec.Code, rec.Body)
	}
	assertNoFiles(t, parent)
}

// TestImportRejectsTraversalUID 覆盖 T6：导入接口不能凭 JSON 里的 uid 穿越写盘。
func TestImportRejectsTraversalUID(t *testing.T) {
	parent := t.TempDir()
	authDir := filepath.Join(parent, "auths")
	up := newFakeUpstream(t, func(string) (int, string, bool) { return 200, `{}`, false })
	h := newSecurityHandler(t, authDir, up, "key")

	flat := `{"accessToken":"at","refreshToken":"rt","uid":"../../evil"}`
	payload, _ := json.Marshal(map[string]string{"json": flat})
	req := httptest.NewRequest(http.MethodPost, "/admin/api/accounts/import", strings.NewReader(string(payload)))
	req.Header.Set("Authorization", "Bearer key")
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("import traversal uid: code=%d want 400 body=%s", rec.Code, rec.Body)
	}
	assertNoFiles(t, parent)
}

// TestDeleteRejectsTraversalUID 覆盖 T6/T14：DELETE 的 PathValue 已 URL 解码，
// 必须再走白名单，不能穿越删除 auths 之外的文件。
func TestDeleteRejectsTraversalUID(t *testing.T) {
	parent := t.TempDir()
	authDir := filepath.Join(parent, "auths")
	if err := os.MkdirAll(authDir, 0o755); err != nil {
		t.Fatal(err)
	}
	// 穿越目标：authDir 之外的受害者文件。
	victim := filepath.Join(parent, "victim.json")
	if err := os.WriteFile(victim, []byte("keep"), 0o600); err != nil {
		t.Fatal(err)
	}
	up := newFakeUpstream(t, func(string) (int, string, bool) { return 200, `{}`, false })
	h := newSecurityHandler(t, authDir, up, "")

	req := httptest.NewRequest(http.MethodDelete, "/admin/api/accounts/x", nil)
	req.SetPathValue("uid", "../victim")
	rec := httptest.NewRecorder()
	h.adminDeleteAccount(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("delete traversal uid: code=%d want 400 body=%s", rec.Code, rec.Body)
	}
	if _, err := os.Stat(victim); err != nil {
		t.Fatalf("victim file was deleted via traversal: %v", err)
	}
}

// TestCallbackHandlerOnlyExposesAuthorize 覆盖 T8：回调端口只挂 /authorize，
// 不得暴露管理/代理路由。
func TestCallbackHandlerOnlyExposesAuthorize(t *testing.T) {
	up := newFakeUpstream(t, func(string) (int, string, bool) { return 200, `{}`, false })
	h := newSecurityHandler(t, t.TempDir(), up, "key")
	cb := h.CallbackHandler()

	for _, path := range []string{"/admin/api/accounts", "/v1/models", "/status"} {
		req := httptest.NewRequest(http.MethodGet, path, nil)
		rec := httptest.NewRecorder()
		cb.ServeHTTP(rec, req)
		if rec.Code != http.StatusNotFound {
			t.Errorf("callback handler exposed %s: code=%d", path, rec.Code)
		}
	}
	req := httptest.NewRequest(http.MethodGet, "/authorize", nil)
	rec := httptest.NewRecorder()
	cb.ServeHTTP(rec, req)
	if rec.Code == http.StatusNotFound {
		t.Error("callback handler must route /authorize")
	}
}

// TestAuthorizeRenderEscapesHTML 直接验证渲染层的转义（T5）。
func TestAuthorizeRenderEscapesHTML(t *testing.T) {
	rec := httptest.NewRecorder()
	authorizeRender(rec, http.StatusOK, `<script>alert(1)</script>`, `"><img src=x onerror=alert(2)>`)
	body := rec.Body.String()
	if strings.Contains(body, "<script>alert(1)</script>") || strings.Contains(body, "<img src=x onerror") {
		t.Fatalf("unescaped dynamic HTML: %s", body)
	}
	if !strings.Contains(body, "&lt;script&gt;") || !strings.Contains(body, "&lt;img") {
		t.Errorf("expected escaped output: %s", body)
	}
}

// TestAdminCreditsFanoutBounded 覆盖 T4：单次 credits 请求只探测有限账号数。
func TestAdminCreditsFanoutBounded(t *testing.T) {
	var calls int32
	up := pathUpstream(func(r *http.Request) (int, string) {
		atomic.AddInt32(&calls, 1)
		return 200, `{}`
	})
	p := pool.New("")
	for i := 0; i < adminCreditsMaxAccounts+5; i++ {
		uid := fmt.Sprintf("u%d", i)
		p.Add(&auth.Auth{UID: uid, AccessToken: "at", ExpiresAt: 9999999999})
		p.SetCredits(uid, 100)
	}
	h := NewHandler(Config{Pool: p, Upstream: up, WorkMode: upstream.WorkModeDisabled})
	req := httptest.NewRequest(http.MethodGet, "/admin/api/credits", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("credits code=%d body=%s", rec.Code, rec.Body)
	}
	var resp struct {
		Truncated bool  `json:"truncated"`
		Accounts  []any `json:"accounts"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if !resp.Truncated {
		t.Errorf("expected truncated=true for >%d accounts", adminCreditsMaxAccounts)
	}
	if len(resp.Accounts) != adminCreditsMaxAccounts {
		t.Errorf("accounts=%d want %d", len(resp.Accounts), adminCreditsMaxAccounts)
	}
	// 每号 PackList + CheckinStatus 各一次，不得超出上限账号数。
	if got := atomic.LoadInt32(&calls); got != int32(adminCreditsMaxAccounts*2) {
		t.Errorf("upstream calls=%d want %d", got, adminCreditsMaxAccounts*2)
	}
}
