// Command server is the relaypool gateway: an OpenAI-compatible front door in
// front of one or more WorkBuddy / Qoder / Trae accounts.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/relaypool/relaypool/internal/api"
	"github.com/relaypool/relaypool/internal/buildinfo"
	"github.com/relaypool/relaypool/internal/config"
	"github.com/relaypool/relaypool/internal/store"
)

func main() {
	if err := run(); err != nil {
		log.Fatalf("relaypool: %v", err)
	}
}

func run() error {
	var (
		configPath  = flag.String("config", "", "path to config.json (optional; env overrides apply on top)")
		showVersion = flag.Bool("version", false, "print version information and exit")
		showKey     = flag.Bool("show-api-key", false, "由本机管理员明确读取初始密钥后退出，请勿分享输出")
	)
	flag.Parse()

	if *showVersion {
		fmt.Printf("relaypool %s (commit %s, built %s, %s)\n",
			buildinfo.Version, buildinfo.Commit, buildinfo.BuiltAt, buildinfo.GoVersion())
		return nil
	}

	cfg, err := config.Load(*configPath)
	if err != nil {
		return fmt.Errorf("load config: %w", err)
	}

	// The shutdown context is cancelled by SIGINT/SIGTERM. Everything that
	// must stop cleanly hangs off it.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	dbPath := filepath.Join(cfg.DataDir, "relaypool.db")
	key, err := credentialKey(cfg.DataDir)
	if err != nil {
		return err
	}
	st, err := store.OpenWithCredentialKey(ctx, dbPath, key)
	if err != nil {
		return fmt.Errorf("open store: %w", err)
	}
	defer st.Close()

	schemaVersion, err := st.SchemaVersion(ctx)
	if err != nil {
		return fmt.Errorf("read schema version: %w", err)
	}

	apiKey, generated, err := st.EnsureBootstrapAPIKey(ctx, cfg.APIKey)
	if err != nil {
		return fmt.Errorf("bootstrap api key: %w", err)
	}
	if *showKey {
		valid, err := st.VerifyAPIKey(ctx, apiKey)
		if err != nil || !valid {
			return fmt.Errorf("初始密钥已撤销或不可用，请使用现有管理密钥")
		}
		fmt.Println(apiKey)
		return nil
	}

	log.Printf("relaypool %s (commit %s, %s)", buildinfo.Version, buildinfo.Commit, buildinfo.GoVersion())
	log.Printf("store       : %s (schema v%d)", st.Path(), schemaVersion)
	log.Printf("凭证存储    : 已启用本地加密，请同时备份数据库与独立加密密钥")
	if generated {
		// 启动日志可能被采集或分享，明文只能经本机显式命令读取。
		log.Printf("管理密钥    : 已生成并加密保存；本机管理员可执行 relaypool -show-api-key 读取")
	} else {
		log.Printf("管理密钥    : 已配置，启动日志不显示密钥")
	}

	// Resolve the exposure level into a concrete bind address. This happens
	// after validation so the LAN interface probe (which touches the network
	// stack) only runs on a configuration that is otherwise sound.
	listenAddr, err := cfg.ListenAddress()
	if err != nil {
		return fmt.Errorf("resolve listen address: %w", err)
	}
	// 页面显示已解析的实际监听地址，避免把自动选择的地址当作未生效配置。
	cfg.Listen = listenAddr
	log.Printf("access      : %s -> %s", accessModeLabel(cfg.Access.Mode), listenAddr)
	log.Printf("listen      : http://%s", listenAddr)

	srv := api.New(api.Options{
		Config: cfg,
		Store:  st,
		APIKey: apiKey,
	})
	defer srv.Close()

	httpServer := &http.Server{
		Addr:    listenAddr,
		Handler: srv.Handler(),
		// 不设置整个响应的固定时限；流式处理单独限制每次阻塞写入为 15 秒。
		// 上游读取超时和下游写入期限共同管理流，正常长对话可持续输出。
		ReadHeaderTimeout: 15 * time.Second,
		// 仅限制请求头和正文的读取时间，不限制后续流式回复。
		ReadTimeout:    30 * time.Second,
		MaxHeaderBytes: 32 << 10,
		IdleTimeout:    120 * time.Second,
		BaseContext:    func(net.Listener) context.Context { return ctx },
	}

	errCh := make(chan error, 1)
	go func() {
		log.Printf("relaypool listening on http://%s", listenAddr)
		if err := httpServer.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- fmt.Errorf("serve: %w", err)
		}
	}()

	// Background jobs start after the listener so the API is reachable while
	// the first catalogue refresh runs. A slow upstream must not delay the
	// gateway becoming available.
	if sched := srv.Scheduler(); sched != nil {
		sched.Start(ctx)
	}

	select {
	case err := <-errCh:
		return err
	case <-ctx.Done():
		log.Printf("shutdown signal received, draining connections")
	}

	// 先停止接收并排空请求，再由 defer 关闭号池写盘，保留最后请求的健康状态。
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if err := httpServer.Shutdown(shutdownCtx); err != nil {
		return fmt.Errorf("shutdown: %w", err)
	}
	log.Printf("relaypool stopped")
	return nil
}

// accessModeLabel renders the exposure level for the boot log.
func accessModeLabel(mode string) string { return "loopback" }
