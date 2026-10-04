// 管理面板（/admin）：只读查询，无鉴权（本地面板）；CLI 操作留待开发。
package server

import (
	_ "embed"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"trae2api/internal/pool"
	"trae2api/internal/upstream"
)

//go:embed admin.html
var adminPageHTML []byte

// adminPage 返回内嵌 HTML 面板（深色简洁风，无外部依赖）。
func (h *Handler) adminPage(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	_, _ = w.Write(adminPageHTML)
}

// adminCredits 查询全部账号的实时额度 + 签到状态（并发拉取上游）。
func (h *Handler) adminCredits(w http.ResponseWriter, r *http.Request) {
	// packItem 单个权益包明细。展示口径与 TG 通知一致：
	// 只列「未用完」包里最早过期的 3 个（PackList 已按 expire_at 升序）；
	// 一个未用完包都没有时 PacksDetail 整体省略，NearestDays 置 -1。
	type packItem struct {
		Desc      string `json:"desc"`
		Limit     int64  `json:"limit"`
		Used      int64  `json:"used"`
		Remain    int64  `json:"remain"`
		ExpireAt  int64  `json:"expire_at"`
		ExpireFmt string `json:"expire_fmt"`
		DaysLeft  int    `json:"days_left"`
	}

	type acct struct {
		UID            string     `json:"uid"`
		Nickname       string     `json:"nickname"`
		Remain         int64      `json:"remain"`
		Limit          int64      `json:"limit"`
		Used           int64      `json:"used"`
		Packs          int        `json:"packs"`
		WorkCredits    float64    `json:"work_credits"`
		CheckedIn      bool       `json:"checked_in"`
		CheckinCredits int64      `json:"checkin_credits"`
		CheckinEnable  bool       `json:"checkin_enable"`
		Cooling        bool       `json:"cooling"`
		WorkCooling    bool       `json:"work_cooling"`
		Disabled       bool       `json:"disabled"`
		PacksDetail    []packItem `json:"packs_detail,omitempty"`
		NearestDays    int        `json:"nearest_days"`
		Error          string     `json:"error,omitempty"`
	}

	st := h.cfg.Pool.List()
	out := make([]acct, len(st))
	var wg sync.WaitGroup
	for i, s := range st {
		wg.Add(1)
		go func(i int, s pool.Status) {
			defer wg.Done()
			a := h.cfg.Pool.AuthByUID(s.UID)
			if a == nil {
				out[i] = acct{UID: s.UID, Nickname: s.Nickname, Error: "no auth found"}
				return
			}
			var ac acct
			ac.UID = s.UID
			ac.Nickname = s.Nickname
			ac.Cooling = s.Cooling
			ac.WorkCooling = s.WorkCooling
			ac.WorkCredits = s.WorkCredits
			ac.Disabled = s.Disabled
			// PackList 与 EntUsage 打的是同一个 ide_user_ent_usage 接口，
			// 只是多解析了 display_desc / start_time / expire_time —— 同样一次
			// 请求，既拿到聚合额度也拿到每包过期时间，无需额外调用。
			ac.NearestDays = -1
			packs, perr := h.cfg.Upstream.PackList(a)
			if perr != nil {
				ac.Error = "pack_list: " + perr.Error()
			} else {
				for _, p := range packs {
					ac.Limit += p.Limit
					ac.Used += p.Used
					ac.Remain += p.Limit - p.Used
					ac.Packs++
					if p.Limit-p.Used <= 0 {
						continue // 已用完，不进展示列表
					}
					days := int(time.Until(time.Unix(p.ExpireAt, 0)).Hours() / 24)
					if len(ac.PacksDetail) < 3 {
						ac.PacksDetail = append(ac.PacksDetail, packItem{
							Desc:      p.Desc,
							Limit:     p.Limit,
							Used:      p.Used,
							Remain:    p.Limit - p.Used,
							ExpireAt:  p.ExpireAt,
							ExpireFmt: time.Unix(p.ExpireAt, 0).In(beijing).Format("2006-01-02 15:04"),
							DaysLeft:  days,
						})
					}
					if ac.NearestDays < 0 {
						ac.NearestDays = days
					}
				}
			}
			// 面板展示用：设备号与签到路径同源（按当前代数派生）
			checkinDeviceID := upstream.CheckinDeviceID(upstream.CheckinIdentity(a), h.cfg.Pool.CheckinGeneration(s.UID))
			checkedIn, credits, enable, cerr := h.cfg.Upstream.CheckinStatus(a, checkinDeviceID)
			if cerr != nil {
				if ac.Error != "" {
					ac.Error += "; "
				}
				ac.Error += "checkin: " + cerr.Error()
			} else {
				ac.CheckedIn, ac.CheckinCredits, ac.CheckinEnable = checkedIn, credits, enable
			}
			if h.cfg.WorkClient != nil && h.cfg.WorkMode != upstream.WorkModeDisabled {
				if snap, werr := h.cfg.WorkClient.ProbeCredits(r.Context(), a); werr == nil {
					ac.WorkCredits = snap.WorkCredits
					h.cfg.Pool.SetWorkCredits(s.UID, snap.WorkCredits)
				}
			}
			out[i] = ac
		}(i, s)
	}
	wg.Wait()

	// 应用控制台拖动保存的账号顺序（未出现的按 UID 追加在后，新增账号不会丢）
	ids := make([]string, 0, len(out))
	byUID := make(map[string]acct, len(out))
	for _, a := range out {
		ids = append(ids, a.UID)
		byUID[a.UID] = a
	}
	ordered := make([]acct, 0, len(out))
	for _, uid := range reorderBy(ids, h.loadUIOrder().Accounts) {
		ordered = append(ordered, byUID[uid])
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"fetched_at": time.Now().Format("2006-01-02 15:04:05"),
		"accounts":   ordered,
	})
}

// adminModels 返回模型列表 + 上游消耗倍率（供控制台「模型」tab 展示）。
//
// 数据源与 /v1/models 同源：modelList() 内部走 fetchDynamicModels()，
// 命中 1h 缓存时**不产生任何上游请求**；上游失败则回退静态表（此时倍率为 0）。
// 倍率取自 get_detail_param 内嵌的 consumption_rate.data.rate（如 0.48 = 0.48x）。
func (h *Handler) adminModels(w http.ResponseWriter, r *http.Request) {
	type modelItem struct {
		ID              string  `json:"id"`
		ContextLength   int64   `json:"context_length"`
		ConsumptionRate float64 `json:"consumption_rate"`
	}
	// 默认排序：倍率升序（省的在最上），**倍率未知(0)恒定沉底**，并列按 ID 升序。
	// 未知必须沉底：上游有相当一部分模型不返回 consumption_rate，
	// 若按数值直接升序，这些 0 值会霸占列表顶部，把真正省的模型挤下去。
	sortItems := func(items []modelItem) {
		sort.Slice(items, func(i, j int) bool {
			ri, rj := items[i].ConsumptionRate, items[j].ConsumptionRate
			if ri <= 0 && rj > 0 {
				return false
			}
			if ri > 0 && rj <= 0 {
				return true
			}
			if ri != rj {
				return ri < rj
			}
			return items[i].ID < items[j].ID
		})
	}
	// 默认隐藏上游返回的内部/占位模型（custom_model_*、*_subagent/*_agent、summary），
	// 加 ?all=1 可看全量。上游没有标识字段，只能按命名模式识别。
	showAll := r.URL.Query().Get("all") == "1"
	hidden := 0
	dynamic := false
	order := h.loadUIOrder()

	emit := func(items []modelItem, dynamic bool) {
		if !showAll {
			kept := items[:0]
			for _, it := range items {
				if isInternalModel(it.ID) {
					hidden++
					continue
				}
				kept = append(kept, it)
			}
			items = kept
		}
		sortItems(items)
		// 应用控制台拖动保存的顺序（未出现的按默认序追加在后）
		ids := make([]string, 0, len(items))
		byID := make(map[string]modelItem, len(items))
		for _, it := range items {
			ids = append(ids, it.ID)
			byID[it.ID] = it
		}
		out := make([]modelItem, 0, len(items))
		for _, id := range reorderBy(ids, order.Models) {
			out = append(out, byID[id])
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"fetched_at":      time.Now().Format("2006-01-02 15:04:05"),
			"dynamic":         dynamic,
			"models":          out,
			"internal_hidden": hidden,
		})
	}

	if infos := h.fetchDynamicModels(); len(infos) > 0 {
		dynamic = true
		items := make([]modelItem, 0, len(infos))
		for _, mi := range infos {
			cl := mi.ContextWindow
			if cl == 0 {
				cl = 131072
			}
			items = append(items, modelItem{ID: mi.ID, ContextLength: cl, ConsumptionRate: mi.Rate})
		}
		emit(items, true)
		return
	}
	// 回退：静态表（不含倍率）
	items := make([]modelItem, 0, len(staticModels))
	for _, m := range staticModels {
		id, _ := m["id"].(string)
		cl, _ := m["context_length"].(int)
		items = append(items, modelItem{ID: id, ContextLength: int64(cl), ConsumptionRate: 0})
	}
	emit(items, dynamic)
}

// isInternalModel 判定上游返回的「内部 / 占位」模型（控制台默认隐藏）。
//
// 上游 get_detail_param 会把内部子代理、自定义占位配置一并放在 config_info_list
// 里返回，且**没有任何标识字段**区分它们 —— 只能按命名模式识别：
//   - custom_model_*  ：占位/自定义配置（实测 14 个）
//   - *_subagent/*_agent：内部子代理（browser_use / computer_use / explore / file_search）
//   - summary         ：内部摘要功能
//
// 注意：aquila / sagitta / claude-opus-5 等虽倍率为 0 或有内部色彩，
// 但命名像真实可用模型，保守保留，不过滤。
func isInternalModel(id string) bool {
	if id == "" {
		return true
	}
	if strings.HasPrefix(id, "custom_model_") {
		return true
	}
	// 用子串而非后缀：explore_sub_agent_v2 这类带版本后缀的，后缀判定会漏网
	if strings.Contains(id, "_subagent") || strings.Contains(id, "_agent") {
		return true
	}
	return id == "summary"
}

// uiOrder 控制台自定义顺序（拖动排序后保存）：键为列表名，值为 uid / 模型 ID 序列。
type uiOrder struct {
	Accounts []string `json:"accounts,omitempty"`
	Models   []string `json:"models,omitempty"`
}

// adminOrderPath UI 顺序持久化路径；UsageDir 为空时不落盘（重启后回到默认序）。
func (h *Handler) adminOrderPath() string {
	if h.cfg.UsageDir == "" {
		return ""
	}
	return filepath.Join(h.cfg.UsageDir, "admin-order.json")
}

func (h *Handler) loadUIOrder() uiOrder {
	p := h.adminOrderPath()
	if p == "" {
		return uiOrder{}
	}
	raw, err := os.ReadFile(p)
	if err != nil {
		return uiOrder{}
	}
	var o uiOrder
	_ = json.Unmarshal(raw, &o)
	return o
}

// reorderBy 按自定义顺序重排：order 里出现的按序在前，未出现的保持原相对序追加在后。
// order 为空或全不匹配时原样返回，保证新增条目不会因旧快照而丢失。
func reorderBy(ids []string, order []string) []string {
	if len(order) == 0 || len(ids) == 0 {
		return ids
	}
	pos := make(map[string]int, len(ids))
	for i, id := range ids {
		pos[id] = i
	}
	out := make([]string, 0, len(ids))
	used := make(map[string]bool, len(ids))
	for _, id := range order {
		if _, ok := pos[id]; ok && !used[id] {
			out = append(out, id)
			used[id] = true
		}
	}
	for _, id := range ids {
		if !used[id] {
			out = append(out, id)
			used[id] = true
		}
	}
	return out
}

// adminGetOrder 返回已保存的控制台顺序（读接口，无鉴权）。
func (h *Handler) adminGetOrder(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, h.loadUIOrder())
}

// adminUsage 请求流水：读取某天（默认北京今天）的 usage jsonl，
// 返回明细（新→旧）+ 全量聚合（请求数/成功失败/token/按模型/按小时）。
//
// 除单日外还支持区间（7 天 / 30 天）：跨多个 usage-*.jsonl 汇总，
// 聚合全程累计、明细只留最新 limit 条，并额外给出按天摘要供面板画趋势柱。
//
// 只读接口，无鉴权（与 credits/models 一致，局域网内面板）。
// 参数：
//   - date  ：北京日期 YYYY-MM-DD，默认今天；区间模式下作为**结束日**
//   - range ：1d(默认) | 7d | 30d，统计区间跨度
//   - limit ：明细条数上限，默认 200（最大 1000）
//   - probe ：all(默认) | real | probe，过滤探测/真实流量
func (h *Handler) adminUsage(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	date := strings.TrimSpace(q.Get("date"))
	if date == "" {
		date = time.Now().In(beijing).Format("2006-01-02")
	}
	// 日期格式校验：只允许 YYYY-MM-DD，避免路径穿越（date 会拼进文件名）
	if _, err := time.Parse("2006-01-02", date); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "date 需为 YYYY-MM-DD"})
		return
	}
	// 区间跨度：1d = 只看 date 当天（向后兼容旧前端）；7d / 30d = 以 date 为结束日回溯。
	// 非法值一律退回 1d —— 面板档位是固定的，异常值不值得报错打断。
	spanDays := 1
	switch strings.ToLower(strings.TrimSpace(q.Get("range"))) {
	case "7d", "7":
		spanDays = 7
	case "30d", "30":
		spanDays = 30
	}
	limit := 200
	if v := strings.TrimSpace(q.Get("limit")); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			limit = n
		}
	}

	var entries []usageEntry
	var agg usageAggregate
	var warn []string
	var daily []usageDayStat
	from := date
	if spanDays <= 1 {
		entries, agg, warn = usageScan(h.cfg.UsageDir, date, limit)
	} else {
		f, err := usageShiftDate(date, -(spanDays - 1))
		if err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": "date 需为 YYYY-MM-DD"})
			return
		}
		from = f
		res := usageScanRange(h.cfg.UsageDir, from, date, limit)
		entries, agg, warn, daily = res.Entries, res.Agg, res.Warn, res.Daily
	}

	// 探测/真实过滤（过滤只影响明细，聚合仍是全量 —— 全量口径更有参考价值）
	probeFilter := strings.ToLower(strings.TrimSpace(q.Get("probe")))
	if probeFilter == "probe" || probeFilter == "real" {
		want := probeFilter == "probe"
		kept := entries[:0]
		for _, e := range entries {
			if e.Probe == want {
				kept = append(kept, e)
			}
		}
		entries = kept
	}

	if entries == nil {
		entries = []usageEntry{}
	}

	// 昵称映射 uid→nickname：流水里存的 uid 是纯数字，面板要显示成人话。
	// 直接读池内状态（已在内存），不额外打上游。
	nicknames := map[string]string{}
	for _, s := range h.cfg.Pool.List() {
		if s.Nickname != "" {
			nicknames[s.UID] = s.Nickname
		}
	}
	// 倍率映射 model→consumption_rate：与「模型与倍率」tab 同源（fetchDynamicModels），
	// 命中 1h 缓存时不产生任何上游请求；上游失败为空 map，前端显示「未知」。
	rates := map[string]float64{}
	for _, mi := range h.fetchDynamicModels() {
		if mi.Rate > 0 {
			rates[mi.ID] = mi.Rate
		}
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"date":       date,
		"range":      rangeLabel(spanDays),
		"from":       from,
		"to":         date,
		"fetched_at": time.Now().In(beijing).Format("2006-01-02 15:04:05"),
		"entries":    entries,
		"agg":        agg,
		"daily":      daily,
		"warn":       warn,
		"usage_dir":  h.cfg.UsageDir,
		"nicknames":  nicknames,
		"rates":      rates,
	})
}

// rangeLabel 把区间天数转成面板约定的字符串（1d/7d/30d）。
func rangeLabel(days int) string {
	switch {
	case days >= 30:
		return "30d"
	case days >= 7:
		return "7d"
	default:
		return "1d"
	}
}

// adminSaveOrder 保存控制台拖动排序结果（写操作，需 Bearer 鉴权）。
// 仅影响 UI 展示顺序，**不改变 pool 选号逻辑**（选号仍按权益包过期/剩余积分规则序）。
func (h *Handler) adminSaveOrder(w http.ResponseWriter, r *http.Request) {
	p := h.adminOrderPath()
	if p == "" {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{"error": "UsageDir 未配置，无法保存顺序"})
		return
	}
	var o uiOrder
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&o); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "bad json: " + err.Error()})
		return
	}
	raw, _ := json.MarshalIndent(o, "", "  ")
	if err := os.WriteFile(p, raw, 0o644); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "save failed: " + err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}
