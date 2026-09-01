#!/usr/bin/env bash
# Container acceptance: start the built image against a disposable PostgreSQL 18,
# run migrations, wait for health, then exercise authentication, the dataset
# lifecycle, the public documentation, and graceful shutdown.
#
# Usage: scripts/container-smoke.sh <image-reference>
#
# Everything it creates is named with the RUN_ID prefix and removed on exit.
set -euo pipefail

IMAGE="${1:?usage: container-smoke.sh <image-reference>}"
RUN_ID="smoke-$$"
NETWORK="${RUN_ID}-net"
POSTGRES="${RUN_ID}-pg"
API="${RUN_ID}-api"
CURL_IMAGE="${CURL_IMAGE:-curlimages/curl:8.11.1}"

# Synthetic credentials for a disposable container. They never reach a report.
WRITE_TOKEN="container-smoke-read-write-token-000001"
READ_TOKEN="container-smoke-read-only-token-0000002"
DATABASE_URL="postgres://storage:storage@${POSTGRES}:5432/storage?sslmode=disable"

failures=0

cleanup() {
  docker logs "$API" >"${LOG_DIR:-/tmp}/api.log" 2>&1 || true
  docker rm -f "$API" "$POSTGRES" >/dev/null 2>&1 || true
  docker network rm "$NETWORK" >/dev/null 2>&1 || true
}
trap cleanup EXIT

note() { printf '\n== %s\n' "$1"; }

# check <description> <expected-status> <curl args...>
check() {
  local description="$1" expected="$2"
  shift 2
  local actual
  actual=$(docker run --rm --network "$NETWORK" "$CURL_IMAGE" \
    -s -o /dev/null -w '%{http_code}' "$@" || echo 000)
  if [ "$actual" = "$expected" ]; then
    printf '   ok   %-46s %s\n' "$description" "$actual"
  else
    printf '   FAIL %-46s got %s, want %s\n' "$description" "$actual" "$expected"
    failures=$((failures + 1))
  fi
}

body() {
  docker run --rm --network "$NETWORK" "$CURL_IMAGE" -s "$@"
}

note "starting disposable PostgreSQL 18"
docker network create "$NETWORK" >/dev/null
docker run -d --name "$POSTGRES" --network "$NETWORK" \
  -e POSTGRES_USER=storage -e POSTGRES_PASSWORD=storage -e POSTGRES_DB=storage \
  -e POSTGRES_INITDB_ARGS="--encoding=UTF8 --locale=C" \
  postgres:18 >/dev/null
for _ in $(seq 1 60); do
  if docker exec "$POSTGRES" pg_isready -U storage -d storage >/dev/null 2>&1; then break; fi
  sleep 1
done
docker exec "$POSTGRES" pg_isready -U storage -d storage

note "the image refuses to start without valid configuration"
if docker run --rm --network "$NETWORK" -e DATABASE_URL="$DATABASE_URL" "$IMAGE" >/dev/null 2>&1; then
  echo "   FAIL the API started without credentials"
  failures=$((failures + 1))
else
  echo "   ok   missing credentials refused"
fi
if docker run --rm --network "$NETWORK" -e DATABASE_URL="$DATABASE_URL" \
  -e API_READ_WRITE_TOKEN="$WRITE_TOKEN" -e API_READ_ONLY_TOKEN="$WRITE_TOKEN" \
  "$IMAGE" >/dev/null 2>&1; then
  echo "   FAIL the API started with equal tokens"
  failures=$((failures + 1))
else
  echo "   ok   equal tokens refused"
fi
if docker run --rm --network "$NETWORK" \
  -e DATABASE_URL="postgres://storage:storage@127.0.0.1:1/none?sslmode=disable" \
  -e API_READ_WRITE_TOKEN="$WRITE_TOKEN" -e API_READ_ONLY_TOKEN="$READ_TOKEN" \
  "$IMAGE" >/dev/null 2>&1; then
  echo "   FAIL the API started without a reachable database"
  failures=$((failures + 1))
else
  echo "   ok   unreachable database refused"
fi

note "the API refuses to start before migrations run"
if docker run --rm --network "$NETWORK" -e DATABASE_URL="$DATABASE_URL" \
  -e API_READ_WRITE_TOKEN="$WRITE_TOKEN" -e API_READ_ONLY_TOKEN="$READ_TOKEN" \
  "$IMAGE" >/dev/null 2>&1; then
  echo "   FAIL the API started against an unmigrated database"
  failures=$((failures + 1))
else
  echo "   ok   unmigrated schema refused"
fi

note "running migrations with the separate command"
docker run --rm --network "$NETWORK" --entrypoint /app/migrate \
  -e DATABASE_URL="$DATABASE_URL" "$IMAGE"
echo "   ok   migrations applied"
docker run --rm --network "$NETWORK" --entrypoint /app/migrate \
  -e DATABASE_URL="$DATABASE_URL" "$IMAGE"
echo "   ok   repeated migration is a no-op"

note "image properties"
image_user=$(docker image inspect "$IMAGE" --format '{{.Config.User}}')
if [ "$image_user" = "nonroot:nonroot" ] || [ "$image_user" = "65532:65532" ]; then
  echo "   ok   runs as $image_user"
else
  echo "   FAIL image user is '$image_user', want a non-root user"
  failures=$((failures + 1))
fi
if docker run --rm --entrypoint /bin/sh "$IMAGE" -c 'exit 0' >/dev/null 2>&1; then
  echo "   FAIL the runtime image ships a shell"
  failures=$((failures + 1))
else
  echo "   ok   no shell in the runtime image"
fi

note "starting the API with a read-only root filesystem and no volumes"
docker run -d --name "$API" --network "$NETWORK" \
  --read-only --cap-drop ALL \
  -e DATABASE_URL="$DATABASE_URL" \
  -e API_READ_WRITE_TOKEN="$WRITE_TOKEN" -e API_READ_ONLY_TOKEN="$READ_TOKEN" \
  "$IMAGE" >/dev/null
ready=0
for _ in $(seq 1 60); do
  if [ "$(docker run --rm --network "$NETWORK" "$CURL_IMAGE" \
      -s -o /dev/null -w '%{http_code}' "http://${API}:8080/health" || echo 000)" = "200" ]; then
    ready=1
    break
  fi
  sleep 1
done
if [ "$ready" != "1" ]; then
  echo "   FAIL the API never became healthy"
  docker logs "$API" || true
  exit 1
fi
echo "   ok   health reports ready"

BASE="http://${API}:8080"
DATASET="${BASE}/v1/providers/SmokeProvider/datasets/SmokeDataset"
REPLACEMENT='{"source_stamp":{"run":"smoke"},"id":["sex","year"],
  "dimension":{"sex":{"index":{"M":0,"F":1}},"year":{"index":{"2024":0,"2025":1}}},
  "value":[1,2,3,4]}'
QUERY='{"id":["year","sex"],"dimension":{"year":{"index":{"2025":0}},"sex":{"index":{"F":0,"M":1}}}}'

note "authentication and authorization"
check "no credentials are rejected" 401 "${BASE}/v1/providers"
check "an invalid token is rejected" 401 -H "Authorization: Bearer not-a-real-token-not-a-real-token" "${BASE}/v1/providers"
check "the read-only token may read" 200 -H "Authorization: Bearer ${READ_TOKEN}" "${BASE}/v1/providers"
check "the read-only token may not write" 403 -X POST \
  -H "Authorization: Bearer ${READ_TOKEN}" -H 'Content-Type: application/json' \
  -d "$REPLACEMENT" "$DATASET"
check "the read-only token may not delete" 403 -X DELETE \
  -H "Authorization: Bearer ${READ_TOKEN}" "$DATASET"

note "dataset lifecycle"
check "creation returns 201" 201 -X POST \
  -H "Authorization: Bearer ${WRITE_TOKEN}" -H 'Content-Type: application/json' \
  -d "$REPLACEMENT" "$DATASET"
check "create-only conflicts" 409 -X POST \
  -H "Authorization: Bearer ${WRITE_TOKEN}" -H 'Content-Type: application/json' \
  -d "$REPLACEMENT" "$DATASET"
check "replacement returns 200" 200 -X POST \
  -H "Authorization: Bearer ${WRITE_TOKEN}" -H 'Content-Type: application/json' \
  -d "$(printf '%s' "$REPLACEMENT" | sed 's/^{/{"replace":true,/')" "$DATASET"
check "the summary is readable" 200 -H "Authorization: Bearer ${READ_TOKEN}" "$DATASET"
check "the structure is readable" 200 -H "Authorization: Bearer ${READ_TOKEN}" "${DATASET}/structure"
check "sparse data is readable" 200 -H "Authorization: Bearer ${READ_TOKEN}" "${DATASET}/data"
check "dense data is readable" 200 -H "Authorization: Bearer ${READ_TOKEN}" "${DATASET}/data?format=dense"
check "an unknown query parameter is rejected" 400 \
  -H "Authorization: Bearer ${READ_TOKEN}" "${DATASET}/data?nope=1"
check "an unsupported media type is rejected" 415 -X POST \
  -H "Authorization: Bearer ${WRITE_TOKEN}" -H 'Content-Type: text/plain' \
  -d "$REPLACEMENT" "$DATASET"

query_result=$(body -X POST -H "Authorization: Bearer ${READ_TOKEN}" \
  -H 'Content-Type: application/json' -d "$QUERY" "${DATASET}/query?format=dense")
# Payload index 0 is (M,2024) and stores at internal index 2, so the reordered
# subset (2025,F) then (2025,M) must answer with 4 then 2.
if printf '%s' "$query_result" | grep -q '"value":\[4,2\]'; then
  echo "   ok   the reordered subset preserves requested order"
else
  echo "   FAIL unexpected query result: $query_result"
  failures=$((failures + 1))
fi
if printf '%s' "$query_result" | grep -q '"cell_count":4'; then
  echo "   ok   the subset keeps whole-dataset counts"
else
  echo "   FAIL the subset lost the whole-dataset counts"
  failures=$((failures + 1))
fi

check "deletion returns 204" 204 -X DELETE -H "Authorization: Bearer ${WRITE_TOKEN}" "$DATASET"
check "deletion is idempotent" 204 -X DELETE -H "Authorization: Bearer ${WRITE_TOKEN}" "$DATASET"
check "the deleted dataset is gone" 404 -H "Authorization: Bearer ${READ_TOKEN}" "$DATASET"

note "public documentation"
check "the OpenAPI document is public" 200 "${BASE}/openapi.json"
check "/docs redirects" 308 "${BASE}/docs"
check "the documentation page is public" 200 "${BASE}/docs/"
check "the documentation bundle is embedded" 200 "${BASE}/docs/swagger-ui-bundle.js"
check "the documentation stylesheet is embedded" 200 "${BASE}/docs/swagger-ui.css"

note "the logs never carry credentials or observation content"
log_output=$(docker logs "$API" 2>&1)
for secret in "$WRITE_TOKEN" "$READ_TOKEN" "SmokeProvider" "SmokeDataset" "storage:storage"; do
  if printf '%s' "$log_output" | grep -qF "$secret"; then
    echo "   FAIL the logs leaked '$secret'"
    failures=$((failures + 1))
  fi
done
if [ "$failures" -eq 0 ]; then
  echo "   ok   logs are clean"
fi

note "graceful shutdown on SIGTERM"
docker stop --signal SIGTERM --timeout 40 "$API" >/dev/null
exit_code=$(docker inspect "$API" --format '{{.State.ExitCode}}')
if [ "$exit_code" = "0" ]; then
  echo "   ok   SIGTERM produced a clean exit"
else
  echo "   FAIL SIGTERM produced exit code $exit_code"
  failures=$((failures + 1))
fi
if docker logs "$API" 2>&1 | grep -q "shutdown complete"; then
  echo "   ok   shutdown ran to completion"
else
  echo "   FAIL the shutdown never completed"
  failures=$((failures + 1))
fi

printf '\n'
if [ "$failures" -ne 0 ]; then
  echo "container smoke test failed with $failures problem(s)"
  exit 1
fi
echo "container smoke test passed"
