# Storage Semantics

This document defines the storage invariants and transaction behavior behind the API contract. PostgreSQL is the only durable state owner, and direct writes outside the service are unsupported.

## Identity and Normalization

Codes are normalized in Go in this exact order:

1. Trim leading and trailing Unicode whitespace.
2. Apply Unicode NFKC normalization.
3. Apply Unicode default case folding.
4. Apply Unicode NFKC normalization again.

The service stores the result in explicit normalized-key columns. PostgreSQL compares and sorts those columns bytewise with `COLLATE "C"`; it does not independently recreate Unicode normalization. The Unicode normalization dependency and data version must remain pinned. Changing either the algorithm or Unicode data requires a migration that recalculates every stored key and checks for new collisions.

Provider and dataset spellings are immutable after successful creation. A replacement deletes and recreates dimensions and categories, so their stored spellings come from the latest successful request. Responses use stored spellings, while matching and deterministic ordering use normalized keys.

An empty normalized key is invalid. The following collisions are invalid:

- Provider keys globally.
- Dataset keys within a provider.
- Dimension keys within a dataset.
- Category keys within a dimension.

Submitted spellings and normalized keys are limited to 256 UTF-8 bytes. Replacement structures contain 1 through 64 dimensions and at least one category in every dimension.

Full structure and data responses sort dimensions by normalized key and categories by normalized key within each dimension. The service stores that ordering as zero-based internal positions. These positions exist only for deterministic encoding and storage efficiency; they are not business presentation order.

## Coordinates and Values

For dimensions with sizes `s[0]` through `s[n-1]` and category positions `p[0]` through `p[n-1]`, the internal row-major index is:

```text
index = p[0]×(s[1]×…×s[n-1]) + p[1]×(s[2]×…×s[n-1]) + … + p[n-1]
```

The last dimension therefore varies fastest. Products and intermediate calculations use checked 64-bit arithmetic and must produce a logical cell count between 1 and 1,000,000.

POST indexes are payload-local. The service decodes them using the request's `id` and category index maps, normalizes each code, resolves it to the sorted internal positions, and remaps every populated payload index to an internal cell index. No request index is persisted as an external ordering guarantee.

Query indexes are output-local. The service enumerates the requested Cartesian product in the exact requested dimension/category index order, maps each coordinate to an internal index, and returns stored code spellings. Categories requested by the caller remain in the response structure even when every corresponding observation is inferred null.

The observations table stores only the union of indexes having at least one of:

- A numeric value.
- A text value.
- A status code.

A row may contain a numeric or text value, never both. Status is independent, including on a null-valued cell. A cell with no numeric value, text value, or status has no row and is inferred at response-encoding time.

`valued_cell_count` counts cells having a numeric or text value. Status-only rows do not increase it. `null_cell_count` is generated as `cell_count - valued_cell_count`; physical observation-row count is not exposed as dataset metadata.

Numeric values are Go `float64` values stored as PostgreSQL `double precision`. Binary floating-point rounding is accepted, and submitted lexical formatting is not preserved. Go validation and the database constraint both reject NaN and positive or negative infinity. PostgreSQL documents `double precision` as its inexact binary floating-point type and supports those special values, which is why the explicit rejection is required: [PostgreSQL numeric types](https://www.postgresql.org/docs/15/datatype-numeric.html).

Source stamps are stored as `jsonb`. The JSON literal `null` is a valid required stamp and is distinct from SQL NULL. JSON meaning is preserved, but whitespace, object-key order, and other lexically insignificant formatting are not.

## Partitioning

Observations are hash-partitioned by `dataset_id` into 32 static partitions. All rows for one dataset route to one partition, while independent datasets distribute across partitions. Dataset-qualified reads and deletes permit partition pruning. The primary key includes both `dataset_id` and `cell_index`, satisfying PostgreSQL's requirement that a partitioned unique or primary key contain the partition key: [PostgreSQL table partitioning](https://www.postgresql.org/docs/current/ddl-partitioning.html).

All observation writes target the partitioned parent table and let PostgreSQL route rows. The initial partition count is fixed at 32. Changing it requires benchmark evidence and a data migration; the application must not assume a particular remainder.

Dimensions and categories are not partitioned because their volume is bounded by dataset structure rather than populated cell count. Source stamps are opaque and receive no JSON index. No structure cache, loading state, trigger, or secondary observation index is maintained.

## Atomic Creation and Replacement

The service fully decodes, normalizes, remaps, and validates a replacement before destructive database work. Validation includes structure completeness, normalized collisions, checked cell-count calculation, dense/sparse channel consistency, index bounds, finite numerics, and numeric/text exclusivity.

Creation and replacement then use one read-committed transaction:

1. Encode each normalized UTF-8 key as an unsigned 64-bit big-endian byte length followed by its bytes, concatenate provider then dataset, hash with SHA-256, and interpret the first eight digest bytes as a signed big-endian `int64` lock key.
2. Acquire `pg_advisory_xact_lock` for that key.
3. Resolve or insert the provider using its normalized key.
4. Resolve and row-lock the dataset, or create it when absent.
5. If the dataset exists and `replace` is false, roll back and report `dataset_exists`.
6. For a replacement, retain the provider/dataset spellings, delete existing observations and dimensions, and let dimension deletion cascade to categories.
7. Insert dimensions and categories in normalized-key order.
8. Stream every non-empty observation through PostgreSQL `COPY` targeting the partitioned parent.
9. Set the required source stamp, counts, and `updated_at = clock_timestamp()` only after structure and observation insertion succeeds.
10. Commit and report creation or replacement according to whether the dataset existed after the lock was acquired.

For a new dataset, final metadata may be inserted with the row before its dependent rows because the entire transaction remains invisible until commit. Any validation, structure insertion, COPY, cancellation, or commit failure rolls back every change.

No loading flag is needed. PostgreSQL MVCC keeps uncommitted deletes and inserts invisible, so existing readers continue to see the previous committed state. Every successful replacement updates `updated_at`, even when the submitted logical state equals the previous state.

Create, replace, and delete operations for the same normalized identity use the same advisory lock. Advisory-hash collisions may serialize unrelated identities but cannot weaken correctness. Different datasets otherwise write concurrently; initial creation of two datasets under one new provider may briefly contend on the provider's unique key.

`replace: true` is unconditional. Concurrent requests take effect in lock/commit order, and the last committed replacement is current.

## Reads

Metadata and listing operations that use one statement rely on statement-level MVCC consistency. Structure, full-data, and query operations that require multiple statements run in one read-only repeatable-read transaction so metadata, structure, and observations come from one snapshot.

A full read:

1. Resolves the dataset and full metadata in the snapshot.
2. Loads dimensions and categories in stored internal order.
3. Reads the dataset's single pruned observation partition ordered by `cell_index`.
4. Encodes stored rows and inferred null cells in the requested dense or sparse form.

An exact query:

1. Resolves and validates every requested normalized dimension/category key against the snapshot structure.
2. Enumerates output coordinates in requested row-major order.
3. Maps output positions to internal cell indexes using the stored sorted positions.
4. Fetches matching observation rows in bounded batches.
5. Encodes results using output-local indexes while retaining every requested category in the response structure.

Batching is an implementation resource bound and cannot change response order or null inference. Full-dataset counts in a query response come from the dataset row in the same snapshot, not from the selected result.

A reader whose snapshot begins before a replacement commits sees the complete previous state. A reader whose snapshot begins afterward sees the complete new state. No reader may combine metadata or structure from one state with observations from another.

## Deletion

DELETE computes and acquires the same transaction-scoped advisory lock as creation/replacement, resolves the normalized identity, and deletes the dataset row. Foreign-key cascades remove its dimensions, categories, and observations. A missing dataset commits successfully without changing state.

Provider registry rows remain after deletion. Provider listings and provider-specific dataset lookup treat a provider as visible only when at least one current dataset references it, so a retained empty registry row is externally indistinguishable from an unknown provider.

## Service-Owned Invariants

The transaction layer, not SQL triggers, enforces:

- Stored normalized keys equal the specified Go normalization result.
- Dimension and category positions are contiguous and match normalized-key order.
- Dataset `cell_count` equals the product of current category counts.
- Every observation index is less than its dataset's current `cell_count`.
- Cached valued/null counts match current logical values.
- Every replacement updates structure, observations, stamp, counts, and timestamp together.

Database constraints remain a second line of defense for local row invariants and uniqueness. Direct database writes are unsupported because they can violate cross-row invariants.

## Verification

- Execute the target DDL continuously on empty PostgreSQL 18 and inspect all constraints, foreign keys, comments, and 32 attached partitions. PostgreSQL 15 through 17 compatibility is optional manual verification, not a release requirement.
- Prove that one dataset's observations route to one partition and that duplicate internal indexes fail.
- Test normalization collisions, deterministic sorting, POST-to-internal remapping, query permutations, inferred nulls, mutually exclusive values, status-only rows, and non-finite numeric rejection.
- Verify JSON-null source stamps remain distinct from SQL NULL and round-trip semantically.
- Force failures during structure insertion and COPY and confirm the previous state and metadata remain unchanged.
- Hold a replacement before commit and prove concurrent reads see the old state, followed by the new state after commit.
- Test concurrent create-only requests, unconditional replacements, replacement versus deletion, and independent dataset writes.
- Exercise sparse and fully populated one-million-cell datasets without imposing an initial latency threshold.

## Assumptions

- No legacy schema or data migration is required.
- PostgreSQL is the only durable-state component.
- The application is the only supported writer.
- The 32-partition count is changed only through a measured migration.
