// main.go trae2api 入口：加载配置 → 构建 pool → 起 HTTP 服务。
package main

import (
	"context"
	"flag"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"trae2api/internal/auth"
	"trae2api/internal/pool"
	"trae2api/internal/scheduler"
	"trae2api/internal/server"
	"trae2api/internal/upstream"
)

func main() {
	cfgPath := flag.String("config", "config.json", "path to config json")
	flag.Parse()

	cfg, err := Load(*cfgPath)
	if err != nil {
		log.Fatalf("load config: %v", err)
	}

	auths, err := auth.LoadDir(cfg.AuthDir)
	if err != nil {
		log.Fatalf("load auths: %v", err)
	}
	log.Printf("loaded %d account(s) from %s", len(auths), cfg.AuthDir)

	p := pool.New(cfg.StateFile)
	p.SyncToDir(auths) // 对齐：剔除 state.json 中已删除 auth 文件的幽灵账号

	up := upstream.New()
	up.HTTP.Timeout = time.Duration(cfg.Upstream.TimeoutSeconds) * time.Second
	// 流式客户端无总超时，仅用首字节兜底（时长由 SSE 流本身决定）。
	if tr, ok := up.StreamHTTP.Transport.(*http.Transport); ok {
		tr.ResponseHeaderTimeout = time.Duration(cfg.Upstream.TimeoutSeconds) * time.Second
	}
	// function 通道：留空沿用默认（solo_agent，带 __max 档 = 1M 窗口）；
	// 显式配置 solo_work_lite 可回滚旧的免费引流通道（最高 200K）。
	if cfg.Upstream.Function != "" {
		up.Function = cfg.Upstream.Function
	}
	log.Printf("upstream function=%s", up.Function)

	workCfg := upstream.DefaultWorkClientConfig()
	if cfg.WorkHost != "" {
		workCfg.Host = cfg.WorkHost
	}
	if cfg.WorkBridgeURL != "" {
		workCfg.BridgeURL = cfg.WorkBridgeURL
	}
	if cfg.WorkBridgeToken != "" {
		workCfg.BridgeToken = cfg.WorkBridgeToken
	}
	if cfg.WorkMode != "" {
		workCfg.Mode = upstream.WorkMode(cfg.WorkMode)
	}
	workClient := upstream.NewWorkClient(workCfg)
	// 桥接鉴权无内置默认口令（T12）：bridge/auto 模式未配 token 时给出明确提示，
	// 避免继续带着历史默认口令连桥接端。
	if workClient.Mode() != upstream.WorkModeDisabled && workClient.Config().BridgeToken == "" {
		log.Printf("warning: TW2A_WORK_BRIDGE_TOKEN is empty — bridge requests will be sent without Authorization; set it when the bridge enforces auth")
	}

	sch := scheduler.New(scheduler.Config{
		Pool:         p,
		Upstream:     up,
		CheckinHour:  cfg.Schedule.CheckinHour,
		RefreshHours: cfg.Schedule.RefreshHours,
		RefreshSkew:  cfg.RefreshSkewDur,
		// 流水归档钩子：UsageRetentionDays<=0 时 ArchiveUsage 内部直接返回 0,0（不启用）
		ArchiveUsage: func() (int, int, error) {
			return server.ArchiveUsage(cfg.UsageDir, cfg.UsageRetentionDays)
		},
	})

	h := server.NewHandler(server.Config{
		Pool:            p,
		Upstream:        up,
		WorkClient:      workClient,
		WorkMode:        upstream.WorkMode(cfg.WorkMode),
		APIKey:          cfg.APIKey,
		AuthDir:         cfg.AuthDir,
		UsageDir:        cfg.UsageDir,
		PlanCooldown:    cfg.PlanCreditDur,
		SoftCooldown:    cfg.SoftRateDur,
		ErrThreshold:    cfg.Cooldown.ErrThresh,
		ErrCooldown:     cfg.ErrCooldownDur,
		DefaultModel:    cfg.DefaultModel,
		MaxTierMode:     cfg.Upstream.MaxTierMode,
		WorkBridgeURL:   cfg.WorkBridgeURL,
		WorkBridgeToken: cfg.WorkBridgeToken,
		Keepalive:       time.Duration(cfg.Upstream.KeepaliveSeconds) * time.Second,
		// token 预刷新窗口与调度器同源：两条刷新路径（定时 / 请求）用同一个 skew，
		// 避免「控制台看着没过期但请求才刷」这类口径不一致。
		RefreshSkew: cfg.RefreshSkewDur,
	})

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	go sch.Run(ctx)
	// 规则①（最早过期优先）数据源保活：启动即探测 + 每小时探测，
	// 配合 nearest_expire 落盘，保证重启后选号规则①立即生效（2026-10-16 事故治本）。
	go sch.RunExpireProbeLoop(ctx, time.Hour)

	// 启动时在后台探测所有可用账号的 work_credits 积分状态
	if workClient.Mode() != upstream.WorkModeDisabled {
		go func() {
			probeCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			for _, a := range auths {
				snap, err := workClient.ProbeCredits(probeCtx, a)
				if err == nil {
					p.SetWorkCredits(a.UID, snap.WorkCredits)
					log.Printf("[WorkPool] account %s (%s) work_credits: %.4f", a.UID, a.Nickname, snap.WorkCredits)
				}
			}
		}()
	}

	srv := &http.Server{
		Addr:              cfg.Listen,
		Handler:           h,
		ReadHeaderTimeout: 30 * time.Second,
	}
	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutdownCtx)
	}()

	// 第二个 http.Server：TRAE 登录回调（默认 127.0.0.1:18080）。
	// 只挂 /authorize 最小路由（CallbackHandler），不把 /admin、/v1 等完整
	// Handler 暴露到回调端口（T8）。cfg.CallbackPort == "0" 时不启动。
	var cbSrv *http.Server
	if cfg.CallbackPort != "" && cfg.CallbackPort != "0" {
		cbAddr := cfg.CallbackAddr()
		cbSrv = &http.Server{
			Addr:              cbAddr,
			Handler:           h.CallbackHandler(),
			ReadHeaderTimeout: 30 * time.Second,
		}
		go func() {
			<-ctx.Done()
			sc, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()
			_ = cbSrv.Shutdown(sc)
		}()
		go func() {
			log.Printf("trae2api callback server on %s (TRAE login /authorize)", cbAddr)
			if err := cbSrv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
				// 端口被占用（login.sh / 旧实例）不致命，降级为手动粘贴模式。
				log.Printf("callback server (%s) failed: %v — web 登录降级为手动粘贴回调链接", cbAddr, err)
			}
		}()
	}

	log.Printf("trae2api listening on %s (api_key=%v)", cfg.Listen, cfg.APIKey != "")
	if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		log.Fatalf("http: %v", err)
	}
	log.Printf("bye")
}
