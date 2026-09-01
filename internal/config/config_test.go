package config

import (
	"log/slog"
	"strings"
	"testing"
	"time"
)

const (
	writeToken = "write-token-with-at-least-32-bytes!!"
	readToken  = "read-token-with-at-least-32-bytes!!!"
)

// setEnvironment installs a complete, valid environment and then applies the
// supplied overrides. An empty override value unsets the variable.
func setEnvironment(t *testing.T, overrides map[string]string) {
	t.Helper()
	base := map[string]string{
		"DATABASE_URL":         "postgres://user:pass@localhost:5432/storage",
		"API_READ_WRITE_TOKEN": writeToken,
		"API_READ_ONLY_TOKEN":  readToken,
	}
	for _, name := range []string{"PORT", "MAX_REQUEST_BYTES", "MAX_CELLS", "DB_MAX_CONNS",
		"REQUEST_TIMEOUT", "DB_TIMEOUT", "SHUTDOWN_TIMEOUT", "LOG_LEVEL"} {
		t.Setenv(name, "")
	}
	for name, value := range base {
		t.Setenv(name, value)
	}
	for name, value := range overrides {
		t.Setenv(name, value)
	}
}

func TestLoadAppliesDocumentedDefaults(t *testing.T) {
	setEnvironment(t, nil)
	got, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	want := Config{
		DatabaseURL:     "postgres://user:pass@localhost:5432/storage",
		ReadWriteToken:  writeToken,
		ReadOnlyToken:   readToken,
		Port:            8080,
		MaxRequestBytes: 134_217_728,
		MaxCells:        1_000_000,
		DBMaxConns:      4,
		RequestTimeout:  5 * time.Minute,
		DBTimeout:       4*time.Minute + 30*time.Second,
		ShutdownTimeout: 30 * time.Second,
		LogLevel:        slog.LevelInfo,
	}
	if got != want {
		t.Fatalf("configuration = %+v, want %+v", got, want)
	}
}

func TestLoadReadsEveryDocumentedOverride(t *testing.T) {
	setEnvironment(t, map[string]string{
		"PORT":              "9000",
		"MAX_REQUEST_BYTES": "1024",
		"MAX_CELLS":         "500",
		"DB_MAX_CONNS":      "12",
		"REQUEST_TIMEOUT":   "2m",
		"DB_TIMEOUT":        "1m",
		"SHUTDOWN_TIMEOUT":  "5s",
		"LOG_LEVEL":         "debug",
	})
	got, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got.Port != 9000 || got.MaxRequestBytes != 1024 || got.MaxCells != 500 || got.DBMaxConns != 12 {
		t.Fatalf("limits = %+v", got)
	}
	if got.RequestTimeout != 2*time.Minute || got.DBTimeout != time.Minute || got.ShutdownTimeout != 5*time.Second {
		t.Fatalf("timeouts = %+v", got)
	}
	if got.LogLevel != slog.LevelDebug {
		t.Fatalf("log level = %v, want debug", got.LogLevel)
	}
}

func TestLoadAcceptsEveryLogLevel(t *testing.T) {
	levels := map[string]slog.Level{
		"debug": slog.LevelDebug,
		"info":  slog.LevelInfo,
		"warn":  slog.LevelWarn,
		"error": slog.LevelError,
	}
	for name, level := range levels {
		setEnvironment(t, map[string]string{"LOG_LEVEL": name})
		got, err := Load()
		if err != nil {
			t.Fatalf("LOG_LEVEL=%s: %v", name, err)
		}
		if got.LogLevel != level {
			t.Fatalf("LOG_LEVEL=%s gave %v", name, got.LogLevel)
		}
	}
}

func TestLoadRejectsInvalidConfiguration(t *testing.T) {
	cases := []struct {
		name      string
		overrides map[string]string
		contains  string
	}{
		{"missing database url", map[string]string{"DATABASE_URL": ""}, "DATABASE_URL"},
		{"missing write token", map[string]string{"API_READ_WRITE_TOKEN": ""}, "API_READ_WRITE_TOKEN"},
		{"missing read token", map[string]string{"API_READ_ONLY_TOKEN": ""}, "API_READ_ONLY_TOKEN"},
		{"short write token", map[string]string{"API_READ_WRITE_TOKEN": strings.Repeat("a", MinTokenBytes-1)}, "API_READ_WRITE_TOKEN"},
		{"short read token", map[string]string{"API_READ_ONLY_TOKEN": strings.Repeat("b", MinTokenBytes-1)}, "API_READ_ONLY_TOKEN"},
		{"equal tokens", map[string]string{"API_READ_ONLY_TOKEN": writeToken}, "must differ"},
		{"port too low", map[string]string{"PORT": "0"}, "PORT"},
		{"port too high", map[string]string{"PORT": "65536"}, "PORT"},
		{"port not an integer", map[string]string{"PORT": "http"}, "PORT"},
		{"zero request bytes", map[string]string{"MAX_REQUEST_BYTES": "0"}, "MAX_REQUEST_BYTES"},
		{"max cells zero", map[string]string{"MAX_CELLS": "0"}, "MAX_CELLS"},
		{"max cells over the schema ceiling", map[string]string{"MAX_CELLS": "1000001"}, "MAX_CELLS"},
		{"pool too small", map[string]string{"DB_MAX_CONNS": "0"}, "DB_MAX_CONNS"},
		{"pool negative", map[string]string{"DB_MAX_CONNS": "-1"}, "DB_MAX_CONNS"},
		{"request timeout not a duration", map[string]string{"REQUEST_TIMEOUT": "5"}, "REQUEST_TIMEOUT"},
		{"request timeout zero", map[string]string{"REQUEST_TIMEOUT": "0s"}, "REQUEST_TIMEOUT"},
		{"database timeout equals request timeout", map[string]string{"DB_TIMEOUT": "5m"}, "DB_TIMEOUT"},
		{"database timeout exceeds request timeout", map[string]string{"DB_TIMEOUT": "6m"}, "DB_TIMEOUT"},
		{"shutdown timeout zero", map[string]string{"SHUTDOWN_TIMEOUT": "0s"}, "SHUTDOWN_TIMEOUT"},
		{"unknown log level", map[string]string{"LOG_LEVEL": "verbose"}, "LOG_LEVEL"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			setEnvironment(t, tc.overrides)
			_, err := Load()
			if err == nil {
				t.Fatal("invalid configuration accepted")
			}
			if !strings.Contains(err.Error(), tc.contains) {
				t.Fatalf("error = %v, want it to mention %q", err, tc.contains)
			}
		})
	}
}

func TestLoadNeverTrimsCredentials(t *testing.T) {
	padded := "  " + writeToken + "  "
	setEnvironment(t, map[string]string{"API_READ_WRITE_TOKEN": padded})
	got, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got.ReadWriteToken != padded {
		t.Fatalf("token = %q, want the untrimmed value %q", got.ReadWriteToken, padded)
	}
}

func TestConfigurationErrorsNeverLeakCredentials(t *testing.T) {
	secret := strings.Repeat("s", MinTokenBytes)
	url := "postgres://user:hunter2@localhost:5432/storage"
	setEnvironment(t, map[string]string{
		"DATABASE_URL":         url,
		"API_READ_WRITE_TOKEN": secret,
		"API_READ_ONLY_TOKEN":  secret,
	})
	_, err := Load()
	if err == nil {
		t.Fatal("equal tokens were accepted")
	}
	message := err.Error()
	for _, leaked := range []string{secret, "hunter2", url} {
		if strings.Contains(message, leaked) {
			t.Fatalf("configuration error %q leaked a credential", message)
		}
	}
}
