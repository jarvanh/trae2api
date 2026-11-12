// login.go Web 登录闭环：生成登录 URL → pending 态 → /authorize 回调捕获 →
// ExchangeToken + GetUserInfo + 落盘 → 面板轮询 result。
//
// 回调端口策略（M0 结论：双端口 18080）：
//   - TRAE 登录页回调到 http://127.0.0.1:18080/authorize
//   - main.go 起第二个 http.Server 监听 18080，只挂 CallbackHandler（最小路由：
//     仅 /authorize），不暴露管理/代理路由（T8）；
//   - 回调必须绑定到管理面已发起的 pending 登录后才会落盘（T1）。
//   - 回调捕获后直接在服务端完成导入（用户无需再粘赖）
//
// pending 态只在内存（重启丢失），符合「登录态瞬时」语义。
package server

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"trae2api/internal/auth"
)

// pendingState pending 登录状态。
type pendingState string

const (
	pendingStateActive     pendingState = "pending"    // 等待用户登录回调
	pendingStateProcessing pendingState = "processing" // 回调已绑定，正在换 token/落盘（单次消费）
	pendingStateSuccess    pendingState = "success"    // 回调捕获 + 落盘成功
	pendingStateFailed     pendingState = "failed"     // ExchangeToken/落盘失败
	pendingStateCanceled   pendingState = "canceled"   // 用户取消
)

// pendingLogin 单次登录的临时上下文。
type pendingLogin struct {
	state       pendingState
	machineID   string
	deviceID    string
	callbackURL string // auth_callback_url（含端口）
	createdAt   time.Time

	// success 时填
	uid      string
	nickname string
	// failed 时填
	errMsg string
}

// loginStartResponse POST /admin/api/login 返回。
type loginStartResponse struct {
	LoginURL    string `json:"login_url"`
	PendingID   string `json:"pending_id"`
	CallbackURL string `json:"callback_url"`
}

// adminLoginStart POST /admin/api/login：生成登录 URL + pending 态。
// body 可选 {callback_port: "18080"}；默认用 127.0.0.1:18080/authorize。
func (h *Handler) adminLoginStart(w http.ResponseWriter, r *http.Request) {
	var req struct {
		CallbackPort string `json:"callback_port,omitempty"`
	}
	_ = decodeBodyOptional(r, &req)
	if req.CallbackPort == "" {
		req.CallbackPort = "18080"
	}
	// 端口必须是纯数字：它会拼进 auth_callback_url，不能容忍 "/"、"?" 等
	// 注入额外 path/query 的字符。
	if n, perr := strconv.Atoi(req.CallbackPort); perr != nil || n < 1 || n > 65535 {
		writeOpenAIError(w, http.StatusBadRequest, "invalid_request", "callback_port must be a TCP port (1-65535)")
		return
	}
	callbackURL := "http://127.0.0.1:" + req.CallbackPort + "/authorize"

	machineID, err := randomHex(16) // hex32
	if err != nil {
		writeOpenAIError(w, http.StatusInternalServerError, "rand_failed", err.Error())
		return
	}
	// deviceID 用「签到兼容」的 16 位纯数字（见 internal/auth.NewCheckinDeviceID），
	// 与真实注册设备号格式一致，保证该账号随后的每日签到能成功且与其他账号互异。
	deviceID, err := auth.NewCheckinDeviceID()
	if err != nil {
		writeOpenAIError(w, http.StatusInternalServerError, "rand_failed", err.Error())
		return
	}
	loginURL := BuildLoginURL(machineID, deviceID, callbackURL)

	pendingID, err := randomHex(8) // hex16
	if err != nil {
		writeOpenAIError(w, http.StatusInternalServerError, "rand_failed", err.Error())
		return
	}
	pl := &pendingLogin{
		state:       pendingStateActive,
		machineID:   machineID,
		deviceID:    deviceID,
		callbackURL: callbackURL,
		createdAt:   time.Now(),
	}
	h.loginMu.Lock()
	h.logins[pendingID] = pl
	h.loginMu.Unlock()

	writeJSON(w, http.StatusOK, loginStartResponse{
		LoginURL: loginURL, PendingID: pendingID, CallbackURL: callbackURL,
	})
}

// adminLoginResult GET /admin/api/login/result?pending_id=...
func (h *Handler) adminLoginResult(w http.ResponseWriter, r *http.Request) {
	pendingID := r.URL.Query().Get("pending_id")
	pl, ok := h.getPending(pendingID)
	if !ok {
		writeOpenAIError(w, http.StatusNotFound, "not_found", "pending login not found (expired or invalid)")
		return
	}
	resp := map[string]any{
		"pending_id": pendingID,
		"state":      string(pl.state),
	}
	if pl.state == pendingStateSuccess {
		resp["uid"] = pl.uid
		resp["nickname"] = pl.nickname
	} else if pl.state == pendingStateFailed {
		resp["error"] = pl.errMsg
	}
	writeJSON(w, http.StatusOK, resp)
}

// adminLoginCancel POST /admin/api/login/cancel（body {pending_id} 或 query ?pending_id=）
func (h *Handler) adminLoginCancel(w http.ResponseWriter, r *http.Request) {
	pendingID := r.URL.Query().Get("pending_id")
	if pendingID == "" {
		var req struct {
			PendingID string `json:"pending_id"`
		}
		_ = decodeBodyOptional(r, &req)
		pendingID = req.PendingID
	}
	h.loginMu.Lock()
	pl, ok := h.logins[pendingID]
	if ok && pl.state == pendingStateActive {
		pl.state = pendingStateCanceled
	}
	// 直接删 pending（取消后不再保留）
	delete(h.logins, pendingID)
	h.loginMu.Unlock()
	writeJSON(w, http.StatusOK, map[string]any{"pending_id": pendingID, "canceled": ok})
}

// authorizeCallback GET /authorize：TRAE 回调落点。
// 捕获 query → 解析 → ExchangeToken → GetUserInfo → 落盘 → 标记 pending 成功。
//
// 安全约束（T1）：
//   - 回调必须绑定到一次已由管理面发起的 pending 登录（loginTraceID / machine_id
//     反查，且 pending 仍为 active）；找不到即拒绝，绝不「无 pending 直接落盘」。
//   - UID 只信服务端 GetUserInfo 的结果，且必须过 auth.ValidUID 白名单；
//     query 里的 userInfo.UserID 只作为候选，不能绕过校验写文件。
//   - 落盘路径由 auth.FilePathFor 再校验一次，防目录穿越。
func (h *Handler) authorizeCallback(w http.ResponseWriter, r *http.Request) {
	rawURL := r.URL.String()
	if !strings.HasPrefix(rawURL, "http") {
		// /authorize?... → 补全成完整 URL 供 ParseCallback
		rawURL = "http://127.0.0.1" + rawURL
	}
	info, err := ParseCallback(rawURL)
	if err != nil {
		// 回调解析失败：展示一个友好错误页（非 JSON，因为是浏览器跳转）
		authorizeRender(w, http.StatusBadRequest, "登录回调解析失败", err.Error())
		return
	}
	// TRAE 回调不回传 machine_id/device_id/pending_id，但回传 loginTraceID
	//（= machineTraceID(machine,device) 派生）。必须用它（或 machine_id）反查到
	// 一个尚未消费的 pending，才能继续落盘。
	pl, ok := h.bindPendingCallback(
		r.URL.Query().Get("machine_id"),
		r.URL.Query().Get("device_id"),
		r.URL.Query().Get("loginTraceID"),
	)
	if !ok {
		authorizeRender(w, http.StatusForbidden, "登录回调无效",
			"未找到对应的登录流程（pending 不存在、已使用或已过期）。请回到管理面板重新发起登录。")
		return
	}
	machineID, deviceID := pl.machineID, pl.deviceID

	a := &auth.Auth{
		AccessToken:  info.AccessToken,
		RefreshToken: info.RefreshToken,
		UID:          info.UID,
		Nickname:     info.Nickname,
		EnterpriseID: info.EnterpriseID,
		Domain:       "trae.cn",
		ApiHost:      "https://api.trae.com.cn",
		MachineID:    machineID,
		DeviceID:     deviceID,
		ExpiresAt:    info.ExpiresAt,
	}
	// 有 refreshToken → ExchangeToken 换新 access + 轮换 refreshToken
	if a.RefreshToken != "" {
		if exErr := h.cfg.Upstream.RefreshToken(a); exErr != nil {
			h.markPendingByMachine(machineID, "", pendingStateFailed, "", "", exErr.Error())
			authorizeRender(w, http.StatusBadGateway, "ExchangeToken 失败", exErr.Error())
			return
		}
	}
	// UID 只信服务端 GetUserInfo：query 里的 userInfo 可被回调链接伪造，
	// 不能作为落盘依据。GetUserInfo 失败即失败关闭（T1）。
	uid, nick, ent, gErr := h.cfg.Upstream.GetUserInfo(a)
	if gErr != nil {
		h.markPendingByMachine(machineID, "", pendingStateFailed, "", "", gErr.Error())
		authorizeRender(w, http.StatusBadGateway, "获取账号信息失败", gErr.Error())
		return
	}
	if uid != "" {
		a.UID = uid
	}
	if nick != "" {
		a.Nickname = nick
	}
	if ent != "" {
		a.EnterpriseID = ent
	}
	if a.UID == "" {
		err := errors.New("cannot determine uid from GetUserInfo")
		h.markPendingByMachine(machineID, "", pendingStateFailed, "", "", err.Error())
		authorizeRender(w, http.StatusBadRequest, "登录失败", err.Error())
		return
	}
	if !auth.ValidUID(a.UID) {
		err := errors.New("invalid uid in callback (must match ^[A-Za-z0-9_-]{1,64}$)")
		h.markPendingByMachine(machineID, "", pendingStateFailed, "", "", err.Error())
		authorizeRender(w, http.StatusBadRequest, "登录失败", err.Error())
		return
	}
	if a.AccessToken == "" {
		err := errors.New("no access token after exchange")
		h.markPendingByMachine(machineID, "", pendingStateFailed, "", "", err.Error())
		authorizeRender(w, http.StatusBadRequest, "登录失败", err.Error())
		return
	}

	// 落盘
	fp, fpErr := auth.FilePathFor(h.cfg.AuthDir, a.UID)
	if fpErr != nil {
		h.markPendingByMachine(machineID, "", pendingStateFailed, "", "", fpErr.Error())
		authorizeRender(w, http.StatusBadRequest, "登录失败", fpErr.Error())
		return
	}
	a.FilePath = fp
	if mkErr := mkdirAll(h.cfg.AuthDir); mkErr != nil {
		h.markPendingByMachine(machineID, "", pendingStateFailed, a.UID, a.Nickname, mkErr.Error())
		authorizeRender(w, http.StatusInternalServerError, "落盘失败", mkErr.Error())
		return
	}
	if err := a.SaveAtomic(); err != nil {
		h.markPendingByMachine(machineID, "", pendingStateFailed, a.UID, a.Nickname, err.Error())
		authorizeRender(w, http.StatusInternalServerError, "落盘失败", err.Error())
		return
	}
	h.cfg.Pool.Add(a)
	h.markPendingByMachine(machineID, "", pendingStateSuccess, a.UID, a.Nickname, "")

	authorizeRender(w, http.StatusOK, "登录成功", "账号 "+a.UID+"（"+a.Nickname+"）已添加，可关闭此窗口返回面板。")
}

// markPendingByMachine 用 machineID（或 loginTraceID 派生匹配）反查 pending 并标记状态。
func (h *Handler) markPendingByMachine(machineID, traceID string, state pendingState, uid, nick, errMsg string) {
	if machineID == "" && traceID == "" {
		return
	}
	h.loginMu.Lock()
	defer h.loginMu.Unlock()
	for _, pl := range h.logins {
		if pl.machineID == machineID || (traceID != "" && machineTraceID(pl.machineID, pl.deviceID) == traceID) {
			pl.state = state
			if uid != "" {
				pl.uid = uid
			}
			if nick != "" {
				pl.nickname = nick
			}
			if errMsg != "" {
				pl.errMsg = errMsg
			}
			return
		}
	}
}

// bindPendingCallback 把一次 /authorize 回调绑定到已发起的 pending 登录，
// 并原子地把该 pending 置为 processing（单次消费）。
//
// 匹配优先级：loginTraceID（TRAE 回调实际回传，= machineTraceID(machine,device)）
// 或 machine_id（兼容显式带上 machine/device 的客户端）。只接受 active 且未过期
// 的 pending；没有任何匹配 → false，调用方必须拒绝，不能落盘。
func (h *Handler) bindPendingCallback(machineID, deviceID, traceID string) (*pendingLogin, bool) {
	if machineID == "" && traceID == "" {
		return nil, false
	}
	h.loginMu.Lock()
	defer h.loginMu.Unlock()
	for id, pl := range h.logins {
		if time.Since(pl.createdAt) > 10*time.Minute {
			delete(h.logins, id)
			continue
		}
		if pl.state != pendingStateActive {
			continue
		}
		traceMatch := traceID != "" && machineTraceID(pl.machineID, pl.deviceID) == traceID
		machineMatch := machineID != "" && pl.machineID == machineID &&
			(deviceID == "" || pl.deviceID == deviceID)
		if !traceMatch && !machineMatch {
			continue
		}
		// 消费掉 pending：并发/重放的回调不能再绑到同一流程。
		pl.state = pendingStateProcessing
		return pl, true
	}
	return nil, false
}

// getPending 取 pending（不存在或已过期 false）。pending TTL 10 分钟。
func (h *Handler) getPending(id string) (*pendingLogin, bool) {
	if id == "" {
		return nil, false
	}
	h.loginMu.Lock()
	defer h.loginMu.Unlock()
	pl, ok := h.logins[id]
	if !ok {
		return nil, false
	}
	if time.Since(pl.createdAt) > 10*time.Minute {
		delete(h.logins, id)
		return nil, false
	}
	return pl, true
}

// randomHex 生成 n 字节随机 hex（2n hex 字符）。
func randomHex(n int) (string, error) {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}
