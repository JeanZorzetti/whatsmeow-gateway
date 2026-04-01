package main

import (
	"log/slog"
	"os"

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

	// WhatsApp manager
	manager := whatsapp.NewManager(db.Container, db, wh)

	// Restore previously created instances (reconnect existing sessions)
	manager.RestoreInstances()

	// HTTP server
	gin.SetMode(gin.ReleaseMode)
	r := gin.New()
	r.Use(gin.Recovery())

	handler := api.NewHandler(manager, db)
	handler.RegisterRoutes(r, cfg.APIKey)

	slog.Info("server listening", "port", cfg.Port)
	if err := r.Run(":" + cfg.Port); err != nil {
		slog.Error("server error", "error", err)
		os.Exit(1)
	}
}
