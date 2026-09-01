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
	"mime"
	"net/http"
	"strings"
	"time"

	"github.com/nordicintel/nordicintel-storage-api/internal/domain"
	"github.com/nordicintel/nordicintel-storage-api/internal/presentation"
	"github.com/nordicintel/nordicintel-storage-api/internal/store"
)

type Options struct {
	ReadWriteToken  string
	ReadOnlyToken   string
	MaxRequestBytes int64
	MaxCells        int64
	RequestTimeout  time.Duration
	DBTimeout       time.Duration
	OpenAPI         []byte
	Docs            http.Handler
}

type Server struct {
	store           store.Store
	logger          *slog.Logger
	mux             *http.ServeMux
	readWriteDigest [sha256.Size]byte
	readOnlyDigest  [sha256.Size]byte
	maxRequestBytes int64
	maxCells        int64
	requestTimeout  time.Duration
	dbTimeout       time.Duration
	openAPI         []byte
	docs            http.Handler
}

type requestState struct {
	requestID     string
	route         string
	databaseTime  time.Duration
	requestBytes  int64
	responseBytes int64
	status        int
	mutation      string
}

type stateKey struct{}

func New(database store.Store, logger *slog.Logger, options Options) *Server {
	server := &Server{
		store: database, logger: logger,
		readWriteDigest: sha256.Sum256([]byte(options.ReadWriteToken)),
		readOnlyDigest:  sha256.Sum256([]byte(options.ReadOnlyToken)),
		maxRequestBytes: options.MaxRequestBytes, maxCells: options.MaxCells,
		requestTimeout: options.RequestTimeout, dbTimeout: options.DBTimeout,
		openAPI: append([]byte(nil), options.OpenAPI...), docs: options.Docs,
		mux: http.NewServeMux(),
	}
	server.routes()
	return server
}

func (s *Server) Handler() http.Handler { return s }

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	requestID := newRequestID()
	state := &requestState{requestID: requestID}
	ctx, cancel := context.WithTimeout(context.WithValue(r.Context(), stateKey{}, state), s.requestTimeout)
	defer cancel()
	r = r.WithContext(ctx)
	w.Header().Set("X-Request-ID", requestID)
	w.Header().Set("Cache-Control", "no-store")
	recorder := &responseRecorder{ResponseWriter: w, state: state}
	started := time.Now()
	defer func() {
		if recovered := recover(); recovered != nil {
			s.logger.Error("request panic", "request_id", requestID, "route", state.route)
			if state.status == 0 {
				s.writeError(recorder, r, http.StatusInternalServerError, "internal_error", "unexpected internal failure")
			}
		}
		status := state.status
		if status == 0 {
			status = http.StatusOK
		}
		s.logger.Info("request",
			"request_id", requestID,
			"method", r.Method,
			"route", state.route,
			"status", status,
			"duration_ms", time.Since(started).Milliseconds(),
			"database_ms", state.databaseTime.Milliseconds(),
			"request_bytes", state.requestBytes,
			"response_bytes", state.responseBytes,
			"mutation_result", state.mutation,
		)
	}()
	s.mux.ServeHTTP(recorder, r)
}

// Route describes one registered path template and the methods it accepts.
// Tests compare this table against the OpenAPI document so the contract, the
// documentation, and the multiplexer can never drift apart.
type Route struct {
	Pattern string
	Methods []string
	Public  bool
}

// Routes returns the complete registered route table in registration order.
func Routes() []Route {
	return []Route{
		{Pattern: "/health", Methods: []string{http.MethodGet}, Public: true},
		{Pattern: "/openapi.json", Methods: []string{http.MethodGet}, Public: true},
		{Pattern: "/docs", Methods: []string{http.MethodGet}, Public: true},
		{Pattern: "/docs/", Methods: []string{http.MethodGet}, Public: true},
		{Pattern: "/v1/providers", Methods: []string{http.MethodGet}},
		{Pattern: "/v1/providers/{provider}/datasets", Methods: []string{http.MethodGet}},
		{Pattern: "/v1/providers/{provider}/datasets/{dataset}", Methods: []string{http.MethodGet, http.MethodPost, http.MethodDelete}},
		{Pattern: "/v1/providers/{provider}/datasets/{dataset}/structure", Methods: []string{http.MethodGet}},
		{Pattern: "/v1/providers/{provider}/datasets/{dataset}/data", Methods: []string{http.MethodGet}},
		{Pattern: "/v1/providers/{provider}/datasets/{dataset}/query", Methods: []string{http.MethodPost}},
	}
}

func (s *Server) routes() {
	handlers := map[string]http.HandlerFunc{
		"/health":                           s.handleHealth,
		"/openapi.json":                     s.handleOpenAPI,
		"/docs":                             s.handleDocsRedirect,
		"/docs/":                            s.handleDocs,
		"/v1/providers":                     s.handleProviders,
		"/v1/providers/{provider}/datasets": s.handleDatasets,
		"/v1/providers/{provider}/datasets/{dataset}":           s.handleDataset,
		"/v1/providers/{provider}/datasets/{dataset}/structure": s.handleStructure,
		"/v1/providers/{provider}/datasets/{dataset}/data":      s.handleData,
		"/v1/providers/{provider}/datasets/{dataset}/query":     s.handleQuery,
	}
	for _, route := range Routes() {
		handler, registered := handlers[route.Pattern]
		if !registered {
			panic("no handler registered for " + route.Pattern)
		}
		s.mux.HandleFunc(route.Pattern, handler)
	}
	s.mux.HandleFunc("/", s.handleNotFound)
}

func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	setRoute(r, "/health")
	if r.Method != http.MethodGet {
		s.methodNotAllowed(w, r, http.MethodGet)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
	defer cancel()
	if err := s.databaseCall(r, func() error { return s.store.Ping(ctx) }); err != nil {
		s.writeJSON(w, http.StatusServiceUnavailable, map[string]string{"status": "unavailable"})
		return
	}
	s.writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

func (s *Server) handleProviders(w http.ResponseWriter, r *http.Request) {
	setRoute(r, "/v1/providers")
	if r.Method != http.MethodGet {
		s.methodNotAllowed(w, r, http.MethodGet)
		return
	}
	if !s.authorize(w, r, false) || !s.validateNoQuery(w, r) {
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), s.dbTimeout)
	defer cancel()
	var providers []domain.ProviderListItem
	err := s.databaseCall(r, func() error {
		var err error
		providers, err = s.store.ListProviders(ctx)
		return err
	})
	if err != nil {
		s.writeStoreError(w, r, err)
		return
	}
	if providers == nil {
		providers = []domain.ProviderListItem{}
	}
	s.writeJSON(w, http.StatusOK, map[string]any{"providers": providers})
}

func (s *Server) handleDatasets(w http.ResponseWriter, r *http.Request) {
	setRoute(r, "/v1/providers/{provider}/datasets")
	if r.Method != http.MethodGet {
		s.methodNotAllowed(w, r, http.MethodGet)
		return
	}
	if !s.authorize(w, r, false) || !s.validateNoQuery(w, r) {
		return
	}
	provider, ok := s.pathCode(w, r, "provider")
	if !ok {
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), s.dbTimeout)
	defer cancel()
	var spelling string
	var datasets []domain.Summary
	err := s.databaseCall(r, func() error {
		var err error
		spelling, datasets, err = s.store.ListDatasets(ctx, provider)
		return err
	})
	if err != nil {
		s.writeStoreError(w, r, err)
		return
	}
	if datasets == nil {
		datasets = []domain.Summary{}
	}
	s.writeJSON(w, http.StatusOK, map[string]any{"provider_code": spelling, "datasets": datasets})
}

func (s *Server) handleDataset(w http.ResponseWriter, r *http.Request) {
	setRoute(r, "/v1/providers/{provider}/datasets/{dataset}")
	switch r.Method {
	case http.MethodGet:
		s.getSummary(w, r)
	case http.MethodPost:
		s.replace(w, r)
	case http.MethodDelete:
		s.deleteDataset(w, r)
	default:
		s.methodNotAllowed(w, r, http.MethodGet, http.MethodPost, http.MethodDelete)
	}
}

func (s *Server) getSummary(w http.ResponseWriter, r *http.Request) {
	if !s.authorize(w, r, false) || !s.validateNoQuery(w, r) {
		return
	}
	provider, dataset, ok := s.pathIdentity(w, r)
	if !ok {
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), s.dbTimeout)
	defer cancel()
	var summary domain.Summary
	err := s.databaseCall(r, func() error {
		var err error
		summary, err = s.store.GetSummary(ctx, provider, dataset)
		return err
	})
	if err != nil {
		s.writeStoreError(w, r, err)
		return
	}
	s.writeJSON(w, http.StatusOK, summary)
}

func (s *Server) replace(w http.ResponseWriter, r *http.Request) {
	if !s.authorize(w, r, true) || !s.validateNoQuery(w, r) || !s.requireJSON(w, r) {
		return
	}
	providerSpelling := r.PathValue("provider")
	datasetSpelling := r.PathValue("dataset")
	if !validPathSpelling(providerSpelling) || !validPathSpelling(datasetSpelling) {
		s.writeError(w, r, http.StatusBadRequest, "invalid_path_code", "provider and dataset path codes must be non-empty single segments")
		return
	}
	body, ok := s.readBody(w, r)
	if !ok {
		return
	}
	replacement, err := domain.ParseReplacement(providerSpelling, datasetSpelling, body, s.maxCells)
	if err != nil {
		if errors.Is(err, domain.ErrInvalidJSON) {
			s.writeError(w, r, http.StatusBadRequest, "invalid_json", err.Error())
			return
		}
		var limit domain.CellLimitError
		if errors.As(err, &limit) {
			s.writeError(w, r, http.StatusUnprocessableEntity, "cell_limit_exceeded", err.Error())
			return
		}
		s.writeError(w, r, http.StatusUnprocessableEntity, "validation_failed", err.Error())
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), s.dbTimeout)
	defer cancel()
	var result string
	var summary domain.Summary
	err = s.databaseCall(r, func() error {
		var err error
		result, summary, err = s.store.Replace(ctx, replacement)
		return err
	})
	if err != nil {
		s.writeStoreError(w, r, err)
		return
	}
	requestStateFrom(r).mutation = result
	status := http.StatusOK
	if result == "created" {
		status = http.StatusCreated
	}
	s.writeJSON(w, status, map[string]any{"result": result, "dataset": summary})
}

func (s *Server) deleteDataset(w http.ResponseWriter, r *http.Request) {
	if !s.authorize(w, r, true) || !s.validateNoQuery(w, r) {
		return
	}
	provider, dataset, ok := s.pathIdentity(w, r)
	if !ok {
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), s.dbTimeout)
	defer cancel()
	err := s.databaseCall(r, func() error { return s.store.Delete(ctx, provider, dataset) })
	if err != nil {
		s.writeStoreError(w, r, err)
		return
	}
	requestStateFrom(r).mutation = "deleted_or_absent"
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleStructure(w http.ResponseWriter, r *http.Request) {
	setRoute(r, "/v1/providers/{provider}/datasets/{dataset}/structure")
	if r.Method != http.MethodGet {
		s.methodNotAllowed(w, r, http.MethodGet)
		return
	}
	if !s.authorize(w, r, false) || !s.validateNoQuery(w, r) {
		return
	}
	provider, dataset, ok := s.pathIdentity(w, r)
	if !ok {
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), s.dbTimeout)
	defer cancel()
	var view domain.View
	err := s.databaseCall(r, func() error {
		var err error
		view, err = s.store.GetStructure(ctx, provider, dataset)
		return err
	})
	if err != nil {
		s.writeStoreError(w, r, err)
		return
	}
	response, err := presentation.StructureResponse(view)
	if err != nil {
		s.writeError(w, r, http.StatusInternalServerError, "internal_error", "failed to encode stored structure")
		return
	}
	s.writeJSON(w, http.StatusOK, response)
}

func (s *Server) handleData(w http.ResponseWriter, r *http.Request) {
	setRoute(r, "/v1/providers/{provider}/datasets/{dataset}/data")
	if r.Method != http.MethodGet {
		s.methodNotAllowed(w, r, http.MethodGet)
		return
	}
	if !s.authorize(w, r, false) {
		return
	}
	format, ok := s.readFormat(w, r)
	if !ok {
		return
	}
	provider, dataset, ok := s.pathIdentity(w, r)
	if !ok {
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), s.dbTimeout)
	defer cancel()
	var view domain.View
	err := s.databaseCall(r, func() error {
		var err error
		view, err = s.store.GetData(ctx, provider, dataset)
		return err
	})
	if err != nil {
		s.writeStoreError(w, r, err)
		return
	}
	s.writeData(w, r, view, format)
}

func (s *Server) handleQuery(w http.ResponseWriter, r *http.Request) {
	setRoute(r, "/v1/providers/{provider}/datasets/{dataset}/query")
	if r.Method != http.MethodPost {
		s.methodNotAllowed(w, r, http.MethodPost)
		return
	}
	if !s.authorize(w, r, false) || !s.requireJSON(w, r) {
		return
	}
	format, ok := s.readFormat(w, r)
	if !ok {
		return
	}
	provider, dataset, ok := s.pathIdentity(w, r)
	if !ok {
		return
	}
	body, ok := s.readBody(w, r)
	if !ok {
		return
	}
	selection, err := domain.ParseSelection(body)
	if err != nil {
		if errors.Is(err, domain.ErrInvalidJSON) {
			s.writeError(w, r, http.StatusBadRequest, "invalid_json", err.Error())
			return
		}
		s.writeError(w, r, http.StatusUnprocessableEntity, "validation_failed", err.Error())
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), s.dbTimeout)
	defer cancel()
	var view domain.View
	err = s.databaseCall(r, func() error {
		var err error
		view, err = s.store.Query(ctx, provider, dataset, selection, s.maxCells)
		return err
	})
	if err != nil {
		var limit domain.CellLimitError
		if errors.As(err, &limit) {
			s.writeError(w, r, http.StatusUnprocessableEntity, "cell_limit_exceeded", err.Error())
			return
		}
		if errors.Is(err, store.ErrInvalidSelection) {
			s.writeError(w, r, http.StatusUnprocessableEntity, "validation_failed", err.Error())
			return
		}
		s.writeStoreError(w, r, err)
		return
	}
	s.writeData(w, r, view, format)
}

func (s *Server) writeData(w http.ResponseWriter, r *http.Request, view domain.View, format string) {
	response, err := presentation.DataResponse(view, format)
	if err != nil {
		s.writeError(w, r, http.StatusInternalServerError, "internal_error", "failed to encode stored data")
		return
	}
	s.writeJSON(w, http.StatusOK, response)
}

func (s *Server) handleOpenAPI(w http.ResponseWriter, r *http.Request) {
	setRoute(r, "/openapi.json")
	if r.Method != http.MethodGet {
		s.methodNotAllowed(w, r, http.MethodGet)
		return
	}
	if len(s.openAPI) == 0 {
		s.writeError(w, r, http.StatusServiceUnavailable, "service_unavailable", "API documentation is unavailable")
		return
	}
	w.Header().Set("Content-Type", "application/vnd.oai.openapi+json;version=3.1")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(s.openAPI)
}

func (s *Server) handleDocsRedirect(w http.ResponseWriter, r *http.Request) {
	setRoute(r, "/docs")
	if r.Method != http.MethodGet {
		s.methodNotAllowed(w, r, http.MethodGet)
		return
	}
	http.Redirect(w, r, "/docs/", http.StatusPermanentRedirect)
}

func (s *Server) handleDocs(w http.ResponseWriter, r *http.Request) {
	setRoute(r, "/docs/")
	if r.Method != http.MethodGet {
		s.methodNotAllowed(w, r, http.MethodGet)
		return
	}
	if s.docs == nil {
		s.writeError(w, r, http.StatusServiceUnavailable, "service_unavailable", "interactive documentation is unavailable")
		return
	}
	s.docs.ServeHTTP(w, r)
}

func (s *Server) handleNotFound(w http.ResponseWriter, r *http.Request) {
	setRoute(r, "unmatched")
	s.writeError(w, r, http.StatusNotFound, "not_found", "route not found")
}

func (s *Server) authorize(w http.ResponseWriter, r *http.Request, mutation bool) bool {
	header := r.Header.Get("Authorization")
	if !strings.HasPrefix(header, "Bearer ") || len(header) == len("Bearer ") {
		w.Header().Set("WWW-Authenticate", "Bearer")
		s.writeError(w, r, http.StatusUnauthorized, "unauthorized", "missing or invalid bearer token")
		return false
	}
	supplied := sha256.Sum256([]byte(header[len("Bearer "):]))
	writeMatch := subtle.ConstantTimeCompare(supplied[:], s.readWriteDigest[:])
	readMatch := subtle.ConstantTimeCompare(supplied[:], s.readOnlyDigest[:])
	if writeMatch != 1 && readMatch != 1 {
		w.Header().Set("WWW-Authenticate", "Bearer")
		s.writeError(w, r, http.StatusUnauthorized, "unauthorized", "missing or invalid bearer token")
		return false
	}
	if mutation && writeMatch != 1 {
		s.writeError(w, r, http.StatusForbidden, "forbidden", "read-only credentials cannot mutate data")
		return false
	}
	return true
}

func (s *Server) requireJSON(w http.ResponseWriter, r *http.Request) bool {
	if encoding := r.Header.Get("Content-Encoding"); encoding != "" && !strings.EqualFold(encoding, "identity") {
		s.writeError(w, r, http.StatusUnsupportedMediaType, "unsupported_media_type", "content encoding is unsupported")
		return false
	}
	mediaType, params, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil || !strings.EqualFold(mediaType, "application/json") {
		s.writeError(w, r, http.StatusUnsupportedMediaType, "unsupported_media_type", "Content-Type must be application/json")
		return false
	}
	for key, value := range params {
		if !strings.EqualFold(key, "charset") || !strings.EqualFold(value, "utf-8") {
			s.writeError(w, r, http.StatusUnsupportedMediaType, "unsupported_media_type", "only UTF-8 JSON is supported")
			return false
		}
	}
	return true
}

func (s *Server) readBody(w http.ResponseWriter, r *http.Request) ([]byte, bool) {
	if r.ContentLength > s.maxRequestBytes {
		s.writeError(w, r, http.StatusRequestEntityTooLarge, "request_too_large", "request body exceeds the configured limit")
		return nil, false
	}
	data, err := io.ReadAll(io.LimitReader(r.Body, s.maxRequestBytes+1))
	requestStateFrom(r).requestBytes = int64(len(data))
	if err != nil {
		s.writeError(w, r, http.StatusBadRequest, "invalid_json", "failed to read request body")
		return nil, false
	}
	if int64(len(data)) > s.maxRequestBytes {
		s.writeError(w, r, http.StatusRequestEntityTooLarge, "request_too_large", "request body exceeds the configured limit")
		return nil, false
	}
	return data, true
}

func (s *Server) readFormat(w http.ResponseWriter, r *http.Request) (string, bool) {
	query := r.URL.Query()
	if len(query) == 0 {
		return "sparse", true
	}
	values, exists := query["format"]
	if len(query) != 1 || !exists || len(values) != 1 || (values[0] != "dense" && values[0] != "sparse") {
		s.writeError(w, r, http.StatusBadRequest, "invalid_query", "query must contain at most one format=dense|sparse parameter")
		return "", false
	}
	return values[0], true
}

func (s *Server) validateNoQuery(w http.ResponseWriter, r *http.Request) bool {
	if r.URL.RawQuery != "" {
		s.writeError(w, r, http.StatusBadRequest, "invalid_query", "this route does not accept query parameters")
		return false
	}
	return true
}

func (s *Server) pathIdentity(w http.ResponseWriter, r *http.Request) (domain.Code, domain.Code, bool) {
	provider, ok := s.pathCode(w, r, "provider")
	if !ok {
		return domain.Code{}, domain.Code{}, false
	}
	dataset, ok := s.pathCode(w, r, "dataset")
	return provider, dataset, ok
}

func (s *Server) pathCode(w http.ResponseWriter, r *http.Request, name string) (domain.Code, bool) {
	spelling := r.PathValue(name)
	if !validPathSpelling(spelling) {
		s.writeError(w, r, http.StatusBadRequest, "invalid_path_code", name+" path code must be a non-empty single segment")
		return domain.Code{}, false
	}
	code, err := domain.NormalizeCode(spelling)
	if err != nil {
		s.writeError(w, r, http.StatusBadRequest, "invalid_path_code", name+" path code is invalid")
		return domain.Code{}, false
	}
	return code, true
}

func validPathSpelling(value string) bool { return value != "" && !strings.Contains(value, "/") }

func (s *Server) databaseCall(r *http.Request, fn func() error) error {
	started := time.Now()
	err := fn()
	requestStateFrom(r).databaseTime += time.Since(started)
	return err
}

func (s *Server) writeStoreError(w http.ResponseWriter, r *http.Request, err error) {
	switch {
	case errors.Is(err, store.ErrNotFound):
		s.writeError(w, r, http.StatusNotFound, "not_found", "provider or dataset not found")
	case errors.Is(err, store.ErrDatasetExists):
		s.writeError(w, r, http.StatusConflict, "dataset_exists", "dataset already exists; set replace to true to overwrite it")
	case errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) || r.Context().Err() != nil:
		s.writeError(w, r, http.StatusServiceUnavailable, "service_unavailable", "operation deadline exceeded or request cancelled")
	default:
		s.logger.Error("database operation failed", "request_id", requestStateFrom(r).requestID, "route", requestStateFrom(r).route, "error", err)
		s.writeError(w, r, http.StatusInternalServerError, "internal_error", "unexpected internal failure")
	}
}

func (s *Server) methodNotAllowed(w http.ResponseWriter, r *http.Request, methods ...string) {
	w.Header().Set("Allow", strings.Join(methods, ", "))
	s.writeError(w, r, http.StatusMethodNotAllowed, "method_not_allowed", "method not allowed")
}

func (s *Server) writeError(w http.ResponseWriter, r *http.Request, status int, code, message string) {
	s.writeJSON(w, status, map[string]any{"error": map[string]string{
		"code": code, "message": message, "request_id": requestStateFrom(r).requestID,
	}})
}

func (s *Server) writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(value); err != nil {
		s.logger.Warn("response write failed", "error", err)
	}
}

func requestStateFrom(r *http.Request) *requestState {
	return r.Context().Value(stateKey{}).(*requestState)
}

func setRoute(r *http.Request, route string) { requestStateFrom(r).route = route }

func newRequestID() string {
	var id [16]byte
	if _, err := rand.Read(id[:]); err != nil {
		panic(fmt.Errorf("generate request ID: %w", err))
	}
	return hex.EncodeToString(id[:])
}

type responseRecorder struct {
	http.ResponseWriter
	state *requestState
}

func (w *responseRecorder) WriteHeader(status int) {
	if w.state.status != 0 {
		return
	}
	w.state.status = status
	w.ResponseWriter.WriteHeader(status)
}

func (w *responseRecorder) Write(data []byte) (int, error) {
	if w.state.status == 0 {
		w.WriteHeader(http.StatusOK)
	}
	n, err := w.ResponseWriter.Write(data)
	w.state.responseBytes += int64(n)
	return n, err
}
