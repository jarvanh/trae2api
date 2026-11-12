// config.go 加载 JSON 配置 + TW2A_* 环境变量覆盖。
// APIKey 只从环境变量 TW2A_API_KEY 读取（SPEC §0 脱敏纪律：key 走 env，不落盘 git）。
package main

import (
	"encoding/json"
	"fmt"
	"net"
	"net/netip"
	"os"
	"strconv"
	"strings"
	"time"
)

// Config 顶层配置。
type Config struct {
	Listen          string `json:"listen"`            // "127.0.0.1:7864"（默认仅回环）
	CallbackPort    string `json:"callback_port"`     // "18080"（TRAE 登录回调监听端口，0 = 不起）
	CallbackListen  string `json:"callback_listen"`   // 可选 "host:port"；空 = 127.0.0.1:<callback_port>
	APIKey          string `json:"-"`                 // 只读 env TW2A_API_KEY（不读 json）
	AuthDir         string `json:"auth_dir"`          // "./auths"
	UsageDir        string `json:"usage_dir"`         // "./data"（用量明细落盘目录，空 = 不落盘）
	StateFile       string `json:"state_file"`        // "./data/state.json"
	DefaultModel    string `json:"default_model"`     // "glm-5.2"
	WorkMode        string `json:"work_mode"`         // "auto" | "native" | "bridge" | "disabled" (默认 auto)
	WorkHost        string `json:"work_host"`         // 官方 Work 专属端点 (默认 https://api5-normal.mchost.guru)
	WorkBridgeURL   string `json:"work_bridge_url"`   // "http://host-gateway:7865" (本地 Work 积分桥接端点)
	WorkBridgeToken string `json:"work_bridge_token"` // 访问 WorkBridge 的 Bearer token（必须显式配置，无内置默认值）

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
		// Function 上游 function 通道。留空 = 用 upstream.Function 默认值（solo_agent，
		// 带 __max 档 = 1M 窗口）。显式填 "solo_work_lite" 可回滚到旧通道
		// （无 max 档，glm-5.3 只有 200K/168K/32K）。
		Function string `json:"function"`
		// MaxTierMode 裸名请求档位策略："auto"（默认，目录含 __max 档即升级到
		// 1M 窗口）| "dev"（全保持 dev 档，回滚开关）。显式 <model>__max/__dev
		// 点名不受此开关限制。
		MaxTierMode string `json:"max_tier_mode"`
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
		// 默认只监听回环：避免空 APIKey 时把管理面板/代理裸奔到局域网（T2）。
		Listen:          "127.0.0.1:7864",
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
	if v := os.Getenv("TW2A_CALLBACK_LISTEN"); v != "" {
		c.CallbackListen = v
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
	if v := os.Getenv("TW2A_FUNCTION"); v != "" {
		c.Upstream.Function = strings.TrimSpace(v)
	}
	if v := os.Getenv("TW2A_MAX_TIER_MODE"); v != "" {
		c.Upstream.MaxTierMode = strings.TrimSpace(v)
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
		c.Listen = "127.0.0.1:7864"
	}
	// 只写了端口（如 "7864"）时按安全默认绑回环；要暴露到局域网请显式写
	// "0.0.0.0:7864" / ":7864" 并配置 TW2A_API_KEY。
	if !strings.Contains(c.Listen, ":") {
		c.Listen = "127.0.0.1:" + c.Listen
	}
	// 空 APIKey + 非回环监听 = 管理面板/代理无鉴权暴露到网络，必须拒绝启动。
	// 显式 ":7864" / "0.0.0.0:7864" 都算非回环（host 为空 = 全部网卡）。
	if c.APIKey == "" && !isLoopbackListen(c.Listen) {
		return fmt.Errorf("refusing to start: listen %q is not loopback while TW2A_API_KEY is empty; "+
			"set TW2A_API_KEY or listen on 127.0.0.1 (e.g. TW2A_LISTEN=127.0.0.1:7864)", c.Listen)
	}
	// CallbackPort：空/未设 → 默认 18080；显式 "0" → 不起回调 server（纯手动粘贴模式）。
	// 端口必须是纯数字，避免拼进 auth_callback_url 时注入额外 query/path。
	if c.CallbackPort == "" {
		c.CallbackPort = "18080"
	}
	if c.CallbackPort != "0" {
		if n, err := strconv.Atoi(c.CallbackPort); err != nil || n < 1 || n > 65535 {
			return fmt.Errorf("callback_port %q: must be 0 or a valid TCP port (1-65535)", c.CallbackPort)
		}
	}
	// Work 桥接鉴权：删除历史硬编码默认 token（T12）。显式 bridge 模式没有
	// token 即拒绝启动，避免带着一个「谁都知道的默认口令」连到桥接端。
	if c.WorkMode == "bridge" && strings.TrimSpace(c.WorkBridgeToken) == "" {
		return fmt.Errorf("work_bridge_token / TW2A_WORK_BRIDGE_TOKEN is required when work_mode=bridge " +
			"(generate one with e.g. `openssl rand -hex 16`)")
	}
	return nil
}

// CallbackAddr 返回 TRAE 回调 server 的监听地址。
// 默认只监听回环（浏览器在本机跳转 127.0.0.1:18080）；容器部署可显式
// 设置 callback_listen=0.0.0.0:18080，并由 docker-compose 在宿主机侧
// 绑定 127.0.0.1:18080 限制暴露面（T2/T8）。
func (c *Config) CallbackAddr() string {
	if c.CallbackListen != "" {
		return c.CallbackListen
	}
	return net.JoinHostPort("127.0.0.1", c.CallbackPort)
}

// isLoopbackListen 判断监听地址的 host 是否仅回环。
// host 为空（":7864"）代表全部网卡，不算回环；localhost / 127.0.0.0/8 / ::1 算。
func isLoopbackListen(addr string) bool {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		// 没有端口的裸 host/port 已在上面补过冒号；这里按不安全处理。
		return false
	}
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip, err := netip.ParseAddr(host)
	if err != nil {
		return false
	}
	return ip.IsLoopback()
}
