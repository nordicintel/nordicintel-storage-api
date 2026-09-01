package store

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/nordicintel/nordicintel-storage-api/internal/domain"
	"github.com/nordicintel/nordicintel-storage-api/internal/migrations"
)

// These tests need a disposable PostgreSQL 18 server. DATABASE_URL points at a
// maintenance database; every test that needs isolation creates and drops its
// own database underneath it.
func requireDatabaseURL(t *testing.T) string {
	t.Helper()
	url := os.Getenv("DATABASE_URL")
	if url == "" {
		t.Skip("DATABASE_URL is not set; skipping PostgreSQL integration tests")
	}
	return url
}

var databaseCounter atomic.Int64

// freshDatabase creates an empty database and returns its URL. The database is
// dropped when the test finishes.
func freshDatabase(t *testing.T, options string) string {
	t.Helper()
	base := requireDatabaseURL(t)
	name := fmt.Sprintf("storage_test_%d_%d", time.Now().UnixNano()%1_000_000, databaseCounter.Add(1))

	ctx := t.Context()
	admin, err := pgx.Connect(ctx, base)
	if err != nil {
		t.Fatalf("connect to the maintenance database: %v", err)
	}
	defer admin.Close(context.Background())
	statement := fmt.Sprintf(`create database %s`, pgx.Identifier{name}.Sanitize())
	if options != "" {
		statement += " " + options
	}
	if _, err := admin.Exec(ctx, statement); err != nil {
		t.Fatalf("create database %s: %v", name, err)
	}
	t.Cleanup(func() {
		cleanupContext, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		conn, err := pgx.Connect(cleanupContext, base)
		if err != nil {
			return
		}
		defer conn.Close(context.Background())
		_, _ = conn.Exec(cleanupContext,
			fmt.Sprintf(`drop database if exists %s with (force)`, pgx.Identifier{name}.Sanitize()))
	})
	return replaceDatabaseName(t, base, name)
}

func replaceDatabaseName(t *testing.T, url, name string) string {
	t.Helper()
	config, err := pgx.ParseConfig(url)
	if err != nil {
		t.Fatalf("parse DATABASE_URL: %v", err)
	}
	config.Database = name
	rest := ""
	if index := strings.Index(url, "?"); index >= 0 {
		rest = url[index:]
	}
	return fmt.Sprintf("postgres://%s:%s@%s:%d/%s%s",
		config.User, config.Password, config.Host, config.Port, name, rest)
}

// migratedDatabase returns a database with migration 001 applied.
func migratedDatabase(t *testing.T) string {
	t.Helper()
	url := freshDatabase(t, "")
	if err := migrations.Run(t.Context(), url); err != nil {
		t.Fatalf("run migrations: %v", err)
	}
	return url
}

func openStore(t *testing.T) *Postgres {
	t.Helper()
	database, err := Open(t.Context(), migratedDatabase(t), 8)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(database.Close)
	return database
}

func connect(t *testing.T, url string) *pgx.Conn {
	t.Helper()
	conn, err := pgx.Connect(t.Context(), url)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(func() { conn.Close(context.Background()) })
	return conn
}

// ------------------------------------------------------------ migrations ---

func TestMigrationCreatesTheDocumentedSchema(t *testing.T) {
	url := migratedDatabase(t)
	conn := connect(t, url)
	ctx := t.Context()

	var version int32
	if err := conn.QueryRow(ctx, `select version from public.schema_version`).Scan(&version); err != nil {
		t.Fatalf("read schema version: %v", err)
	}
	if version != migrations.ExpectedVersion {
		t.Fatalf("schema version = %d, want %d", version, migrations.ExpectedVersion)
	}

	t.Run("the migration table lives outside the storage schema", func(t *testing.T) {
		var inStorage bool
		if err := conn.QueryRow(ctx, `
			select exists(
				select 1 from pg_class c join pg_namespace n on n.oid = c.relnamespace
				where n.nspname = 'storage' and c.relname like '%schema_version%')
		`).Scan(&inStorage); err != nil {
			t.Fatal(err)
		}
		if inStorage {
			t.Fatal("migration metadata was created inside the storage schema")
		}
	})

	t.Run("all five logical tables exist", func(t *testing.T) {
		want := []string{"categories", "datasets", "dimensions", "observations", "providers"}
		rows, err := conn.Query(ctx, `
			select c.relname from pg_class c join pg_namespace n on n.oid = c.relnamespace
			where n.nspname = 'storage' and c.relkind in ('r','p') and not c.relispartition
			order by c.relname
		`)
		if err != nil {
			t.Fatal(err)
		}
		defer rows.Close()
		var got []string
		for rows.Next() {
			var name string
			if err := rows.Scan(&name); err != nil {
				t.Fatal(err)
			}
			got = append(got, name)
		}
		if strings.Join(got, ",") != strings.Join(want, ",") {
			t.Fatalf("tables = %v, want %v", got, want)
		}
	})

	t.Run("observations have exactly 32 hash partitions", func(t *testing.T) {
		var partitions int
		if err := conn.QueryRow(ctx, `
			select count(*) from pg_inherits i
			join pg_class p on p.oid = i.inhparent
			join pg_namespace n on n.oid = p.relnamespace
			where n.nspname = 'storage' and p.relname = 'observations'
		`).Scan(&partitions); err != nil {
			t.Fatal(err)
		}
		if partitions != 32 {
			t.Fatalf("partitions = %d, want 32", partitions)
		}
		var strategy string
		if err := conn.QueryRow(ctx, `
			select partstrat from pg_partitioned_table pt
			join pg_class c on c.oid = pt.partrelid
			join pg_namespace n on n.oid = c.relnamespace
			where n.nspname = 'storage' and c.relname = 'observations'
		`).Scan(&strategy); err != nil {
			t.Fatal(err)
		}
		if strategy != "h" {
			t.Fatalf("partition strategy = %q, want hash", strategy)
		}
	})

	t.Run("every documented constraint is present", func(t *testing.T) {
		want := []string{
			"categories_code_length", "categories_code_not_empty", "categories_dimension_fkey",
			"categories_key_length", "categories_key_not_empty", "categories_key_unique",
			"categories_pkey", "categories_position_nonnegative", "categories_position_unique",
			"datasets_cell_count_range", "datasets_code_length", "datasets_code_not_empty",
			"datasets_identity_unique", "datasets_key_length", "datasets_key_not_empty",
			"datasets_pkey", "datasets_provider_fkey", "datasets_valued_cell_count_range",
			"dimensions_code_length", "dimensions_code_not_empty", "dimensions_dataset_fkey",
			"dimensions_key_length", "dimensions_key_not_empty", "dimensions_key_unique",
			"dimensions_pkey", "dimensions_position_nonnegative", "dimensions_position_unique",
			"observations_cell_index_range", "observations_dataset_fkey", "observations_has_content",
			"observations_numeric_value_finite", "observations_pkey", "observations_value_exclusive",
			"providers_code_length", "providers_code_not_empty", "providers_key_length",
			"providers_key_not_empty", "providers_key_unique", "providers_pkey",
		}
		rows, err := conn.Query(ctx, `
			select distinct con.conname from pg_constraint con
			join pg_class c on c.oid = con.conrelid
			join pg_namespace n on n.oid = c.relnamespace
			where n.nspname = 'storage' and not c.relispartition
			order by con.conname
		`)
		if err != nil {
			t.Fatal(err)
		}
		defer rows.Close()
		found := make(map[string]struct{})
		for rows.Next() {
			var name string
			if err := rows.Scan(&name); err != nil {
				t.Fatal(err)
			}
			found[name] = struct{}{}
		}
		for _, name := range want {
			if _, ok := found[name]; !ok {
				t.Errorf("constraint %s is missing", name)
			}
		}
	})

	t.Run("tables and columns are documented", func(t *testing.T) {
		var tables, columns int
		if err := conn.QueryRow(ctx, `
			select
				count(*) filter (where d.objsubid = 0),
				count(*) filter (where d.objsubid > 0)
			from pg_description d
			join pg_class c on c.oid = d.objoid
			join pg_namespace n on n.oid = c.relnamespace
			where n.nspname = 'storage' and not c.relispartition
		`).Scan(&tables, &columns); err != nil {
			t.Fatal(err)
		}
		if tables != 5 {
			t.Fatalf("%d table comments, want one per table", tables)
		}
		if columns < 15 {
			t.Fatalf("%d column comments, want the documented set", columns)
		}
		var schemaComment string
		if err := conn.QueryRow(ctx,
			`select obj_description('storage'::regnamespace, 'pg_namespace')`).Scan(&schemaComment); err != nil {
			t.Fatal(err)
		}
		if schemaComment == "" {
			t.Fatal("the storage schema has no comment")
		}
	})
}

func TestRepeatedMigrationIsANoOp(t *testing.T) {
	url := migratedDatabase(t)
	for range 3 {
		if err := migrations.Run(t.Context(), url); err != nil {
			t.Fatalf("repeated migration failed: %v", err)
		}
	}
	conn := connect(t, url)
	var version int32
	if err := conn.QueryRow(t.Context(), `select version from public.schema_version`).Scan(&version); err != nil {
		t.Fatal(err)
	}
	if version != migrations.ExpectedVersion {
		t.Fatalf("schema version = %d after repeated migration", version)
	}
}

func TestMigrationRollsBackCompletelyOnFailure(t *testing.T) {
	url := freshDatabase(t, "")
	conn := connect(t, url)
	// Occupying the schema name makes the forward migration fail part way.
	if _, err := conn.Exec(t.Context(), `create schema storage`); err != nil {
		t.Fatal(err)
	}
	if err := migrations.Run(t.Context(), url); err == nil {
		t.Fatal("the migration succeeded despite a conflicting schema")
	}
	var tables int
	if err := conn.QueryRow(t.Context(), `
		select count(*) from pg_class c join pg_namespace n on n.oid = c.relnamespace
		where n.nspname = 'storage'
	`).Scan(&tables); err != nil {
		t.Fatal(err)
	}
	if tables != 0 {
		t.Fatalf("a failed migration left %d objects behind", tables)
	}
	var version int32
	if err := conn.QueryRow(t.Context(), `select version from public.schema_version`).Scan(&version); err != nil {
		t.Fatal(err)
	}
	if version != 0 {
		t.Fatalf("schema version = %d after a failed migration, want 0", version)
	}
}

func TestDownAndUpMigrationsRoundTrip(t *testing.T) {
	url := migratedDatabase(t)
	conn := connect(t, url)
	ctx := t.Context()

	if err := migrations.MigrateTo(ctx, url, 0); err != nil {
		t.Fatalf("down migration: %v", err)
	}
	var exists bool
	if err := conn.QueryRow(ctx, `select exists(select 1 from pg_namespace where nspname='storage')`).Scan(&exists); err != nil {
		t.Fatal(err)
	}
	if exists {
		t.Fatal("the down migration left the storage schema in place")
	}
	if err := migrations.Run(ctx, url); err != nil {
		t.Fatalf("re-applying the migration: %v", err)
	}
	var partitions int
	if err := conn.QueryRow(ctx, `
		select count(*) from pg_inherits i join pg_class p on p.oid = i.inhparent
		join pg_namespace n on n.oid = p.relnamespace
		where n.nspname='storage' and p.relname='observations'
	`).Scan(&partitions); err != nil {
		t.Fatal(err)
	}
	if partitions != 32 {
		t.Fatalf("partitions = %d after down/up, want 32", partitions)
	}
}

func TestStartupChecksRejectUnmigratedAndMismatchedDatabases(t *testing.T) {
	t.Run("missing migration table", func(t *testing.T) {
		url := freshDatabase(t, "")
		if _, err := Open(t.Context(), url, 2); err == nil {
			t.Fatal("the store opened against an unmigrated database")
		}
	})
	t.Run("wrong schema version", func(t *testing.T) {
		url := migratedDatabase(t)
		conn := connect(t, url)
		if _, err := conn.Exec(t.Context(), `update public.schema_version set version = 99`); err != nil {
			t.Fatal(err)
		}
		_, err := Open(t.Context(), url, 2)
		if err == nil {
			t.Fatal("the store opened against a mismatched schema version")
		}
		if !strings.Contains(err.Error(), "99") {
			t.Fatalf("error = %v, want it to report the mismatch", err)
		}
	})
	t.Run("non UTF-8 encoding", func(t *testing.T) {
		url := freshDatabase(t, "encoding 'SQL_ASCII' template template0 lc_collate 'C' lc_ctype 'C'")
		err := migrations.Run(t.Context(), url)
		if err == nil {
			t.Fatal("migrations ran against a non UTF-8 database")
		}
		if !strings.Contains(err.Error(), "UTF-8") {
			t.Fatalf("error = %v, want an encoding error", err)
		}
	})
}

// ---------------------------------------------------------------- schema ---

func TestSchemaConstraintsRejectInvalidRows(t *testing.T) {
	url := migratedDatabase(t)
	conn := connect(t, url)
	ctx := t.Context()

	var providerID, datasetID int64
	if err := conn.QueryRow(ctx, `
		insert into storage.providers(provider_code, provider_key) values ('SCB','scb')
		returning provider_id`).Scan(&providerID); err != nil {
		t.Fatal(err)
	}
	if err := conn.QueryRow(ctx, `
		insert into storage.datasets(provider_id, dataset_code, dataset_key, source_stamp,
			cell_count, valued_cell_count, updated_at)
		values ($1,'P','p','null'::jsonb, 10, 2, now())
		returning dataset_id`, providerID).Scan(&datasetID); err != nil {
		t.Fatal(err)
	}

	cases := []struct {
		name      string
		statement string
		arguments []any
	}{
		{"empty provider code", `insert into storage.providers(provider_code, provider_key) values ('','x')`, nil},
		{"empty provider key", `insert into storage.providers(provider_code, provider_key) values ('X','')`, nil},
		{"duplicate provider key", `insert into storage.providers(provider_code, provider_key) values ('Other','scb')`, nil},
		{"oversized provider code",
			`insert into storage.providers(provider_code, provider_key) values (repeat('a',257),'long')`, nil},
		{"duplicate dataset identity",
			`insert into storage.datasets(provider_id, dataset_code, dataset_key, source_stamp, cell_count, valued_cell_count, updated_at)
			 values ($1,'P2','p','null'::jsonb,1,0,now())`, []any{providerID}},
		{"zero cell count",
			`insert into storage.datasets(provider_id, dataset_code, dataset_key, source_stamp, cell_count, valued_cell_count, updated_at)
			 values ($1,'Z','z','null'::jsonb,0,0,now())`, []any{providerID}},
		{"cell count over the ceiling",
			`insert into storage.datasets(provider_id, dataset_code, dataset_key, source_stamp, cell_count, valued_cell_count, updated_at)
			 values ($1,'B','b','null'::jsonb,1000001,0,now())`, []any{providerID}},
		{"valued count above cell count",
			`insert into storage.datasets(provider_id, dataset_code, dataset_key, source_stamp, cell_count, valued_cell_count, updated_at)
			 values ($1,'V','v','null'::jsonb,5,6,now())`, []any{providerID}},
		{"sql null source stamp",
			`insert into storage.datasets(provider_id, dataset_code, dataset_key, source_stamp, cell_count, valued_cell_count, updated_at)
			 values ($1,'N','n',null,1,0,now())`, []any{providerID}},
		{"negative dimension position",
			`insert into storage.dimensions(dataset_id, dimension_code, dimension_key, position)
			 values ($1,'d','d',-1)`, []any{datasetID}},
		{"observation with neither value nor status",
			`insert into storage.observations(dataset_id, cell_index) values ($1, 0)`, []any{datasetID}},
		{"observation with both a numeric and a text value",
			`insert into storage.observations(dataset_id, cell_index, numeric_value, text_value) values ($1,1,1.0,'x')`,
			[]any{datasetID}},
		{"observation with a NaN value",
			`insert into storage.observations(dataset_id, cell_index, numeric_value) values ($1,2,'NaN'::double precision)`,
			[]any{datasetID}},
		{"observation with positive infinity",
			`insert into storage.observations(dataset_id, cell_index, numeric_value) values ($1,3,'Infinity'::double precision)`,
			[]any{datasetID}},
		{"observation with negative infinity",
			`insert into storage.observations(dataset_id, cell_index, numeric_value) values ($1,4,'-Infinity'::double precision)`,
			[]any{datasetID}},
		{"observation index below zero",
			`insert into storage.observations(dataset_id, cell_index, numeric_value) values ($1,-1,1.0)`, []any{datasetID}},
		{"observation index over the ceiling",
			`insert into storage.observations(dataset_id, cell_index, numeric_value) values ($1,1000000,1.0)`, []any{datasetID}},
		{"observation for an unknown dataset",
			`insert into storage.observations(dataset_id, cell_index, numeric_value) values (987654321,0,1.0)`, nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			transaction, err := conn.Begin(ctx)
			if err != nil {
				t.Fatal(err)
			}
			defer transaction.Rollback(context.Background())
			if _, err := transaction.Exec(ctx, tc.statement, tc.arguments...); err == nil {
				t.Fatal("the database accepted an invalid row")
			}
		})
	}
}

func TestDuplicateObservationIndexesAreRejected(t *testing.T) {
	url := migratedDatabase(t)
	conn := connect(t, url)
	ctx := t.Context()
	datasetID := seedDataset(t, conn, "SCB", "P", 10, 0)
	if _, err := conn.Exec(ctx,
		`insert into storage.observations(dataset_id, cell_index, numeric_value) values ($1,5,1.0)`, datasetID); err != nil {
		t.Fatal(err)
	}
	if _, err := conn.Exec(ctx,
		`insert into storage.observations(dataset_id, cell_index, numeric_value) values ($1,5,2.0)`, datasetID); err == nil {
		t.Fatal("a duplicate cell index was accepted")
	}
}

func TestOneDatasetRoutesToOnePartition(t *testing.T) {
	url := migratedDatabase(t)
	conn := connect(t, url)
	ctx := t.Context()
	datasetID := seedDataset(t, conn, "SCB", "P", 1000, 0)
	for index := range 500 {
		if _, err := conn.Exec(ctx,
			`insert into storage.observations(dataset_id, cell_index, numeric_value) values ($1,$2,$3)`,
			datasetID, index, float64(index)); err != nil {
			t.Fatal(err)
		}
	}
	rows, err := conn.Query(ctx, `
		select c.relname, count(*) from storage.observations o
		join pg_class c on c.oid = o.tableoid
		where o.dataset_id = $1
		group by c.relname
	`, datasetID)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	partitions := 0
	for rows.Next() {
		var name string
		var count int64
		if err := rows.Scan(&name, &count); err != nil {
			t.Fatal(err)
		}
		partitions++
		if count != 500 {
			t.Fatalf("partition %s holds %d of 500 rows", name, count)
		}
	}
	if partitions != 1 {
		t.Fatalf("the dataset spread across %d partitions, want 1", partitions)
	}

	// Independent datasets must not all land in the same partition.
	distinct := make(map[string]struct{})
	for i := range 40 {
		other := seedDataset(t, conn, "SCB", fmt.Sprintf("D%d", i), 10, 0)
		if _, err := conn.Exec(ctx,
			`insert into storage.observations(dataset_id, cell_index, numeric_value) values ($1,0,1.0)`, other); err != nil {
			t.Fatal(err)
		}
		var name string
		if err := conn.QueryRow(ctx, `
			select c.relname from storage.observations o join pg_class c on c.oid = o.tableoid
			where o.dataset_id = $1 limit 1`, other).Scan(&name); err != nil {
			t.Fatal(err)
		}
		distinct[name] = struct{}{}
	}
	if len(distinct) < 5 {
		t.Fatalf("40 datasets used only %d partitions", len(distinct))
	}
}

func TestJSONNullSourceStampIsDistinctFromSQLNull(t *testing.T) {
	url := migratedDatabase(t)
	conn := connect(t, url)
	ctx := t.Context()
	datasetID := seedDataset(t, conn, "SCB", "P", 1, 0)
	var isSQLNull bool
	var jsonType string
	if err := conn.QueryRow(ctx, `
		select source_stamp is null, jsonb_typeof(source_stamp)
		from storage.datasets where dataset_id = $1`, datasetID).Scan(&isSQLNull, &jsonType); err != nil {
		t.Fatal(err)
	}
	if isSQLNull {
		t.Fatal("the stored JSON null became a SQL NULL")
	}
	if jsonType != "null" {
		t.Fatalf("jsonb_typeof = %q, want null", jsonType)
	}
}

func TestGeneratedNullCellCountTracksTheStoredCounts(t *testing.T) {
	url := migratedDatabase(t)
	conn := connect(t, url)
	ctx := t.Context()
	datasetID := seedDataset(t, conn, "SCB", "P", 10, 4)
	var nullCount int64
	if err := conn.QueryRow(ctx,
		`select null_cell_count from storage.datasets where dataset_id=$1`, datasetID).Scan(&nullCount); err != nil {
		t.Fatal(err)
	}
	if nullCount != 6 {
		t.Fatalf("null_cell_count = %d, want 6", nullCount)
	}
	if _, err := conn.Exec(ctx,
		`update storage.datasets set valued_cell_count = 10 where dataset_id=$1`, datasetID); err != nil {
		t.Fatal(err)
	}
	if err := conn.QueryRow(ctx,
		`select null_cell_count from storage.datasets where dataset_id=$1`, datasetID).Scan(&nullCount); err != nil {
		t.Fatal(err)
	}
	if nullCount != 0 {
		t.Fatalf("null_cell_count = %d after update, want 0", nullCount)
	}
}

func seedDataset(t *testing.T, conn *pgx.Conn, provider, dataset string, cellCount, valuedCount int64) int64 {
	t.Helper()
	ctx := t.Context()
	var providerID int64
	if err := conn.QueryRow(ctx, `
		insert into storage.providers(provider_code, provider_key) values ($1,$2)
		on conflict (provider_key) do update set provider_code = storage.providers.provider_code
		returning provider_id`, provider, strings.ToLower(provider)).Scan(&providerID); err != nil {
		t.Fatal(err)
	}
	var datasetID int64
	if err := conn.QueryRow(ctx, `
		insert into storage.datasets(provider_id, dataset_code, dataset_key, source_stamp,
			cell_count, valued_cell_count, updated_at)
		values ($1,$2,$3,'null'::jsonb,$4,$5,now())
		returning dataset_id`,
		providerID, dataset, strings.ToLower(dataset), cellCount, valuedCount).Scan(&datasetID); err != nil {
		t.Fatal(err)
	}
	return datasetID
}

// ------------------------------------------------------ store operations ---

func code(t *testing.T, spelling string) domain.Code {
	t.Helper()
	value, err := domain.NormalizeCode(spelling)
	if err != nil {
		t.Fatalf("normalize %q: %v", spelling, err)
	}
	return value
}

func parse(t *testing.T, provider, dataset, body string) domain.Replacement {
	t.Helper()
	replacement, err := domain.ParseReplacement(provider, dataset, []byte(body), 1_000_000)
	if err != nil {
		t.Fatalf("parse replacement: %v", err)
	}
	return replacement
}

// twoByTwo is the contract example: sex/year with M,F and 2024,2025.
const twoByTwo = `"id":["sex","year"],` +
	`"dimension":{"sex":{"index":{"M":0,"F":1}},"year":{"index":{"2024":0,"2025":1}}}`

func TestCreateReadReplaceAndDelete(t *testing.T) {
	database := openStore(t)
	ctx := t.Context()
	provider, dataset := code(t, "SCB"), code(t, "Population")

	body := `{"source_stamp":{"etag":"abc"},` + twoByTwo + `,` +
		`"value":[10.5,null,null,null],"text":[null,null,null,"confidential"],"status":[null,null,null,"c"]}`
	result, summary, err := database.Replace(ctx, parse(t, "SCB", "Population", body))
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if result != "created" {
		t.Fatalf("result = %q, want created", result)
	}
	if summary.CellCount != 4 || summary.ValuedCellCount != 2 || summary.NullCellCount != 2 {
		t.Fatalf("counts = %+v", summary)
	}
	if summary.ProviderCode != "SCB" || summary.DatasetCode != "Population" {
		t.Fatalf("spellings = %+v", summary)
	}
	if string(summary.SourceStamp) != `{"etag": "abc"}` && string(summary.SourceStamp) != `{"etag":"abc"}` {
		t.Fatalf("source stamp = %s", summary.SourceStamp)
	}

	t.Run("create-only conflicts once the dataset exists", func(t *testing.T) {
		_, _, err := database.Replace(ctx, parse(t, "SCB", "Population", body))
		if err != ErrDatasetExists {
			t.Fatalf("error = %v, want ErrDatasetExists", err)
		}
	})

	t.Run("summary", func(t *testing.T) {
		got, err := database.GetSummary(ctx, provider, dataset)
		if err != nil {
			t.Fatal(err)
		}
		if got.CellCount != 4 || got.ValuedCellCount != 2 {
			t.Fatalf("summary = %+v", got)
		}
	})

	t.Run("structure is sorted by normalized key", func(t *testing.T) {
		view, err := database.GetStructure(ctx, provider, dataset)
		if err != nil {
			t.Fatal(err)
		}
		if len(view.Dimensions) != 2 {
			t.Fatalf("dimensions = %+v", view.Dimensions)
		}
		if view.Dimensions[0].Code.Key != "sex" || view.Dimensions[1].Code.Key != "year" {
			t.Fatalf("dimension order = %q, %q", view.Dimensions[0].Code.Key, view.Dimensions[1].Code.Key)
		}
		sex := view.Dimensions[0].Categories
		if sex[0].Code.Spelling != "F" || sex[1].Code.Spelling != "M" {
			t.Fatalf("sex categories = %+v", sex)
		}
		if len(view.Cells) != 0 {
			t.Fatal("the structure read returned observations")
		}
	})

	t.Run("data holds only populated cells in index order", func(t *testing.T) {
		view, err := database.GetData(ctx, provider, dataset)
		if err != nil {
			t.Fatal(err)
		}
		// Sorting F before M reorders the payload: payload index 0 is
		// (M, 2024) and maps to internal index 2, while payload index 3 is
		// (F, 2025) and maps to internal index 1.
		if len(view.Cells) != 2 {
			t.Fatalf("cells = %+v, want two populated cells", view.Cells)
		}
		if view.Cells[0].Index != 1 || view.Cells[0].Text == nil || *view.Cells[0].Text != "confidential" {
			t.Fatalf("first cell = %+v", view.Cells[0])
		}
		if view.Cells[0].Status == nil || *view.Cells[0].Status != "c" {
			t.Fatalf("first cell status = %+v", view.Cells[0])
		}
		if view.Cells[1].Index != 2 || view.Cells[1].Numeric == nil || *view.Cells[1].Numeric != 10.5 {
			t.Fatalf("second cell = %+v", view.Cells[1])
		}
	})

	t.Run("replacement overwrites everything and advances the timestamp", func(t *testing.T) {
		before, err := database.GetSummary(ctx, provider, dataset)
		if err != nil {
			t.Fatal(err)
		}
		replacementBody := `{"replace":true,"source_stamp":42,` + twoByTwo + `,"value":[1,2,3,4]}`
		result, summary, err := database.Replace(ctx, parse(t, "scb", "population", replacementBody))
		if err != nil {
			t.Fatalf("replace: %v", err)
		}
		if result != "replaced" {
			t.Fatalf("result = %q, want replaced", result)
		}
		if summary.ValuedCellCount != 4 || summary.NullCellCount != 0 {
			t.Fatalf("counts = %+v", summary)
		}
		if !summary.UpdatedAt.After(before.UpdatedAt) {
			t.Fatalf("updated_at did not advance: %v then %v", before.UpdatedAt, summary.UpdatedAt)
		}
		if string(summary.SourceStamp) != "42" {
			t.Fatalf("source stamp = %s, want 42", summary.SourceStamp)
		}
		view, err := database.GetData(ctx, provider, dataset)
		if err != nil {
			t.Fatal(err)
		}
		if len(view.Cells) != 4 {
			t.Fatalf("cells = %d, want 4", len(view.Cells))
		}
		for _, cell := range view.Cells {
			if cell.Text != nil || cell.Status != nil {
				t.Fatalf("cell %d kept data from the previous state: %+v", cell.Index, cell)
			}
		}
	})

	t.Run("identity spellings are preserved from first creation", func(t *testing.T) {
		summary, err := database.GetSummary(ctx, provider, dataset)
		if err != nil {
			t.Fatal(err)
		}
		if summary.ProviderCode != "SCB" || summary.DatasetCode != "Population" {
			t.Fatalf("spellings = %q/%q, want the first-creation spellings", summary.ProviderCode, summary.DatasetCode)
		}
	})

	t.Run("structure spellings come from the latest replacement", func(t *testing.T) {
		body := `{"replace":true,"source_stamp":null,"id":["SEX","YEAR"],` +
			`"dimension":{"SEX":{"index":{"m":0,"f":1}},"YEAR":{"index":{"2024":0,"2025":1}}},"value":{}}`
		if _, _, err := database.Replace(ctx, parse(t, "SCB", "Population", body)); err != nil {
			t.Fatal(err)
		}
		view, err := database.GetStructure(ctx, provider, dataset)
		if err != nil {
			t.Fatal(err)
		}
		if view.Dimensions[0].Code.Spelling != "SEX" {
			t.Fatalf("dimension spelling = %q, want the latest SEX", view.Dimensions[0].Code.Spelling)
		}
		if view.Dimensions[0].Categories[0].Code.Spelling != "f" {
			t.Fatalf("category spelling = %q, want the latest f", view.Dimensions[0].Categories[0].Code.Spelling)
		}
	})

	t.Run("delete is idempotent and cascades", func(t *testing.T) {
		for range 2 {
			if err := database.Delete(ctx, provider, dataset); err != nil {
				t.Fatalf("delete: %v", err)
			}
		}
		if _, err := database.GetSummary(ctx, provider, dataset); err != ErrNotFound {
			t.Fatalf("error = %v, want ErrNotFound", err)
		}
	})
}

func TestDeleteRemovesEveryDependentRow(t *testing.T) {
	url := migratedDatabase(t)
	database, err := Open(t.Context(), url, 4)
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	ctx := t.Context()
	body := `{"source_stamp":null,` + twoByTwo + `,"value":[1,2,3,4]}`
	if _, _, err := database.Replace(ctx, parse(t, "SCB", "Population", body)); err != nil {
		t.Fatal(err)
	}
	if err := database.Delete(ctx, code(t, "SCB"), code(t, "Population")); err != nil {
		t.Fatal(err)
	}
	conn := connect(t, url)
	for _, table := range []string{"datasets", "dimensions", "categories", "observations"} {
		var count int64
		if err := conn.QueryRow(ctx, `select count(*) from storage.`+table).Scan(&count); err != nil {
			t.Fatal(err)
		}
		if count != 0 {
			t.Fatalf("%s still holds %d rows after deletion", table, count)
		}
	}
	var providers int64
	if err := conn.QueryRow(ctx, `select count(*) from storage.providers`).Scan(&providers); err != nil {
		t.Fatal(err)
	}
	if providers != 1 {
		t.Fatalf("providers = %d, want the registry row to remain", providers)
	}
}

func TestProviderSpellingSurvivesDeletionAndDatasetSpellingDoesNot(t *testing.T) {
	database := openStore(t)
	ctx := t.Context()
	body := `{"source_stamp":null,"id":["a"],"dimension":{"a":{"index":{"x":0}}},"value":[1]}`

	if _, _, err := database.Replace(ctx, parse(t, "SCB", "Population", body)); err != nil {
		t.Fatal(err)
	}
	if err := database.Delete(ctx, code(t, "SCB"), code(t, "Population")); err != nil {
		t.Fatal(err)
	}
	// A provider with no datasets is externally invisible.
	if _, _, err := database.ListDatasets(ctx, code(t, "SCB")); err != ErrNotFound {
		t.Fatalf("error = %v, want the empty provider to be invisible", err)
	}
	providers, err := database.ListProviders(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(providers) != 0 {
		t.Fatalf("providers = %+v, want the empty provider hidden", providers)
	}

	// Recreating under a different spelling keeps the retained provider
	// spelling but establishes a new dataset spelling.
	if _, _, err := database.Replace(ctx, parse(t, "scb", "POPULATION", body)); err != nil {
		t.Fatal(err)
	}
	summary, err := database.GetSummary(ctx, code(t, "SCB"), code(t, "Population"))
	if err != nil {
		t.Fatal(err)
	}
	if summary.ProviderCode != "SCB" {
		t.Fatalf("provider spelling = %q, want the retained SCB", summary.ProviderCode)
	}
	if summary.DatasetCode != "POPULATION" {
		t.Fatalf("dataset spelling = %q, want the new POPULATION", summary.DatasetCode)
	}
}

func TestListingsAreDeterministicAndCountDatasets(t *testing.T) {
	database := openStore(t)
	ctx := t.Context()
	body := `{"source_stamp":null,"id":["a"],"dimension":{"a":{"index":{"x":0}}},"value":[1]}`
	for _, identity := range [][2]string{
		{"Zeta", "b"}, {"Alpha", "z"}, {"Alpha", "a"}, {"Mid", "m"}, {"Alpha", "M"},
	} {
		if _, _, err := database.Replace(ctx, parse(t, identity[0], identity[1], body)); err != nil {
			t.Fatalf("create %v: %v", identity, err)
		}
	}
	providers, err := database.ListProviders(ctx)
	if err != nil {
		t.Fatal(err)
	}
	want := []domain.ProviderListItem{
		{ProviderCode: "Alpha", DatasetCount: 3},
		{ProviderCode: "Mid", DatasetCount: 1},
		{ProviderCode: "Zeta", DatasetCount: 1},
	}
	if len(providers) != len(want) {
		t.Fatalf("providers = %+v", providers)
	}
	for i := range want {
		if providers[i] != want[i] {
			t.Fatalf("providers = %+v, want %+v", providers, want)
		}
	}

	spelling, datasets, err := database.ListDatasets(ctx, code(t, "alpha"))
	if err != nil {
		t.Fatal(err)
	}
	if spelling != "Alpha" {
		t.Fatalf("provider spelling = %q", spelling)
	}
	var codes []string
	for _, summary := range datasets {
		codes = append(codes, summary.DatasetCode)
	}
	if strings.Join(codes, ",") != "a,M,z" {
		t.Fatalf("dataset order = %v, want normalized-key order a,M,z", codes)
	}

	if _, _, err := database.ListDatasets(ctx, code(t, "unknown")); err != ErrNotFound {
		t.Fatalf("error = %v, want ErrNotFound", err)
	}
	if _, err := database.GetSummary(ctx, code(t, "Alpha"), code(t, "missing")); err != ErrNotFound {
		t.Fatalf("error = %v, want ErrNotFound", err)
	}
}

func TestQueryReturnsRequestedOrderAndInfersNulls(t *testing.T) {
	database := openStore(t)
	ctx := t.Context()
	provider, dataset := code(t, "SCB"), code(t, "Population")
	// Internal index is sex*2 + year with sex F=0,M=1 and year 2024=0,2025=1,
	// so payload index 0 (M, 2024) stores at 2 and payload index 3 (F, 2025)
	// stores at 1.
	body := `{"source_stamp":null,` + twoByTwo + `,"value":{"0":10,"3":40}}`
	if _, _, err := database.Replace(ctx, parse(t, "SCB", "Population", body)); err != nil {
		t.Fatal(err)
	}

	selection, err := domain.ParseSelection([]byte(
		`{"id":["year","sex"],"dimension":{"year":{"index":{"2025":0,"2024":1}},"sex":{"index":{"M":0,"F":1}}}}`))
	if err != nil {
		t.Fatal(err)
	}
	view, err := database.Query(ctx, provider, dataset, selection, 1_000_000)
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	if view.Dimensions[0].Code.Key != "year" || view.Dimensions[1].Code.Key != "sex" {
		t.Fatalf("output order = %+v", view.Dimensions)
	}
	if view.Dimensions[1].Categories[0].Code.Spelling != "M" {
		t.Fatalf("stored spellings were not returned: %+v", view.Dimensions[1].Categories)
	}
	// Output order is (2025,M) (2025,F) (2024,M) (2024,F), whose internal
	// indexes are 3, 1, 2, 0. Only internal 1 and 2 are populated, so the
	// answer appears at output indexes 1 and 2.
	values := make(map[int64]float64)
	for _, cell := range view.Cells {
		if cell.Numeric == nil {
			t.Fatalf("cell %d has no numeric value", cell.Index)
		}
		values[cell.Index] = *cell.Numeric
	}
	if len(values) != 2 || values[1] != 40 || values[2] != 10 {
		t.Fatalf("query values = %v, want output index 1 -> 40 and 2 -> 10", values)
	}
	if view.Summary.CellCount != 4 || view.Summary.ValuedCellCount != 2 {
		t.Fatalf("subset response lost the whole-dataset counts: %+v", view.Summary)
	}

	t.Run("a subset keeps requested categories with no data", func(t *testing.T) {
		selection, err := domain.ParseSelection([]byte(
			`{"id":["sex","year"],"dimension":{"sex":{"index":{"F":0}},"year":{"index":{"2024":0}}}}`))
		if err != nil {
			t.Fatal(err)
		}
		view, err := database.Query(ctx, provider, dataset, selection, 1_000_000)
		if err != nil {
			t.Fatal(err)
		}
		if len(view.Dimensions) != 2 || len(view.Dimensions[0].Categories) != 1 {
			t.Fatalf("dimensions = %+v", view.Dimensions)
		}
		if view.Dimensions[0].Categories[0].Code.Spelling != "F" {
			t.Fatalf("the requested category was dropped: %+v", view.Dimensions[0].Categories)
		}
		// (F, 2024) is internal index 0, which has no row, so the whole subset
		// is inferred null while still describing the requested categories.
		if len(view.Cells) != 0 {
			t.Fatalf("cells = %+v, want every cell inferred null", view.Cells)
		}
	})

	t.Run("unknown selections are reported distinctly", func(t *testing.T) {
		selection, err := domain.ParseSelection([]byte(
			`{"id":["sex","region"],"dimension":{"sex":{"index":{"F":0}},"region":{"index":{"x":0}}}}`))
		if err != nil {
			t.Fatal(err)
		}
		_, err = database.Query(ctx, provider, dataset, selection, 1_000_000)
		if err == nil || !strings.Contains(err.Error(), ErrInvalidSelection.Error()) {
			t.Fatalf("error = %v, want an invalid-selection error", err)
		}
	})
}

func TestQueryFetchesMoreThanOneBatch(t *testing.T) {
	database := openStore(t)
	ctx := t.Context()
	// 25,000 cells forces three batches of at most 10,000 indexes.
	const size = 25_000
	values := make([]string, size)
	categories := make([]string, size)
	for i := range size {
		values[i] = fmt.Sprintf("%d", i)
		categories[i] = fmt.Sprintf("%q:%d", fmt.Sprintf("c%05d", i), i)
	}
	body := fmt.Sprintf(`{"source_stamp":null,"id":["c"],"dimension":{"c":{"index":{%s}}},"value":[%s]}`,
		strings.Join(categories, ","), strings.Join(values, ","))
	if _, _, err := database.Replace(ctx, parse(t, "SCB", "Big", body)); err != nil {
		t.Fatalf("create: %v", err)
	}
	selection, err := domain.ParseSelection([]byte(fmt.Sprintf(
		`{"id":["c"],"dimension":{"c":{"index":{%s}}}}`, strings.Join(categories, ","))))
	if err != nil {
		t.Fatal(err)
	}
	view, err := database.Query(ctx, code(t, "SCB"), code(t, "Big"), selection, 1_000_000)
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	if len(view.Cells) != size {
		t.Fatalf("cells = %d, want %d across every batch", len(view.Cells), size)
	}
	for i, cell := range view.Cells {
		if cell.Index != int64(i) {
			t.Fatalf("cell %d has output index %d; batching changed the order", i, cell.Index)
		}
		if cell.Numeric == nil || *cell.Numeric != float64(i) {
			t.Fatalf("cell %d = %+v", i, cell)
		}
	}
}

func TestChannelCombinationsRoundTrip(t *testing.T) {
	database := openStore(t)
	ctx := t.Context()
	cases := []struct {
		name        string
		body        string
		cells       int
		valuedCount int64
	}{
		{"numeric only", `{"source_stamp":null,` + twoByTwo + `,"value":[1,2,3,4]}`, 4, 4},
		{"text only", `{"source_stamp":null,` + twoByTwo +
			`,"value":[null,null,null,null],"text":["a","b","c","d"]}`, 4, 4},
		{"mixed channels", `{"source_stamp":null,` + twoByTwo +
			`,"value":[1,null,3,null],"text":[null,"b",null,"d"]}`, 4, 4},
		{"status only", `{"source_stamp":null,` + twoByTwo + `,"value":{},"status":"c"}`, 4, 0},
		{"empty", `{"source_stamp":null,` + twoByTwo + `,"value":{}}`, 0, 0},
		{"sparse", `{"source_stamp":null,` + twoByTwo + `,"value":{"1":2.5},"status":{"2":"e"}}`, 2, 1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dataset := strings.ReplaceAll(tc.name, " ", "-")
			if _, _, err := database.Replace(ctx, parse(t, "SCB", dataset, tc.body)); err != nil {
				t.Fatalf("create: %v", err)
			}
			view, err := database.GetData(ctx, code(t, "SCB"), code(t, dataset))
			if err != nil {
				t.Fatal(err)
			}
			if len(view.Cells) != tc.cells {
				t.Fatalf("stored %d rows, want %d", len(view.Cells), tc.cells)
			}
			if view.Summary.ValuedCellCount != tc.valuedCount {
				t.Fatalf("valued count = %d, want %d", view.Summary.ValuedCellCount, tc.valuedCount)
			}
			if view.Summary.NullCellCount != 4-tc.valuedCount {
				t.Fatalf("null count = %d, want %d", view.Summary.NullCellCount, 4-tc.valuedCount)
			}
			for _, cell := range view.Cells {
				if cell.Numeric != nil && cell.Text != nil {
					t.Fatalf("cell %d holds both a numeric and a text value", cell.Index)
				}
			}
		})
	}
}

func TestNumericFidelityAndSourceStampSemantics(t *testing.T) {
	database := openStore(t)
	ctx := t.Context()
	body := `{"source_stamp":{"b":[1,{"c":null}],"a":"x"},` + twoByTwo +
		`,"value":[0.1,-0,1e308,9007199254740993]}`
	if _, _, err := database.Replace(ctx, parse(t, "SCB", "Numbers", body)); err != nil {
		t.Fatal(err)
	}
	view, err := database.GetData(ctx, code(t, "SCB"), code(t, "Numbers"))
	if err != nil {
		t.Fatal(err)
	}
	stored := make(map[int64]float64)
	for _, cell := range view.Cells {
		stored[cell.Index] = *cell.Numeric
	}
	// Payload indexes 0..3 map to internal 2, 3, 0, 1.
	want := map[int64]float64{2: 0.1, 3: 0, 0: 1e308, 1: 9007199254740993}
	for index, value := range want {
		if stored[index] != value {
			t.Fatalf("internal index %d = %v, want %v", index, stored[index], value)
		}
	}
	var decoded map[string]any
	if err := json.Unmarshal(view.Summary.SourceStamp, &decoded); err != nil {
		t.Fatalf("source stamp %s is not JSON: %v", view.Summary.SourceStamp, err)
	}
	if decoded["a"] != "x" {
		t.Fatalf("source stamp lost meaning: %v", decoded)
	}
	if list, ok := decoded["b"].([]any); !ok || len(list) != 2 {
		t.Fatalf("source stamp lost structure: %v", decoded)
	}
}

func TestPingReportsConnectivity(t *testing.T) {
	database := openStore(t)
	if err := database.Ping(t.Context()); err != nil {
		t.Fatalf("ping: %v", err)
	}
}

func TestMigrationsSerializeOnTheGlobalAdvisoryLock(t *testing.T) {
	url := freshDatabase(t, "")

	t.Run("a held lock makes the migration wait", func(t *testing.T) {
		holder := connect(t, url)
		if _, err := holder.Exec(t.Context(),
			`select pg_advisory_lock($1)`, migrations.AdvisoryLockKey); err != nil {
			t.Fatalf("take the migration lock: %v", err)
		}

		blocked, cancel := context.WithTimeout(t.Context(), 2*time.Second)
		defer cancel()
		err := migrations.Run(blocked, url)
		if err == nil {
			t.Fatal("the migration ran while another session held the lock")
		}
		if !strings.Contains(err.Error(), "context deadline exceeded") {
			t.Fatalf("error = %v, want the migration to block on the lock", err)
		}

		conn := connect(t, url)
		var objects int
		if err := conn.QueryRow(t.Context(), `
			select count(*) from pg_class c join pg_namespace n on n.oid = c.relnamespace
			where n.nspname = 'storage'
		`).Scan(&objects); err != nil {
			t.Fatal(err)
		}
		if objects != 0 {
			t.Fatalf("the blocked migration created %d objects", objects)
		}
		if _, err := holder.Exec(t.Context(),
			`select pg_advisory_unlock($1)`, migrations.AdvisoryLockKey); err != nil {
			t.Fatal(err)
		}
	})

	t.Run("the migration succeeds once the lock is free", func(t *testing.T) {
		if err := migrations.Run(t.Context(), url); err != nil {
			t.Fatalf("migrate: %v", err)
		}
	})

	t.Run("the lock is released afterwards", func(t *testing.T) {
		conn := connect(t, url)
		var held bool
		if err := conn.QueryRow(t.Context(), `
			select exists(
				select 1 from pg_locks
				where locktype = 'advisory'
				  and objid = ($1::bigint & 4294967295)::oid
				  and classid = (($1::bigint >> 32) & 4294967295)::oid)
		`, migrations.AdvisoryLockKey).Scan(&held); err != nil {
			t.Fatal(err)
		}
		if held {
			t.Fatal("the migration left its advisory lock held")
		}
	})
}

func TestConcurrentMigrationsConverge(t *testing.T) {
	url := freshDatabase(t, "")
	const runners = 4
	var group sync.WaitGroup
	errs := make([]error, runners)
	start := make(chan struct{})
	for i := range runners {
		group.Add(1)
		go func() {
			defer group.Done()
			<-start
			errs[i] = migrations.Run(t.Context(), url)
		}()
	}
	close(start)
	group.Wait()
	for i, err := range errs {
		if err != nil {
			t.Fatalf("runner %d: %v", i, err)
		}
	}
	conn := connect(t, url)
	var version int32
	if err := conn.QueryRow(t.Context(), `select version from public.schema_version`).Scan(&version); err != nil {
		t.Fatal(err)
	}
	if version != migrations.ExpectedVersion {
		t.Fatalf("schema version = %d after concurrent migrations", version)
	}
	var partitions int
	if err := conn.QueryRow(t.Context(), `
		select count(*) from pg_inherits i join pg_class p on p.oid = i.inhparent
		join pg_namespace n on n.oid = p.relnamespace
		where n.nspname='storage' and p.relname='observations'
	`).Scan(&partitions); err != nil {
		t.Fatal(err)
	}
	if partitions != 32 {
		t.Fatalf("partitions = %d after concurrent migrations", partitions)
	}
}
