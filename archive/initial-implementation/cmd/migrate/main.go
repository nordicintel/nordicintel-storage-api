package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/tern/v2/migrate"
	"github.com/nordicintel/nordicintel-storage-api/internal/migrations"
)

func main() {
	if err := run(); err != nil {
		slog.Error("migration failed", "error", err)
		os.Exit(1)
	}
}

func run() error {
	databaseURL := os.Getenv("DATABASE_URL")
	if databaseURL == "" {
		return fmt.Errorf("DATABASE_URL is required")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	conn, err := pgx.Connect(ctx, databaseURL)
	if err != nil {
		return fmt.Errorf("connect: %w", err)
	}
	defer conn.Close(ctx)
	var encoding string
	var version int
	if err := conn.QueryRow(ctx, "select current_setting('server_encoding'), current_setting('server_version_num')::int").Scan(&encoding, &version); err != nil {
		return fmt.Errorf("inspect PostgreSQL: %w", err)
	}
	if encoding != "UTF8" || version < 150000 {
		return fmt.Errorf("PostgreSQL 15 or newer with UTF8 encoding is required (encoding=%s, server_version_num=%d)", encoding, version)
	}
	migrator, err := migrate.NewMigrator(ctx, conn, "public.schema_version")
	if err != nil {
		return fmt.Errorf("initialize tern: %w", err)
	}
	if err := migrator.LoadMigrations(migrations.FS()); err != nil {
		return fmt.Errorf("load migrations: %w", err)
	}
	migrator.OnStart = func(sequence int32, name, direction, _ string) {
		slog.Info("applying migration", "sequence", sequence, "name", name, "direction", direction)
	}
	if err := migrator.Migrate(ctx); err != nil {
		return fmt.Errorf("apply migrations: %w", err)
	}
	slog.Info("migrations complete")
	return nil
}
