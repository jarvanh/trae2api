package pool

import (
	"path/filepath"
	"testing"
	"time"

	"trae2api/internal/auth"
)

func TestCheckinGenerationRotateAndPersist(t *testing.T) {
	dir := t.TempDir()
	sf := filepath.Join(dir, "state.json")
	p := New(sf)
	p.Add(&auth.Auth{UID: "u1", AccessToken: "at", RefreshToken: "rt"})

	if got := p.CheckinGeneration("u1"); got != 0 {
		t.Fatalf("初始代数=%d want 0", got)
	}
	if got := p.BumpCheckinGeneration("u1"); got != 1 {
		t.Fatalf("换代后=%d want 1", got)
	}
	if got := p.CheckinGeneration("u1"); got != 1 {
		t.Fatalf("读回代数=%d want 1", got)
	}

	// 换代数必须跨重启生效（落 state.json）
	p2 := New(sf)
	p2.Add(&auth.Auth{UID: "u1", AccessToken: "at", RefreshToken: "rt"})
	if got := p2.CheckinGeneration("u1"); got != 1 {
		t.Errorf("重载后代数=%d want 1（代数未持久化）", got)
	}
	if got := p.BumpCheckinGeneration("nosuch"); got != 0 {
		t.Errorf("未知账号换代应返回 0，got %d", got)
	}
}

// TestCheckinBackoffGrowsAndCaps 退避必须递增且封顶，避免把账号锁死。
func TestCheckinBackoffGrowsAndCaps(t *testing.T) {
	prev := time.Duration(0)
	for i := 0; i < 3; i++ {
		d := checkinBackoff(i)
		if d <= prev {
			t.Fatalf("第 %d 次退避 %v 未大于上次 %v", i, d, prev)
		}
		prev = d
	}
	if got := checkinBackoff(0); got != time.Minute {
		t.Errorf("首次退避=%v want 1m", got)
	}
	// 封顶 8 分钟
	if got := checkinBackoff(100); got != 8*time.Minute {
		t.Errorf("封顶退避=%v want 8m", got)
	}
}

// TestCheckinRateLimitedThenCleared 9074 退避写入后，成功要清零计数但**保留代数**。
func TestCheckinRateLimitedThenCleared(t *testing.T) {
	p := New(filepath.Join(t.TempDir(), "state.json"))
	p.Add(&auth.Auth{UID: "u2", AccessToken: "at", RefreshToken: "rt"})

	if !p.CheckinRetryAfter("u2").IsZero() {
		t.Fatal("初始不应有退避窗口")
	}
	after := p.NoteCheckinRateLimited("u2")
	if after.IsZero() || !after.After(time.Now()) {
		t.Fatalf("退避截止=%v 异常", after)
	}
	if got := p.CheckinRetryAfter("u2"); got.IsZero() {
		t.Error("退避窗口未记录")
	}

	p.BumpCheckinGeneration("u2") // 假设已换代
	p.NoteCheckinChecked("u2")    // 签到成功
	if got := p.CheckinRetryAfter("u2"); !got.IsZero() {
		t.Errorf("成功后退避窗口应清零，got %v", got)
	}
	if got := p.CheckinGeneration("u2"); got != 1 {
		t.Errorf("成功后代数应保留=1，got %d", got)
	}
}
