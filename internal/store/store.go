package store

import (
	"context"
	"errors"

	"github.com/nordicintel/nordicintel-storage-api/internal/domain"
)

var (
	ErrNotFound         = errors.New("not found")
	ErrDatasetExists    = errors.New("dataset exists")
	ErrInvalidSelection = errors.New("invalid selection")
)

type Store interface {
	Ping(context.Context) error
	ListProviders(context.Context) ([]domain.ProviderListItem, error)
	ListDatasets(context.Context, domain.Code) (string, []domain.Summary, error)
	GetSummary(context.Context, domain.Code, domain.Code) (domain.Summary, error)
	GetStructure(context.Context, domain.Code, domain.Code) (domain.View, error)
	GetData(context.Context, domain.Code, domain.Code) (domain.View, error)
	Query(context.Context, domain.Code, domain.Code, domain.Selection, int64) (domain.View, error)
	Replace(context.Context, domain.Replacement) (string, domain.Summary, error)
	Delete(context.Context, domain.Code, domain.Code) error
	Close()
}
