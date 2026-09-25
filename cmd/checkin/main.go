package main

// checkin — 全账号批量签到（旁路工具，与调度器同一条签到路径，带失败冷却）。
// 补齐 signin.sh 引用但源码树缺失的 ./cmd/signin 能力：
// 复用 internal/{auth,pool,upstream}，签到逻辑与 internal/scheduler
// 的 RunCheckinNow 一致（CheckinStatus → 未签则 CheckinClaim →
// UserEntUsage 积分刷新 + ReenableIfCredits 解冻），幂等，已签到自动跳过。
//
// 冷却策略（避免上游 9074「参与用户太多」限流下每轮空转重试）：
//   - 签到成功/已签：当日不再发起任何签到请求（last_claim_date 记录）
//   - 签到失败：30 分钟起步、指数递增（30m → 60m → 120m 封顶），
//     冷却中的账号本轮排到冷却结束后重试（不早于其错峰槽位）
// 冷却与当日状态持久化在 <state 同目录>/checkin-state.json，跨轮次/跨重启生效。
//
// 错峰排程（主人 09-25 定）：启动先零请求输出全部账号的排程快照行
// （今日已签 / 冷却·计划时刻 / 排程·计划时刻），部署通知拍快照时即可看到
// 完整账号池与安排的签到时间；随后按预排时刻逐个真实签到，完成后追加结果行
// （通知按 uid 取最后一行 = 有结果显示结果，无结果显示排程）。
//
// 输出 JSON Lines（逐账号一至两行），供 openclaw workflow 组装 Telegram 通知。

import (
	"encoding/json"
	"flag"
	"fmt"
	"math/rand"
	"os"
	"path/filepath"
	"sort"
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
	Skipped    string `json:"skipped,omitempty"`     // "already" | "cooldown" | "scheduled"
	SkipReason string `json:"skip_reason,omitempty"` // 人类可读跳过原因
	// PlannedAt 本轮未发真实请求时，预计下一次真实签到请求的东八区时刻
	//（冷却结束 / 错峰排程到点，跨天带日期）。空串 = 本轮已有真实结果。
	PlannedAt string `json:"planned_at,omitempty"`
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
	enc := json.NewEncoder(os.Stdout)
	total := 0

	// 东八区展示（runner 是 UTC，直接 Format 会差 8 小时，见 deploy skill 实测坑）
	cst := time.FixedZone("Asia/Shanghai", 8*3600)
	// fmtPlanned 计划时刻展示：当天只给 HH:MM，跨天（深夜冷却跨午夜）带上月-日
	fmtPlanned := func(t time.Time) string {
		pt, now := t.In(cst), time.Now().In(cst)
		if pt.Format("2006-01-02") == now.Format("2006-01-02") {
			return pt.Format("15:04")
		}
		return pt.Format("01-02 15:04")
	}
	// randWait 随机错峰间隔 [staggerMin, staggerMax] 分钟
	randWait := func() time.Duration {
		lo, hi := *staggerMin, *staggerMax
		if hi < lo {
			hi = lo
		}
		wait := time.Duration(lo) * time.Minute
		if hi > lo {
			wait += time.Duration(rand.Int63n(int64(hi-lo) * int64(time.Minute)))
		}
		return wait
	}

	// —— Pass A：排程快照 —— 零请求、秒级完成，全部账号立即各落一行，
	// 让部署通知在拍快照时就能看到完整账号池与安排的签到时间（主人 09-25 定）。
	type job struct {
		uid string
		a   *auth.Auth
		at  time.Time // 预排的真实请求时刻
	}
	var jobs []job
	var nextAt time.Time
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
		r.Credits = st.Credits // 池内当前积分（排程/冷却行也展示真实积分，不是 0）

		// 当日已签 → 直接跳过，不发任何请求
		if cs.LastClaimDate == today {
			r.CheckedIn = true
			r.Skipped = "already"
			r.SkipReason = "今日已签（跳过查询）"
			_ = enc.Encode(r)
			continue
		}
		// 本轮会真实请求：预排时刻 = 错峰槽位（首个立即，其后随机 30–60 分钟间隔）；
		// 失败冷却（30m 起步指数递增，封顶 2h）不早于冷却结束 —— 冷却中的账号
		// 排到冷却后本轮内重试，安排的签到时间对主人真实可见（主人 09-25 定）
		at := time.Now()
		if !nextAt.IsZero() {
			at = nextAt.Add(randWait())
		}
		nextAt = at
		skipped, reason := "scheduled", "排队错峰"
		if cs.LastFailAt > 0 {
			cd := 30 * time.Minute << uint(min(cs.FailCount-1, 2))
			if coolEnd := time.Unix(cs.LastFailAt, 0).Add(cd); coolEnd.After(at) {
				at = coolEnd
				skipped, reason = "cooldown", "冷却中"
			}
		}
		jobs = append(jobs, job{uid: st.UID, a: a, at: at})
		r.Skipped = skipped
		r.SkipReason = reason
		r.PlannedAt = fmtPlanned(at)
		_ = enc.Encode(r)
	}

	// Pass B 串行执行：按预排时刻排序，等待单调递增，各账号按展示的计划时间执行
	sort.Slice(jobs, func(i, j int) bool { return jobs[i].at.Before(jobs[j].at) })

	// —— Pass B：按排程逐个真实签到（结果行追加在排程行之后，通知按 uid 取末行）——
	ok := 0
	for _, j := range jobs {
		if w := time.Until(j.at); w > 0 {
			fmt.Fprintf(os.Stderr, "[stagger] uid=%s 按排程等待 %s（至 %s）\n",
				j.uid, w.Round(time.Second), j.at.In(cst).Format("15:04"))
			time.Sleep(w)
		}
		// —— 凭据保鲜：错峰后单轮流程可达数小时 ——
		// 先从磁盘重读账号文件（常驻服务会持续刷新 token 并写回），拿到最新凭据；
		// 仅当确实临近过期才自行刷新（与服务端的刷新存在轮换竞态，尽量避免自刷；
		// 自刷失败也不记账号失败，交由签到接口按真实结果判定）。
		a := j.a
		if fp := a.FilePath; fp != "" {
			if raw, rerr := os.ReadFile(fp); rerr == nil {
				if na, perr := auth.Parse(raw); perr == nil {
					a.Lock()
					a.AccessToken = na.AccessToken
					a.RefreshToken = na.RefreshToken
					a.ExpiresAt = na.ExpiresAt
					a.Unlock()
				}
			}
		}
		if a.NeedsRefresh(5 * time.Minute) {
			if err := up.RefreshToken(a); err != nil {
				fmt.Fprintf(os.Stderr, "refresh %s: %v（继续用现有凭据发起，成败交由签到接口判定）\n", j.uid, err)
			} else if serr := a.SaveAtomic(); serr != nil {
				fmt.Fprintf(os.Stderr, "save %s: %v\n", j.uid, serr)
			}
		}

		r := result{UID: j.uid, Nickname: a.Nickname}
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
			p.ReenableIfCredits(j.uid, remain)
		}

		cs := perAcct[j.uid]
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
