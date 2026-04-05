package main

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
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

	// Database
	db, err := store.Connect(cfg.DatabaseURL)
	if err != nil {
		slog.Error("failed to connect to database", "error", err)
		os.Exit(1)
	}
	defer db.Close()

	// Webhook client
	wh := webhook.NewClient(cfg.CRMWebhookURL, cfg.CRMWebhookSecret)

	// Dead-letter handler: persist failed webhooks to DB
	wh.SetDeadLetterHandler(func(evt webhook.Event, lastErr error) {
		payload, _ := json.Marshal(evt)
		if err := db.InsertDeadLetter(evt.Type, evt.InstanceID, payload, lastErr.Error()); err != nil {
			slog.Error("failed to insert dead letter", "error", err)
		}
	})

	// Redis (optional — graceful fallback to direct webhook if unavailable)
	redisClient := store.NewRedisClient(cfg.RedisURL)

	// WhatsApp manager
	manager := whatsapp.NewManager(db.Container, db, wh, redisClient)

	// Restore previously created instances (reconnect existing sessions)
	manager.RestoreInstances()

	// HTTP server
	gin.SetMode(gin.ReleaseMode)
	r := gin.New()
	r.Use(gin.Recovery())

	handler := api.NewHandler(manager, db, cfg.MaxInstancesPerOrg)
	handler.RegisterRoutes(r, cfg.APIKey)

	srv := &http.Server{
		Addr:    ":" + cfg.Port,
		Handler: r,
	}

	// Start server in goroutine
	go func() {
		slog.Info("server listening", "port", cfg.Port)
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			slog.Error("server error", "error", err)
			os.Exit(1)
		}
	}()

	// Graceful shutdown: wait for SIGINT/SIGTERM
	quit := make(chan os.Signal, 1)
	signal.Notify(quit, syscall.SIGINT, syscall.SIGTERM)
	<-quit

	slog.Info("shutting down gracefully...")

	// 1. Stop accepting new HTTP requests, wait up to 15s for in-flight
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if err := srv.Shutdown(ctx); err != nil {
		slog.Error("http server shutdown error", "error", err)
	}

	// 2. Disconnect all WhatsApp clients
	manager.DisconnectAll()

	// 3. Drain webhook retry queue
	wh.Shutdown()

	// 4. Close Redis
	if redisClient != nil {
		redisClient.Close()
	}

	slog.Info("shutdown complete")
}
