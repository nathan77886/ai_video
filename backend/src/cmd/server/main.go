package main

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/nathan77886/ai_video/backend/src/internal/app"
)

func main() {
	logger := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	dataDir := env("DATA_DIR", "data")
	store, err := app.OpenStore(dataDir)
	if err != nil {
		logger.Error("open store", "error", err)
		os.Exit(1)
	}

	paidAllowed := envBool("ALLOW_PAID_GENERATION", false)
	miniMaxKey := strings.TrimSpace(os.Getenv("MINIMAX_API_KEY"))
	miniMax, err := app.NewMiniMaxProvider(env("MINIMAX_BASE_URL", "https://api.minimax.io"), miniMaxKey, paidAllowed)
	if err != nil {
		logger.Error("configure MiniMax", "error", err)
		os.Exit(1)
	}
	providers := map[string]app.VideoProvider{
		"mock":    app.MockProvider{},
		"minimax": miniMax,
	}
	pollInterval := envDuration("POLL_INTERVAL", 10*time.Second)
	worker := app.NewWorker(
		store,
		providers,
		pollInterval,
		envInt64("MAX_DOWNLOAD_BYTES", 2<<30),
		logger,
	)

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	if err := worker.Start(ctx, envInt("WORKER_COUNT", 2)); err != nil {
		logger.Error("start workers", "error", err)
		os.Exit(1)
	}

	handler := app.NewHTTPServer(
		store,
		worker,
		logger,
		envInt64("MAX_UPLOAD_BYTES", 250<<20),
		paidAllowed,
		miniMaxKey != "",
	)
	server := &http.Server{
		Addr:              env("LISTEN_ADDR", "127.0.0.1:8780"),
		Handler:           handler,
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      30 * time.Minute,
		IdleTimeout:       60 * time.Second,
		MaxHeaderBytes:    1 << 20,
	}
	serverErr := make(chan error, 1)
	go func() {
		logger.Info("server started", "address", server.Addr, "paid_generation", paidAllowed)
		serverErr <- server.ListenAndServe()
	}()

	select {
	case <-ctx.Done():
	case err := <-serverErr:
		if !errors.Is(err, http.ErrServerClosed) {
			logger.Error("serve HTTP", "error", err)
		}
		stop()
	}
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if err := server.Shutdown(shutdownCtx); err != nil {
		logger.Error("shutdown HTTP server", "error", err)
	}
	worker.Wait()
}

func env(name, fallback string) string {
	if value := strings.TrimSpace(os.Getenv(name)); value != "" {
		return value
	}
	return fallback
}

func envBool(name string, fallback bool) bool {
	value := strings.TrimSpace(os.Getenv(name))
	if value == "" {
		return fallback
	}
	parsed, err := strconv.ParseBool(value)
	if err != nil {
		return fallback
	}
	return parsed
}

func envInt(name string, fallback int) int {
	value, err := strconv.Atoi(strings.TrimSpace(os.Getenv(name)))
	if err != nil || value < 1 {
		return fallback
	}
	return value
}

func envInt64(name string, fallback int64) int64 {
	value, err := strconv.ParseInt(strings.TrimSpace(os.Getenv(name)), 10, 64)
	if err != nil || value < 1 {
		return fallback
	}
	return value
}

func envDuration(name string, fallback time.Duration) time.Duration {
	value, err := time.ParseDuration(strings.TrimSpace(os.Getenv(name)))
	if err != nil || value <= 0 {
		return fallback
	}
	return value
}
