package upstream

import (
	"encoding/json"
	"testing"
)

// 2026-10-26 实测配方逐字段断言：model/config_name → <config>__max，
// extra_info.v2_max_mode_enabled=true，缺一都会被 dev 档 4022 拒。
func TestSetMaxTierModeRewritesConfigAndSwitch(t *testing.T) {
	src := []byte(`{"model":"glm-5.3","config_name":"glm-5.3","messages":[{"role":"user","content":"hi"}]}`)
	out := SetMaxTierMode(src)
	var obj map[string]any
	if err := json.Unmarshal(out, &obj); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if obj["config_name"] != "glm-5.3__max" || obj["model"] != "glm-5.3__max" {
		t.Errorf("config_name/model = %v/%v, want __max", obj["config_name"], obj["model"])
	}
	extra, ok := obj["extra_info"].(map[string]any)
	if !ok || extra["v2_max_mode_enabled"] != true {
		t.Fatalf("extra_info.v2_max_mode_enabled missing: %v", obj["extra_info"])
	}
	if msgs, ok := obj["messages"].([]any); !ok || len(msgs) != 1 {
		t.Errorf("messages 应原样保留, got %v", obj["messages"])
	}
}

func TestSetMaxTierModeKeepsExistingExtraInfo(t *testing.T) {
	out := SetMaxTierMode([]byte(`{"config_name":"glm-5.3","extra_info":{"v2_user_preference":"x"}}`))
	var obj map[string]any
	_ = json.Unmarshal(out, &obj)
	extra := obj["extra_info"].(map[string]any)
	if extra["v2_user_preference"] != "x" || extra["v2_max_mode_enabled"] != true {
		t.Errorf("extra_info = %v, 既有字段应保留且追加开关", extra)
	}
}

func TestSetMaxTierModeFallsBackToDefaultConfig(t *testing.T) {
	out := SetMaxTierMode([]byte(`{"messages":[]}`))
	var obj map[string]any
	_ = json.Unmarshal(out, &obj)
	if obj["config_name"] != DefaultConfigName+"__max" {
		t.Errorf("config_name = %v, want %s__max", obj["config_name"], DefaultConfigName)
	}
}

func TestSetMaxTierModeUnparseableReturnsSrc(t *testing.T) {
	src := []byte("not json")
	if got := SetMaxTierMode(src); string(got) != string(src) {
		t.Errorf("解析失败应原样返回, got %s", got)
	}
}

func TestStripMaxTierModeRoundTrip(t *testing.T) {
	out := StripMaxTierMode(SetMaxTierMode([]byte(`{"config_name":"glm-5.3","model":"glm-5.3"}`)))
	var obj map[string]any
	_ = json.Unmarshal(out, &obj)
	if obj["config_name"] != "glm-5.3" || obj["model"] != "glm-5.3" {
		t.Errorf("strip 后 config_name/model = %v/%v", obj["config_name"], obj["model"])
	}
	if _, ok := obj["extra_info"]; ok {
		t.Errorf("strip 后 extra_info 应删除, got %v", obj["extra_info"])
	}
}

func TestMaxTierModelNameIdempotent(t *testing.T) {
	if got := MaxTierModelName("glm-5.3__max"); got != "glm-5.3__max" {
		t.Errorf("got %q", got)
	}
	if got := MaxTierModelName("glm-5.3"); got != "glm-5.3__max" {
		t.Errorf("got %q", got)
	}
}
