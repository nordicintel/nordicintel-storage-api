# NordicIntel Storage API

A private JSON API that stores the latest accepted state of observation
datasets. Internal ingestion and backend services replace whole datasets
atomically, then read them back complete or as exact Cartesian subsets.

- **Language and datastore:** Go 1.27 and PostgreSQL 18.
- **Authoritative design:** [`planning/spec/`](planning/spec/README.md). The
  Markdown contract governs; [`internal/apidocs/openapi.json`](internal/apidocs/openapi.json)
  is a verified machine-readable projection of it.
- **Scope:** one million logical cells per dataset, including fully populated
  ones. No history, no patching, no public access.

## Contents

- [Quick start](#quick-start)
- [Configuration](#configuration)
- [Migrations](#migrations)
- [API examples](#api-examples)
- [Local development](#local-development)
- [Deployment](#deployment)
- [Smoke tests](#smoke-tests)
- [Token rotation](#token-rotation)
- [Logging and alerts](#logging-and-alerts)
- [Disaster recovery](#disaster-recovery)
- [Repository layout](#repository-layout)

## Quick start

```sh
cp .env.example .env          # then replace both placeholder tokens
docker compose up -d postgres # disposable PostgreSQL 18, no volume

set -a && . ./.env && set +a
go run ./cmd/migrate          # apply the schema
go run ./cmd/api              # serve on :8080
```

Then open <http://localhost:8080/docs/> for the interactive documentation, or:

```sh
curl -s localhost:8080/health
```

## Configuration

Configuration is environment only. Startup fails on a missing, invalid, or
undersized value; credentials and database URLs are never logged or trimmed.

| Variable | Required | Default | Rule |
|---|---:|---|---|
| `DATABASE_URL` | yes | — | External PostgreSQL 18 URL. Production must enable provider-appropriate TLS. |
| `API_READ_WRITE_TOKEN` | yes | — | At least 32 UTF-8 bytes. Permits reads, queries, creation, replacement, and deletion. |
| `API_READ_ONLY_TOKEN` | yes | — | At least 32 UTF-8 bytes, and different from the write token. Permits reads and queries only. |
| `PORT` | no | `8080` | Binds on all interfaces. |
| `MAX_REQUEST_BYTES` | no | `134217728` | Maximum decoded JSON body size. |
| `MAX_CELLS` | no | `1000000` | May be lowered, never raised above the schema ceiling. Limits replacements and query result products; already-stored datasets stay fully readable. |
| `DB_MAX_CONNS` | no | `4` | Bounded pgx pool size. |
| `REQUEST_TIMEOUT` | no | `5m` | Whole-request deadline. |
| `DB_TIMEOUT` | no | `4m30s` | Database deadline; must be shorter than `REQUEST_TIMEOUT`. |
| `SHUTDOWN_TIMEOUT` | no | `30s` | Grace period before remaining requests are cancelled. |
| `LOG_LEVEL` | no | `info` | `debug`, `info`, `warn`, or `error`. |

Generate tokens with `openssl rand -base64 48`.

## Migrations

The API never migrates automatically. `/app/migrate` is a separate command and a
separate deployment job.

```sh
DATABASE_URL=... go run ./cmd/migrate      # from source
docker run --rm --entrypoint /app/migrate \
  -e DATABASE_URL=... <image>              # from the release image
```

- The migration is embedded in the binary and applied inside one transaction
  under a fixed global advisory lock, with a ten-minute deadline. A second
  deployment job waits rather than racing.
- Re-running it is a no-op.
- Production migrations are forward only. The down migration is compiled out of
  release builds by the `production` build tag and exists solely for disposable
  test databases.
- The API refuses to start unless the server is PostgreSQL 18, the database is
  UTF-8, it can connect within 15 seconds, and the schema is at the exact
  expected version.
- A migration failure blocks the API rollout.

## API examples

Every `/v1` route needs `Authorization: Bearer <token>`. `/health`,
`/openapi.json`, and `/docs/` are the only public routes.

Create a dataset (`replace: false` is the default, so this is create-only):

```sh
curl -X POST localhost:8080/v1/providers/SCB/datasets/Population \
  -H "Authorization: Bearer $API_READ_WRITE_TOKEN" \
  -H 'Content-Type: application/json' \
  -d '{
        "source_stamp": {"etag": "abc"},
        "id": ["sex", "year"],
        "dimension": {
          "sex":  {"index": {"M": 0, "F": 1}},
          "year": {"index": {"2024": 0, "2025": 1}}
        },
        "value":  [10.5, null, null, null],
        "text":   [null, null, null, "confidential"],
        "status": [null, null, null, "c"]
      }'
```

`201` with `{"result":"created","dataset":{…}}`. Repeating it returns
`409 dataset_exists`; add `"replace": true` to overwrite, which returns `200`
and `"result":"replaced"`.

Read it back. Sparse is the default; `?format=dense` returns every logical cell:

```sh
curl localhost:8080/v1/providers/SCB/datasets/Population/data \
  -H "Authorization: Bearer $API_READ_ONLY_TOKEN"

curl "localhost:8080/v1/providers/SCB/datasets/Population/data?format=dense" \
  -H "Authorization: Bearer $API_READ_ONLY_TOKEN"
```

Request an exact subset. The body is a structure-only selector, and its
dimension and category order defines the response order:

```sh
curl -X POST localhost:8080/v1/providers/SCB/datasets/Population/query \
  -H "Authorization: Bearer $API_READ_ONLY_TOKEN" \
  -H 'Content-Type: application/json' \
  -d '{"id": ["year", "sex"],
       "dimension": {"year": {"index": {"2025": 0}},
                     "sex":  {"index": {"F": 0, "M": 1}}}}'
```

List, then delete. Deletion is idempotent and always returns `204`:

```sh
curl localhost:8080/v1/providers -H "Authorization: Bearer $API_READ_ONLY_TOKEN"
curl localhost:8080/v1/providers/SCB/datasets -H "Authorization: Bearer $API_READ_ONLY_TOKEN"
curl -X DELETE localhost:8080/v1/providers/SCB/datasets/Population \
  -H "Authorization: Bearer $API_READ_WRITE_TOKEN"
```

Errors always use one envelope with a stable code:

```json
{"error": {"code": "validation_failed",
           "message": "dimension indexes must be contiguous",
           "request_id": "…"}}
```

Codes: `invalid_json`, `invalid_query`, `invalid_path_code`, `unauthorized`,
`forbidden`, `not_found`, `method_not_allowed`, `dataset_exists`,
`request_too_large`, `unsupported_media_type`, `validation_failed`,
`cell_limit_exceeded`, `internal_error`, `service_unavailable`.

## Local development

```sh
docker compose up -d postgres
export DATABASE_URL='postgres://storage:storage@localhost:5432/storage?sslmode=disable'

gofmt -l cmd internal tools    # must print nothing
go vet ./...
go test -race ./...            # integration tests need DATABASE_URL
```

Without `DATABASE_URL` the PostgreSQL-backed tests skip and the unit, contract,
and fuzz tests still run.

Useful subsets:

```sh
go test ./internal/domain/ -run Fuzz -fuzz FuzzNormalizeCodeIsIdempotent -fuzztime=60s
MILLION_CELL_TEST=1 go test ./internal/httpapi/ -run TestMillionCell -v -timeout=30m
```

Build and exercise the production image exactly as CI does:

```sh
docker build -t nordicintel-storage-api:local .
bash scripts/container-smoke.sh nordicintel-storage-api:local
SIDE=40 bash scripts/million-cell.sh nordicintel-storage-api:local reports
```

## Deployment

The service is a platform-neutral OCI image that connects to externally hosted
PostgreSQL. It runs as a non-root user, needs no writable application
filesystem and no volumes, writes logs only to stdout and stderr, and handles
`SIGTERM` gracefully. Release images are published for `linux/amd64` and
`linux/arm64` to GHCR with immutable version tags and recorded digests.

Deploy immutable digests in this order:

1. Run `/app/migrate` as a one-off job and wait for it to exit zero.
2. Roll out `/app/api`.
3. Verify `/health` returns `200`.
4. Run the authenticated read smoke test.
5. Run the create, query, and delete smoke test.

Roll the application image back only when the migration is
backward-compatible; otherwise deploy a forward fix. Assume inbound TLS is
terminated by the platform or a reverse proxy. CORS is disabled.

Releases are tag driven: push a unique `vX.Y.Z` tag whose commit already carries
a successful `acceptance/million-cell` status. The release workflow refuses to
overwrite a version tag that already exists in the registry.

## Smoke tests

After a rollout, against the deployed base URL:

```sh
BASE=https://storage.internal.example

curl -fsS "$BASE/health"

# Authenticated read.
curl -fsS "$BASE/v1/providers" -H "Authorization: Bearer $API_READ_ONLY_TOKEN"

# Create, query, delete against a throwaway identity.
D="$BASE/v1/providers/SmokeProvider/datasets/SmokeDataset"
curl -fsS -X POST "$D" -H "Authorization: Bearer $API_READ_WRITE_TOKEN" \
  -H 'Content-Type: application/json' \
  -d '{"replace":true,"source_stamp":{"smoke":true},"id":["a"],
       "dimension":{"a":{"index":{"x":0}}},"value":[1]}'
curl -fsS -X POST "$D/query" -H "Authorization: Bearer $API_READ_ONLY_TOKEN" \
  -H 'Content-Type: application/json' \
  -d '{"id":["a"],"dimension":{"a":{"index":{"x":0}}}}'
curl -fsS -o /dev/null -w '%{http_code}\n' -X DELETE "$D" \
  -H "Authorization: Bearer $API_READ_WRITE_TOKEN"
```

`scripts/container-smoke.sh` runs the same checks plus authorization, media
type, documentation, and `SIGTERM` handling against a locally built image.

## Token rotation

One token per role, rotated by updating the environment and restarting:

1. Generate a replacement (`openssl rand -base64 48`).
2. Update the secret in the deployment platform.
3. Restart the service and confirm `/health`.
4. Update every client of that role.

Overlapping credentials for zero-downtime rotation are outside v1, so rotate
the read-only and read/write tokens in separate windows to limit the blast
radius. The two tokens must always differ; startup fails otherwise.

## Logging and alerts

Logs are JSON on stdout, one line per request, carrying the request ID, method,
**route template**, status, total duration, database duration, request and
response byte counts, and the mutation result. Raw paths, provider and dataset
codes, credentials, database URLs, bodies, source stamps, values, text values,
and statuses are never logged. Every response returns `X-Request-ID`, and the
same ID appears in the error envelope, so a client report maps to one log line.

Initial log-based alerts:

| Condition | Signal |
|---|---|
| Migration failure | Repeated `"msg":"migration failed"` from the migration job. |
| Sustained readiness failure | `/health` returning `503` beyond a short window. |
| Elevated errors | A rising rate of `5xx`, especially `503`. |
| Pool exhaustion | Database durations approaching `DB_TIMEOUT` alongside `503`s. |
| Restart loop | Repeated container restarts or repeated `"msg":"API ready"`. |

No public metrics endpoint is required in v1.

## Disaster recovery

PostgreSQL is the only durable component, and it is rebuildable from retained
upstream sources. Database backups are optional. To recover:

1. Provision a fresh UTF-8 PostgreSQL 18 database.
2. Point `DATABASE_URL` at it and run `/app/migrate`.
3. Roll out `/app/api` and verify `/health`.
4. Reingest every dataset from the upstream sources.

Direct database writes are unsupported: the service, not SQL triggers, enforces
the cross-row invariants.

## Repository layout

```
cmd/api            HTTP service entry point
cmd/migrate        forward-only migration command
internal/apidocs   embedded OpenAPI document and Swagger UI handler
internal/config    strict startup configuration
internal/domain    normalization, coordinates, payload parsing and remapping
internal/httpapi   routing, middleware, and handlers
internal/jsonx     strict, duplicate-key-aware JSON decoding
internal/migrations embedded migrations and startup compatibility checks
internal/presentation dense and sparse response encoding
internal/store     PostgreSQL storage engine
planning/spec      authoritative design documents
scripts            container smoke test and million-cell gate
tools/millioncell  acceptance driver for the million-cell gate
```
