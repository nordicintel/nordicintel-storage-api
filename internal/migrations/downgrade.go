//go:build !production

package migrations

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/tern/v2/migrate"
)

// MigrateTo moves a database to an explicit migration version, including
// backwards. Production migrations are forward only, so this is excluded from
// the shipped binaries by the "production" build tag and exists purely so
// disposable test databases can exercise the down migration.
func MigrateTo(ctx context.Context, databaseURL string, version int32) error {
	conn, err := pgx.Connect(ctx, databaseURL)
	if err != nil {
		return fmt.Errorf("connect for migrations: %w", err)
	}
	defer conn.Close(context.Background())
	if err := CheckServer(ctx, conn); err != nil {
		return err
	}
	if _, err := conn.Exec(ctx, `select pg_advisory_lock($1)`, AdvisoryLockKey); err != nil {
		return fmt.Errorf("acquire migration lock: %w", err)
	}
	defer func() {
		releaseContext, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_, _ = conn.Exec(releaseContext, `select pg_advisory_unlock($1)`, AdvisoryLockKey)
	}()
	migrator, err := migrate.NewMigrator(ctx, conn, VersionTable)
	if err != nil {
		return fmt.Errorf("initialize migrator: %w", err)
	}
	if err := migrator.LoadMigrations(Files()); err != nil {
		return fmt.Errorf("load migrations: %w", err)
	}
	if err := migrator.MigrateTo(ctx, version); err != nil {
		return fmt.Errorf("migrate to version %d: %w", version, err)
	}
	return nil
}
