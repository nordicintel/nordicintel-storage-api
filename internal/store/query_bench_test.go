package store

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/nordicintel/nordicintel-storage-api/internal/domain"
)

// This isolates setup time from query time for a single high-cardinality
// dimension (one dimension with many categories, as opposed to the
// million-cell benchmark's three balanced 100-category dimensions), because
// TestQueryFetchesMoreThanOneBatch's ~11s runtime conflates the two and
// Postgres.Replace's dimension/category insert loop
// (internal/store/postgres.go) inserts one category per round trip -- a
// dataset shape the million-cell benchmark never exercises, since it spreads
// its categories across three dimensions instead of concentrating them in
// one. Requires a live database (see openStore); gated the same way as the
// other million-cell-scale benchmarks since building a 25,000-category
// payload isn't free either.
func TestQueryBenchmarkHighCardinalityDimension(t *testing.T) {
	requireMillionCellBenchmark(t)
	database := openStore(t)
	ctx := t.Context()

	const size = 25_000
	values := make([]string, size)
	categories := make([]string, size)
	for i := range size {
		values[i] = fmt.Sprintf("%d", i)
		categories[i] = fmt.Sprintf("%q:%d", fmt.Sprintf("c%05d", i), i)
	}
	body := fmt.Sprintf(`{"source_stamp":null,"id":["c"],"dimension":{"c":{"index":{%s}}},"value":[%s]}`,
		strings.Join(categories, ","), strings.Join(values, ","))

	replacement := parse(t, "SCB", "Big", body)
	logPhases := phaseCheckpoints(t, database, "Replace with one 25,000-category dimension")
	if _, _, err := database.Replace(ctx, replacement); err != nil {
		t.Fatalf("seed: %v", err)
	}
	logPhases()

	selection, err := domain.ParseSelection([]byte(fmt.Sprintf(
		`{"id":["c"],"dimension":{"c":{"index":{%s}}}}`, strings.Join(categories, ","))))
	if err != nil {
		t.Fatal(err)
	}
	queryStarted := time.Now()
	view, err := database.Query(ctx, code(t, "SCB"), code(t, "Big"), selection, 1_000_000)
	queryElapsed := time.Since(queryStarted)
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	if len(view.Cells) != size {
		t.Fatalf("cells = %d, want %d", len(view.Cells), size)
	}
	t.Logf("Query for all 25,000 cells (3 batches of 10,000): %s", queryElapsed.Round(time.Millisecond))

	small, err := domain.ParseSelection([]byte(`{"id":["c"],"dimension":{"c":{"index":{"c00000":0,"c00001":1,"c00099":2}}}}`))
	if err != nil {
		t.Fatal(err)
	}
	smallStarted := time.Now()
	if _, err := database.Query(ctx, code(t, "SCB"), code(t, "Big"), small, 1_000_000); err != nil {
		t.Fatalf("small query: %v", err)
	}
	t.Logf("Query for 3 specific cells out of 25,000: %s", time.Since(smallStarted).Round(time.Millisecond))

	getDataStarted := time.Now()
	if _, err := database.GetData(ctx, code(t, "SCB"), code(t, "Big")); err != nil {
		t.Fatalf("get data: %v", err)
	}
	t.Logf("GetData (full read) of the 25,000-cell dataset: %s", time.Since(getDataStarted).Round(time.Millisecond))
}
