package main

// checkin — 全账号批量签到（旁路工具，与调度器同一条签到路径，带失败冷却）。
// 补齐 signin.sh 引用但源码树缺失的 ./cmd/signin 能力：
// 复用 internal/{auth,pool,upstream}，签到逻辑与 internal/scheduler
// 的 RunCheckinNow 一致（CheckinStatus → 未签则 CheckinClaim →
// UserEntUsage 积分刷新 + ReenableIfCredits 解冻），幂等，已签到自动跳过。
//
// 冷却策略（避免上游 9074「参与用户太多」限流下每轮空转重试）：
//   - 签到成功/已签：当日不再发起任何签到请求（last_claim_date 记录）
//   - 签到失败：30 分钟起步、指数递增（30m → 60m → 120m 封顶）
// 冷却与当日状态持久化在 <state 同目录>/checkin-state.json，跨轮次/跨重启生效。
//
// 输出 JSON Lines（逐账号一行），供 openclaw workflow 组装 Telegram 通知。

import (
	"encoding/json"
	"flag"
	"fmt"
	"math/rand"
	"os"
	"path/filepath"
	"time"

	"trae2api/internal/auth"
	"trae2api/internal/pool"
	"trae2api/internal/upstream"
)

type result struct {
	UID        string `json:"uid"`
	Nickname   string `json:"nickname"`
	CheckedIn  bool   `json:"checked_in"`
	Claimed    bool   `json:"claimed"`
	Credits    int64  `json:"credits"`
	Err        string `json:"err,omitempty"`
	Skipped    string `json:"skipped,omitempty"`     // "already" | "cooldown"
	SkipReason string `json:"skip_reason,omitempty"` // 人类可读跳过原因
}

type acctState struct {
	LastClaimDate string `json:"last_claim_date,omitempty"`
	LastCredits   int64  `json:"last_credits,omitempty"`
	LastFailAt    int64  `json:"last_fail_at,omitempty"`
	FailCount     int    `json:"fail_count,omitempty"`
}

func main() {
	dir := flag.String("dir", "auths", "auths 目录")
	state := flag.String("state", "data/state.json", "pool 状态文件")
	staggerMin := flag.Int("stagger-min", 30, "账号真实请求间最小间隔（分钟）")
	staggerMax := flag.Int("stagger-max", 60, "账号真实请求间最大间隔（分钟）")
	flag.Parse()

	auths, err := auth.LoadDir(*dir)
	if err != nil {
		fmt.Fprintln(os.Stderr, "load auths:", err)
		os.Exit(1)
	}
	p := pool.New(*state)
	p.SyncToDir(auths)
	up := upstream.New()

	// 冷却/当日状态与 pool state 同目录存放，随 Dropbox 持久化
	csPath := filepath.Join(filepath.Dir(*state), "checkin-state.json")
	perAcct := map[string]*acctState{}
	if b, err := os.ReadFile(csPath); err == nil {
		_ = json.Unmarshal(b, &perAcct)
	}
	save := func() {
		if b, err := json.MarshalIndent(perAcct, "", "  "); err == nil {
			_ = os.WriteFile(csPath, b, 0o644)
		}
	}
	defer save()

	today := time.Now().Format("2006-01-02")
	var lastReq time.Time // 上次真实签到请求时刻（错峰基准；零请求分支不计入）
	enc := json.NewEncoder(os.Stdout)
	total, ok := 0, 0

	for _, st := range p.List() {
		if st.Disabled {
			continue
		}
		a := p.AuthByUID(st.UID)
		if a == nil || a.RefreshTokenValue() == "" {
			continue
		}
		total++
		// 补齐 machineId（每账号互异 16 位数字，与 deviceId 同风格）并固化：
		// 多账号设备画像进一步区分（主人 09-25 定「不同账号独立设备」）
		if ch, err := a.EnsureCheckinMachineID(); err == nil && ch {
			if err := a.SaveAtomic(); err != nil {
				fmt.Fprintf(os.Stderr, "ensure machineId %s: %v\n", st.UID, err)
			}
		}
		cs, exists := perAcct[st.UID]
		if !exists {
			cs = &acctState{}
			perAcct[st.UID] = cs
		}
		r := result{UID: st.UID, Nickname: a.Nickname}

		// 当日已签 → 直接跳过，不发任何请求
		if cs.LastClaimDate == today {
			r.CheckedIn = true
			r.Credits = cs.LastCredits
			r.Skipped = "already"
			r.SkipReason = "今日已签（跳过查询）"
			_ = enc.Encode(r)
			continue
		}
		// 失败冷却：30m 起步指数递增，封顶 2h
		if cs.LastFailAt > 0 {
			cd := 30 * time.Minute << uint(min(cs.FailCount-1, 2))
			if remain := time.Until(time.Unix(cs.LastFailAt, 0).Add(cd)); remain > 0 {
				r.Skipped = "cooldown"
				r.SkipReason = fmt.Sprintf("冷却中（剩约 %d 分钟）", int(remain.Minutes())+1)
				_ = enc.Encode(r)
				continue
			}
		}

		// —— 错峰：真实请求之间保底随机间隔（默认 30–60 分钟，主人 09-25 定）——
		// already/cooldown 分支零请求直接跳过，不计入间隔；只有真发请求才需错峰。
		if !lastReq.IsZero() {
			lo, hi := *staggerMin, *staggerMax
			if hi < lo {
				hi = lo
			}
			wait := time.Duration(lo) * time.Minute
			if hi > lo {
				wait += time.Duration(rand.Int63n(int64(hi-lo) * int64(time.Minute)))
			}
			if w := time.Until(lastReq.Add(wait)); w > 0 {
				fmt.Fprintf(os.Stderr, "[stagger] uid=%s 距上次请求 %s，再等 %s\n",
					st.UID, time.Since(lastReq).Round(time.Second), w.Round(time.Second))
				time.Sleep(w)
			}
		}

		// —— token 刷新：错峰后单轮流程可达数小时，发请求前确保 token 有效 ——
		if a.NeedsRefresh(5 * time.Minute) {
			if err := up.RefreshToken(a); err != nil {
				r.Err = fmt.Sprintf("refresh: %v", err)
				cs.LastFailAt = time.Now().Unix()
				cs.FailCount++
				_ = enc.Encode(r)
				lastReq = time.Now()
				continue
			}
			if err := a.SaveAtomic(); err != nil {
				fmt.Fprintf(os.Stderr, "save %s: %v\n", st.UID, err)
			}
		}
		lastReq = time.Now()

		checkedIn, _, enable, err := up.CheckinStatus(a)
		if err != nil {
			r.Err = fmt.Sprintf("checkin status: %v", err)
		} else if !checkedIn && enable {
			if err := up.CheckinClaim(a); err != nil {
				r.Err = fmt.Sprintf("checkin claim: %v", err)
			} else {
				r.Claimed = true
			}
		} else {
			r.CheckedIn = checkedIn
		}
		// 积分刷新 + 解冻（与调度器一致：无论签到成败都查一次用量）
		remain, err := up.UserEntUsage(a)
		if err != nil {
			if r.Err == "" {
				r.Err = fmt.Sprintf("ent-usage: %v", err)
			}
		} else {
			r.Credits = remain
			p.ReenableIfCredits(st.UID, remain)
		}

		if r.Err == "" {
			ok++
			// 成功/已签：记录当日并清零失败计数
			cs.LastClaimDate = today
			cs.LastCredits = r.Credits
			cs.LastFailAt = 0
			cs.FailCount = 0
		} else {
			cs.LastFailAt = time.Now().Unix()
			cs.FailCount++
		}
		_ = enc.Encode(r)
	}
	fmt.Fprintf(os.Stderr, "checkin done: %d/%d ok\n", ok, total)
}
