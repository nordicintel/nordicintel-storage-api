# Operations

## Configuration

Configuration is environment-only. Startup rejects missing or invalid required
values; `.env.example` contains safe local placeholders and all supported keys.

| Variable | Required | Default |
|---|---:|---|
| `DATABASE_URL` | yes | — |
| `API_READ_WRITE_TOKEN` | yes | — |
| `API_READ_ONLY_TOKEN` | yes | — |
| `PORT` | no | `8080` |
| `MAX_REQUEST_BYTES` | no | `134217728` |
| `MAX_CELLS` | no | `1000000` |
| `DB_MAX_CONNS` | no | `4` |
| `REQUEST_TIMEOUT` | no | `5m` |
| `DB_TIMEOUT` | no | `4m30s` |
| `SHUTDOWN_TIMEOUT` | no | `30s` |
| `LOG_LEVEL` | no | `info` |

Tokens must be at least 32 UTF-8 bytes and differ. Production database URLs must
enable provider-appropriate TLS. `MAX_CELLS` may be lowered but not raised above
the schema ceiling.

## Local development and testing

```sh
docker compose up -d postgres
export DATABASE_URL='postgres://storage:storage@localhost:5432/storage?sslmode=disable'

gofmt -l cmd internal tools
go vet ./...
go test -race ./...
```

PostgreSQL-backed tests skip without `DATABASE_URL`. Build and exercise the
production image with:

```sh
docker build -t nordicintel-storage-api:local .
bash scripts/container-smoke.sh nordicintel-storage-api:local
SIDE=40 bash scripts/million-cell.sh nordicintel-storage-api:local reports
```

The JSONL files under `tools/evaluator/testdata/` are source-derived insertion
and expected-query fixtures for the deferred evaluator. Each non-empty line is
one dataset or `{ "input": ..., "output": ... }` query case. Preserve source
provenance when refreshing them and review size plus record-count changes.

## Migrations and deployment

The API never migrates automatically. Run the embedded migration as a separate
job before rolling out the API:

```sh
DATABASE_URL=... go run ./cmd/migrate
docker run --rm --entrypoint /app/migrate -e DATABASE_URL=... <image>
```

PostgreSQL 18 with UTF-8 is required. Migrations are serialized, transactional,
and forward-only in production; down migrations exist only for disposable tests.

Release images are minimal, non-root, multi-architecture OCI images published to
GHCR with immutable version and commit tags. Deploy by digest in this order:

1. Run `/app/migrate` and require success.
2. Roll out `/app/api`.
3. Verify `/health`.
4. Run authenticated read and create/query/delete smoke tests.

CI runs formatting, module-tidiness, vet, race-enabled tests, fuzz seeds, binary
builds, container checks, and smoke tests. The manual million-cell workflow must
succeed for the exact commit before a semantic-version release tag is accepted.

## Runtime and recovery

The container needs no writable application filesystem or persistent volume,
writes structured logs to stdout/stderr, and handles `SIGTERM` gracefully. Logs
include request IDs, route templates, status, timing, and byte counts, but never
credentials, raw paths, codes, database URLs, bodies, or observation content.

Alert on migration failures, sustained readiness failures, elevated `5xx`/`503`,
pool exhaustion, and restart loops. PostgreSQL is rebuildable from retained
upstream data: provision a fresh UTF-8 PostgreSQL 18 database, migrate it, deploy
the API, and reingest all datasets.
