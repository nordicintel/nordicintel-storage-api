package store

import (
	"fmt"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// Single-query latency numbers (see query_bench_test.go and
// millioncell_bench_test.go) don't tell you how the read path behaves as the
// dominant, mostly-concurrent workload -- which is the actual usage pattern
// (writes are a bounded daily window; queries run continuously). The pool
// size (DB_MAX_CONNS, default 4) is a hard ceiling on how many reads can be
// in flight against Postgres at once, and every read (GetData, GetStructure,
// Query) holds a transaction, and therefore a connection, for its whole
// duration. This measures aggregate throughput and per-call latency for many
// concurrent reads at the production-default pool size versus a much larger
// one, to see whether that ceiling is actually a bottleneck for realistic
// concurrent read load. Gated the same as the other million-cell-scale
// benchmarks; the dataset here is modest (50,000 cells) but the point is
// concurrency, not per-query data volume.
func requireConcurrentReadBenchmark(t *testing.T) {
	t.Helper()
	requireMillionCellBenchmark(t)
}

const (
	concurrentReadSide  = 50
	concurrentReadCells = concurrentReadSide * concurrentReadSide * concurrentReadSide // 125,000
)

func concurrentReadDatasetBody() string {
	var b strings.Builder
	b.WriteString(`{"source_stamp":{"generation":"concurrent-read"},"id":["a","b","c"],"dimension":{`)
	for d, name := range []string{"a", "b", "c"} {
		if d > 0 {
			b.WriteByte(',')
		}
		fmt.Fprintf(&b, `%q:{"index":{`, name)
		for i := range concurrentReadSide {
			if i > 0 {
				b.WriteByte(',')
			}
			fmt.Fprintf(&b, `"%s%03d":%d`, name, i, i)
		}
		b.WriteString(`}}`)
	}
	b.WriteString(`},"value":[`)
	for i := range concurrentReadCells {
		if i > 0 {
			b.WriteByte(',')
		}
		b.WriteString(strconv.Itoa(i))
	}
	b.WriteString(`]}`)
	return b.String()
}

// runConcurrentReads fires `concurrency` goroutines, each performing `perGoroutine`
// sequential GetData calls, against a pool capped at maxConns, and reports
// total wall time plus min/median/max per-call latency.
func runConcurrentReads(t *testing.T, maxConns int32, concurrency, perGoroutine int) {
	t.Helper()
	url := migratedDatabase(t)
	database, err := Open(t.Context(), url, maxConns)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer database.Close()
	ctx := t.Context()

	if _, _, err := database.Replace(ctx, parse(t, "SCB", "Concurrent", concurrentReadDatasetBody())); err != nil {
		t.Fatalf("seed: %v", err)
	}
	provider, dataset := code(t, "SCB"), code(t, "Concurrent")

	total := concurrency * perGoroutine
	latencies := make([]time.Duration, total)
	var wg sync.WaitGroup
	started := time.Now()
	for g := range concurrency {
		wg.Add(1)
		go func(worker int) {
			defer wg.Done()
			for i := range perGoroutine {
				callStarted := time.Now()
				if _, err := database.GetData(ctx, provider, dataset); err != nil {
					t.Errorf("worker %d call %d: %v", worker, i, err)
					return
				}
				latencies[worker*perGoroutine+i] = time.Since(callStarted)
			}
		}(g)
	}
	wg.Wait()
	elapsed := time.Since(started)

	sorted := append([]time.Duration(nil), latencies...)
	for i := 1; i < len(sorted); i++ {
		for j := i; j > 0 && sorted[j] < sorted[j-1]; j-- {
			sorted[j], sorted[j-1] = sorted[j-1], sorted[j]
		}
	}
	t.Logf("pool=%d concurrency=%d calls=%d total=%s throughput=%.1f/s min=%s p50=%s p95=%s max=%s",
		maxConns, concurrency, total, elapsed.Round(time.Millisecond),
		float64(total)/elapsed.Seconds(),
		sorted[0].Round(time.Millisecond),
		sorted[len(sorted)/2].Round(time.Millisecond),
		sorted[len(sorted)*95/100].Round(time.Millisecond),
		sorted[len(sorted)-1].Round(time.Millisecond))
}

func TestSerialReadBaseline(t *testing.T) {
	requireConcurrentReadBenchmark(t)
	runConcurrentReads(t, 4, 1, 10)
}

func TestConcurrentReadsAtDefaultPoolSize(t *testing.T) {
	requireConcurrentReadBenchmark(t)
	runConcurrentReads(t, 4, 32, 5)
}

func TestConcurrentReadsAtALargerPoolSize(t *testing.T) {
	requireConcurrentReadBenchmark(t)
	runConcurrentReads(t, 32, 32, 5)
}
