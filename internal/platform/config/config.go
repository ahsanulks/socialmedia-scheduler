package config

import (
	"fmt"
	"os"
	"strconv"
	"time"
)

type Threads struct {
	Enabled     bool
	UserID      string
	AccessToken string
	BaseURL     string
	DailyLimit  int
}

type Facebook struct {
	Enabled   bool
	PageID    string
	PageToken string
	BaseURL   string
}

type Instagram struct {
	Enabled    bool
	UserID     string
	Token      string
	BaseURL    string
	DailyLimit int
}

type Config struct {
	HTTPAddr        string
	DatabasePath    string
	APIToken        string
	DefaultPlatform string
	SchedulerTick   time.Duration
	SchedulerBatch  int
	Lease           time.Duration
	MaxAttempts     int
	LogLevel        string
	Threads         Threads
	Facebook        Facebook
	Instagram       Instagram
	FakeEnabled     bool
	FakePlatform    string
}

func Load() (Config, error) {
	cfg := Config{
		HTTPAddr:        env("HTTP_ADDR", ":8080"),
		DatabasePath:    env("DATABASE_PATH", "./data/scheduler.db"),
		APIToken:        os.Getenv("API_TOKEN"),
		DefaultPlatform: env("DEFAULT_PLATFORM", "threads"),
		SchedulerTick:   envDuration("SCHEDULER_TICK", 5*time.Second),
		SchedulerBatch:  envInt("SCHEDULER_BATCH", 10),
		Lease:           envDuration("PUBLISH_LEASE", 2*time.Minute),
		MaxAttempts:     envInt("MAX_PUBLISH_ATTEMPTS", 5),
		LogLevel:        env("LOG_LEVEL", "info"),
		Threads: Threads{
			Enabled:     envBool("THREADS_ENABLED", false),
			UserID:      os.Getenv("THREADS_USER_ID"),
			AccessToken: os.Getenv("THREADS_ACCESS_TOKEN"),
			BaseURL:     env("THREADS_API_BASE", "https://graph.threads.net/v1.0"),
			DailyLimit:  envInt("THREADS_DAILY_LIMIT", 240),
		},
		Facebook: Facebook{
			Enabled:   envBool("FACEBOOK_ENABLED", false),
			PageID:    os.Getenv("FACEBOOK_PAGE_ID"),
			PageToken: os.Getenv("FACEBOOK_PAGE_TOKEN"),
			BaseURL:   env("FACEBOOK_API_BASE", "https://graph.facebook.com"),
		},
		Instagram: Instagram{
			Enabled:    envBool("INSTAGRAM_ENABLED", false),
			UserID:     os.Getenv("INSTAGRAM_USER_ID"),
			Token:      os.Getenv("INSTAGRAM_ACCESS_TOKEN"),
			BaseURL:    env("INSTAGRAM_API_BASE", "https://graph.facebook.com"),
			DailyLimit: envInt("INSTAGRAM_DAILY_LIMIT", 90),
		},
		FakeEnabled:  envBool("FAKE_PLATFORM_ENABLED", false),
		FakePlatform: env("FAKE_PLATFORM_NAME", "fake"),
	}
	if cfg.APIToken == "" {
		return Config{}, fmt.Errorf("API_TOKEN is required")
	}
	if cfg.SchedulerTick <= 0 {
		return Config{}, fmt.Errorf("SCHEDULER_TICK must be positive")
	}
	if cfg.SchedulerBatch <= 0 {
		return Config{}, fmt.Errorf("SCHEDULER_BATCH must be positive")
	}
	return cfg, nil
}

func env(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

func envInt(key string, fallback int) int {
	if v := os.Getenv(key); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			return n
		}
	}
	return fallback
}

func envBool(key string, fallback bool) bool {
	if v := os.Getenv(key); v != "" {
		if b, err := strconv.ParseBool(v); err == nil {
			return b
		}
	}
	return fallback
}

func envDuration(key string, fallback time.Duration) time.Duration {
	if v := os.Getenv(key); v != "" {
		if d, err := time.ParseDuration(v); err == nil {
			return d
		}
	}
	return fallback
}
