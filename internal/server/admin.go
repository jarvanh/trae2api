// 管理面板（/admin）：只读查询，无鉴权（本地面板）；CLI 操作留待开发。
package server

import (
	_ "embed"
	"net/http"
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

	writeJSON(w, http.StatusOK, map[string]any{
		"fetched_at": time.Now().Format("2006-01-02 15:04:05"),
		"accounts":   out,
	})
}
