package httpapi

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/nordicintel/nordicintel-storage-api/internal/apidocs"
	"github.com/nordicintel/nordicintel-storage-api/internal/domain"
	"github.com/nordicintel/nordicintel-storage-api/internal/store"
)

const (
	writeToken = "read-write-token-of-at-least-32-bytes"
	readToken  = "read-only-token-of-at-least-32-bytes!"
)

// stubStore records the calls the handlers make and returns canned results, so
// the HTTP layer can be tested independently of PostgreSQL.
type stubStore struct {
	pingErr error

	providers    []domain.ProviderListItem
	providersErr error

	providerSpelling string
	datasets         []domain.Summary
	datasetsErr      error

	summary    domain.Summary
	summaryErr error

	view    domain.View
	viewErr error

	queryErr error

	result      string
	replaceErr  error
	replacement *domain.Replacement

	deleteErr    error
	deleteCalled bool

	selection *domain.Selection
	maxCells  int64
}

func (s *stubStore) Ping(context.Context) error { return s.pingErr }

func (s *stubStore) ListProviders(context.Context) ([]domain.ProviderListItem, error) {
	return s.providers, s.providersErr
}

func (s *stubStore) ListDatasets(context.Context, domain.Code) (string, []domain.Summary, error) {
	return s.providerSpelling, s.datasets, s.datasetsErr
}

func (s *stubStore) GetSummary(context.Context, domain.Code, domain.Code) (domain.Summary, error) {
	return s.summary, s.summaryErr
}

func (s *stubStore) GetStructure(context.Context, domain.Code, domain.Code) (domain.View, error) {
	return s.view, s.viewErr
}

func (s *stubStore) GetData(context.Context, domain.Code, domain.Code) (domain.View, error) {
	return s.view, s.viewErr
}

func (s *stubStore) Query(_ context.Context, _, _ domain.Code, selection domain.Selection, maxCells int64) (domain.View, error) {
	s.selection = &selection
	s.maxCells = maxCells
	return s.view, s.queryErr
}

func (s *stubStore) Replace(_ context.Context, replacement domain.Replacement) (string, domain.Summary, error) {
	s.replacement = &replacement
	return s.result, s.summary, s.replaceErr
}

func (s *stubStore) Delete(context.Context, domain.Code, domain.Code) error {
	s.deleteCalled = true
	return s.deleteErr
}

func (s *stubStore) Close() {}

func exampleSummary() domain.Summary {
	return domain.Summary{
		ProviderCode: "SCB", DatasetCode: "Population",
		SourceStamp:     json.RawMessage(`{"etag":"abc"}`),
		CellCount:       4,
		ValuedCellCount: 3,
		NullCellCount:   1,
		UpdatedAt:       time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC),
	}
}

func exampleView() domain.View {
	return domain.View{
		Summary: exampleSummary(),
		Dimensions: []domain.Dimension{
			{Code: domain.Code{Spelling: "sex", Key: "sex"}, Position: 0, Categories: []domain.Category{
				{Code: domain.Code{Spelling: "F", Key: "f"}, Position: 0},
				{Code: domain.Code{Spelling: "M", Key: "m"}, Position: 1},
			}},
			{Code: domain.Code{Spelling: "year", Key: "year"}, Position: 1, Categories: []domain.Category{
				{Code: domain.Code{Spelling: "2024", Key: "2024"}, Position: 0},
				{Code: domain.Code{Spelling: "2025", Key: "2025"}, Position: 1},
			}},
		},
	}
}

type harness struct {
	server *Server
	store  *stubStore
	logs   *bytes.Buffer
}

func newHarness(t *testing.T, configure func(*stubStore)) *harness {
	t.Helper()
	database := &stubStore{
		providerSpelling: "SCB",
		summary:          exampleSummary(),
		view:             exampleView(),
		result:           "created",
	}
	if configure != nil {
		configure(database)
	}
	logs := &bytes.Buffer{}
	logger := slog.New(slog.NewJSONHandler(logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
	server := New(database, logger, Options{
		ReadWriteToken: writeToken, ReadOnlyToken: readToken,
		MaxRequestBytes: 4096, MaxCells: 1_000_000,
		RequestTimeout: 5 * time.Second, DBTimeout: 4 * time.Second,
		OpenAPI: apidocs.Specification(), Docs: apidocs.Handler(),
	})
	return &harness{server: server, store: database, logs: logs}
}

type call struct {
	method      string
	target      string
	token       string
	contentType string
	body        string
	headers     map[string]string
}

func (h *harness) do(t *testing.T, c call) *httptest.ResponseRecorder {
	t.Helper()
	var body io.Reader
	if c.body != "" {
		body = strings.NewReader(c.body)
	}
	request := httptest.NewRequest(c.method, c.target, body)
	if c.token != "" {
		request.Header.Set("Authorization", "Bearer "+c.token)
	}
	if c.contentType != "" {
		request.Header.Set("Content-Type", c.contentType)
	}
	for name, value := range c.headers {
		request.Header.Set(name, value)
	}
	recorder := httptest.NewRecorder()
	h.server.Handler().ServeHTTP(recorder, request)
	return recorder
}

func decodeBody(t *testing.T, recorder *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	var decoded map[string]any
	if err := json.Unmarshal(recorder.Body.Bytes(), &decoded); err != nil {
		t.Fatalf("response body %q is not a JSON object: %v", recorder.Body.String(), err)
	}
	return decoded
}

func assertError(t *testing.T, recorder *httptest.ResponseRecorder, status int, code string) map[string]any {
	t.Helper()
	if recorder.Code != status {
		t.Fatalf("status = %d, want %d (body %s)", recorder.Code, status, recorder.Body.String())
	}
	decoded := decodeBody(t, recorder)
	envelope, ok := decoded["error"].(map[string]any)
	if !ok {
		t.Fatalf("response %s does not use the error envelope", recorder.Body.String())
	}
	if envelope["code"] != code {
		t.Fatalf("error code = %v, want %q", envelope["code"], code)
	}
	if message, ok := envelope["message"].(string); !ok || message == "" {
		t.Fatalf("error message = %v, want a non-empty string", envelope["message"])
	}
	requestID, ok := envelope["request_id"].(string)
	if !ok || requestID == "" {
		t.Fatalf("error request_id = %v, want a non-empty string", envelope["request_id"])
	}
	if header := recorder.Header().Get("X-Request-ID"); header != requestID {
		t.Fatalf("X-Request-ID = %q but the envelope reported %q", header, requestID)
	}
	return envelope
}

const validBody = `{"source_stamp":null,"id":["sex","year"],` +
	`"dimension":{"sex":{"index":{"M":0,"F":1}},"year":{"index":{"2024":0,"2025":1}}},` +
	`"value":[1,2,3,4],"status":{"0":"a"}}`

const validQuery = `{"id":["year","sex"],"dimension":{"year":{"index":{"2025":0}},"sex":{"index":{"F":0,"M":1}}}}`

// ---------------------------------------------------------------- routing ---

func TestEveryDocumentedRouteIsReachable(t *testing.T) {
	h := newHarness(t, nil)
	cases := []call{
		{method: http.MethodGet, target: "/health"},
		{method: http.MethodGet, target: "/openapi.json"},
		{method: http.MethodGet, target: "/docs/"},
		{method: http.MethodGet, target: "/v1/providers", token: readToken},
		{method: http.MethodGet, target: "/v1/providers/SCB/datasets", token: readToken},
		{method: http.MethodGet, target: "/v1/providers/SCB/datasets/Population", token: readToken},
		{method: http.MethodGet, target: "/v1/providers/SCB/datasets/Population/structure", token: readToken},
		{method: http.MethodGet, target: "/v1/providers/SCB/datasets/Population/data", token: readToken},
		{method: http.MethodPost, target: "/v1/providers/SCB/datasets/Population/query", token: readToken,
			contentType: "application/json", body: validQuery},
		{method: http.MethodPost, target: "/v1/providers/SCB/datasets/Population", token: writeToken,
			contentType: "application/json", body: validBody},
		{method: http.MethodDelete, target: "/v1/providers/SCB/datasets/Population", token: writeToken},
	}
	for _, c := range cases {
		recorder := h.do(t, c)
		if recorder.Code >= 400 {
			t.Fatalf("%s %s returned %d: %s", c.method, c.target, recorder.Code, recorder.Body.String())
		}
	}
}

func TestUnknownRoutesReturnNotFound(t *testing.T) {
	h := newHarness(t, nil)
	for _, target := range []string{"/", "/v1", "/v1/providers/", "/v2/providers",
		"/v1/providers/SCB/datasets/Population/unknown", "/docsx"} {
		recorder := h.do(t, call{method: http.MethodGet, target: target, token: writeToken})
		assertError(t, recorder, http.StatusNotFound, "not_found")
	}
}

func TestUnsupportedMethodsReturnAllow(t *testing.T) {
	h := newHarness(t, nil)
	cases := []struct {
		method string
		target string
		allow  string
	}{
		{http.MethodPost, "/health", "GET"},
		{http.MethodPost, "/openapi.json", "GET"},
		{http.MethodPost, "/docs", "GET"},
		{http.MethodPost, "/docs/", "GET"},
		{http.MethodDelete, "/v1/providers", "GET"},
		{http.MethodPost, "/v1/providers/SCB/datasets", "GET"},
		{http.MethodPut, "/v1/providers/SCB/datasets/Population", "GET, POST, DELETE"},
		{http.MethodPost, "/v1/providers/SCB/datasets/Population/structure", "GET"},
		{http.MethodDelete, "/v1/providers/SCB/datasets/Population/data", "GET"},
		{http.MethodGet, "/v1/providers/SCB/datasets/Population/query", "POST"},
	}
	for _, tc := range cases {
		recorder := h.do(t, call{method: tc.method, target: tc.target, token: writeToken})
		assertError(t, recorder, http.StatusMethodNotAllowed, "method_not_allowed")
		if allow := recorder.Header().Get("Allow"); allow != tc.allow {
			t.Fatalf("%s %s: Allow = %q, want %q", tc.method, tc.target, allow, tc.allow)
		}
	}
}

func TestDocsRedirectsToTheTrailingSlash(t *testing.T) {
	h := newHarness(t, nil)
	recorder := h.do(t, call{method: http.MethodGet, target: "/docs"})
	if recorder.Code != http.StatusPermanentRedirect {
		t.Fatalf("status = %d, want %d", recorder.Code, http.StatusPermanentRedirect)
	}
	if location := recorder.Header().Get("Location"); location != "/docs/" {
		t.Fatalf("Location = %q, want /docs/", location)
	}
}

// ------------------------------------------------------- authentication ---

func TestPublicRoutesRequireNoCredentials(t *testing.T) {
	h := newHarness(t, nil)
	for _, target := range []string{"/health", "/openapi.json", "/docs", "/docs/"} {
		recorder := h.do(t, call{method: http.MethodGet, target: target})
		if recorder.Code == http.StatusUnauthorized {
			t.Fatalf("%s demanded credentials", target)
		}
	}
}

func TestPrivateRoutesRejectMissingOrInvalidCredentials(t *testing.T) {
	h := newHarness(t, nil)
	targets := []call{
		{method: http.MethodGet, target: "/v1/providers"},
		{method: http.MethodGet, target: "/v1/providers/SCB/datasets"},
		{method: http.MethodGet, target: "/v1/providers/SCB/datasets/Population"},
		{method: http.MethodGet, target: "/v1/providers/SCB/datasets/Population/structure"},
		{method: http.MethodGet, target: "/v1/providers/SCB/datasets/Population/data"},
		{method: http.MethodPost, target: "/v1/providers/SCB/datasets/Population/query",
			contentType: "application/json", body: validQuery},
		{method: http.MethodPost, target: "/v1/providers/SCB/datasets/Population",
			contentType: "application/json", body: validBody},
		{method: http.MethodDelete, target: "/v1/providers/SCB/datasets/Population"},
	}
	credentials := []struct {
		name   string
		header map[string]string
	}{
		{"no header", nil},
		{"empty bearer", map[string]string{"Authorization": "Bearer "}},
		{"wrong token", map[string]string{"Authorization": "Bearer nope-nope-nope-nope-nope-nope-nope"}},
		{"basic scheme", map[string]string{"Authorization": "Basic " + writeToken}},
		{"missing scheme", map[string]string{"Authorization": writeToken}},
		{"lowercase scheme", map[string]string{"Authorization": "bearer " + writeToken}},
		{"token prefix only", map[string]string{"Authorization": "Bearer " + writeToken[:10]}},
	}
	for _, target := range targets {
		for _, credential := range credentials {
			c := target
			c.headers = credential.header
			recorder := h.do(t, c)
			assertError(t, recorder, http.StatusUnauthorized, "unauthorized")
			if challenge := recorder.Header().Get("WWW-Authenticate"); challenge != "Bearer" {
				t.Fatalf("%s %s (%s): WWW-Authenticate = %q", c.method, c.target, credential.name, challenge)
			}
		}
	}
}

func TestReadOnlyCredentialsCannotMutate(t *testing.T) {
	h := newHarness(t, nil)
	mutations := []call{
		{method: http.MethodPost, target: "/v1/providers/SCB/datasets/Population", token: readToken,
			contentType: "application/json", body: validBody},
		{method: http.MethodDelete, target: "/v1/providers/SCB/datasets/Population", token: readToken},
	}
	for _, c := range mutations {
		recorder := h.do(t, c)
		assertError(t, recorder, http.StatusForbidden, "forbidden")
	}
	if h.store.replacement != nil {
		t.Fatal("a forbidden replacement still reached the store")
	}
	if h.store.deleteCalled {
		t.Fatal("a forbidden delete still reached the store")
	}
}

func TestReadOnlyCredentialsMayRead(t *testing.T) {
	h := newHarness(t, nil)
	reads := []call{
		{method: http.MethodGet, target: "/v1/providers", token: readToken},
		{method: http.MethodGet, target: "/v1/providers/SCB/datasets", token: readToken},
		{method: http.MethodGet, target: "/v1/providers/SCB/datasets/Population", token: readToken},
		{method: http.MethodGet, target: "/v1/providers/SCB/datasets/Population/structure", token: readToken},
		{method: http.MethodGet, target: "/v1/providers/SCB/datasets/Population/data", token: readToken},
		{method: http.MethodPost, target: "/v1/providers/SCB/datasets/Population/query", token: readToken,
			contentType: "application/json", body: validQuery},
	}
	for _, c := range reads {
		if recorder := h.do(t, c); recorder.Code != http.StatusOK {
			t.Fatalf("%s %s returned %d for the read-only token", c.method, c.target, recorder.Code)
		}
	}
}

// --------------------------------------------------------- request rules ---

func TestJSONRoutesRequireTheJSONMediaType(t *testing.T) {
	h := newHarness(t, nil)
	accepted := []string{"application/json", "application/json; charset=utf-8",
		"application/json;charset=UTF-8", "APPLICATION/JSON"}
	for _, contentType := range accepted {
		recorder := h.do(t, call{method: http.MethodPost, target: "/v1/providers/SCB/datasets/Population",
			token: writeToken, contentType: contentType, body: validBody})
		if recorder.Code >= 400 {
			t.Fatalf("Content-Type %q was rejected with %d: %s", contentType, recorder.Code, recorder.Body.String())
		}
	}
	rejected := []string{"", "text/plain", "application/xml", "application/json; charset=iso-8859-1",
		"application/json; boundary=x", "multipart/form-data"}
	for _, contentType := range rejected {
		recorder := h.do(t, call{method: http.MethodPost, target: "/v1/providers/SCB/datasets/Population",
			token: writeToken, contentType: contentType, body: validBody})
		assertError(t, recorder, http.StatusUnsupportedMediaType, "unsupported_media_type")
	}
}

func TestUnsupportedContentEncodingIsRejected(t *testing.T) {
	h := newHarness(t, nil)
	recorder := h.do(t, call{method: http.MethodPost, target: "/v1/providers/SCB/datasets/Population",
		token: writeToken, contentType: "application/json", body: validBody,
		headers: map[string]string{"Content-Encoding": "gzip"}})
	assertError(t, recorder, http.StatusUnsupportedMediaType, "unsupported_media_type")
}

func TestOversizedBodiesAreRejectedBeforeStoreWork(t *testing.T) {
	h := newHarness(t, nil)
	padding := strings.Repeat("x", 8192)
	body := `{"source_stamp":"` + padding + `","id":["a"],"dimension":{"a":{"index":{"x":0}}},"value":[1]}`
	recorder := h.do(t, call{method: http.MethodPost, target: "/v1/providers/SCB/datasets/Population",
		token: writeToken, contentType: "application/json", body: body})
	assertError(t, recorder, http.StatusRequestEntityTooLarge, "request_too_large")
	if h.store.replacement != nil {
		t.Fatal("an oversized body still reached the store")
	}
}

func TestInvalidPathCodesAreRejected(t *testing.T) {
	h := newHarness(t, nil)
	for _, target := range []string{
		"/v1/providers/%20/datasets",
		"/v1/providers/%20/datasets/Population",
		"/v1/providers/SCB/datasets/%20",
		"/v1/providers/SCB/datasets/%20/structure",
		"/v1/providers/SCB/datasets/%20/data",
	} {
		recorder := h.do(t, call{method: http.MethodGet, target: target, token: writeToken})
		assertError(t, recorder, http.StatusBadRequest, "invalid_path_code")
	}
}

func TestQueryParametersAreRestrictedToFormat(t *testing.T) {
	h := newHarness(t, nil)
	t.Run("data accepts one format", func(t *testing.T) {
		for _, target := range []string{
			"/v1/providers/SCB/datasets/Population/data",
			"/v1/providers/SCB/datasets/Population/data?format=dense",
			"/v1/providers/SCB/datasets/Population/data?format=sparse",
		} {
			if recorder := h.do(t, call{method: http.MethodGet, target: target, token: readToken}); recorder.Code != http.StatusOK {
				t.Fatalf("%s returned %d", target, recorder.Code)
			}
		}
	})
	t.Run("data rejects anything else", func(t *testing.T) {
		for _, target := range []string{
			"/v1/providers/SCB/datasets/Population/data?format=dense&format=sparse",
			"/v1/providers/SCB/datasets/Population/data?format=DENSE",
			"/v1/providers/SCB/datasets/Population/data?format=",
			"/v1/providers/SCB/datasets/Population/data?format=dense&extra=1",
			"/v1/providers/SCB/datasets/Population/data?unknown=1",
		} {
			recorder := h.do(t, call{method: http.MethodGet, target: target, token: readToken})
			assertError(t, recorder, http.StatusBadRequest, "invalid_query")
		}
	})
	t.Run("other routes accept no parameters", func(t *testing.T) {
		for _, target := range []string{
			"/v1/providers?format=dense",
			"/v1/providers/SCB/datasets?format=dense",
			"/v1/providers/SCB/datasets/Population?format=dense",
			"/v1/providers/SCB/datasets/Population/structure?format=dense",
		} {
			recorder := h.do(t, call{method: http.MethodGet, target: target, token: readToken})
			assertError(t, recorder, http.StatusBadRequest, "invalid_query")
		}
		recorder := h.do(t, call{method: http.MethodDelete,
			target: "/v1/providers/SCB/datasets/Population?format=dense", token: writeToken})
		assertError(t, recorder, http.StatusBadRequest, "invalid_query")
	})
	t.Run("query accepts format", func(t *testing.T) {
		for _, target := range []string{
			"/v1/providers/SCB/datasets/Population/query?format=dense",
			"/v1/providers/SCB/datasets/Population/query",
		} {
			recorder := h.do(t, call{method: http.MethodPost, target: target, token: readToken,
				contentType: "application/json", body: validQuery})
			if recorder.Code != http.StatusOK {
				t.Fatalf("%s returned %d: %s", target, recorder.Code, recorder.Body.String())
			}
		}
	})
}

func TestMalformedBodiesReturnInvalidJSON(t *testing.T) {
	h := newHarness(t, nil)
	for _, body := range []string{`{`, `{"source_stamp":null,"source_stamp":1}`, `{} 7`, `[]`} {
		recorder := h.do(t, call{method: http.MethodPost, target: "/v1/providers/SCB/datasets/Population",
			token: writeToken, contentType: "application/json", body: body})
		assertError(t, recorder, http.StatusBadRequest, "invalid_json")
	}
	for _, body := range []string{`{`, `{"id":["a"],"id":["b"]}`, `{} 7`} {
		recorder := h.do(t, call{method: http.MethodPost, target: "/v1/providers/SCB/datasets/Population/query",
			token: readToken, contentType: "application/json", body: body})
		assertError(t, recorder, http.StatusBadRequest, "invalid_json")
	}
}

func TestSemanticViolationsReturnValidationFailed(t *testing.T) {
	h := newHarness(t, nil)
	recorder := h.do(t, call{method: http.MethodPost, target: "/v1/providers/SCB/datasets/Population",
		token: writeToken, contentType: "application/json",
		body: `{"source_stamp":null,"id":["a"],"dimension":{"a":{"index":{"x":0}}},"value":[1,2]}`})
	assertError(t, recorder, http.StatusUnprocessableEntity, "validation_failed")

	recorder = h.do(t, call{method: http.MethodPost, target: "/v1/providers/SCB/datasets/Population/query",
		token: readToken, contentType: "application/json",
		body: `{"id":["a"],"dimension":{"a":{"index":{"x":1}}}}`})
	assertError(t, recorder, http.StatusUnprocessableEntity, "validation_failed")
}

func TestCellLimitsReturnTheDedicatedCode(t *testing.T) {
	h := newHarness(t, nil)
	body := `{"source_stamp":null,"id":["a","b"],` +
		`"dimension":{"a":{"index":{"x":0,"y":1}},"b":{"index":{"p":0,"q":1}}},"value":{}}`
	server := New(h.store, slog.New(slog.NewJSONHandler(io.Discard, nil)), Options{
		ReadWriteToken: writeToken, ReadOnlyToken: readToken,
		MaxRequestBytes: 4096, MaxCells: 3,
		RequestTimeout: 5 * time.Second, DBTimeout: 4 * time.Second,
	})
	request := httptest.NewRequest(http.MethodPost, "/v1/providers/SCB/datasets/Population", strings.NewReader(body))
	request.Header.Set("Authorization", "Bearer "+writeToken)
	request.Header.Set("Content-Type", "application/json")
	recorder := httptest.NewRecorder()
	server.Handler().ServeHTTP(recorder, request)
	assertError(t, recorder, http.StatusUnprocessableEntity, "cell_limit_exceeded")
}

// -------------------------------------------------------------- responses ---

func TestProviderListEnvelope(t *testing.T) {
	h := newHarness(t, func(s *stubStore) {
		s.providers = []domain.ProviderListItem{{ProviderCode: "SCB", DatasetCount: 2}}
	})
	recorder := h.do(t, call{method: http.MethodGet, target: "/v1/providers", token: readToken})
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d", recorder.Code)
	}
	decoded := decodeBody(t, recorder)
	providers, ok := decoded["providers"].([]any)
	if !ok || len(providers) != 1 {
		t.Fatalf("body = %s", recorder.Body.String())
	}
	first := providers[0].(map[string]any)
	if first["provider_code"] != "SCB" || first["dataset_count"] != float64(2) {
		t.Fatalf("provider entry = %v", first)
	}
}

func TestEmptyProviderListIsAnEmptyArray(t *testing.T) {
	h := newHarness(t, nil)
	recorder := h.do(t, call{method: http.MethodGet, target: "/v1/providers", token: readToken})
	if !strings.Contains(recorder.Body.String(), `"providers":[]`) {
		t.Fatalf("body = %s, want an empty array rather than null", recorder.Body.String())
	}
}

func TestProviderDatasetsEnvelope(t *testing.T) {
	h := newHarness(t, func(s *stubStore) { s.datasets = []domain.Summary{exampleSummary()} })
	recorder := h.do(t, call{method: http.MethodGet, target: "/v1/providers/SCB/datasets", token: readToken})
	decoded := decodeBody(t, recorder)
	if decoded["provider_code"] != "SCB" {
		t.Fatalf("provider_code = %v", decoded["provider_code"])
	}
	datasets, ok := decoded["datasets"].([]any)
	if !ok || len(datasets) != 1 {
		t.Fatalf("body = %s", recorder.Body.String())
	}
	summary := datasets[0].(map[string]any)
	for _, field := range []string{"provider_code", "dataset_code", "source_stamp",
		"cell_count", "valued_cell_count", "null_cell_count", "updated_at"} {
		if _, present := summary[field]; !present {
			t.Fatalf("dataset summary is missing %q", field)
		}
	}
}

func TestSingleSummaryIsReturnedDirectly(t *testing.T) {
	h := newHarness(t, nil)
	recorder := h.do(t, call{method: http.MethodGet, target: "/v1/providers/SCB/datasets/Population", token: readToken})
	decoded := decodeBody(t, recorder)
	if decoded["dataset_code"] != "Population" {
		t.Fatalf("body = %s, want the summary object itself", recorder.Body.String())
	}
	if _, wrapped := decoded["dataset"]; wrapped {
		t.Fatal("the single-summary route must not wrap the summary")
	}
}

func TestCreationAndReplacementEnvelopes(t *testing.T) {
	t.Run("created", func(t *testing.T) {
		h := newHarness(t, func(s *stubStore) { s.result = "created" })
		recorder := h.do(t, call{method: http.MethodPost, target: "/v1/providers/SCB/datasets/Population",
			token: writeToken, contentType: "application/json", body: validBody})
		if recorder.Code != http.StatusCreated {
			t.Fatalf("status = %d, want 201", recorder.Code)
		}
		decoded := decodeBody(t, recorder)
		if decoded["result"] != "created" {
			t.Fatalf("result = %v", decoded["result"])
		}
		if _, ok := decoded["dataset"].(map[string]any); !ok {
			t.Fatalf("body = %s, want the summary under dataset", recorder.Body.String())
		}
	})
	t.Run("replaced", func(t *testing.T) {
		h := newHarness(t, func(s *stubStore) { s.result = "replaced" })
		recorder := h.do(t, call{method: http.MethodPost, target: "/v1/providers/SCB/datasets/Population",
			token: writeToken, contentType: "application/json", body: validBody})
		if recorder.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200", recorder.Code)
		}
		if decodeBody(t, recorder)["result"] != "replaced" {
			t.Fatalf("body = %s", recorder.Body.String())
		}
	})
}

func TestDeleteIsIdempotentAndReturnsNoContent(t *testing.T) {
	h := newHarness(t, nil)
	for range 2 {
		recorder := h.do(t, call{method: http.MethodDelete,
			target: "/v1/providers/SCB/datasets/Population", token: writeToken})
		if recorder.Code != http.StatusNoContent {
			t.Fatalf("status = %d, want 204", recorder.Code)
		}
		if recorder.Body.Len() != 0 {
			t.Fatalf("204 response carried a body: %s", recorder.Body.String())
		}
	}
}

func TestDataResponsesHonourTheRequestedFormat(t *testing.T) {
	h := newHarness(t, func(s *stubStore) {
		value := 10.5
		view := exampleView()
		view.Cells = []domain.Cell{{Index: 0, Numeric: &value}}
		s.view = view
	})
	sparse := decodeBody(t, h.do(t, call{method: http.MethodGet,
		target: "/v1/providers/SCB/datasets/Population/data", token: readToken}))
	if _, ok := sparse["value"].(map[string]any); !ok {
		t.Fatalf("the default format must be sparse, got %v", sparse["value"])
	}
	dense := decodeBody(t, h.do(t, call{method: http.MethodGet,
		target: "/v1/providers/SCB/datasets/Population/data?format=dense", token: readToken}))
	values, ok := dense["value"].([]any)
	if !ok || len(values) != 4 {
		t.Fatalf("dense value = %v", dense["value"])
	}
}

func TestQueryPassesTheParsedSelectionAndCellLimitToTheStore(t *testing.T) {
	h := newHarness(t, nil)
	h.do(t, call{method: http.MethodPost, target: "/v1/providers/SCB/datasets/Population/query",
		token: readToken, contentType: "application/json", body: validQuery})
	if h.store.selection == nil {
		t.Fatal("the store never received a selection")
	}
	if len(h.store.selection.Dimensions) != 2 || h.store.selection.Dimensions[0].Code.Key != "year" {
		t.Fatalf("selection = %+v, want the requested order", h.store.selection.Dimensions)
	}
	if h.store.maxCells != 1_000_000 {
		t.Fatalf("maxCells = %d, want the configured limit", h.store.maxCells)
	}
}

// ---------------------------------------------------------- store errors ---

func TestStoreErrorsMapToTheDocumentedStatuses(t *testing.T) {
	cases := []struct {
		name   string
		err    error
		status int
		code   string
	}{
		{"not found", store.ErrNotFound, http.StatusNotFound, "not_found"},
		{"conflict", store.ErrDatasetExists, http.StatusConflict, "dataset_exists"},
		{"deadline", context.DeadlineExceeded, http.StatusServiceUnavailable, "service_unavailable"},
		{"cancelled", context.Canceled, http.StatusServiceUnavailable, "service_unavailable"},
		{"unexpected", errors.New("connection reset"), http.StatusInternalServerError, "internal_error"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t, func(s *stubStore) { s.replaceErr = tc.err })
			recorder := h.do(t, call{method: http.MethodPost, target: "/v1/providers/SCB/datasets/Population",
				token: writeToken, contentType: "application/json", body: validBody})
			assertError(t, recorder, tc.status, tc.code)
		})
	}
}

func TestUnknownProviderOrDatasetReturnsNotFound(t *testing.T) {
	h := newHarness(t, func(s *stubStore) {
		s.datasetsErr = store.ErrNotFound
		s.summaryErr = store.ErrNotFound
		s.viewErr = store.ErrNotFound
		s.queryErr = store.ErrNotFound
	})
	targets := []call{
		{method: http.MethodGet, target: "/v1/providers/Unknown/datasets", token: readToken},
		{method: http.MethodGet, target: "/v1/providers/SCB/datasets/Missing", token: readToken},
		{method: http.MethodGet, target: "/v1/providers/SCB/datasets/Missing/structure", token: readToken},
		{method: http.MethodGet, target: "/v1/providers/SCB/datasets/Missing/data", token: readToken},
		{method: http.MethodPost, target: "/v1/providers/SCB/datasets/Missing/query", token: readToken,
			contentType: "application/json", body: validQuery},
	}
	for _, c := range targets {
		assertError(t, h.do(t, c), http.StatusNotFound, "not_found")
	}
}

func TestInvalidSelectionsFromTheStoreReturnValidationFailed(t *testing.T) {
	h := newHarness(t, func(s *stubStore) {
		s.queryErr = errors.New("query contains an unknown dimension: " + store.ErrInvalidSelection.Error())
	})
	h.store.queryErr = store.ErrInvalidSelection
	recorder := h.do(t, call{method: http.MethodPost, target: "/v1/providers/SCB/datasets/Population/query",
		token: readToken, contentType: "application/json", body: validQuery})
	assertError(t, recorder, http.StatusUnprocessableEntity, "validation_failed")
}

func TestQueryResultsOverTheCellLimitReturnTheDedicatedCode(t *testing.T) {
	h := newHarness(t, func(s *stubStore) { s.queryErr = domain.CellLimitError{Limit: 10} })
	recorder := h.do(t, call{method: http.MethodPost, target: "/v1/providers/SCB/datasets/Population/query",
		token: readToken, contentType: "application/json", body: validQuery})
	assertError(t, recorder, http.StatusUnprocessableEntity, "cell_limit_exceeded")
}

// ------------------------------------------------------------- health ---

func TestHealthReflectsDatabaseReadinessWithoutDetail(t *testing.T) {
	h := newHarness(t, nil)
	recorder := h.do(t, call{method: http.MethodGet, target: "/health"})
	if recorder.Code != http.StatusOK || decodeBody(t, recorder)["status"] != "ok" {
		t.Fatalf("healthy response = %d %s", recorder.Code, recorder.Body.String())
	}

	unhealthy := newHarness(t, func(s *stubStore) {
		s.pingErr = errors.New("dial tcp 10.0.0.1:5432: connection refused")
	})
	recorder = unhealthy.do(t, call{method: http.MethodGet, target: "/health"})
	if recorder.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", recorder.Code)
	}
	decoded := decodeBody(t, recorder)
	if decoded["status"] != "unavailable" || len(decoded) != 1 {
		t.Fatalf("body = %s, want only the status field", recorder.Body.String())
	}
	if strings.Contains(recorder.Body.String(), "10.0.0.1") {
		t.Fatal("health leaked database connection detail")
	}
}

// ------------------------------------------------------- response headers ---

func TestEveryResponseCarriesRequestIDAndNoStore(t *testing.T) {
	h := newHarness(t, nil)
	seen := make(map[string]struct{})
	calls := []call{
		{method: http.MethodGet, target: "/health"},
		{method: http.MethodGet, target: "/openapi.json"},
		{method: http.MethodGet, target: "/docs/"},
		{method: http.MethodGet, target: "/v1/providers", token: readToken},
		{method: http.MethodGet, target: "/v1/providers", token: "bad"},
		{method: http.MethodDelete, target: "/v1/providers/SCB/datasets/Population", token: writeToken},
		{method: http.MethodGet, target: "/nowhere"},
	}
	for _, c := range calls {
		recorder := h.do(t, c)
		requestID := recorder.Header().Get("X-Request-ID")
		if len(requestID) != 32 {
			t.Fatalf("%s: X-Request-ID = %q, want 32 hexadecimal characters", c.target, requestID)
		}
		if _, duplicate := seen[requestID]; duplicate {
			t.Fatalf("request ID %q was reused", requestID)
		}
		seen[requestID] = struct{}{}
		if cache := recorder.Header().Get("Cache-Control"); cache != "no-store" {
			t.Fatalf("%s: Cache-Control = %q", c.target, cache)
		}
	}
}

func TestClientSuppliedRequestIDsAreIgnored(t *testing.T) {
	h := newHarness(t, nil)
	recorder := h.do(t, call{method: http.MethodGet, target: "/health",
		headers: map[string]string{"X-Request-ID": "client-controlled"}})
	if recorder.Header().Get("X-Request-ID") == "client-controlled" {
		t.Fatal("the server echoed a client-supplied request ID")
	}
}

func TestJSONResponsesDeclareUTF8(t *testing.T) {
	h := newHarness(t, nil)
	for _, c := range []call{
		{method: http.MethodGet, target: "/health"},
		{method: http.MethodGet, target: "/v1/providers", token: readToken},
		{method: http.MethodGet, target: "/nowhere"},
	} {
		recorder := h.do(t, c)
		if got := recorder.Header().Get("Content-Type"); got != "application/json; charset=utf-8" {
			t.Fatalf("%s: Content-Type = %q", c.target, got)
		}
	}
}

// -------------------------------------------------------------- logging ---

func TestLogsRecordRouteTemplatesAndNeverIdentifiersOrContent(t *testing.T) {
	h := newHarness(t, nil)
	h.do(t, call{method: http.MethodPost, target: "/v1/providers/SecretProvider/datasets/SecretDataset",
		token: writeToken, contentType: "application/json",
		body: `{"source_stamp":{"etag":"secret-stamp"},"id":["sex"],` +
			`"dimension":{"sex":{"index":{"SecretCategory":0}}},"value":["not-a-number"]}`})
	logs := h.logs.String()
	for _, forbidden := range []string{"SecretProvider", "SecretDataset", "SecretCategory",
		"secret-stamp", "not-a-number", writeToken, readToken} {
		if strings.Contains(logs, forbidden) {
			t.Fatalf("logs leaked %q:\n%s", forbidden, logs)
		}
	}
	if !strings.Contains(logs, "/v1/providers/{provider}/datasets/{dataset}") {
		t.Fatalf("logs do not carry the route template:\n%s", logs)
	}
	for _, field := range []string{"request_id", "method", "route", "status",
		"duration_ms", "database_ms", "request_bytes", "response_bytes", "mutation_result"} {
		if !strings.Contains(logs, field) {
			t.Fatalf("logs are missing the %q field:\n%s", field, logs)
		}
	}
}

func TestSuccessfulMutationsAreLoggedWithTheirResult(t *testing.T) {
	h := newHarness(t, func(s *stubStore) { s.result = "replaced" })
	h.do(t, call{method: http.MethodPost, target: "/v1/providers/SCB/datasets/Population",
		token: writeToken, contentType: "application/json", body: validBody})
	if !strings.Contains(h.logs.String(), `"mutation_result":"replaced"`) {
		t.Fatalf("logs do not record the mutation result:\n%s", h.logs.String())
	}
}

// -------------------------------------------------------- documentation ---

func TestOpenAPIDocumentIsServedPublicly(t *testing.T) {
	h := newHarness(t, nil)
	recorder := h.do(t, call{method: http.MethodGet, target: "/openapi.json"})
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d", recorder.Code)
	}
	if contentType := recorder.Header().Get("Content-Type"); !strings.Contains(contentType, "openapi+json") {
		t.Fatalf("Content-Type = %q", contentType)
	}
	var document map[string]any
	if err := json.Unmarshal(recorder.Body.Bytes(), &document); err != nil {
		t.Fatalf("the served document is not valid JSON: %v", err)
	}
	if document["openapi"] != "3.1.0" {
		t.Fatalf("openapi = %v, want 3.1.0", document["openapi"])
	}
}

func TestInteractiveDocumentationIsSelfContained(t *testing.T) {
	h := newHarness(t, nil)
	recorder := h.do(t, call{method: http.MethodGet, target: "/docs/"})
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", recorder.Code, recorder.Body.String())
	}
	body := recorder.Body.String()
	if !strings.Contains(strings.ToLower(body), "swagger") {
		t.Fatalf("the documentation page does not look like Swagger UI: %s", body)
	}
	// Every script, stylesheet, and icon must be served from this binary.
	for _, reference := range regexp.MustCompile(`(?:src|href)="([^"]*)"`).FindAllStringSubmatch(body, -1) {
		if !strings.HasPrefix(reference[1], "/docs/") {
			t.Fatalf("the documentation page loads %q from outside the embedded bundle", reference[1])
		}
		asset := h.do(t, call{method: http.MethodGet, target: reference[1]})
		if asset.Code != http.StatusOK || asset.Body.Len() == 0 {
			t.Fatalf("embedded asset %s returned %d with %d bytes", reference[1], asset.Code, asset.Body.Len())
		}
	}
	for _, host := range []string{"unpkg.com", "cdn.jsdelivr.net", "cdnjs.cloudflare.com",
		"petstore.swagger.io", "validator.swagger.io"} {
		if strings.Contains(body, host) {
			t.Fatalf("the documentation page references the external host %q", host)
		}
	}
	if !strings.Contains(body, "persistAuthorization") || !strings.Contains(body, "persistAuthorization: false") {
		t.Fatalf("the documentation page does not disable persistAuthorization: %s", body)
	}
	for _, credential := range []string{writeToken, readToken} {
		if strings.Contains(body, credential) {
			t.Fatal("the documentation page embedded a credential")
		}
	}
}

func TestDocumentationRoutesDegradeWhenNotBundled(t *testing.T) {
	server := New(&stubStore{}, slog.New(slog.NewJSONHandler(io.Discard, nil)), Options{
		ReadWriteToken: writeToken, ReadOnlyToken: readToken,
		MaxRequestBytes: 4096, MaxCells: 1_000_000,
		RequestTimeout: time.Second, DBTimeout: 500 * time.Millisecond,
	})
	for _, target := range []string{"/openapi.json", "/docs/"} {
		request := httptest.NewRequest(http.MethodGet, target, nil)
		recorder := httptest.NewRecorder()
		server.Handler().ServeHTTP(recorder, request)
		assertError(t, recorder, http.StatusServiceUnavailable, "service_unavailable")
	}
}
