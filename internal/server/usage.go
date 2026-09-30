// usage.go 每请求用量落盘：/app/data/usage-<北京日期>.jsonl。
//
// 供 quota-board 做「探测/真实用量」分流：探测请求带 X-Probe: 1 请求头，
// 落盘时 probe=true，下游据此把探测流量与真实流量分开统计。
//
// 同时作为控制台「请求流水」的数据源：每行一条完整请求记录（时间/模型/账号/
// 通道/耗时/状态码/token），成功与失败均记账 —— 失败请求也是真实发生过的请求，
// 漏记会让排障时看到的吞吐量虚低。
//
// 设计纪律：
//   - best-effort：任何失败只 log，绝不阻断主流程（用量统计不能影响对话可用性）
//   - 单文件追加写：O_APPEND + sync.Mutex，多请求并发安全
//   - 按北京自然日切分文件，避免 UTC 日期跨天错位
//   - 向后兼容：历史上只有 {t,model,probe,in,out} 的行仍可解析（新字段读为零值）
package server

import (
	"bufio"
	"encoding/json"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"sort"
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

// maxUsageLimit 单次查询返回的最大明细条数（防爆内存/传输）。
const maxUsageLimit = 1000

// usageRecord 一次请求的落盘输入（recordUsage 的参数集合）。
// 成功路径由 handler 在拿到 token 后填满；失败路径在 defer 里补记，token 为 0。
type usageRecord struct {
	Dir    string // 落盘目录（空 = 不落盘）
	Model  string // 模型名（客户端原始传入值）
	Probe  bool   // 探测标记
	In     int    // 输入 token（缺失为 0）
	Out    int    // 输出 token（缺失为 0）
	UID    string // 实际消费账号（缺失为空）
	Status int    // HTTP 状态码（0 = 未走到写响应，异常出口）
	Ms     int64  // 端到端耗时 ms
	TTFB   int64  // 首字节耗时 ms（流式有意义；同步为 0）
	Mode   string // "stream" | "sync" | "work-stream" | "work-sync"
	Err    string // 失败原因摘要（成功为空；超长会被截断）
}

// maxUsageErr 失败原因落盘截断长度（含 \n 压平），避免超长错误撑爆 jsonl。
const maxUsageErr = 200

// usageEntry 单条用量记录（一行 JSON，字段名精简以压缩体积）。
type usageEntry struct {
	T      int64  `json:"t"`                // unix 秒（请求完成时刻）
	Model  string `json:"model"`            // 模型名（客户端原始传入值）
	Probe  bool   `json:"probe"`            // 是否探测请求（X-Probe: 1/true/yes）
	In     int    `json:"in"`               // 输入 token 数（缺失为 0）
	Out    int    `json:"out"`              // 输出 token 数（缺失为 0）
	UID    string `json:"uid,omitempty"`    // 消费账号 uid（旧记录无此字段 → 空）
	Status int    `json:"status,omitempty"` // HTTP 状态码（旧记录无 → 0，展示为 -）
	Ms     int64  `json:"ms,omitempty"`     // 端到端耗时 ms
	TTFB   int64  `json:"ttfb,omitempty"`   // 首字节 ms
	Mode   string `json:"mode,omitempty"`   // stream / sync / work-*（旧记录无 → 空）
	Err    string `json:"err,omitempty"`    // 失败摘要
}

// OK 判定该次请求成功（旧记录 status=0 时按成功处理：历史上只有成功才落盘）。
func (e usageEntry) OK() bool { return e.Status == 0 || (e.Status >= 200 && e.Status < 300) }

// recordUsage 追加一条用量记录到 <dir>/usage-<北京日期>.jsonl。
// best-effort：目录不存在则创建；任何错误只 log，不返回错误、不 panic。
func recordUsage(rec usageRecord) {
	dir := rec.Dir
	if dir == "" {
		return
	}
	if rec.In < 0 {
		rec.In = 0
	}
	if rec.Out < 0 {
		rec.Out = 0
	}
	if rec.Model == "" {
		rec.Model = "-"
	}
	if len(rec.Err) > maxUsageErr {
		rec.Err = rec.Err[:maxUsageErr]
	}
	rec.Err = strings.Map(func(r rune) rune {
		if r == '\n' || r == '\r' || r == '\t' {
			return ' '
		}
		return r
	}, rec.Err)

	e := usageEntry{
		T:      time.Now().Unix(),
		Model:  rec.Model,
		Probe:  rec.Probe,
		In:     rec.In,
		Out:    rec.Out,
		UID:    rec.UID,
		Status: rec.Status,
		Ms:     rec.Ms,
		TTFB:   rec.TTFB,
		Mode:   rec.Mode,
		Err:    rec.Err,
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

// usageFilePath 返回指定北京日期对应的 usage 文件路径。
func usageFilePath(dir, date string) string {
	return filepath.Join(dir, "usage-"+date+".jsonl")
}

// usageScan 读取某个日期的 usage jsonl，返回倒序（新→旧）的最多 limit 条明细 +
// 全量聚合统计。
//
// 只读一次文件：聚合在扫描过程中累计，**明细则用一个长度为 limit 的环形缓冲**
// 保留最新的若干条 —— 避免为了取最新 200 条而在内存里存下全天所有记录。
// 文件不存在返回空结果而非错误 —— 当天还没流量属正常状态。
func usageScan(dir, date string, limit int) ([]usageEntry, usageAggregate, []string) {
	var agg usageAggregate
	var warn []string
	if dir == "" {
		warn = append(warn, "UsageDir 未配置，无法读取用量流水")
		return nil, agg, warn
	}
	if limit <= 0 {
		limit = 200
	}
	if limit > maxUsageLimit {
		limit = maxUsageLimit
	}
	path := usageFilePath(dir, date)
	f, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, agg, nil // 当天无流量：正常
		}
		warn = append(warn, "读取 "+filepath.Base(path)+" 失败："+err.Error())
		return nil, agg, warn
	}
	defer f.Close()

	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64<<10), 1<<20) // 单行上限 1MB
	ring := make([]usageEntry, 0, limit)      // 环形：满了就丢最旧
	n := 0
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		var e usageEntry
		if err := json.Unmarshal([]byte(line), &e); err != nil {
			continue // 坏行跳过，不影响面板
		}
		agg.add(e)
		if len(ring) < limit {
			ring = append(ring, e)
		} else {
			ring[n%limit] = e // 覆盖最旧的一条
		}
		n++
	}
	if err := sc.Err(); err != nil {
		warn = append(warn, "扫描中断："+err.Error())
	}

	// ring 内的实际时序：n<=limit 时是自然顺序；超过后是环形，从 n%limit 起才是最新
	out := make([]usageEntry, 0, len(ring))
	if n > limit {
		start := n % limit
		out = append(out, ring[start:]...)
		out = append(out, ring[:start]...)
	} else {
		out = append(out, ring...)
	}
	// 面板要新→旧
	for i, j := 0, len(out)-1; i < j; i, j = i+1, j-1 {
		out[i], out[j] = out[j], out[i]
	}
	agg.finish()
	return out, agg, warn
}

// usageAggregate 某天 usage jsonl 的全量聚合（用于流水页头部卡片）。
type usageAggregate struct {
	Requests int            `json:"requests"`
	Probe    int            `json:"probe"`
	Real     int            `json:"real"`
	Errors   int            `json:"errors"`
	In       int64          `json:"in"`
	Out      int64          `json:"out"`
	ByModel  []usageByModel `json:"by_model"`
	ByHour   []int          `json:"by_hour"` // 24 格，下标 = 北京小时

	byModel map[string]*usageByModel
	hasHour bool
}

type usageByModel struct {
	Model    string `json:"model"`
	Requests int    `json:"requests"`
	In       int64  `json:"in"`
	Out      int64  `json:"out"`
	Errors   int    `json:"errors"`
}

func (a *usageAggregate) add(e usageEntry) {
	a.Requests++
	if e.Probe {
		a.Probe++
	} else {
		a.Real++
	}
	a.In += int64(e.In)
	a.Out += int64(e.Out)
	if !e.OK() {
		a.Errors++
	}
	if !a.hasHour {
		a.ByHour = make([]int, 24)
		a.hasHour = true
	}
	h := time.Unix(e.T, 0).In(beijing).Hour()
	if h >= 0 && h < 24 {
		a.ByHour[h]++
	}

	// 模型维度：在 add 阶段同步累计，避免再扫一遍明细
	if a.byModel == nil {
		a.byModel = make(map[string]*usageByModel, 16)
	}
	b, ok := a.byModel[e.Model]
	if !ok {
		b = &usageByModel{Model: e.Model}
		a.byModel[e.Model] = b
	}
	b.Requests++
	b.In += int64(e.In)
	b.Out += int64(e.Out)
	if !e.OK() {
		b.Errors++
	}
}

// finish 把内部 map 累计固化成有序切片，并按请求数降序（并列按模型名）。
// 必须在全部 add 之后、序列化之前调用一次。
func (a *usageAggregate) finish() {
	if len(a.byModel) == 0 {
		a.ByModel = []usageByModel{}
		return
	}
	out := make([]usageByModel, 0, len(a.byModel))
	for _, b := range a.byModel {
		out = append(out, *b)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Requests != out[j].Requests {
			return out[i].Requests > out[j].Requests
		}
		return out[i].Model < out[j].Model
	})
	a.ByModel = out
}
