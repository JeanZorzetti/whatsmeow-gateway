package config

import (
	"os"
	"strconv"
	"strings"
)

type Config struct {
	Port               string
	APIKey             string
	DatabaseURL        string
	CRMWebhookURL      string
	CRMWebhookSecret   string
	LogLevel           string
	MaxInstancesPerOrg int
}

func Load() *Config {
	maxInst, _ := strconv.Atoi(getEnv("MAX_INSTANCES_PER_ORG", "5"))
	if maxInst <= 0 {
		maxInst = 5
	}

	return &Config{
		Port:               getEnv("PORT", "8090"),
		APIKey:             getEnv("API_KEY", ""),
		DatabaseURL:        getEnv("DATABASE_URL", "postgres://postgres:postgres@localhost:5432/whatsmeow?sslmode=disable"),
		CRMWebhookURL:      getEnv("CRM_WEBHOOK_URL", ""),
		CRMWebhookSecret:   getEnv("CRM_WEBHOOK_SECRET", ""),
		LogLevel:           strings.ToLower(getEnv("LOG_LEVEL", "info")),
		MaxInstancesPerOrg: maxInst,
	}
}

func getEnv(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}
