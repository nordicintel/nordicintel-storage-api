package domain

import (
	"fmt"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"
)

// This isolates ParseReplacement's cost (JSON decode plus remapReplacement's
// per-cell coordinate remapping and the final sort.Slice) from every database
// concern, so it can be compared against internal/store's phase timings and
// internal/httpapi's full round-trip timings to find where a million-cell
// replacement actually spends its time. It is expensive to build the payload
// even before parsing it, so it runs only when MILLION_CELL_TEST is set,
// matching internal/httpapi/millioncell_test.go's gate.
func requireMillionCellBenchmark(t *testing.T) {
	t.Helper()
	if os.Getenv("MILLION_CELL_TEST") == "" {
		t.Skip("MILLION_CELL_TEST is not set; skipping the million-cell parse benchmark")
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

func benchDenseMillionCellBody() string {
	var b strings.Builder
	b.Grow(12 << 20)
	b.WriteByte('{')
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

func benchSparseMillionCellBody() string {
	var b strings.Builder
	b.Grow(16 << 20)
	b.WriteByte('{')
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

func TestParseReplacementMillionCellDense(t *testing.T) {
	requireMillionCellBenchmark(t)
	body := benchDenseMillionCellBody()
	t.Logf("dense body: %.1fMiB", float64(len(body))/(1<<20))

	decodeStarted := time.Now()
	replacement, err := ParseReplacement("SCB", "Million", []byte(body), 1_000_000)
	elapsed := time.Since(decodeStarted)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(replacement.Cells) != benchMillionCells {
		t.Fatalf("cells = %d, want %d", len(replacement.Cells), benchMillionCells)
	}
	t.Logf("dense ParseReplacement (JSON decode + remapReplacement, no DB): duration=%s",
		elapsed.Round(time.Millisecond))
}

func TestParseReplacementMillionCellSparse(t *testing.T) {
	requireMillionCellBenchmark(t)
	body := benchSparseMillionCellBody()
	t.Logf("sparse body: %.1fMiB", float64(len(body))/(1<<20))

	decodeStarted := time.Now()
	replacement, err := ParseReplacement("SCB", "Million", []byte(body), 1_000_000)
	elapsed := time.Since(decodeStarted)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(replacement.Cells) != benchMillionCells {
		t.Fatalf("cells = %d, want %d", len(replacement.Cells), benchMillionCells)
	}
	t.Logf("sparse ParseReplacement (JSON decode + remapReplacement, no DB): duration=%s",
		elapsed.Round(time.Millisecond))
}
