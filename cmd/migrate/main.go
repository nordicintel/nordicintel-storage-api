package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"time"

	"github.com/nordicintel/nordicintel-storage-api/internal/migrations"
)

func main() {
	logger := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	databaseURL := os.Getenv("DATABASE_URL")
	if databaseURL == "" {
		logger.Error("migration failed", "error", "DATABASE_URL is required")
		os.Exit(1)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	if err := migrations.Run(ctx, databaseURL); err != nil {
		logger.Error("migration failed", "error", fmt.Sprintf("%v", err))
		os.Exit(1)
	}
	logger.Info("migrations complete", "schema_version", migrations.ExpectedVersion)
}
