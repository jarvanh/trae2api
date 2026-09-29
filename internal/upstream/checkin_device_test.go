package upstream

import (
	"errors"
	"net/http"
	"testing"

	"trae2api/internal/auth"
)

// TestCheckinDeviceIDGolden 交叉向量（与 Python 版 Trae2api-cn 一致）：
// CheckinDeviceID("u1", 0) == "4302850041909017"。
// 该值是与移植源核对过的锚点，改动派生算法会在此失败。
func TestCheckinDeviceIDGolden(t *testing.T) {
	if got := CheckinDeviceID("u1", 0); got != "4302850041909017" {
		t.Errorf("CheckinDeviceID(u1,0)=%s want 4302850041909017", got)
	}
	// 代数变化必须产生完全不同的号
	gen1 := CheckinDeviceID("u1", 1)
	if gen1 == "4302850041909017" {
		t.Fatalf("gen1 与基线相同：%s", gen1)
	}
	if len(gen1) != 16 {
		t.Errorf("gen1 长度=%d want 16", len(gen1))
	}
	// 幂等：同参数必须同结果
	if CheckinDeviceID("u1", 1) != gen1 {
		t.Errorf("同参数结果不稳定")
	}
}

func TestCheckinDeviceIDShape(t *testing.T) {
	cases := []struct {
		name string
		in   string
		gen  int
	}{
		{"空身份", "", 0},
		{"普通uid", "1589236987332375", 0},
		{"高代数", "779994471072188", 9},
	}
	for _, c := range cases {
		got := CheckinDeviceID(c.in, c.gen)
		if c.in == "" {
			if got != "" {
				t.Errorf("%s: got %q want empty", c.name, got)
			}
			continue
		}
		if len(got) != 16 {
			t.Errorf("%s: len=%d want 16 (%s)", c.name, len(got), got)
		}
		for _, r := range got {
			if r < '0' || r > '9' {
				t.Errorf("%s: 非数字字符 %q in %s", c.name, r, got)
				break
			}
		}
	}
}

func TestCheckinIdentity(t *testing.T) {
	if got := CheckinIdentity(&auth.Auth{UID: "u1", DeviceID: "dev"}); got != "u1" {
		t.Errorf("UID 优先失败: %s", got)
	}
	if got := CheckinIdentity(&auth.Auth{DeviceID: "dev"}); got != "dev" {
		t.Errorf("回退 DeviceID 失败: %s", got)
	}
	if got := CheckinIdentity(nil); got != "" {
		t.Errorf("nil 应返回空串: %s", got)
	}
}

// TestCheckinHeadersMinimal 签到头必须极简：不带 X-Machine-Id / UA / X-User-Region，
// 且带派生设备号。这是 2026-09-29 实证成功的请求形态。
func TestCheckinHeadersMinimal(t *testing.T) {
	req, _ := http.NewRequest(http.MethodPost, "http://x/y", nil)
	CheckinHeaders(req, &auth.Auth{AccessToken: "at", UID: "u1", MachineID: "m1", DeviceID: "old"}, "9988776655443322")

	if got := req.Header.Get("X-Device-Id"); got != "9988776655443322" {
		t.Errorf("X-Device-Id=%s want 派生号", got)
	}
	if got := req.Header.Get("Authorization"); got != "Cloud-IDE-JWT at" {
		t.Errorf("Authorization=%s", got)
	}
	for _, bad := range []string{"X-Machine-Id", "X-User-Region", "User-Agent"} {
		if v := req.Header.Get(bad); v != "" {
			t.Errorf("不该带的头 %s=%s", bad, v)
		}
	}
}

func TestIsCheckinBusy(t *testing.T) {
	if IsCheckinBusy(nil) {
		t.Error("nil 不应判定为 9074")
	}
	if !IsCheckinBusy(&CheckinError{Code: 9074, Msg: "当前参与用户太多"}) {
		t.Error("9074 应判定为 busy")
	}
	if IsCheckinBusy(&CheckinError{Code: 9095, Msg: "设备今日已签到"}) {
		t.Error("9095 不是 busy")
	}
	if IsCheckinBusy(errors.New("boom")) {
		t.Error("普通错误不应判定为 busy")
	}
}
