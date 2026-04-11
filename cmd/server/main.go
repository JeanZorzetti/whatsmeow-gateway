package main

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/joho/godotenv"

	"github.com/JeanZorzetti/whatsmeow-gateway/internal/api"
	"github.com/JeanZorzetti/whatsmeow-gateway/internal/config"
	"github.com/JeanZorzetti/whatsmeow-gateway/internal/store"
	"github.com/JeanZorzetti/whatsmeow-gateway/internal/webhook"
	"github.com/JeanZorzetti/whatsmeow-gateway/internal/whatsapp"
)

func main() {
	// Register signal handler FIRST — before any init — so we never miss a SIGTERM.
	// EasyPanel sends SIGTERM ~3s into startup during rolling deploys; buffered
	// channel ensures the signal is queued even if we haven't reached the wait yet.
	quit := make(chan os.Signal, 1)
	signal.Notify(quit, syscall.SIGINT, syscall.SIGTERM)
	startupTime := time.Now()

	godotenv.Load()
	cfg := config.Load()

	// Setup logger
	level := slog.LevelInfo
	switch cfg.LogLevel {
	case "debug":
		level = slog.LevelDebug
	case "warn":
		level = slog.LevelWarn
	case "error":
		level = slog.LevelError
	}
	slog.SetDefault(slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: level})))

	slog.Info("starting whatsmeow gateway", "port", cfg.Port)

	gin.SetMode(gin.ReleaseMode)
	r := gin.New()
	r.Use(gin.Recovery())

	// ── Startup health probe ────────────────────────────────────────────────
	// The HTTP server binds the port immediately so EasyPanel's health check
	// never kills the process while DB / WhatsApp sessions are still loading.
	// Once ready, /health returns "ok"; until then it returns "starting" (still 200).
	var ready atomic.Bool
	r.GET("/health", func(c *gin.Context) {
		if ready.Load() {
			c.JSON(http.StatusOK, gin.H{"status": "ok"})
		} else {
			c.JSON(http.StatusOK, gin.H{"status": "starting"})
		}
	})

	srv := &http.Server{
		Addr:    ":" + cfg.Port,
		Handler: r,
	}
	go func() {
		slog.Info("server listening", "port", cfg.Port)
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			slog.Error("server error", "error", err)
			os.Exit(1)
		}
	}()

	// ── Heavy initialisation (DB + Redis + WhatsApp) ───────────────────────
	db, err := store.Connect(cfg.DatabaseURL)
	if err != nil {
		slog.Error("failed to connect to database", "error", err)
		os.Exit(1)
	}
	defer db.Close()

	wh := webhook.NewClient(cfg.CRMWebhookURL, cfg.CRMWebhookSecret)
	wh.SetDeadLetterHandler(func(evt webhook.Event, lastErr error) {
		payload, _ := json.Marshal(evt)
		if err := db.InsertDeadLetter(evt.Type, evt.InstanceID, payload, lastErr.Error()); err != nil {
			slog.Error("failed to insert dead letter", "error", err)
		}
	})

	redisClient := store.NewRedisClient(cfg.RedisURL)

	manager := whatsapp.NewManager(db.Container, db, wh, redisClient)
	manager.RestoreInstances()

	healthCtx, cancelHealth := context.WithCancel(context.Background())
	defer cancelHealth()
	manager.StartHealthcheck(healthCtx)

	// ── Register full routes and mark ready ───────────────────────────────
	handler := api.NewHandler(manager, db, cfg.MaxInstancesPerOrg)
	handler.RegisterRoutes(r, cfg.APIKey)
	ready.Store(true)

	slog.Info("gateway ready")

	// ── Graceful shutdown: drain signals, ignore any that arrived within
	// the first 15s of startup (EasyPanel rolling-deploy race condition).
	const gracePeriod = 15 * time.Second
	for {
		sig := <-quit
		elapsed := time.Since(startupTime)
		if elapsed >= gracePeriod {
			slog.Info("received signal, shutting down", "signal", sig)
			break
		}
		slog.Warn("ignoring signal during startup grace period", "signal", sig,
			"elapsed_ms", elapsed.Milliseconds(),
			"grace_ms", gracePeriod.Milliseconds())
	}

	slog.Info("shutting down gracefully...")

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if err := srv.Shutdown(ctx); err != nil {
		slog.Error("http server shutdown error", "error", err)
	}

	manager.DisconnectAll()
	wh.Shutdown()

	if redisClient != nil {
		redisClient.Close()
	}

	slog.Info("shutdown complete")
}
