// client.go SOLO 上游客户端：llm_utils_chat / get_detail_param / ExchangeToken /
// checkin_credits / ide_user_ent_usage + 错误分类。
package upstream

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"sort"
	"strings"
	"time"

	"trae2api/internal/auth"
)

// ErrKind 错误分类，pool 据此决定冷却时长（SPEC §4.3）。
type ErrKind int

const (
	ErrNone         ErrKind = iota // 成功
	ErrPlanLimit                   // 1005 + plan → 权益不足（硬冷却 12h）
	ErrSoftRate                    // 429 → 短冷却 60s
	ErrSessionDead                 // 401 + Cloud-IDE-JWT 失效 → 禁用
	ErrNotFound                    // 404 → 短冷却 60s 不累计 errCount
	ErrServer                      // 5xx
	ErrClient                      // 其他 4xx
	ErrUpstreamBusy                // 502/504 边缘网关（volc-dcdn/tengine）→ 全局性拥堵，换号无用
)

func (k ErrKind) String() string {
	switch k {
	case ErrPlanLimit:
		return "plan_limit"
	case ErrSoftRate:
		return "soft_rate"
	case ErrSessionDead:
		return "session_dead"
	case ErrNotFound:
		return "not_found"
	case ErrServer:
		return "server"
	case ErrClient:
		return "client"
	case ErrUpstreamBusy:
		return "upstream_busy"
	default:
		return "none"
	}
}

// Error 带分类的上游错误。
type Error struct {
	Kind   ErrKind
	Status int
	Msg    string
}

func (e *Error) Error() string {
	return fmt.Sprintf("upstream %s (http %d): %s", e.Kind, e.Status, e.Msg)
}

// CheckinError 签到接口 HTTP 200 但业务码非 0 的业务错误。
// 9074「当前参与用户太多」= 当前设备号被风控标记，需换代换号而非硬重试。
type CheckinError struct {
	Code int
	Msg  string
}

func (e *CheckinError) Error() string {
	return fmt.Sprintf("%d %s", e.Code, e.Msg)
}

// IsCheckinBusy 判定错误是否为签到 9074 限流（设备号被标记）。
func IsCheckinBusy(err error) bool {
	var ce *CheckinError
	if errors.As(err, &ce) {
		return ce.Code == CheckinBusyCode
	}
	return false
}

var sessionDeadMarkers = []string{"login", "token 失效", "token invalid", "session", "unauthorized", "401"}

// Classify 按 HTTP 状态码 + body 判定错误类别（SPEC §4.3）。
func Classify(status int, body string) ErrKind {
	lower := strings.ToLower(body)
	// 1005 plan 权益不足
	if strings.Contains(body, `"code":1005`) || (strings.Contains(body, "1005") && strings.Contains(lower, "plan")) {
		return ErrPlanLimit
	}
	// session 失效
	if status == http.StatusUnauthorized {
		for _, m := range sessionDeadMarkers {
			if strings.Contains(lower, strings.ToLower(m)) {
				return ErrSessionDead
			}
		}
		return ErrSessionDead
	}
	if status == http.StatusTooManyRequests {
		return ErrSoftRate
	}
	if status == http.StatusNotFound {
		return ErrNotFound
	}
	// 502/504 是火山边缘网关（volc-dcdn/tengine）在算力紧张时掐断连接的产物：
	// 属全局性拥堵，与账号无关，换号只会放大上游流量，故单列一类由调用方快速失败。
	if status == http.StatusBadGateway || status == http.StatusGatewayTimeout {
		return ErrUpstreamBusy
	}
	if status >= 500 {
		return ErrServer
	}
	if status >= 400 {
		return ErrClient
	}
	return ErrNone
}

// Client SOLO 上游 HTTP 客户端。Host 字段可覆盖便于测试。
type Client struct {
	// HTTP 用于短 JSON 请求（ExchangeToken/模型/签到/积分），有总超时兜底。
	HTTP *http.Client
	// StreamHTTP 用于 SSE 流式对话：不设总超时，避免长流被截断；
	// 通过 Transport.ResponseHeaderTimeout 兜底「上游一直不返回首字节」的悬挂。
	// 与 HTTP 共享同一 Transport（连接池复用）。nil 时 ChatStream 回退 HTTP。
	StreamHTTP *http.Client

	AgentHost string // https://trae-api-cn.mchost.guru
	UgHost    string // https://api.trae.cn
	OAuthHost string // https://api.trae.com.cn
	ClientID  string // en1oxy7wnw8j9n
	// Function 上游 function 通道；空 = 用默认常量 Function（solo_agent）。
	// solo_agent 带 max 档（glm-5.3 = 1M/936K/64K），solo_work_lite 只有 200K/168K/32K。
	Function string
}

// New 生产默认值。配置连接池减少 TLS 握手。
func New() *Client {
	tr := &http.Transport{
		MaxIdleConns:          100,
		MaxIdleConnsPerHost:   20,
		IdleConnTimeout:       90 * time.Second,
		ResponseHeaderTimeout: 120 * time.Second, // 首字节兜底（长推理预留），不限制整流时长
	}
	return &Client{
		HTTP:       &http.Client{Timeout: 120 * time.Second, Transport: tr},
		StreamHTTP: &http.Client{Transport: tr}, // 无总超时
		AgentHost:  AgentHost,
		UgHost:     UgHost,
		OAuthHost:  OAuthHost,
		ClientID:   ClientID,
		Function:   Function,
	}
}

func (c *Client) agentBase() string { return c.AgentHost }

// function 返回生效的 function 通道名（未设置时回落默认常量）。
func (c *Client) function() string {
	if f := strings.TrimSpace(c.Function); f != "" {
		return f
	}
	return Function
}

// FunctionSupportsMaxTier 判定当前 function 通道是否带 __max 档。
// 2026-10-26 实测：solo_agent / chat_v3 有 __max 档（glm-5.3 = 1M/936K/64K），
// solo_work_lite 无（只有 200K/168K/32K）。未收录的通道一律视为无 max 档，
// 保守不改写 —— 改写错档位上游会按 dev 档 4022 拒。
// 覆盖通道用 upstream.upstream_function 配置 / TW2A_FUNCTION 回滚。
func (c *Client) FunctionSupportsMaxTier() bool {
	return c.function() == FunctionHasMaxTier
}

func (c *Client) ugBase() string    { return c.UgHost }
func (c *Client) oauthBase() string { return c.OAuthHost }

// doJSONLimit 响应体上限。solo_agent 的 get_detail_param 实测 1.44MB（48 配置时
// 从未踩到旧 1MB 上限），静默截断会让 JSON 解析报「unexpected end of JSON input」
// 这种看起来像上游炸了的错，故直接放到 8MB（与请求体 maxBodyBytes 同量级）。
const doJSONLimit = 8 << 20

// doJSON 发请求并解 JSON；HTTP 非 2xx 时返回带 body 片段的 *Error。
func (c *Client) doJSON(req *http.Request) (json.RawMessage, error) {
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, doJSONLimit))
	if resp.StatusCode >= 400 {
		kind := Classify(resp.StatusCode, string(raw))
		return nil, &Error{Kind: kind, Status: resp.StatusCode, Msg: truncate(string(raw), 200)}
	}
	return raw, nil
}

// RefreshToken 通过 ExchangeToken 强制刷新 access token（refreshToken 轮换）。
// 成功时更新 a 的字段；调用方负责 SaveAtomic。全程持 a 写锁。
func (c *Client) RefreshToken(a *auth.Auth) error {
	a.Lock()
	defer a.Unlock()
	return c.refreshLocked(a)
}

// RefreshTokenIfNeeded 仅当 token 在 skew 内即将过期（或已过期）时才刷新，
// 返回是否真正刷新。持锁内重查，避免并发请求对同一账号重复 ExchangeToken 轮换。
// 调用方仅在 returned 为 true 时需要 SaveAtomic。
func (c *Client) RefreshTokenIfNeeded(a *auth.Auth, skew time.Duration) (bool, error) {
	a.Lock()
	defer a.Unlock()
	if !a.NeedsRefreshLocked(skew) {
		return false, nil
	}
	if err := c.refreshLocked(a); err != nil {
		return false, err
	}
	return true, nil
}

// refreshLocked 是 RefreshToken 的持锁内部实现；调用方必须已持有 a 写锁。
// 任何失败路径都不改写 a 字段，保证旧 refreshToken 可重试。
func (c *Client) refreshLocked(a *auth.Auth) error {
	if strings.TrimSpace(a.RefreshToken) == "" {
		return fmt.Errorf("no refreshToken")
	}
	host := a.ApiHost
	if host == "" {
		host = c.oauthBase()
	}
	body := map[string]any{
		"ClientID":     c.ClientID,
		"RefreshToken": a.RefreshToken, // 已持 a 写锁，直接读
		"ClientSecret": "-",
		"UserID":       "",
	}
	raw, _ := json.Marshal(body)
	req, err := http.NewRequest(http.MethodPost, host+EpExchange, bytes.NewReader(raw))
	if err != nil {
		return err
	}
	OAuthHeaders(req)
	data, err := c.doJSON(req)
	if err != nil {
		return err
	}
	var resp struct {
		Result struct {
			Token               string `json:"Token"`
			TokenExpireAt       int64  `json:"TokenExpireAt"`
			TokenExpireDuration int64  `json:"TokenExpireDuration"`
			RefreshToken        string `json:"RefreshToken"`
			RefreshExpireAt     int64  `json:"RefreshExpireAt"`
		} `json:"Result"`
	}
	if err := json.Unmarshal(data, &resp); err != nil {
		return fmt.Errorf("exchange parse: %w", err)
	}
	if resp.Result.Token == "" {
		return fmt.Errorf("refresh_failed: no token in response — re-login required")
	}
	a.AccessToken = resp.Result.Token
	if resp.Result.RefreshToken != "" {
		a.RefreshToken = resp.Result.RefreshToken
	}
	// 过期时间：优先 TokenExpireAt（上游返回毫秒，需归一化为 Unix 秒）
	if resp.Result.TokenExpireAt > 0 {
		a.ExpiresAt = normalizeExpiresAt(resp.Result.TokenExpireAt)
	} else if resp.Result.TokenExpireDuration > 0 {
		a.ExpiresAt = time.Now().Add(time.Duration(resp.Result.TokenExpireDuration) * time.Second).Unix()
	}
	return nil
}

// normalizeExpiresAt 把 ExchangeToken 的 TokenExpireAt 归一化为 Unix 秒。
// 上游返回毫秒（如 1786847930141），auth 文件用秒（1786847930）。
// 毫秒时间戳 ~1.7e12，秒时间戳 ~1.7e9，用 1e12 区分。
func normalizeExpiresAt(v int64) int64 {
	if v > 1e12 {
		return v / 1000
	}
	return v
}

// ChatStream 发 llm_utils_chat 请求并返回原始 SSE body 流（调用方负责 Close）。
// ctx 用于客户端断连时立即取消上游请求，避免下游放弃后上游仍空烧算力。
// 非 2xx 时 rc 为 nil、body 为上游响应体（供调用方 Classify）、err 为 nil；
// 只有传输层失败才返回 err。
func (c *Client) ChatStream(ctx context.Context, a *auth.Auth, body []byte) (rc io.ReadCloser, status int, respBody []byte, err error) {
	if ctx == nil {
		ctx = context.Background()
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.agentBase()+EpChat, bytes.NewReader(PrepareBodyFunction(body, c.function())))
	if err != nil {
		return nil, 0, nil, err
	}
	SOLOHeaders(req, a, true)
	// 用专用流客户端（无总超时），避免长 SSE 流被 HTTP.Timeout 截断。
	hc := c.HTTP
	if c.StreamHTTP != nil {
		hc = c.StreamHTTP
	}
	resp, err := hc.Do(req)
	if err != nil {
		log.Printf("chat_stream uid=%s: transport error: %v", a.UID, err)
		return nil, 0, nil, err
	}
	if resp.StatusCode >= 400 {
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		resp.Body.Close()
		kind := Classify(resp.StatusCode, string(raw))
		log.Printf("chat_stream uid=%s: upstream %d %s body=%s",
			a.UID, resp.StatusCode, kind, truncate(string(raw), 200))
		return nil, resp.StatusCode, raw, nil
	}
	return resp.Body, resp.StatusCode, nil, nil
}

// ModelInfo 动态模型信息。
type ModelInfo struct {
	ID              string
	Name            string
	ContextWindow   int64   // = context_window_tokens（总窗口）
	PromptMaxTokens int64   // = model_detail_list.prompt_max_tokens（最大输入）
	MaxTokens       int64   // = model_detail_list.max_tokens（最大输出）
	Rate            float64 // 上游消耗倍率（consumption_rate），0 = 未知
	// Invisible = 上游 is_invisible_to_user：只影响 /v1/models 展示，不影响路由。
	// 实测部分 invisible 配置（如 kimi-k2.6）直接请求仍能正常返回，
	// 因此不能把它们在 mapModel 门口一并拒掉。
	Invisible bool
	// HasMaxTier = get_detail_param 的 model_detail_list 是否含 __max 档。
	// 仅当为 true 时请求才可改写 config_name=<model>__max（10-26 实测：
	// 裸名或仅 extra_info 开关都会被 4022 prompt tokens exceeded 拒）。
	HasMaxTier bool
}

// FetchModels 拉 SOLO 模型表（get_detail_param）。
//
// 返回全量配置（含 is_invisible_to_user=true 的内部配置），由调用方决定展示口径：
// solo_agent 通道会额外返回 title_generation / summary / *_auto / 各类 sub_agent
// 等配置（实测 28/69 条），列进 /v1/models 只会污染客户端的模型选择器。
func (c *Client) FetchModels(a *auth.Auth) ([]ModelInfo, error) {
	return c.FetchModelsCtx(context.Background(), a)
}

// FetchModelsCtx 同 FetchModels，但受 ctx 约束。
// 跨账号拉模型目录时用它给每个账号单独设超时 —— 否则一个卡住的账号会把
// 整轮刷新拖到 HTTP 客户端的全局超时（实测上游偶发挂住），拖坏 /v1/models。
func (c *Client) FetchModelsCtx(ctx context.Context, a *auth.Auth) ([]ModelInfo, error) {
	body := map[string]any{
		"function":            c.function(),
		"config_names":        nil,
		"need_prompt":         false,
		"current_config_info": nil,
		"poly_prompt":         true,
		"mode_type":           nil,
		"agent_type":          nil,
	}
	raw, _ := json.Marshal(body)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.agentBase()+EpModels, bytes.NewReader(raw))
	if err != nil {
		return nil, err
	}
	SOLOHeaders(req, a, false)
	data, err := c.doJSON(req)
	if err != nil {
		return nil, err
	}
	var resp struct {
		ConfigInfoList []struct {
			ConfigName    string `json:"config_name"`
			IsInvisible   bool   `json:"is_invisible_to_user"`
			DisplayConfig struct {
				DisplayName string `json:"display_name"`
			} `json:"display_config"`
			// 上下文窗口按运行环境映射（实测 {"dev":256000} 或 {"dev":184000,"max":184000}）。
			// 少数模型（pro/flash/free）该字段为空，回退 model_detail_list.prompt_max_tokens。
			ContextWindowTokens map[string]float64 `json:"context_window_tokens"`
			// 内嵌 JSON 字符串：consumption_rate.data.rate 即客户端显示的模型倍率（如 0.48x）
			DisplayContactConfig string `json:"display_contact_config"`
			ModelDetailList      []struct {
				ModelName       string `json:"model_name"`
				PromptMaxTokens int64  `json:"prompt_max_tokens"`
				MaxTokens       int64  `json:"max_tokens"`
			} `json:"model_detail_list"`
		} `json:"config_info_list"`
	}
	if err := json.Unmarshal(data, &resp); err != nil {
		return nil, fmt.Errorf("models parse: %w", err)
	}
	out := make([]ModelInfo, 0, len(resp.ConfigInfoList))
	for _, cfg := range resp.ConfigInfoList {
		if cfg.ConfigName == "" {
			continue
		}
		// 多档模型（solo_agent 的 __dev/__max 双档）取容量最大的一档：
		// 报 dev 档会把可用能力低估一半。max 档的请求改写见
		// server.mapModel + setMaxModeInBody（裸名不够，10-26 实测结论）。
		promptMax, maxOut := maxTierLimits(cfg.ModelDetailList)
		hasMaxTier := false
		for _, d := range cfg.ModelDetailList {
			if strings.HasSuffix(d.ModelName, "__max") {
				hasMaxTier = true
				break
			}
		}
		// 窗口至少要等于所选档的输入上限：少数模型 context_window_tokens
		// 只写了 dev 值，直接取 max 会低估。
		window := contextWindowTokens(cfg.ContextWindowTokens, promptMax)
		if promptMax > window {
			window = promptMax + maxOut
		}
		out = append(out, ModelInfo{
			ID:              cfg.ConfigName,
			Name:            cfg.DisplayConfig.DisplayName,
			ContextWindow:   window,
			PromptMaxTokens: promptMax,
			MaxTokens:       maxOut,
			Rate:            consumptionRate(cfg.DisplayContactConfig),
			Invisible:       cfg.IsInvisible,
			HasMaxTier:      hasMaxTier,
		})
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("models api returned empty list")
	}
	return out, nil
}

// maxTierLimits 从 model_detail_list 里取输入上限最大的那一档，
// 返回该档的 (prompt_max_tokens, max_tokens)。空列表返回 (0,0)。
func maxTierLimits(details []struct {
	ModelName       string `json:"model_name"`
	PromptMaxTokens int64  `json:"prompt_max_tokens"`
	MaxTokens       int64  `json:"max_tokens"`
}) (promptMax, maxOut int64) {
	for _, d := range details {
		if d.PromptMaxTokens > promptMax {
			promptMax, maxOut = d.PromptMaxTokens, d.MaxTokens
		}
	}
	return promptMax, maxOut
}

// contextWindowTokens 取模型真实上下文窗口。
//
// 上游 context_window_tokens 是环境映射（dev/max 等），正常只有一个键；
// 多键时取最大值，避免不同环境口径不一致。映射为空（实测 pro/flash/free）
// 时回退 prompt_max_tokens —— 这类模型的输入上限即窗口上限。
func contextWindowTokens(m map[string]float64, promptMax int64) int64 {
	var out int64
	for _, v := range m {
		if n := int64(v); n > out {
			out = n
		}
	}
	if out == 0 {
		out = promptMax
	}
	return out
}

// consumptionRate 从 display_contact_config 内嵌 JSON 提取 consumption_rate.data.rate。
// 该字段是 JSON 字符串而非嵌套对象，需二次解析；缺失/禁用/解析失败返回 0。
func consumptionRate(raw string) float64 {
	if raw == "" {
		return 0
	}
	var dcc struct {
		ConsumptionRate struct {
			Enable bool `json:"enable"`
			Data   struct {
				Rate float64 `json:"rate"`
			} `json:"data"`
		} `json:"consumption_rate"`
	}
	if err := json.Unmarshal([]byte(raw), &dcc); err != nil {
		return 0
	}
	if !dcc.ConsumptionRate.Enable {
		return 0
	}
	return dcc.ConsumptionRate.Data.Rate
}

// CheckinStatus 查询签到状态。
// deviceID 为签到专用派生设备号（空串时回退 auth 文件里的 DeviceID）；
// 请求头走 CheckinHeaders 极简形态，与 UgHeaders（积分查询）区分。
func (c *Client) CheckinStatus(a *auth.Auth, deviceID string) (checkedIn bool, credits int64, enable bool, err error) {
	req, err := http.NewRequest(http.MethodPost, c.ugBase()+EpCheckinStatus, bytes.NewReader([]byte("{}")))
	if err != nil {
		return false, 0, false, err
	}
	CheckinHeaders(req, a, deviceID)
	data, err := c.doJSON(req)
	if err != nil {
		return false, 0, false, err
	}
	var resp struct {
		CheckedIn bool  `json:"checked_in"`
		Credits   int64 `json:"credits"`
		Enable    bool  `json:"enable"`
	}
	if err := json.Unmarshal(data, &resp); err != nil {
		return false, 0, false, fmt.Errorf("checkin status parse: %w", err)
	}
	return resp.CheckedIn, resp.Credits, resp.Enable, nil
}

// CheckinClaim 执行签到。deviceID 语义同 CheckinStatus。
// HTTP 200 但业务码非 0 时返回 *CheckinError，供调用方判定 9074 限流。
func (c *Client) CheckinClaim(a *auth.Auth, deviceID string) error {
	req, err := http.NewRequest(http.MethodPost, c.ugBase()+EpCheckinClaim, bytes.NewReader([]byte("{\"req_source\":2}")))
	if err != nil {
		return err
	}
	CheckinHeaders(req, a, deviceID)
	data, err := c.doJSON(req)
	if err != nil {
		return err
	}
	var resp struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
	}
	if err := json.Unmarshal(data, &resp); err == nil && resp.Code != 0 {
		return &CheckinError{Code: resp.Code, Msg: resp.Message}
	}
	return nil
}

// UserEntUsage 聚合积分（ide_user_ent_usage 的 credits_limit 求和）。
func (c *Client) UserEntUsage(a *auth.Auth) (remain int64, err error) {
	remain, _, _, _, err = c.EntUsage(a)
	return remain, err
}

// EntUsage 查询账号额度明细（积分总量/已用/剩余/权益包数）。
// remain = limit - used，usage.credits_amount 是已用积分（实测）。
func (c *Client) EntUsage(a *auth.Auth) (remain, limit, used int64, packs int, err error) {
	req, err := http.NewRequest(http.MethodPost, c.ugBase()+EpEntUsage, bytes.NewReader([]byte("{}")))
	if err != nil {
		return 0, 0, 0, 0, err
	}
	UgHeaders(req, a)
	data, err := c.doJSON(req)
	if err != nil {
		return 0, 0, 0, 0, err
	}
	var resp struct {
		UserEntitlementPackList []struct {
			EntitlementBaseInfo struct {
				Quota struct {
					CreditsLimit int64 `json:"credits_limit"`
				} `json:"quota"`
			} `json:"entitlement_base_info"`
			Usage struct {
				CreditsAmount float64 `json:"credits_amount"`
			} `json:"usage"`
		} `json:"user_entitlement_pack_list"`
	}
	if err := json.Unmarshal(data, &resp); err != nil {
		return 0, 0, 0, 0, fmt.Errorf("ent usage parse: %w", err)
	}
	for _, p := range resp.UserEntitlementPackList {
		l := p.EntitlementBaseInfo.Quota.CreditsLimit
		if l <= 0 {
			continue
		}
		u := int64(p.Usage.CreditsAmount)
		limit += l
		used += u
		remain += l - u
		packs++
	}
	return remain, limit, used, packs, nil
}

// PackDetail 单个权益包明细（含积分有效期）。
type PackDetail struct {
	Desc      string `json:"desc"`
	Limit     int64  `json:"limit"`
	Used      int64  `json:"used"`
	StartTime int64  `json:"start_time"`
	ExpireAt  int64  `json:"expire_at"`
}

// PackList 查询账号权益包明细（含每包积分过期时间，按过期时间升序）。
// 数据源与 EntUsage 相同（ide_user_ent_usage），额外解析 expire_time/start_time；
// 签到/登录赠送的积分是独立权益包，各自 30 天（或自然月）有效期，先进先出消耗。
func (c *Client) PackList(a *auth.Auth) ([]PackDetail, error) {
	req, err := http.NewRequest(http.MethodPost, c.ugBase()+EpEntUsage, bytes.NewReader([]byte("{}")))
	if err != nil {
		return nil, err
	}
	UgHeaders(req, a)
	data, err := c.doJSON(req)
	if err != nil {
		return nil, err
	}
	var resp struct {
		UserEntitlementPackList []struct {
			DisplayDesc         string `json:"display_desc"`
			ExpireTime          int64  `json:"expire_time"`
			EntitlementBaseInfo struct {
				StartTime int64 `json:"start_time"`
				Quota     struct {
					CreditsLimit int64 `json:"credits_limit"`
				} `json:"quota"`
			} `json:"entitlement_base_info"`
			Usage struct {
				CreditsAmount float64 `json:"credits_amount"`
			} `json:"usage"`
		} `json:"user_entitlement_pack_list"`
	}
	if err := json.Unmarshal(data, &resp); err != nil {
		return nil, fmt.Errorf("ent usage parse: %w", err)
	}
	var out []PackDetail
	for _, p := range resp.UserEntitlementPackList {
		l := p.EntitlementBaseInfo.Quota.CreditsLimit
		if l <= 0 {
			continue
		}
		out = append(out, PackDetail{
			Desc:      p.DisplayDesc,
			Limit:     l,
			Used:      int64(p.Usage.CreditsAmount),
			StartTime: p.EntitlementBaseInfo.StartTime,
			ExpireAt:  p.ExpireTime,
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ExpireAt < out[j].ExpireAt })
	return out, nil
}

// GetUserInfo 查询账号信息（登录用）。
func (c *Client) GetUserInfo(a *auth.Auth) (uid, nickname, enterpriseID string, err error) {
	host := a.ApiHost
	if host == "" {
		host = c.oauthBase()
	}
	body := map[string]any{"ReqSource": "IDE", "IDEVersion": IdeVersion}
	raw, _ := json.Marshal(body)
	req, err := http.NewRequest(http.MethodPost, host+EpUserInfo, bytes.NewReader(raw))
	if err != nil {
		return "", "", "", err
	}
	OAuthHeaders(req)
	req.Header.Set("X-Cloudide-Token", a.JWT()) // 读锁快照
	data, err := c.doJSON(req)
	if err != nil {
		return "", "", "", err
	}
	var resp struct {
		Result struct {
			UserID       string `json:"UserID"`
			ScreenName   string `json:"ScreenName"`
			EnterpriseID string `json:"EnterpriseID"`
		} `json:"Result"`
	}
	if err := json.Unmarshal(data, &resp); err != nil {
		return "", "", "", fmt.Errorf("userinfo parse: %w", err)
	}
	return resp.Result.UserID, resp.Result.ScreenName, resp.Result.EnterpriseID, nil
}

func truncate(s string, n int) string {
	s = strings.TrimSpace(s)
	if len(s) > n {
		return s[:n]
	}
	return s
}
