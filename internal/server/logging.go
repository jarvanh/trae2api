// logging.go 请求级表格日志：每个 /v1/chat/completions 请求结束后打印一行到 stdout。
package server

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"sync/atomic"
	"time"
)

// chatSeq 进程级请求序号。
var chatSeq atomic.Int64

// chatLogEnabled 聊天表格日志总开关。生产恒 true；
var chatLogEnabled = true

// chatStat 单个 chat 请求的日志统计；handler 挂 defer，请求出口后落一行。
type chatStat struct {
	start  time.Time
	model  string
	mode   string // "stream" | "sync"
	uid    string // 完整 uid，展示时只取前 8 位
	ttfb   time.Duration
	toks   int // <0 表示 usage 缺失 → 显示 "-"
	status int

	logged      bool
	usageLogged bool // 本次请求已在成功路径落过用量账（避免失败兜底重复写一行）
}

// newChatStat 以请求进入 handler 的时刻为起点构造统计对象；toks 默认 -1（usage 缺失）。
func newChatStat(now time.Time, body []byte, stream bool) *chatStat {
	mode := "sync"
	if stream {
		mode = "stream"
	}
	return &chatStat{start: now, model: parseModelFromBody(body), mode: mode, toks: -1}
}

// done 幂等落一行表格日志。
func (s *chatStat) done() {
	if s.logged {
		return
	}
	s.logged = true
	logChatRow(s.ttfb, time.Since(s.start), s.model, s.mode, s.uid, s.status, s.toks)
}

// msSince 返回从 s.start 到现在的耗时（毫秒）。
// s 为 nil 或 start 为零值时返回 0 —— 用量流水是 best-effort 旁路，
// 宁可记 0 也不能让记账逻辑 panic 或反向影响主流程。
func msSince(s *chatStat) int64 {
	if s == nil || s.start.IsZero() {
		return 0
	}
	return time.Since(s.start).Milliseconds()
}

// noteUsage 在成功路径落一次用量账并打标。
//
// 必须经由它（而非直接 recordUsage）记账：请求出口有「失败兜底」逻辑，
// 靠 usageLogged 判断这次请求是否已被成功路径记过；漏打标会让成功的请求
// 被再补一行「失败」记录，把面板的成功率与吞吐量都算错。
func noteUsage(s *chatStat, rec usageRecord) {
	if s != nil {
		s.usageLogged = true
	}
	recordUsage(rec)
}

// recordFailure 失败请求兜底记账：成功路径已通过 noteUsage 落账的请求会跳过。
//
// 为什么必须补这一层：原实现只在拿到 token 的成功路径落盘，失败请求
// （上游 502/超时/全部账号冷却/参数错误）完全不入账 —— 面板看到的吞吐量
// 会明显虚低，而排障时最关键的错误样本恰好是缺的那一批。
//
// 仍遵循 best-effort：只追加一行，绝不改变已写出的响应。
func (h *Handler) recordFailure(r *http.Request, st *chatStat, lastErr error) {
	if st == nil || st.usageLogged || h.cfg.UsageDir == "" {
		return
	}
	status := st.status
	if status >= 200 && status < 300 {
		return // 成功却没记过账（理论上不该发生）：宁可不记，也不能误写成失败
	}
	if status == 0 {
		status = http.StatusInternalServerError // 未走到写响应：异常出口
	}
	var errMsg string
	if lastErr != nil {
		errMsg = lastErr.Error()
	}
	recordUsage(usageRecord{
		Dir:    h.cfg.UsageDir,
		Model:  st.model,
		Probe:  isProbeRequest(r),
		UID:    st.uid,
		Status: status,
		Mode:   st.mode,
		Ms:     msSince(st),
		TTFB:   st.ttfb.Milliseconds(),
		Err:    errMsg,
	})
}

// soloStatsReader 解析原生 SOLO SSE（event:/data: 双行），记录首个 output 事件的
// TTFB 与 token_usage 事件的 completion_tokens。原始字节原样返回给下游透传。
type soloStatsReader struct {
	br       *bufio.Reader
	start    time.Time
	ttfb     time.Duration
	seen     bool // 已见过首个 output 事件（TTFB 只记一次）
	hasUsage bool // 末帧是否带 token_usage
	tokens   int
	pend     []byte // 已读未返回的行缓存
	event    string
}

// newChatStatsReaderSince 以 since 为 TTFB 计时起点（通常是请求进入 handler 的时刻）。
func newChatStatsReaderSince(r io.Reader, since time.Time) *soloStatsReader {
	return &soloStatsReader{br: bufio.NewReaderSize(r, 64*1024), start: since}
}

// TTFB 返回首个 output 事件到达耗时；无帧时为 0。
func (s *soloStatsReader) TTFB() time.Duration { return s.ttfb }

// Tokens 返回 token_usage 的 completion_tokens 与是否缺失；无 usage 时 ok=false。
func (s *soloStatsReader) Tokens() (int, bool) { return s.tokens, s.hasUsage }

// parseSSELine 解析原生 SOLO 事件行：首个 output 事件记 TTFB，token_usage 事件采信 usage。
func (s *soloStatsReader) parseSSELine(line string) {
	line = strings.TrimRight(line, "\r\n")
	switch {
	case strings.HasPrefix(line, "event:"):
		s.event = strings.TrimSpace(strings.TrimPrefix(line, "event:"))
		return
	case strings.HasPrefix(line, "data:"):
		payload := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if payload == "" {
			return
		}
		switch s.event {
		case "output":
			if !s.seen {
				s.seen = true
				s.ttfb = time.Since(s.start)
			}
		case "token_usage":
			var u struct {
				CompletionTokens int `json:"completion_tokens"`
			}
			if json.Unmarshal([]byte(payload), &u) == nil && u.CompletionTokens > 0 {
				s.hasUsage = true
				s.tokens = u.CompletionTokens
			}
		}
		return
	default:
		// OpenAI 兼容透传行（如 data: {...}）的兜底：首帧记 TTFB，末帧 usage 采信。
		if !strings.HasPrefix(line, "data:") {
			return
		}
		payload := strings.TrimPrefix(line, "data: ")
		if payload == "[DONE]" {
			return
		}
		if !s.seen {
			s.seen = true
			s.ttfb = time.Since(s.start)
		}
		var chunk struct {
			Usage *struct {
				CompletionTokens int `json:"completion_tokens"`
			} `json:"usage"`
		}
		if json.Unmarshal([]byte(payload), &chunk) == nil && chunk.Usage != nil {
			s.hasUsage = true
			s.tokens = chunk.Usage.CompletionTokens
		}
	}
}

// chatStatsReader 别名（兼容旧名）。
type chatStatsReader = soloStatsReader

// Read 返回原始数据，同时解析统计 TTFB/token。
func (s *chatStatsReader) Read(p []byte) (int, error) {
	if len(s.pend) > 0 {
		n := copy(p, s.pend)
		s.pend = s.pend[n:]
		return n, nil
	}
	line, err := s.br.ReadString('\n')
	if line != "" {
		s.parseSSELine(line)
		s.pend = []byte(line)
		n := copy(p, s.pend)
		s.pend = s.pend[n:]
		return n, nil
	}
	return 0, err
}

// parseModelFromBody 从请求 JSON 取 model 字段，缺省标 "-"。
func parseModelFromBody(body []byte) string {
	var obj struct {
		Model string `json:"model"`
	}
	if err := json.Unmarshal(body, &obj); err != nil || obj.Model == "" {
		return "-"
	}
	return obj.Model
}

// completionTokens 从 Aggregate 返回的响应中提取 usage.completion_tokens；缺失返回 -1。
func completionTokens(resp map[string]any) int {
	u, ok := resp["usage"].(map[string]any)
	if !ok {
		return -1
	}
	v, ok := u["completion_tokens"].(float64)
	if !ok {
		return -1
	}
	return int(v)
}

// promptTokens 同上，提取 usage.prompt_tokens（用量记账用，缺失返回 -1 → recordUsage 钻 0）。
func promptTokens(resp map[string]any) int {
	u, ok := resp["usage"].(map[string]any)
	if !ok {
		return -1
	}
	v, ok := u["prompt_tokens"].(float64)
	if !ok {
		return -1
	}
	return int(v)
}

// uidPrefix 只显示 uid 前 8 位；空 uid 显示 "-"。
func uidPrefix(uid string) string {
	if uid == "" {
		return "-"
	}
	if len(uid) > 8 {
		return uid[:8]
	}
	return uid
}

// logChatRow 打印一行请求级表格日志（直接输出 stdout，无 log 时间戳前缀）。
func logChatRow(ttfb, total time.Duration, model, mode, uid string, status int, toks int) {
	if !chatLogEnabled {
		return
	}
	seq := chatSeq.Add(1)
	if len(model) > 15 {
		model = model[:15]
	}
	tokField := "-"
	tokpsField := "-"
	if toks >= 0 {
		tokField = fmt.Sprintf("%d", toks)
		if total > 0 {
			tokpsField = fmt.Sprintf("%.1f", float64(toks)/total.Seconds())
		} else {
			tokpsField = "0.0"
		}
	}
	ttfbMS := "-"
	if ttfb > 0 {
		ttfbMS = fmt.Sprintf("%dms", ttfb.Milliseconds())
	}
	fmt.Fprintf(os.Stdout, "| #%03d | %s | %s | %s | %d | uid=%s | TTFB=%s | tok=%s | %stok/s | total=%.1fs |\n",
		seq,
		time.Now().Format("15:04:05"),
		model,
		mode,
		status,
		uidPrefix(uid),
		ttfbMS,
		tokField,
		tokpsField,
		total.Seconds(),
	)
}
