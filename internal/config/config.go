package config

import (
	"errors"
	"fmt"
	"os"
	"strconv"
	"time"
)

const (
	defaultPort            = "8080"
	defaultMaxRequestBytes = int64(128 << 20)
	defaultMaxCells        = int64(1_000_000)
	defaultMaxDBConns      = int32(4)
	defaultDBTimeout       = 25 * time.Second
)

type Config struct {
	DatabaseURL     string
	BearerToken     string
	Port            string
	MaxRequestBytes int64
	MaxCells        int64
	MaxDBConns      int32
	DBTimeout       time.Duration
}

func Load() (Config, error) {
	cfg := Config{
		DatabaseURL:     os.Getenv("DATABASE_URL"),
		BearerToken:     os.Getenv("API_BEARER_TOKEN"),
		Port:            getenv("PORT", defaultPort),
		MaxRequestBytes: defaultMaxRequestBytes,
		MaxCells:        defaultMaxCells,
		MaxDBConns:      defaultMaxDBConns,
		DBTimeout:       defaultDBTimeout,
	}

	if cfg.DatabaseURL == "" {
		return Config{}, errors.New("DATABASE_URL is required")
	}
	if cfg.BearerToken == "" {
		return Config{}, errors.New("API_BEARER_TOKEN is required")
	}

	var err error
	if cfg.MaxRequestBytes, err = int64Env("MAX_REQUEST_BYTES", cfg.MaxRequestBytes); err != nil {
		return Config{}, err
	}
	if cfg.MaxCells, err = int64Env("MAX_CELLS", cfg.MaxCells); err != nil {
		return Config{}, err
	}
	maxConns, err := int64Env("DB_MAX_CONNS", int64(cfg.MaxDBConns))
	if err != nil {
		return Config{}, err
	}
	if maxConns > int64(^uint32(0)>>1) {
		return Config{}, errors.New("DB_MAX_CONNS is too large")
	}
	cfg.MaxDBConns = int32(maxConns)

	if value := os.Getenv("DB_TIMEOUT"); value != "" {
		cfg.DBTimeout, err = time.ParseDuration(value)
		if err != nil || cfg.DBTimeout <= 0 {
			return Config{}, fmt.Errorf("DB_TIMEOUT must be a positive duration: %q", value)
		}
	}

	return cfg, nil
}

func getenv(name, fallback string) string {
	if value := os.Getenv(name); value != "" {
		return value
	}
	return fallback
}

func int64Env(name string, fallback int64) (int64, error) {
	value := os.Getenv(name)
	if value == "" {
		return fallback, nil
	}
	parsed, err := strconv.ParseInt(value, 10, 64)
	if err != nil || parsed <= 0 {
		return 0, fmt.Errorf("%s must be a positive integer: %q", name, value)
	}
	return parsed, nil
}
