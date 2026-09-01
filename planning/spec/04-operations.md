# Operations

## Runtime and Containers

The API is distributed as a platform-neutral OCI container that connects to externally hosted PostgreSQL through `DATABASE_URL`.

- Use one multi-stage Docker build to produce static `api` and `migrate` binaries in a minimal, pinned, non-root runtime image. Multi-stage builds keep build tooling out of the runtime image: [Docker multi-stage builds](https://docs.docker.com/build/building/multi-stage/).
- Use `/app/api` as the default command. Deployments invoke `/app/migrate` explicitly before rolling out the API.
- Support a read-only root filesystem, require no persistent container volumes, write logs only to stdout/stderr, and handle `SIGTERM` gracefully.
- Publish `linux/amd64` and `linux/arm64` release images with immutable version tags and digests.
- Publish release images to the public GitHub Container Registry only after automated tests and the million-cell gate pass. Fail rather than overwrite an existing version tag.
- Do not include PostgreSQL in the production image.
- Provide a disposable test Compose definition using the official `postgres:18` image, UTF-8, password authentication, `pg_isready`, and no persistent volume. The official image supports initialization through `POSTGRES_USER`, `POSTGRES_PASSWORD`, and `POSTGRES_DB`: [PostgreSQL Docker image](https://hub.docker.com/_/postgres/).

## Configuration

| Variable | Required | Default | Rule |
|---|---:|---:|---|
| `DATABASE_URL` | yes | — | External PostgreSQL URL; production configuration must enable provider-appropriate TLS |
| `API_READ_WRITE_TOKEN` | yes | — | At least 32 bytes; permits reads, queries, creation, replacement, and deletion |
| `API_READ_ONLY_TOKEN` | yes | — | At least 32 bytes; permits only reads and queries; must differ from the write token |
| `PORT` | no | `8080` | Bind on all interfaces |
| `MAX_REQUEST_BYTES` | no | `134217728` | Maximum decoded JSON body size |
| `MAX_CELLS` | no | `1000000` | May lower, but never exceed the schema ceiling |
| `DB_MAX_CONNS` | no | `4` | Positive bounded pool size |
| `REQUEST_TIMEOUT` | no | `5m` | Whole-request deadline, including body read and response construction |
| `DB_TIMEOUT` | no | `4m30s` | Database deadline within the request deadline |
| `SHUTDOWN_TIMEOUT` | no | `30s` | Grace period before canceling remaining requests |
| `LOG_LEVEL` | no | `info` | `debug`, `info`, `warn`, or `error` |

Invalid, missing, equal, or undersized credentials and invalid limits cause startup failure. Secrets and database URLs are never logged.

## Lifecycle and Database

- Require PostgreSQL 18 operationally. The target DDL remains PostgreSQL 15-compatible, but only PostgreSQL 18 is continuously tested and supported for v1.
- Require UTF-8 and the exact expected migration version at startup.
- Ping the database within 15 seconds before accepting traffic; fail fast if connection or schema checks fail.
- Run embedded forward migrations as a separate deployment job with a 10-minute deadline and a global advisory migration lock.
- A migration failure blocks API rollout. The API never migrates automatically.
- Production migrations are forward-only. Down/up checks are limited to disposable test databases.
- Deploy immutable image digests in this order: migration command, API rollout, `/health`, authenticated read smoke test, then create/query/delete smoke test.
- Roll back the application image only when migrations are backward-compatible; otherwise deploy a forward fix.
- Rotate one token per role by updating the environment and restarting the service. Zero-downtime overlapping credentials are outside v1.
- Database backups are optional. Disaster recovery creates a fresh database, applies migrations, and reingests all datasets from retained upstream sources.

## HTTP, Security, and Observability

- Set a 5-second header timeout, 16 KiB maximum request headers, 60-second idle timeout, configured request deadline, 2-second health database timeout, and configured graceful-shutdown timeout.
- On shutdown, stop accepting requests, allow in-flight work to finish, then cancel remaining work so PostgreSQL rolls back open transactions.
- Compare bearer-token digests in constant time. Allow both tokens on read/query routes and only the read/write token on mutation routes.
- Assume inbound TLS is terminated by the chosen deployment platform or reverse proxy. Disable CORS by default.
- Emit JSON logs containing timestamp, level, request ID, method, route template, status, total duration, database duration, request/response byte counts, and mutation result.
- Never log raw paths, provider/dataset codes, credentials, database URLs, request/response bodies, source stamps, values, text values, or statuses.
- Generate a new server-controlled request ID for every request and return it through `X-Request-ID`.
- Keep `/health` public and minimal: return `200` only when the process and database are ready; otherwise return `503`.
- Serve the OpenAPI contract and embedded interactive documentation publicly at `/openapi.json` and `/docs/`. The documentation must work without a runtime CDN, must not persist entered bearer tokens, and must not expose environment-specific credentials or database details.
- Define initial alerts around repeated migration failure, sustained health failure, elevated `5xx`/`503`, database-pool exhaustion, and container restarts. No public metrics endpoint is required in v1.
