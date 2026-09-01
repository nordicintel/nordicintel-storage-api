package migrations

import (
	"context"
	"embed"
	"fmt"
	"io/fs"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/tern/v2/migrate"
)

const (
	ExpectedVersion = int32(1)
	VersionTable    = "public.schema_version"
)

//go:embed sql/*.sql
var embedded embed.FS

func Files() fs.FS {
	sub, err := fs.Sub(embedded, "sql")
	if err != nil {
		panic(err)
	}
	return sub
}

func Run(ctx context.Context, databaseURL string) error {
	conn, err := pgx.Connect(ctx, databaseURL)
	if err != nil {
		return fmt.Errorf("connect for migrations: %w", err)
	}
	defer conn.Close(context.Background())
	if err := CheckServer(ctx, conn); err != nil {
		return err
	}
	migrator, err := migrate.NewMigrator(ctx, conn, VersionTable)
	if err != nil {
		return fmt.Errorf("initialize migrator: %w", err)
	}
	if err := migrator.LoadMigrations(Files()); err != nil {
		return fmt.Errorf("load migrations: %w", err)
	}
	if err := migrator.Migrate(ctx); err != nil {
		return fmt.Errorf("apply migrations: %w", err)
	}
	return nil
}

type queryRower interface {
	QueryRow(context.Context, string, ...any) pgx.Row
}

func CheckServer(ctx context.Context, db queryRower) error {
	var version int
	var encoding string
	err := db.QueryRow(ctx, `
		select current_setting('server_version_num')::integer,
		       pg_encoding_to_char(encoding)
		from pg_database
		where datname = current_database()
	`).Scan(&version, &encoding)
	if err != nil {
		return fmt.Errorf("inspect PostgreSQL server: %w", err)
	}
	if version/10000 != 18 {
		return fmt.Errorf("PostgreSQL 18 is required (server_version_num=%d)", version)
	}
	if encoding != "UTF8" {
		return fmt.Errorf("UTF-8 database encoding is required (encoding=%s)", encoding)
	}
	return nil
}

func CheckVersion(ctx context.Context, db queryRower) error {
	var exists bool
	if err := db.QueryRow(ctx, `select to_regclass($1) is not null`, VersionTable).Scan(&exists); err != nil {
		return fmt.Errorf("inspect migration table: %w", err)
	}
	if !exists {
		return fmt.Errorf("migration table is missing")
	}
	var version int32
	if err := db.QueryRow(ctx, `select version from public.schema_version`).Scan(&version); err != nil {
		return fmt.Errorf("read migration version: %w", err)
	}
	if version != ExpectedVersion {
		return fmt.Errorf("schema version %d does not match expected version %d", version, ExpectedVersion)
	}
	return nil
}
