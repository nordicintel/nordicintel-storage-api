// Command millioncell drives the release acceptance gate against a running API
// container. It exercises fully populated dense and sparse replacements, full
// dense and sparse reads, a reordered exact subset, atomic visibility during a
// replacement, and a forced rollback, then writes a JSON report.
//
// It is a test driver, not part of the service: the Dockerfile never copies it,
// so it cannot reach the runtime image.
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintf(os.Stderr, "million-cell gate failed: %v\n", err)
		os.Exit(1)
	}
}

type config struct {
	baseURL    string
	writeToken string
	readToken  string
	provider   string
	dataset    string
	side       int
	reportPath string
	timeout    time.Duration
}

// measurement records one exercised operation for the published report.
type measurement struct {
	Name          string  `json:"name"`
	DurationMS    int64   `json:"duration_ms"`
	RequestBytes  int64   `json:"request_bytes"`
	ResponseBytes int64   `json:"response_bytes"`
	Status        int     `json:"status"`
	RequestID     string  `json:"request_id"`
	Throughput    float64 `json:"cells_per_second,omitempty"`
}

type report struct {
	GeneratedAt  time.Time     `json:"generated_at"`
	CellCount    int           `json:"cell_count"`
	Dimensions   []int         `json:"dimensions"`
	Measurements []measurement `json:"measurements"`
	Checks       []string      `json:"checks_passed"`
}

func run() error {
	var c config
	flag.StringVar(&c.baseURL, "base-url", "http://localhost:8080", "API base URL")
	flag.StringVar(&c.writeToken, "write-token", os.Getenv("API_READ_WRITE_TOKEN"), "read/write bearer token")
	flag.StringVar(&c.readToken, "read-token", os.Getenv("API_READ_ONLY_TOKEN"), "read-only bearer token")
	flag.StringVar(&c.provider, "provider", "AcceptanceProvider", "provider code")
	flag.StringVar(&c.dataset, "dataset", "MillionCell", "dataset code")
	flag.IntVar(&c.side, "side", 100, "length of each of the three dimensions")
	flag.StringVar(&c.reportPath, "report", "million-cell-report.json", "where to write the JSON report")
	flag.DurationVar(&c.timeout, "timeout", 10*time.Minute, "per-request timeout")
	flag.Parse()

	if c.writeToken == "" || c.readToken == "" {
		return errors.New("both -write-token and -read-token are required")
	}
	cells := c.side * c.side * c.side
	driver := &driver{
		config: c,
		cells:  cells,
		client: &http.Client{Timeout: c.timeout},
		report: report{
			GeneratedAt: time.Now().UTC(),
			CellCount:   cells,
			Dimensions:  []int{c.side, c.side, c.side},
		},
	}
	fmt.Printf("driving %s with a %d cell dataset\n", c.baseURL, cells)

	steps := []struct {
		name string
		fn   func() error
	}{
		{"clean slate", driver.deleteDataset},
		{"dense replacement", driver.denseReplacement},
		{"full dense read", driver.fullDenseRead},
		{"full sparse read", driver.fullSparseRead},
		{"reordered exact subset", driver.reorderedSubset},
		{"sparse replacement", driver.sparseReplacement},
		{"atomic visibility", driver.atomicVisibility},
		{"forced rollback", driver.forcedRollback},
		{"deletion", driver.finalDelete},
	}
	for _, step := range steps {
		fmt.Printf("-- %s\n", step.name)
		if err := step.fn(); err != nil {
			return fmt.Errorf("%s: %w", step.name, err)
		}
		driver.report.Checks = append(driver.report.Checks, step.name)
	}

	encoded, err := json.MarshalIndent(driver.report, "", "  ")
	if err != nil {
		return err
	}
	if err := os.WriteFile(c.reportPath, append(encoded, '\n'), 0o644); err != nil {
		return fmt.Errorf("write report: %w", err)
	}
	fmt.Printf("\nreport written to %s\n%s\n", c.reportPath, encoded)
	return nil
}

type driver struct {
	config config
	cells  int
	client *http.Client
	report report
}

func (d *driver) path(suffix string) string {
	return fmt.Sprintf("%s/v1/providers/%s/datasets/%s%s",
		strings.TrimSuffix(d.config.baseURL, "/"), d.config.provider, d.config.dataset, suffix)
}

// structure writes the shared 3-dimensional structure. Category codes are zero
// padded so their normalized order matches their payload order, which keeps the
// expected values arithmetic.
func (d *driver) structure(w io.Writer) error {
	if _, err := io.WriteString(w, `"id":["a","b","c"],"dimension":{`); err != nil {
		return err
	}
	for index, name := range []string{"a", "b", "c"} {
		if index > 0 {
			if _, err := io.WriteString(w, ","); err != nil {
				return err
			}
		}
		if _, err := fmt.Fprintf(w, `%q:{"index":{`, name); err != nil {
			return err
		}
		for i := range d.config.side {
			separator := ","
			if i == 0 {
				separator = ""
			}
			if _, err := fmt.Fprintf(w, `%s"%s%04d":%d`, separator, name, i, i); err != nil {
				return err
			}
		}
		if _, err := io.WriteString(w, `}}`); err != nil {
			return err
		}
	}
	_, err := io.WriteString(w, `}`)
	return err
}

// streamBody builds a request body incrementally so a fully populated payload
// never has to exist in the driver's memory at once.
func (d *driver) streamBody(write func(io.Writer) error) (io.ReadCloser, *int64) {
	reader, writer := io.Pipe()
	var counted int64
	counter := &countingWriter{inner: writer, total: &counted}
	go func() {
		writer.CloseWithError(write(counter))
	}()
	return reader, &counted
}

type countingWriter struct {
	inner io.Writer
	total *int64
}

func (w *countingWriter) Write(data []byte) (int, error) {
	n, err := w.inner.Write(data)
	*w.total += int64(n)
	return n, err
}

func (d *driver) denseBody(replace bool) func(io.Writer) error {
	return func(w io.Writer) error {
		if _, err := io.WriteString(w, "{"); err != nil {
			return err
		}
		if replace {
			if _, err := io.WriteString(w, `"replace":true,`); err != nil {
				return err
			}
		}
		if _, err := io.WriteString(w, `"source_stamp":{"generation":"dense"},`); err != nil {
			return err
		}
		if err := d.structure(w); err != nil {
			return err
		}
		if _, err := io.WriteString(w, `,"value":[`); err != nil {
			return err
		}
		buffer := make([]byte, 0, 16)
		for i := range d.cells {
			buffer = buffer[:0]
			if i > 0 {
				buffer = append(buffer, ',')
			}
			buffer = strconv.AppendInt(buffer, int64(i), 10)
			if _, err := w.Write(buffer); err != nil {
				return err
			}
		}
		_, err := io.WriteString(w, `],"status":"a"}`)
		return err
	}
}

func (d *driver) sparseBody(replace bool) func(io.Writer) error {
	return func(w io.Writer) error {
		if _, err := io.WriteString(w, "{"); err != nil {
			return err
		}
		if replace {
			if _, err := io.WriteString(w, `"replace":true,`); err != nil {
				return err
			}
		}
		if _, err := io.WriteString(w, `"source_stamp":{"generation":"sparse"},`); err != nil {
			return err
		}
		if err := d.structure(w); err != nil {
			return err
		}
		if _, err := io.WriteString(w, `,"value":{`); err != nil {
			return err
		}
		buffer := make([]byte, 0, 32)
		for i := range d.cells {
			buffer = buffer[:0]
			if i > 0 {
				buffer = append(buffer, ',')
			}
			buffer = append(buffer, '"')
			buffer = strconv.AppendInt(buffer, int64(i), 10)
			buffer = append(buffer, '"', ':')
			buffer = strconv.AppendInt(buffer, int64(i), 10)
			if _, err := w.Write(buffer); err != nil {
				return err
			}
		}
		_, err := io.WriteString(w, `}}`)
		return err
	}
}

// do performs one request and records it. The response body is streamed through
// a counter and handed to inspect, so a multi-megabyte response never needs to
// be buffered twice.
func (d *driver) do(name, method, url, token string, body io.ReadCloser, requestBytes *int64,
	wantStatus int, inspect func(io.Reader) error) error {
	request, err := http.NewRequest(method, url, body)
	if err != nil {
		return err
	}
	request.Header.Set("Authorization", "Bearer "+token)
	if body != nil {
		request.Header.Set("Content-Type", "application/json")
	}
	started := time.Now()
	response, err := d.client.Do(request)
	if err != nil {
		return err
	}
	defer response.Body.Close()

	var responseBytes int64
	counter := &countingReader{inner: response.Body, total: &responseBytes}
	var inspectErr error
	if response.StatusCode != wantStatus {
		payload, _ := io.ReadAll(io.LimitReader(counter, 4096))
		inspectErr = fmt.Errorf("status %d, want %d: %s", response.StatusCode, wantStatus, payload)
	} else if inspect != nil {
		inspectErr = inspect(counter)
	}
	_, _ = io.Copy(io.Discard, counter)
	duration := time.Since(started)

	entry := measurement{
		Name: name, DurationMS: duration.Milliseconds(),
		ResponseBytes: responseBytes, Status: response.StatusCode,
		RequestID: response.Header.Get("X-Request-ID"),
	}
	if requestBytes != nil {
		entry.RequestBytes = *requestBytes
	}
	if seconds := duration.Seconds(); seconds > 0 {
		entry.Throughput = float64(d.cells) / seconds
	}
	d.report.Measurements = append(d.report.Measurements, entry)
	fmt.Printf("   %s: %s status=%d request=%s response=%s\n",
		name, duration.Round(time.Millisecond), response.StatusCode,
		humanBytes(entry.RequestBytes), humanBytes(entry.ResponseBytes))
	return inspectErr
}

type countingReader struct {
	inner io.Reader
	total *int64
}

func (r *countingReader) Read(buffer []byte) (int, error) {
	n, err := r.inner.Read(buffer)
	*r.total += int64(n)
	return n, err
}

func humanBytes(value int64) string {
	switch {
	case value >= 1<<20:
		return fmt.Sprintf("%.1fMiB", float64(value)/(1<<20))
	case value >= 1<<10:
		return fmt.Sprintf("%.1fKiB", float64(value)/(1<<10))
	default:
		return fmt.Sprintf("%dB", value)
	}
}

func (d *driver) deleteDataset() error {
	return d.do("initial delete", http.MethodDelete, d.path(""), d.config.writeToken,
		nil, nil, http.StatusNoContent, nil)
}

func (d *driver) finalDelete() error {
	if err := d.do("final delete", http.MethodDelete, d.path(""), d.config.writeToken,
		nil, nil, http.StatusNoContent, nil); err != nil {
		return err
	}
	return d.do("read after delete", http.MethodGet, d.path(""), d.config.readToken,
		nil, nil, http.StatusNotFound, nil)
}

type mutationResponse struct {
	Result  string `json:"result"`
	Dataset struct {
		CellCount       int64           `json:"cell_count"`
		ValuedCellCount int64           `json:"valued_cell_count"`
		NullCellCount   int64           `json:"null_cell_count"`
		SourceStamp     json.RawMessage `json:"source_stamp"`
	} `json:"dataset"`
}

func (d *driver) checkMutation(wantResult string, wantValued int64) func(io.Reader) error {
	return func(r io.Reader) error {
		var decoded mutationResponse
		if err := json.NewDecoder(r).Decode(&decoded); err != nil {
			return fmt.Errorf("decode: %w", err)
		}
		if decoded.Result != wantResult {
			return fmt.Errorf("result = %q, want %q", decoded.Result, wantResult)
		}
		if decoded.Dataset.CellCount != int64(d.cells) {
			return fmt.Errorf("cell_count = %d, want %d", decoded.Dataset.CellCount, d.cells)
		}
		if decoded.Dataset.ValuedCellCount != wantValued {
			return fmt.Errorf("valued_cell_count = %d, want %d", decoded.Dataset.ValuedCellCount, wantValued)
		}
		if decoded.Dataset.NullCellCount != int64(d.cells)-wantValued {
			return fmt.Errorf("null_cell_count = %d, want %d",
				decoded.Dataset.NullCellCount, int64(d.cells)-wantValued)
		}
		return nil
	}
}

func (d *driver) denseReplacement() error {
	body, requestBytes := d.streamBody(d.denseBody(false))
	return d.do("fully populated dense replacement", http.MethodPost, d.path(""), d.config.writeToken,
		body, requestBytes, http.StatusCreated, d.checkMutation("created", int64(d.cells)))
}

func (d *driver) sparseReplacement() error {
	body, requestBytes := d.streamBody(d.sparseBody(true))
	if err := d.do("fully populated sparse replacement", http.MethodPost, d.path(""), d.config.writeToken,
		body, requestBytes, http.StatusOK, d.checkMutation("replaced", int64(d.cells))); err != nil {
		return err
	}
	// The sparse payload carries no status, so the previous scalar status must
	// be gone rather than merged into the new state.
	return d.do("status cleared by replacement", http.MethodGet, d.path("/data?format=dense"),
		d.config.readToken, nil, nil, http.StatusOK, func(r io.Reader) error {
			var decoded struct {
				Status json.RawMessage `json:"status"`
			}
			if err := json.NewDecoder(r).Decode(&decoded); err != nil {
				return err
			}
			if len(decoded.Status) != 0 {
				return fmt.Errorf("status survived the replacement: %s", decoded.Status)
			}
			return nil
		})
}

func (d *driver) fullDenseRead() error {
	return d.do("full dense read", http.MethodGet, d.path("/data?format=dense"), d.config.readToken,
		nil, nil, http.StatusOK, func(r io.Reader) error {
			var decoded struct {
				CellCount int64     `json:"cell_count"`
				Value     []float64 `json:"value"`
				Status    string    `json:"status"`
			}
			if err := json.NewDecoder(r).Decode(&decoded); err != nil {
				return fmt.Errorf("decode: %w", err)
			}
			if len(decoded.Value) != d.cells {
				return fmt.Errorf("value length = %d, want %d (truncated output)", len(decoded.Value), d.cells)
			}
			if decoded.Status != "a" {
				return fmt.Errorf("status = %q, want the scalar a", decoded.Status)
			}
			for _, index := range []int{0, 1, d.cells / 2, d.cells - 1} {
				if decoded.Value[index] != float64(index) {
					return fmt.Errorf("value[%d] = %v, want %d", index, decoded.Value[index], index)
				}
			}
			return nil
		})
}

func (d *driver) fullSparseRead() error {
	return d.do("full sparse read", http.MethodGet, d.path("/data"), d.config.readToken,
		nil, nil, http.StatusOK, func(r io.Reader) error {
			var decoded struct {
				Value map[string]float64 `json:"value"`
			}
			if err := json.NewDecoder(r).Decode(&decoded); err != nil {
				return fmt.Errorf("decode: %w", err)
			}
			if len(decoded.Value) != d.cells {
				return fmt.Errorf("value entries = %d, want %d", len(decoded.Value), d.cells)
			}
			last := strconv.Itoa(d.cells - 1)
			if decoded.Value[last] != float64(d.cells-1) {
				return fmt.Errorf("value[%s] = %v", last, decoded.Value[last])
			}
			return nil
		})
}

// reorderedSubset requests a c/b/a slice with dimension b reversed, which
// exercises output-local ordering across the whole stored cube.
func (d *driver) reorderedSubset() error {
	side := d.config.side
	var query bytes.Buffer
	query.WriteString(`{"id":["c","b","a"],"dimension":{"c":{"index":{"c0000":0}},"b":{"index":{`)
	for i := range side {
		if i > 0 {
			query.WriteByte(',')
		}
		fmt.Fprintf(&query, `"b%04d":%d`, side-1-i, i)
	}
	query.WriteString(`}},"a":{"index":{`)
	for i := range side {
		if i > 0 {
			query.WriteByte(',')
		}
		fmt.Fprintf(&query, `"a%04d":%d`, i, i)
	}
	query.WriteString(`}}}}`)

	requestBytes := int64(query.Len())
	return d.do("reordered exact subset", http.MethodPost, d.path("/query?format=dense"), d.config.readToken,
		io.NopCloser(bytes.NewReader(query.Bytes())), &requestBytes, http.StatusOK, func(r io.Reader) error {
			var decoded struct {
				CellCount int64     `json:"cell_count"`
				ID        []string  `json:"id"`
				Value     []float64 `json:"value"`
			}
			if err := json.NewDecoder(r).Decode(&decoded); err != nil {
				return fmt.Errorf("decode: %w", err)
			}
			if len(decoded.Value) != side*side {
				return fmt.Errorf("value length = %d, want %d", len(decoded.Value), side*side)
			}
			if decoded.CellCount != int64(d.cells) {
				return fmt.Errorf("cell_count = %d, want the whole-dataset count %d", decoded.CellCount, d.cells)
			}
			if strings.Join(decoded.ID, ",") != "c,b,a" {
				return fmt.Errorf("id = %v, want the requested order", decoded.ID)
			}
			// The stored value at (a, b, c) is a*side*side + b*side + c, and
			// the output index is bOut*side + aOut with b reversed and c fixed.
			for _, probe := range [][2]int{{0, 0}, {0, side - 1}, {side - 1, 0}, {side / 2, side / 2}} {
				bOut, aOut := probe[0], probe[1]
				want := float64(aOut*side*side + (side-1-bOut)*side)
				if got := decoded.Value[bOut*side+aOut]; got != want {
					return fmt.Errorf("output (b=%d, a=%d) = %v, want %v", bOut, aOut, got, want)
				}
			}
			return nil
		})
}

// atomicVisibility runs a replacement while reads run concurrently. Every read
// must observe one complete generation, never a mixture.
func (d *driver) atomicVisibility() error {
	stop := make(chan struct{})
	var group sync.WaitGroup
	var readErr error
	var once sync.Once
	var reads int

	group.Add(1)
	go func() {
		defer group.Done()
		for {
			select {
			case <-stop:
				return
			default:
			}
			generation, valued, cellCount, err := d.summarize()
			if err != nil {
				once.Do(func() { readErr = err })
				return
			}
			reads++
			// Either generation is acceptable, but the counts must always match
			// a complete dataset.
			if cellCount != int64(d.cells) || valued != int64(d.cells) {
				once.Do(func() {
					readErr = fmt.Errorf("a reader saw a partial dataset during a replacement: "+
						"generation=%s cell_count=%d valued=%d", generation, cellCount, valued)
				})
				return
			}
		}
	}()

	body, requestBytes := d.streamBody(d.denseBody(true))
	err := d.do("replacement under concurrent reads", http.MethodPost, d.path(""), d.config.writeToken,
		body, requestBytes, http.StatusOK, d.checkMutation("replaced", int64(d.cells)))
	close(stop)
	group.Wait()
	if err != nil {
		return err
	}
	if readErr != nil {
		return readErr
	}
	fmt.Printf("   %d concurrent reads all observed a complete dataset\n", reads)
	return nil
}

func (d *driver) summarize() (generation string, valued, cellCount int64, err error) {
	request, err := http.NewRequest(http.MethodGet, d.path(""), nil)
	if err != nil {
		return "", 0, 0, err
	}
	request.Header.Set("Authorization", "Bearer "+d.config.readToken)
	response, err := d.client.Do(request)
	if err != nil {
		return "", 0, 0, err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		payload, _ := io.ReadAll(io.LimitReader(response.Body, 2048))
		return "", 0, 0, fmt.Errorf("summary status %d: %s", response.StatusCode, payload)
	}
	var decoded struct {
		CellCount       int64 `json:"cell_count"`
		ValuedCellCount int64 `json:"valued_cell_count"`
		SourceStamp     struct {
			Generation string `json:"generation"`
		} `json:"source_stamp"`
	}
	if err := json.NewDecoder(response.Body).Decode(&decoded); err != nil {
		return "", 0, 0, err
	}
	return decoded.SourceStamp.Generation, decoded.ValuedCellCount, decoded.CellCount, nil
}

// forcedRollback aborts a replacement mid-flight by closing the connection. The
// API must cancel its database work and leave the previous state untouched.
func (d *driver) forcedRollback() error {
	before, beforeValued, beforeCount, err := d.summarize()
	if err != nil {
		return err
	}

	ctx, cancel := context.WithCancel(context.Background())
	body, _ := d.streamBody(func(w io.Writer) error {
		if _, err := io.WriteString(w, `{"replace":true,"source_stamp":{"generation":"aborted"},`); err != nil {
			return err
		}
		if err := d.structure(w); err != nil {
			return err
		}
		if _, err := io.WriteString(w, `,"value":[`); err != nil {
			return err
		}
		buffer := make([]byte, 0, 16)
		for i := range d.cells {
			// Abort once the server is committed to reading a large body.
			if i == d.cells/2 {
				cancel()
			}
			buffer = buffer[:0]
			if i > 0 {
				buffer = append(buffer, ',')
			}
			buffer = strconv.AppendInt(buffer, int64(i), 10)
			if _, err := w.Write(buffer); err != nil {
				return err
			}
		}
		_, err := io.WriteString(w, `]}`)
		return err
	})

	request, err := http.NewRequestWithContext(ctx, http.MethodPost, d.path(""), body)
	if err != nil {
		cancel()
		return err
	}
	request.Header.Set("Authorization", "Bearer "+d.config.writeToken)
	request.Header.Set("Content-Type", "application/json")
	response, err := d.client.Do(request)
	if err == nil {
		defer response.Body.Close()
		_, _ = io.Copy(io.Discard, response.Body)
		cancel()
		return fmt.Errorf("the aborted replacement returned %d instead of failing", response.StatusCode)
	}
	cancel()
	if !errors.Is(err, context.Canceled) && !isConnectionError(err) {
		return fmt.Errorf("unexpected client error: %w", err)
	}

	// Give the server a moment to notice the disconnect and roll back.
	deadline := time.Now().Add(30 * time.Second)
	for {
		after, afterValued, afterCount, err := d.summarize()
		if err != nil {
			return fmt.Errorf("read after the aborted replacement: %w", err)
		}
		if after == "aborted" {
			return errors.New("the aborted replacement became visible")
		}
		if after == before && afterValued == beforeValued && afterCount == beforeCount {
			fmt.Printf("   previous generation %q intact after the abort\n", before)
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("state changed after an aborted replacement: "+
				"was %s/%d/%d, now %s/%d/%d", before, beforeValued, beforeCount, after, afterValued, afterCount)
		}
		time.Sleep(250 * time.Millisecond)
	}
}

func isConnectionError(err error) bool {
	var netErr net.Error
	return errors.As(err, &netErr) || strings.Contains(err.Error(), "EOF") ||
		strings.Contains(err.Error(), "broken pipe") || strings.Contains(err.Error(), "connection reset")
}
