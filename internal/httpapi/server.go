package httpapi

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/nordicintel/nordicintel-storage-api/internal/domain"
	"github.com/nordicintel/nordicintel-storage-api/internal/store"
)

type Server struct {
	repository      store.Repository
	bearerDigest    [32]byte
	maxRequestBytes int64
	maxCells        int64
	dbTimeout       time.Duration
	logger          *slog.Logger
	handler         http.Handler
}

type requestStateKey struct{}

type requestState struct {
	databaseDuration time.Duration
	started          time.Time
}

func New(repository store.Repository, bearerToken string, maxRequestBytes, maxCells int64, dbTimeout time.Duration, logger *slog.Logger) *Server {
	s := &Server{
		repository:      repository,
		bearerDigest:    sha256.Sum256([]byte(bearerToken)),
		maxRequestBytes: maxRequestBytes,
		maxCells:        maxCells,
		dbTimeout:       dbTimeout,
		logger:          logger,
	}
	api := http.NewServeMux()
	api.HandleFunc("/v1/providers", s.providers)
	api.HandleFunc("/v1/datasets", s.datasets)
	api.HandleFunc("/v1/datasets/{provider}", s.providerDatasets)
	api.HandleFunc("/v1/datasets/{provider}/{dataset}", s.dataset)
	api.HandleFunc("/v1/data/{provider}/{dataset}", s.data)
	api.HandleFunc("/", s.notFound)

	root := http.NewServeMux()
	root.HandleFunc("/health", s.health)
	root.Handle("/v1/", s.authenticate(api))
	root.HandleFunc("/", s.notFound)
	s.handler = s.logRequests(root)
	return s
}

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) { s.handler.ServeHTTP(w, r) }

func (s *Server) health(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		s.methodNotAllowed(w, http.MethodGet)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
	defer cancel()
	err := s.measured(ctx, r, func(ctx context.Context) error { return s.repository.Ping(ctx) })
	if err != nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"status": "unavailable", "database": "unavailable"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok", "database": "ok"})
}

func (s *Server) providers(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		s.methodNotAllowed(w, http.MethodGet)
		return
	}
	var providers []domain.ProviderSummary
	err := s.withDB(r, func(ctx context.Context) error {
		var err error
		providers, err = s.repository.Providers(ctx)
		return err
	})
	if s.handleStoreError(w, err) {
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"providers": providers})
}

func (s *Server) datasets(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		s.methodNotAllowed(w, http.MethodGet)
		return
	}
	full, err := parseFull(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request", err.Error())
		return
	}
	var datasets []domain.DatasetSummary
	err = s.withDB(r, func(ctx context.Context) error {
		var err error
		datasets, err = s.repository.Datasets(ctx, nil, full)
		return err
	})
	if s.handleStoreError(w, err) {
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"datasets": datasets})
}

func (s *Server) providerDatasets(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		s.methodNotAllowed(w, http.MethodGet)
		return
	}
	provider := r.PathValue("provider")
	if err := validIdentity("provider", provider); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request", err.Error())
		return
	}
	full, err := parseFull(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request", err.Error())
		return
	}
	var datasets []domain.DatasetSummary
	err = s.withDB(r, func(ctx context.Context) error {
		var err error
		datasets, err = s.repository.Datasets(ctx, &provider, full)
		return err
	})
	if s.handleStoreError(w, err) {
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"datasets": datasets})
}

func (s *Server) dataset(w http.ResponseWriter, r *http.Request) {
	provider, dataset, ok := s.pathIdentity(w, r)
	if !ok {
		return
	}
	switch r.Method {
	case http.MethodGet:
		var metadata domain.DatasetMetadata
		err := s.withDB(r, func(ctx context.Context) error {
			var err error
			metadata, err = s.repository.Metadata(ctx, provider, dataset)
			return err
		})
		if s.handleStoreError(w, err) {
			return
		}
		writeJSON(w, http.StatusOK, metadata)
	case http.MethodPut:
		s.putDataset(w, r, provider, dataset)
	case http.MethodPatch:
		s.patchDataset(w, r, provider, dataset)
	case http.MethodDelete:
		err := s.withDB(r, func(ctx context.Context) error { return s.repository.Delete(ctx, provider, dataset) })
		if s.handleStoreError(w, err) {
			return
		}
		w.WriteHeader(http.StatusNoContent)
	default:
		s.methodNotAllowed(w, http.MethodGet, http.MethodPut, http.MethodPatch, http.MethodDelete)
	}
}

func (s *Server) putDataset(w http.ResponseWriter, r *http.Request, provider, dataset string) {
	var request domain.PutRequest
	if err := s.decode(w, r, &request); err != nil {
		s.writeDecodeError(w, err)
		return
	}
	cellCount, err := domain.ValidateDimensions(request.Dimensions, s.maxCells)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request", err.Error())
		return
	}
	if int64(len(request.Values)) != cellCount {
		writeError(w, http.StatusBadRequest, "invalid_request", fmt.Sprintf("values must contain exactly %d items", cellCount))
		return
	}
	statuses := make([]*string, cellCount)
	if len(request.Statuses) > 0 {
		if err := json.Unmarshal(request.Statuses, &statuses); err != nil {
			writeError(w, http.StatusBadRequest, "invalid_request", "statuses must be an array of strings or nulls")
			return
		}
		if int64(len(statuses)) != cellCount {
			writeError(w, http.StatusBadRequest, "invalid_request", fmt.Sprintf("statuses must contain exactly %d items", cellCount))
			return
		}
	}
	cells := make([]domain.Cell, 0)
	for i, raw := range request.Values {
		value, err := domain.ParseDecimal(raw)
		if err != nil {
			writeError(w, http.StatusBadRequest, "invalid_request", fmt.Sprintf("values[%d] %v", i, err))
			return
		}
		if value != nil || statuses[i] != nil {
			cells = append(cells, domain.Cell{Index: int64(i), Value: value, StatusCode: statuses[i]})
		}
	}
	var result store.ReplaceResult
	err = s.withDB(r, func(ctx context.Context) error {
		var err error
		result, err = s.repository.Replace(ctx, store.ReplaceInput{
			ProviderCode: provider, DatasetCode: dataset, Dimensions: request.Dimensions,
			Cells: cells, SourceStamp: request.SourceStamp,
		})
		return err
	})
	if s.handleStoreError(w, err) {
		return
	}
	status := http.StatusOK
	operation := "updated"
	if result.Created {
		status = http.StatusCreated
		operation = "created"
	}
	writeJSON(w, status, map[string]any{
		"result": operation, "observation_count": result.ObservationCount,
		"observations_updated_at": result.ObservationsUpdatedAt,
	})
}

func (s *Server) patchDataset(w http.ResponseWriter, r *http.Request, provider, dataset string) {
	var request domain.PatchRequest
	if err := s.decode(w, r, &request); err != nil {
		s.writeDecodeError(w, err)
		return
	}
	var result store.PatchResult
	err := s.withDB(r, func(ctx context.Context) error {
		var err error
		result, err = s.repository.Patch(ctx, provider, dataset, request)
		return err
	})
	if s.handleStoreError(w, err) {
		return
	}
	writeJSON(w, http.StatusOK, result)
}

func (s *Server) data(w http.ResponseWriter, r *http.Request) {
	provider, dataset, ok := s.pathIdentity(w, r)
	if !ok {
		return
	}
	switch r.Method {
	case http.MethodGet:
		var result store.DataResult
		err := s.withDB(r, func(ctx context.Context) error {
			var err error
			result, err = s.repository.FullData(ctx, provider, dataset)
			return err
		})
		if s.handleStoreError(w, err) {
			return
		}
		s.writeData(w, result)
	case http.MethodPost:
		var request domain.SelectionRequest
		if err := s.decode(w, r, &request); err != nil {
			s.writeDecodeError(w, err)
			return
		}
		var result store.DataResult
		err := s.withDB(r, func(ctx context.Context) error {
			var err error
			result, err = s.repository.SelectedData(ctx, provider, dataset, request.Dimensions)
			return err
		})
		if s.handleStoreError(w, err) {
			return
		}
		s.writeData(w, result)
	default:
		s.methodNotAllowed(w, http.MethodGet, http.MethodPost)
	}
}

func (s *Server) writeData(w http.ResponseWriter, result store.DataResult) {
	values := make([]any, int(result.CellCount))
	statuses := make([]any, int(result.CellCount))
	for _, cell := range result.Cells {
		if cell.Index < 0 || cell.Index >= result.CellCount {
			writeError(w, http.StatusInternalServerError, "internal_error", "stored cell index is invalid")
			return
		}
		values[cell.Index] = cell.Value
		statuses[cell.Index] = cell.StatusCode
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"provider_code": result.ProviderCode,
		"dataset_code":  result.DatasetCode,
		"dimensions":    result.Dimensions,
		"values":        values,
		"statuses":      statuses,
	})
}

func (s *Server) pathIdentity(w http.ResponseWriter, r *http.Request) (string, string, bool) {
	provider, dataset := r.PathValue("provider"), r.PathValue("dataset")
	if err := validIdentity("provider", provider); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request", err.Error())
		return "", "", false
	}
	if err := validIdentity("dataset", dataset); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request", err.Error())
		return "", "", false
	}
	return provider, dataset, true
}

func validIdentity(name, value string) error {
	if domain.NormalizeCode(value) == "" {
		return fmt.Errorf("%s code must be non-empty", name)
	}
	return nil
}

func parseFull(r *http.Request) (bool, error) {
	values, exists := r.URL.Query()["full"]
	if !exists {
		return false, nil
	}
	if len(values) != 1 {
		return false, errors.New("full must be supplied once")
	}
	full, err := strconv.ParseBool(values[0])
	if err != nil {
		return false, errors.New("full must be true or false")
	}
	return full, nil
}

func (s *Server) decode(w http.ResponseWriter, r *http.Request, target any) error {
	if contentType := r.Header.Get("Content-Type"); contentType != "" && !strings.HasPrefix(strings.ToLower(contentType), "application/json") {
		return errors.New("Content-Type must be application/json")
	}
	r.Body = http.MaxBytesReader(w, r.Body, s.maxRequestBytes)
	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return err
	}
	var extra any
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		if err == nil {
			return errors.New("request body must contain one JSON value")
		}
		return err
	}
	return nil
}

func (s *Server) writeDecodeError(w http.ResponseWriter, err error) {
	var maxBytes *http.MaxBytesError
	if errors.As(err, &maxBytes) {
		writeError(w, http.StatusRequestEntityTooLarge, "request_too_large", "request body exceeds the configured limit")
		return
	}
	writeError(w, http.StatusBadRequest, "invalid_request", err.Error())
}

func (s *Server) withDB(r *http.Request, fn func(context.Context) error) error {
	deadline := time.Now().Add(s.dbTimeout)
	if state, ok := r.Context().Value(requestStateKey{}).(*requestState); ok {
		deadline = state.started.Add(s.dbTimeout)
	}
	ctx, cancel := context.WithDeadline(r.Context(), deadline)
	defer cancel()
	return s.measured(ctx, r, fn)
}

func (s *Server) measured(ctx context.Context, r *http.Request, fn func(context.Context) error) error {
	started := time.Now()
	err := fn(ctx)
	if state, ok := r.Context().Value(requestStateKey{}).(*requestState); ok {
		state.databaseDuration += time.Since(started)
	}
	return err
}

func (s *Server) handleStoreError(w http.ResponseWriter, err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, store.ErrNotFound) {
		writeError(w, http.StatusNotFound, "not_found", "dataset not found")
		return true
	}
	var invalid *store.ValidationError
	if errors.As(err, &invalid) {
		writeError(w, http.StatusBadRequest, "invalid_request", invalid.Error())
		return true
	}
	var postgresError *pgconn.PgError
	if errors.As(err, &postgresError) && (strings.HasPrefix(postgresError.Code, "22") || strings.HasPrefix(postgresError.Code, "23")) {
		writeError(w, http.StatusBadRequest, "invalid_request", "request violates a storage constraint")
		return true
	}
	if errors.Is(err, context.DeadlineExceeded) {
		writeError(w, http.StatusServiceUnavailable, "database_timeout", "database operation timed out")
		return true
	}
	s.logger.Error("request storage failure", "error", err)
	writeError(w, http.StatusInternalServerError, "internal_error", "internal server error")
	return true
}

func (s *Server) authenticate(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		const prefix = "Bearer "
		header := r.Header.Get("Authorization")
		if !strings.HasPrefix(header, prefix) {
			w.Header().Set("WWW-Authenticate", "Bearer")
			writeError(w, http.StatusUnauthorized, "unauthorized", "valid bearer token required")
			return
		}
		provided := sha256.Sum256([]byte(strings.TrimPrefix(header, prefix)))
		if subtle.ConstantTimeCompare(provided[:], s.bearerDigest[:]) != 1 {
			w.Header().Set("WWW-Authenticate", "Bearer")
			writeError(w, http.StatusUnauthorized, "unauthorized", "valid bearer token required")
			return
		}
		next.ServeHTTP(w, r)
	})
}

type responseRecorder struct {
	http.ResponseWriter
	status int
}

func (w *responseRecorder) WriteHeader(status int) {
	w.status = status
	w.ResponseWriter.WriteHeader(status)
}

func (w *responseRecorder) Write(body []byte) (int, error) {
	if w.status == 0 {
		w.status = http.StatusOK
	}
	return w.ResponseWriter.Write(body)
}

func (s *Server) logRequests(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		started := time.Now()
		state := &requestState{started: started}
		requestID := newRequestID()
		w.Header().Set("X-Request-ID", requestID)
		recorder := &responseRecorder{ResponseWriter: w}
		request := r.WithContext(context.WithValue(r.Context(), requestStateKey{}, state))
		next.ServeHTTP(recorder, request)
		status := recorder.status
		if status == 0 {
			status = http.StatusOK
		}
		s.logger.Info("http request",
			"request_id", requestID,
			"method", r.Method,
			"route", request.Pattern,
			"status", status,
			"duration_ms", time.Since(started).Milliseconds(),
			"database_ms", state.databaseDuration.Milliseconds(),
		)
	})
}

func newRequestID() string {
	var value [12]byte
	if _, err := rand.Read(value[:]); err != nil {
		return strconv.FormatInt(time.Now().UnixNano(), 36)
	}
	return hex.EncodeToString(value[:])
}

func (s *Server) methodNotAllowed(w http.ResponseWriter, methods ...string) {
	w.Header().Set("Allow", strings.Join(methods, ", "))
	writeError(w, http.StatusMethodNotAllowed, "method_not_allowed", "method not allowed")
}

func (s *Server) notFound(w http.ResponseWriter, _ *http.Request) {
	writeError(w, http.StatusNotFound, "not_found", "route not found")
}

func writeError(w http.ResponseWriter, status int, code, message string) {
	writeJSON(w, status, map[string]any{"error": map[string]string{"code": code, "message": message}})
}

func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}
