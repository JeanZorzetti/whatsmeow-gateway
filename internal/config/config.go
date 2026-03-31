package config

import (
	"os"
	"strings"
)

type Config struct {
	Port             string
	APIKey           string
	DatabaseURL      string
	CRMWebhookURL    string
	CRMWebhookSecret string
	LogLevel         string
}

func Load() *Config {
	return &Config{
		Port:             getEnv("PORT", "8090"),
		APIKey:           getEnv("API_KEY", ""),
		DatabaseURL:      getEnv("DATABASE_URL", "postgres://postgres:postgres@localhost:5432/whatsmeow?sslmode=disable"),
		CRMWebhookURL:    getEnv("CRM_WEBHOOK_URL", ""),
		CRMWebhookSecret: getEnv("CRM_WEBHOOK_SECRET", ""),
		LogLevel:         strings.ToLower(getEnv("LOG_LEVEL", "info")),
	}
}

func getEnv(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}
