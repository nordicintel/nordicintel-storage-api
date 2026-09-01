package store

import (
	"context"
	"encoding/json"
	"os"
	"strconv"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/tern/v2/migrate"
	"github.com/nordicintel/nordicintel-storage-api/internal/domain"
	"github.com/nordicintel/nordicintel-storage-api/internal/migrations"
)

func TestPostgresLifecycle(t *testing.T) {
	databaseURL := os.Getenv("TEST_DATABASE_URL")
	if databaseURL == "" {
		t.Skip("TEST_DATABASE_URL is not set")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	conn, err := pgx.Connect(ctx, databaseURL)
	if err != nil {
		t.Fatal(err)
	}
	migrator, err := migrate.NewMigrator(ctx, conn, "public.schema_version")
	if err != nil {
		t.Fatal(err)
	}
	if err := migrator.LoadMigrations(migrations.FS()); err != nil {
		t.Fatal(err)
	}
	if err := migrator.MigrateTo(ctx, 0); err != nil {
		t.Fatal(err)
	}
	if err := migrator.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	if err := conn.Close(ctx); err != nil {
		t.Fatal(err)
	}

	db, err := Open(ctx, databaseURL, 4, 1_000_000)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	ten := domain.Decimal("10.000")
	status := "p"
	result, err := db.Replace(ctx, ReplaceInput{
		ProviderCode: "Provider", DatasetCode: "Dataset",
		Dimensions:  testDimensions,
		SourceStamp: json.RawMessage(`{"etag":"one"}`),
		Cells: []domain.Cell{
			{Index: 0, Value: &ten},
			{Index: 1, StatusCode: &status},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if !result.Created || result.ObservationCount != 2 {
		t.Fatalf("unexpected replace result: %#v", result)
	}

	full, err := db.FullData(ctx, " provider ", "DATASET")
	if err != nil {
		t.Fatal(err)
	}
	if full.CellCount != 4 || len(full.Cells) != 2 || string(*full.Cells[0].Value) != "10.000" {
		t.Fatalf("unexpected full data: %#v", full)
	}
	if _, err := db.Replace(ctx, ReplaceInput{
		ProviderCode: "Provider", DatasetCode: "Dataset", Dimensions: testDimensions,
		Cells: []domain.Cell{{Index: 0, Value: &ten}, {Index: 0, Value: &ten}},
	}); err == nil {
		t.Fatal("expected duplicate COPY rows to fail")
	}
	afterRollback, err := db.FullData(ctx, "Provider", "Dataset")
	if err != nil {
		t.Fatal(err)
	}
	if len(afterRollback.Cells) != 2 || string(*afterRollback.Cells[0].Value) != "10.000" {
		t.Fatalf("failed replacement was not atomic: %#v", afterRollback)
	}

	patch, err := db.Patch(ctx, "Provider", "Dataset", domain.PatchRequest{
		Observations: []domain.PatchObservation{
			{Categories: map[string]string{"Sex": "M", "Year": "2024"}, Value: json.RawMessage("11")},
			{Categories: map[string]string{"Sex": "M", "Year": "2025"}, Delete: json.RawMessage("true")},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if patch.UpdatedCount != 1 || patch.DeletedCount != 1 || patch.ObservationCount != 1 {
		t.Fatalf("unexpected patch result: %#v", patch)
	}

	selected, err := db.SelectedData(ctx, "Provider", "Dataset", []domain.Dimension{
		{Code: "Year", Categories: []string{"2024"}},
		{Code: "Sex", Categories: []string{"F", "M"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if selected.CellCount != 2 || len(selected.Cells) != 1 || selected.Cells[0].Index != 1 {
		t.Fatalf("unexpected selected data: %#v", selected)
	}

	metadata, err := db.Metadata(ctx, "Provider", "Dataset")
	if err != nil {
		t.Fatal(err)
	}
	previousTimestamp := metadata.ObservationsUpdatedAt
	if string(metadata.SourceStamp) != `{"etag": "one"}` && string(metadata.SourceStamp) != `{"etag":"one"}` {
		t.Fatalf("omitted source stamp was not retained: %s", metadata.SourceStamp)
	}
	noOp, err := db.Patch(ctx, "Provider", "Dataset", domain.PatchRequest{
		SourceStamp: json.RawMessage("null"),
		Observations: []domain.PatchObservation{{
			Categories: map[string]string{"Sex": "M", "Year": "2024"}, Value: json.RawMessage("11.0"),
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if noOp.UpdatedCount != 0 || !noOp.ObservationsUpdatedAt.Equal(*previousTimestamp) {
		t.Fatalf("numeric no-op changed state: %#v", noOp)
	}
	metadata, err = db.Metadata(ctx, "Provider", "Dataset")
	if err != nil || string(metadata.SourceStamp) != "null" {
		t.Fatalf("explicit null did not clear source stamp: %s, %v", metadata.SourceStamp, err)
	}

	if err := db.Delete(ctx, "provider", "dataset"); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Metadata(ctx, "Provider", "Dataset"); err != ErrNotFound {
		t.Fatalf("metadata after delete error = %v, want ErrNotFound", err)
	}
}

func BenchmarkPostgresReplaceMillionCells(b *testing.B) {
	if os.Getenv("RUN_MILLION_CELL_BENCHMARK") != "1" {
		b.Skip("set RUN_MILLION_CELL_BENCHMARK=1 with TEST_DATABASE_URL to run")
	}
	databaseURL := os.Getenv("TEST_DATABASE_URL")
	if databaseURL == "" {
		b.Skip("TEST_DATABASE_URL is not set")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	conn, err := pgx.Connect(ctx, databaseURL)
	if err != nil {
		b.Fatal(err)
	}
	migrator, err := migrate.NewMigrator(ctx, conn, "public.schema_version")
	if err != nil {
		b.Fatal(err)
	}
	if err := migrator.LoadMigrations(migrations.FS()); err != nil {
		b.Fatal(err)
	}
	if err := migrator.Migrate(ctx); err != nil {
		b.Fatal(err)
	}
	if err := conn.Close(ctx); err != nil {
		b.Fatal(err)
	}
	db, err := Open(ctx, databaseURL, 4, 1_000_000)
	if err != nil {
		b.Fatal(err)
	}
	defer db.Close()
	categories := make([]string, 1000)
	for i := range categories {
		categories[i] = strconv.Itoa(i)
	}
	dimensions := []domain.Dimension{
		{Code: "Row", Categories: categories},
		{Code: "Column", Categories: categories},
	}
	value := domain.Decimal("1.25")
	cells := make([]domain.Cell, 1_000_000)
	for i := range cells {
		cells[i] = domain.Cell{Index: int64(i), Value: &value}
	}
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		started := time.Now()
		_, err := db.Replace(ctx, ReplaceInput{
			ProviderCode: "Benchmark", DatasetCode: "MillionCells",
			Dimensions: dimensions, Cells: cells,
		})
		if err != nil {
			b.Fatal(err)
		}
		b.ReportMetric(float64(time.Since(started).Milliseconds()), "ms/replace")
	}
}
