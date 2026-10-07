// Package scheduler 定时任务：每日签到 + token 预刷新。
// 签到成功后重新查积分，积分 > 0 的冷却账号自动解冻。
package scheduler

import (
	"context"
	"errors"
	"fmt"
	"log"
	"time"

	"trae2api/internal/auth"
	"trae2api/internal/pool"
	"trae2api/internal/upstream"
)

// Config 调度器依赖。
type Config struct {
	Pool         *pool.Pool
	Upstream     *upstream.Client
	CheckinHour  int           // 每日签到小时，默认 9
	RefreshHours []int         // token 预刷新小时，默认 [3]
	RefreshSkew  time.Duration // 预刷新窗口，默认 72h

	// ArchiveUsage 流水归档钩子（可选，nil = 不启用）。
	// 用函数注入而非直接 import server 包：scheduler 只依赖 pool/upstream，
	// 反向依赖会把「调度器」和「HTTP 服务层」绑死，也让单测没法独立构造。
	ArchiveUsage func() (archived int, removed int, err error)
}

// Scheduler 调度器。
type Scheduler struct {
	cfg Config
}

// New 构建。
func New(cfg Config) *Scheduler {
	if cfg.CheckinHour < 0 {
		cfg.CheckinHour = 9
	}
	if len(cfg.RefreshHours) == 0 {
		cfg.RefreshHours = []int{3}
	}
	if cfg.RefreshSkew <= 0 {
		cfg.RefreshSkew = 72 * time.Hour
	}
	return &Scheduler{cfg: cfg}
}

// nextFire 返回 now 之后最近的一个整点触发时间；hours 为本地小时（0-23）。
func nextFire(now time.Time, hours []int) time.Time {
	var earliest time.Time
	for _, h := range hours {
		t := time.Date(now.Year(), now.Month(), now.Day(), h, 0, 0, 0, now.Location())
		if !t.After(now) {
			t = t.Add(24 * time.Hour)
		}
		if earliest.IsZero() || t.Before(earliest) {
			earliest = t
		}
	}
	return earliest
}

// Run 主循环，阻塞直到 ctx 取消。
func (s *Scheduler) Run(ctx context.Context) {
	all := append(append([]int{}, s.cfg.RefreshHours...), s.cfg.CheckinHour)
	for {
		next := nextFire(time.Now(), all)
		timer := time.NewTimer(time.Until(next))
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-timer.C:
			h := time.Now().Hour()
			if contains(s.cfg.RefreshHours, h) {
				s.RunRefreshNow()
			}
			if s.cfg.CheckinHour == h {
				s.RunCheckinNow()
				// 归档挂在签到这一轮：一天一次足够，且此时上游刚被查过一轮
				s.RunArchiveNow()
			}
		}
	}
}

// RunArchiveNow 立即执行一次流水归档（超保留期的日明细 → 月汇总）。
// 未配置钩子或归档失败都只 log：归档是后台维护动作，绝不能影响签到主流程。
func (s *Scheduler) RunArchiveNow() {
	if s.cfg.ArchiveUsage == nil {
		return
	}
	archived, removed, err := s.cfg.ArchiveUsage()
	if err != nil {
		log.Printf("[scheduler] 流水归档失败：%v", err)
		return
	}
	if archived > 0 || removed > 0 {
		log.Printf("[scheduler] 流水归档完成：归档 %d 天，删除明细 %d 个", archived, removed)
	}
}

func contains(hours []int, h int) bool {
	for _, v := range hours {
		if v == h {
			return true
		}
	}
	return false
}

// checkinOne 对单账号执行签到（status → 未签则 claim）。
//
// 设备身份用 CheckinDeviceID 按 UID 派生，而不是 auth 文件里那个 deviceId：
// 后者可能是批量导入时合成的，一旦被上游风控标记就恒定返回 9074
// 「当前参与用户太多」，错峰重试再多次也签不成 —— 换代换新设备号才是解。
// 命中 9074 时换代后**立即重试一次**（自愈），仍失败才退避等下一轮。
func (s *Scheduler) checkinOne(uid string, a *auth.Auth) {
	identity := upstream.CheckinIdentity(a)
	gen := s.cfg.Pool.CheckinGeneration(uid)

	try := func(generation int) error {
		devID := upstream.CheckinDeviceID(identity, generation)
		checkedIn, _, enable, err := s.cfg.Upstream.CheckinStatus(a, devID)
		if err != nil {
			return err
		}
		if checkedIn {
			s.cfg.Pool.NoteCheckinChecked(uid)
			log.Printf("checkin %s: already checked in", uid)
			return nil
		}
		if !enable {
			return fmt.Errorf("checkin disabled")
		}
		if err := s.cfg.Upstream.CheckinClaim(a, devID); err != nil {
			return err
		}
		s.cfg.Pool.NoteCheckinChecked(uid)
		log.Printf("checkin %s: ok", uid)
		return nil
	}

	err := try(gen)
	if err == nil {
		return
	}
	if !upstream.IsCheckinBusy(err) {
		log.Printf("checkin %s: %v", uid, err)
		return
	}
	// 9074 ＝ 当前设备号被标记 → 换代换号重试一次
	next := s.cfg.Pool.BumpCheckinGeneration(uid)
	log.Printf("checkin %s: 9074 命中，设备换代 %d→%d 重试（旧错误: %v）", uid, gen, next, err)
	if err2 := try(next); err2 != nil {
		after := s.cfg.Pool.NoteCheckinRateLimited(uid)
		log.Printf("checkin %s: 换代后仍失败 %v；退避至 %s", uid, err2, after.Format("15:04:05"))
	}
}

// RunCheckinNow 立即对所有账号执行签到 + 积分刷新 + 解冻 + 过期探测。
// 冷却中的账号也参与（签到就是为了解冻它们）；禁用的跳过。
func (s *Scheduler) RunCheckinNow() {
	for _, st := range s.cfg.Pool.List() {
		if st.Disabled {
			continue
		}
		a := s.cfg.Pool.AuthByUID(st.UID)
		if a == nil || a.RefreshTokenValue() == "" {
			continue
		}
		// 签到：设备号改用 UID 派生的签到专用号（见 checkinOne 注释）
		s.checkinOne(st.UID, a)
		// 查积分 + 解冻
		remain, err := s.cfg.Upstream.UserEntUsage(a)
		if err != nil {
			log.Printf("ent-usage %s: %v", st.UID, err)
		} else {
			s.cfg.Pool.ReenableIfCredits(st.UID, remain)
		}
		// 探测最早到期权益包：挑选规则①的数据源。
		// 失败不影响本轮其余流程（排序键会退化为 0 → 按规则②积分最少裁决）。
		s.probeNearestExpire(st.UID, a)
	}
}

// probeNearestExpire 拉取权益包明细，把最早到期的未用完包时间注入 pool。
// PackList 已按 expire_at 升序，取第一个还有余额的包即可。
func (s *Scheduler) probeNearestExpire(uid string, a *auth.Auth) {
	packs, err := s.cfg.Upstream.PackList(a)
	if err != nil {
		log.Printf("pack-list %s: %v", uid, err)
		return
	}
	for _, p := range packs {
		if p.Limit-p.Used <= 0 {
			continue // 已耗尽，跳过
		}
		s.cfg.Pool.SetNearestExpire(uid, p.ExpireAt)
		return
	}
	// 名下已无未用完的包 → 置 0（未探测同义），不参与规则①竞争
	s.cfg.Pool.SetNearestExpire(uid, 0)
}

// RunRefreshNow 立即对所有账号刷新 token；session 失效的自动禁用。
func (s *Scheduler) RunRefreshNow() {
	for _, st := range s.cfg.Pool.List() {
		if st.Disabled {
			continue
		}
		a := s.cfg.Pool.AuthByUID(st.UID)
		if a == nil || a.RefreshTokenValue() == "" {
			continue
		}
		if !a.NeedsRefresh(s.cfg.RefreshSkew) {
			continue
		}
		if err := s.cfg.Upstream.RefreshToken(a); err != nil {
			log.Printf("refresh %s: %v", st.UID, err)
			var ue *upstream.Error
			if errors.As(err, &ue) && ue.Kind == upstream.ErrSessionDead {
				s.cfg.Pool.Disable(st.UID, "session dead")
			}
			continue
		}
		if err := a.SaveAtomic(); err != nil {
			log.Printf("refresh %s save: %v", st.UID, err)
		}
	}
}
