# Test and Acceptance

## Required Pull-Request Checks

- Verify formatting, run `go vet`, run race-enabled unit and integration tests, and compile the release binaries.
- Run PostgreSQL 18 integration tests in a disposable official service container. GitHub Actions supports PostgreSQL service containers for this workflow: [GitHub service-container documentation](https://docs.github.com/en/actions/tutorials/use-containerized-services).
- Build the production API image on every pull request.
- Start the built image with disposable PostgreSQL, run migrations, wait for health, then smoke-test authentication, creation, querying, deletion, and graceful shutdown.
- Keep PostgreSQL 15 through 17 outside the continuous matrix. Compatibility checks may be run manually but are not release requirements.

## Unit and Contract Coverage

- Test normalization order, Unicode collisions, sorting, row-major indexing, overflow checks, and input-to-internal remapping.
- Test strict JSON parsing, duplicate keys, unknown fields, trailing values, source-stamp presence, dense/sparse validation, channel consistency, `float64` limits, and numeric/text conflicts.
- Test dense and sparse response encoding, shared status strings, inferred nulls, query permutations, and canonical stored spellings.
- Test authentication and authorization for both tokens, every status/error mapping, request IDs, content types, cache headers, method handling, and body limits.
- Fuzz normalization idempotence, sparse index parsing, row-major coordinate conversion, and JSON decoding without relying on a line-coverage threshold.

## PostgreSQL and Transaction Coverage

- Test the initial migration, repeated no-op migration, disposable down/up checks, failed-migration rollback, schema-version mismatch, all constraints, comments, and 32 partitions.
- Verify that one dataset routes to one partition, duplicate indexes fail, JSON null differs from SQL NULL, and generated counts remain correct.
- Test every endpoint with numeric, text, mixed-channel, status-only, empty, sparse, and fully populated datasets.
- Use deterministic synchronization hooks, not sleeps, to pause writes and prove old-state/new-state visibility.
- Force failures during dimension/category insertion, observation `COPY`, context cancellation, and commit; verify complete rollback.
- Test concurrent create-only conflicts, serialized replacements, replacement versus deletion, and independent writes to different datasets.
- Run the Go race detector for all unit and integration tests.

## Container and Release Acceptance

- Confirm that the runtime image runs as non-root, needs no writable application filesystem, contains both commands, and starts with only documented environment variables.
- Confirm that the migration command exits nonzero on incompatible PostgreSQL, invalid encoding, or failed migration.
- Confirm that the API refuses startup with missing secrets, equal tokens, unreachable PostgreSQL, or the wrong schema version.
- Confirm that `SIGTERM` stops new traffic and either completes or safely rolls back in-flight writes.
- Retain useful logs and test reports as CI artifacts while ensuring they contain no credentials or observation content.

## Million-Cell Gate

- Provide a manual CI workflow and make its successful run mandatory before each release.
- Test a fully populated one-million-cell dense replacement, sparse-object replacement, full dense read, full sparse read, and reordered exact subset.
- Verify counts, partition routing, float/text/status fidelity, inferred-null behavior, atomic visibility, and rollback.
- Run against PostgreSQL 18 with the intended production container resource allocation.
- Require completion within configured request/database deadlines and without OOM or partial output, but set no separate latency or RSS threshold.
- Record request duration, database duration, peak container memory, response size, and resulting database size as artifacts for future threshold selection.

## Assumptions

- The deployment platform and image registry remain unspecified.
- Production PostgreSQL is external and rebuildable from upstream source data.
- PostgreSQL 18 is the supported v1 runtime even though the DDL avoids features newer than PostgreSQL 15.
- Pull requests run PostgreSQL 18 only; the expensive million-cell workflow is manual and a pre-release gate.
- No code-coverage percentage, latency SLO, backup RPO, or recovery RTO is imposed in v1.
