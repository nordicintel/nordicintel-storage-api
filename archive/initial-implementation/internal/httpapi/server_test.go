package httpapi

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/nordicintel/nordicintel-storage-api/internal/domain"
	"github.com/nordicintel/nordicintel-storage-api/internal/store"
)

type fakeRepository struct {
	replaceInput store.ReplaceInput
	data         store.DataResult
}

func (f *fakeRepository) Ping(context.Context) error { return nil }
func (f *fakeRepository) Close()                     {}
func (f *fakeRepository) Providers(context.Context) ([]domain.ProviderSummary, error) {
	return []domain.ProviderSummary{}, nil
}
func (f *fakeRepository) Datasets(context.Context, *string, bool) ([]domain.DatasetSummary, error) {
	return []domain.DatasetSummary{}, nil
}
func (f *fakeRepository) Metadata(context.Context, string, string) (domain.DatasetMetadata, error) {
	return domain.DatasetMetadata{}, store.ErrNotFound
}
func (f *fakeRepository) Replace(_ context.Context, input store.ReplaceInput) (store.ReplaceResult, error) {
	f.replaceInput = input
	now := time.Unix(1, 0).UTC()
	return store.ReplaceResult{Created: true, ObservationCount: int64(len(input.Cells)), ObservationsUpdatedAt: &now}, nil
}
func (f *fakeRepository) Patch(context.Context, string, string, domain.PatchRequest) (store.PatchResult, error) {
	return store.PatchResult{}, nil
}
func (f *fakeRepository) Delete(context.Context, string, string) error { return nil }
func (f *fakeRepository) FullData(context.Context, string, string) (store.DataResult, error) {
	return f.data, nil
}
func (f *fakeRepository) SelectedData(context.Context, string, string, []domain.Dimension) (store.DataResult, error) {
	return f.data, nil
}

func testServer(repository store.Repository) *Server {
	return New(repository, "test-token", 1<<20, 100, time.Second, slog.New(slog.NewTextHandler(io.Discard, nil)))
}

func TestHealthDoesNotRequireAuthentication(t *testing.T) {
	recorder := httptest.NewRecorder()
	testServer(&fakeRepository{}).ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/health", nil))
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", recorder.Code)
	}
}

func TestV1RequiresAuthentication(t *testing.T) {
	recorder := httptest.NewRecorder()
	testServer(&fakeRepository{}).ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/v1/providers", nil))
	if recorder.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", recorder.Code)
	}
}

func TestPutStrictValidationAndSparseCells(t *testing.T) {
	repository := &fakeRepository{}
	body := `{"source_stamp":null,"dimensions":[{"code":"Sex","categories":["M","F"]}],"values":[12345678901234567890.01,null],"statuses":[null,"p"]}`
	request := httptest.NewRequest(http.MethodPut, "/v1/datasets/P/D", strings.NewReader(body))
	request.Header.Set("Authorization", "Bearer test-token")
	request.Header.Set("Content-Type", "application/json")
	recorder := httptest.NewRecorder()
	testServer(repository).ServeHTTP(recorder, request)
	if recorder.Code != http.StatusCreated {
		t.Fatalf("status = %d, body = %s", recorder.Code, recorder.Body.String())
	}
	if len(repository.replaceInput.Cells) != 2 || string(*repository.replaceInput.Cells[0].Value) != "12345678901234567890.01" {
		t.Fatalf("unexpected cells: %#v", repository.replaceInput.Cells)
	}
	if string(repository.replaceInput.SourceStamp) != "null" {
		t.Fatalf("source stamp = %s, want explicit null", repository.replaceInput.SourceStamp)
	}
}

func TestPutRejectsUnknownFields(t *testing.T) {
	body := `{"dimensions":[{"code":"A","categories":["1"]}],"values":[1],"extra":true}`
	request := httptest.NewRequest(http.MethodPut, "/v1/datasets/P/D", strings.NewReader(body))
	request.Header.Set("Authorization", "Bearer test-token")
	recorder := httptest.NewRecorder()
	testServer(&fakeRepository{}).ServeHTTP(recorder, request)
	if recorder.Code != http.StatusBadRequest || !strings.Contains(recorder.Body.String(), "unknown field") {
		t.Fatalf("status/body = %d %s", recorder.Code, recorder.Body.String())
	}
}

func TestPutRejectsOversizedBody(t *testing.T) {
	server := New(&fakeRepository{}, "test-token", 32, 100, time.Second, slog.New(slog.NewTextHandler(io.Discard, nil)))
	request := httptest.NewRequest(http.MethodPut, "/v1/datasets/P/D", strings.NewReader(`"`+strings.Repeat("x", 31)+`"`))
	request.Header.Set("Authorization", "Bearer test-token")
	recorder := httptest.NewRecorder()
	server.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("status = %d, body = %s", recorder.Code, recorder.Body.String())
	}
}

func TestMethodNotAllowedUsesJSONError(t *testing.T) {
	request := httptest.NewRequest(http.MethodPost, "/v1/providers", nil)
	request.Header.Set("Authorization", "Bearer test-token")
	recorder := httptest.NewRecorder()
	testServer(&fakeRepository{}).ServeHTTP(recorder, request)
	if recorder.Code != http.StatusMethodNotAllowed || recorder.Header().Get("Allow") != http.MethodGet {
		t.Fatalf("status/allow = %d %q", recorder.Code, recorder.Header().Get("Allow"))
	}
	if !strings.Contains(recorder.Header().Get("Content-Type"), "application/json") {
		t.Fatalf("Content-Type = %q", recorder.Header().Get("Content-Type"))
	}
}

func TestDataEmitsExactNumberAndMissingCells(t *testing.T) {
	value := domain.Decimal("12345678901234567890.01")
	repository := &fakeRepository{data: store.DataResult{
		ProviderCode: "P", DatasetCode: "D", CellCount: 2,
		Dimensions: []domain.Dimension{{Code: "A", Categories: []string{"1", "2"}}},
		Cells:      []domain.Cell{{Index: 0, Value: &value}},
	}}
	request := httptest.NewRequest(http.MethodGet, "/v1/data/P/D", nil)
	request.Header.Set("Authorization", "Bearer test-token")
	recorder := httptest.NewRecorder()
	testServer(repository).ServeHTTP(recorder, request)
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d", recorder.Code)
	}
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(recorder.Body.Bytes(), &raw); err != nil {
		t.Fatal(err)
	}
	if got := string(raw["values"]); got != "[12345678901234567890.01,null]" {
		t.Fatalf("values = %s", got)
	}
}
