package main

// checkin — 全账号批量签到（旁路工具）。
// 上游 signin.sh 引用的 ./cmd/signin 在当前源码树不存在（脚本与源码不同步），
// 本工具复用 internal/{auth,pool,upstream}，签到逻辑与 internal/scheduler
// 的 RunCheckinNow 完全一致：CheckinStatus → 未签则 CheckinClaim →
// UserEntUsage 积分刷新 + ReenableIfCredits 解冻；幂等，已签到自动跳过。
// 输出 JSON Lines（逐账号一行），供 openclaw.yml trae2api 步骤组装 Telegram 通知。

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"

	"trae2api/internal/auth"
	"trae2api/internal/pool"
	"trae2api/internal/upstream"
)

type result struct {
	UID       string `json:"uid"`
	Nickname  string `json:"nickname"`
	CheckedIn bool   `json:"checked_in"`
	Claimed   bool   `json:"claimed"`
	Credits   int64  `json:"credits"`
	Err       string `json:"err,omitempty"`
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
		r := result{UID: st.UID, Nickname: a.Nickname}
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
		}
		_ = enc.Encode(r)
	}
	fmt.Fprintf(os.Stderr, "checkin done: %d/%d ok\n", ok, total)
}
