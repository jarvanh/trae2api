package upstream

import (
	"encoding/json"
	"testing"
)

// 2026-10-30 实测配方逐字段断言：model → <model>__max（config_name 保持裸名，
// 带 __max 后缀上游 4001），extra_info.v2_max_mode_enabled=true。
func TestSetMaxTierModeRewritesModelAndSwitch(t *testing.T) {
	src := []byte(`{"model":"glm-5.3","config_name":"glm-5.3","messages":[{"role":"user","content":"hi"}]}`)
	out := SetMaxTierMode(src)
	var obj map[string]any
	if err := json.Unmarshal(out, &obj); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if obj["config_name"] != "glm-5.3" || obj["model"] != "glm-5.3__max" {
		t.Errorf("config_name 应保持裸名、model 应升级 __max: %v/%v", obj["config_name"], obj["model"])
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
	if obj["model"] != DefaultConfigName+"__max" {
		t.Errorf("model = %v, want %s__max", obj["model"], DefaultConfigName)
	}
	if _, has := obj["config_name"]; has {
		t.Errorf("config_name 本就缺失不应被新增（交给 PrepareBody 归一化）, got %v", obj["config_name"])
	}
}

// PrepareBody 归一化防护：config_name 恒为裸目录身份，__max 后缀只允许在
// model 字段（2026-10-30 直连矩阵：config_name 带 __max → 上游 4001）。
func TestPrepareBodyKeepsConfigNameBare(t *testing.T) {
	out := PrepareBodyFunction([]byte(`{"model":"glm-5.3__max","config_name":"glm-5.3__max","messages":[]}`), Function)
	var obj map[string]any
	_ = json.Unmarshal(out, &obj)
	if obj["config_name"] != "glm-5.3" {
		t.Errorf("config_name = %v, want 裸名（4001 根因防护）", obj["config_name"])
	}
	if obj["model"] != "glm-5.3__max" {
		t.Errorf("model = %v, want __max 保留", obj["model"])
	}
}

func TestSetMaxTierModeUnparseableReturnsSrc(t *testing.T) {
	src := []byte("not json")
	if got := SetMaxTierMode(src); string(got) != string(src) {
		t.Errorf("解析失败应原样返回, got %s", got)
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
