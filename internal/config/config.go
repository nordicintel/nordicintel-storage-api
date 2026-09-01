package config

import (
	"errors"
	"fmt"
	"log/slog"
	"os"
	"strconv"
	"time"
)

const (
	SchemaMaxCells = int64(1_000_000)
	MinTokenBytes  = 32
)

type Config struct {
	DatabaseURL     string
	ReadWriteToken  string
	ReadOnlyToken   string
	Port            int
	MaxRequestBytes int64
	MaxCells        int64
	DBMaxConns      int32
	RequestTimeout  time.Duration
	DBTimeout       time.Duration
	ShutdownTimeout time.Duration
	LogLevel        slog.Level
}

func Load() (Config, error) {
	c := Config{
		DatabaseURL:     os.Getenv("DATABASE_URL"),
		ReadWriteToken:  os.Getenv("API_READ_WRITE_TOKEN"),
		ReadOnlyToken:   os.Getenv("API_READ_ONLY_TOKEN"),
		Port:            8080,
		MaxRequestBytes: 134_217_728,
		MaxCells:        SchemaMaxCells,
		DBMaxConns:      4,
		RequestTimeout:  5 * time.Minute,
		DBTimeout:       4*time.Minute + 30*time.Second,
		ShutdownTimeout: 30 * time.Second,
		LogLevel:        slog.LevelInfo,
	}

	var err error
	if c.Port, err = envInt("PORT", c.Port); err != nil {
		return Config{}, err
	}
	if c.MaxRequestBytes, err = envInt64("MAX_REQUEST_BYTES", c.MaxRequestBytes); err != nil {
		return Config{}, err
	}
	if c.MaxCells, err = envInt64("MAX_CELLS", c.MaxCells); err != nil {
		return Config{}, err
	}
	if c.DBMaxConns, err = envInt32("DB_MAX_CONNS", c.DBMaxConns); err != nil {
		return Config{}, err
	}
	if c.RequestTimeout, err = envDuration("REQUEST_TIMEOUT", c.RequestTimeout); err != nil {
		return Config{}, err
	}
	if c.DBTimeout, err = envDuration("DB_TIMEOUT", c.DBTimeout); err != nil {
		return Config{}, err
	}
	if c.ShutdownTimeout, err = envDuration("SHUTDOWN_TIMEOUT", c.ShutdownTimeout); err != nil {
		return Config{}, err
	}
	if c.LogLevel, err = envLogLevel("LOG_LEVEL", c.LogLevel); err != nil {
		return Config{}, err
	}
	if err := c.Validate(); err != nil {
		return Config{}, err
	}
	return c, nil
}

func (c Config) Validate() error {
	switch {
	case c.DatabaseURL == "":
		return errors.New("DATABASE_URL is required")
	case len([]byte(c.ReadWriteToken)) < MinTokenBytes:
		return fmt.Errorf("API_READ_WRITE_TOKEN must contain at least %d UTF-8 bytes", MinTokenBytes)
	case len([]byte(c.ReadOnlyToken)) < MinTokenBytes:
		return fmt.Errorf("API_READ_ONLY_TOKEN must contain at least %d UTF-8 bytes", MinTokenBytes)
	case c.ReadWriteToken == c.ReadOnlyToken:
		return errors.New("API_READ_WRITE_TOKEN and API_READ_ONLY_TOKEN must differ")
	case c.Port < 1 || c.Port > 65535:
		return errors.New("PORT must be between 1 and 65535")
	case c.MaxRequestBytes < 1:
		return errors.New("MAX_REQUEST_BYTES must be positive")
	case c.MaxCells < 1 || c.MaxCells > SchemaMaxCells:
		return fmt.Errorf("MAX_CELLS must be between 1 and %d", SchemaMaxCells)
	case c.DBMaxConns < 1:
		return errors.New("DB_MAX_CONNS must be positive")
	case c.RequestTimeout <= 0:
		return errors.New("REQUEST_TIMEOUT must be positive")
	case c.DBTimeout <= 0 || c.DBTimeout >= c.RequestTimeout:
		return errors.New("DB_TIMEOUT must be positive and shorter than REQUEST_TIMEOUT")
	case c.ShutdownTimeout <= 0:
		return errors.New("SHUTDOWN_TIMEOUT must be positive")
	}
	return nil
}

func envInt(name string, fallback int) (int, error) {
	v := os.Getenv(name)
	if v == "" {
		return fallback, nil
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		return 0, fmt.Errorf("%s must be an integer: %w", name, err)
	}
	return n, nil
}

func envInt64(name string, fallback int64) (int64, error) {
	v := os.Getenv(name)
	if v == "" {
		return fallback, nil
	}
	n, err := strconv.ParseInt(v, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("%s must be an integer: %w", name, err)
	}
	return n, nil
}

func envInt32(name string, fallback int32) (int32, error) {
	n, err := envInt64(name, int64(fallback))
	if err != nil {
		return 0, err
	}
	if n > int64(^uint32(0)>>1) || n < 0 {
		return 0, fmt.Errorf("%s is outside the supported range", name)
	}
	return int32(n), nil
}

func envDuration(name string, fallback time.Duration) (time.Duration, error) {
	v := os.Getenv(name)
	if v == "" {
		return fallback, nil
	}
	d, err := time.ParseDuration(v)
	if err != nil {
		return 0, fmt.Errorf("%s must be a Go duration: %w", name, err)
	}
	return d, nil
}

func envLogLevel(name string, fallback slog.Level) (slog.Level, error) {
	v := os.Getenv(name)
	if v == "" {
		return fallback, nil
	}
	switch v {
	case "debug":
		return slog.LevelDebug, nil
	case "info":
		return slog.LevelInfo, nil
	case "warn":
		return slog.LevelWarn, nil
	case "error":
		return slog.LevelError, nil
	default:
		return 0, errors.New("LOG_LEVEL must be debug, info, warn, or error")
	}
}
