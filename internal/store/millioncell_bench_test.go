package store

import (
	"context"
	"fmt"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"
)

// This breaks Postgres.Replace into phases using the transactionCheckpoints
// hooks that transaction_test.go otherwise uses only for fault injection, so
// a million-cell replacement's wall-clock time can be attributed to "lock and
// dataset upsert/delete", "the dimension and category insert loop", "the
// observation COPY plus the final summary update", and "commit" instead of
// being one opaque total. It requires a live database (see openStore), and is
// expensive on top of that, so it runs only when MILLION_CELL_TEST is set,
// matching internal/httpapi/millioncell_test.go's gate.
func requireMillionCellBenchmark(t *testing.T) {
	t.Helper()
	if os.Getenv("MILLION_CELL_TEST") == "" {
		t.Skip("MILLION_CELL_TEST is not set; skipping the million-cell phase benchmark")
	}
}

const (
	benchMillionSide  = 100
	benchMillionCells = benchMillionSide * benchMillionSide * benchMillionSide
)

func benchMillionCellStructure() string {
	var b strings.Builder
	b.WriteString(`"id":["a","b","c"],"dimension":{`)
	for d, name := range []string{"a", "b", "c"} {
		if d > 0 {
			b.WriteByte(',')
		}
		fmt.Fprintf(&b, `%q:{"index":{`, name)
		for i := range benchMillionSide {
			if i > 0 {
				b.WriteByte(',')
			}
			fmt.Fprintf(&b, `"%s%03d":%d`, name, i, i)
		}
		b.WriteString(`}}`)
	}
	b.WriteString(`}`)
	return b.String()
}

func benchDenseMillionCellBody(replace bool) string {
	var b strings.Builder
	b.Grow(12 << 20)
	b.WriteByte('{')
	if replace {
		b.WriteString(`"replace":true,`)
	}
	b.WriteString(`"source_stamp":{"generation":"dense"},`)
	b.WriteString(benchMillionCellStructure())
	b.WriteString(`,"value":[`)
	for i := range benchMillionCells {
		if i > 0 {
			b.WriteByte(',')
		}
		b.WriteString(strconv.Itoa(i))
	}
	b.WriteString(`],"status":"a"}`)
	return b.String()
}

func benchSparseMillionCellBody(replace bool) string {
	var b strings.Builder
	b.Grow(16 << 20)
	b.WriteByte('{')
	if replace {
		b.WriteString(`"replace":true,`)
	}
	b.WriteString(`"source_stamp":{"generation":"sparse"},`)
	b.WriteString(benchMillionCellStructure())
	b.WriteString(`,"value":{`)
	for i := range benchMillionCells {
		if i > 0 {
			b.WriteByte(',')
		}
		fmt.Fprintf(&b, `"%d":%d`, i, i)
	}
	b.WriteString(`}}`)
	return b.String()
}

// phaseCheckpoints installs timing checkpoints on database and returns a
// function that logs the four phase durations plus the total once Replace
// has returned.
func phaseCheckpoints(t *testing.T, database *Postgres, label string) func() {
	t.Helper()
	var structureAt, copyAt, commitAt time.Time
	start := time.Now()
	database.checkpoints = &transactionCheckpoints{
		BeforeStructure: func(context.Context) error { structureAt = time.Now(); return nil },
		BeforeCopy:      func(context.Context) error { copyAt = time.Now(); return nil },
		BeforeCommit:    func(context.Context) error { commitAt = time.Now(); return nil },
	}
	return func() {
		finished := time.Now()
		database.checkpoints = nil
		t.Logf("%s: lock+upsert/delete=%s structure=%s copy+update=%s commit=%s total=%s",
			label,
			structureAt.Sub(start).Round(time.Millisecond),
			copyAt.Sub(structureAt).Round(time.Millisecond),
			commitAt.Sub(copyAt).Round(time.Millisecond),
			finished.Sub(commitAt).Round(time.Millisecond),
			finished.Sub(start).Round(time.Millisecond))
	}
}

func TestReplacePhaseTimingMillionCellCreate(t *testing.T) {
	requireMillionCellBenchmark(t)
	database := openStore(t)
	ctx := t.Context()
	replacement := parse(t, "SCB", "Million", benchDenseMillionCellBody(false))

	logPhases := phaseCheckpoints(t, database, "create (dense)")
	result, _, err := database.Replace(ctx, replacement)
	logPhases()
	if err != nil {
		t.Fatalf("replace: %v", err)
	}
	if result != "created" {
		t.Fatalf("result = %q, want created", result)
	}
}

func TestReplacePhaseTimingMillionCellReplaceExisting(t *testing.T) {
	requireMillionCellBenchmark(t)
	database := openStore(t)
	ctx := t.Context()
	if _, _, err := database.Replace(ctx, parse(t, "SCB", "Million", benchDenseMillionCellBody(false))); err != nil {
		t.Fatalf("seed: %v", err)
	}
	replacement := parse(t, "SCB", "Million", benchSparseMillionCellBody(true))

	logPhases := phaseCheckpoints(t, database, "replace-existing (sparse)")
	result, _, err := database.Replace(ctx, replacement)
	logPhases()
	if err != nil {
		t.Fatalf("replace: %v", err)
	}
	if result != "replaced" {
		t.Fatalf("result = %q, want replaced", result)
	}
}
