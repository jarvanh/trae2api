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
	"fmt"
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

// maxRangeDays 区间扫描的天数上限（防误传超大区间把磁盘/内存扫爆）。
// 略大于面板最大档 30 天，留出余量；超过则截断到 from 侧并给出告警。
const maxRangeDays = 92

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
	if dir == "" {
		return nil, agg, []string{"UsageDir 未配置，无法读取用量流水"}
	}
	ring := newUsageRing(limit)
	_, warn := usageScanFile(dir, date, ring, &agg)
	agg.finish()
	return ring.entries(), agg, warn
}

// ─── 区间扫描（7 天 / 30 天）────────────────────────────────────────────────
//
// 单日 scan 只够看当天；面板要「近 7 天 / 近 30 天」趋势，就得跨文件汇总。
// 实现上复用同一套「环形缓冲 + 聚合累加」：按日期升序逐文件扫描，
// 聚合全程累计，明细始终只留最新 limit 条 —— 30 天十万级记录也不会撑爆内存。

// usageDayStat 区间内某天的摘要（供面板画按天柱状图）。
type usageDayStat struct {
	Date     string `json:"date"`
	Requests int    `json:"requests"`
	Probe    int    `json:"probe"`
	Real     int    `json:"real"`
	Errors   int    `json:"errors"`
	In       int64  `json:"in"`
	Out      int64  `json:"out"`
}

// usageRangeResult 区间扫描结果。
type usageRangeResult struct {
	Entries []usageEntry   // 区间内最新的 limit 条（新→旧）
	Agg     usageAggregate // 区间全量聚合（明细条数不影响）
	Daily   []usageDayStat // 按天摘要（升序，含无流量的天 → 全 0）
	Warn    []string
	Days    []string // 实际读到流水文件的日期（升序）
	Missing []string // 区间内无流水文件的日期（升序，可能是没流量或已归档删明细）
}

// usageScanRange 扫描 [from, to] 闭区间（北京日期 YYYY-MM-DD）内所有日粒度流水。
// from > to 时自动交换；天数超过 maxRangeDays 从 from 侧截断并告警。
func usageScanRange(dir, from, to string, limit int) usageRangeResult {
	var res usageRangeResult
	if dir == "" {
		res.Warn = []string{"UsageDir 未配置，无法读取用量流水"}
		return res
	}
	dates, err := usageDateRange(from, to)
	if err != nil {
		res.Warn = []string{"区间日期非法：" + err.Error()}
		return res
	}
	if len(dates) > maxRangeDays {
		res.Warn = append(res.Warn, fmt.Sprintf("区间 %d 天超过上限 %d 天，已截断为最近的 %d 天",
			len(dates), maxRangeDays, maxRangeDays))
		dates = dates[len(dates)-maxRangeDays:]
	}
	ring := newUsageRing(limit)
	var agg usageAggregate
	res.Daily = make([]usageDayStat, 0, len(dates))
	for _, d := range dates {
		var day usageAggregate // 当天小计（一次扫描同时喂给总量与当天）
		ok, w := usageScanFile(dir, d, ring, &agg, &day)
		res.Warn = append(res.Warn, w...)
		if ok {
			res.Days = append(res.Days, d)
		} else {
			res.Missing = append(res.Missing, d)
		}
		res.Daily = append(res.Daily, usageDayStat{
			Date: d, Requests: day.Requests, Probe: day.Probe, Real: day.Real,
			Errors: day.Errors, In: day.In, Out: day.Out,
		})
	}
	agg.finish()
	res.Agg = agg
	res.Entries = ring.entries()
	return res
}

// usageDateRange 生成 [from,to] 闭区间的北京日期列表（升序）。
// from > to 时交换；解析失败返回错误（日期来自 URL，必须先校验再拼文件名）。
func usageDateRange(from, to string) ([]string, error) {
	f, err := time.Parse("2006-01-02", from)
	if err != nil {
		return nil, fmt.Errorf("from %q 非法", from)
	}
	t, err := time.Parse("2006-01-02", to)
	if err != nil {
		return nil, fmt.Errorf("to %q 非法", to)
	}
	if f.After(t) {
		f, t = t, f
	}
	var out []string
	// UTC 零点按天递增，无 DST 干扰；日期只用作字符串与文件名
	for d := f; !d.After(t); d = d.AddDate(0, 0, 1) {
		out = append(out, d.Format("2006-01-02"))
	}
	return out, nil
}

// usageShiftDate 把 YYYY-MM-DD 平移 delta 天（面板算区间起点用）。
func usageShiftDate(date string, delta int) (string, error) {
	d, err := time.Parse("2006-01-02", date)
	if err != nil {
		return "", err
	}
	return d.AddDate(0, 0, delta).Format("2006-01-02"), nil
}

// usageScanFile 扫描单日流水文件：解析每行 → 累加进所有 aggs → 明细进 ring。
//
// aggs 是可变的：区间统计同时要「区间总量」和「当天小计」，传两个即可一次扫完，
// 不必为按天柱状图再读一遍文件。
// 文件不存在返回 (false, nil)：当天无流量是正常状态，不算错误。
func usageScanFile(dir, date string, ring *usageRing, aggs ...*usageAggregate) (bool, []string) {
	var warn []string
	path := usageFilePath(dir, date)
	f, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			return false, nil
		}
		return false, []string{"读取 " + filepath.Base(path) + " 失败：" + err.Error()}
	}
	defer f.Close()

	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64<<10), 1<<20) // 单行上限 1MB
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		var e usageEntry
		if err := json.Unmarshal([]byte(line), &e); err != nil {
			continue // 坏行跳过，不影响面板
		}
		for _, a := range aggs {
			if a != nil {
				a.add(e)
			}
		}
		ring.add(e)
	}
	if err := sc.Err(); err != nil {
		warn = append(warn, "扫描 "+filepath.Base(path)+" 中断："+err.Error())
	}
	return true, warn
}

// usageRing 保留最新 limit 条明细的环形缓冲（单日与跨日区间共用）。
//
// 必须按**时间升序** add：日期升序 + 文件内追加顺序天然满足。
// 满了就覆盖最旧一条，避免为取最新 N 条而在内存里存下全部记录。
type usageRing struct {
	buf   []usageEntry
	limit int
	n     int // 累计 add 次数
}

func newUsageRing(limit int) *usageRing {
	if limit <= 0 {
		limit = 200
	}
	if limit > maxUsageLimit {
		limit = maxUsageLimit
	}
	return &usageRing{buf: make([]usageEntry, 0, limit), limit: limit}
}

func (r *usageRing) add(e usageEntry) {
	if len(r.buf) < r.limit {
		r.buf = append(r.buf, e)
	} else {
		r.buf[r.n%r.limit] = e // 覆盖最旧的一条
	}
	r.n++
}

// entries 返回新→旧顺序的明细。
// 未填满时 buf 即自然升序；填满后从 n%limit 起才是最新的那条。
func (r *usageRing) entries() []usageEntry {
	out := make([]usageEntry, 0, len(r.buf))
	if r.n > r.limit {
		start := r.n % r.limit
		out = append(out, r.buf[start:]...)
		out = append(out, r.buf[:start]...)
	} else {
		out = append(out, r.buf...)
	}
	// 面板要新→旧
	for i, j := 0, len(out)-1; i < j; i, j = i+1, j-1 {
		out[i], out[j] = out[j], out[i]
	}
	return out
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

// ArchiveUsage 导出入口：供 main 装配到 scheduler 的每日归档钩子。
// 内部转调 usageArchiveOlderThan，以「当前时间」为保留期基准。
// 安全纪律（顺序不可颠倒）：以上归档逻辑**先写汇总、确认成功后再删明细**。

// ─── 保留期归档 ────────────────────────────────────────────────────────────
//
// 日粒度明细按保留期（默认 90 天）滚动：超期的日子归档成**月粒度汇总**后删除明细。
// 汇总保留统计（请求数/成败/token/按模型/按天），丢掉逐条明细 —— 长期趋势不丢，
// 体积不再无限增长。retentionDays <= 0 时不启用（维持永久保留的现状）。

// ArchiveUsage 导出入口：供 main 装配到 scheduler 的每日归档钩子。
// 内部转调 usageArchiveOlderThan，以「当前时间」为保留期基准。
func ArchiveUsage(dir string, retentionDays int) (int, int, error) {
	return usageArchiveOlderThan(dir, retentionDays, time.Now())
}

// usageMonthlyPath 月粒度归档文件路径。
func usageMonthlyPath(dir, month string) string {
	return filepath.Join(dir, "usage-monthly-"+month+".json")
}

// usageByDay 归档里的单日摘要（只留统计，明细已丢）。
type usageByDay struct {
	Date     string `json:"date"`
	Requests int    `json:"requests"`
	Probe    int    `json:"probe"`
	Errors   int    `json:"errors"`
	In       int64  `json:"in"`
	Out      int64  `json:"out"`
}

// usageMonthly 月粒度归档。
type usageMonthly struct {
	Month     string         `json:"month"` // "2026-06"
	Days      int            `json:"days"`  // 已归档天数
	Requests  int            `json:"requests"`
	Probe     int            `json:"probe"`
	Real      int            `json:"real"`
	Errors    int            `json:"errors"`
	In        int64          `json:"in"`
	Out       int64          `json:"out"`
	ByModel   []usageByModel `json:"by_model"`
	ByDay     []usageByDay   `json:"by_day"`
	UpdatedAt int64          `json:"updated_at"`
}

// hasDay 该日期是否已归档（幂等：同一天绝不重复累计）。
func (m *usageMonthly) hasDay(date string) bool {
	for _, d := range m.ByDay {
		if d.Date == date {
			return true
		}
	}
	return false
}

// addDay 把某天的聚合并入月汇总。
func (m *usageMonthly) addDay(date string, agg usageAggregate) {
	m.Days++
	m.Requests += agg.Requests
	m.Probe += agg.Probe
	m.Real += agg.Real
	m.Errors += agg.Errors
	m.In += agg.In
	m.Out += agg.Out
	m.ByDay = append(m.ByDay, usageByDay{
		Date: date, Requests: agg.Requests, Probe: agg.Probe,
		Errors: agg.Errors, In: agg.In, Out: agg.Out,
	})
	m.mergeModels(agg.ByModel)
}

// mergeModels 按模型名累加（同名合并，新增追加）。
func (m *usageMonthly) mergeModels(list []usageByModel) {
	idx := make(map[string]int, len(m.ByModel))
	for i, b := range m.ByModel {
		idx[b.Model] = i
	}
	for _, b := range list {
		if i, ok := idx[b.Model]; ok {
			m.ByModel[i].Requests += b.Requests
			m.ByModel[i].In += b.In
			m.ByModel[i].Out += b.Out
			m.ByModel[i].Errors += b.Errors
		} else {
			m.ByModel = append(m.ByModel, b)
			idx[b.Model] = len(m.ByModel) - 1
		}
	}
}

// usageListDayFiles 列出目录内所有日粒度流水文件，返回 date→path。
// 文件名不符合 usage-YYYY-MM-DD.jsonl 的一律忽略（防误删无关文件）。
func usageListDayFiles(dir string) map[string]string {
	out := map[string]string{}
	if dir == "" {
		return out
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return out
	}
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		name := e.Name()
		if !strings.HasPrefix(name, "usage-") || !strings.HasSuffix(name, ".jsonl") {
			continue
		}
		date := strings.TrimSuffix(strings.TrimPrefix(name, "usage-"), ".jsonl")
		if _, err := time.Parse("2006-01-02", date); err != nil {
			continue // 名字不合法，不动它
		}
		out[date] = filepath.Join(dir, name)
	}
	return out
}

// usageArchiveOlderThan 归档超过保留期的日粒度流水。
//
// 安全纪律（顺序不可颠倒）：
//  1. 先读全部待归档日 → 聚合 → 写月汇总（原子写 tmp+rename）
//  2. **确认汇总写成功之后**才删除日明细文件
//     先删后写一旦中途失败，数据就真没了 —— 归档绝不能成为数据丢失的路径
//  3. 幂等：月汇总里已有的日期跳过，重复执行不会把统计翻倍
//
// 返回归档天数、删除文件数；retentionDays <= 0 时返回 0,0（不启用）。
func usageArchiveOlderThan(dir string, retentionDays int, now time.Time) (int, int, error) {
	if dir == "" || retentionDays <= 0 {
		return 0, 0, nil
	}
	today := now.In(beijing).Format("2006-01-02")
	dayFiles := usageListDayFiles(dir)

	// 按目标月份分组待归档日期
	byMonth := map[string][]string{}
	for date := range dayFiles {
		if date >= today {
			continue // 今天及以后永不归档
		}
		d, err := time.Parse("2006-01-02", date)
		if err != nil {
			continue
		}
		ageDays := int(now.In(beijing).Sub(d).Hours() / 24)
		if ageDays <= retentionDays {
			continue
		}
		month := date[:7]
		byMonth[month] = append(byMonth[month], date)
	}

	archived, removed := 0, 0
	var firstErr error
	for month, dates := range byMonth {
		sort.Strings(dates)
		m, err := usageLoadMonthly(dir, month)
		if err != nil {
			if firstErr == nil {
				firstErr = fmt.Errorf("读取月度归档 %s 失败：%w", month, err)
			}
			continue
		}
		var done []string
		for _, date := range dates {
			if m.hasDay(date) {
				continue
			}
			_, agg, _ := usageScan(dir, date, 1) // 只要聚合，明细不进内存
			m.addDay(date, agg)
			done = append(done, date)
		}
		if len(done) == 0 {
			continue
		}
		m.UpdatedAt = time.Now().Unix()
		if err := usageWriteMonthly(dir, m); err != nil {
			if firstErr == nil {
				firstErr = fmt.Errorf("写入月度归档 %s 失败：%w", month, err)
			}
			continue // 汇总没写成功 → 绝不删明细
		}
		// 汇总已落盘，才敢删明细
		for _, date := range done {
			if err := os.Remove(dayFiles[date]); err != nil {
				log.Printf("[usage] 归档后删除 %s 失败：%v", dayFiles[date], err)
				continue
			}
			removed++
		}
		archived += len(done)
	}
	return archived, removed, firstErr
}

// usageLoadMonthly 读取月归档；不存在时返回空结构（不是错误）。
func usageLoadMonthly(dir, month string) (*usageMonthly, error) {
	m := &usageMonthly{Month: month, ByModel: []usageByModel{}, ByDay: []usageByDay{}}
	raw, err := os.ReadFile(usageMonthlyPath(dir, month))
	if err != nil {
		if os.IsNotExist(err) {
			return m, nil
		}
		return nil, err
	}
	if err := json.Unmarshal(raw, m); err != nil {
		return nil, err
	}
	if m.ByModel == nil {
		m.ByModel = []usageByModel{}
	}
	if m.ByDay == nil {
		m.ByDay = []usageByDay{}
	}
	return m, nil
}

// usageWriteMonthly 原子写月归档（tmp + rename），避免半截文件。
func usageWriteMonthly(dir string, m *usageMonthly) error {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	raw, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return err
	}
	final := usageMonthlyPath(dir, m.Month)
	tmp := final + ".tmp"
	if err := os.WriteFile(tmp, raw, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, final)
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
