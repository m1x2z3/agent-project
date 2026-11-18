package main

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"agent-project/internal/config"
	"agent-project/internal/httpapi"
	"agent-project/internal/llm"
)

const shutdownTimeout = 10 * time.Second

const (
	readHeaderTimeout = 5 * time.Second
	readTimeout       = 10 * time.Second
	idleTimeout       = 60 * time.Second
	writeTimeoutGrace = 30 * time.Second
)

func main() {
	cfg, err := config.Load()
	if err != nil {
		slog.Error("load configuration failed", "error", err)
		os.Exit(1)
	}

	logger := newLogger(cfg.LogLevel)
	client := llm.NewClient(cfg, logger)
	server := &http.Server{
		Addr:              cfg.HTTPAddr,
		Handler:           httpapi.NewHandler(client, logger),
		ReadHeaderTimeout: readHeaderTimeout,
		ReadTimeout:       readTimeout,
		IdleTimeout:       idleTimeout,
		// 流式响应的最大写入时间基于模型超时，再留出网络和收尾缓冲。
		WriteTimeout: cfg.LLMTimeout + writeTimeoutGrace,
	}

	runServer(server, logger)
}

// runServer 启动 HTTP 服务，并在收到退出信号后等待请求优雅完成。
func runServer(server *http.Server, logger *slog.Logger) {
	serverErr := make(chan error, 1)
	go func() {
		logger.Info("HTTP server starting", "addr", server.Addr)
		serverErr <- server.ListenAndServe()
	}()

	signalCtx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	select {
	case err := <-serverErr:
		if !errors.Is(err, http.ErrServerClosed) {
			logger.Error("HTTP server stopped unexpectedly", "error", err)
		}
	case <-signalCtx.Done():
		logger.Info("shutdown signal received")
		shutdownCtx, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
		defer cancel()
		if err := server.Shutdown(shutdownCtx); err != nil {
			logger.Error("HTTP server graceful shutdown failed", "error", err)
			return
		}
		logger.Info("HTTP server stopped")
	}
}

// newLogger 根据配置创建结构化文本日志；未知级别回退到 info。
func newLogger(level string) *slog.Logger {
	var slogLevel slog.Level
	switch level {
	case "debug":
		slogLevel = slog.LevelDebug
	case "warn":
		slogLevel = slog.LevelWarn
	case "error":
		slogLevel = slog.LevelError
	default:
		slogLevel = slog.LevelInfo
	}
	return slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: slogLevel}))
}
