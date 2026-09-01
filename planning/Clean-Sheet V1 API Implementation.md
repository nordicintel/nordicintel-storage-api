# Clean-Sheet V1 API Implementation

## Preliminary Step: Isolate the Legacy Implementation

- Create `redesign-v1-implementation` from the planning branch.
- As the first repository-content change, move every tracked legacy application artifact into `archive/initial-implementation/`, including source, tests, workflows, deployment files, module files, and legacy root documentation.
- Keep `planning/spec/**`, `.gitignore`, and repository metadata at the root. Do not move, inspect, or copy local secrets such as `.env`.
- Treat the archive as sealed: nothing inside it may inform the new architecture, code, tests, dependencies, schema, or naming.
- Exclude the archive from Go package discovery and Docker build contexts.
- Keep the archive tracked until the redesigned implementation passes all automated checks and the million-cell acceptance gate. Then delete it; Git history remains the recovery mechanism.

## Contract Harmonization Before Coding

Update the planning documents and their index so implementation begins from one decision-complete specification:

- Add exact response envelopes:
  - Providers: `{"providers":[{"provider_code":"...","dataset_count":1}]}`
  - Provider datasets: `{"provider_code":"...","datasets":[<dataset summaries>]}`
  - Creation/replacement: `{"result":"created|replaced","dataset":<summary>}`
  - Single summary remains the summary object itself.
  - Structure, full-data, and query responses retain their documented flat metadata-plus-data shape.
- Define query bodies as structure-only selectors using `id` and `dimension`; indexes are contiguous, payload-local, and determine output order.
- Require replacement structures to contain 1–64 dimensions and at least one category per dimension. Limit submitted and normalized codes to 256 UTF-8 bytes.
- Clarify channel behavior:
  - `value` determines dense versus sparse representation for `value` and `text`.
  - Non-scalar `status` independently selects dense or sparse representation from its JSON type.
  - A scalar status expands across the whole payload.
  - Responses use scalar status only when every returned cell has the same non-null status.
  - `value` is always returned; all-null `text` and `status` are omitted.
- Define query-string handling: only one `format=dense|sparse` parameter is accepted on data/query reads; unknown or duplicate parameters return `400`.
- Accept `application/json` with an optional UTF-8 charset and reject unsupported content encodings/media types.
- Add stable error codes: `invalid_json`, `invalid_query`, `invalid_path_code`, `unauthorized`, `forbidden`, `not_found`, `method_not_allowed`, `dataset_exists`, `request_too_large`, `unsupported_media_type`, `validation_failed`, `cell_limit_exceeded`, `internal_error`, and `service_unavailable`.
- Clarify that provider spelling survives deletion because the registry row remains, while a deleted and later recreated dataset receives new first-creation spelling.
- Define the advisory-lock hash byte layout: big-endian length-prefixed normalized UTF-8 keys, SHA-256, first eight bytes interpreted as a signed big-endian `int64`.
- Reconcile PostgreSQL support language: DDL remains PostgreSQL 15-compatible, but PostgreSQL 18 is the only required CI and production runtime.
- Add public `GET /openapi.json`, `/docs`, and `/docs/` routes as explicit exceptions to `/v1` authentication. Keep the underlying data API private.
- Update `planning/spec/README.md` with authority, status, reading order, and conflict precedence.

## Stage 1 — Foundation, Schema, and Storage Engine

- Create a fresh Go 1.27 module using the standard HTTP library, `pgx/v5`, `tern/v2`, pinned `golang.org/x/text`, and structured `slog` logging.
- Establish separate `api` and `migrate` commands with small internal packages for configuration, normalization, contract types, JSON processing, coordinate mapping, PostgreSQL storage, migrations, and HTTP delivery.
- Implement strict startup configuration:
  - Validate both tokens as distinct values of at least 32 UTF-8 bytes.
  - Validate all documented limits and timeout relationships.
  - Never trim or log credentials.
  - Set a bounded pgx pool using `DB_MAX_CONNS`.
- Convert the target DDL into embedded migration `001`:
  - Preserve the five logical tables, constraints, comments, and 32 observation partitions.
  - Keep migration metadata outside `storage`, using the migration tool’s version table.
  - Provide a test-only down migration while exposing forward migration only through the production command.
  - Acquire a fixed global advisory lock and enforce the ten-minute migration deadline.
- Make API startup require PostgreSQL major version 18, UTF-8, connectivity within 15 seconds, and the exact embedded migration version.
- Implement domain processing before database mutation:
  - Strict duplicate-key-aware JSON decoding, including nested source stamps.
  - Unicode normalization and collision detection.
  - Checked dimension products and row-major index conversion.
  - Dense/sparse validation, finite `float64` parsing, text/numeric exclusion, status expansion, null inference, and payload-to-internal remapping.
  - Store only populated cells and compute valued counts from numeric/text presence.
- Implement PostgreSQL operations:
  - Deterministic provider/dataset listings and summaries.
  - Repeatable-read structure, full-data, and exact-subset reads.
  - Atomic create/replace using the shared advisory lock, row locking, structure replacement, `COPY` into the partitioned parent, final metadata update, and commit.
  - Idempotent delete using the same lock.
  - Query index fetching in deterministic batches of at most 10,000 indexes.
- Add nil-by-default, internal transaction checkpoints for deterministic concurrency and rollback tests; production construction cannot enable them.

Stage 1 exits when migrations, domain logic, storage integration tests, rollback tests, and concurrency tests pass against disposable PostgreSQL 18.

## Stage 2 — HTTP API, OpenAPI, and Interactive Documentation

- Implement the complete route table from the harmonized contract with explicit method handling and `Allow` headers.
- Add middleware for:
  - Cryptographically random server-controlled request IDs.
  - Whole-request deadlines and database sub-deadlines.
  - Constant-time SHA-256 token comparison against both configured credentials.
  - Role enforcement, body limits, byte accounting, sanitized route-template logging, panic recovery, and `Cache-Control: no-store`.
- Configure the server with a 5-second header timeout, 16 KiB maximum headers, 60-second idle timeout, request deadline enforcement, and graceful shutdown.
- Return the specified JSON error envelope consistently. Deadline/database failures before response commitment return `503`; client disconnects cancel database work and allow transaction rollback.
- Implement `/health` with a separate two-second database timeout and no diagnostic leakage.
- Encode `float64` directly as valid JSON numbers without preserving submitted lexical spelling.
- Complete all endpoint behavior:
  - Provider and dataset listing.
  - Dataset summary and structure.
  - Create-only POST and unconditional replacement.
  - Full dense/sparse reads.
  - Reordered exact Cartesian queries.
  - Idempotent deletion.
- Once the complete API and its contract tests pass:
  - Add an OpenAPI 3.1 JSON document derived from the finalized Markdown contract.
  - Validate the document and examples in tests and compare its operations with the registered routes.
  - Serve it publicly from `/openapi.json`.
  - Bundle a pinned Swagger UI distribution and license into the API binary with `go:embed`; serve it from `/docs/` without a CDN or writable filesystem.
  - Configure Swagger bearer authentication with `persistAuthorization` disabled.
  - Redirect `/docs` to `/docs/` and keep documentation routes free of credentials and environment-specific server URLs.

Stage 2 exits when every endpoint, error mapping, auth role, response format, OpenAPI operation, public documentation route, and graceful cancellation case passes unit and PostgreSQL-backed contract tests.

## Stage 3 — Containers, CI, Release, and Acceptance

- Add a multi-stage Docker build:
  - Pin Go builder and minimal non-root runtime images by digest.
  - Build static `/app/api` and `/app/migrate` binaries for `linux/amd64` and `linux/arm64`.
  - Default to `/app/api`.
  - Include TLS CA certificates and support a read-only root filesystem without volumes.
- Add `.dockerignore`, a safe `.env.example`, disposable PostgreSQL 18 Compose configuration, and replacement root documentation covering configuration, migrations, local development, API examples, deployment, smoke tests, token rotation, logging, alerts, and disaster recovery.
- Implement pull-request CI:
  - Formatting verification, `go vet`, race-enabled unit/integration tests, binary builds, migration/schema verification, and production image build.
  - Start the built image with disposable PostgreSQL, migrate, wait for health, and test authentication, create/query/delete, documentation, and `SIGTERM`.
  - Retain sanitized reports without credentials or observation content.
- Add a manual million-cell workflow:
  - Use PostgreSQL 18 and default resource limits of 1 vCPU/1 GiB for the API and 2 vCPU/2 GiB for PostgreSQL, with explicit override inputs for the intended deployment allocation.
  - Exercise fully populated dense and sparse replacements, full dense/sparse reads, reordered subsets, atomic visibility, and forced rollback.
  - Require completion within configured request/database deadlines without OOM or truncated successful output.
  - Record request/database duration, peak memory, response size, database size, and selected resource limits.
  - Publish a successful commit status tied to the exact tested SHA.
- Only after the normal test suite and million-cell gate pass, add the release workflow:
  - Require a unique semantic-version tag whose commit has a successful million-cell status.
  - Build both architectures, emit provenance/SBOM metadata, and publish immutable version and commit tags plus recorded digests to GHCR.
  - Fail rather than overwrite an existing version tag.
  - GHCR is selected because the repository is public and GitHub currently states that Container Registry storage and bandwidth are free; reassess if GitHub announces a billing change. [GitHub Packages billing](https://docs.github.com/en/billing/concepts/product-billing/github-packages)
- Document platform-neutral rollout order: run `/app/migrate`, roll out `/app/api`, verify health, perform authenticated read smoke testing, then create/query/delete smoke testing.
- Define log-based alert conditions for migration failures, sustained readiness failures, elevated `5xx`/`503`, pool exhaustion, and container restart loops.
- Delete `archive/initial-implementation/` only after every Stage 3 gate succeeds, then rerun the complete automated suite and image build against the archive-free tree.

## Final Acceptance

The implementation is complete when:

- All behavior in goals, API, schema, storage, operations, and test specifications is represented consistently in Markdown, OpenAPI, SQL, code, and tests.
- A failed or concurrent replacement never exposes mixed dataset state.
- Read-only credentials cannot mutate data.
- Exact subsets preserve requested ordering and infer absent cells correctly.
- Schema constraints, partition routing, migration locking, and startup compatibility checks work on PostgreSQL 18.
- The API and migration commands run in the pinned non-root multi-architecture container.
- Public interactive documentation matches the implemented private API.
- A fully populated million-cell dataset passes the release gate.
- The sealed legacy archive has been deleted without any legacy artifact being reused.

## Assumptions

- This is a clean-sheet implementation; the archived codebase is never a design or implementation input.
- The target database is fresh or rebuildable. No legacy schema or data migration is provided.
- PostgreSQL 18 is the supported runtime; PostgreSQL 15–17 compatibility is best-effort DDL compatibility only.
- The Markdown contract remains authoritative; OpenAPI is a verified machine-readable projection.
- `MAX_CELLS` limits replacements and query result products. Existing stored datasets remain fully readable if an operator later lowers the configured write/query limit.
- Pagination, compression, history, patching, background jobs, public data access, and deployment-platform-specific infrastructure remain outside v1.
