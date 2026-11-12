package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestDefault(t *testing.T) {
	c := Default()
	if c.Listen != "127.0.0.1:7864" {
		t.Errorf("listen=%s", c.Listen)
	}
	if c.DefaultModel != "glm-5.2" {
		t.Errorf("default_model=%s", c.DefaultModel)
	}
	if err := c.normalize(); err != nil {
		t.Fatalf("normalize: %v", err)
	}
	if c.PlanCreditDur.Hours() != 12 {
		t.Errorf("plan_credit=%v", c.PlanCreditDur)
	}
	if c.SoftRateDur.Seconds() != 60 {
		t.Errorf("soft_rate=%v", c.SoftRateDur)
	}
}

func TestLoadFile(t *testing.T) {
	dir := t.TempDir()
	fp := filepath.Join(dir, "c.json")
	os.WriteFile(fp, []byte(`{"listen":"127.0.0.1:9999","default_model":"kimi-k2.7-code","schedule":{"checkin_hour":7}}`), 0o600)
	c, err := Load(fp)
	if err != nil {
		t.Fatal(err)
	}
	if c.Listen != "127.0.0.1:9999" || c.DefaultModel != "kimi-k2.7-code" || c.Schedule.CheckinHour != 7 {
		t.Errorf("c=%+v", c)
	}
	// APIKey 不读 json
	if c.APIKey != "" {
		t.Errorf("api_key should not come from json, got %q", c.APIKey)
	}
}

func TestLoadMissingFileFallsBackToDefaults(t *testing.T) {
	c, err := Load("/nonexistent/config.json")
	if err != nil {
		t.Fatal(err)
	}
	if c.Listen != "127.0.0.1:7864" {
		t.Errorf("listen=%s", c.Listen)
	}
}

func TestAPIKeyOnlyFromEnv(t *testing.T) {
	t.Setenv("TW2A_API_KEY", "test-key")
	c, err := Load("")
	if err != nil {
		t.Fatal(err)
	}
	if c.APIKey != "test-key" {
		t.Errorf("api_key=%q", c.APIKey)
	}
}

func TestEnvOverride(t *testing.T) {
	t.Setenv("TW2A_LISTEN", ":7777")
	t.Setenv("TW2A_API_KEY", "test-key")
	t.Setenv("TW2A_DEFAULT_MODEL", "qwen-3.7-plus")
	t.Setenv("TW2A_SOFT_RATE", "30s")
	c, err := Load("")
	if err != nil {
		t.Fatal(err)
	}
	if c.Listen != ":7777" || c.DefaultModel != "qwen-3.7-plus" {
		t.Errorf("c=%+v", c)
	}
	if c.SoftRateDur.Seconds() != 30 {
		t.Errorf("soft_rate=%v", c.SoftRateDur)
	}
}

// TestEmptyKeyNonLoopbackRejected 安全默认（T2）：空 APIKey 时任何非回环
// 监听都必须拒绝启动，避免管理面板/代理无鉴权暴露到网络。
func TestEmptyKeyNonLoopbackRejected(t *testing.T) {
	for _, listen := range []string{":7864", "0.0.0.0:7864", "[::]:7864", "192.0.2.10:7864"} {
		t.Setenv("TW2A_API_KEY", "")
		t.Setenv("TW2A_LISTEN", listen)
		if _, err := Load(""); err == nil {
			t.Errorf("listen %q with empty api key: want startup error", listen)
		}
	}
	// 回环监听允许空 key。
	for _, listen := range []string{"127.0.0.1:7864", "localhost:7864", "[::1]:7864"} {
		t.Setenv("TW2A_API_KEY", "")
		t.Setenv("TW2A_LISTEN", listen)
		if _, err := Load(""); err != nil {
			t.Errorf("listen %q with empty api key should be allowed: %v", listen, err)
		}
	}
}

// TestNonLoopbackAllowedWithAPIKey 配了 key 的正常部署不受影响。
func TestNonLoopbackAllowedWithAPIKey(t *testing.T) {
	t.Setenv("TW2A_API_KEY", "test-key")
	t.Setenv("TW2A_LISTEN", ":7864")
	c, err := Load("")
	if err != nil {
		t.Fatalf("non-loopback listen with api key should be allowed: %v", err)
	}
	if c.Listen != ":7864" {
		t.Errorf("listen=%s", c.Listen)
	}
}

// TestCallbackAddrDefaultsToLoopback 回调监听默认回环（T2/T8）。
func TestCallbackAddrDefaultsToLoopback(t *testing.T) {
	t.Setenv("TW2A_API_KEY", "test-key")
	t.Setenv("TW2A_CALLBACK_PORT", "")
	t.Setenv("TW2A_CALLBACK_LISTEN", "")
	c, err := Load("")
	if err != nil {
		t.Fatal(err)
	}
	if got := c.CallbackAddr(); got != "127.0.0.1:18080" {
		t.Errorf("callback addr=%q want 127.0.0.1:18080", got)
	}
	// 容器部署可显式覆盖为 0.0.0.0（宿主机侧端口仍限制回环）。
	t.Setenv("TW2A_CALLBACK_LISTEN", "0.0.0.0:18080")
	c, err = Load("")
	if err != nil {
		t.Fatal(err)
	}
	if got := c.CallbackAddr(); got != "0.0.0.0:18080" {
		t.Errorf("callback addr=%q", got)
	}
}

// TestCallbackPortInjectionRejected 回调端口必须是纯数字，防止拼进
// auth_callback_url 时注入额外 path/query。
func TestCallbackPortInjectionRejected(t *testing.T) {
	t.Setenv("TW2A_API_KEY", "test-key")
	t.Setenv("TW2A_CALLBACK_PORT", "18080/evil?x=1")
	if _, err := Load(""); err == nil {
		t.Fatal("want error for non-numeric callback_port")
	}
}

// TestBridgeModeRequiresToken 删除硬编码默认 token 后（T12），显式 bridge
// 模式缺 token 必须启动失败；已配置 token 的部署正常启动。
func TestBridgeModeRequiresToken(t *testing.T) {
	dir := t.TempDir()
	fp := filepath.Join(dir, "c.json")

	t.Setenv("TW2A_API_KEY", "test-key")
	t.Setenv("TW2A_WORK_BRIDGE_TOKEN", "")
	os.WriteFile(fp, []byte(`{"work_mode":"bridge"}`), 0o600)
	if _, err := Load(fp); err == nil {
		t.Fatal("bridge mode without token: want startup error")
	}

	t.Setenv("TW2A_WORK_BRIDGE_TOKEN", "random-test-token")
	if _, err := Load(fp); err != nil {
		t.Fatalf("bridge mode with token should start: %v", err)
	}
}

func TestBadDuration(t *testing.T) {
	dir := t.TempDir()
	fp := filepath.Join(dir, "c.json")
	os.WriteFile(fp, []byte(`{"cooldown":{"plan_credit":"not-a-duration"}}`), 0o600)
	if _, err := Load(fp); err == nil {
		t.Fatal("want error for bad duration")
	}
}
