package server

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"trae2api/internal/auth"
	"trae2api/internal/pool"
	"trae2api/internal/upstream"
)

// 模拟 SOLO SSE 响应（glm-5.2 回答"你好"）。
const soloSSE = "event:metadata\ndata:{\"model\":\"\",\"session_id\":\"s1\"}\n\n" +
	"event:output\ndata:{\"response\":\"你好\",\"reasoning_content\":\"想一下\",\"tool_calls\":null}\n\n" +
	"event:token_usage\ndata:{\"prompt_tokens\":5,\"completion_tokens\":2,\"total_tokens\":7}\n\n" +
	"event:done\ndata:{\"finish_reason\":\"stop\"}\n\n"

// newFakeUpstream 返回 ChatStream 走 fake 的 upstream.Client。
func newFakeUpstream(t *testing.T, behavior func(auth string) (status int, body string, isStream bool)) *upstream.Client {
	t.Helper()
	return &upstream.Client{
		HTTP: &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
			authz := r.Header.Get("Authorization")
			status, body, isStream := behavior(authz)
			ct := "application/json"
			if isStream {
				ct = "text/event-stream"
			}
			return &http.Response{
				StatusCode: status,
				Header:     http.Header{"Content-Type": []string{ct}},
				Body:       io.NopCloser(strings.NewReader(body)),
			}, nil
		})},
		AgentHost: "https://fake.example",
		UgHost:    "https://fake.example",
		OAuthHost: "https://fake.example",
		ClientID:  upstream.ClientID,
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func testPoolWith(auths ...*auth.Auth) *pool.Pool {
	p := pool.New("")
	for _, a := range auths {
		p.Add(a)
		p.SetCredits(a.UID, 1000)
	}
	return p
}

func TestChatNonStreamAggregates(t *testing.T) {
	up := newFakeUpstream(t, func(authz string) (int, string, bool) {
		if authz != "Cloud-IDE-JWT at1" {
			t.Errorf("auth=%q", authz)
		}
		return 200, soloSSE, true
	})
	h := NewHandler(Config{
		Pool:     testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999}),
		Upstream: up,
	})
	req := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(`{"model":"glm-5.2","messages":[{"role":"user","content":"hi"}]}`))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body)
	}
	var resp map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("resp not json: %v body=%s", err, rec.Body)
	}
	if resp["object"] != "chat.completion" {
		t.Errorf("object=%v", resp["object"])
	}
	msg := resp["choices"].([]any)[0].(map[string]any)["message"].(map[string]any)
	if msg["content"] != "你好" {
		t.Errorf("content=%q", msg["content"])
	}
	if msg["reasoning_content"] != "想一下" {
		t.Errorf("reasoning=%q", msg["reasoning_content"])
	}
}

func TestChatStreamPassthrough(t *testing.T) {
	up := newFakeUpstream(t, func(authz string) (int, string, bool) {
		return 200, soloSSE, true
	})
	h := NewHandler(Config{
		Pool:     testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999}),
		Upstream: up,
	})
	req := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(`{"model":"glm-5.2","stream":true,"messages":[{"role":"user","content":"hi"}]}`))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("code=%d", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); !strings.Contains(ct, "text/event-stream") {
		t.Errorf("ct=%q", ct)
	}
	body := rec.Body.String()
	if !strings.Contains(body, "你好") || !strings.Contains(body, "data: [DONE]") {
		t.Errorf("body=%q", body)
	}
}

func TestChatRotatesOnPlanLimit(t *testing.T) {
	calls := map[string]int{}
	up := &upstream.Client{
		HTTP: &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
			// 只数对话请求；目录扇出（get_detail_param）不计入
			if strings.HasSuffix(r.URL.Path, "/api/agent/v3/llm_utils_chat") {
				calls[r.Header.Get("Authorization")]++
				if r.Header.Get("Authorization") == "Cloud-IDE-JWT at-bad" {
					return &http.Response{
						StatusCode: 200,
						Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
						Body:       io.NopCloser(strings.NewReader("event:error\ndata:{\"code\":1005,\"message\":\"plan limit\",\"extra\":{\"plan\":2}}\n\n")),
					}, nil
				}
				return &http.Response{
					StatusCode: 200,
					Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
					Body:       io.NopCloser(strings.NewReader(soloSSE)),
				}, nil
			}
			if strings.HasSuffix(r.URL.Path, "/api/ide/v1/get_detail_param") {
				return &http.Response{
					StatusCode: 200,
					Header:     http.Header{"Content-Type": []string{"application/json"}},
					Body:       io.NopCloser(strings.NewReader(modelsJSON("glm-5.2"))),
				}, nil
			}
			return &http.Response{StatusCode: 404, Header: http.Header{"Content-Type": []string{"application/json"}}, Body: io.NopCloser(strings.NewReader("{}"))}, nil
		})},
		AgentHost: "https://fake.example", UgHost: "https://fake.example",
		OAuthHost: "https://fake.example", ClientID: upstream.ClientID,
	}
	p := testPoolWith(
		&auth.Auth{UID: "bad", AccessToken: "at-bad", ExpiresAt: 9999999999},
		&auth.Auth{UID: "good", AccessToken: "at-good", ExpiresAt: 9999999999},
	)
	// 规则②「积分最少优先」→ bad 必须是最少那个才能被首轮选中；
	// 它撞 1005 plan_limit 后轮换到 good，正是本用例要验的行为。
	p.SetCredits("bad", 500)
	p.SetCredits("good", 1000)
	h := NewHandler(Config{Pool: p, Upstream: up})
	req := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(`{"model":"glm-5.2","messages":[]}`))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body)
	}
	if calls["Cloud-IDE-JWT at-bad"] != 1 || calls["Cloud-IDE-JWT at-good"] != 1 {
		t.Errorf("calls=%v", calls)
	}
}

// TestChatStreamCooldownOnStreamError 流式请求中上游 event:error（如 5xx/参数错误）
// 应触发 NoteError 冷却（而非仅透传不冷却）。
func TestChatStreamCooldownOnStreamError(t *testing.T) {
	up := newFakeUpstream(t, func(authz string) (int, string, bool) {
		return 200, "event:error\ndata:{\"code\":5001,\"message\":\"upstream broke\"}\n\n", true
	})
	p := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999})
	h := NewHandler(Config{Pool: p, Upstream: up})
	req := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(`{"model":"glm-5.2","stream":true,"messages":[]}`))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	// 流内错误不应中断整个请求的 SSE 输出（仍有 event:error + [DONE]）。
	body := rec.Body.String()
	if !strings.Contains(body, "solo error") || !strings.Contains(body, "data: [DONE]") {
		t.Errorf("body=%q want error event + [DONE]", body)
	}
	st, _ := p.Status("u1")
	if st.ErrCount == 0 {
		t.Errorf("stream error should bump errCount: %+v", st)
	}
}

func TestChatAllUnavailableReturns503(t *testing.T) {
	up := newFakeUpstream(t, func(authz string) (int, string, bool) {
		return 429, `rate limited`, false
	})
	h := NewHandler(Config{
		Pool:     testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999}),
		Upstream: up,
	})
	req := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(`{"model":"glm-5.2","messages":[]}`))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != 503 {
		t.Errorf("code=%d body=%s", rec.Code, rec.Body)
	}
	var e map[string]any
	json.Unmarshal(rec.Body.Bytes(), &e)
	if e["error"] == nil {
		t.Errorf("want error envelope: %s", rec.Body)
	}
}

func TestChatSessionDeadDisables(t *testing.T) {
	up := newFakeUpstream(t, func(authz string) (int, string, bool) {
		return 401, `{"code":1001,"msg":"login required"}`, false
	})
	p := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999})
	h := NewHandler(Config{Pool: p, Upstream: up})
	req := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(`{"model":"glm-5.2","messages":[]}`))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != 503 {
		t.Errorf("code=%d", rec.Code)
	}
	st, _ := p.Status("u1")
	if !st.Disabled {
		t.Errorf("account should be disabled: %+v", st)
	}
}

func TestChatUnknownModel400(t *testing.T) {
	h := NewHandler(Config{Pool: testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at", ExpiresAt: 9999999999}), Upstream: upstream.New()})
	req := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(`{"model":"does-not-exist-xyz","messages":[]}`))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != 400 {
		t.Errorf("code=%d body=%s", rec.Code, rec.Body)
	}
	var e map[string]any
	json.Unmarshal(rec.Body.Bytes(), &e)
	if e["error"] == nil {
		t.Errorf("want error envelope: %s", rec.Body)
	}
}

func TestModelsEndpoint(t *testing.T) {
	h := NewHandler(Config{Pool: testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at", ExpiresAt: 9999999999}), Upstream: upstream.New()})
	req := httptest.NewRequest("GET", "/v1/models", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("code=%d", rec.Code)
	}
	var resp map[string]any
	json.Unmarshal(rec.Body.Bytes(), &resp)
	if resp["object"] != "list" {
		t.Errorf("object=%v", resp["object"])
	}
	data := resp["data"].([]any)
	// 动态拉取在单测里必失败（无真实上游），走的是 staticModels 回退表 ——
	// 且回退同样要过展示口径（剔掉 custom_model_* / summary 等占位项），
	// 否则 /v1/models 与管理台两处计数会不一致。
	want := 0
	for _, m := range staticModels {
		if id, _ := m["id"].(string); !isInternalModel(id) {
			want++
		}
	}
	if len(data) != want {
		t.Errorf("models count=%d want %d (static=%d 去掉内部项)", len(data), want, len(staticModels))
	}
	if len(data) == 0 {
		t.Fatal("empty static fallback table")
	}
	found := false
	for _, m := range data {
		id, _ := m.(map[string]any)["id"].(string)
		if isInternalModel(id) {
			t.Errorf("内部/占位模型不应出现在 /v1/models：%s", id)
		}
		if id == "glm-5.2" {
			found = true
		}
	}
	if !found {
		t.Error("glm-5.2 missing")
	}
	// 回退表不得声明 context_length：上游不可用时宁可不写，也不伪造窗口值。
	for _, m := range data {
		mm := m.(map[string]any)
		if _, ok := mm["context_length"]; ok {
			t.Errorf("static fallback %v must not declare context_length", mm["id"])
		}
	}
}

func TestModelListDynamicContextLength(t *testing.T) {
	dynamicModelsCache.Lock()
	oldIDs, oldFetched, oldFail := dynamicModelsCache.ids, dynamicModelsCache.fetched, dynamicModelsCache.lastFail
	dynamicModelsCache.ids = []upstream.ModelInfo{
		{ID: "real", ContextWindow: 256000, PromptMaxTokens: 224000, MaxTokens: 32000},
		{ID: "unknown", ContextWindow: 0},
	}
	dynamicModelsCache.fetched = time.Now()
	dynamicModelsCache.lastFail = time.Time{}
	dynamicModelsCache.Unlock()
	defer func() {
		dynamicModelsCache.Lock()
		dynamicModelsCache.ids = oldIDs
		dynamicModelsCache.fetched = oldFetched
		dynamicModelsCache.lastFail = oldFail
		dynamicModelsCache.Unlock()
	}()

	h := NewHandler(Config{})
	byID := make(map[string]map[string]any)
	for _, m := range h.modelList() {
		byID[m["id"].(string)] = m
	}
	if got := byID["real"]["context_length"]; got != int64(256000) {
		t.Errorf("real context=%v want 256000", got)
	}
	if got := byID["real"]["max_input_tokens"]; got != int64(224000) {
		t.Errorf("real max_input=%v want 224000", got)
	}
	if got := byID["real"]["max_output_tokens"]; got != int64(32000) {
		t.Errorf("real max_output=%v want 32000", got)
	}
	if got, ok := byID["unknown"]["context_length"]; ok {
		t.Errorf("unknown context should be omitted, got %v", got)
	}
}

func TestAPIKeyAuth(t *testing.T) {
	h := NewHandler(Config{
		Pool:     testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at", ExpiresAt: 9999999999}),
		Upstream: upstream.New(),
		APIKey:   "test-key",
	})
	// 无 key → 401
	req := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(`{}`))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != 401 {
		t.Errorf("no key: code=%d", rec.Code)
	}
	// 错 key → 401
	req = httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(`{}`))
	req.Header.Set("Authorization", "Bearer wrong")
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != 401 {
		t.Errorf("wrong key: code=%d", rec.Code)
	}
	// 对 key → 200（models）
	req = httptest.NewRequest("GET", "/v1/models", nil)
	req.Header.Set("Authorization", "Bearer test-key")
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Errorf("right key: code=%d", rec.Code)
	}
}

func TestRequestBodyTooLarge(t *testing.T) {
	h := NewHandler(Config{Pool: testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at", ExpiresAt: 9999999999}), Upstream: upstream.New()})
	big := strings.Repeat("a", maxBodyBytes+1)
	req := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(big))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Errorf("code=%d body=%s", rec.Code, rec.Body)
	}
	var e map[string]any
	json.Unmarshal(rec.Body.Bytes(), &e)
	if e["error"] == nil {
		t.Errorf("want error envelope: %s", rec.Body)
	}
}

func TestAPIKeyAuthCaseInsensitivePrefix(t *testing.T) {
	h := NewHandler(Config{
		Pool:     testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at", ExpiresAt: 9999999999}),
		Upstream: upstream.New(),
		APIKey:   "test-key",
	})
	// 大小写不同的 Bearer 前缀也应接受（按规范，前缀大小写不敏感）。
	req := httptest.NewRequest("GET", "/v1/models", nil)
	req.Header.Set("Authorization", "bearer test-key")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Errorf("lowercase bearer: code=%d", rec.Code)
	}
	// 错误 key 仍拒绝。
	req = httptest.NewRequest("GET", "/v1/models", nil)
	req.Header.Set("Authorization", "Bearer wrong")
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != 401 {
		t.Errorf("wrong key: code=%d", rec.Code)
	}
}

func TestStatusEndpoint(t *testing.T) {
	p := testPoolWith(&auth.Auth{UID: "u1", Nickname: "nick", AccessToken: "at", ExpiresAt: 9999999999})
	p.SetCredits("u1", 42)
	h := NewHandler(Config{Pool: p, Upstream: upstream.New()})
	req := httptest.NewRequest("GET", "/status", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("code=%d", rec.Code)
	}
	body := rec.Body.String()
	if !strings.Contains(body, `"uid":"u1"`) || !strings.Contains(body, `"credits":42`) {
		t.Errorf("body=%s", body)
	}
	if strings.Contains(body, "AccessToken") || strings.Contains(body, `"at"`) {
		t.Error("token leaked in status output")
	}
}

func TestHealthz(t *testing.T) {
	h := NewHandler(Config{Pool: pool.New(""), Upstream: upstream.New()})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/healthz", nil))
	if rec.Code != 200 {
		t.Errorf("code=%d", rec.Code)
	}
}

// TestImportEmptyDeviceAutoAssign — 新增账号(JSON 导入, 无 deviceId)时自动生成 16 位数字
// deviceId 并固化(pool + 落盘文件)。是本仓库「新增账号随机 deviceId 固化」的核心行为。
func TestImportEmptyDeviceAutoAssign(t *testing.T) {
	authDir := t.TempDir()
	up := newFakeUpstream(t, func(authz string) (int, string, bool) {
		return 200, `{}`, false
	})
	h := NewHandler(Config{
		Pool:         pool.New(""),
		Upstream:     up,
		AuthDir:      authDir,
		APIKey:       "k",
		PlanCooldown: 12 * time.Hour,
		SoftCooldown: time.Minute,
		ErrThreshold: 3,
		ErrCooldown:  10 * time.Minute,
		DefaultModel: upstream.DefaultConfigName,
	})

	// 扁平 JSON（无 deviceId）作为 {"json": "<string>"} 里的字符串，走 JSON 导入路径
	flat := `{"accessToken":"at","refreshToken":"rt","uid":"u-new","nickname":"NewUser"}`
	flatEsc, _ := json.Marshal(flat)
	payload := `{"json":` + string(flatEsc) + `}`
	req := httptest.NewRequest("POST", "/admin/api/accounts/import", strings.NewReader(payload))
	req.Header.Set("Authorization", "Bearer k")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("import code=%d body=%s", rec.Code, rec.Body.String())
	}

	// pool 中的 Auth 应有 16 位数字 deviceId
	a := h.cfg.Pool.AuthByUID("u-new")
	if a == nil {
		t.Fatal("account not in pool")
	}
	if !regexp.MustCompile(`^[1-9][0-9]{15}$`).MatchString(a.DeviceID) {
		t.Fatalf("auto-assigned deviceId invalid: %q", a.DeviceID)
	}
	// 落盘文件也固化
	raw, err := os.ReadFile(filepath.Join(authDir, "trae-u-new.json"))
	if err != nil {
		t.Fatalf("read persisted file: %v", err)
	}
	parsed, err := auth.Parse(raw)
	if err != nil {
		t.Fatalf("parse persisted: %v", err)
	}
	if parsed.DeviceID != a.DeviceID {
		t.Fatalf("persisted deviceId mismatch: pool=%q file=%q", a.DeviceID, parsed.DeviceID)
	}
}

// 模拟 Work 原生 SSE 响应
const mockWorkSSE = "event: plan_item\n" +
	"data: {\"event\":\"plan_item\",\"payload\":{\"thought\":\"work plan\",\"reasoning_content\":\"thinking\"}}\n\n" +
	"event: output\n" +
	"data: {\"event\":\"output\",\"payload\":{\"choices\":[{\"text\":\"Hello from Work!\"}]}}\n\n" +
	"event: token_usage\n" +
	"data: {\"event\":\"token_usage\",\"payload\":{\"total_tokens\":42}}\n\n" +
	"event: done\n" +
	"data: {\"event\":\"done\"}\n\n"

func TestWorkModelRoutingAndStreaming(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == upstream.EpCreateAgentTask {
			if r.Header.Get("Authorization") != "Cloud-IDE-JWT at-work" {
				t.Errorf("expected Cloud-IDE-JWT at-work, got %s", r.Header.Get("Authorization"))
			}
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = w.Write([]byte(mockWorkSSE))
			return
		}
		if r.URL.Path == upstream.EpChat {
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = w.Write([]byte("data: {\"billing_mode\":\"credits\",\"cn_credits_remain_info\":{\"ide_credits\":0,\"work_credits\":888.0}}\n\n"))
			return
		}
		http.NotFound(w, r)
	}))
	defer ts.Close()

	workClient := upstream.NewWorkClient(upstream.WorkClientConfig{
		Host: ts.URL,
		Mode: upstream.WorkModeNative,
	})

	p := pool.New("")
	p.Add(&auth.Auth{UID: "u-work", AccessToken: "at-work", ExpiresAt: 9999999999})
	p.SetWorkCredits("u-work", 888.0)

	h := NewHandler(Config{
		Pool:       p,
		Upstream:   newFakeUpstream(t, func(auth string) (int, string, bool) { return 500, "solo should not be called", false }),
		WorkClient: workClient,
		WorkMode:   upstream.WorkModeNative,
	})

	req := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(`{"model":"DeepSeek-V4-Flash-Official","stream":true,"messages":[{"role":"user","content":"ping"}]}`))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != 200 {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body)
	}

	body := rec.Body.String()
	if !strings.Contains(body, "Hello from Work!") {
		t.Errorf("missing Work content in stream: %s", body)
	}
	if !strings.Contains(body, "[DONE]") {
		t.Errorf("missing [DONE] in stream: %s", body)
	}
}

func TestWorkModelMultiAccountRotationOnFailure(t *testing.T) {
	attempt := 0
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == upstream.EpCreateAgentTask {
			attempt++
			authz := r.Header.Get("Authorization")
			if authz == "Cloud-IDE-JWT at-acct1" {
				// 第一个账号模拟 429 报错
				w.WriteHeader(http.StatusTooManyRequests)
				_, _ = w.Write([]byte(`{"code":1004,"message":"rate limit exceeded"}`))
				return
			}
			if authz == "Cloud-IDE-JWT at-acct2" {
				// 第二个账号成功
				w.Header().Set("Content-Type", "text/event-stream")
				_, _ = w.Write([]byte(mockWorkSSE))
				return
			}
		}
		if r.URL.Path == upstream.EpChat {
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = w.Write([]byte("data: {\"billing_mode\":\"credits\",\"cn_credits_remain_info\":{\"ide_credits\":0,\"work_credits\":500.0}}\n\n"))
			return
		}
		http.NotFound(w, r)
	}))
	defer ts.Close()

	workClient := upstream.NewWorkClient(upstream.WorkClientConfig{
		Host: ts.URL,
		Mode: upstream.WorkModeNative,
	})

	p := pool.New("")
	// 账号 1 积分更高 (1000)，优先被挑选
	p.Add(&auth.Auth{UID: "acct1", AccessToken: "at-acct1", ExpiresAt: 9999999999})
	p.SetWorkCredits("acct1", 1000.0)

	// 账号 2 积分次之 (500)
	p.Add(&auth.Auth{UID: "acct2", AccessToken: "at-acct2", ExpiresAt: 9999999999})
	p.SetWorkCredits("acct2", 500.0)

	h := NewHandler(Config{
		Pool:         p,
		Upstream:     newFakeUpstream(t, func(auth string) (int, string, bool) { return 500, "solo should not be called", false }),
		WorkClient:   workClient,
		WorkMode:     upstream.WorkModeNative,
		SoftCooldown: 1 * time.Minute,
	})

	req := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(`{"model":"DeepSeek-V4-Flash-Official","stream":false,"messages":[{"role":"user","content":"test"}]}`))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != 200 {
		t.Fatalf("expected 200 after rotation, got %d: %s", rec.Code, rec.Body)
	}

	if attempt != 2 {
		t.Errorf("expected 2 attempts across accounts, got %d", attempt)
	}

	// 验证 acct1 已被标记 WorkCooling
	st1, _ := p.Status("acct1")
	if !st1.WorkCooling {
		t.Errorf("acct1 should be cooling for work, got %+v", st1)
	}

	var resp map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("invalid json: %v", err)
	}
	choices := resp["choices"].([]any)
	firstChoice := choices[0].(map[string]any)
	msg := firstChoice["message"].(map[string]any)
	if !strings.Contains(msg["content"].(string), "Hello from Work!") {
		t.Errorf("unexpected content: %v", msg["content"])
	}
}

func TestWorkModeDisabled(t *testing.T) {
	h := NewHandler(Config{
		Pool:     testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at", ExpiresAt: 9999999999}),
		Upstream: newFakeUpstream(t, func(auth string) (int, string, bool) { return 200, soloSSE, true }),
		WorkMode: upstream.WorkModeDisabled,
	})

	req := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(`{"model":"DeepSeek-V4-Flash-Official","messages":[{"role":"user","content":"hi"}]}`))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusForbidden {
		t.Fatalf("expected 403 when work mode is disabled, got %d: %s", rec.Code, rec.Body)
	}
	if !strings.Contains(rec.Body.String(), "work_disabled") {
		t.Errorf("expected error code work_disabled, got %s", rec.Body)
	}
}

func TestSoloFallsBackToWorkOnPlanLimit(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == upstream.EpCreateAgentTask {
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = w.Write([]byte(mockWorkSSE))
			return
		}
		if r.URL.Path == upstream.EpChat {
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = w.Write([]byte("data: {\"billing_mode\":\"credits\",\"cn_credits_remain_info\":{\"ide_credits\":0,\"work_credits\":50.0}}\n\n"))
			return
		}
		http.NotFound(w, r)
	}))
	defer ts.Close()

	workClient := upstream.NewWorkClient(upstream.WorkClientConfig{
		Host: ts.URL,
		Mode: upstream.WorkModeNative,
	})

	// SOLO 上游模拟 1005 (plan 权益不足)
	soloUp := newFakeUpstream(t, func(auth string) (int, string, bool) {
		return 400, `{"code":1005,"msg":"plan limit reached"}`, false
	})

	p := pool.New("")
	p.Add(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999})
	p.SetCredits("u1", 0)
	p.SetWorkCredits("u1", 50.0)

	h := NewHandler(Config{
		Pool:       p,
		Upstream:   soloUp,
		WorkClient: workClient,
		WorkMode:   upstream.WorkModeNative,
	})

	req := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(`{"model":"glm-5.2","messages":[{"role":"user","content":"hi"}]}`))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != 200 {
		t.Fatalf("expected fallback to 200, got %d: %s", rec.Code, rec.Body)
	}

	var resp map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &resp)
	if resp["model"] != "glm-5.2" {
		t.Errorf("expected model=glm-5.2 in response, got %v", resp["model"])
	}
}

func TestAdminCreditsIncludesWorkCredits(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == upstream.EpChat {
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = w.Write([]byte("data: {\"billing_mode\":\"credits\",\"cn_credits_remain_info\":{\"ide_credits\":0,\"work_credits\":666.66}}\n\n"))
			return
		}
		http.NotFound(w, r)
	}))
	defer ts.Close()

	workClient := upstream.NewWorkClient(upstream.WorkClientConfig{
		Host: ts.URL,
		Mode: upstream.WorkModeNative,
	})

	p := pool.New("")
	p.Add(&auth.Auth{UID: "u1", Nickname: "TestUser", AccessToken: "at", ExpiresAt: 9999999999})

	h := NewHandler(Config{
		Pool:       p,
		Upstream:   upstream.New(),
		WorkClient: workClient,
		WorkMode:   upstream.WorkModeNative,
	})

	req := httptest.NewRequest("GET", "/admin/api/credits", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != 200 {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body)
	}

	var data map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &data); err != nil {
		t.Fatalf("unmarshal error: %v", err)
	}
	accts := data["accounts"].([]any)
	if len(accts) != 1 {
		t.Fatalf("expected 1 account, got %d", len(accts))
	}
	first := accts[0].(map[string]any)
	if first["work_credits"] != 666.66 {
		t.Errorf("expected work_credits 666.66, got %v", first["work_credits"])
	}
}

func TestSessionAffinityRouting(t *testing.T) {
	var chatAuths []string
	p := pool.New("")
	p.Add(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999})
	p.Add(&auth.Auth{UID: "u2", AccessToken: "at2", ExpiresAt: 9999999999})
	p.SetCredits("u1", 100)
	p.SetCredits("u2", 200)
	up := &upstream.Client{
		HTTP: &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
			if strings.HasSuffix(r.URL.Path, upstream.EpChat) {
				chatAuths = append(chatAuths, r.Header.Get("Authorization"))
				return &http.Response{
					StatusCode: 200,
					Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
					Body:       io.NopCloser(strings.NewReader(soloSSE)),
				}, nil
			}
			return &http.Response{
				StatusCode: 200,
				Header:     http.Header{"Content-Type": []string{"application/json"}},
				Body:       io.NopCloser(strings.NewReader(`{}`)),
			}, nil
		})},
	}

	h := NewHandler(Config{
		Pool:         p,
		Upstream:     up,
		DefaultModel: "glm-5.2",
	})

	// 轮次 1：包含首条消息
	req1 := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(`{"model":"glm-5.2","stream":true,"messages":[{"role":"user","content":"turn 1 hello"}]}`))
	rec1 := httptest.NewRecorder()
	h.ServeHTTP(rec1, req1)
	if rec1.Code != 200 {
		t.Fatalf("req1 failed: %d", rec1.Code)
	}

	// 此时动态提升 u1 的积分为 999 —— 按规则②（积分最少优先），
	// 无粘性时第二轮本应漂移到 u2(at2)
	p.SetCredits("u1", 999)

	// 轮次 2：相同对话流后续提问（带相同的首轮消息特征）
	req2 := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(`{"model":"glm-5.2","stream":true,"messages":[{"role":"user","content":"turn 1 hello"},{"role":"assistant","content":"hi"},{"role":"user","content":"turn 2"}]}`))
	rec2 := httptest.NewRecorder()
	h.ServeHTTP(rec2, req2)
	if rec2.Code != 200 {
		t.Fatalf("req2 failed: %d", rec2.Code)
	}

	if len(chatAuths) != 2 {
		t.Fatalf("expected 2 chat requests, got %d", len(chatAuths))
	}
	// 验证第二轮由于会话特征指纹粘性，仍锁定在首轮选中的账号上。
	// 未探测过期 → 规则①打平 → 规则②「积分最少」→ 首轮选 u1(at1)；
	// 第二轮 u1 被抬到 999 后若无知会漂移到 u2，粘性必须压住这个漂移。
	if chatAuths[0] != "Cloud-IDE-JWT at1" || chatAuths[1] != "Cloud-IDE-JWT at1" {
		t.Fatalf("affinity failed: got %v, expected both to be at1 (least credits on first pick)", chatAuths)
	}
}

// TestChatBusyDoesNotRotate 验证 502/504（边缘网关全局拥堵）不会轮换账号：
// 上游只应被调用一次，直接快速失败给调用方，而不是把 1 个请求放大成 N 个上游请求。
func TestChatBusyDoesNotRotate(t *testing.T) {
	var calls int32
	up := newFakeUpstreamPath(t, &calls, func(authz string) (int, string, bool) {
		if authz == "" {
			t.Errorf("missing auth header")
		}
		return 502, `<html><h1>502 Bad Gateway</h1></html><hr/>Powered by volc-dcdn`, false
	})
	// 两个健康账号：若发生轮换，calls 会 >1
	h := NewHandler(Config{
		Pool: testPoolWith(
			&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999},
			&auth.Auth{UID: "u2", AccessToken: "at2", ExpiresAt: 9999999999},
		),
		Upstream: up,
	})
	req := httptest.NewRequest("POST", "/v1/chat/completions",
		strings.NewReader(`{"model":"glm-5.2","messages":[{"role":"user","content":"hi"}]}`))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if got := atomic.LoadInt32(&calls); got != 1 {
		t.Fatalf("upstream called %d times, want 1 (busy must not rotate)", got)
	}
	if rec.Code != 502 {
		t.Errorf("code=%d want 502, body=%s", rec.Code, rec.Body)
	}
	if rec.Header().Get("Retry-After") == "" {
		t.Errorf("missing Retry-After header")
	}
	var resp map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("body not json: %v", err)
	}
	if errObj, ok := resp["error"].(map[string]any); !ok || errObj["code"] != "upstream_busy" {
		t.Errorf("error payload = %v, want code upstream_busy", resp["error"])
	}
	// 账号不应因全局拥堵被冷却/禁用
	for _, uid := range []string{"u1", "u2"} {
		if st, ok := h.cfg.Pool.Status(uid); ok && (st.Cooling || st.Disabled) {
			t.Errorf("account %s must stay healthy on global busy: %+v", uid, st)
		}
	}
}

// newFakeUpstreamPath 同 newFakeUpstream，但只统计聊天端点（llm_utils_chat）的调用次数：
// 排除 get_detail_param 等模型列表拉取，避免把既有行为误判为「轮换」。
func newFakeUpstreamPath(t *testing.T, chatCalls *int32, behavior func(auth string) (int, string, bool)) *upstream.Client {
	t.Helper()
	return &upstream.Client{
		HTTP: &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
			if strings.HasSuffix(r.URL.Path, "/api/agent/v3/llm_utils_chat") {
				atomic.AddInt32(chatCalls, 1)
			}
			status, body, isStream := behavior(r.Header.Get("Authorization"))
			ct := "application/json"
			if isStream {
				ct = "text/event-stream"
			}
			return &http.Response{
				StatusCode: status,
				Header:     http.Header{"Content-Type": []string{ct}},
				Body:       io.NopCloser(strings.NewReader(body)),
			}, nil
		})},
		AgentHost: "https://fake.example",
		UgHost:    "https://fake.example",
		OAuthHost: "https://fake.example",
		ClientID:  upstream.ClientID,
	}
}

// resetModelCache 清掉进程级模型缓存，让单个用例自己控制拉取。
func resetModelCache(t *testing.T) {
	t.Helper()
	dynamicModelsCache.Lock()
	dynamicModelsCache.ids = nil
	dynamicModelsCache.fetched = time.Time{}
	dynamicModelsCache.lastFail = time.Time{}
	dynamicModelsCache.refreshing = false
	dynamicModelsCache.Unlock()
	t.Cleanup(func() {
		dynamicModelsCache.Lock()
		dynamicModelsCache.ids = nil
		dynamicModelsCache.fetched = time.Time{}
		dynamicModelsCache.lastFail = time.Time{}
		dynamicModelsCache.refreshing = false
		dynamicModelsCache.Unlock()
	})
}

func modelsJSON(ids ...string) string {
	var b strings.Builder
	b.WriteString(`{"config_info_list":[`)
	for i, id := range ids {
		if i > 0 {
			b.WriteString(",")
		}
		b.WriteString(`{"config_name":"` + id + `","context_window_tokens":{"dev":1000000},` +
			`"model_detail_list":[{"model_name":"` + id + `__max","prompt_max_tokens":936000,"max_tokens":64000}]}`)
	}
	b.WriteString("]}")
	return b.String()
}

// TestFetchModelsIntersectionAcrossAccounts 覆盖 2026-10-27 主人定稿：模型目录取
// 跨账号交集 —— 列表是「任何健康账号都能接」的承诺，并集会把只属于个别账号的
// 模型挂出去（实测 free/pro/flash 对话上游 4001）。每号能力仍各自写回 pool，
// 选号过滤按单号能力进行（交集只影响目录，不影响能力路由）。
func TestFetchModelsIntersectionAcrossAccounts(t *testing.T) {
	resetModelCache(t)
	var calls int32
	up := &upstream.Client{
		HTTP: &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
			atomic.AddInt32(&calls, 1)
			var body string
			switch r.Header.Get("Authorization") {
			case "Cloud-IDE-JWT at1":
				body = modelsJSON("glm-5.3", "free")
			case "Cloud-IDE-JWT at2":
				body = modelsJSON("glm-5.3", "pro")
			default:
				body = `{"config_info_list":[]}`
			}
			return &http.Response{
				StatusCode: 200,
				Header:     http.Header{"Content-Type": []string{"application/json"}},
				Body:       io.NopCloser(strings.NewReader(body)),
			}, nil
		})},
		AgentHost: "https://fake.example", UgHost: "https://fake.example",
		OAuthHost: "https://fake.example", ClientID: upstream.ClientID,
	}
	p := testPoolWith(
		&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999},
		&auth.Auth{UID: "u2", AccessToken: "at2", ExpiresAt: 9999999999},
	)
	h := NewHandler(Config{Pool: p, Upstream: up})

	infos := h.fetchDynamicModels()
	byID := map[string]upstream.ModelInfo{}
	for _, mi := range infos {
		byID[mi.ID] = mi
	}
	// 交集：只有两号都有的 glm-5.3 进目录；单号独占的 free/pro 不挂列表
	for _, id := range []string{"glm-5.3"} {
		if _, ok := byID[id]; !ok {
			t.Errorf("交集缺少模型 %s（got %d 条）", id, len(infos))
		}
	}
	for _, id := range []string{"free", "pro"} {
		if _, ok := byID[id]; ok {
			t.Errorf("仅单号持有的 %s 不应出现在交集目录", id)
		}
	}
	if n := atomic.LoadInt32(&calls); n != 2 {
		t.Errorf("应按账号各拉一次，calls=%d", n)
	}
	// 每号能力仍写回 pool：free 只有 u1 能接，选号仍按单号能力过滤
	if got := p.PickAffinityForModel("", nil, "free"); got == nil || got.UID != "u1" {
		t.Errorf("选号未按能力过滤，got=%v want u1", got)
	}
	if got := p.PickAffinityForModel("", nil, "pro"); got == nil || got.UID != "u2" {
		t.Errorf("选号未按能力过滤，got=%v want u2", got)
	}
}

// TestModelListHidesInternalConfigs 覆盖展示口径统一：上游把 custom_model_* /
// summary 标成「可见」但实为占位项，/v1/models 与管理台都必须隐藏；
// 同时 knownModel 仍要接受它们（只影响展示，不影响路由）。
func TestModelListHidesInternalConfigs(t *testing.T) {
	resetModelCache(t)
	up := &upstream.Client{
		HTTP: &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
			return &http.Response{
				StatusCode: 200,
				Header:     http.Header{"Content-Type": []string{"application/json"}},
				Body: io.NopCloser(strings.NewReader(`{"config_info_list":[
					{"config_name":"glm-5.3","context_window_tokens":{"dev":1000000},
					 "model_detail_list":[{"model_name":"g__max","prompt_max_tokens":936000,"max_tokens":64000}]},
					{"config_name":"custom_model_1M","context_window_tokens":{"dev":1000000},
					 "model_detail_list":[{"model_name":"c","prompt_max_tokens":936000,"max_tokens":64000}]},
					{"config_name":"summary","context_window_tokens":{"dev":200000},
					 "model_detail_list":[{"model_name":"s","prompt_max_tokens":168000,"max_tokens":32000}]},
					{"config_name":"title_gen","is_invisible_to_user":true,"context_window_tokens":{"dev":200000},
					 "model_detail_list":[{"model_name":"t","prompt_max_tokens":168000,"max_tokens":32000}]}
				]}`)),
			}, nil
		})},
		AgentHost: "https://fake.example", UgHost: "https://fake.example",
		OAuthHost: "https://fake.example", ClientID: upstream.ClientID,
	}
	h := NewHandler(Config{
		Pool: testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999}), Upstream: up,
	})

	got := map[string]bool{}
	for _, m := range h.modelList() {
		got[m["id"].(string)] = true
	}
	if !got["glm-5.3"] {
		t.Error("真实模型应展示")
	}
	for _, hidden := range []string{"custom_model_1M", "summary", "title_gen"} {
		if got[hidden] {
			t.Errorf("%s 是内部/占位配置，不应出现在 /v1/models", hidden)
		}
	}
	// 路由口径不变宽于展示口径：knownModel 仍接受这些 config
	for _, id := range []string{"custom_model_1M", "summary", "title_gen"} {
		if !h.knownModel(id) {
			t.Errorf("knownModel(%s) 应为 true：展示过滤不得影响路由", id)
		}
	}
}

// TestChatInvalidParamDoesNotRotate 覆盖 2026-10-27 实弹事故：目录里有但上游
// 对话不认的模型（free/pro/flash）回 4001 invalid param，这是账号无关的 4xx ——
// 换号必然复现，还会把整池打冷却。必须立即透传 400，不冷却、不计错、不轮换。
func TestChatInvalidParamDoesNotRotate(t *testing.T) {
	var calls int32
	// 目录端点正常返回（free 在并集表里），对话端点回 4001 invalid param ——
	// 复刻生产实弹：模型列表说有、对话上游不认。
	up := &upstream.Client{
		HTTP: &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
			if strings.HasSuffix(r.URL.Path, "/api/ide/v1/get_detail_param") {
				return &http.Response{
					StatusCode: 200,
					Header:     http.Header{"Content-Type": []string{"application/json"}},
					Body:       io.NopCloser(strings.NewReader(modelsJSON("free"))),
				}, nil
			}
			if strings.HasSuffix(r.URL.Path, "/api/agent/v3/llm_utils_chat") {
				atomic.AddInt32(&calls, 1)
				// 生产实弹形态：HTTP 200 + SSE 流内 event:error code=4001
				return &http.Response{
					StatusCode: 200,
					Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
					Body:       io.NopCloser(strings.NewReader("event:error\ndata:{\"code\":4001,\"message\":\"We're sorry, the param is invalid. Please try with a valid param.\"}\n\n")),
				}, nil
			}
			return &http.Response{StatusCode: 404, Header: http.Header{"Content-Type": []string{"application/json"}}, Body: io.NopCloser(strings.NewReader("{}"))}, nil
		})},
		AgentHost: "https://fake.example", UgHost: "https://fake.example",
		OAuthHost: "https://fake.example", ClientID: upstream.ClientID,
	}
	p := testPoolWith(
		&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999},
		&auth.Auth{UID: "u2", AccessToken: "at2", ExpiresAt: 9999999999},
	)
	h := NewHandler(Config{Pool: p, Upstream: up})
	resetModelCache(t)

	req := httptest.NewRequest("POST", "/v1/chat/completions",
		strings.NewReader(`{"model":"free","messages":[{"role":"user","content":"hi"}]}`))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	t.Logf("calls=%d code=%d body=%s", atomic.LoadInt32(&calls), rec.Code, rec.Body.String())
	if got := atomic.LoadInt32(&calls); got != 1 {
		t.Fatalf("upstream called %d times, want 1 (account-independent 4xx must not rotate)", got)
	}
	if rec.Code != 400 {
		t.Errorf("code=%d want 400, body=%s", rec.Code, rec.Body)
	}
	var resp map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("body not json: %v", err)
	}
	if errObj, ok := resp["error"].(map[string]any); !ok || errObj["code"] != "invalid_param" {
		t.Errorf("error payload = %v, want code invalid_param", resp["error"])
	}
	// 池不受损：两号都不冷却不禁用，后续正常模型仍可选到任一号
	if got := p.Pick(); got == nil {
		t.Fatal("池应保持健康：账号无关 4xx 不得冷却任何账号")
	}
}

// TestChatPlanGateBlocksModelNotAccount 覆盖 2026-10-27 实弹事故：kimi-k3.1-pro
// 在交集目录里、但对话上游对所有号回 1005，而各号账本有 3000+ 积分 —— 这是
// 模型级套餐门禁，不是账号耗尽。旧行为把 3 个号全打进 12h 冷却（整池自残）；
// 现在账本有钱吃 1005 → 只拉黑 (号,模型)，账号对其他模型照常可用。
func TestChatPlanGateBlocksModelNotAccount(t *testing.T) {
	resetModelCache(t)
	var calls int32
	up := &upstream.Client{
		HTTP: &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
			if strings.HasSuffix(r.URL.Path, "/api/ide/v1/get_detail_param") {
				return &http.Response{
					StatusCode: 200,
					Header:     http.Header{"Content-Type": []string{"application/json"}},
					Body:       io.NopCloser(strings.NewReader(modelsJSON("kimi-k3.1-pro", "glm-5.3"))),
				}, nil
			}
			if strings.HasSuffix(r.URL.Path, "/api/agent/v3/llm_utils_chat") {
				atomic.AddInt32(&calls, 1)
				// 按上游 body 里的 config_name 分流：kimi-k3.1-pro 永远 1005，其余正常。
				// 剥 __max 后缀再比：门禁是模型级，max 档同样被拦（裸名请求在
				// auto 策略下会被网关改写成 <model>__max + max 开关）。
				raw, _ := io.ReadAll(r.Body)
				var peek struct {
					ConfigName string `json:"config_name"`
				}
				_ = json.Unmarshal(raw, &peek)
				if strings.TrimSuffix(peek.ConfigName, "__max") == "kimi-k3.1-pro" {
					return &http.Response{
						StatusCode: 200,
						Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
						Body: io.NopCloser(strings.NewReader(
							"event:error\ndata:{\"code\":1005,\"message\":\"plan limit\",\"extra\":{\"plan\":2}}\n\n")),
					}, nil
				}
				return &http.Response{
					StatusCode: 200,
					Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
					Body:       io.NopCloser(strings.NewReader(soloSSE)),
				}, nil
			}
			return &http.Response{StatusCode: 404, Header: http.Header{"Content-Type": []string{"application/json"}}, Body: io.NopCloser(strings.NewReader("{}"))}, nil
		})},
		AgentHost: "https://fake.example", UgHost: "https://fake.example",
		OAuthHost: "https://fake.example", ClientID: upstream.ClientID,
	}
	p := testPoolWith(
		&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999},
		&auth.Auth{UID: "u2", AccessToken: "at2", ExpiresAt: 9999999999},
	)
	p.SetCredits("u1", 3000)
	p.SetCredits("u2", 3000)
	// 关掉 Work 兜底：不显式禁用时 NewHandler 会自动创建真 WorkClient，
	// solo 轮转穷尽后 executeWorkRequest 拿假 token 打真实上游吃 401，
	// 把两个号 Disable("work session dead")——测的是夹具泄漏而非拉黑语义。
	h := NewHandler(Config{Pool: p, Upstream: up, WorkMode: upstream.WorkModeDisabled})

	// 第一次：kimi-k3.1-pro 轮完全部号都 1005 → 拉黑而非冷却
	req := httptest.NewRequest("POST", "/v1/chat/completions",
		strings.NewReader(`{"model":"kimi-k3.1-pro","messages":[{"role":"user","content":"hi"}]}`))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if got := atomic.LoadInt32(&calls); got != 2 {
		t.Errorf("每号各试一次，calls=%d want 2", got)
	}
	for _, uid := range []string{"u1", "u2"} {
		st, _ := p.Status(uid)
		if st.Cooling {
			t.Errorf("%s 不应被冷却（账本有钱，1005 是模型门禁）", uid)
		}
	}

	// 第二次：同模型直接 503，不再打上游（全部 (号,模型) 已拉黑）
	atomic.StoreInt32(&calls, 0)
	req2 := httptest.NewRequest("POST", "/v1/chat/completions",
		strings.NewReader(`{"model":"kimi-k3.1-pro","messages":[{"role":"user","content":"hi"}]}`))
	rec2 := httptest.NewRecorder()
	h.ServeHTTP(rec2, req2)
	if got := atomic.LoadInt32(&calls); got != 0 {
		t.Errorf("拉黑后不应再打上游，calls=%d", got)
	}
	if rec2.Code != http.StatusServiceUnavailable {
		t.Errorf("code=%d want 503, body=%s", rec2.Code, rec2.Body)
	}

	// 第三次：同池换 glm-5.3 仍正常 —— 拉黑只挡 kimi-k3.1-pro 一个模型，
	// 账号没被冷却/禁用，正常模型照常走通上游拿到 200。
	req3 := httptest.NewRequest("POST", "/v1/chat/completions",
		strings.NewReader(`{"model":"glm-5.3","messages":[{"role":"user","content":"hi"}]}`))
	rec3 := httptest.NewRecorder()
	h.ServeHTTP(rec3, req3)
	if got := atomic.LoadInt32(&calls); got == 0 {
		t.Fatal("glm-5.3 应仍能选到账号发起上游调用（拉黑不得殃及账号）")
	}
	if rec3.Code != http.StatusOK {
		t.Errorf("glm-5.3 code=%d want 200, body=%s", rec3.Code, rec3.Body)
	}
}

// 2026-10-30 主人需求：裸名请求在 auto 策略 + 目录确认含 __max 档时默认升级 max 档。
// 请求体必须 model=<config>__max（config_name 保持裸名，带后缀 4001）且
// extra_info.v2_max_mode_enabled=true（10-26 实测配方，大输入下缺一都被 4022 拒）。
func TestChatBareModelAutoUpgradesToMaxTier(t *testing.T) {
	resetModelCache(t)
	var chatBody []byte
	up := &upstream.Client{
		HTTP: &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
			if strings.HasSuffix(r.URL.Path, upstream.EpModels) {
				return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": []string{"application/json"}},
					Body: io.NopCloser(strings.NewReader(modelsJSON("glm-5.3")))}, nil
			}
			chatBody, _ = io.ReadAll(r.Body)
			return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": []string{"text/event-stream"}},
				Body: io.NopCloser(strings.NewReader(soloSSE))}, nil
		})},
		AgentHost: "https://fake.example", UgHost: "https://fake.example",
		OAuthHost: "https://fake.example", ClientID: upstream.ClientID,
	}
	p := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999})
	h := NewHandler(Config{Pool: p, Upstream: up, WorkMode: upstream.WorkModeDisabled})

	req := httptest.NewRequest("POST", "/v1/chat/completions",
		strings.NewReader(`{"model":"glm-5.3","messages":[{"role":"user","content":"hi"}]}`))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body)
	}
	var body map[string]any
	if err := json.Unmarshal(chatBody, &body); err != nil {
		t.Fatalf("chat body not json: %v", err)
	}
	if body["config_name"] != "glm-5.3" || body["model"] != "glm-5.3__max" {
		t.Errorf("config_name 应保持裸名、model 应升级 __max: config_name=%v model=%v", body["config_name"], body["model"])
	}
	extra, ok := body["extra_info"].(map[string]any)
	if !ok || extra["v2_max_mode_enabled"] != true {
		t.Errorf("应注入 extra_info.v2_max_mode_enabled=true, got %v", body["extra_info"])
	}
}

// 显式点名 __max 但目录无该档 → 400 不发上游（静默降级 dev 会把超 200K 请求烧成 4022）；
// 显式 __dev 与无 max 档的裸名 → 保持 dev 档，不注入 max 开关。
func TestChatExplicitTierWithoutMaxRejected(t *testing.T) {
	resetModelCache(t)
	var chatBody []byte
	chatCalled := false
	up := &upstream.Client{
		HTTP: &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
			if strings.HasSuffix(r.URL.Path, upstream.EpModels) {
				body := `{"config_info_list":[{"config_name":"onlydev","context_window_tokens":{"dev":200000},` +
					`"model_detail_list":[{"model_name":"onlydev__dev","prompt_max_tokens":168000,"max_tokens":32000}]}]}`
				return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": []string{"application/json"}},
					Body: io.NopCloser(strings.NewReader(body))}, nil
			}
			chatCalled = true
			chatBody, _ = io.ReadAll(r.Body)
			return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": []string{"text/event-stream"}},
				Body: io.NopCloser(strings.NewReader(soloSSE))}, nil
		})},
		AgentHost: "https://fake.example", UgHost: "https://fake.example",
		OAuthHost: "https://fake.example", ClientID: upstream.ClientID,
	}
	p := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999})
	h := NewHandler(Config{Pool: p, Upstream: up, WorkMode: upstream.WorkModeDisabled})

	req := httptest.NewRequest("POST", "/v1/chat/completions",
		strings.NewReader(`{"model":"onlydev__max","messages":[{"role":"user","content":"hi"}]}`))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("code=%d want 400, body=%s", rec.Code, rec.Body)
	}
	if chatCalled {
		t.Errorf("点名档不存在不应打上游")
	}
	var errResp struct {
		Error struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &errResp)
	if errResp.Error.Code != "invalid_request" || !strings.Contains(errResp.Error.Message, "__max") {
		t.Errorf("error = %+v, want invalid_request + __max 提示", errResp.Error)
	}

	// 同一目录：裸名 onlydev（无 max 档）保持 dev 档，不加开关
	req2 := httptest.NewRequest("POST", "/v1/chat/completions",
		strings.NewReader(`{"model":"onlydev","messages":[{"role":"user","content":"hi"}]}`))
	rec2 := httptest.NewRecorder()
	h.ServeHTTP(rec2, req2)
	if rec2.Code != http.StatusOK {
		t.Fatalf("code=%d body=%s", rec2.Code, rec2.Body)
	}
	var body map[string]any
	if err := json.Unmarshal(chatBody, &body); err != nil {
		t.Fatalf("chat body not json: %v", err)
	}
	if body["config_name"] != "onlydev" || body["model"] != "onlydev" {
		t.Errorf("无 max 档应保持裸名: config_name=%v model=%v", body["config_name"], body["model"])
	}
	if _, has := body["extra_info"]; has {
		t.Errorf("dev 档不应注入 extra_info, got %v", body["extra_info"])
	}
}

// SOLO 1005 回退 Work 前必须剥离 max 改写：Work（native/bridge）无档位概念，
// bridge 透传原始 body，残留 __max 名/开关可能被桥接端误消费。
func TestChatWorkFallbackStripsMaxRewrite(t *testing.T) {
	resetModelCache(t)
	var taskBody []byte
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == upstream.EpCreateAgentTask {
			taskBody, _ = io.ReadAll(r.Body)
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = w.Write([]byte(mockWorkSSE))
			return
		}
		http.NotFound(w, r)
	}))
	defer ts.Close()

	up := &upstream.Client{
		HTTP: &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
			if strings.HasSuffix(r.URL.Path, upstream.EpModels) {
				return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": []string{"application/json"}},
					Body: io.NopCloser(strings.NewReader(modelsJSON("glm-5.3")))}, nil
			}
			return &http.Response{StatusCode: 400, Header: http.Header{"Content-Type": []string{"application/json"}},
				Body: io.NopCloser(strings.NewReader(`{"code":1005,"message":"plan limit exceeded"}`))}, nil
		})},
		AgentHost: "https://fake.example", UgHost: "https://fake.example",
		OAuthHost: "https://fake.example", ClientID: upstream.ClientID,
	}
	p := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999})
	p.SetWorkCredits("u1", 888.0)
	workClient := upstream.NewWorkClient(upstream.WorkClientConfig{Host: ts.URL, Mode: upstream.WorkModeNative})
	h := NewHandler(Config{Pool: p, Upstream: up, WorkClient: workClient, WorkMode: upstream.WorkModeNative})

	req := httptest.NewRequest("POST", "/v1/chat/completions",
		strings.NewReader(`{"model":"glm-5.3","stream":true,"messages":[{"role":"user","content":"hi"}]}`))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body)
	}
	if !strings.Contains(rec.Body.String(), "Hello from Work!") {
		t.Errorf("missing Work content: %s", rec.Body.String())
	}
	task := string(taskBody)
	if strings.Contains(task, "__max") || strings.Contains(task, "v2_max_mode_enabled") {
		t.Errorf("work 回退请求泄漏 max 改写: %s", task)
	}
}
