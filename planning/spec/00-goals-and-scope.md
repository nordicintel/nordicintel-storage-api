# Goals and Scope

## Purpose

NordicIntel Storage API is the authoritative private store for the latest accepted state of observation datasets. Its clients are internal ingestion and backend services.

## V1 Goals

- Atomically replace, retrieve, and delete complete datasets.
- Retrieve complete datasets and exact subsets.
- List providers and all datasets belonging to a provider.
- Retrieve each dataset's dimensions and category-code membership.
- Include dataset summaries with the opaque source stamp, logical cell count, valued-cell count, null-cell count, and last successful replacement time.
- Support one million logical cells, including fully populated datasets.
- Ensure failed replacements never expose partial state.

## Data Boundary

- Store provider and dataset identity, dimensions, category codes, observations, and an opaque JSON source stamp.
- Each cell may contain either a `float64` numeric value or a text value, never both, plus an independent optional status.
- Binary floating-point rounding is acceptable.
- NaN and infinities must be converted to null upstream.
- Cells without a numeric value, text value, or status are inferred and need no physical row.
- Dimension and category business ordering is owned and reapplied by the upstream service; internal indexes or ordering exist only for storage efficiency.

## Access and Constraints

- Provide one read/write credential and one read-only credential.
- Keep Go as the implementation language and PostgreSQL as the required datastore.
- Keep the hosting platform open.
- Require a redesigned observation-partitioning strategy, to be specified in the schema and storage-semantics documents.
- Require synchronous correctness at the million-cell limit, but set no initial latency acceptance threshold.
- Require no compatibility with the current API, schema, or stored data.

## Non-Goals

- Historical dataset versions.
- Coordinate-level patching or append-only writes.
- Rich descriptive metadata, labels, or presentation ordering.
- Aggregation, wildcard, range, search, or analyst-oriented queries.
- Public access, per-client permissions, or provider-specific authorization.
- Background ingestion jobs.
- Migration of the current implementation's data.

## Success Criteria

V1 is successful when authenticated internal clients can replace and read a dataset without partial visibility, exact subsets reconstruct inferred null cells correctly, listings and counts describe the current accepted state, read-only credentials cannot mutate data, and fully populated million-cell datasets work correctly.
