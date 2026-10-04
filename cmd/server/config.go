// config.go 加载 JSON 配置 + TW2A_* 环境变量覆盖。
// APIKey 只从环境变量 TW2A_API_KEY 读取（SPEC §0 脱敏纪律：key 走 env，不落盘 git）。
package main

import (
	"encoding/json"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"
)

// Config 顶层配置。
type Config struct {
	Listen          string `json:"listen"`            // ":7864"
	CallbackPort    string `json:"callback_port"`     // "18080"（TRAE 登录回调监听端口，0 = 不起）
	APIKey          string `json:"-"`                 // 只读 env TW2A_API_KEY（不读 json）
	AuthDir         string `json:"auth_dir"`          // "./auths"
	UsageDir        string `json:"usage_dir"`         // "./data"（用量明细落盘目录，空 = 不落盘）
	StateFile       string `json:"state_file"`        // "./data/state.json"
	DefaultModel    string `json:"default_model"`     // "glm-5.2"
	WorkMode        string `json:"work_mode"`         // "auto" | "native" | "bridge" | "disabled" (默认 auto)
	WorkHost        string `json:"work_host"`         // 官方 Work 专属端点 (默认 https://api5-normal.mchost.guru)
	WorkBridgeURL   string `json:"work_bridge_url"`   // "http://host-gateway:7865" (本地 Work 积分桥接端点)
	WorkBridgeToken string `json:"work_bridge_token"` // 访问 WorkBridge 的 Bearer token（空 = 不鉴权）

	Cooldown struct {
		PlanCredit  string `json:"plan_credit"`   // "12h"
		SoftRate    string `json:"soft_rate"`     // "60s"
		ErrThresh   int    `json:"err_threshold"` // 3
		ErrCooldown string `json:"err_cooldown"`  // "10m"
	} `json:"cooldown"`

	Schedule struct {
		CheckinHour  int   `json:"checkin_hour"`  // 9
		RefreshHours []int `json:"refresh_hours"` // [3]
		// RefreshSkew token 预刷新窗口（如 "72h"）：到期前多久就开始续期。
		// 留空/非法 → 默认 72h。上游每次续期给 14 天，窗口太窄（如 24h）
		// 会让刷新只押在到期当天那一轮定时任务上，上游抽风就真过期了。
		RefreshSkew string `json:"refresh_skew"` // "72h"
	} `json:"schedule"`

	// UsageRetentionDays 日粒度流水保留天数：超期归档成月汇总后删除明细。
	// 0（默认）= 不启用，维持永久保留；建议 90。
	UsageRetentionDays int `json:"usage_retention_days"`

	Upstream struct {
		TimeoutSeconds int `json:"timeout_seconds"` // 120
		// KeepaliveSeconds 流式响应在「首个真实帧到达前」的心跳间隔（SSE 注释帧 `: ping`）。
		// 用于防止边缘网关（volc-dcdn/tengine）因长排队窗口连接静默而提前掐断（502）。
		// 0 = 关闭心跳（等同旧行为）。
		KeepaliveSeconds int `json:"keepalive_seconds"` // 15
	} `json:"upstream"`

	// 解析后的 duration。
	PlanCreditDur  time.Duration `json:"-"`
	SoftRateDur    time.Duration `json:"-"`
	ErrCooldownDur time.Duration `json:"-"`
	// RefreshSkewDur 由 schedule.refresh_skew 解析而来（token 预刷新窗口）。
	RefreshSkewDur time.Duration `json:"-"`
}

// Default 返回默认配置。
func Default() *Config {
	c := &Config{
		Listen:          ":7864",
		CallbackPort:    "18080",
		APIKey:          "",
		AuthDir:         "./auths",
		StateFile:       "./data/state.json",
		DefaultModel:    "glm-5.2",
		WorkMode:        "auto",
		WorkHost:        "",
		WorkBridgeURL:   "http://127.0.0.1:7865",
		WorkBridgeToken: "",
	}
	c.Cooldown.PlanCredit = "12h"
	c.Cooldown.SoftRate = "60s"
	c.Cooldown.ErrThresh = 3
	c.Cooldown.ErrCooldown = "10m"
	c.Schedule.CheckinHour = 9
	c.Schedule.RefreshHours = []int{3}
	c.Schedule.RefreshSkew = "72h"
	c.Upstream.TimeoutSeconds = 120
	c.UsageRetentionDays = 0 // 默认不启用归档（维持现状）
	return c
}

// Load 从 path 读配置，再用 TW2A_* env 覆盖。path 为空或不存在时用默认 + env。
func Load(path string) (*Config, error) {
	c := Default()
	if path != "" {
		raw, err := os.ReadFile(path)
		if err != nil {
			if os.IsNotExist(err) {
				// 配置文件可选：不存在 → 纯默认 + env
				c = Default()
			} else {
				return nil, fmt.Errorf("read config: %w", err)
			}
		} else if err := json.Unmarshal(raw, c); err != nil {
			return nil, fmt.Errorf("parse config: %w", err)
		}
	}
	applyEnv(c)
	if err := c.normalize(); err != nil {
		return nil, err
	}
	return c, nil
}

func applyEnv(c *Config) {
	if v := os.Getenv("TW2A_API_KEY"); v != "" {
		c.APIKey = v
	}
	if v := os.Getenv("TW2A_LISTEN"); v != "" {
		c.Listen = v
	}
	if v := os.Getenv("TW2A_CALLBACK_PORT"); v != "" {
		c.CallbackPort = v
	}
	if v := os.Getenv("TW2A_AUTH_DIR"); v != "" {
		c.AuthDir = v
	}
	if v := os.Getenv("TW2A_STATE_FILE"); v != "" {
		c.StateFile = v
	}
	if v := os.Getenv("TW2A_USAGE_DIR"); v != "" {
		c.UsageDir = v
	}
	if v := os.Getenv("TW2A_DEFAULT_MODEL"); v != "" {
		c.DefaultModel = v
	}
	if v := os.Getenv("TW2A_WORK_MODE"); v != "" {
		c.WorkMode = v
	}
	if v := os.Getenv("TW2A_WORK_HOST"); v != "" {
		c.WorkHost = v
	}
	if v := os.Getenv("TW2A_WORK_BRIDGE_URL"); v != "" {
		c.WorkBridgeURL = v
	}
	if v := os.Getenv("TW2A_WORK_BRIDGE_TOKEN"); v != "" {
		c.WorkBridgeToken = v
	}
	if v := os.Getenv("TW2A_PLAN_CREDIT"); v != "" {
		c.Cooldown.PlanCredit = v
	}
	if v := os.Getenv("TW2A_SOFT_RATE"); v != "" {
		c.Cooldown.SoftRate = v
	}
	if v := os.Getenv("TW2A_ERR_THRESHOLD"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			c.Cooldown.ErrThresh = n
		}
	}
	if v := os.Getenv("TW2A_ERR_COOLDOWN"); v != "" {
		c.Cooldown.ErrCooldown = v
	}
	if v := os.Getenv("TW2A_CHECKIN_HOUR"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			c.Schedule.CheckinHour = n
		}
	}
	if v := os.Getenv("TW2A_REFRESH_SKEW"); v != "" {
		c.Schedule.RefreshSkew = v
	}
	if v := os.Getenv("TW2A_USAGE_RETENTION_DAYS"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n >= 0 {
			c.UsageRetentionDays = n
		}
	}
	if v := os.Getenv("TW2A_TIMEOUT_SECONDS"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			c.Upstream.TimeoutSeconds = n
		}
	}
}

func (c *Config) normalize() error {
	var err error
	if c.PlanCreditDur, err = time.ParseDuration(c.Cooldown.PlanCredit); err != nil {
		return fmt.Errorf("cooldown.plan_credit: %w", err)
	}
	if c.SoftRateDur, err = time.ParseDuration(c.Cooldown.SoftRate); err != nil {
		return fmt.Errorf("cooldown.soft_rate: %w", err)
	}
	if c.ErrCooldownDur, err = time.ParseDuration(c.Cooldown.ErrCooldown); err != nil {
		return fmt.Errorf("cooldown.err_cooldown: %w", err)
	}
	// refresh_skew：合法值直接用；留空/非法/非正数一律回落默认 72h（不因配置笔误启不来）。
	if s := strings.TrimSpace(c.Schedule.RefreshSkew); s != "" {
		if d, derr := time.ParseDuration(s); derr == nil && d > 0 {
			c.RefreshSkewDur = d
		}
	}
	if c.RefreshSkewDur <= 0 {
		c.RefreshSkewDur = 72 * time.Hour
	}
	if c.Cooldown.ErrThresh <= 0 {
		c.Cooldown.ErrThresh = 3
	}
	if c.Upstream.TimeoutSeconds <= 0 {
		c.Upstream.TimeoutSeconds = 120
	}
	// KeepaliveSeconds 未设置（0）→ 默认 15s 开启心跳；负数 → 显式关闭。
	if c.Upstream.KeepaliveSeconds < 0 {
		c.Upstream.KeepaliveSeconds = 0
	} else if c.Upstream.KeepaliveSeconds == 0 {
		c.Upstream.KeepaliveSeconds = 15
	}
	if c.DefaultModel == "" {
		c.DefaultModel = "glm-5.2"
	}
	c.WorkMode = strings.ToLower(strings.TrimSpace(c.WorkMode))
	if c.WorkMode == "" {
		c.WorkMode = "auto"
	}
	if c.Listen == "" {
		c.Listen = ":7864"
	}
	if !strings.HasPrefix(c.Listen, ":") && !strings.Contains(c.Listen, ":") {
		c.Listen = ":" + c.Listen
	}
	// CallbackPort：空/未设 → 默认 18080；显式 "0" → 不起回调 server（纯手动粘贴模式）
	if c.CallbackPort == "" {
		c.CallbackPort = "18080"
	}
	return nil
}
