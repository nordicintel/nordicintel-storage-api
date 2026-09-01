# NordicIntel Storage API

A private JSON API for the latest accepted state of observation datasets.
Internal services replace complete datasets atomically and can read either the
full cube or exact Cartesian subsets.

- Go 1.27 and PostgreSQL 18.
- Up to 1,000,000 logical cells per dataset.
- Dense and sparse observation encoding with inferred null cells.
- No history, patching, aggregation, or public data access.

## Documentation

- [API guide](docs/api.md) — authentication, payloads, and query semantics.
- [Architecture](docs/architecture.md) — normalization, storage, and consistency.
- [Operations](docs/operations.md) — configuration, development, testing, and deployment.
- [OpenAPI 3.1](internal/apidocs/openapi.json) — exhaustive HTTP contract, also
  served at `/openapi.json` with Swagger UI at `/docs/`.

Executable artifacts are authoritative: OpenAPI defines HTTP, migrations define
the database schema, and code plus tests define behavior. The prose documents
are concise guides to those artifacts.

## Quick start

```sh
cp .env.example .env          # replace both placeholder tokens
docker compose up -d postgres # disposable PostgreSQL 18

set -a && . ./.env && set +a
go run ./cmd/migrate
go run ./cmd/api
```

Then check readiness with `curl -s localhost:8080/health` or open
<http://localhost:8080/docs/>.

## Key constraints

- Every `/v1` route requires a bearer token; only the read/write token may mutate.
- Replacements are synchronous and atomic. Failed writes leave the prior state visible.
- Codes are matched through Unicode normalization while stored spellings are preserved.
- Observation order is row-major: the last dimension varies fastest.
- PostgreSQL must be version 18, use UTF-8, and have the exact expected migration version.

## Repository layout

```text
cmd/                       API and migration entry points
docs/                      concise API, architecture, and operations guides
internal/apidocs/          embedded OpenAPI document and Swagger UI
internal/domain/           normalization, indexing, and payload validation
internal/httpapi/          routes, middleware, handlers, and integration tests
internal/migrations/       embedded authoritative database migrations
internal/presentation/     dense and sparse response encoding
internal/store/            PostgreSQL storage engine
scripts/                   container smoke and million-cell acceptance scripts
tools/millioncell/         million-cell acceptance driver
tools/evaluator/testdata/  JSONL fixtures reserved for the evaluation tool
```

## License

Licensed under the [MIT License](LICENSE).
