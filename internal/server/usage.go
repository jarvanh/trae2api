// usage.go 每请求用量落盘：/app/data/usage-<北京日期>.jsonl。
//
// 供 quota-board 做「探测/真实用量」分流：探测请求带 X-Probe: 1 请求头，
// 落盘时 probe=true，下游据此把探测流量与真实流量分开统计。
//
// 设计纪律：
//   - best-effort：任何失败只 log，绝不阻断主流程（用量统计不能影响对话可用性）
//   - 单文件追加写：O_APPEND + sync.Mutex，多请求并发安全
//   - 按北京自然日切分文件，避免 UTC 日期跨天错位
package server

import (
	"encoding/json"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// beijing 北京时区（UTC+8），用量文件按北京自然日切分。
var beijing = time.FixedZone("CST", 8*3600)

// isProbeRequest 判定探测请求：X-Probe 为 1/true/yes（大小写不敏感，允许首尾空格）。
func isProbeRequest(r *http.Request) bool {
	switch strings.ToLower(strings.TrimSpace(r.Header.Get("X-Probe"))) {
	case "1", "true", "yes":
		return true
	}
	return false
}

// usageMu 保护 usage 文件写入（多请求并发追加）。
var usageMu sync.Mutex

// usageEntry 单条用量记录（一行 JSON，字段名精简以压缩体积）。
type usageEntry struct {
	T     int64  `json:"t"`     // unix 秒（请求完成时刻）
	Model string `json:"model"` // 模型名（客户端原始传入值）
	Probe bool   `json:"probe"` // 是否探测请求（X-Probe: 1/true/yes）
	In    int    `json:"in"`    // 输入 token 数（缺失为 0）
	Out   int    `json:"out"`   // 输出 token 数（缺失为 0）
}

// recordUsage 追加一条用量记录到 <dir>/usage-<北京日期>.jsonl。
// best-effort：目录不存在则创建；任何错误只 log，不返回错误、不 panic。
func recordUsage(dir, model string, probe bool, inTok, outTok int) {
	if dir == "" {
		return
	}
	if inTok < 0 {
		inTok = 0
	}
	if outTok < 0 {
		outTok = 0
	}
	if model == "" {
		model = "-"
	}

	e := usageEntry{
		T:     time.Now().Unix(),
		Model: model,
		Probe: probe,
		In:    inTok,
		Out:   outTok,
	}
	raw, err := json.Marshal(e)
	if err != nil {
		log.Printf("[usage] marshal failed: %v", err)
		return
	}
	raw = append(raw, '\n')

	usageMu.Lock()
	defer usageMu.Unlock()

	if err := os.MkdirAll(dir, 0o755); err != nil {
		log.Printf("[usage] mkdir %s failed: %v", dir, err)
		return
	}
	path := filepath.Join(dir, "usage-"+time.Now().In(beijing).Format("2006-01-02")+".jsonl")
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		log.Printf("[usage] open %s failed: %v", path, err)
		return
	}
	defer f.Close()
	if _, err := f.Write(raw); err != nil {
		log.Printf("[usage] write %s failed: %v", path, err)
	}
}
