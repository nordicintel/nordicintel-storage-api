# nordicintel-storage-api

Private PostgreSQL-backed storage for the current observation state of NordicIntel datasets. The service preserves provider codes and submitted dimension order, provides normalized exact-code lookup, and supports atomic full replacements plus coordinate-based incremental changes.

## Runtime

- Go 1.27
- PostgreSQL 15–18 (PostgreSQL 18 is used in CI)
- `net/http`, `pgx/v5`, and embedded `tern/v2` migrations
- Heroku native Go buildpack with GitHub deployment

The service stores observations only. Provider descriptions, labels, search indexes, historical versions, wildcard parsing, and public indexes belong elsewhere.

## Configuration

| Variable | Required | Default | Purpose |
|---|---:|---:|---|
| `DATABASE_URL` | yes | — | PostgreSQL connection URL; supplied by Heroku Postgres in production |
| `API_BEARER_TOKEN` | yes | — | Shared token required by every `/v1/*` request |
| `PORT` | no | `8080` | HTTP listen port; supplied by Heroku |
| `MAX_REQUEST_BYTES` | no | `134217728` | Maximum JSON request body size |
| `MAX_CELLS` | no | `1000000` | Maximum full cube or selected result size |
| `DB_MAX_CONNS` | no | `4` | PostgreSQL pool limit |
| `DB_TIMEOUT` | no | `25s` | Per-operation database deadline |

Use a long random bearer token. Rotate it with `heroku config:set API_BEARER_TOKEN=...`; changing a config var restarts the application. Never commit `.env` files or database credentials.

## Development

The project intentionally has no Docker database configuration. Point local commands at an existing PostgreSQL database:

```powershell
$env:DATABASE_URL = "postgres://postgres:postgres@localhost/storage_api?sslmode=disable"
$env:TEST_DATABASE_URL = "postgres://postgres:postgres@localhost/storage_api_test?sslmode=disable"
$env:API_BEARER_TOKEN = "development-only-token"

go run ./cmd/migrate
go run ./cmd/api
```

Run checks with:

```powershell
go test -race ./...
go vet ./...
go build ./cmd/api ./cmd/migrate
```

Integration tests are skipped unless `TEST_DATABASE_URL` is set. The integration database is destructive test scope: its `hub` schema is migrated down and up during testing.

Run the opt-in million-cell database benchmark against production-like capacity with:

```powershell
$env:RUN_MILLION_CELL_BENCHMARK = "1"
go test ./internal/store -run '^$' -bench BenchmarkPostgresReplaceMillionCells -benchtime=1x
```

## API conventions

`GET /health` is public and reports both process and database health. All `/v1/*` routes require:

```text
Authorization: Bearer <API_BEARER_TOKEN>
```

JSON errors use this shape:

```json
{"error":{"code":"invalid_request","message":"..."}}
```

Codes are returned exactly as first stored, but lookup and uniqueness ignore surrounding whitespace, Unicode compatibility normalization, and case. JSON numbers are preserved without conversion through binary floating point. Clients that cannot safely parse large JSON numbers must use a decimal-capable JSON parser.

### Routes

```text
GET    /health
GET    /v1/providers
GET    /v1/datasets
GET    /v1/datasets?full=true
GET    /v1/datasets/{provider}
GET    /v1/datasets/{provider}?full=true
GET    /v1/datasets/{provider}/{dataset}
PUT    /v1/datasets/{provider}/{dataset}
PATCH  /v1/datasets/{provider}/{dataset}
DELETE /v1/datasets/{provider}/{dataset}
GET    /v1/data/{provider}/{dataset}
POST   /v1/data/{provider}/{dataset}
```

### Complete replacement

`values` must contain exactly the product of the category counts. `statuses` may be omitted; when present it must have the same length. The last dimension varies fastest. A position with both value and status null is absent from storage.

```bash
curl -X PUT "$BASE_URL/v1/datasets/SCB/Population" \
  -H "Authorization: Bearer $API_BEARER_TOKEN" \
  -H "Content-Type: application/json" \
  -d '{
    "source_stamp":{"etag":"abc123"},
    "dimensions":[
      {"code":"Sex","categories":["M","F"]},
      {"code":"Year","categories":["2024","2025"]}
    ],
    "values":[10,11,12,13],
    "statuses":[null,null,"p",null]
  }'
```

An omitted `source_stamp` retains its current value; explicit `null` clears it. A new dataset returns `201` and `{"result":"created",...}`; replacement returns `200` and `{"result":"updated",...}`.

### Incremental changes

Every item names one exact category for every dimension. Non-delete items replace the complete cell state, so omitting `value` or `status_code` means null for that field. Delete items may contain only `categories` and `"delete":true`. Duplicate coordinates are rejected atomically.

```bash
curl -X PATCH "$BASE_URL/v1/datasets/SCB/Population" \
  -H "Authorization: Bearer $API_BEARER_TOKEN" \
  -H "Content-Type: application/json" \
  -d '{
    "observations":[
      {"categories":{"Sex":"M","Year":"2025"},"value":14,"status_code":"p"},
      {"categories":{"Sex":"F","Year":"2024"},"delete":true}
    ]
  }'
```

### Reads and selections

Full and selected reads return `dimensions`, `values`, and `statuses`. Missing cells are null in both arrays. Exact selections must include every dimension exactly once, may reorder dimensions/categories, and use the last requested dimension as the fastest-varying output dimension.

```bash
curl -X POST "$BASE_URL/v1/data/SCB/Population" \
  -H "Authorization: Bearer $API_BEARER_TOKEN" \
  -H "Content-Type: application/json" \
  -d '{"dimensions":[
    {"code":"Year","categories":["2025"]},
    {"code":"Sex","categories":["F","M"]}
  ]}'
```

## Heroku deployment

The repository contains explicit `web` and `release` processes. Every GitHub-triggered release builds both binaries; the release process applies transactional migrations before the web dyno is promoted.

Before the first deployment:

1. Confirm the Postgres add-on is attached as `DATABASE_URL` and uses UTF-8 PostgreSQL 15 or newer.
2. Set `API_BEARER_TOKEN`; optionally tune the limits and pool for the selected dyno/database plans.
3. Deploy and confirm the release migration succeeds in Heroku logs.
4. Call `/health`, then an authenticated `/v1/providers` request.
5. Run a production-like one-million-cell replacement before accepting that limit. The database portion must finish comfortably below Heroku’s 30-second router deadline; lower `MAX_CELLS` if it does not.

The service emits JSON request logs containing request ID, method, matched route, status, total duration, and database duration. It never logs bearer tokens, request bodies, observation values, or source stamps.
