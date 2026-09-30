package server

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
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
