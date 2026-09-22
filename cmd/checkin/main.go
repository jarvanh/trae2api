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
