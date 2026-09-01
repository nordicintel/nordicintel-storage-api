package store

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/nordicintel/nordicintel-storage-api/internal/domain"
	"github.com/nordicintel/nordicintel-storage-api/internal/migrations"
)

const queryBatchSize = 10_000

type transactionCheckpoints struct {
	BeforeStructure func(context.Context) error
	BeforeCopy      func(context.Context) error
	BeforeCommit    func(context.Context) error
}

type Postgres struct {
	pool        *pgxpool.Pool
	checkpoints *transactionCheckpoints
}

func Open(ctx context.Context, databaseURL string, maxConns int32) (*Postgres, error) {
	config, err := pgxpool.ParseConfig(databaseURL)
	if err != nil {
		return nil, fmt.Errorf("parse DATABASE_URL: %w", err)
	}
	config.MaxConns = maxConns
	config.MinConns = 0
	pool, err := pgxpool.NewWithConfig(ctx, config)
	if err != nil {
		return nil, fmt.Errorf("create database pool: %w", err)
	}
	postgres := &Postgres{pool: pool}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("ping database: %w", err)
	}
	if err := migrations.CheckServer(ctx, pool); err != nil {
		pool.Close()
		return nil, err
	}
	if err := migrations.CheckVersion(ctx, pool); err != nil {
		pool.Close()
		return nil, err
	}
	return postgres, nil
}

func (p *Postgres) Close() { p.pool.Close() }

func (p *Postgres) Ping(ctx context.Context) error { return p.pool.Ping(ctx) }

func (p *Postgres) ListProviders(ctx context.Context) ([]domain.ProviderListItem, error) {
	rows, err := p.pool.Query(ctx, `
		select p.provider_code, count(d.dataset_id)::bigint
		from storage.providers p
		join storage.datasets d on d.provider_id = p.provider_id
		group by p.provider_id, p.provider_code, p.provider_key
		order by p.provider_key collate "C"
	`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	items := make([]domain.ProviderListItem, 0)
	for rows.Next() {
		var item domain.ProviderListItem
		if err := rows.Scan(&item.ProviderCode, &item.DatasetCount); err != nil {
			return nil, err
		}
		items = append(items, item)
	}
	return items, rows.Err()
}

func (p *Postgres) ListDatasets(ctx context.Context, provider domain.Code) (string, []domain.Summary, error) {
	rows, err := p.pool.Query(ctx, `
		select p.provider_code, d.dataset_code, d.source_stamp, d.cell_count,
		       d.valued_cell_count, d.null_cell_count, d.updated_at
		from storage.providers p
		join storage.datasets d on d.provider_id = p.provider_id
		where p.provider_key = $1
		order by d.dataset_key collate "C"
	`, provider.Key)
	if err != nil {
		return "", nil, err
	}
	defer rows.Close()
	var providerSpelling string
	summaries := make([]domain.Summary, 0)
	for rows.Next() {
		var summary domain.Summary
		var stamp []byte
		if err := rows.Scan(&summary.ProviderCode, &summary.DatasetCode, &stamp, &summary.CellCount,
			&summary.ValuedCellCount, &summary.NullCellCount, &summary.UpdatedAt); err != nil {
			return "", nil, err
		}
		summary.SourceStamp = append(json.RawMessage(nil), stamp...)
		providerSpelling = summary.ProviderCode
		summaries = append(summaries, summary)
	}
	if err := rows.Err(); err != nil {
		return "", nil, err
	}
	if len(summaries) == 0 {
		return "", nil, ErrNotFound
	}
	return providerSpelling, summaries, nil
}

func (p *Postgres) GetSummary(ctx context.Context, provider, dataset domain.Code) (domain.Summary, error) {
	return scanSummary(p.pool.QueryRow(ctx, summarySQL, provider.Key, dataset.Key))
}

func (p *Postgres) GetStructure(ctx context.Context, provider, dataset domain.Code) (domain.View, error) {
	tx, err := p.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		return domain.View{}, err
	}
	defer tx.Rollback(context.Background())
	summary, datasetID, err := getSummaryAndID(ctx, tx, provider.Key, dataset.Key)
	if err != nil {
		return domain.View{}, err
	}
	dimensions, err := loadStructure(ctx, tx, datasetID)
	if err != nil {
		return domain.View{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return domain.View{}, err
	}
	return domain.View{Summary: summary, Dimensions: dimensions}, nil
}

func (p *Postgres) GetData(ctx context.Context, provider, dataset domain.Code) (domain.View, error) {
	tx, err := p.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		return domain.View{}, err
	}
	defer tx.Rollback(context.Background())
	summary, datasetID, err := getSummaryAndID(ctx, tx, provider.Key, dataset.Key)
	if err != nil {
		return domain.View{}, err
	}
	dimensions, err := loadStructure(ctx, tx, datasetID)
	if err != nil {
		return domain.View{}, err
	}
	cells, err := loadAllCells(ctx, tx, datasetID)
	if err != nil {
		return domain.View{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return domain.View{}, err
	}
	return domain.View{Summary: summary, Dimensions: dimensions, Cells: cells}, nil
}

func (p *Postgres) Query(ctx context.Context, provider, dataset domain.Code, selection domain.Selection, maxCells int64) (domain.View, error) {
	tx, err := p.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		return domain.View{}, err
	}
	defer tx.Rollback(context.Background())
	summary, datasetID, err := getSummaryAndID(ctx, tx, provider.Key, dataset.Key)
	if err != nil {
		return domain.View{}, err
	}
	storedDimensions, err := loadStructure(ctx, tx, datasetID)
	if err != nil {
		return domain.View{}, err
	}
	outputDimensions, internalIndices, err := domain.ResolveSelection(selection, storedDimensions, maxCells)
	if err != nil {
		var limitError domain.CellLimitError
		if errors.As(err, &limitError) {
			return domain.View{}, err
		}
		return domain.View{}, fmt.Errorf("%w: %v", ErrInvalidSelection, err)
	}
	storedCells := make(map[int64]domain.Cell)
	for start := 0; start < len(internalIndices); start += queryBatchSize {
		end := min(start+queryBatchSize, len(internalIndices))
		rows, err := tx.Query(ctx, `
			select cell_index, numeric_value, text_value, status_code
			from storage.observations
			where dataset_id = $1 and cell_index = any($2::bigint[])
		`, datasetID, internalIndices[start:end])
		if err != nil {
			return domain.View{}, err
		}
		for rows.Next() {
			cell, err := scanCell(rows)
			if err != nil {
				rows.Close()
				return domain.View{}, err
			}
			storedCells[cell.Index] = cell
		}
		if err := rows.Err(); err != nil {
			rows.Close()
			return domain.View{}, err
		}
		rows.Close()
	}
	outputCells := make([]domain.Cell, 0, len(storedCells))
	for outputIndex, internalIndex := range internalIndices {
		if cell, exists := storedCells[internalIndex]; exists {
			cell.Index = int64(outputIndex)
			outputCells = append(outputCells, cell)
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return domain.View{}, err
	}
	return domain.View{Summary: summary, Dimensions: outputDimensions, Cells: outputCells}, nil
}

func (p *Postgres) Replace(ctx context.Context, replacement domain.Replacement) (string, domain.Summary, error) {
	tx, err := p.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	if err != nil {
		return "", domain.Summary{}, err
	}
	defer tx.Rollback(context.Background())
	if _, err := tx.Exec(ctx, `select pg_advisory_xact_lock($1)`, advisoryKey(replacement.Provider.Key, replacement.Dataset.Key)); err != nil {
		return "", domain.Summary{}, err
	}
	if _, err := tx.Exec(ctx, `
		insert into storage.providers(provider_code, provider_key)
		values ($1, $2)
		on conflict (provider_key) do nothing
	`, replacement.Provider.Spelling, replacement.Provider.Key); err != nil {
		return "", domain.Summary{}, err
	}
	var providerID int64
	var providerSpelling string
	if err := tx.QueryRow(ctx, `
		select provider_id, provider_code from storage.providers where provider_key = $1
	`, replacement.Provider.Key).Scan(&providerID, &providerSpelling); err != nil {
		return "", domain.Summary{}, err
	}

	var datasetID int64
	var datasetSpelling string
	err = tx.QueryRow(ctx, `
		select dataset_id, dataset_code
		from storage.datasets
		where provider_id = $1 and dataset_key = $2
		for update
	`, providerID, replacement.Dataset.Key).Scan(&datasetID, &datasetSpelling)
	existed := err == nil
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return "", domain.Summary{}, err
	}
	if existed && !replacement.Replace {
		return "", domain.Summary{}, ErrDatasetExists
	}
	result := "replaced"
	if !existed {
		result = "created"
		datasetSpelling = replacement.Dataset.Spelling
		if err := tx.QueryRow(ctx, `
			insert into storage.datasets(
				provider_id, dataset_code, dataset_key, source_stamp,
				cell_count, valued_cell_count, updated_at
			) values ($1, $2, $3, $4::jsonb, $5, $6, clock_timestamp())
			returning dataset_id
		`, providerID, datasetSpelling, replacement.Dataset.Key, string(replacement.SourceStamp),
			replacement.CellCount, replacement.ValuedCount).Scan(&datasetID); err != nil {
			return "", domain.Summary{}, err
		}
	} else {
		if _, err := tx.Exec(ctx, `delete from storage.observations where dataset_id = $1`, datasetID); err != nil {
			return "", domain.Summary{}, err
		}
		if _, err := tx.Exec(ctx, `delete from storage.dimensions where dataset_id = $1`, datasetID); err != nil {
			return "", domain.Summary{}, err
		}
	}
	if err := p.checkpoint(ctx, func(c *transactionCheckpoints) func(context.Context) error { return c.BeforeStructure }); err != nil {
		return "", domain.Summary{}, err
	}
	for _, dimension := range replacement.Dimensions {
		var dimensionID int64
		if err := tx.QueryRow(ctx, `
			insert into storage.dimensions(dataset_id, dimension_code, dimension_key, position)
			values ($1, $2, $3, $4)
			returning dimension_id
		`, datasetID, dimension.Code.Spelling, dimension.Code.Key, dimension.Position).Scan(&dimensionID); err != nil {
			return "", domain.Summary{}, err
		}
		for _, category := range dimension.Categories {
			if _, err := tx.Exec(ctx, `
				insert into storage.categories(dimension_id, category_code, category_key, position)
				values ($1, $2, $3, $4)
			`, dimensionID, category.Code.Spelling, category.Code.Key, category.Position); err != nil {
				return "", domain.Summary{}, err
			}
		}
	}
	if err := p.checkpoint(ctx, func(c *transactionCheckpoints) func(context.Context) error { return c.BeforeCopy }); err != nil {
		return "", domain.Summary{}, err
	}
	if len(replacement.Cells) > 0 {
		_, err = tx.CopyFrom(ctx, pgx.Identifier{"storage", "observations"},
			[]string{"dataset_id", "cell_index", "numeric_value", "text_value", "status_code"},
			pgx.CopyFromSlice(len(replacement.Cells), func(i int) ([]any, error) {
				cell := replacement.Cells[i]
				return []any{datasetID, cell.Index, cell.Numeric, cell.Text, cell.Status}, nil
			}))
		if err != nil {
			return "", domain.Summary{}, err
		}
	}
	var summary domain.Summary
	var stamp []byte
	if err := tx.QueryRow(ctx, `
		update storage.datasets
		set source_stamp = $2::jsonb,
		    cell_count = $3,
		    valued_cell_count = $4,
		    updated_at = clock_timestamp()
		where dataset_id = $1
		returning dataset_code, source_stamp, cell_count, valued_cell_count, null_cell_count, updated_at
	`, datasetID, string(replacement.SourceStamp), replacement.CellCount, replacement.ValuedCount).Scan(
		&summary.DatasetCode, &stamp, &summary.CellCount, &summary.ValuedCellCount,
		&summary.NullCellCount, &summary.UpdatedAt,
	); err != nil {
		return "", domain.Summary{}, err
	}
	summary.ProviderCode = providerSpelling
	summary.SourceStamp = append(json.RawMessage(nil), stamp...)
	if err := p.checkpoint(ctx, func(c *transactionCheckpoints) func(context.Context) error { return c.BeforeCommit }); err != nil {
		return "", domain.Summary{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return "", domain.Summary{}, err
	}
	return result, summary, nil
}

func (p *Postgres) Delete(ctx context.Context, provider, dataset domain.Code) error {
	tx, err := p.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	if err != nil {
		return err
	}
	defer tx.Rollback(context.Background())
	if _, err := tx.Exec(ctx, `select pg_advisory_xact_lock($1)`, advisoryKey(provider.Key, dataset.Key)); err != nil {
		return err
	}
	_, err = tx.Exec(ctx, `
		delete from storage.datasets d
		using storage.providers p
		where d.provider_id = p.provider_id
		  and p.provider_key = $1
		  and d.dataset_key = $2
	`, provider.Key, dataset.Key)
	if err != nil {
		return err
	}
	return tx.Commit(ctx)
}

const summarySQL = `
	select p.provider_code, d.dataset_code, d.source_stamp, d.cell_count,
	       d.valued_cell_count, d.null_cell_count, d.updated_at
	from storage.providers p
	join storage.datasets d on d.provider_id = p.provider_id
	where p.provider_key = $1 and d.dataset_key = $2
`

func scanSummary(row pgx.Row) (domain.Summary, error) {
	var summary domain.Summary
	var stamp []byte
	err := row.Scan(&summary.ProviderCode, &summary.DatasetCode, &stamp, &summary.CellCount,
		&summary.ValuedCellCount, &summary.NullCellCount, &summary.UpdatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return domain.Summary{}, ErrNotFound
	}
	if err != nil {
		return domain.Summary{}, err
	}
	summary.SourceStamp = append(json.RawMessage(nil), stamp...)
	return summary, nil
}

func getSummaryAndID(ctx context.Context, tx pgx.Tx, providerKey, datasetKey string) (domain.Summary, int64, error) {
	var summary domain.Summary
	var datasetID int64
	var stamp []byte
	err := tx.QueryRow(ctx, `
		select p.provider_code, d.dataset_code, d.source_stamp, d.cell_count,
		       d.valued_cell_count, d.null_cell_count, d.updated_at, d.dataset_id
		from storage.providers p
		join storage.datasets d on d.provider_id = p.provider_id
		where p.provider_key = $1 and d.dataset_key = $2
	`, providerKey, datasetKey).Scan(&summary.ProviderCode, &summary.DatasetCode, &stamp,
		&summary.CellCount, &summary.ValuedCellCount, &summary.NullCellCount, &summary.UpdatedAt, &datasetID)
	if errors.Is(err, pgx.ErrNoRows) {
		return domain.Summary{}, 0, ErrNotFound
	}
	if err != nil {
		return domain.Summary{}, 0, err
	}
	summary.SourceStamp = append(json.RawMessage(nil), stamp...)
	return summary, datasetID, nil
}

func loadStructure(ctx context.Context, tx pgx.Tx, datasetID int64) ([]domain.Dimension, error) {
	rows, err := tx.Query(ctx, `
		select dimension_id, dimension_code, dimension_key, position
		from storage.dimensions
		where dataset_id = $1
		order by position
	`, datasetID)
	if err != nil {
		return nil, err
	}
	dimensions := make([]domain.Dimension, 0)
	dimensionIndexes := make(map[int64]int)
	for rows.Next() {
		var id int64
		var dimension domain.Dimension
		if err := rows.Scan(&id, &dimension.Code.Spelling, &dimension.Code.Key, &dimension.Position); err != nil {
			rows.Close()
			return nil, err
		}
		dimensionIndexes[id] = len(dimensions)
		dimensions = append(dimensions, dimension)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return nil, err
	}
	rows.Close()

	rows, err = tx.Query(ctx, `
		select c.dimension_id, c.category_code, c.category_key, c.position
		from storage.categories c
		join storage.dimensions d on d.dimension_id = c.dimension_id
		where d.dataset_id = $1
		order by d.position, c.position
	`, datasetID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var dimensionID int64
		var category domain.Category
		if err := rows.Scan(&dimensionID, &category.Code.Spelling, &category.Code.Key, &category.Position); err != nil {
			return nil, err
		}
		index, exists := dimensionIndexes[dimensionID]
		if !exists {
			return nil, fmt.Errorf("category references an unloaded dimension")
		}
		dimensions[index].Categories = append(dimensions[index].Categories, category)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return dimensions, nil
}

func loadAllCells(ctx context.Context, tx pgx.Tx, datasetID int64) ([]domain.Cell, error) {
	rows, err := tx.Query(ctx, `
		select cell_index, numeric_value, text_value, status_code
		from storage.observations
		where dataset_id = $1
		order by cell_index
	`, datasetID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	cells := make([]domain.Cell, 0)
	for rows.Next() {
		cell, err := scanCell(rows)
		if err != nil {
			return nil, err
		}
		cells = append(cells, cell)
	}
	return cells, rows.Err()
}

type rowScanner interface {
	Scan(...any) error
}

func scanCell(row rowScanner) (domain.Cell, error) {
	var cell domain.Cell
	var numeric pgtype.Float8
	var textValue pgtype.Text
	var status pgtype.Text
	if err := row.Scan(&cell.Index, &numeric, &textValue, &status); err != nil {
		return domain.Cell{}, err
	}
	if numeric.Valid {
		value := numeric.Float64
		cell.Numeric = &value
	}
	if textValue.Valid {
		value := textValue.String
		cell.Text = &value
	}
	if status.Valid {
		value := status.String
		cell.Status = &value
	}
	return cell, nil
}

func advisoryKey(providerKey, datasetKey string) int64 {
	buffer := make([]byte, 0, 16+len(providerKey)+len(datasetKey))
	var length [8]byte
	binary.BigEndian.PutUint64(length[:], uint64(len(providerKey)))
	buffer = append(buffer, length[:]...)
	buffer = append(buffer, providerKey...)
	binary.BigEndian.PutUint64(length[:], uint64(len(datasetKey)))
	buffer = append(buffer, length[:]...)
	buffer = append(buffer, datasetKey...)
	digest := sha256.Sum256(buffer)
	return int64(binary.BigEndian.Uint64(digest[:8]))
}

func (p *Postgres) checkpoint(ctx context.Context, selectCheckpoint func(*transactionCheckpoints) func(context.Context) error) error {
	if p.checkpoints == nil {
		return nil
	}
	checkpoint := selectCheckpoint(p.checkpoints)
	if checkpoint == nil {
		return nil
	}
	return checkpoint(ctx)
}
