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
