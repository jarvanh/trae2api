// Package server 暴露 OpenAI 兼容 HTTP 接口，内部驱动 pool 挑号 + upstream 转发。
package server

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	"trae2api/internal/auth"
	"trae2api/internal/pool"
	"trae2api/internal/upstream"
)

// Config handler 依赖。
type Config struct {
	Pool            *pool.Pool
	Upstream        *upstream.Client
	WorkClient      *upstream.WorkClient
	WorkMode        upstream.WorkMode
	APIKey          string        // 空 = 不鉴权
	AuthDir         string        // auths/ 目录，用于 import/delete 落盘 trae-*.json
	UsageDir        string        // 用量明细目录（/app/data），空 = 不落盘
	MaxRotate       int           // 单请求最多换号次数，默认 3
	PlanCooldown    time.Duration // 1005 冷却，默认 12h
	SoftCooldown    time.Duration // 429 冷却，默认 60s
	ErrThreshold    int           // 连续错误阈值，默认 3
	ErrCooldown     time.Duration // 错误冷却，默认 10m
	RefreshSkew     time.Duration // token 预刷新窗口，默认 72h（schedule.refresh_skew 可配）
	DefaultModel    string        // 默认 glm-5.2
	WorkBridgeURL   string        // 本地 Work 积分桥接端点，如 http://127.0.0.1:7865
	WorkBridgeToken string        // 访问 WorkBridge 的 Bearer token（空 = 不带鉴权）
	// Keepalive 流式响应在「首个真实帧到达前」的心跳间隔（SSE 注释帧 `: ping`）。
	// <=0 = 不发心跳（cmd/server 载入配置时未设/0 已归一化为默认 15s，负数才是显式关闭）。用于防止边缘网关因长排队窗口连接静默而提前掐断（502）。
	Keepalive time.Duration
}

// maxBodyBytes 请求体大小上限（8MB），超过返回 413。
const maxBodyBytes = 8 << 20

// Handler 主路由。
type Handler struct {
	cfg Config
	mux *http.ServeMux

	// Web 登录 pending 态：pendingID → 登录进行中的临时上下文。
	// 回调 /authorize 捕获后标记成功；面板轮询 result 取结果。
	loginMu sync.Mutex
	logins  map[string]*pendingLogin
}

// NewHandler 构建 handler。
func NewHandler(cfg Config) *Handler {
	if cfg.MaxRotate <= 0 {
		cfg.MaxRotate = 3
	}
	if cfg.PlanCooldown <= 0 {
		cfg.PlanCooldown = 12 * time.Hour
	}
	if cfg.SoftCooldown <= 0 {
		cfg.SoftCooldown = 60 * time.Second
	}
	if cfg.ErrThreshold <= 0 {
		cfg.ErrThreshold = 3
	}
	if cfg.ErrCooldown <= 0 {
		cfg.ErrCooldown = 10 * time.Minute
	}
	if cfg.RefreshSkew <= 0 {
		cfg.RefreshSkew = 72 * time.Hour
	}
	if cfg.DefaultModel == "" {
		cfg.DefaultModel = upstream.DefaultConfigName
	}
	if cfg.WorkClient == nil {
		workCfg := upstream.DefaultWorkClientConfig()
		if cfg.WorkBridgeURL != "" {
			workCfg.BridgeURL = cfg.WorkBridgeURL
		}
		if cfg.WorkBridgeToken != "" {
			workCfg.BridgeToken = cfg.WorkBridgeToken
		}
		if cfg.WorkMode != "" {
			workCfg.Mode = cfg.WorkMode
		}
		cfg.WorkClient = upstream.NewWorkClient(workCfg)
	}
	if cfg.WorkMode == "" {
		cfg.WorkMode = cfg.WorkClient.Mode()
	}
	h := &Handler{cfg: cfg, mux: http.NewServeMux(), logins: map[string]*pendingLogin{}}
	h.mux.HandleFunc("POST /v1/chat/completions", h.withAuth(h.chatCompletions))
	h.mux.HandleFunc("GET /v1/models", h.withAuth(h.models))
	h.mux.HandleFunc("GET /status", h.withAuth(h.status))
	h.mux.HandleFunc("GET /healthz", h.healthz)
	// 管理面板：本地面板
	// 读接口无鉴权（局域网内只读）；写接口（accounts 写/login/refresh/authorize）
	// 经 withAdminAuth 校验 Bearer = TW2A_API_KEY（见 §4 安全设计）。
	h.mux.HandleFunc("GET /admin", h.adminPage)
	h.mux.HandleFunc("GET /admin/api/credits", h.adminCredits)
	h.mux.HandleFunc("GET /admin/api/models", h.adminModels)
	// 请求流水：读 usage-<日期>.jsonl 明细 + 聚合（只读，无鉴权）
	h.mux.HandleFunc("GET /admin/api/usage", h.adminUsage)
	// 控制台拖动排序：GET 读已保存顺序，POST 保存（写操作需 Bearer = TW2A_API_KEY）
	h.mux.HandleFunc("GET /admin/api/order", h.adminGetOrder)
	h.mux.HandleFunc("POST /admin/api/order", h.withAdminAuth(h.adminSaveOrder))
	// 账号 CRUD
	h.mux.HandleFunc("GET /admin/api/accounts", h.adminAccounts)
	h.mux.HandleFunc("POST /admin/api/accounts/import", h.withAdminAuth(h.adminImportAccount))
	h.mux.HandleFunc("DELETE /admin/api/accounts/{uid}", h.withAdminAuth(h.adminDeleteAccount))
	h.mux.HandleFunc("PATCH /admin/api/accounts/{uid}", h.withAdminAuth(h.adminPatchAccount))
	h.mux.HandleFunc("POST /admin/api/accounts/{uid}/refresh", h.withAdminAuth(h.adminRefreshAccount))
	h.mux.HandleFunc("GET /admin/api/accounts/{uid}/json", h.adminAccountJSON)
	// Web 登录闭环
	h.mux.HandleFunc("POST /admin/api/login", h.withAdminAuth(h.adminLoginStart))
	h.mux.HandleFunc("GET /admin/api/login/result", h.adminLoginResult)
	h.mux.HandleFunc("POST /admin/api/login/cancel", h.withAdminAuth(h.adminLoginCancel))
	// TRAE 回调落点（/authorize）：无需 Bearer（TRAE 浏览器 302 不带 key），
	// 仅捕获 query 写 pending 队列，不直接落盘 token。
	h.mux.HandleFunc("GET /authorize", h.authorizeCallback)
	return h
}

// withAdminAuth 校验写操作的 Bearer API Key（常量时间比较，复用 withAuth 逻辑）。
// APIKey 为空时（未配置 TW2A_API_KEY）退化为不鉴权——本地无 key 场景仍可用，
// 但生产强烈建议配 key（见 PLAN §4）。
func (h *Handler) withAdminAuth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if h.cfg.APIKey == "" {
			next(w, r)
			return
		}
		authz := r.Header.Get("Authorization")
		const prefix = "Bearer "
		if len(authz) < len(prefix) || !strings.EqualFold(authz[:len(prefix)], prefix) {
			writeOpenAIError(w, http.StatusUnauthorized, "invalid_api_key", "missing or invalid API key")
			return
		}
		key := authz[len(prefix):]
		if subtle.ConstantTimeCompare([]byte(key), []byte(h.cfg.APIKey)) != 1 {
			writeOpenAIError(w, http.StatusUnauthorized, "invalid_api_key", "missing or invalid API key")
			return
		}
		next(w, r)
	}
}

func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	h.mux.ServeHTTP(w, r)
}

func (h *Handler) withAuth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if h.cfg.APIKey != "" {
			authz := r.Header.Get("Authorization")
			const prefix = "Bearer "
			if len(authz) < len(prefix) || !strings.EqualFold(authz[:len(prefix)], prefix) {
				writeOpenAIError(w, http.StatusUnauthorized, "invalid_api_key", "missing or invalid API key")
				return
			}
			key := authz[len(prefix):]
			// 常量时间比较，防时序攻击（本地代理但按规范）。
			if subtle.ConstantTimeCompare([]byte(key), []byte(h.cfg.APIKey)) != 1 {
				writeOpenAIError(w, http.StatusUnauthorized, "invalid_api_key", "missing or invalid API key")
				return
			}
		}
		next(w, r)
	}
}

func (h *Handler) healthz(w http.ResponseWriter, r *http.Request) {
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte("ok"))
}

func (h *Handler) status(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"accounts": h.cfg.Pool.List(),
	})
}

// ---------------------------------------------------------------------------
// 模型映射
// ---------------------------------------------------------------------------

// isWorkModel 判断请求模型是否属于 Work 通道专有模型
func isWorkModel(model string) bool {
	m := strings.ToLower(strings.TrimSpace(model))
	return strings.Contains(m, "work") || strings.Contains(model, "Flash-Official")
}

// mapModel 将客户端传入的 model 映射为 config_name（SPEC §4.5）：
//
//	"glm-5.2"（config_name）        → 直接转发
//	"glm-5.2__dev"（内部名）        → 去掉后缀映射回 config_name
//	"auto" / ""                     → 默认模型
//	其他未知                        → 400
func (h *Handler) mapModel(model string) (string, error) {
	model = strings.TrimSpace(model)
	if model == "" || model == "auto" {
		return h.cfg.DefaultModel, nil
	}
	if strings.ToLower(model) == "work" {
		return upstream.DefaultWorkModel, nil
	}
	// 去掉内部名后缀（__dev / __max 等）
	base := model
	if i := strings.Index(model, "__"); i >= 0 {
		base = model[:i]
	}
	if h.knownModel(base) {
		return base, nil
	}
	// 宽松匹配：下划线 → 横线，大小写不敏感（deepseek_v4_pro → DeepSeek-V4-Pro）
	norm := normalizeModelName(base)
	if h.knownModel(norm) {
		return norm, nil
	}
	if isWorkModel(model) {
		return model, nil
	}
	return "", fmt.Errorf("unknown model %q", model)
}

// normalizeModelName 将下划线命名的内部名归一化为 config_name 风格（横线分隔）。
func normalizeModelName(s string) string {
	parts := strings.Split(s, "_")
	for i, p := range parts {
		if p == "" {
			continue
		}
		parts[i] = strings.ToUpper(p[:1]) + strings.ToLower(p[1:])
	}
	return strings.Join(parts, "-")
}

// knownModel 判断 model 是否是上游已知的 config_name。
//
// 口径故意比 /v1/models 宽：动态表里的 invisible 配置（如 kimi-k2.6）实测
// 仍可正常请求，只是不该出现在模型列表里污染客户端选择器；若在此一并拒掉，
// 切换 function 通道后这些 id 会凭空 400。动态表为空时回退静态表。
func (h *Handler) knownModel(model string) bool {
	if infos := h.fetchDynamicModels(); len(infos) > 0 {
		for _, mi := range infos {
			if mi.ID == model {
				return true
			}
		}
		return false
	}
	for _, m := range staticModels {
		if m["id"] == model {
			return true
		}
	}
	return false
}

// 静态 SOLO 模型表（动态拉取失败时回退，41 个 config_name 对齐 solo_agent 通道实测可见集）。
// 不含 context_length：上游不可用时宁可不声明，也不伪造统一窗口值。
var staticModels = []map[string]any{
	{"id": "DeepSeek-V4-Flash-Official", "object": "model", "created": 1753600000, "owned_by": "trae-work"},
	{"id": "Doubao-Seed-2.1-Pro", "object": "model", "created": 1753600000, "owned_by": "trae-solo"},
	{"id": "Doubao-Seed-2.1-Turbo", "object": "model", "created": 1753600000, "owned_by": "trae-solo"},
	{"id": "glm-5.2", "object": "model", "created": 1753600000, "owned_by": "trae-solo"},
	{"id": "kimi-k3", "object": "model", "created": 1753600000, "owned_by": "trae-solo"},
	{"id": "minimax-m3", "object": "model", "created": 1753600000, "owned_by": "trae-solo"},
	{"id": "qwen-3.7-plus", "object": "model", "created": 1753600000, "owned_by": "trae-solo"},
	{"id": "custom_model_gemini", "object": "model", "created": 1753600000, "owned_by": "trae-solo"},
	{"id": "custom_model_placeholder", "object": "model", "created": 1753600000, "owned_by": "trae-solo"},
	{"id": "custom_model_1M_text", "object": "model", "created": 1753600000, "owned_by": "trae-solo"},
	{"id": "custom_model_1M", "object": "model", "created": 1753600000, "owned_by": "trae-solo"},
	{"id": "custom_model_kimi", "object": "model", "created": 1753600000, "owned_by": "trae-solo"},
	{"id": "custom_model_gpt-5", "object": "model", "created": 1753600000, "owned_by": "trae-solo"},
	{"id": "custom_model_no-fc", "object": "model", "created": 1753600000, "owned_by": "trae-solo"},
	{"id": "custom_model_deepseek_chat", "object": "model", "created": 1753600000, "owned_by": "trae-solo"},
	{"id": "custom_model_deepseek_reasoner", "object": "model", "created": 1753600000, "owned_by": "trae-solo"},
	{"id": "custom_model_deepseek_v4", "object": "model", "created": 1753600000, "owned_by": "trae-solo"},
	{"id": "summary", "object": "model", "created": 1753600000, "owned_by": "trae-solo"},
	{"id": "DeepSeek-V4-Pro-Official", "object": "model", "created": 1753600000, "owned_by": "trae-work"},
	{"id": "Doubao-Seed-Code", "object": "model", "created": 1753600000, "owned_by": "trae-solo"},
	{"id": "Doubao-Seed-Evolving", "object": "model", "created": 1753600000, "owned_by": "trae-solo"},
	{"id": "claude-opus-5", "object": "model", "created": 1753600000, "owned_by": "trae-solo"},
	{"id": "custom_model_200k", "object": "model", "created": 1753600000, "owned_by": "trae-solo"},
	{"id": "custom_model_200k_text", "object": "model", "created": 1753600000, "owned_by": "trae-solo"},
	{"id": "custom_model_doubao_1M", "object": "model", "created": 1753600000, "owned_by": "trae-solo"},
	{"id": "custom_model_doubao_256k", "object": "model", "created": 1753600000, "owned_by": "trae-solo"},
	{"id": "custom_model_gpt-6", "object": "model", "created": 1753600000, "owned_by": "trae-solo"},
	{"id": "deepseek-v4.1-flash", "object": "model", "created": 1753600000, "owned_by": "trae-solo"},
	{"id": "glm-5.3", "object": "model", "created": 1753600000, "owned_by": "trae-solo"},
	{"id": "glm-5.3-flash", "object": "model", "created": 1753600000, "owned_by": "trae-solo"},
	{"id": "glm-5.3-flashx", "object": "model", "created": 1753600000, "owned_by": "trae-solo"},
	{"id": "input_optimization", "object": "model", "created": 1753600000, "owned_by": "trae-solo"},
	{"id": "kimi-k2.8-preview", "object": "model", "created": 1753600000, "owned_by": "trae-solo"},
	{"id": "kimi-k3.1-pro", "object": "model", "created": 1753600000, "owned_by": "trae-solo"},
	{"id": "mimo-v2.6-flash", "object": "model", "created": 1753600000, "owned_by": "trae-solo"},
	{"id": "mimo-v2.6-pro", "object": "model", "created": 1753600000, "owned_by": "trae-solo"},
	{"id": "minimax-m3.1-flash", "object": "model", "created": 1753600000, "owned_by": "trae-solo"},
	{"id": "qwen3.8-flash", "object": "model", "created": 1753600000, "owned_by": "trae-solo"},
	{"id": "qwen3.8-max", "object": "model", "created": 1753600000, "owned_by": "trae-solo"},
	{"id": "step-5-preview", "object": "model", "created": 1753600000, "owned_by": "trae-solo"},
	{"id": "title_generation", "object": "model", "created": 1753600000, "owned_by": "trae-solo"},
}

// dynamicModelsCache 动态模型缓存（成功 1h / 失败负缓存 5min）。
//
// refreshing 是单飞标志：跨账号并集刷新要向池内每个账号各发一次
// get_detail_param（实测单号响应 1.44MB），比旧的单号版贵 N 倍，
// 绝不能被并发请求放大。
var dynamicModelsCache struct {
	sync.RWMutex
	ids        []upstream.ModelInfo
	fetched    time.Time
	lastFail   time.Time
	refreshing bool
}

const (
	dynamicModelsTTL = time.Hour
	// modelsFetchFailCooldown 全部账号都拉失败后的负缓存时长：这段时间内不再
	// 扇出（否则每个 /v1/models 请求都打 N 次上游），也不再反复空刷。
	// 取 30s（原 5min）：并集失败通常是短时抖动，5min 会让新上线的模型迟迟不出现。
	modelsFetchFailCooldown = 30 * time.Second
	// modelsFetchPerAccountTimeout 单账号拉模型表的硬超时。不设的话一个挂死的
	// 账号会把整轮并集刷新拖到 HTTP 客户端全局超时（实测上游偶发挂住）。
	modelsFetchPerAccountTimeout = 25 * time.Second
)

func (h *Handler) models(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"object": "list",
		"data":   h.modelList(),
	})
}

// displayModel 判断某 config 是否该出现在对外的模型列表里。
//
// 两个条件缺一不可：
//   - is_invisible_to_user：上游标的内部配置（*_auto / sub_agent 等）
//   - isInternalModel：命名启发式补集 —— 上游把 custom_model_* / summary /
//
// input_optimization 标成「可见」，但它们实测是占位或
//
//	内部功能位（custom_model_1M 实测 503 code=3003）。管理台一直在隐藏它们，
//	/v1/models 若跟着上游标走就会两处口径不一致，客户端选择器也会塞满不可用项。
//
// 只影响展示，不影响路由：knownModel 仍接受全部 config（见其注释）。
func displayModel(mi upstream.ModelInfo) bool {
	return displayableModel(mi.Invisible, mi.ID)
}

// displayableModel 是 /v1/models 与管理台共用的展示口径（单一事实源）。
// 两处各自实现过一次就漂移过一回，故收敛到这一个函数。
func displayableModel(invisible bool, id string) bool {
	return !invisible && !isInternalModel(id)
}

// modelList 动态获取模型列表并包装成 OpenAI 格式；失败回退静态表。
func (h *Handler) modelList() []map[string]any {
	if infos := h.fetchDynamicModels(); len(infos) > 0 {
		out := make([]map[string]any, 0, len(infos))
		for _, mi := range infos {
			if !displayModel(mi) {
				continue
			}
			entry := map[string]any{
				"id":       mi.ID,
				"object":   "model",
				"created":  1753600000,
				"owned_by": "trae-solo",
			}
			if cl, ok := modelContextLength(mi); ok {
				entry["context_length"] = cl
			}
			if mi.PromptMaxTokens > 0 {
				entry["max_input_tokens"] = mi.PromptMaxTokens
			}
			if mi.MaxTokens > 0 {
				entry["max_output_tokens"] = mi.MaxTokens
			}
			if mi.Rate > 0 {
				entry["consumption_rate"] = mi.Rate
			}
			out = append(out, entry)
		}
		return out
	}
	// 静态回退表同样过一遍展示口径：表里也混着 custom_model_* / summary 等占位项。
	out := make([]map[string]any, 0, len(staticModels))
	for _, m := range staticModels {
		if id, _ := m["id"].(string); isInternalModel(id) {
			continue
		}
		out = append(out, m)
	}
	return out
}

// fetchDynamicModels 取跨账号并集的模型表，缓存 1h。
//
// 为什么不能只问一个号（旧行为）：solo_agent 的模型集按账号漂移 —— 实测 4 个
// 账号分别返回 68/68/69/71 条，free/flash/pro/claude-opus-5 只在其中一个号上。
// 只问一个号并缓存 1h，会让 /v1/models 列出「本轮那个号有、下轮换号就没」的
// 模型，客户端间歇性 400/503。
//
// 并发语义（stale-while-revalidate）：
//   - 缓存新鲜 → 直接返回；
//   - 过期但非空 → 立即返回旧数据，由后台 goroutine 单飞刷新（单飞标志保证
//     同一时刻至多一轮跨号扇出），绝不阻塞请求。并集刷新要向每个号各发一次
//     get_detail_param（实测单号 1.44MB），同步做会把 TTL 边界上的那一个请求
//     拖出可感知的延迟；
//   - 无数据（冷启动/上轮全败）→ 同步刷新一次，宁可这一个请求慢，也不能
//     拿静态回退表糊弄唯一需要真实数据的场景。
func (h *Handler) fetchDynamicModels() []upstream.ModelInfo {
	dynamicModelsCache.RLock()
	fresh := len(dynamicModelsCache.ids) > 0 && time.Since(dynamicModelsCache.fetched) < dynamicModelsTTL
	negCached := !dynamicModelsCache.lastFail.IsZero() && time.Since(dynamicModelsCache.lastFail) < modelsFetchFailCooldown
	cached := dynamicModelsCache.ids
	dynamicModelsCache.RUnlock()

	if fresh {
		return cached
	}
	if len(cached) > 0 {
		// 有旧数据：后台异步刷，本请求立即返回旧表。
		go h.refreshModelCacheAsync()
		return cached
	}
	if negCached {
		return nil
	}

	// 冷启动：同步刷，保证首次 /v1/models 与首次选号就能拿到真表。
	infos, ok := h.refreshModelCacheOnce()
	if !ok {
		return nil
	}
	return infos
}

// refreshModelCacheAsync 后台单飞刷新；已有刷新在跑时直接退出。
func (h *Handler) refreshModelCacheAsync() {
	if _, ok := h.refreshModelCacheOnce(); !ok {
		// 失败已记负缓存；冷启动兜底由下一次真实请求触发。
		return
	}
}

// refreshModelCacheOnce 单飞地执行一轮并集刷新。
// 返回 (新表, 是否成功)。调用方不要并发扇出本函数 —— 单飞标志会合并并发请求。
func (h *Handler) refreshModelCacheOnce() ([]upstream.ModelInfo, bool) {
	dynamicModelsCache.Lock()
	// 锁内复查：关掉「读判需要刷 → 抢到锁」窗口里的竞态，保证至多一轮在刷。
	if dynamicModelsCache.refreshing ||
		(len(dynamicModelsCache.ids) > 0 && time.Since(dynamicModelsCache.fetched) < dynamicModelsTTL) ||
		(!dynamicModelsCache.lastFail.IsZero() && time.Since(dynamicModelsCache.lastFail) < modelsFetchFailCooldown) {
		out := dynamicModelsCache.ids
		dynamicModelsCache.Unlock()
		return out, len(out) > 0
	}
	dynamicModelsCache.refreshing = true
	dynamicModelsCache.Unlock()

	infos := h.refreshModelUnion()

	dynamicModelsCache.Lock()
	dynamicModelsCache.refreshing = false
	if len(infos) > 0 {
		dynamicModelsCache.ids = infos
		dynamicModelsCache.fetched = time.Now()
		dynamicModelsCache.lastFail = time.Time{}
	} else {
		dynamicModelsCache.lastFail = time.Now()
	}
	out := dynamicModelsCache.ids
	dynamicModelsCache.Unlock()
	return out, len(infos) > 0
}

// refreshModelUnion 并发向全部健康账号拉模型表，取并集，并把每号能力集写回 pool
// （选号过滤用，见 PickAffinityForModel）。返回 nil = 一个号都没成功。
func (h *Handler) refreshModelUnion() []upstream.ModelInfo {
	if h.cfg.Pool == nil || h.cfg.Upstream == nil {
		return nil
	}
	accts := h.cfg.Pool.Healthy()
	if len(accts) == 0 {
		return nil
	}
	results := make([]*[]upstream.ModelInfo, len(accts))
	var wg sync.WaitGroup
	for i, a := range accts {
		wg.Add(1)
		go func(i int, a *auth.Auth) {
			defer wg.Done()
			ctx, cancel := context.WithTimeout(context.Background(), modelsFetchPerAccountTimeout)
			defer cancel()
			infos, err := h.cfg.Upstream.FetchModelsCtx(ctx, a)
			if err != nil || len(infos) == 0 {
				// 显式清能力集：宁可回到「不限制」，也别拿上一轮老数据继续卡选号。
				h.cfg.Pool.SetModels(a.UID, nil)
				return
			}
			ids := make([]string, 0, len(infos))
			for _, mi := range infos {
				ids = append(ids, mi.ID)
			}
			h.cfg.Pool.SetModels(a.UID, ids)
			results[i] = &infos
		}(i, a)
	}
	wg.Wait()

	// 并集：同 ID 各字段取跨号最大值（能力档随套餐浮动，取见过的最强档）；
	// Invisible 收紧为「所有号都隐藏」——任一号可见即对外可见。
	byID := make(map[string]upstream.ModelInfo, 256)
	order := make([]string, 0, 256)
	for _, p := range results {
		if p == nil {
			continue
		}
		for _, mi := range *p {
			if mi.ID == "" {
				continue
			}
			cur, seen := byID[mi.ID]
			if !seen {
				byID[mi.ID] = mi
				order = append(order, mi.ID)
				continue
			}
			if mi.ContextWindow > cur.ContextWindow {
				cur.ContextWindow = mi.ContextWindow
			}
			if mi.PromptMaxTokens > cur.PromptMaxTokens {
				cur.PromptMaxTokens = mi.PromptMaxTokens
			}
			if mi.MaxTokens > cur.MaxTokens {
				cur.MaxTokens = mi.MaxTokens
			}
			if cur.Name == "" {
				cur.Name = mi.Name
			}
			if cur.Rate == 0 {
				cur.Rate = mi.Rate
			}
			cur.Invisible = cur.Invisible && mi.Invisible
			byID[mi.ID] = cur
		}
	}
	if len(order) == 0 {
		return nil
	}
	out := make([]upstream.ModelInfo, 0, len(order))
	for _, id := range order {
		out = append(out, byID[id])
	}
	return out
}

// ---------------------------------------------------------------------------
// chat
// ---------------------------------------------------------------------------

// setModelInBody 将 body 中 model 字段替换为 configName，并返回改写后的 body。
func setModelInBody(body []byte, configName string) []byte {
	var obj map[string]any
	if err := json.Unmarshal(body, &obj); err != nil {
		return body
	}
	obj["model"] = configName
	out, err := json.Marshal(obj)
	if err != nil {
		return body
	}
	return out
}

// extractSessionKey 从 HTTP 请求头或 OpenAI 请求体中提取会话标识，用于多轮对话粘性路由。
// 优先级：
// 1. HTTP 请求头 (X-Session-ID / X-Conversation-ID / Session-Id)
// 2. 请求体中的 user 字段
// 3. 多轮对话前缀特征指纹 (提取 messages[0] 与 messages[1] 角色与内容计算 SHA-256)
func extractSessionKey(r *http.Request, body []byte) string {
	if sid := r.Header.Get("X-Session-ID"); sid != "" {
		return sid
	}
	if cid := r.Header.Get("X-Conversation-ID"); cid != "" {
		return cid
	}
	if sid := r.Header.Get("Session-Id"); sid != "" {
		return sid
	}

	var req struct {
		User     string `json:"user"`
		Messages []struct {
			Role    string          `json:"role"`
			Content json.RawMessage `json:"content"`
		} `json:"messages"`
	}
	if err := json.Unmarshal(body, &req); err == nil {
		if req.User != "" {
			return req.User
		}
		if len(req.Messages) > 0 {
			// 在同一多轮对话流中：系统提示词(若有)和首轮用户提问在所有轮次中均保持恒定。
			h := sha256.New()
			if req.Messages[0].Role == "system" {
				h.Write([]byte("sys:"))
				h.Write(req.Messages[0].Content)
			}
			// 找到首条非系统消息（即首轮用户提问）
			for _, m := range req.Messages {
				if m.Role != "system" {
					h.Write([]byte(m.Role + ":"))
					h.Write(m.Content)
					break
				}
			}
			return "fp:" + hex.EncodeToString(h.Sum(nil)[:8])
		}
	}
	return ""
}

// executeWorkRequest 使用账号池多账号动态轮转调度执行 Work 通道请求。
// st 为请求级统计对象（可为 nil）：命中账号时写回 st.uid，供用量流水记录消费账号。
// 调度与容灾策略：
// 1. 检查 Work 通道是否被 TW2A_WORK_MODE=disabled 禁用；
// 2. 根据可用 work_credits 降序择优选取健康账号（支持 sessionKey 会话粘性，排除已尝试过的账号）；
// 3. 检查并按需预刷新 token；
// 4. 发起 ChatStream 调用（在 Auto 模式下优先 Native 直连，失败平滑降级 Bridge）；
// 5. 若发生故障（网络错误/429限流/401会话失效/额度不足），进入 pool 对应冷却/禁用状态机，并自动轮转下一账号；
// 6. 成功响应后，异步触发积分探测，更新账号 work_credits 状态。
func (h *Handler) executeWorkRequest(w http.ResponseWriter, r *http.Request, body []byte, model string, stream bool, sessionKey string, st *chatStat) (bool, error) {
	if h.cfg.WorkMode == upstream.WorkModeDisabled || (h.cfg.WorkClient != nil && h.cfg.WorkClient.Mode() == upstream.WorkModeDisabled) {
		return false, errors.New("work channel is disabled by configuration (TW2A_WORK_MODE=disabled)")
	}

	workBody := body
	if !isWorkModel(model) {
		workBody = setModelInBody(body, upstream.DefaultWorkModel)
	}

	tried := map[string]bool{}
	var lastErr error

	for i := 0; i < h.cfg.MaxRotate; i++ {
		acct := h.cfg.Pool.PickWorkAffinity(sessionKey, tried)
		if acct == nil {
			break
		}
		tried[acct.UID] = true
		if st != nil {
			st.uid = acct.UID
		}

		refreshed, err := h.cfg.Upstream.RefreshTokenIfNeeded(acct, h.cfg.RefreshSkew)
		if err != nil {
			lastErr = err
			var ue *upstream.Error
			if errors.As(err, &ue) && ue.Kind == upstream.ErrSessionDead {
				h.cfg.Pool.Disable(acct.UID, "refresh session dead")
			} else {
				h.cfg.Pool.CooldownWork(acct.UID, h.cfg.ErrCooldown, "refresh: "+err.Error())
			}
			continue
		}
		if refreshed {
			_ = acct.SaveAtomic()
		}

		rc, status, respBody, terr := h.cfg.WorkClient.ChatStream(r.Context(), acct, workBody)
		if terr != nil {
			lastErr = terr
			h.cfg.Pool.NoteWorkError(acct.UID, h.cfg.ErrThreshold, h.cfg.ErrCooldown)
			continue
		}
		if status >= 400 {
			lastErr = fmt.Errorf("work upstream status %d: %s", status, string(respBody))
			if status == 429 {
				h.cfg.Pool.CooldownWork(acct.UID, h.cfg.SoftCooldown, "work 429 rate limit")
			} else if status == 401 || status == 403 {
				h.cfg.Pool.Disable(acct.UID, "work session dead")
			} else if status == 400 && (strings.Contains(string(respBody), "credit") || strings.Contains(string(respBody), "1005")) {
				h.cfg.Pool.CooldownWork(acct.UID, h.cfg.PlanCooldown, "work_credits 余额不足")
			} else {
				h.cfg.Pool.NoteWorkError(acct.UID, h.cfg.ErrThreshold, h.cfg.ErrCooldown)
			}
			continue
		}

		h.cfg.Pool.NoteWorkSuccess(acct.UID)

		// 异步刷新账号最新 work_credits 积分
		go func(a *auth.Auth) {
			probeCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			if snap, perr := h.cfg.WorkClient.ProbeCredits(probeCtx, a); perr == nil {
				h.cfg.Pool.SetWorkCredits(a.UID, snap.WorkCredits)
				if snap.WorkCredits <= 0 {
					h.cfg.Pool.CooldownWork(a.UID, h.cfg.PlanCooldown, "work_credits 余额不足")
				}
			}
		}(acct)

		if stream {
			_ = upstream.StreamWorkToOpenAI(w, rc, model, "")
			_ = rc.Close()
			// Work 流式拿不到 token 明细，记 0/0 仅保留探测/真实分流计数
			noteUsage(st, usageRecord{
				Dir: h.cfg.UsageDir, Model: model, Probe: isProbeRequest(r), UID: acct.UID,
				Status: http.StatusOK, Mode: "work-stream",
				Ms: msSince(st),
			})
			return true, nil
		}

		resp, aerr := upstream.AggregateWork(rc, model, "")
		_ = rc.Close()
		if aerr != nil {
			lastErr = aerr
			h.cfg.Pool.NoteWorkError(acct.UID, h.cfg.ErrThreshold, h.cfg.ErrCooldown)
			continue
		}
		noteUsage(st, usageRecord{
			Dir: h.cfg.UsageDir, Model: model, Probe: isProbeRequest(r), UID: acct.UID,
			Status: http.StatusOK, Mode: "work-sync", In: promptTokens(resp), Out: completionTokens(resp),
			Ms: msSince(st),
		})
		writeJSON(w, http.StatusOK, resp)
		return true, nil
	}

	if lastErr == nil {
		lastErr = errors.New("no healthy account available for work channel")
	}
	return false, lastErr
}

func (h *Handler) chatCompletions(w http.ResponseWriter, r *http.Request) {
	// X-Probe 探测打标：1/true/yes 大小写不敏感即视为探测流量（模型测试/健康检查）
	probe := isProbeRequest(r)
	body, err := io.ReadAll(io.LimitReader(r.Body, maxBodyBytes+1))
	if err != nil {
		writeOpenAIError(w, http.StatusBadRequest, "invalid_request", "read body: "+err.Error())
		return
	}
	if len(body) > maxBodyBytes {
		writeOpenAIError(w, http.StatusRequestEntityTooLarge, "request_too_large", "request body exceeds 8MB limit")
		return
	}
	var peek struct {
		Stream bool   `json:"stream"`
		Model  string `json:"model"`
		Tools  []any  `json:"tools"`
	}
	_ = json.Unmarshal(body, &peek)

	// 请求级统计：出口打印表格日志
	st := newChatStat(time.Now(), body, peek.Stream)
	// lastErr 提前声明：出口兜底记账的 defer 要读它（成功路径已落账则跳过）
	var lastErr error
	defer func() {
		st.done()
		h.recordFailure(r, st, lastErr)
	}()

	sessionKey := extractSessionKey(r, body)

	configName, err := h.mapModel(peek.Model)
	if err != nil {
		st.status = http.StatusBadRequest
		writeOpenAIError(w, http.StatusBadRequest, "invalid_request", err.Error())
		return
	}

	// 若请求携带外部工具定义 (tools)，说明客户端期望进行 Function Calling (如 DSH / Agent 自动化工具)
	// 由于 Work 专有通道是 Trae 内部自主 Agent 回环、无法消费外部自定义 tools，若传入了 tools 则将
	// Official Work 模型对齐回原生裸模型名称（如 DeepSeek-V4-Flash），交由支持工具调用的 SOLO 裸模型通道处理
	hasTools := len(peek.Tools) > 0
	if hasTools && strings.HasSuffix(configName, "-Official") {
		configName = strings.TrimSuffix(configName, "-Official")
	}

	body = setModelInBody(body, configName)

	// 若显式请求 Work 模型（且未携带外部自定义 tools），走 Work 通道多账号动态调度（带会话粘性，消费 work_credits）
	if isWorkModel(peek.Model) && !hasTools {
		if h.cfg.WorkMode == upstream.WorkModeDisabled || (h.cfg.WorkClient != nil && h.cfg.WorkClient.Mode() == upstream.WorkModeDisabled) {
			st.status = http.StatusForbidden
			writeOpenAIError(w, http.StatusForbidden, "work_disabled", "work channel is disabled by configuration (TW2A_WORK_MODE=disabled)")
			return
		}
		handled, err := h.executeWorkRequest(w, r, body, configName, peek.Stream, sessionKey, st)
		if !handled {
			st.status = http.StatusServiceUnavailable
			msg := "all accounts unavailable for work channel"
			if err != nil {
				msg += ": " + err.Error()
			}
			writeOpenAIError(w, http.StatusServiceUnavailable, "no_healthy_account", msg)
		} else {
			st.status = http.StatusOK
		}
		return
	}

	// 在途租约管理：出口统一释放
	var heldUID string
	defer func() {
		if heldUID != "" {
			h.cfg.Pool.Release(heldUID)
		}
	}()
	acquireAcct := func(uid string) {
		if heldUID != "" {
			h.cfg.Pool.Release(heldUID)
		}
		heldUID = uid
		h.cfg.Pool.Acquire(uid)
	}

	// 能力过滤（2026-10-27）：solo_agent 的模型集按账号漂移（实测 4 号分别
	// 68/68/69/71 条，free/flash/pro/claude-opus-5 只在一个号上）。并集刷新已把
	// 每号能力写进 pool，这里据此只选真能承接该 config 的号 —— 否则模型列表
	// 说有、轮到没这个号的账号就失败，等于把 400 换成了 503。
	//
	// 只在「该 config 确实被某个号报告过」时开启过滤：查不到的名字（Work 透传
	// 模型、或能力数据尚未探到）必须回退成不限制。能力过滤是优化，不是闸门 ——
	// 拿缺失的数据去拒请求，会把本来能跑通的调用打死。
	capFilter := ""
	if h.cfg.Pool.ModelKnown(configName) {
		capFilter = configName
	}

	tried := map[string]bool{}
	for i := 0; i < h.cfg.MaxRotate; i++ {
		// 注意：换模型会重绑 sessionKey（绑定账号没这能力时按能力重选），
		// 牺牲一点 KV cache 命中率换路由正确性 —— 后者更重要。
		acct := h.cfg.Pool.PickAffinityForModel(sessionKey, tried, capFilter)
		if acct == nil {
			break
		}
		tried[acct.UID] = true
		acquireAcct(acct.UID)
		st.uid = acct.UID

		// token 临近过期 → 先 refresh（持锁重查，避免并发重复轮换；失败冷却换号）
		refreshed, err := h.cfg.Upstream.RefreshTokenIfNeeded(acct, h.cfg.RefreshSkew)
		if err != nil {
			lastErr = err
			h.cfg.Pool.RecordError(acct.UID)
			var ue *upstream.Error
			if errors.As(err, &ue) && ue.Kind == upstream.ErrSessionDead {
				h.cfg.Pool.Disable(acct.UID, "refresh session dead")
			} else {
				h.cfg.Pool.Cooldown(acct.UID, pool.CoolErr, h.cfg.ErrCooldown, "refresh: "+err.Error())
			}
			continue
		}
		if refreshed {
			_ = acct.SaveAtomic()
		}

		// 接 r.Context()：客户端断连（含 Cloudflare 100s 524 掐断）立即取消上游，
		// 避免下游放弃后上游仍空烧算力（拥堵窗口的主要放大器之一）。
		rc, status, respBody, terr := h.cfg.Upstream.ChatStream(r.Context(), acct, body)
		if terr != nil {
			lastErr = terr
			h.cfg.Pool.RecordError(acct.UID)
			// 首字节超时（timeout awaiting response headers）也是全局排队信号：
			// 换号只会往同一个拥堵的算力池再灌一份流量，故快速失败而非轮换。
			var ne net.Error
			if errors.As(terr, &ne) && ne.Timeout() {
				st.status = http.StatusGatewayTimeout
				w.Header().Set("Retry-After", "8")
				writeOpenAIError(w, http.StatusGatewayTimeout, "upstream_busy",
					"upstream timed out before first byte (global congestion), not rotating accounts: "+terr.Error())
				return
			}
			h.cfg.Pool.NoteError(acct.UID, h.cfg.ErrThreshold, h.cfg.ErrCooldown)
			continue
		}
		if status >= 400 {
			h.cfg.Pool.RecordError(acct.UID)
			st.status = status
			kind := upstream.Classify(status, string(respBody))
			switch kind {
			case upstream.ErrPlanLimit:
				h.cfg.Pool.Cooldown(acct.UID, pool.CoolPlan, h.cfg.PlanCooldown, "plan 权益不足")
				lastErr = &upstream.Error{Kind: kind, Status: status, Msg: string(respBody)}
				if handled, _ := h.executeWorkRequest(w, r, body, configName, peek.Stream, sessionKey, st); handled {
					st.status = http.StatusOK
					return
				}
				continue
			case upstream.ErrSoftRate:
				h.cfg.Pool.Cooldown(acct.UID, pool.CoolSoft, h.cfg.SoftCooldown, "429 rate limit")
				lastErr = &upstream.Error{Kind: kind, Status: status, Msg: string(respBody)}
				continue
			case upstream.ErrSessionDead:
				h.cfg.Pool.Disable(acct.UID, "session dead")
				lastErr = &upstream.Error{Kind: kind, Status: status, Msg: string(respBody)}
				continue
			case upstream.ErrNotFound:
				// 404 短冷却不累计 errCount（防雪崩）
				h.cfg.Pool.Cooldown(acct.UID, pool.CoolSoft, h.cfg.SoftCooldown, "upstream 404")
				lastErr = &upstream.Error{Kind: kind, Status: status, Msg: string(respBody)}
				continue
			case upstream.ErrUpstreamBusy:
				// 502/504 是边缘网关（volc-dcdn/tengine）在算力紧张时掐断连接，与账号无关。
				// 换号只会把 1 个请求放大成 N 个上游请求，加剧拥堵 → 不冷却、不计错、不轮换，
				// 直接快速失败，把重试/换源的决定权交回调用方（如 CliRelay 的多源路由）。
				st.status = status
				w.Header().Set("Retry-After", "8")
				writeOpenAIError(w, status, "upstream_busy",
					"upstream gateway busy (global congestion), not rotating accounts")
				return
			default:
				h.cfg.Pool.NoteError(acct.UID, h.cfg.ErrThreshold, h.cfg.ErrCooldown)
				lastErr = &upstream.Error{Kind: kind, Status: status, Msg: string(respBody)}
				continue
			}
		}
		if peek.Stream {
			h.cfg.Pool.NoteSuccess(acct.UID)
			h.cfg.Pool.RecordSuccess(acct.UID)
			st.status = http.StatusOK
			statsR := newChatStatsReaderSince(rc, st.start)
			// 流内业务错误（1005 plan/5xx 等）→ 冷却账号，错误信息注入 SSE。
			// Keepalive > 0 时在首帧到达前发 `: ping` 注释帧，防边缘网关静默掐断（502）。
			_ = upstream.StreamWithUsageKeepalive(w, statsR, func(se *upstream.SOLOStreamError) {
				h.handleStreamError(acct.UID, se)
			}, func(remain float64) {
				// 流内记账（2026-10-16 定案）：notify_usage 账单事件 → 本地包账本
				// FIFO 扣减。零额外上游请求，扣费后余额实时进 pool。
				h.cfg.Pool.ApplyUsageRemain(acct.UID, remain)
			}, h.cfg.Keepalive)
			rc.Close()
			if toks, ok := statsR.Tokens(); ok {
				st.toks = toks
			}
			st.ttfb = statsR.TTFB()
			// 用量落盘：流式拿不到输入 tokens，记 completion 数（探测/真实分流足够）
			noteUsage(st, usageRecord{
				Dir: h.cfg.UsageDir, Model: peek.Model, Probe: probe, UID: acct.UID,
				Status: http.StatusOK, Mode: "stream", Out: max(st.toks, 0),
				Ms: msSince(st), TTFB: st.ttfb.Milliseconds(),
			})
			return
		}
		resp, notifyRemain, haveRemain, err := upstream.Aggregate(rc)
		rc.Close() // 已完全消费，立即释放上游连接（防轮转 continue 泄漏 body）
		if err != nil {
			h.cfg.Pool.RecordError(acct.UID)
			// 流内业务错误（如 1005 plan 权益不足）→ 冷却账号并轮转下一账号。
			var se *upstream.SOLOStreamError
			if errors.As(err, &se) {
				lastErr = err
				switch se.Kind() {
				case upstream.ErrPlanLimit:
					h.cfg.Pool.Cooldown(acct.UID, pool.CoolPlan, h.cfg.PlanCooldown, "plan 权益不足")
				default:
					h.cfg.Pool.NoteError(acct.UID, h.cfg.ErrThreshold, h.cfg.ErrCooldown)
				}
				continue
			}
			st.status = http.StatusBadGateway
			writeOpenAIError(w, http.StatusBadGateway, "upstream_parse", err.Error())
			return
		}
		h.cfg.Pool.NoteSuccess(acct.UID)
		h.cfg.Pool.RecordSuccess(acct.UID)
		st.status = http.StatusOK
		st.toks = completionTokens(resp)
		if haveRemain {
			// 流内记账：非流式出口同样顺带拿扣费后余额（零额外上游请求）。
			h.cfg.Pool.ApplyUsageRemain(acct.UID, notifyRemain)
		}
		noteUsage(st, usageRecord{
			Dir: h.cfg.UsageDir, Model: peek.Model, Probe: probe, UID: acct.UID,
			Status: http.StatusOK, Mode: "sync", In: promptTokens(resp), Out: max(st.toks, 0),
			Ms: msSince(st),
		})
		writeJSON(w, http.StatusOK, resp)
		return
	}
	// 所有账号耗尽/冷却，尝试 Work 通道兜底消费 work_credits
	if handled, _ := h.executeWorkRequest(w, r, body, configName, peek.Stream, sessionKey, st); handled {
		st.status = http.StatusOK
		return
	}
	st.status = http.StatusServiceUnavailable
	// 一次号都没轮到 + 开了能力过滤 = 池里没有能承接该模型的账号。
	// 这和「全在冷却」是两回事，分开报错才排得动（也方便上游侧看出是能力漂移还是号池耗尽）。
	if capFilter != "" && len(tried) == 0 {
		writeOpenAIError(w, http.StatusServiceUnavailable, "model_unavailable",
			"no healthy account serves model "+capFilter+" (per-account model set)")
		return
	}
	msg := "all accounts unavailable (cooling/disabled)"
	if lastErr != nil {
		msg += ": " + lastErr.Error()
	}
	writeOpenAIError(w, http.StatusServiceUnavailable, "no_healthy_account", msg)
}

// handleStreamError 流式响应中的上游业务错误 → pool 冷却状态机。
// 1005 / 4008 plan 权益不足 → 长冷却；4011 限流 → 短冷却；其余（5xx/参数错误等）→ 累计错误冷却。
func (h *Handler) handleStreamError(uid string, se *upstream.SOLOStreamError) {
	switch se.Kind() {
	case upstream.ErrPlanLimit:
		h.cfg.Pool.Cooldown(uid, pool.CoolPlan, h.cfg.PlanCooldown, "plan 权益不足")
	case upstream.ErrSoftRate:
		h.cfg.Pool.Cooldown(uid, pool.CoolSoft, h.cfg.SoftCooldown, "429 rate limit")
	default:
		h.cfg.Pool.NoteError(uid, h.cfg.ErrThreshold, h.cfg.ErrCooldown)
	}
}

// ---------------------------------------------------------------------------
// helpers
// ---------------------------------------------------------------------------

func writeJSON(w http.ResponseWriter, status int, v any) {
	raw, _ := json.Marshal(v)
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write(raw)
}

func writeOpenAIError(w http.ResponseWriter, status int, code, msg string) {
	writeJSON(w, status, map[string]any{
		"error": map[string]any{
			"message": msg,
			"type":    "api_error",
			"code":    code,
		},
	})
}
