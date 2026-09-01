package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"math/big"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/nordicintel/nordicintel-storage-api/internal/domain"
)

type Postgres struct {
	pool     *pgxpool.Pool
	maxCells int64
}

func Open(ctx context.Context, databaseURL string, maxConns int32, maxCells int64) (*Postgres, error) {
	cfg, err := pgxpool.ParseConfig(databaseURL)
	if err != nil {
		return nil, fmt.Errorf("parse DATABASE_URL: %w", err)
	}
	cfg.MaxConns = maxConns
	cfg.ConnConfig.RuntimeParams["application_name"] = "nordicintel-storage-api"
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return nil, fmt.Errorf("create database pool: %w", err)
	}
	db := &Postgres{pool: pool, maxCells: maxCells}
	if err := db.Ping(ctx); err != nil {
		pool.Close()
		return nil, err
	}
	var encoding string
	var version int
	if err := pool.QueryRow(ctx, "select current_setting('server_encoding'), current_setting('server_version_num')::int").Scan(&encoding, &version); err != nil {
		pool.Close()
		return nil, fmt.Errorf("inspect PostgreSQL: %w", err)
	}
	if encoding != "UTF8" {
		pool.Close()
		return nil, fmt.Errorf("PostgreSQL server_encoding must be UTF8, got %s", encoding)
	}
	if version < 150000 {
		pool.Close()
		return nil, fmt.Errorf("PostgreSQL 15 or newer is required, got server_version_num %d", version)
	}
	return db, nil
}

func (db *Postgres) Ping(ctx context.Context) error {
	if err := db.pool.Ping(ctx); err != nil {
		return fmt.Errorf("database ping: %w", err)
	}
	return nil
}

func (db *Postgres) Close() { db.pool.Close() }

func (db *Postgres) Providers(ctx context.Context) ([]domain.ProviderSummary, error) {
	rows, err := db.pool.Query(ctx, `
		select p.provider_code, count(d.dataset_id)::bigint
		from hub.providers p
		left join hub.datasets d on d.provider_code = p.provider_code and d.load_status = 'ready'
		group by p.provider_code, p.provider_key
		order by p.provider_key`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := make([]domain.ProviderSummary, 0)
	for rows.Next() {
		var item domain.ProviderSummary
		if err := rows.Scan(&item.ProviderCode, &item.DatasetCount); err != nil {
			return nil, err
		}
		result = append(result, item)
	}
	return result, rows.Err()
}

func (db *Postgres) Datasets(ctx context.Context, provider *string, full bool) ([]domain.DatasetSummary, error) {
	query := `
		select d.provider_code, d.dataset_code, d.observation_count,
		       d.observations_updated_at, d.source_stamp, c.structure
		from hub.datasets d
		join hub.providers p on p.provider_code = d.provider_code
		join hub.dataset_structure_cache c using (dataset_id)
		where d.load_status = 'ready'`
	args := []any{}
	if provider != nil {
		query += " and p.provider_key = hub.normalize_code($1)"
		args = append(args, *provider)
	}
	query += " order by p.provider_key, d.dataset_key"
	rows, err := db.pool.Query(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := make([]domain.DatasetSummary, 0)
	for rows.Next() {
		var item domain.DatasetSummary
		var stamp, structure []byte
		if err := rows.Scan(&item.ProviderCode, &item.DatasetCode, &item.ObservationCount, &item.ObservationsUpdatedAt, &stamp, &structure); err != nil {
			return nil, err
		}
		if full {
			item.SourceStamp = nullJSON(stamp)
			if err := json.Unmarshal(structure, &item.Dimensions); err != nil {
				return nil, fmt.Errorf("decode cached structure: %w", err)
			}
		}
		result = append(result, item)
	}
	return result, rows.Err()
}

func (db *Postgres) Metadata(ctx context.Context, provider, dataset string) (domain.DatasetMetadata, error) {
	return metadataQuery(ctx, db.pool, provider, dataset)
}

type querier interface {
	QueryRow(context.Context, string, ...any) pgx.Row
}

func metadataQuery(ctx context.Context, q querier, provider, dataset string) (domain.DatasetMetadata, error) {
	var result domain.DatasetMetadata
	var stamp, structure []byte
	err := q.QueryRow(ctx, `
		select d.provider_code, d.dataset_code, d.observation_count,
		       d.observations_updated_at, d.source_stamp, c.structure
		from hub.datasets d
		join hub.providers p on p.provider_code = d.provider_code
		join hub.dataset_structure_cache c using (dataset_id)
		where p.provider_key = hub.normalize_code($1)
		  and d.dataset_key = hub.normalize_code($2)
		  and d.load_status = 'ready'`, provider, dataset).Scan(
		&result.ProviderCode, &result.DatasetCode, &result.ObservationCount,
		&result.ObservationsUpdatedAt, &stamp, &structure,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return domain.DatasetMetadata{}, ErrNotFound
	}
	if err != nil {
		return domain.DatasetMetadata{}, err
	}
	result.SourceStamp = nullJSON(stamp)
	if err := json.Unmarshal(structure, &result.Dimensions); err != nil {
		return domain.DatasetMetadata{}, fmt.Errorf("decode cached structure: %w", err)
	}
	return result, nil
}

func (db *Postgres) Replace(ctx context.Context, input ReplaceInput) (ReplaceResult, error) {
	tx, err := db.pool.Begin(ctx)
	if err != nil {
		return ReplaceResult{}, err
	}
	defer tx.Rollback(ctx)

	var canonicalProvider string
	err = tx.QueryRow(ctx, `
		insert into hub.providers (provider_code) values ($1)
		on conflict (provider_key) do update set provider_code = hub.providers.provider_code
		returning provider_code`, input.ProviderCode).Scan(&canonicalProvider)
	if err != nil {
		return ReplaceResult{}, err
	}

	var datasetID int64
	created := true
	err = tx.QueryRow(ctx, `
		insert into hub.datasets (provider_code, dataset_code)
		values ($1, $2)
		on conflict (provider_code, dataset_key) do nothing
		returning dataset_id`, canonicalProvider, input.DatasetCode).Scan(&datasetID)
	if errors.Is(err, pgx.ErrNoRows) {
		created = false
		err = tx.QueryRow(ctx, `select dataset_id from hub.datasets
			where provider_code = $1 and dataset_key = hub.normalize_code($2)`, canonicalProvider, input.DatasetCode).Scan(&datasetID)
	}
	if err != nil {
		return ReplaceResult{}, err
	}
	if err := lockDataset(ctx, tx, datasetID); err != nil {
		return ReplaceResult{}, err
	}
	if _, err := tx.Exec(ctx, "update hub.datasets set load_status = 'loading' where dataset_id = $1", datasetID); err != nil {
		return ReplaceResult{}, err
	}
	if _, err := tx.Exec(ctx, "delete from hub.observations where dataset_id = $1", datasetID); err != nil {
		return ReplaceResult{}, err
	}
	if _, err := tx.Exec(ctx, "delete from hub.dimensions where dataset_id = $1", datasetID); err != nil {
		return ReplaceResult{}, err
	}

	for position, dimension := range input.Dimensions {
		var dimensionID int64
		err := tx.QueryRow(ctx, `insert into hub.dimensions (dataset_id, code, position, size)
			values ($1, $2, $3, $4) returning dimension_id`, datasetID, dimension.Code, position, len(dimension.Categories)).Scan(&dimensionID)
		if err != nil {
			return ReplaceResult{}, err
		}
		categoryRows := make([][]any, len(dimension.Categories))
		for categoryPosition, category := range dimension.Categories {
			categoryRows[categoryPosition] = []any{dimensionID, category, categoryPosition}
		}
		if _, err := tx.CopyFrom(ctx, pgx.Identifier{"hub", "categories"}, []string{"dimension_id", "code", "position"}, pgx.CopyFromRows(categoryRows)); err != nil {
			return ReplaceResult{}, err
		}
	}

	if len(input.Cells) > 0 {
		_, err = tx.CopyFrom(ctx, pgx.Identifier{"hub", "observations"}, []string{"dataset_id", "cell_index", "value", "status_code"}, pgx.CopyFromSlice(len(input.Cells), func(i int) ([]any, error) {
			cell := input.Cells[i]
			var numeric any
			if cell.Value != nil {
				parsed, err := parsePGNumeric(string(*cell.Value))
				if err != nil {
					return nil, Invalid("values[%d] cannot be represented as PostgreSQL numeric: %v", cell.Index, err)
				}
				numeric = parsed
			}
			return []any{datasetID, cell.Index, numeric, cell.StatusCode}, nil
		}))
		if err != nil {
			return ReplaceResult{}, err
		}
	}

	structure, err := json.Marshal(input.Dimensions)
	if err != nil {
		return ReplaceResult{}, err
	}
	if _, err := tx.Exec(ctx, `insert into hub.dataset_structure_cache (dataset_id, structure)
		values ($1, $2) on conflict (dataset_id) do update set structure = excluded.structure`, datasetID, json.RawMessage(structure)); err != nil {
		return ReplaceResult{}, err
	}
	if len(input.SourceStamp) > 0 {
		if _, err := tx.Exec(ctx, "update hub.datasets set source_stamp = $2 where dataset_id = $1", datasetID, input.SourceStamp); err != nil {
			return ReplaceResult{}, err
		}
	}
	var updatedAt time.Time
	err = tx.QueryRow(ctx, `update hub.datasets
		set observation_count = $2, observations_updated_at = clock_timestamp(), load_status = 'ready'
		where dataset_id = $1 returning observations_updated_at`, datasetID, len(input.Cells)).Scan(&updatedAt)
	if err != nil {
		return ReplaceResult{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return ReplaceResult{}, err
	}
	return ReplaceResult{Created: created, ObservationCount: int64(len(input.Cells)), ObservationsUpdatedAt: &updatedAt}, nil
}

func lockDataset(ctx context.Context, tx pgx.Tx, datasetID int64) error {
	if _, err := tx.Exec(ctx, "select pg_advisory_xact_lock($1)", datasetID); err != nil {
		return err
	}
	var found int64
	if err := tx.QueryRow(ctx, "select dataset_id from hub.datasets where dataset_id = $1 for update", datasetID).Scan(&found); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrNotFound
		}
		return err
	}
	return nil
}

func (db *Postgres) resolveReadyForWrite(ctx context.Context, tx pgx.Tx, provider, dataset string) (int64, []domain.Dimension, *time.Time, error) {
	var datasetID int64
	err := tx.QueryRow(ctx, `select d.dataset_id
		from hub.datasets d join hub.providers p using (provider_code)
		where p.provider_key = hub.normalize_code($1) and d.dataset_key = hub.normalize_code($2)
		  and d.load_status = 'ready'`, provider, dataset).Scan(&datasetID)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, nil, nil, ErrNotFound
	}
	if err != nil {
		return 0, nil, nil, err
	}
	if err := lockDataset(ctx, tx, datasetID); err != nil {
		return 0, nil, nil, err
	}
	var structure []byte
	var updatedAt *time.Time
	err = tx.QueryRow(ctx, `select c.structure, d.observations_updated_at
		from hub.datasets d join hub.dataset_structure_cache c using (dataset_id)
		where d.dataset_id = $1 and d.load_status = 'ready'`, datasetID).Scan(&structure, &updatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, nil, nil, ErrNotFound
	}
	if err != nil {
		return 0, nil, nil, err
	}
	var dimensions []domain.Dimension
	if err := json.Unmarshal(structure, &dimensions); err != nil {
		return 0, nil, nil, err
	}
	return datasetID, dimensions, updatedAt, nil
}

func (db *Postgres) Patch(ctx context.Context, provider, dataset string, request domain.PatchRequest) (PatchResult, error) {
	tx, err := db.pool.Begin(ctx)
	if err != nil {
		return PatchResult{}, err
	}
	defer tx.Rollback(ctx)
	datasetID, dimensions, previousUpdatedAt, err := db.resolveReadyForWrite(ctx, tx, provider, dataset)
	if err != nil {
		return PatchResult{}, err
	}
	changes, err := preparePatch(dimensions, request.Observations)
	if err != nil {
		return PatchResult{}, err
	}
	result := PatchResult{ObservationsUpdatedAt: previousUpdatedAt}
	for _, change := range changes {
		var same bool
		newValue := decimalString(change.Value)
		err := tx.QueryRow(ctx, `select value is not distinct from $3::numeric
			and status_code is not distinct from $4
			from hub.observations where dataset_id = $1 and cell_index = $2`,
			datasetID, change.Index, newValue, change.StatusCode).Scan(&same)
		exists := err == nil
		if err != nil && !errors.Is(err, pgx.ErrNoRows) {
			return PatchResult{}, err
		}
		if change.Delete {
			if exists {
				if _, err := tx.Exec(ctx, "delete from hub.observations where dataset_id = $1 and cell_index = $2", datasetID, change.Index); err != nil {
					return PatchResult{}, err
				}
				result.DeletedCount++
			}
			continue
		}
		if !exists {
			if _, err := tx.Exec(ctx, `insert into hub.observations (dataset_id, cell_index, value, status_code)
				values ($1, $2, $3::numeric, $4)`, datasetID, change.Index, newValue, change.StatusCode); err != nil {
				return PatchResult{}, err
			}
			result.InsertedCount++
		} else if !same {
			if _, err := tx.Exec(ctx, `update hub.observations set value = $3::numeric, status_code = $4
				where dataset_id = $1 and cell_index = $2`, datasetID, change.Index, newValue, change.StatusCode); err != nil {
				return PatchResult{}, err
			}
			result.UpdatedCount++
		}
	}
	if len(request.SourceStamp) > 0 {
		if _, err := tx.Exec(ctx, "update hub.datasets set source_stamp = $2 where dataset_id = $1", datasetID, request.SourceStamp); err != nil {
			return PatchResult{}, err
		}
	}
	changed := result.InsertedCount + result.UpdatedCount + result.DeletedCount
	if changed > 0 {
		var updated time.Time
		err = tx.QueryRow(ctx, `update hub.datasets set
			observation_count = observation_count + $2 - $3,
			observations_updated_at = clock_timestamp()
			where dataset_id = $1 returning observation_count, observations_updated_at`,
			datasetID, result.InsertedCount, result.DeletedCount).Scan(&result.ObservationCount, &updated)
		result.ObservationsUpdatedAt = &updated
	} else {
		err = tx.QueryRow(ctx, "select observation_count from hub.datasets where dataset_id = $1", datasetID).Scan(&result.ObservationCount)
	}
	if err != nil {
		return PatchResult{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return PatchResult{}, err
	}
	return result, nil
}

func preparePatch(dimensions []domain.Dimension, observations []domain.PatchObservation) ([]domain.PatchCell, error) {
	if len(observations) == 0 {
		return nil, Invalid("observations must be a non-empty array")
	}
	result := make([]domain.PatchCell, 0, len(observations))
	seen := make(map[int64]struct{}, len(observations))
	for i, observation := range observations {
		index, err := coordinateIndex(dimensions, observation.Categories)
		if err != nil {
			return nil, Invalid("observations[%d]: %v", i, err)
		}
		if _, exists := seen[index]; exists {
			return nil, Invalid("observations[%d] duplicates another coordinate", i)
		}
		seen[index] = struct{}{}
		deleting := string(observation.Delete) == "true"
		if len(observation.Delete) > 0 && !deleting {
			return nil, Invalid("observations[%d].delete must be true when supplied", i)
		}
		if deleting && (len(observation.Value) > 0 || len(observation.StatusCode) > 0) {
			return nil, Invalid("observations[%d]: delete items cannot include value or status_code", i)
		}
		value, err := domain.ParseDecimal(observation.Value)
		if len(observation.Value) == 0 {
			value = nil
			err = nil
		}
		if err != nil {
			return nil, Invalid("observations[%d].value %v", i, err)
		}
		status, err := domain.ParseNullableString(observation.StatusCode)
		if err != nil {
			return nil, Invalid("observations[%d].status_code %v", i, err)
		}
		if !deleting && value == nil && status == nil {
			return nil, Invalid("observations[%d] requires a non-null value or status_code", i)
		}
		result = append(result, domain.PatchCell{Index: index, Value: value, StatusCode: status, Delete: deleting})
	}
	return result, nil
}

func coordinateIndex(dimensions []domain.Dimension, coordinate domain.Coordinate) (int64, error) {
	if len(coordinate) != len(dimensions) {
		return 0, errors.New("categories must contain every dimension exactly once")
	}
	normalized := make(map[string]string, len(coordinate))
	for code, category := range coordinate {
		key := domain.NormalizeCode(code)
		if _, exists := normalized[key]; exists {
			return 0, errors.New("categories contains duplicate normalized dimension codes")
		}
		normalized[key] = category
	}
	var index int64
	for _, dimension := range dimensions {
		category, exists := normalized[domain.NormalizeCode(dimension.Code)]
		if !exists {
			return 0, fmt.Errorf("missing dimension %q", dimension.Code)
		}
		categoryKey := domain.NormalizeCode(category)
		position := -1
		for i, candidate := range dimension.Categories {
			if domain.NormalizeCode(candidate) == categoryKey {
				position = i
				break
			}
		}
		if position < 0 {
			return 0, fmt.Errorf("unknown category %q for dimension %q", category, dimension.Code)
		}
		index = index*int64(len(dimension.Categories)) + int64(position)
	}
	return index, nil
}

func (db *Postgres) Delete(ctx context.Context, provider, dataset string) error {
	tx, err := db.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	var datasetID int64
	var canonicalProvider string
	err = tx.QueryRow(ctx, `select d.dataset_id from hub.datasets d join hub.providers p using (provider_code)
		where p.provider_key = hub.normalize_code($1) and d.dataset_key = hub.normalize_code($2)
		and d.load_status = 'ready'`, provider, dataset).Scan(&datasetID)
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrNotFound
	}
	if err != nil {
		return err
	}
	if err := lockDataset(ctx, tx, datasetID); err != nil {
		return err
	}
	if err := tx.QueryRow(ctx, "select provider_code from hub.datasets where dataset_id = $1", datasetID).Scan(&canonicalProvider); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, "delete from hub.datasets where dataset_id = $1", datasetID); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `delete from hub.providers p where p.provider_code = $1
		and not exists (select 1 from hub.datasets d where d.provider_code = p.provider_code)`, canonicalProvider); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func (db *Postgres) FullData(ctx context.Context, provider, dataset string) (DataResult, error) {
	tx, err := db.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		return DataResult{}, err
	}
	defer tx.Rollback(ctx)
	metadata, err := metadataQuery(ctx, tx, provider, dataset)
	if err != nil {
		return DataResult{}, err
	}
	count, err := cellProduct(metadata.Dimensions, db.maxCells)
	if err != nil {
		return DataResult{}, err
	}
	datasetID, err := resolveDatasetID(ctx, tx, provider, dataset)
	if err != nil {
		return DataResult{}, err
	}
	cells, err := readSparseCells(ctx, tx, datasetID)
	if err != nil {
		return DataResult{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return DataResult{}, err
	}
	return DataResult{ProviderCode: metadata.ProviderCode, DatasetCode: metadata.DatasetCode, Dimensions: metadata.Dimensions, Cells: cells, CellCount: count}, nil
}

func (db *Postgres) SelectedData(ctx context.Context, provider, dataset string, requested []domain.Dimension) (DataResult, error) {
	tx, err := db.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		return DataResult{}, err
	}
	defer tx.Rollback(ctx)
	metadata, err := metadataQuery(ctx, tx, provider, dataset)
	if err != nil {
		return DataResult{}, err
	}
	canonical, indexes, err := selectionIndexes(metadata.Dimensions, requested, db.maxCells)
	if err != nil {
		return DataResult{}, err
	}
	datasetID, err := resolveDatasetID(ctx, tx, provider, dataset)
	if err != nil {
		return DataResult{}, err
	}
	rows, err := tx.Query(ctx, `select requested.ordinality, requested.cell_index, o.value::text, o.status_code
		from unnest($2::bigint[]) with ordinality requested(cell_index, ordinality)
		left join hub.observations o on o.dataset_id = $1 and o.cell_index = requested.cell_index
		order by requested.ordinality`, datasetID, indexes)
	if err != nil {
		return DataResult{}, err
	}
	defer rows.Close()
	cells := make([]domain.Cell, 0)
	for rows.Next() {
		var ordinality, index int64
		var value, status *string
		if err := rows.Scan(&ordinality, &index, &value, &status); err != nil {
			return DataResult{}, err
		}
		if value != nil || status != nil {
			cells = append(cells, domain.Cell{Index: ordinality - 1, Value: decimal(value), StatusCode: status})
		}
	}
	if err := rows.Err(); err != nil {
		return DataResult{}, err
	}
	rows.Close()
	if err := tx.Commit(ctx); err != nil {
		return DataResult{}, err
	}
	return DataResult{ProviderCode: metadata.ProviderCode, DatasetCode: metadata.DatasetCode, Dimensions: canonical, Cells: cells, CellCount: int64(len(indexes))}, nil
}

func (db *Postgres) resolveDatasetID(ctx context.Context, provider, dataset string) (int64, error) {
	return resolveDatasetID(ctx, db.pool, provider, dataset)
}

func resolveDatasetID(ctx context.Context, q querier, provider, dataset string) (int64, error) {
	var id int64
	err := q.QueryRow(ctx, `select d.dataset_id from hub.datasets d join hub.providers p using (provider_code)
		where p.provider_key = hub.normalize_code($1) and d.dataset_key = hub.normalize_code($2)
		and d.load_status = 'ready'`, provider, dataset).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, ErrNotFound
	}
	return id, err
}

type rowsQuerier interface {
	Query(context.Context, string, ...any) (pgx.Rows, error)
}

func readSparseCells(ctx context.Context, q rowsQuerier, datasetID int64) ([]domain.Cell, error) {
	rows, err := q.Query(ctx, `select cell_index, value::text, status_code from hub.observations
		where dataset_id = $1 order by cell_index`, datasetID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	cells := make([]domain.Cell, 0)
	for rows.Next() {
		var cell domain.Cell
		var value *string
		if err := rows.Scan(&cell.Index, &value, &cell.StatusCode); err != nil {
			return nil, err
		}
		cell.Value = decimal(value)
		cells = append(cells, cell)
	}
	return cells, rows.Err()
}

func selectionIndexes(stored, requested []domain.Dimension, maxCells int64) ([]domain.Dimension, []int64, error) {
	if len(requested) != len(stored) {
		return nil, nil, Invalid("dimensions must contain every dataset dimension exactly once")
	}
	type storedDimension struct {
		position   int
		categories map[string]int
	}
	storedByKey := make(map[string]storedDimension, len(stored))
	for position, dimension := range stored {
		categories := make(map[string]int, len(dimension.Categories))
		for i, category := range dimension.Categories {
			categories[domain.NormalizeCode(category)] = i
		}
		storedByKey[domain.NormalizeCode(dimension.Code)] = storedDimension{position: position, categories: categories}
	}
	canonical := make([]domain.Dimension, len(requested))
	requestedStoredPositions := make([]int, len(requested))
	requestedCategoryPositions := make([][]int, len(requested))
	seenDimensions := make(map[string]struct{}, len(requested))
	total := int64(1)
	for i, dimension := range requested {
		key := domain.NormalizeCode(dimension.Code)
		storedDimension, exists := storedByKey[key]
		if !exists {
			return nil, nil, Invalid("unknown dimension %q", dimension.Code)
		}
		if _, duplicate := seenDimensions[key]; duplicate {
			return nil, nil, Invalid("duplicate dimension %q", dimension.Code)
		}
		seenDimensions[key] = struct{}{}
		if len(dimension.Categories) == 0 {
			return nil, nil, Invalid("dimension %q must select at least one category", dimension.Code)
		}
		canonical[i].Code = stored[storedDimension.position].Code
		canonical[i].Categories = make([]string, len(dimension.Categories))
		requestedStoredPositions[i] = storedDimension.position
		requestedCategoryPositions[i] = make([]int, len(dimension.Categories))
		seenCategories := make(map[string]struct{}, len(dimension.Categories))
		for j, category := range dimension.Categories {
			categoryKey := domain.NormalizeCode(category)
			position, exists := storedDimension.categories[categoryKey]
			if !exists {
				return nil, nil, Invalid("unknown category %q for dimension %q", category, dimension.Code)
			}
			if _, duplicate := seenCategories[categoryKey]; duplicate {
				return nil, nil, Invalid("duplicate category %q for dimension %q", category, dimension.Code)
			}
			seenCategories[categoryKey] = struct{}{}
			requestedCategoryPositions[i][j] = position
			canonical[i].Categories[j] = stored[storedDimension.position].Categories[position]
		}
		if total > maxCells/int64(len(dimension.Categories)) {
			return nil, nil, Invalid("selection exceeds the %d cell limit", maxCells)
		}
		total *= int64(len(dimension.Categories))
	}
	indexes := make([]int64, total)
	storedPositions := make([]int, len(stored))
	for outputIndex := int64(0); outputIndex < total; outputIndex++ {
		remaining := outputIndex
		for requestedPosition := len(requested) - 1; requestedPosition >= 0; requestedPosition-- {
			choices := requestedCategoryPositions[requestedPosition]
			choice := remaining % int64(len(choices))
			remaining /= int64(len(choices))
			storedPositions[requestedStoredPositions[requestedPosition]] = choices[choice]
		}
		var cellIndex int64
		for i, dimension := range stored {
			cellIndex = cellIndex*int64(len(dimension.Categories)) + int64(storedPositions[i])
		}
		indexes[outputIndex] = cellIndex
	}
	return canonical, indexes, nil
}

func cellProduct(dimensions []domain.Dimension, maxCells int64) (int64, error) {
	return domain.ValidateDimensions(dimensions, maxCells)
}

func decimal(value *string) *domain.Decimal {
	if value == nil {
		return nil
	}
	d := domain.Decimal(*value)
	return &d
}

func decimalString(value *domain.Decimal) *string {
	if value == nil {
		return nil
	}
	s := string(*value)
	return &s
}

func parsePGNumeric(value string) (pgtype.Numeric, error) {
	mantissa, exponentText := value, ""
	if index := strings.IndexAny(value, "eE"); index >= 0 {
		mantissa, exponentText = value[:index], value[index+1:]
	}
	exponent := int64(0)
	if exponentText != "" {
		parsed, err := strconv.ParseInt(exponentText, 10, 32)
		if err != nil {
			return pgtype.Numeric{}, fmt.Errorf("invalid exponent")
		}
		exponent = parsed
	}
	fractionalDigits := 0
	if point := strings.IndexByte(mantissa, '.'); point >= 0 {
		fractionalDigits = len(mantissa) - point - 1
		mantissa = mantissa[:point] + mantissa[point+1:]
	}
	integer := new(big.Int)
	if _, ok := integer.SetString(mantissa, 10); !ok {
		return pgtype.Numeric{}, fmt.Errorf("invalid mantissa")
	}
	exponent -= int64(fractionalDigits)
	if exponent < math.MinInt32 || exponent > math.MaxInt32 {
		return pgtype.Numeric{}, fmt.Errorf("exponent is outside the supported range")
	}
	return pgtype.Numeric{Int: integer, Exp: int32(exponent), Valid: true}, nil
}

func nullJSON(value []byte) json.RawMessage {
	if len(value) == 0 {
		return json.RawMessage("null")
	}
	return json.RawMessage(value)
}
