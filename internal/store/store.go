package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/nordicintel/nordicintel-storage-api/internal/domain"
)

var ErrNotFound = errors.New("dataset not found")

type ValidationError struct {
	Message string
}

func (e *ValidationError) Error() string { return e.Message }

func Invalid(format string, args ...any) error {
	return &ValidationError{Message: fmt.Sprintf(format, args...)}
}

type ReplaceInput struct {
	ProviderCode string
	DatasetCode  string
	Dimensions   []domain.Dimension
	Cells        []domain.Cell
	SourceStamp  json.RawMessage
}

type ReplaceResult struct {
	Created               bool       `json:"-"`
	ObservationCount      int64      `json:"observation_count"`
	ObservationsUpdatedAt *time.Time `json:"observations_updated_at"`
}

type PatchResult struct {
	InsertedCount         int64      `json:"inserted_count"`
	UpdatedCount          int64      `json:"updated_count"`
	DeletedCount          int64      `json:"deleted_count"`
	ObservationCount      int64      `json:"observation_count"`
	ObservationsUpdatedAt *time.Time `json:"observations_updated_at"`
}

type DataResult struct {
	ProviderCode string
	DatasetCode  string
	Dimensions   []domain.Dimension
	Cells        []domain.Cell
	CellCount    int64
}

type Repository interface {
	Ping(context.Context) error
	Close()
	Providers(context.Context) ([]domain.ProviderSummary, error)
	Datasets(context.Context, *string, bool) ([]domain.DatasetSummary, error)
	Metadata(context.Context, string, string) (domain.DatasetMetadata, error)
	Replace(context.Context, ReplaceInput) (ReplaceResult, error)
	Patch(context.Context, string, string, domain.PatchRequest) (PatchResult, error)
	Delete(context.Context, string, string) error
	FullData(context.Context, string, string) (DataResult, error)
	SelectedData(context.Context, string, string, []domain.Dimension) (DataResult, error)
}
