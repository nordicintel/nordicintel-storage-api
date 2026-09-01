# Architecture

PostgreSQL is the only durable component. The API owns all cross-row invariants;
direct database writes are unsupported. The authoritative schema is the
[embedded migration](../internal/migrations/sql/001_initial.sql).

## Identity and structure

Provider, dataset, dimension, and category codes are matched using a bounded,
idempotent normalization: trim Unicode whitespace, apply NFKC, apply Unicode
case folding, apply NFKC again, and trim again until stable. Normalized keys are
stored and compared bytewise; submitted spellings are retained for responses.

Dimensions and categories are stored in normalized-key order. Those internal
positions support deterministic encoding and are not business presentation
order. Changing the normalization algorithm or Unicode data requires a migration
that rebuilds keys and checks for new collisions.

## Observations

Coordinates use checked row-major indexing, with the last dimension varying
fastest. Request indexes are payload-local and remapped to stored positions.

Only cells containing a numeric value, text value, or status receive physical
rows. Absent rows are inferred nulls. Numeric and text values are mutually
exclusive; status is independent. Numeric values use finite `float64` semantics,
and source stamps are opaque `jsonb` values.

Observations are hash-partitioned by dataset into 32 partitions. All rows for a
dataset share one partition, enabling partition-pruned reads and deletes.

## Consistency

Creation, replacement, and deletion for one normalized dataset identity share a
transaction-scoped advisory lock. A replacement validates and remaps the full
payload before destructive work, then replaces structure and observations in one
transaction. PostgreSQL MVCC keeps the previous committed state visible until
the new state commits.

Multi-statement reads use a read-only repeatable-read transaction, so metadata,
structure, and observations come from one snapshot. Concurrent readers therefore
observe either the complete old dataset or the complete new dataset, never a mix.

The service stores only current state: there are no historical versions,
coordinate patches, background ingestion jobs, aggregations, or search queries.
