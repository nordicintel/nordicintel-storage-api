package httpapi

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/nordicintel/nordicintel-storage-api/internal/domain"
)

// The million-cell test is the acceptance gate for the documented limit. It is
// expensive, so it runs only when MILLION_CELL_TEST is set; the manual CI
// workflow sets it, and a developer can set it locally.
func requireMillionCellTest(t *testing.T) {
	t.Helper()
	if os.Getenv("MILLION_CELL_TEST") == "" {
		t.Skip("MILLION_CELL_TEST is not set; skipping the million-cell acceptance test")
	}
}

const (
	millionSide  = 100
	millionCells = millionSide * millionSide * millionSide
)

// millionCellStructure builds a 100x100x100 cube. Category codes are zero
// padded so their normalized order matches their payload order, which keeps the
// expected values easy to state.
func millionCellStructure() string {
	var builder strings.Builder
	builder.WriteString(`"id":["a","b","c"],"dimension":{`)
	for d, name := range []string{"a", "b", "c"} {
		if d > 0 {
			builder.WriteByte(',')
		}
		fmt.Fprintf(&builder, `%q:{"index":{`, name)
		for i := range millionSide {
			if i > 0 {
				builder.WriteByte(',')
			}
			fmt.Fprintf(&builder, `"%s%03d":%d`, name, i, i)
		}
		builder.WriteString(`}}`)
	}
	builder.WriteString(`}`)
	return builder.String()
}

func denseMillionCellBody(t *testing.T, replace bool) string {
	t.Helper()
	var builder strings.Builder
	builder.Grow(12 << 20)
	builder.WriteByte('{')
	if replace {
		builder.WriteString(`"replace":true,`)
	}
	builder.WriteString(`"source_stamp":{"generation":"dense"},`)
	builder.WriteString(millionCellStructure())
	builder.WriteString(`,"value":[`)
	for i := range millionCells {
		if i > 0 {
			builder.WriteByte(',')
		}
		builder.WriteString(strconv.Itoa(i))
	}
	builder.WriteString(`],"status":"a"}`)
	return builder.String()
}

func sparseMillionCellBody(t *testing.T, replace bool) string {
	t.Helper()
	var builder strings.Builder
	builder.Grow(16 << 20)
	builder.WriteByte('{')
	if replace {
		builder.WriteString(`"replace":true,`)
	}
	builder.WriteString(`"source_stamp":{"generation":"sparse"},`)
	builder.WriteString(millionCellStructure())
	builder.WriteString(`,"value":{`)
	for i := range millionCells {
		if i > 0 {
			builder.WriteByte(',')
		}
		fmt.Fprintf(&builder, `"%d":%d`, i, i)
	}
	builder.WriteString(`}}`)
	return builder.String()
}

// logDomainParseOnly times domain.ParseReplacement in isolation, with no HTTP
// or database work involved, so it can be compared against the full HTTP
// round-trip timed by reportUsage below and against
// internal/store's phase-checkpoint timings, to see how much of the total
// belongs to Go-side JSON decode/remapping versus Postgres.
func logDomainParseOnly(t *testing.T, label, body string) {
	t.Helper()
	started := time.Now()
	if _, err := domain.ParseReplacement("SCB", "Million", []byte(body), 1_000_000); err != nil {
		t.Fatalf("domain-only parse: %v", err)
	}
	t.Logf("%s domain.ParseReplacement (isolated, no HTTP/DB): duration=%s",
		label, time.Since(started).Round(time.Millisecond))
}

func reportUsage(t *testing.T, label string, started time.Time, responseBytes int) {
	t.Helper()
	var memory runtime.MemStats
	runtime.ReadMemStats(&memory)
	t.Logf("%s: duration=%s response_bytes=%d heap_in_use=%s total_alloc=%s sys=%s",
		label, time.Since(started).Round(time.Millisecond), responseBytes,
		humanBytes(memory.HeapInuse), humanBytes(memory.TotalAlloc), humanBytes(memory.Sys))
}

func humanBytes(value uint64) string {
	switch {
	case value >= 1<<30:
		return fmt.Sprintf("%.2fGiB", float64(value)/(1<<30))
	case value >= 1<<20:
		return fmt.Sprintf("%.1fMiB", float64(value)/(1<<20))
	default:
		return fmt.Sprintf("%dKiB", value/(1<<10))
	}
}

func TestMillionCellDatasetRoundTrips(t *testing.T) {
	requireMillionCellTest(t)
	h := newLiveHarness(t)
	const path = "/v1/providers/SCB/datasets/Million"

	t.Run("fully populated dense replacement", func(t *testing.T) {
		body := denseMillionCellBody(t, false)
		t.Logf("dense request body: %s", humanBytes(uint64(len(body))))
		logDomainParseOnly(t, "dense", body)
		started := time.Now()
		got := h.expect(t, http.StatusCreated, http.MethodPost, path, writeToken, body)
		reportUsage(t, "dense replacement", started, len(got.body))
		summary := got.decode(t)["dataset"].(map[string]any)
		if summary["cell_count"] != float64(millionCells) ||
			summary["valued_cell_count"] != float64(millionCells) ||
			summary["null_cell_count"] != float64(0) {
			t.Fatalf("summary = %v", summary)
		}
	})

	t.Run("full dense read", func(t *testing.T) {
		started := time.Now()
		got := h.expect(t, http.StatusOK, http.MethodGet, path+"/data?format=dense", readToken, "")
		reportUsage(t, "dense read", started, len(got.body))
		var decoded struct {
			CellCount int64     `json:"cell_count"`
			Value     []float64 `json:"value"`
			Status    string    `json:"status"`
		}
		if err := json.Unmarshal(got.body, &decoded); err != nil {
			t.Fatalf("decode: %v", err)
		}
		if len(decoded.Value) != millionCells {
			t.Fatalf("value length = %d, want %d", len(decoded.Value), millionCells)
		}
		if decoded.Status != "a" {
			t.Fatalf("status = %q, want the scalar a", decoded.Status)
		}
		for _, index := range []int{0, 1, millionCells / 2, millionCells - 1} {
			if decoded.Value[index] != float64(index) {
				t.Fatalf("value[%d] = %v, want %d", index, decoded.Value[index], index)
			}
		}
	})

	t.Run("full sparse read", func(t *testing.T) {
		started := time.Now()
		got := h.expect(t, http.StatusOK, http.MethodGet, path+"/data", readToken, "")
		reportUsage(t, "sparse read", started, len(got.body))
		var decoded struct {
			Value map[string]float64 `json:"value"`
		}
		if err := json.Unmarshal(got.body, &decoded); err != nil {
			t.Fatalf("decode: %v", err)
		}
		if len(decoded.Value) != millionCells {
			t.Fatalf("value entries = %d, want %d", len(decoded.Value), millionCells)
		}
		if decoded.Value["999999"] != 999999 {
			t.Fatalf("value[999999] = %v", decoded.Value["999999"])
		}
	})

	t.Run("fully populated sparse replacement", func(t *testing.T) {
		body := sparseMillionCellBody(t, true)
		t.Logf("sparse request body: %s", humanBytes(uint64(len(body))))
		logDomainParseOnly(t, "sparse", body)
		started := time.Now()
		got := h.expect(t, http.StatusOK, http.MethodPost, path, writeToken, body)
		reportUsage(t, "sparse replacement", started, len(got.body))
		summary := got.decode(t)["dataset"].(map[string]any)
		if summary["valued_cell_count"] != float64(millionCells) {
			t.Fatalf("summary = %v", summary)
		}
		read := h.expect(t, http.StatusOK, http.MethodGet, path+"/data?format=dense", readToken, "").decode(t)
		if _, present := read["status"]; present {
			t.Fatal("the sparse replacement did not clear the previous scalar status")
		}
	})

	t.Run("reordered exact subset", func(t *testing.T) {
		// Reverse dimension b and select a 100x100 slice of dimension a and c.
		var builder strings.Builder
		builder.WriteString(`{"id":["c","b","a"],"dimension":{`)
		builder.WriteString(`"c":{"index":{"c000":0}},`)
		builder.WriteString(`"b":{"index":{`)
		for i := range millionSide {
			if i > 0 {
				builder.WriteByte(',')
			}
			fmt.Fprintf(&builder, `"b%03d":%d`, millionSide-1-i, i)
		}
		builder.WriteString(`}},"a":{"index":{`)
		for i := range millionSide {
			if i > 0 {
				builder.WriteByte(',')
			}
			fmt.Fprintf(&builder, `"a%03d":%d`, i, i)
		}
		builder.WriteString(`}}}}`)

		started := time.Now()
		got := h.expect(t, http.StatusOK, http.MethodPost, path+"/query?format=dense", readToken, builder.String())
		reportUsage(t, "reordered subset", started, len(got.body))
		var decoded struct {
			CellCount int64     `json:"cell_count"`
			Value     []float64 `json:"value"`
		}
		if err := json.Unmarshal(got.body, &decoded); err != nil {
			t.Fatalf("decode: %v", err)
		}
		if len(decoded.Value) != millionSide*millionSide {
			t.Fatalf("value length = %d, want %d", len(decoded.Value), millionSide*millionSide)
		}
		if decoded.CellCount != millionCells {
			t.Fatalf("cell_count = %d, want the whole-dataset count", decoded.CellCount)
		}
		// Output order is c, b, a with strides [100*100, 100, 1] over sizes
		// [1, 100, 100]. Output index = bOut*100 + aOut, and the stored value
		// at (a, b, c) is a*10000 + b*100 + c with c fixed at 0.
		for _, probe := range [][2]int{{0, 0}, {0, 99}, {99, 0}, {50, 50}, {99, 99}} {
			bOut, aOut := probe[0], probe[1]
			b := millionSide - 1 - bOut
			want := float64(aOut*millionSide*millionSide + b*millionSide)
			if got := decoded.Value[bOut*millionSide+aOut]; got != want {
				t.Fatalf("output (b=%d, a=%d) = %v, want %v", bOut, aOut, got, want)
			}
		}
	})
}
