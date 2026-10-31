package pool

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"trae2api/internal/auth"
)

// 未探测过期时间时规则①打平，退化为规则②「积分最少者胜」。
func TestPickLeastCreditsWhenNoExpireData(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "u1"})
	p.Add(&auth.Auth{UID: "u2"})
	p.Add(&auth.Auth{UID: "u3"})
	p.SetCredits("u1", 100)
	p.SetCredits("u2", 500)
	p.SetCredits("u3", 300)
	got := p.Pick()
	if got == nil || got.UID != "u1" {
		t.Fatalf("pick=%+v want u1 (least credits)", got)
	}
}

// 规则①：最早过期优先，压过规则②（哪怕它积分最多）。
func TestPickEarliestExpireWins(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "u1"})
	p.Add(&auth.Auth{UID: "u2"})
	p.SetCredits("u1", 9000)
	p.SetCredits("u2", 10)
	now := time.Now()
	p.SetNearestExpire("u1", now.Add(30*24*time.Hour).Unix()) // 一个月后
	p.SetNearestExpire("u2", now.Add(2*time.Hour).Unix())     // 两小时后到期
	got := p.Pick()
	if got == nil || got.UID != "u2" {
		t.Fatalf("pick=%+v want u2 (earliest expire)", got)
	}
}

// 规则①并列 → 规则②积分最少者胜。
func TestPickEqualExpireThenLeastCredits(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "u1"})
	p.Add(&auth.Auth{UID: "u2"})
	p.SetCredits("u1", 700)
	p.SetCredits("u2", 200)
	same := time.Now().Add(24 * time.Hour).Unix()
	p.SetNearestExpire("u1", same)
	p.SetNearestExpire("u2", same)
	got := p.Pick()
	if got == nil || got.UID != "u2" {
		t.Fatalf("pick=%+v want u2 (same expire, least credits)", got)
	}
}

// 未探测（0）视为无穷远，必须排在已探明账号之后。
func TestPickProbedBeatsUnprobed(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "u1"})
	p.Add(&auth.Auth{UID: "u2"})
	p.SetCredits("u1", 1)    // 积分最少，但未探测 → 应排最后
	p.SetCredits("u2", 9999) // 积分最多，但两小时后到期
	p.SetNearestExpire("u2", time.Now().Add(time.Hour).Unix())
	got := p.Pick()
	if got == nil || got.UID != "u2" {
		t.Fatalf("pick=%+v want u2 (u1 unprobed ranks last)", got)
	}
}

func TestSetNearestExpire(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "u1"})
	ts := time.Now().Add(3 * time.Hour).Unix()
	p.SetNearestExpire("u1", ts)
	if got := p.NearestExpire("u1"); got != ts {
		t.Fatalf("nearest=%d want %d", got, ts)
	}
	st, _ := p.Status("u1")
	if st.NearestExpire != ts {
		t.Fatalf("status nearest=%d want %d", st.NearestExpire, ts)
	}
}

// nearest_expire 必须落盘并在重启（重新 load）后恢复 —— 规则①数据源，
// 2026-10-16 事故回归测试：重启清零 → 规则①失效 → 积分最多账号被轮空。
func TestNearestExpirePersistence(t *testing.T) {
	dir := t.TempDir()
	fp := filepath.Join(dir, "state.json")
	p := New(fp)
	p.Add(&auth.Auth{UID: "u1"})
	ts := time.Now().Add(26 * time.Hour).Unix()
	p.SetNearestExpire("u1", ts)

	// 模拟重启：用同一状态文件重建池
	p2 := New(fp)
	p2.Add(&auth.Auth{UID: "u1"})
	if got := p2.NearestExpire("u1"); got != ts {
		t.Fatalf("nearest=%d want %d (must survive restart)", got, ts)
	}
}

func TestPickSkipsCooling(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "u1"})
	p.Add(&auth.Auth{UID: "u2"})
	p.SetCredits("u1", 100)
	p.SetCredits("u2", 50)
	p.Cooldown("u1", CoolPlan, time.Hour, "plan limit")
	got := p.Pick()
	if got == nil || got.UID != "u2" {
		t.Fatalf("pick=%+v want u2", got)
	}
}

func TestPickExpiredCooldownReturnsToHealthy(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "u1"})
	p.SetCredits("u1", 100)
	p.Cooldown("u1", CoolSoft, time.Millisecond, "429")
	time.Sleep(5 * time.Millisecond)
	got := p.Pick()
	if got == nil || got.UID != "u1" {
		t.Fatalf("pick=%+v want u1 after cooldown expiry", got)
	}
}

func TestPickNilWhenAllCooling(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "u1"})
	p.Cooldown("u1", CoolPlan, time.Hour, "x")
	if got := p.Pick(); got != nil {
		t.Fatalf("want nil, got %+v", got)
	}
}

func TestPickExcluding(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "u1"})
	p.Add(&auth.Auth{UID: "u2"})
	p.SetCredits("u1", 100)
	p.SetCredits("u2", 50)
	tried := map[string]bool{"u1": true}
	got := p.PickExcluding(tried)
	if got == nil || got.UID != "u2" {
		t.Fatalf("pick=%+v want u2", got)
	}
	tried["u2"] = true
	if got := p.PickExcluding(tried); got != nil {
		t.Fatalf("want nil, got %+v", got)
	}
}

func TestCooldownPersists(t *testing.T) {
	dir := t.TempDir()
	fp := filepath.Join(dir, "state.json")
	p := New(fp)
	p.Add(&auth.Auth{UID: "u1"})
	p.Cooldown("u1", CoolPlan, time.Hour, "plan limit")
	p2 := New(fp)
	p2.Add(&auth.Auth{UID: "u1"})
	if p2.Pick() != nil {
		t.Fatal("cooldown lost after reload")
	}
	st, ok := p2.Status("u1")
	if !ok || st.Reason != "plan limit" {
		t.Errorf("status=%+v ok=%v", st, ok)
	}
}

func TestDisablePersists(t *testing.T) {
	dir := t.TempDir()
	fp := filepath.Join(dir, "state.json")
	p := New(fp)
	p.Add(&auth.Auth{UID: "u1"})
	p.Disable("u1", "session dead")
	p2 := New(fp)
	p2.Add(&auth.Auth{UID: "u1"})
	if p2.Pick() != nil {
		t.Fatal("disabled account picked after reload")
	}
	st, _ := p2.Status("u1")
	if !st.Disabled || st.Reason != "session dead" {
		t.Errorf("status=%+v", st)
	}
}

func TestReenableIfCredits(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "u1"})
	p.Cooldown("u1", CoolPlan, time.Hour, "plan limit")
	p.ReenableIfCredits("u1", 500)
	got := p.Pick()
	if got == nil || got.UID != "u1" {
		t.Fatalf("should reenable, pick=%+v", got)
	}
}

func TestReenableZeroCreditsKeepsCooling(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "u1"})
	p.Cooldown("u1", CoolPlan, time.Hour, "plan limit")
	p.ReenableIfCredits("u1", 0)
	if p.Pick() != nil {
		t.Fatal("zero credits should stay cooling")
	}
}

func TestReenableDoesNotTouchDisabled(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "u1"})
	p.Disable("u1", "session dead")
	p.ReenableIfCredits("u1", 500)
	if p.Pick() != nil {
		t.Fatal("disabled must not auto-reenable")
	}
}

func TestNoteErrorThreshold(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "u1"})
	for i := 0; i < 2; i++ {
		p.NoteError("u1", 3, 10*time.Minute)
		if p.Pick() == nil {
			t.Fatalf("cooling too early at %d", i+1)
		}
	}
	p.NoteError("u1", 3, 10*time.Minute)
	if p.Pick() != nil {
		t.Fatal("threshold 3 should cool the account")
	}
}

func TestNoteSuccessResetsCounter(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "u1"})
	p.NoteError("u1", 3, time.Hour)
	p.NoteError("u1", 3, time.Hour)
	p.NoteSuccess("u1")
	p.NoteError("u1", 3, time.Hour)
	p.NoteError("u1", 3, time.Hour)
	if p.Pick() == nil {
		t.Fatal("success should reset error counter")
	}
}

func TestList(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "u1", Nickname: "nick1"})
	p.Add(&auth.Auth{UID: "u2"})
	p.SetCredits("u1", 42)
	p.Cooldown("u2", CoolSoft, time.Minute, "429")
	list := p.List()
	if len(list) != 2 {
		t.Fatalf("list=%d", len(list))
	}
	var s1, s2 Status
	for _, s := range list {
		if s.UID == "u1" {
			s1 = s
		}
		if s.UID == "u2" {
			s2 = s
		}
	}
	if s1.Credits != 42 || s1.Nickname != "nick1" || s1.Disabled || s1.Cooling {
		t.Errorf("s1=%+v", s1)
	}
	if !s2.Cooling || s2.Reason != "429" {
		t.Errorf("s2=%+v", s2)
	}
}

func TestSyncToDirRemovesMissing(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "u1"})
	p.Add(&auth.Auth{UID: "u2"})
	p.SyncToDir([]*auth.Auth{{UID: "u2"}})
	if p.Pick() == nil || p.Pick().UID != "u2" {
		t.Fatal("u1 should be removed")
	}
	if _, ok := p.Status("u1"); ok {
		t.Fatal("u1 should not exist")
	}
}

func TestRemove(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "u1"})
	if !p.Remove("u1") {
		t.Fatal("remove existing should return true")
	}
	if p.Pick() != nil {
		t.Fatal("u1 should be gone after remove")
	}
	if _, ok := p.Status("u1"); ok {
		t.Fatal("u1 status should not exist")
	}
	if p.Remove("u1") {
		t.Fatal("remove missing should return false")
	}
}

// TestRemoveClearsStateEntry Remove 后 state.json 不再含该条目；
// 模拟「auths 文件也已删」场景：reload（不再 Add 该 uid）后池中无该账号。
func TestRemoveClearsStateEntry(t *testing.T) {
	dir := t.TempDir()
	fp := filepath.Join(dir, "state.json")
	p := New(fp)
	p.Add(&auth.Auth{UID: "u1"})
	p.SetCredits("u1", 100) // 触发 saveLocked 落盘
	p.Add(&auth.Auth{UID: "u2"})
	p.SetCredits("u2", 50)
	p.Remove("u1")
	// reload：不再 Add u1（等价于 auths/trae-u1.json 已被 API 层删除）
	p2 := New(fp)
	if _, ok := p2.Status("u1"); ok {
		t.Fatal("u1 should not be in state after remove + reload")
	}
	if _, ok := p2.Status("u2"); !ok {
		t.Fatal("u2 should remain in state")
	}
	// u2 从 state reload，credits 应保留
	st, _ := p2.Status("u2")
	if st.Credits != 50 {
		t.Errorf("u2 credits lost on reload: %+v", st)
	}
}

func TestSetEnabledSoftSwitch(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "u1"})
	p.SetCredits("u1", 100)
	if !p.SetEnabled("u1", false, "user paused") {
		t.Fatal("set enabled on existing should return true")
	}
	if p.Pick() != nil {
		t.Fatal("disabled-by-user account should be skipped by Pick")
	}
	st, _ := p.Status("u1")
	if st.Enabled {
		t.Errorf("Enabled flag should be false, got %+v", st)
	}
	if st.Disabled {
		t.Errorf("soft switch must not set Disabled (session dead), got %+v", st)
	}
	if st.Reason != "user paused" {
		t.Errorf("reason should be recorded, got %+v", st)
	}
	// 重新启用
	p.SetEnabled("u1", true, "")
	got := p.Pick()
	if got == nil || got.UID != "u1" {
		t.Fatalf("re-enabled account should be picked, got %+v", got)
	}
	st2, _ := p.Status("u1")
	if !st2.Enabled {
		t.Errorf("Enabled should be true after re-enable, got %+v", st2)
	}
}

func TestSetEnabledDoesNotAffectDisabled(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "u1"})
	p.Disable("u1", "session dead")
	// session-dead 硬禁用时切软开关不应当作恢复
	p.SetEnabled("u1", true, "")
	if p.Pick() != nil {
		t.Fatal("disabled (session dead) must stay un-pickable even if enabled=true")
	}
	st, _ := p.Status("u1")
	if !st.Disabled {
		t.Errorf("Disabled should remain true, got %+v", st)
	}
}

func TestSetEnabledOnMissing(t *testing.T) {
	p := New("")
	if p.SetEnabled("nope", false, "x") {
		t.Fatal("set enabled on missing should return false")
	}
}

func TestSetEnabledPersistsAcrossReload(t *testing.T) {
	dir := t.TempDir()
	fp := filepath.Join(dir, "state.json")
	p := New(fp)
	p.Add(&auth.Auth{UID: "u1"})
	p.SetEnabled("u1", false, "paused")
	p2 := New(fp)
	p2.Add(&auth.Auth{UID: "u1"})
	if p2.Pick() != nil {
		t.Fatal("soft-disabled account should stay skipped after reload")
	}
	st, _ := p2.Status("u1")
	if st.Enabled || st.Reason != "paused" {
		t.Errorf("soft switch state lost: %+v", st)
	}
}

// TestStateFileBackwardCompat 旧 state.json 无 enabled 字段时，reload 默认按启用处理。
func TestStateFileBackwardCompat(t *testing.T) {
	dir := t.TempDir()
	fp := filepath.Join(dir, "state.json")
	// 手写一份无 enabled 字段的旧格式
	old := `{"accounts":{"u1":{"credits":100,"disabled":false,"reason":""}}}`
	if err := os.WriteFile(fp, []byte(old), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	p := New(fp)
	p.Add(&auth.Auth{UID: "u1"})
	st, ok := p.Status("u1")
	if !ok {
		t.Fatal("u1 should load from old state")
	}
	if !st.Enabled {
		t.Errorf("missing enabled field should default to true, got %+v", st)
	}
	got := p.Pick()
	if got == nil || got.UID != "u1" {
		t.Fatalf("old-format account should be pickable, got %+v", got)
	}
}

func TestPickWorkHighestCredits(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "u1"})
	p.Add(&auth.Auth{UID: "u2"})
	p.Add(&auth.Auth{UID: "u3"})
	p.SetWorkCredits("u1", 50.5)
	p.SetWorkCredits("u2", 200.1)
	p.SetWorkCredits("u3", 120.0)

	got := p.PickWork()
	if got == nil || got.UID != "u2" {
		t.Fatalf("PickWork should pick u2 (200.1 credits), got %v", got)
	}

	// 排除 u2，应该轮换到 u3
	got2 := p.PickWorkExcluding(map[string]bool{"u2": true})
	if got2 == nil || got2.UID != "u3" {
		t.Fatalf("PickWorkExcluding should pick u3 (120 credits), got %v", got2)
	}

	// 再次排除 u3，应该轮换到 u1
	got3 := p.PickWorkExcluding(map[string]bool{"u2": true, "u3": true})
	if got3 == nil || got3.UID != "u1" {
		t.Fatalf("PickWorkExcluding should pick u1 (50.5 credits), got %v", got3)
	}

	// 全部排除返回 nil
	got4 := p.PickWorkExcluding(map[string]bool{"u1": true, "u2": true, "u3": true})
	if got4 != nil {
		t.Fatalf("all excluded should return nil, got %v", got4)
	}
}

func TestWorkCooldownAndIndependentFromSOLO(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "u1"})
	p.SetCredits("u1", 500)
	p.SetWorkCredits("u1", 100.0)

	// SOLO 进入 1005 长冷却
	p.Cooldown("u1", CoolPlan, 10*time.Minute, "solo plan limit")

	// SOLO 应该不可被挑选
	if p.Pick() != nil {
		t.Fatal("u1 should not be picked for SOLO during CoolPlan")
	}

	// 但 Work 通道仍然可用！
	if p.PickWork() == nil || p.PickWork().UID != "u1" {
		t.Fatal("u1 should still be pickable for Work while SOLO is in CoolPlan")
	}

	// 现在对 Work 进行冷却
	p.CooldownWork("u1", 10*time.Minute, "work credits empty")
	if p.PickWork() != nil {
		t.Fatal("u1 should not be picked for Work during CooldownWork")
	}

	st, _ := p.Status("u1")
	if !st.WorkCooling || st.WorkReason != "work credits empty" {
		t.Errorf("expected WorkCooling true, got %+v", st)
	}
}

func TestPickAffinity(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "u1"})
	p.Add(&auth.Auth{UID: "u2"})
	p.SetCredits("u1", 100)
	p.SetCredits("u2", 200)

	// 未探测过期 → 规则②积分最少者优先 → 首选 u1
	got1 := p.PickAffinity("sess-1", nil)
	if got1 == nil || got1.UID != "u1" {
		t.Fatalf("expected u1, got %+v", got1)
	}

	// 此时即便 u2 积分降到最少，sess-1 仍应粘性锁定在 u1
	p.SetCredits("u2", 1)
	got2 := p.PickAffinity("sess-1", nil)
	if got2 == nil || got2.UID != "u1" {
		t.Fatalf("expected sticky u1, got %+v", got2)
	}

	// 新会话 sess-2 按规则序择优：积分更少的 u2
	got3 := p.PickAffinity("sess-2", nil)
	if got3 == nil || got3.UID != "u2" {
		t.Fatalf("expected u2 for sess-2, got %+v", got3)
	}

	// 当 u1 冷却时，sess-1 自动漂移至 u2
	p.Cooldown("u1", CoolSoft, time.Minute, "rate limit")
	got4 := p.PickAffinity("sess-1", nil)
	if got4 == nil || got4.UID != "u2" {
		t.Fatalf("expected failover to u2, got %+v", got4)
	}
}

// TestPickAffinityExpirePressure 覆盖 2026-10-23 事故：错误风暴把会话粘性
// 劫持到到期最晚的账号后，冷却恢复时必须按规则①改回临期账号。
func TestPickAffinityExpirePressure(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "late"})
	p.Add(&auth.Auth{UID: "soon"})
	now := time.Now()
	p.SetPacks("late", []PackState{{Desc: "签到奖励", Limit: 100, ExpireAt: now.Add(30 * 24 * time.Hour).Unix()}})
	p.SetPacks("soon", []PackState{{Desc: "签到奖励", Limit: 100, ExpireAt: now.Add(2 * time.Hour).Unix()}})

	// 新会话按规则①选中临期账号
	if got := p.PickAffinity("sess-1", nil); got == nil || got.UID != "soon" {
		t.Fatalf("expected soon, got %+v", got)
	}

	// 模拟 4001 风暴：临期账号冷却，粘性漂移到 late
	p.Cooldown("soon", CoolSoft, time.Minute, "consecutive errors")
	if got := p.PickAffinity("sess-1", nil); got == nil || got.UID != "late" {
		t.Fatalf("expected failover to late, got %+v", got)
	}

	// 冷却恢复：临期压制应把粘性改回 soon，而不是永久钉在 late
	p.Cooldown("soon", CoolSoft, 0, "")
	if got := p.PickAffinity("sess-1", nil); got == nil || got.UID != "soon" {
		t.Fatalf("expected expire pressure to switch back to soon, got %+v", got)
	}
	// 改绑后应保持（soon 仍是最临期）
	if got := p.PickAffinity("sess-1", nil); got == nil || got.UID != "soon" {
		t.Fatalf("expected sticky soon, got %+v", got)
	}
}

// TestPickAffinityExpireSlack 到期时间接近（<1h）时不抢粘性，避免两账号互抖。
func TestPickAffinityExpireSlack(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "a"})
	p.Add(&auth.Auth{UID: "b"})
	now := time.Now()
	p.SetPacks("a", []PackState{{Desc: "签到奖励", Limit: 100, ExpireAt: now.Add(2 * time.Hour).Unix()}})
	p.SetPacks("b", []PackState{{Desc: "签到奖励", Limit: 100, ExpireAt: now.Add(150 * time.Minute).Unix()}})

	// a 冷却时把 sess-1 绑到 b
	p.Cooldown("a", CoolSoft, time.Minute, "test")
	if got := p.PickAffinity("sess-1", nil); got == nil || got.UID != "b" {
		t.Fatalf("expected b, got %+v", got)
	}

	// a 恢复：仅早 30 分钟 < 1h 阈值 → 保持粘性
	p.Cooldown("a", CoolSoft, 0, "")
	if got := p.PickAffinity("sess-1", nil); got == nil || got.UID != "b" {
		t.Fatalf("expected sticky b within slack, got %+v", got)
	}
}

// TestPickAffinityUnknownExpire 绑定账号未探测到期（0）时，已知到期的账号构成压制。
func TestPickAffinityUnknownExpire(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "unknown"})
	p.Add(&auth.Auth{UID: "known"})
	p.SetCredits("unknown", 100)
	p.SetCredits("known", 200)

	// 两者都未探测到期 → 规则②先绑到积分更少的 unknown
	if got := p.PickAffinity("sess-1", nil); got == nil || got.UID != "unknown" {
		t.Fatalf("expected unknown, got %+v", got)
	}
	// known 探测到到期后，绑定账号（到期未知）应被压制，改绑 known
	p.SetPacks("known", []PackState{{Desc: "签到奖励", Limit: 200, ExpireAt: time.Now().Add(48 * time.Hour).Unix()}})
	if got := p.PickAffinity("sess-1", nil); got == nil || got.UID != "known" {
		t.Fatalf("expected known by expire pressure, got %+v", got)
	}
}

func TestSetPacksAndApplyUsageRemain(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "u1"})
	// 两个包：临期包 150（remain 83）+ 后包 100（remain 100）→ credits 合计 183
	p.SetPacks("u1", []PackState{
		{Desc: "签到奖励", Limit: 150, Used: 67, ExpireAt: 2000000000},
		{Desc: "签到奖励", Limit: 100, Used: 0, ExpireAt: 2500000000},
	})
	if got := p.NearestExpire("u1"); got != 2000000000 {
		t.Fatalf("nearest=%d", got)
	}
	if got := p.Packs("u1"); len(got) != 2 {
		t.Fatalf("packs=%d", len(got))
	}
	// 流内记账：新总额 50 → 扣 133。FIFO：先扣临期包 83 剩余，再扣后包 50
	p.ApplyUsageRemain("u1", 50)
	ps := p.Packs("u1")
	if ps[0].Used != 150 || ps[0].Limit-ps[0].Used != 0 {
		t.Errorf("pack0 used=%d/%d", ps[0].Used, ps[0].Limit)
	}
	if ps[1].Used != 50 || ps[1].Limit-ps[1].Used != 50 {
		t.Errorf("pack1 used=%d/%d", ps[1].Used, ps[1].Limit)
	}
	// 临期包耗尽 → nearest 切到后包
	if got := p.NearestExpire("u1"); got != 2500000000 {
		t.Fatalf("nearest after drain=%d", got)
	}
	// 涨额（签到加包）：只更新总额，不动账本（等探测轮全量同步）
	p.ApplyUsageRemain("u1", 250)
	if got := p.Credits("u1"); got != 250 {
		t.Fatalf("credits after gain=%d", got)
	}
}

func TestApplyUsageRemainNoLedger(t *testing.T) {
	// 无账本（未探测过）：只更新总额，不报错、不建账
	p := New("")
	p.Add(&auth.Auth{UID: "u2"})
	p.SetCredits("u2", 100)
	p.ApplyUsageRemain("u2", 61)
	if got := p.Credits("u2"); got != 61 {
		t.Fatalf("credits=%d", got)
	}
	if ps := p.Packs("u2"); ps != nil {
		t.Fatalf("unexpected packs: %+v", ps)
	}
}

// TestPickForModelCapability 覆盖 solo_agent 通道模型集按账号漂移的场景：
// 只有真报告了该 config 的账号才能被选中，否则会间歇性失败。
func TestPickForModelCapability(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "u1", AccessToken: "at1"})
	p.Add(&auth.Auth{UID: "u2", AccessToken: "at2"})
	p.SetCredits("u1", 1000)
	p.SetCredits("u2", 1000)
	p.SetModels("u1", []string{"glm-5.3"})
	p.SetModels("u2", []string{"glm-5.3", "free"})

	// free 只在 u2 上：即便 u1 按规则②并列更靠前，也不能选它
	if got := p.PickAffinityForModel("", nil, "free"); got == nil || got.UID != "u2" {
		t.Fatalf("pick(free)=%v want u2", got)
	}
	// glm-5.3 两号都有：回到常规规则序（积分并列 → UID 升序）
	if got := p.PickAffinityForModel("", nil, "glm-5.3"); got == nil || got.UID != "u1" {
		t.Fatalf("pick(glm-5.3)=%v want u1", got)
	}
	if !p.ModelKnown("free") || p.ModelKnown("nope") {
		t.Error("ModelKnown 判定错误")
	}
}

// TestPickNoModelKeepsRuleOrder 传空 model = 不做能力限制（等价旧行为）。
// 单独开一个干净池：上一用例里两个号都刚被 pick 过，会被 minPickGap 防惊群
// 窗口改挑「久未使用」的那个，混在一起测不出规则序。
func TestPickNoModelKeepsRuleOrder(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "u1", AccessToken: "at1"})
	p.Add(&auth.Auth{UID: "u2", AccessToken: "at2"})
	p.SetCredits("u1", 1000)
	p.SetCredits("u2", 1000)
	p.SetModels("u1", []string{"glm-5.3"})
	p.SetModels("u2", []string{"glm-5.3", "free"})

	if got := p.PickAffinityForModel("", nil, ""); got == nil || got.UID != "u1" {
		t.Fatalf("pick(不限制)=%v want u1", got)
	}
	// 只给能力全都不匹配的 model → 候选集为空，必须返回 nil（由调用方给出明确错）
	if got := p.PickAffinityForModel("", nil, "only-on-unknown-account"); got != nil {
		t.Fatalf("两号都没这能力时应返回 nil，got=%v", got)
	}
}

// TestPickForModelUnprobedNotRestricted 未探测能力集的账号必须视为「什么都能接」：
// 能力数据缺失时宁可不限制，也不能让整池凭空 503。
func TestPickForModelUnprobedNotRestricted(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "probed", AccessToken: "at1"})
	p.Add(&auth.Auth{UID: "unprobed", AccessToken: "at2"})
	p.SetCredits("probed", 100)
	p.SetCredits("unprobed", 5000) // 积分更多，规则②本应排在后面
	p.SetModels("probed", []string{"glm-5.3"})

	if got := p.PickAffinityForModel("", nil, "some-new-model"); got == nil || got.UID != "unprobed" {
		t.Fatalf("未探测账号应作为兜底可选，got=%v", got)
	}
	// 清能力后恢复不限制：probed 回到候选集，且规则②（积分最少者胜）选它
	p.SetModels("probed", nil)
	if got := p.PickAffinityForModel("", nil, "glm-5.3"); got == nil || got.UID != "probed" {
		t.Fatalf("清除能力后应按规则序选中 probed，got=%v", got)
	}
}

// TestPickAffinityRebindsForCapability 会话粘性不得凌驾于能力之上：
// 绑定账号没有该模型的能力时，要改绑到有的账号（否则列表说有、请求却失败）。
func TestPickAffinityRebindsForCapability(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "u1", AccessToken: "at1"})
	p.Add(&auth.Auth{UID: "u2", AccessToken: "at2"})
	p.SetCredits("u1", 1000)
	p.SetCredits("u2", 1000)
	p.SetModels("u1", []string{"glm-5.3"})
	p.SetModels("u2", []string{"glm-5.3", "free"})

	if got := p.PickAffinityForModel("s1", nil, "glm-5.3"); got == nil || got.UID != "u1" {
		t.Fatalf("首次绑定=%v want u1", got)
	}
	// 同会话换请求只有 u2 能承接的模型 → 必须重绑到 u2
	if got := p.PickAffinityForModel("s1", nil, "free"); got == nil || got.UID != "u2" {
		t.Fatalf("能力不匹配时应重绑，got=%v want u2", got)
	}
	// 重绑生效后，回到 u1 也能承接的模型仍按粘性走 u2（绑定已被改写）
	if got := p.PickAffinityForModel("s1", nil, "glm-5.3"); got == nil || got.UID != "u2" {
		t.Fatalf("粘性应延续到新绑定账号，got=%v want u2", got)
	}
}

// TestHealthyExcludesCooling Healthy 供跨账号并集刷新用，必须跳过冷却/禁用号。
func TestHealthyExcludesCooling(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "u1", AccessToken: "at1"})
	p.Add(&auth.Auth{UID: "u2", AccessToken: "at2"})
	p.Cooldown("u2", CoolSoft, time.Minute, "429")
	got := p.Healthy()
	if len(got) != 1 || got[0].UID != "u1" {
		t.Fatalf("healthy=%v want 只有 u1", got)
	}
}

// TestPickForTierMaxEntitlement 覆盖 2026-10-31 max 权益预门控：
// __max 档按账号下发（get_detail_param 档位表逐号漂移），wantMax 选号只落
// 在权益集内的账号；空集（探测过、全无档）≠ 未探测（nil，保守放行）。
func TestPickForTierMaxEntitlement(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "u1", AccessToken: "at1"})
	p.Add(&auth.Auth{UID: "u2", AccessToken: "at2"})
	p.SetCredits("u1", 1000)
	p.SetCredits("u2", 1000)
	p.SetMaxModels("u1", []string{"glm-5.3"})
	p.SetMaxModels("u2", []string{})

	// wantMax：只有 u1 可承接（u2 已探测且无权益，提前出局）
	if got := p.PickAffinityForTier("", nil, "glm-5.3", true); got == nil || got.UID != "u1" {
		t.Fatalf("pick(max glm-5.3)=%v want u1", got)
	}
	// dev 请求不受 max 权益限制（models 未探测 = 不限制），应正常选到号
	if got := p.PickAffinityForTier("", nil, "glm-5.3", false); got == nil {
		t.Fatal("dev 请求应正常选号")
	}
	// 池内权益判定：glm-5.3 有人可承接；other 全部已探测且无权益
	if !p.MaxServed("glm-5.3") {
		t.Error("MaxServed(glm-5.3)=false, u1 有权益应为 true")
	}
	if p.MaxServed("other") {
		t.Error("MaxServed(other)=true, 全部已探测且无权益应为 false")
	}
	if !p.AccountMaxEntitled("u-unknown", "glm-5.3") {
		t.Error("未知账号应放行（宁可不限制）")
	}
	if p.AccountMaxEntitled("u2", "glm-5.3") {
		t.Error("u2 无 glm-5.3 max 权益应为 false")
	}
}

// TestMaxServedUnprobedPermissive 未探测（冷启动/目录拉取失败）不得拦请求：
// 任一号未探测 → MaxServed 视为可服务；全部探测且无权益才判 false。
func TestMaxServedUnprobedPermissive(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "probed-empty", AccessToken: "at1"})
	p.Add(&auth.Auth{UID: "unprobed", AccessToken: "at2"})
	p.SetMaxModels("probed-empty", []string{})
	if !p.MaxServed("glm-5.3") {
		t.Error("存在未探测号时应放行")
	}
	p.SetMaxModels("unprobed", []string{})
	if p.MaxServed("glm-5.3") {
		t.Error("全部已探测且无权益应为 false")
	}
}

// TestBlockMaxModel 运行时拉黑只封 __max 档：该号 dev 档照常可选，
// 其他账号不受影响；全部拉黑后 wantMax 无候选返回 nil（调用方降级 dev）。
func TestBlockMaxModel(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "u1", AccessToken: "at1"})
	p.Add(&auth.Auth{UID: "u2", AccessToken: "at2"})
	p.SetCredits("u1", 1000)
	p.SetCredits("u2", 1000)
	p.SetMaxModels("u1", []string{"glm-5.3"})
	p.SetMaxModels("u2", []string{"glm-5.3"})

	p.BlockMaxModel("u1", "glm-5.3")
	if p.AccountMaxEntitled("u1", "glm-5.3") {
		t.Error("拉黑后 u1 不应有 max 权益")
	}
	if !p.AccountMaxEntitled("u2", "glm-5.3") {
		t.Error("u2 不受 u1 拉黑影响")
	}
	if got := p.PickAffinityForTier("", nil, "glm-5.3", true); got == nil || got.UID != "u2" {
		t.Fatalf("pick(max)=%v want u2（u1 已拉黑）", got)
	}
	if got := p.PickAffinityForTier("", nil, "glm-5.3", false); got == nil {
		t.Fatal("u1 dev 档应照常可选")
	}
	p.BlockMaxModel("u2", "glm-5.3")
	if got := p.PickAffinityForTier("", nil, "glm-5.3", true); got != nil {
		t.Fatalf("全部拉黑后 wantMax 应返回 nil, got=%v", got)
	}
}
