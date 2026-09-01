package httpapi

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/nordicintel/nordicintel-storage-api/internal/apidocs"
	"github.com/nordicintel/nordicintel-storage-api/internal/migrations"
	"github.com/nordicintel/nordicintel-storage-api/internal/store"
)

// liveHarness runs the real router over a real PostgreSQL 18 database, so the
// contract is exercised end to end rather than against a stub.
type liveHarness struct {
	server *httptest.Server
	client *http.Client
}

func newLiveHarness(t *testing.T) *liveHarness {
	t.Helper()
	base := os.Getenv("DATABASE_URL")
	if base == "" {
		t.Skip("DATABASE_URL is not set; skipping end-to-end tests")
	}
	url := freshMigratedDatabase(t, base)

	database, err := store.Open(t.Context(), url, 8)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(database.Close)

	handler := New(database, slog.New(slog.NewJSONHandler(io.Discard, nil)), Options{
		ReadWriteToken: writeToken, ReadOnlyToken: readToken,
		MaxRequestBytes: 64 << 20, MaxCells: 1_000_000,
		RequestTimeout: 60 * time.Second, DBTimeout: 45 * time.Second,
		OpenAPI: apidocs.Specification(), Docs: apidocs.Handler(),
	})
	server := httptest.NewServer(handler.Handler())
	t.Cleanup(server.Close)
	return &liveHarness{server: server, client: server.Client()}
}

func freshMigratedDatabase(t *testing.T, base string) string {
	t.Helper()
	name := fmt.Sprintf("api_test_%d", time.Now().UnixNano()%1_000_000_000)
	admin, err := pgx.Connect(t.Context(), base)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer admin.Close(context.Background())
	if _, err := admin.Exec(t.Context(),
		fmt.Sprintf(`create database %s`, pgx.Identifier{name}.Sanitize())); err != nil {
		t.Fatalf("create database: %v", err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		conn, err := pgx.Connect(ctx, base)
		if err != nil {
			return
		}
		defer conn.Close(context.Background())
		_, _ = conn.Exec(ctx, fmt.Sprintf(`drop database if exists %s with (force)`,
			pgx.Identifier{name}.Sanitize()))
	})

	config, err := pgx.ParseConfig(base)
	if err != nil {
		t.Fatal(err)
	}
	suffix := ""
	if index := strings.Index(base, "?"); index >= 0 {
		suffix = base[index:]
	}
	url := fmt.Sprintf("postgres://%s:%s@%s:%d/%s%s",
		config.User, config.Password, config.Host, config.Port, name, suffix)
	if err := migrations.Run(t.Context(), url); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	return url
}

type response struct {
	status int
	body   []byte
	header http.Header
}

func (r response) decode(t *testing.T) map[string]any {
	t.Helper()
	var decoded map[string]any
	if err := json.Unmarshal(r.body, &decoded); err != nil {
		t.Fatalf("body %q is not a JSON object: %v", r.body, err)
	}
	return decoded
}

func (h *liveHarness) request(t *testing.T, method, target, token, body string) response {
	t.Helper()
	var reader io.Reader
	if body != "" {
		reader = strings.NewReader(body)
	}
	request, err := http.NewRequestWithContext(t.Context(), method, h.server.URL+target, reader)
	if err != nil {
		t.Fatal(err)
	}
	if token != "" {
		request.Header.Set("Authorization", "Bearer "+token)
	}
	if body != "" {
		request.Header.Set("Content-Type", "application/json")
	}
	result, err := h.client.Do(request)
	if err != nil {
		t.Fatalf("%s %s: %v", method, target, err)
	}
	defer result.Body.Close()
	payload, err := io.ReadAll(result.Body)
	if err != nil {
		t.Fatal(err)
	}
	return response{status: result.StatusCode, body: payload, header: result.Header}
}

func (h *liveHarness) expect(t *testing.T, status int, method, target, token, body string) response {
	t.Helper()
	got := h.request(t, method, target, token, body)
	if got.status != status {
		t.Fatalf("%s %s = %d, want %d: %s", method, target, got.status, status, got.body)
	}
	return got
}

const liveDataset = `{"source_stamp":{"etag":"abc"},` +
	`"id":["sex","year"],"dimension":{"sex":{"index":{"M":0,"F":1}},"year":{"index":{"2024":0,"2025":1}}},` +
	`"value":[10.5,null,null,null],"text":[null,null,null,"confidential"],"status":[null,null,null,"c"]}`

func TestEndToEndDatasetLifecycle(t *testing.T) {
	h := newLiveHarness(t)
	const path = "/v1/providers/SCB/datasets/Population"

	t.Run("health is ready", func(t *testing.T) {
		got := h.expect(t, http.StatusOK, http.MethodGet, "/health", "", "")
		if got.decode(t)["status"] != "ok" {
			t.Fatalf("health = %s", got.body)
		}
	})

	t.Run("an unknown dataset is not found", func(t *testing.T) {
		h.expect(t, http.StatusNotFound, http.MethodGet, path, readToken, "")
		h.expect(t, http.StatusNotFound, http.MethodGet, "/v1/providers/SCB/datasets", readToken, "")
	})

	t.Run("creation returns 201 and the summary", func(t *testing.T) {
		got := h.expect(t, http.StatusCreated, http.MethodPost, path, writeToken, liveDataset)
		decoded := got.decode(t)
		if decoded["result"] != "created" {
			t.Fatalf("result = %v", decoded["result"])
		}
		summary := decoded["dataset"].(map[string]any)
		if summary["cell_count"] != float64(4) || summary["valued_cell_count"] != float64(2) ||
			summary["null_cell_count"] != float64(2) {
			t.Fatalf("summary = %v", summary)
		}
		if summary["provider_code"] != "SCB" || summary["dataset_code"] != "Population" {
			t.Fatalf("summary spellings = %v", summary)
		}
	})

	t.Run("re-creating without replace conflicts", func(t *testing.T) {
		got := h.expect(t, http.StatusConflict, http.MethodPost, path, writeToken, liveDataset)
		if got.decode(t)["error"].(map[string]any)["code"] != "dataset_exists" {
			t.Fatalf("error = %s", got.body)
		}
	})

	t.Run("providers list the new dataset", func(t *testing.T) {
		got := h.expect(t, http.StatusOK, http.MethodGet, "/v1/providers", readToken, "")
		providers := got.decode(t)["providers"].([]any)
		if len(providers) != 1 {
			t.Fatalf("providers = %s", got.body)
		}
		first := providers[0].(map[string]any)
		if first["provider_code"] != "SCB" || first["dataset_count"] != float64(1) {
			t.Fatalf("provider = %v", first)
		}
	})

	t.Run("provider datasets use the documented envelope", func(t *testing.T) {
		got := h.expect(t, http.StatusOK, http.MethodGet, "/v1/providers/scb/datasets", readToken, "")
		decoded := got.decode(t)
		if decoded["provider_code"] != "SCB" {
			t.Fatalf("provider_code = %v", decoded["provider_code"])
		}
		if len(decoded["datasets"].([]any)) != 1 {
			t.Fatalf("datasets = %s", got.body)
		}
	})

	t.Run("structure sorts by normalized code", func(t *testing.T) {
		got := h.expect(t, http.StatusOK, http.MethodGet, path+"/structure", readToken, "")
		decoded := got.decode(t)
		ids := decoded["id"].([]any)
		if len(ids) != 2 || ids[0] != "sex" || ids[1] != "year" {
			t.Fatalf("id = %v", ids)
		}
		sex := decoded["dimension"].(map[string]any)["sex"].(map[string]any)["index"].(map[string]any)
		if sex["F"] != float64(0) || sex["M"] != float64(1) {
			t.Fatalf("sex index = %v", sex)
		}
		if _, present := decoded["value"]; present {
			t.Fatal("the structure response carried observations")
		}
	})

	t.Run("sparse data omits inferred nulls", func(t *testing.T) {
		got := h.expect(t, http.StatusOK, http.MethodGet, path+"/data", readToken, "")
		decoded := got.decode(t)
		// Sorting F before M maps payload index 0 to internal 2 and payload
		// index 3 to internal 1.
		values := decoded["value"].(map[string]any)
		if len(values) != 1 || values["2"] != 10.5 {
			t.Fatalf("value = %v", values)
		}
		texts := decoded["text"].(map[string]any)
		if len(texts) != 1 || texts["1"] != "confidential" {
			t.Fatalf("text = %v", texts)
		}
		statuses := decoded["status"].(map[string]any)
		if len(statuses) != 1 || statuses["1"] != "c" {
			t.Fatalf("status = %v", statuses)
		}
	})

	t.Run("dense data returns every logical cell", func(t *testing.T) {
		got := h.expect(t, http.StatusOK, http.MethodGet, path+"/data?format=dense", readToken, "")
		decoded := got.decode(t)
		values := decoded["value"].([]any)
		if len(values) != 4 || values[2] != 10.5 || values[0] != nil || values[3] != nil {
			t.Fatalf("value = %v", values)
		}
		texts := decoded["text"].([]any)
		if len(texts) != 4 || texts[1] != "confidential" {
			t.Fatalf("text = %v", texts)
		}
	})

	t.Run("a reordered query preserves the requested order", func(t *testing.T) {
		query := `{"id":["year","sex"],"dimension":{"year":{"index":{"2025":0,"2024":1}},` +
			`"sex":{"index":{"M":0,"F":1}}}}`
		got := h.expect(t, http.StatusOK, http.MethodPost, path+"/query", readToken, query)
		decoded := got.decode(t)
		ids := decoded["id"].([]any)
		if ids[0] != "year" || ids[1] != "sex" {
			t.Fatalf("id = %v, want the requested order", ids)
		}
		// Output order is (2025,M) (2025,F) (2024,M) (2024,F) whose internal
		// indexes are 3, 1, 2, 0. Only internal 1 and 2 hold rows.
		values := decoded["value"].(map[string]any)
		if len(values) != 1 || values["2"] != 10.5 {
			t.Fatalf("value = %v", values)
		}
		texts := decoded["text"].(map[string]any)
		if len(texts) != 1 || texts["1"] != "confidential" {
			t.Fatalf("text = %v", texts)
		}
		if decoded["cell_count"] != float64(4) || decoded["valued_cell_count"] != float64(2) {
			t.Fatalf("the subset lost the whole-dataset counts: %s", got.body)
		}
	})

	t.Run("a subset keeps requested categories with no data", func(t *testing.T) {
		query := `{"id":["sex","year"],"dimension":{"sex":{"index":{"F":0}},"year":{"index":{"2024":0}}}}`
		got := h.expect(t, http.StatusOK, http.MethodPost, path+"/query", readToken, query)
		decoded := got.decode(t)
		sex := decoded["dimension"].(map[string]any)["sex"].(map[string]any)["index"].(map[string]any)
		if len(sex) != 1 {
			t.Fatalf("the requested category was dropped: %v", sex)
		}
		if len(decoded["value"].(map[string]any)) != 0 {
			t.Fatalf("value = %v, want every cell inferred null", decoded["value"])
		}
		if _, present := decoded["text"]; present {
			t.Fatal("an all-null text channel was returned")
		}
	})

	t.Run("read-only credentials cannot mutate", func(t *testing.T) {
		h.expect(t, http.StatusForbidden, http.MethodPost, path, readToken, liveDataset)
		h.expect(t, http.StatusForbidden, http.MethodDelete, path, readToken, "")
		got := h.expect(t, http.StatusOK, http.MethodGet, path, readToken, "")
		if got.decode(t)["dataset_code"] != "Population" {
			t.Fatal("the forbidden mutation changed the dataset")
		}
	})

	t.Run("replacement returns 200 and replaces everything", func(t *testing.T) {
		body := `{"replace":true,"source_stamp":null,"id":["region"],` +
			`"dimension":{"region":{"index":{"north":0,"south":1}}},"value":{"1":5},"status":"e"}`
		got := h.expect(t, http.StatusOK, http.MethodPost, path, writeToken, body)
		if got.decode(t)["result"] != "replaced" {
			t.Fatalf("result = %s", got.body)
		}
		data := h.expect(t, http.StatusOK, http.MethodGet, path+"/data", readToken, "").decode(t)
		ids := data["id"].([]any)
		if len(ids) != 1 || ids[0] != "region" {
			t.Fatalf("the previous structure survived: %v", ids)
		}
		if data["source_stamp"] != nil {
			t.Fatalf("source_stamp = %v, want the JSON null to round trip", data["source_stamp"])
		}
		if data["status"] != "e" {
			t.Fatalf("status = %v, want the scalar e", data["status"])
		}
		if _, present := data["text"]; present {
			t.Fatal("an all-null text channel was returned")
		}
	})

	t.Run("deletion is idempotent", func(t *testing.T) {
		h.expect(t, http.StatusNoContent, http.MethodDelete, path, writeToken, "")
		h.expect(t, http.StatusNoContent, http.MethodDelete, path, writeToken, "")
		h.expect(t, http.StatusNotFound, http.MethodGet, path, readToken, "")
		got := h.expect(t, http.StatusOK, http.MethodGet, "/v1/providers", readToken, "")
		if len(got.decode(t)["providers"].([]any)) != 0 {
			t.Fatalf("providers = %s, want the empty provider hidden", got.body)
		}
	})

	t.Run("recreation establishes a new dataset spelling", func(t *testing.T) {
		got := h.expect(t, http.StatusCreated, http.MethodPost,
			"/v1/providers/scb/datasets/POPULATION", writeToken, liveDataset)
		summary := got.decode(t)["dataset"].(map[string]any)
		if summary["provider_code"] != "SCB" {
			t.Fatalf("provider spelling = %v, want the retained SCB", summary["provider_code"])
		}
		if summary["dataset_code"] != "POPULATION" {
			t.Fatalf("dataset spelling = %v, want the new POPULATION", summary["dataset_code"])
		}
	})
}

func TestEndToEndErrorMappings(t *testing.T) {
	h := newLiveHarness(t)
	const path = "/v1/providers/SCB/datasets/Population"
	h.expect(t, http.StatusCreated, http.MethodPost, path, writeToken, liveDataset)

	cases := []struct {
		name   string
		method string
		target string
		token  string
		body   string
		status int
		code   string
	}{
		{"unauthenticated", http.MethodGet, path, "", "", http.StatusUnauthorized, "unauthorized"},
		{"forbidden", http.MethodDelete, path, readToken, "", http.StatusForbidden, "forbidden"},
		{"unknown dataset", http.MethodGet, "/v1/providers/SCB/datasets/Missing", readToken, "",
			http.StatusNotFound, "not_found"},
		{"unknown provider", http.MethodGet, "/v1/providers/Nope/datasets", readToken, "",
			http.StatusNotFound, "not_found"},
		{"unknown route", http.MethodGet, "/v1/nope", readToken, "", http.StatusNotFound, "not_found"},
		{"bad method", http.MethodPut, path, writeToken, "", http.StatusMethodNotAllowed, "method_not_allowed"},
		{"malformed json", http.MethodPost, path, writeToken, `{`, http.StatusBadRequest, "invalid_json"},
		{"duplicate keys", http.MethodPost, path, writeToken,
			`{"source_stamp":null,"source_stamp":1,"id":["a"],"dimension":{"a":{"index":{"x":0}}},"value":[1]}`,
			http.StatusBadRequest, "invalid_json"},
		{"conflicting channels", http.MethodPost, path, writeToken,
			`{"replace":true,"source_stamp":null,"id":["a"],"dimension":{"a":{"index":{"x":0}}},` +
				`"value":[1],"text":["x"]}`, http.StatusUnprocessableEntity, "validation_failed"},
		{"unknown query dimension", http.MethodPost, path + "/query", readToken,
			`{"id":["nope"],"dimension":{"nope":{"index":{"x":0}}}}`,
			http.StatusUnprocessableEntity, "validation_failed"},
		{"incomplete query", http.MethodPost, path + "/query", readToken,
			`{"id":["sex"],"dimension":{"sex":{"index":{"M":0}}}}`,
			http.StatusUnprocessableEntity, "validation_failed"},
		{"unknown query parameter", http.MethodGet, path + "/data?compact=1", readToken, "",
			http.StatusBadRequest, "invalid_query"},
		{"query parameter on a summary route", http.MethodGet, path + "?format=dense", readToken, "",
			http.StatusBadRequest, "invalid_query"},
		{"blank path code", http.MethodGet, "/v1/providers/%20/datasets", readToken, "",
			http.StatusBadRequest, "invalid_path_code"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := h.expect(t, tc.status, tc.method, tc.target, tc.token, tc.body)
			envelope, ok := got.decode(t)["error"].(map[string]any)
			if !ok {
				t.Fatalf("body %s is not an error envelope", got.body)
			}
			if envelope["code"] != tc.code {
				t.Fatalf("code = %v, want %q", envelope["code"], tc.code)
			}
			if envelope["request_id"] != got.header.Get("X-Request-ID") {
				t.Fatalf("request_id %v does not match the header %q",
					envelope["request_id"], got.header.Get("X-Request-ID"))
			}
			if got.header.Get("Cache-Control") != "no-store" {
				t.Fatalf("Cache-Control = %q", got.header.Get("Cache-Control"))
			}
		})
	}

	t.Run("unsupported media type", func(t *testing.T) {
		request, err := http.NewRequestWithContext(t.Context(), http.MethodPost,
			h.server.URL+path, strings.NewReader(liveDataset))
		if err != nil {
			t.Fatal(err)
		}
		request.Header.Set("Authorization", "Bearer "+writeToken)
		request.Header.Set("Content-Type", "text/plain")
		result, err := h.client.Do(request)
		if err != nil {
			t.Fatal(err)
		}
		defer result.Body.Close()
		if result.StatusCode != http.StatusUnsupportedMediaType {
			t.Fatalf("status = %d, want 415", result.StatusCode)
		}
	})
}

func TestEndToEndDocumentationIsPublicAndMatchesTheAPI(t *testing.T) {
	h := newLiveHarness(t)

	specification := h.expect(t, http.StatusOK, http.MethodGet, "/openapi.json", "", "")
	var document map[string]any
	if err := json.Unmarshal(specification.body, &document); err != nil {
		t.Fatalf("the served document is not JSON: %v", err)
	}
	if document["openapi"] != "3.1.0" {
		t.Fatalf("openapi = %v", document["openapi"])
	}

	redirect := h.client.CheckRedirect
	h.client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	defer func() { h.client.CheckRedirect = redirect }()
	got := h.expect(t, http.StatusPermanentRedirect, http.MethodGet, "/docs", "", "")
	if got.header.Get("Location") != "/docs/" {
		t.Fatalf("Location = %q", got.header.Get("Location"))
	}
	page := h.expect(t, http.StatusOK, http.MethodGet, "/docs/", "", "")
	if !strings.Contains(strings.ToLower(string(page.body)), "swagger") {
		t.Fatal("the documentation page is not Swagger UI")
	}
	h.expect(t, http.StatusOK, http.MethodGet, "/docs/swagger-ui-bundle.js", "", "")
}

func TestEndToEndMediumDatasetRoundTrip(t *testing.T) {
	h := newLiveHarness(t)
	const path = "/v1/providers/SCB/datasets/Wide"
	const size = 20_000

	categories := make([]string, size)
	values := make([]string, size)
	for i := range size {
		categories[i] = fmt.Sprintf("%q:%d", fmt.Sprintf("c%06d", i), i)
		values[i] = fmt.Sprintf("%d.5", i)
	}
	body := fmt.Sprintf(`{"source_stamp":null,"id":["c"],"dimension":{"c":{"index":{%s}}},"value":[%s]}`,
		strings.Join(categories, ","), strings.Join(values, ","))
	h.expect(t, http.StatusCreated, http.MethodPost, path, writeToken, body)

	dense := h.expect(t, http.StatusOK, http.MethodGet, path+"/data?format=dense", readToken, "").decode(t)
	values2 := dense["value"].([]any)
	if len(values2) != size {
		t.Fatalf("dense value length = %d, want %d", len(values2), size)
	}
	// Category codes sort as strings, and the fixture is zero padded, so the
	// stored order matches the payload order.
	for _, index := range []int{0, 1, size / 2, size - 1} {
		if values2[index] != float64(index)+0.5 {
			t.Fatalf("dense value[%d] = %v, want %v", index, values2[index], float64(index)+0.5)
		}
	}

	sparse := h.expect(t, http.StatusOK, http.MethodGet, path+"/data", readToken, "").decode(t)
	if len(sparse["value"].(map[string]any)) != size {
		t.Fatalf("sparse value length = %d, want %d", len(sparse["value"].(map[string]any)), size)
	}

	// Reverse the requested category order and confirm the response follows it.
	reversed := make([]string, size)
	for i := range size {
		reversed[i] = fmt.Sprintf("%q:%d", fmt.Sprintf("c%06d", size-1-i), i)
	}
	query := fmt.Sprintf(`{"id":["c"],"dimension":{"c":{"index":{%s}}}}`, strings.Join(reversed, ","))
	result := h.expect(t, http.StatusOK, http.MethodPost, path+"/query?format=dense", readToken, query).decode(t)
	out := result["value"].([]any)
	if len(out) != size {
		t.Fatalf("query value length = %d", len(out))
	}
	for _, index := range []int{0, 1, size - 1} {
		want := float64(size-1-index) + 0.5
		if out[index] != want {
			t.Fatalf("query value[%d] = %v, want %v", index, out[index], want)
		}
	}
	if result["cell_count"] != float64(size) {
		t.Fatalf("cell_count = %v", result["cell_count"])
	}
}
