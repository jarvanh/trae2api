package server

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// writeUsageFile 构造一个 usage jsonl 测试文件并返回其目录。
func writeUsageFile(t *testing.T, date string, lines []string) string {
	t.Helper()
	dir := t.TempDir()
	path := usageFilePath(dir, date)
	if err := os.WriteFile(path, []byte(strings.Join(lines, "\n")+"\n"), 0o644); err != nil {
		t.Fatalf("写入测试文件失败：%v", err)
	}
	return dir
}

// entry 构造一行 usage 记录（旧格式每行只有 5 个字段的场景另行手写）。
func entry(t int64, model string, probe bool, in, out int) string {
	return usageLine(t, model, probe, in, out, 200, 0, 0, "stream", "")
}

func usageLine(t int64, model string, probe bool, in, out, status int, ms, ttfb int64, mode, errMsg string) string {
	e := usageEntry{T: t, Model: model, Probe: probe, In: in, Out: out,
		Status: status, Ms: ms, TTFB: ttfb, Mode: mode, Err: errMsg}
	raw, mErr := json.Marshal(e)
	if mErr != nil {
		panic(mErr) // 测试构造用，不可能失败
	}
	return string(raw)
}

// TestUsageScanNewestFirst 明细必须新→旧：面板第一眼要看到最新请求。
func TestUsageScanNewestFirst(t *testing.T) {
	dir := writeUsageFile(t, "2026-09-30", []string{
		entry(1000, "m1", false, 10, 20),
		entry(2000, "m2", false, 10, 20),
		entry(3000, "m3", false, 10, 20),
	})
	got, agg, warn := usageScan(dir, "2026-09-30", 10)
	if len(warn) != 0 {
		t.Fatalf("不应有告警：%v", warn)
	}
	if len(got) != 3 {
		t.Fatalf("明细条数 = %d, 期望 3", len(got))
	}
	want := []int64{3000, 2000, 1000}
	for i, ts := range want {
		if got[i].T != ts {
			t.Errorf("第 %d 条 T = %d, 期望 %d（必须新→旧）", i, got[i].T, ts)
		}
	}
	if agg.Requests != 3 {
		t.Errorf("聚合请求数 = %d, 期望 3", agg.Requests)
	}
}

// TestUsageScanRingOverwrite limit 小于总行数时，环形缓冲必须保留**最新**的 limit 条，
// 且时序仍然正确 —— 这是最容易写错的一处（覆盖后起点是 n%limit 而非 0）。
func TestUsageScanRingOverwrite(t *testing.T) {
	var lines []string
	// 10 条，时间戳 1000..10000
	for i := 1; i <= 10; i++ {
		lines = append(lines, entry(int64(i*1000), "m", false, 1, 1))
	}
	dir := writeUsageFile(t, "2026-09-30", lines)

	got, agg, _ := usageScan(dir, "2026-09-30", 4)
	if len(got) != 4 {
		t.Fatalf("明细条数 = %d, 期望 4", len(got))
	}
	// 最新的 4 条应为 10000, 9000, 8000, 7000
	want := []int64{10000, 9000, 8000, 7000}
	for i, ts := range want {
		if got[i].T != ts {
			t.Errorf("第 %d 条 T = %d, 期望 %d", i, got[i].T, ts)
		}
	}
	// 聚合必须仍是全量 10 条（过滤明细不影响统计口径）
	if agg.Requests != 10 {
		t.Errorf("聚合请求数 = %d, 期望 10（聚合应全量）", agg.Requests)
	}
}

// TestUsageScanProbeFailCounter 保障两个关键口径：
//   - 探测/真实分流计数正确
//   - 失败请求计入 agg.errors（旧代码只在成功路径落盘，失败全丢）
func TestUsageScanProbeFailCounter(t *testing.T) {
	dir := writeUsageFile(t, "2026-09-30", []string{
		entry(1000, "m", true, 1, 1),                                        // 探测成功
		entry(2000, "m", false, 1, 1),                                       // 真实成功
		usageLine(3000, "m", false, 0, 0, 502, 1200, 0, "stream", "upstream"), // 真实失败
		usageLine(4000, "m", false, 0, 0, 503, 900, 0, "sync", "no acct"),     // 真实失败
	})
	_, agg, _ := usageScan(dir, "2026-09-30", 10)
	if agg.Requests != 4 {
		t.Errorf("总请求 = %d, 期望 4", agg.Requests)
	}
	if agg.Probe != 1 || agg.Real != 3 {
		t.Errorf("探测/真实 = %d/%d, 期望 1/3", agg.Probe, agg.Real)
	}
	if agg.Errors != 2 {
		t.Errorf("失败数 = %d, 期望 2", agg.Errors)
	}
	if agg.In != 2 || agg.Out != 2 {
		t.Errorf("token 合计 = %d/%d, 期望 2/2", agg.In, agg.Out)
	}
}

// TestUsageScanLegacyLines 向后兼容：历史上只有 5 个字段的行必须仍能解析，
// 且 status=0 视为成功（旧版只在成功时落盘）。
func TestUsageScanLegacyLines(t *testing.T) {
	dir := writeUsageFile(t, "2026-09-30", []string{
		`{"t":1000,"model":"old-model","probe":false,"in":5,"out":6}`,
	})
	got, agg, _ := usageScan(dir, "2026-09-30", 10)
	if len(got) != 1 {
		t.Fatalf("明细条数 = %d, 期望 1", len(got))
	}
	if got[0].Model != "old-model" || got[0].In != 5 || got[0].Out != 6 {
		t.Errorf("旧记录解析错误：%+v", got[0])
	}
	if !got[0].OK() {
		t.Errorf("status=0 的旧记录应视为成功")
	}
	if agg.Errors != 0 {
		t.Errorf("旧记录不应计入失败")
	}
}

// TestUsageScanBadLines 坏行（截断 JSON）跳过但不影响其余记录 —— best-effort 原则。
func TestUsageScanBadLines(t *testing.T) {
	dir := writeUsageFile(t, "2026-09-30", []string{
		`{"t":1000,"model":"bad","probe":fal`, // 截断
		entry(2000, "good", false, 1, 1),
	})
	got, agg, _ := usageScan(dir, "2026-09-30", 10)
	if len(got) != 1 {
		t.Fatalf("坏行应跳过，明细条数 = %d, 期望 1", len(got))
	}
	if got[0].Model != "good" {
		t.Errorf("剩余记录错误：%s", got[0].Model)
	}
	if agg.Requests != 1 {
		t.Errorf("聚合应只计有效行 = %d, 期望 1", agg.Requests)
	}
}

// TestUsageScanMissingFile 当天无流量：返回空结果而非错误（属正常状态）。
func TestUsageScanMissingFile(t *testing.T) {
	dir := t.TempDir()
	got, agg, warn := usageScan(dir, "2026-09-30", 10)
	if len(got) != 0 || agg.Requests != 0 {
		t.Errorf("无文件应返回空结果，got=%d agg=%d", len(got), agg.Requests)
	}
	if len(warn) != 0 {
		t.Errorf("文件缺失不算异常，不应告警：%v", warn)
	}
}

// TestUsageScanNoDir UsageDir 未配置时给出明确告警，而不是静默返回空白。
func TestUsageScanNoDir(t *testing.T) {
	_, _, warn := usageScan("", "2026-09-30", 10)
	if len(warn) == 0 {
		t.Errorf("UsageDir 为空应给出告警")
	}
}

// TestUsageScanByModel 按模型维度聚合，且按请求数降序。
func TestUsageScanByModel(t *testing.T) {
	dir := writeUsageFile(t, "2026-09-30", []string{
		entry(1000, "rare", false, 1, 1),
		entry(2000, "hot", false, 1, 1),
		entry(3000, "hot", false, 1, 1),
	})
	_, agg, _ := usageScan(dir, "2026-09-30", 10)
	if len(agg.ByModel) != 2 {
		t.Fatalf("模型数 = %d, 期望 2", len(agg.ByModel))
	}
	if agg.ByModel[0].Model != "hot" || agg.ByModel[0].Requests != 2 {
		t.Errorf("应按请求数降序，got %+v", agg.ByModel)
	}
	if agg.ByModel[1].Model != "rare" {
		t.Errorf("第二位应是 rare，got %+v", agg.ByModel[1])
	}
}

// TestUsageScanHourBucket 24 小时桶：给定时间戳要落在正确的北京小时格。
// 1500 = 1970-01-01 00:25 UTC = 08:25 北京 → 下标 8。
func TestUsageScanHourBucket(t *testing.T) {
	dir := writeUsageFile(t, "2026-09-30", []string{entry(1500, "m", false, 1, 1)})
	_, agg, _ := usageScan(dir, "2026-09-30", 10)
	if len(agg.ByHour) != 24 {
		t.Fatalf("小时桶长度 = %d, 期望 24", len(agg.ByHour))
	}
	for h, v := range agg.ByHour {
		if h == 8 && v != 1 {
			t.Errorf("北京 8 点桶应为 1，got %d", v)
		}
		if h != 8 && v != 0 {
			t.Errorf("第 %d 小时桶应为 0，got %d", h, v)
		}
	}
}

// TestUsageScanLimitCap limit 超上限要被夹住，防止一次把内存打爆。
func TestUsageScanLimitCap(t *testing.T) {
	var lines []string
	for i := 1; i <= 10; i++ {
		lines = append(lines, entry(int64(i*1000), "m", false, 1, 1))
	}
	dir := writeUsageFile(t, "2026-09-30", lines)
	got, _, _ := usageScan(dir, "2026-09-30", 999999)
	if len(got) > maxUsageLimit {
		t.Errorf("limit 未被夹住：%d > %d", len(got), maxUsageLimit)
	}
	if len(got) != 10 {
		t.Errorf("全部 10 条都应返回，got %d", len(got))
	}
}

// TestUsageScanPathTraversal 防御：date 参数会被拼进文件名，畸形值必须被挡住。
func TestUsageScanPathTraversal(t *testing.T) {
	dir := t.TempDir()
	// 直接调 usageFilePath 看拼出来的路径是否仍被限制在目标目录内
	p := usageFilePath(dir, "../../etc/passwd")
	if strings.Contains(p, "..") && filepath.IsAbs(p) && !strings.HasPrefix(filepath.Clean(p), filepath.Clean(dir)) {
		t.Errorf("路径穿越防护失效：%s", p)
	}
}

/* ─── 保留期归档 ─── */

// mkDay 在 dir 下造一个指定日期的日流水文件，返回该日期字符串。
func mkDay(t *testing.T, dir string, d time.Time, n int) string {
	t.Helper()
	date := d.In(beijing).Format("2006-01-02")
	var lines []string
	for i := 0; i < n; i++ {
		lines = append(lines, entry(d.Unix()+int64(i), "glm-5.3", false, 10, 20))
	}
	if err := os.WriteFile(usageFilePath(dir, date), []byte(strings.Join(lines, "\n")+"\n"), 0o644); err != nil {
		t.Fatalf("写 %s 失败：%v", date, err)
	}
	return date
}

// TestArchiveDisabled retentionDays<=0 必须什么都不做（维持永久保留的现状）。
func TestArchiveDisabled(t *testing.T) {
	dir := t.TempDir()
	now := time.Now().In(beijing)
	mkDay(t, dir, now.AddDate(0, 0, -200), 3)
	archived, removed, err := usageArchiveOlderThan(dir, 0, now)
	if err != nil || archived != 0 || removed != 0 {
		t.Errorf("retention=0 应不启用：archived=%d removed=%d err=%v", archived, removed, err)
	}
	if files := usageListDayFiles(dir); len(files) != 1 {
		t.Errorf("retention=0 不应删任何文件，剩 %d", len(files))
	}
}

// TestArchiveArchivesOldDays 超保留期的日明细应被归档进月汇总并删除。
func TestArchiveArchivesOldDays(t *testing.T) {
	dir := t.TempDir()
	now := time.Now().In(beijing)
	old := mkDay(t, dir, now.AddDate(0, 0, -120), 4)
	recent := mkDay(t, dir, now.AddDate(0, 0, -10), 2)

	archived, removed, err := usageArchiveOlderThan(dir, 90, now)
	if err != nil {
		t.Fatalf("归档失败：%v", err)
	}
	if archived != 1 || removed != 1 {
		t.Fatalf("应归档 1 天删 1 个文件，got archived=%d removed=%d", archived, removed)
	}
	// 近期文件必须还在
	if _, err := os.Stat(usageFilePath(dir, recent)); err != nil {
		t.Errorf("未超期的 %s 不应被删", recent)
	}
	// 超期的明细文件应已消失
	if _, err := os.Stat(usageFilePath(dir, old)); !os.IsNotExist(err) {
		t.Errorf("超期的 %s 应已被删", old)
	}
	// 月汇总应存在且统计正确
	m, err := usageLoadMonthly(dir, old[:7])
	if err != nil {
		t.Fatalf("读月汇总失败：%v", err)
	}
	if m.Requests != 4 {
		t.Errorf("月汇总请求数 = %d, 期望 4", m.Requests)
	}
	if m.In != 40 || m.Out != 80 {
		t.Errorf("月汇总 token = %d/%d, 期望 40/80", m.In, m.Out)
	}
	if len(m.ByDay) != 1 || m.ByDay[0].Date != old {
		t.Errorf("月汇总按天记录错误：%+v", m.ByDay)
	}
}

// TestArchiveIdempotent 幂等：重复执行不能把统计翻倍。
// 这是归档最容易踩的坑 —— 同一天被算两次，长期趋势就全错了。
func TestArchiveIdempotent(t *testing.T) {
	dir := t.TempDir()
	now := time.Now().In(beijing)
	mkDay(t, dir, now.AddDate(0, 0, -120), 5)

	if _, _, err := usageArchiveOlderThan(dir, 90, now); err != nil {
		t.Fatalf("首次归档失败：%v", err)
	}
	// 第二次：文件已删，应无新增
	a2, r2, err := usageArchiveOlderThan(dir, 90, now)
	if err != nil {
		t.Fatalf("二次归档失败：%v", err)
	}
	if a2 != 0 || r2 != 0 {
		t.Errorf("二次归档应无操作，got archived=%d removed=%d", a2, r2)
	}
	m, _ := usageLoadMonthly(dir, now.AddDate(0, 0, -120).In(beijing).Format("2006-01-02")[:7])
	if m.Requests != 5 {
		t.Errorf("重复执行后请求数 = %d, 期望 5（不能翻倍）", m.Requests)
	}
}

// TestArchiveNeverTouchesToday 今天的文件绝不归档 —— 归档当天正在写入的数据会丢账单。
func TestArchiveNeverTouchesToday(t *testing.T) {
	dir := t.TempDir()
	now := time.Now().In(beijing)
	today := mkDay(t, dir, now, 6)

	a, r, err := usageArchiveOlderThan(dir, 0, now) // retention=0 意为「全部超期」
	if err != nil {
		t.Fatalf("归档失败：%v", err)
	}
	// retention<=0 直接不启用；换用小 retention 验证「今天」保护
	a, r, err = usageArchiveOlderThan(dir, 1, now)
	if err != nil {
		t.Fatalf("归档失败：%v", err)
	}
	if a != 0 || r != 0 {
		t.Errorf("今天的数据不应被归档：archived=%d removed=%d", a, r)
	}
	if _, err := os.Stat(usageFilePath(dir, today)); err != nil {
		t.Errorf("今天的 %s 必须保留", today)
	}
}

// TestArchiveMergesMultipleDays 同一个月的多天应合并进一份月汇总。
func TestArchiveMergesMultipleDays(t *testing.T) {
	dir := t.TempDir()
	now := time.Now().In(beijing)
	// 取 180 天前所在月份的 1/2/3 号：既确保**都超期**（>90 天），
	// 又确保**同一个月**（对齐到月初，避免连加天数跨月把用例带偏）。
	old := now.AddDate(0, 0, -180)
	base := time.Date(old.Year(), old.Month(), 1, 12, 0, 0, 0, beijing)
	var dates []string
	for i := 0; i < 3; i++ {
		dates = append(dates, mkDay(t, dir, base.AddDate(0, 0, i), 2))
	}
	archived, removed, err := usageArchiveOlderThan(dir, 90, now)
	if err != nil {
		t.Fatalf("归档失败：%v", err)
	}
	if archived != 3 || removed != 3 {
		t.Fatalf("应归档 3 天，got archived=%d removed=%d", archived, removed)
	}
	month := dates[0][:7]
	m, err := usageLoadMonthly(dir, month)
	if err != nil {
		t.Fatalf("读月汇总失败：%v", err)
	}
	if m.Days != 3 || m.Requests != 6 {
		t.Errorf("月汇总应合并 3 天 6 请求，got days=%d requests=%d", m.Days, m.Requests)
	}
}

// TestArchiveIgnoresJunkFiles 目录里无关文件绝不能被归档逻辑误删。
func TestArchiveIgnoresJunkFiles(t *testing.T) {
	dir := t.TempDir()
	now := time.Now().In(beijing)
	mkDay(t, dir, now.AddDate(0, 0, -120), 2)
	junk := []string{"state.json", "admin-order.json", "usage-notadate.jsonl"}
	for _, name := range junk {
		if err := os.WriteFile(filepath.Join(dir, name), []byte("{}"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if _, _, err := usageArchiveOlderThan(dir, 90, now); err != nil {
		t.Fatalf("归档失败：%v", err)
	}
	for _, name := range junk {
		if _, err := os.Stat(filepath.Join(dir, name)); err != nil {
			t.Errorf("无关文件 %s 不应被删", name)
		}
	}
}

// TestArchiveInvalidDir 空目录/空 dir：安全返回，不 panic。
func TestArchiveInvalidDir(t *testing.T) {
	a, r, err := usageArchiveOlderThan("", 90, time.Now())
	if err != nil || a != 0 || r != 0 {
		t.Errorf("空 dir 应安全返回，got %d/%d/%v", a, r, err)
	}
	// 目录存在但为空
	a, r, err = usageArchiveOlderThan(t.TempDir(), 90, time.Now())
	if err != nil || a != 0 || r != 0 {
		t.Errorf("空目录应无操作，got %d/%d/%v", a, r, err)
	}
}

// ─── 区间扫描（7 天 / 30 天）────────────────────────────────────────────────

// writeUsageDays 在同一目录下写多天流水文件（区间扫描要求多文件共存）。
func writeUsageDays(t *testing.T, days map[string][]string) string {
	t.Helper()
	dir := t.TempDir()
	for date, lines := range days {
		path := usageFilePath(dir, date)
		if err := os.WriteFile(path, []byte(strings.Join(lines, "\n")+"\n"), 0o644); err != nil {
			t.Fatalf("写入 %s 失败：%v", date, err)
		}
	}
	return dir
}

// TestUsageScanRangeAggregate 区间聚合 = 各天之和（这是 7 天/30 天卡片的数据源）。
func TestUsageScanRangeAggregate(t *testing.T) {
	dir := writeUsageDays(t, map[string][]string{
		"2026-09-28": {entry(1000, "m1", false, 10, 20), entry(1100, "m1", true, 1, 1)},
		"2026-09-29": {entry(2000, "m2", false, 30, 40)},
		"2026-09-30": {entry(3000, "m1", false, 5, 5), entry(3100, "m3", false, 5, 5)},
	})
	res := usageScanRange(dir, "2026-09-28", "2026-09-30", 100)
	if len(res.Warn) != 0 {
		t.Fatalf("不应有告警：%v", res.Warn)
	}
	if res.Agg.Requests != 5 {
		t.Errorf("区间请求数 = %d, 期望 5", res.Agg.Requests)
	}
	if res.Agg.Probe != 1 || res.Agg.Real != 4 {
		t.Errorf("探测/真实 = %d/%d, 期望 1/4", res.Agg.Probe, res.Agg.Real)
	}
	// 10+1+30+5+5 = 51
	if res.Agg.In != 51 {
		t.Errorf("区间输入 token = %d, 期望 51", res.Agg.In)
	}
	if len(res.Days) != 3 {
		t.Errorf("有流水的天数 = %d, 期望 3", len(res.Days))
	}
}

// TestUsageScanRangeDaily 按天摘要必须与区间同步产出，且无流量的天补 0（否则柱状图错位）。
func TestUsageScanRangeDaily(t *testing.T) {
	dir := writeUsageDays(t, map[string][]string{
		"2026-09-28": {entry(1000, "m1", false, 1, 1), entry(1050, "m1", false, 1, 1)},
		// 09-29 故意不写文件：模拟当天无流量
		"2026-09-30": {entry(3000, "m1", false, 1, 1)},
	})
	res := usageScanRange(dir, "2026-09-28", "2026-09-30", 100)
	if len(res.Daily) != 3 {
		t.Fatalf("daily 长度 = %d, 期望 3（无流量的天也要占位）", len(res.Daily))
	}
	want := map[string]int{"2026-09-28": 2, "2026-09-29": 0, "2026-09-30": 1}
	for _, d := range res.Daily {
		if d.Requests != want[d.Date] {
			t.Errorf("%s 请求数 = %d, 期望 %d", d.Date, d.Requests, want[d.Date])
		}
	}
	// 顺序必须升序 —— 面板柱状图按数组下标画，乱序会让趋势看着像随机跳动
	if res.Daily[0].Date != "2026-09-28" || res.Daily[2].Date != "2026-09-30" {
		t.Errorf("daily 必须按日期升序，got %v", []string{res.Daily[0].Date, res.Daily[1].Date, res.Daily[2].Date})
	}
	if len(res.Missing) != 1 || res.Missing[0] != "2026-09-29" {
		t.Errorf("缺失日期 = %v, 期望 [2026-09-29]", res.Missing)
	}
}

// TestUsageScanRangeNewestFirst 跨天区间：明细仍须新→旧，
// 且 limit 只保留全区间**最新**的若干条 —— 环形缓冲跨文件后起点最易写错。
func TestUsageScanRangeNewestFirst(t *testing.T) {
	dir := writeUsageDays(t, map[string][]string{
		"2026-09-28": {entry(1000, "m", false, 1, 1), entry(2000, "m", false, 1, 1)},
		"2026-09-29": {entry(3000, "m", false, 1, 1), entry(4000, "m", false, 1, 1)},
		"2026-09-30": {entry(5000, "m", false, 1, 1), entry(6000, "m", false, 1, 1)},
	})
	res := usageScanRange(dir, "2026-09-28", "2026-09-30", 3)
	want := []int64{6000, 5000, 4000}
	if len(res.Entries) != len(want) {
		t.Fatalf("明细条数 = %d, 期望 %d", len(res.Entries), len(want))
	}
	for i, ts := range want {
		if res.Entries[i].T != ts {
			t.Errorf("第 %d 条 T = %d, 期望 %d", i, res.Entries[i].T, ts)
		}
	}
	// 聚合不受 limit 影响，仍是全量 6 条
	if res.Agg.Requests != 6 {
		t.Errorf("聚合请求数 = %d, 期望 6（明细 limit 不应影响聚合）", res.Agg.Requests)
	}
}

// TestUsageScanRangeSwapDates from > to 时自动交换，不返回空结果。
func TestUsageScanRangeSwapDates(t *testing.T) {
	dir := writeUsageDays(t, map[string][]string{
		"2026-09-28": {entry(1000, "m", false, 1, 1)},
		"2026-09-30": {entry(3000, "m", false, 1, 1)},
	})
	res := usageScanRange(dir, "2026-09-30", "2026-09-28", 100)
	if res.Agg.Requests != 2 {
		t.Errorf("请求数 = %d, 期望 2（from>to 应自动交换）", res.Agg.Requests)
	}
	if len(res.Daily) != 3 {
		t.Errorf("daily 长度 = %d, 期望 3", len(res.Daily))
	}
}

// TestUsageScanRangeTruncate 超长区间必须被截断并告警 —— 防误传超大值把磁盘扫爆。
func TestUsageScanRangeTruncate(t *testing.T) {
	dir := t.TempDir()
	res := usageScanRange(dir, "2020-01-01", "2026-09-30", 10)
	if len(res.Daily) > maxRangeDays {
		t.Errorf("扫描天数 = %d, 应被截断到 %d 以内", len(res.Daily), maxRangeDays)
	}
	if len(res.Warn) == 0 {
		t.Error("截断时应给出告警，否则用户不知道自己看到的是不完整区间")
	}
}

// TestUsageScanRangeInvalid 非法日期 / 空 dir 都要安全返回，不 panic。
func TestUsageScanRangeInvalid(t *testing.T) {
	if res := usageScanRange("", "2026-09-28", "2026-09-30", 10); len(res.Warn) == 0 {
		t.Error("空 dir 应给出告警")
	}
	res := usageScanRange(t.TempDir(), "not-a-date", "2026-09-30", 10)
	if len(res.Warn) == 0 {
		t.Error("非法日期应给出告警")
	}
	if len(res.Daily) != 0 || res.Agg.Requests != 0 {
		t.Error("非法输入应返回空结果")
	}
}

// TestUsageDateRange 日期列表生成：闭区间、升序、含首尾。
func TestUsageDateRange(t *testing.T) {
	got, err := usageDateRange("2026-09-28", "2026-09-30")
	if err != nil {
		t.Fatalf("不应报错：%v", err)
	}
	want := []string{"2026-09-28", "2026-09-29", "2026-09-30"}
	if len(got) != len(want) {
		t.Fatalf("长度 = %d, 期望 %d", len(got), len(want))
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("第 %d 个 = %s, 期望 %s", i, got[i], want[i])
		}
	}
	// 单日区间 = 只有自己
	one, _ := usageDateRange("2026-09-30", "2026-09-30")
	if len(one) != 1 || one[0] != "2026-09-30" {
		t.Errorf("单日区间 = %v, 期望 [2026-09-30]", one)
	}
	// 跨月边界（9/30 → 10/02）：进位不能出错
	cross, _ := usageDateRange("2026-09-30", "2026-10-02")
	if len(cross) != 3 || cross[1] != "2026-10-01" || cross[2] != "2026-10-02" {
		t.Errorf("跨月区间 = %v, 期望 [09-30 10-01 10-02]", cross)
	}
}

// TestUsageShiftDate 面板算区间起点用：负偏移取回溯日，跨月跨年都要对。
func TestUsageShiftDate(t *testing.T) {
	if got, _ := usageShiftDate("2026-09-30", -6); got != "2026-09-24" {
		t.Errorf("-6 天 = %s, 期望 2026-09-24（7 天区间起点）", got)
	}
	if got, _ := usageShiftDate("2026-09-30", -29); got != "2026-09-01" {
		t.Errorf("-29 天 = %s, 期望 2026-09-01（30 天区间起点）", got)
	}
	if got, _ := usageShiftDate("2026-01-05", -10); got != "2025-12-26" {
		t.Errorf("跨年 -10 天 = %s, 期望 2025-12-26", got)
	}
	if _, err := usageShiftDate("bad", -1); err == nil {
		t.Error("非法日期应返回错误")
	}
}
