// pageshare：静态页分享服务。单二进制 = 管理 API + 公开分享面 + 内嵌管理 UI。
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"pageshare/internal/config"
	"pageshare/internal/mcpserver"
	"pageshare/internal/server"
	"pageshare/internal/storage"
	"pageshare/internal/store"
)

// version 由构建注入：-ldflags "-X main.version=v1.2.3"（见 scripts/release.sh）。
var version = "dev"

func main() {
	logger := slog.New(slog.NewTextHandler(os.Stderr, nil))
	slog.SetDefault(logger)

	// 子命令：pageshare mcp [-config …] —— 以 stdio MCP server 运行（供 MCP 客户端拉起）
	args := os.Args[1:]
	if len(args) > 0 && (args[0] == "-version" || args[0] == "version") {
		fmt.Println("pageshare", version)
		return
	}
	if len(args) > 0 && args[0] == "mcp" {
		os.Args = append([]string{os.Args[0]}, args[1:]...)
		runMCP(logger)
		return
	}

	cfg, err := config.Load()
	if err != nil {
		fmt.Fprintln(os.Stderr, "配置错误:", err)
		os.Exit(2)
	}

	// 首次部署引导模式：配置为空（非 -dev）时不报错退出，起独立向导服务，
	// 用户走完 /admin/setup 后配置落盘并自动重启进入正常模式。
	if cfg.NeedsSetup {
		runSetupWizard(logger, cfg)
		return
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	st, err := store.Open(cfg.DBPath)
	if err != nil {
		logger.Error("打开元数据库失败", "err", err)
		os.Exit(1)
	}
	defer st.Close()

	var backend storage.Storage
	switch cfg.Storage.Backend {
	case "s3":
		backend, err = storage.NewS3(ctx, cfg.Storage)
		if err != nil {
			logger.Error("初始化 S3 后端失败", "err", err)
			os.Exit(1)
		}
		logger.Info("对象存储后端就绪", "bucket", cfg.Storage.Bucket, "endpoint", cfg.Storage.Endpoint)
	case "disk":
		backend, err = storage.NewDisk(cfg.Storage.Root)
		if err != nil {
			logger.Error("初始化磁盘存储失败", "err", err)
			os.Exit(1)
		}
		logger.Info("磁盘存储后端就绪", "root", cfg.Storage.Root)
	default: // mem
		backend = storage.NewMem()
		logger.Info("使用内存存储（-dev/未配置 s3），重启即丢数据")
	}

	srv := server.New(server.Deps{Cfg: cfg, Store: st, Storage: backend, Logger: logger})
	stopSweep := srv.StartSweeper(ctx, time.Hour)
	defer stopSweep()
	// 页览计数按分钟并入元数据；ctx 取消时先做最后一次刷盘再退出
	stopStats := srv.StartStatsFlusher(ctx, time.Minute)
	defer stopStats()

	httpServer := &http.Server{
		Addr:              cfg.Listen,
		Handler:           srv,
		ReadHeaderTimeout: 10 * time.Second,
	}
	go func() {
		logger.Info("pageshare 开始监听", "addr", cfg.Listen, "public_base_url", cfg.PublicBaseURL)
		if cfg.Dev {
			logger.Info("开发模式管理 token: " + cfg.AuthToken)
		}
		if err := httpServer.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			logger.Error("HTTP 服务退出", "err", err)
			os.Exit(1)
		}
	}()

	<-ctx.Done()
	logger.Info("收到退出信号，开始优雅停机")
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_ = httpServer.Shutdown(shutdownCtx)
	logger.Info("已退出")
}

// runSetupWizard 首次使用引导模式：只暴露 setup 向导 API 与内嵌 SPA 的独立轻量服务，
// 配置落盘后自重启（unix execve）进入正常模式；不支持自重启的平台提示手动重启。
func runSetupWizard(logger *slog.Logger, cfg config.Config) {
	svc := server.NewSetupService(cfg.Listen, cfg.ConfigPath)
	httpServer := &http.Server{
		Addr:              cfg.Listen,
		Handler:           svc,
		ReadHeaderTimeout: 10 * time.Second,
	}
	go func() {
		if err := httpServer.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			logger.Error("引导服务退出", "err", err)
			os.Exit(1)
		}
	}()

	fmt.Printf("═══ pageshare 首次使用引导 ═══\n")
	fmt.Printf("浏览器打开: http://%s/admin/setup\n", listenDisplay(cfg.Listen))
	fmt.Printf("完成向导后配置将写入 %s 并自动重启服务。\n", cfg.ConfigPath)

	// 向导 handler 落盘后已留出响应送达时间，这里直接收尾并重启
	<-svc.Done()
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	_ = httpServer.Shutdown(shutdownCtx)
	if err := restartSelf(); err != nil {
		fmt.Println("配置已写入，请手动重启服务完成初始化:", err)
		os.Exit(0)
	}
}

// listenDisplay 把监听地址转成可点击的 host:port：":8300" → "localhost:8300"。
func listenDisplay(listen string) string {
	if listen == "" {
		return "localhost:8300"
	}
	if strings.HasPrefix(listen, ":") {
		return "localhost" + listen
	}
	return listen
}

// runMCP 以 stdio MCP server 运行：工具直接操作本地元数据与对象存储，
// 无需 HTTP 监听；本地目录路径即工作区路径。
func runMCP(logger *slog.Logger) {
	cfg, err := config.Load()
	if err != nil {
		fmt.Fprintln(os.Stderr, "配置错误:", err)
		os.Exit(2)
	}
	ctx := context.Background()
	st, err := store.Open(cfg.DBPath)
	if err != nil {
		logger.Error("打开元数据库失败", "err", err)
		os.Exit(1)
	}
	defer st.Close()
	var backend storage.Storage
	switch cfg.Storage.Backend {
	case "s3":
		backend, err = storage.NewS3(ctx, cfg.Storage)
		if err != nil {
			logger.Error("初始化 S3 后端失败", "err", err)
			os.Exit(1)
		}
	default:
		backend = storage.NewMem()
		logger.Info("警告：mem 存储下 MCP 发布的内容不会持久化")
	}
	srv := server.New(server.Deps{Cfg: cfg, Store: st, Storage: backend, Logger: logger})
	logger.Info("pageshare MCP (stdio) 就绪")
	if err := mcpserver.RunStdio(ctx, srv, mcpserver.Config{MaxUploadMB: cfg.MaxUploadMB}, mcpserver.MCPVersion()); err != nil {
		logger.Error("MCP 退出", "err", err)
		os.Exit(1)
	}
}
