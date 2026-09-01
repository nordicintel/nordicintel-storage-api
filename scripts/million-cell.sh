#!/usr/bin/env bash
# Million-cell release gate: run the built image against PostgreSQL 18 under the
# intended production resource allocation, drive a fully populated one-million
# cell dataset through every documented shape, and record the measurements.
#
# Usage: scripts/million-cell.sh <image-reference> [report-directory]
#
# The report directory is interpreted relative to the repository root, because
# the driver runs in a container with the repository mounted at /src.
#
# Environment:
#   API_CPUS, API_MEMORY  resource limits for the API container   (default 1 / 1g)
#   PG_CPUS,  PG_MEMORY   resource limits for PostgreSQL          (default 2 / 2g)
#   SIDE                  length of each of the three dimensions  (default 100)
set -euo pipefail

IMAGE="${1:?usage: million-cell.sh <image-reference> [report-directory]}"
REPORT_DIR="${2:-reports}"
case "$REPORT_DIR" in
  /*) echo "the report directory must be relative to the repository root" >&2; exit 2 ;;
esac
API_CPUS="${API_CPUS:-1}"
API_MEMORY="${API_MEMORY:-1g}"
PG_CPUS="${PG_CPUS:-2}"
PG_MEMORY="${PG_MEMORY:-2g}"
SIDE="${SIDE:-100}"
GO_IMAGE="${GO_IMAGE:-golang:1.27}"

RUN_ID="million-$$"
NETWORK="${RUN_ID}-net"
POSTGRES="${RUN_ID}-pg"
API="${RUN_ID}-api"

WRITE_TOKEN="million-cell-gate-read-write-token-0001"
READ_TOKEN="million-cell-gate-read-only-token-00002"
DATABASE_URL="postgres://storage:storage@${POSTGRES}:5432/storage?sslmode=disable"

mkdir -p "$REPORT_DIR"
SAMPLES="${REPORT_DIR}/memory-samples.txt"
SIZE_SAMPLES="${REPORT_DIR}/size-samples.txt"
: >"$SAMPLES"
: >"$SIZE_SAMPLES"
sampler_pid=""

cleanup() {
  [ -n "$sampler_pid" ] && kill "$sampler_pid" 2>/dev/null || true
  docker logs "$API" >"${REPORT_DIR}/api.log" 2>&1 || true
  docker rm -f "$API" "$POSTGRES" >/dev/null 2>&1 || true
  docker network rm "$NETWORK" >/dev/null 2>&1 || true
}
trap cleanup EXIT

echo "== starting PostgreSQL 18 with ${PG_CPUS} vCPU / ${PG_MEMORY}"
docker network create "$NETWORK" >/dev/null
docker run -d --name "$POSTGRES" --network "$NETWORK" \
  --cpus "$PG_CPUS" --memory "$PG_MEMORY" \
  -e POSTGRES_USER=storage -e POSTGRES_PASSWORD=storage -e POSTGRES_DB=storage \
  -e POSTGRES_INITDB_ARGS="--encoding=UTF8 --locale=C" \
  postgres:18 >/dev/null
for _ in $(seq 1 60); do
  docker exec "$POSTGRES" pg_isready -U storage -d storage >/dev/null 2>&1 && break
  sleep 1
done
docker exec "$POSTGRES" pg_isready -U storage -d storage

echo "== migrating"
docker run --rm --network "$NETWORK" --entrypoint /app/migrate \
  -e DATABASE_URL="$DATABASE_URL" "$IMAGE"

echo "== starting the API with ${API_CPUS} vCPU / ${API_MEMORY}"
docker run -d --name "$API" --network "$NETWORK" \
  --cpus "$API_CPUS" --memory "$API_MEMORY" --read-only --cap-drop ALL \
  -e DATABASE_URL="$DATABASE_URL" \
  -e API_READ_WRITE_TOKEN="$WRITE_TOKEN" -e API_READ_ONLY_TOKEN="$READ_TOKEN" \
  "$IMAGE" >/dev/null
ready=0
for _ in $(seq 1 60); do
  if docker run --rm --network "$NETWORK" curlimages/curl:8.11.1 \
      -sf -o /dev/null "http://${API}:8080/health" 2>/dev/null; then
    ready=1
    break
  fi
  sleep 1
done
if [ "$ready" != "1" ]; then
  echo "the API never became healthy"
  docker logs "$API" || true
  exit 1
fi

echo "== sampling container memory and database size"
# Sizes are sampled while the gate runs because the driver deletes the dataset
# at the end; the peak sample therefore describes the fully populated database.
# pg_total_relation_size on a partitioned parent reports only the parent's own
# (empty) storage, so the observation total sums the 32 partitions.
sample_sizes() {
  docker exec "$POSTGRES" psql -U storage -d storage -tAc \
    "select pg_database_size('storage') || '|' || coalesce((
       select sum(pg_total_relation_size(i.inhrelid))
       from pg_inherits i
       where i.inhparent = 'storage.observations'::regclass), 0)" \
    2>/dev/null | tr -d '[:blank:]'
}
( while true; do
    docker stats --no-stream --format '{{.Name}} {{.MemUsage}}' "$API" "$POSTGRES" 2>/dev/null >>"$SAMPLES"
    sample_sizes >>"$SIZE_SAMPLES" || true
    sleep 2
  done ) &
sampler_pid=$!

echo "== driving the gate"
set +e
docker run --rm --network "$NETWORK" \
  -v "$(pwd):/src" -w /src "$GO_IMAGE" \
  go run ./tools/millioncell \
    -base-url "http://${API}:8080" \
    -write-token "$WRITE_TOKEN" \
    -read-token "$READ_TOKEN" \
    -side "$SIDE" \
    -report "${REPORT_DIR}/million-cell-report.json"
gate_status=$?
set -e

kill "$sampler_pid" 2>/dev/null || true
sampler_pid=""

echo "== recording resource usage"
DATABASE_BYTES=$(awk -F'|' '{ if ($1 + 0 > max) max = $1 + 0 } END { printf "%d", max }' "$SIZE_SAMPLES")
OBSERVATION_BYTES=$(awk -F'|' '{ if ($2 + 0 > max) max = $2 + 0 } END { printf "%d", max }' "$SIZE_SAMPLES")


peak() {
  awk -v want="$1" '$1 == want {
      value = $2
      unit = "MiB"
      if (value ~ /GiB/) unit = "GiB"
      if (value ~ /KiB/) unit = "KiB"
      gsub(/[A-Za-z]+/, "", value)
      if (unit == "GiB") value = value * 1024
      else if (unit == "KiB") value = value / 1024
      if (value + 0 > max) max = value + 0
    } END { printf "%.1f", max }' "$SAMPLES"
}
API_PEAK_MIB=$(peak "$API")
PG_PEAK_MIB=$(peak "$POSTGRES")

# Fail the gate if the API was killed for exceeding its memory limit.
if docker inspect "$API" --format '{{.State.OOMKilled}}' | grep -qi true; then
  echo "the API container was OOM killed"
  gate_status=1
fi
if [ "$(docker inspect "$API" --format '{{.State.Running}}')" != "true" ]; then
  echo "the API container is no longer running"
  docker logs "$API" | tail -30 || true
  gate_status=1
fi

cat >"${REPORT_DIR}/resources.json" <<JSON
{
  "api_cpus": "${API_CPUS}",
  "api_memory_limit": "${API_MEMORY}",
  "api_peak_memory_mib": ${API_PEAK_MIB:-0},
  "postgres_cpus": "${PG_CPUS}",
  "postgres_memory_limit": "${PG_MEMORY}",
  "postgres_peak_memory_mib": ${PG_PEAK_MIB:-0},
  "database_size_bytes": ${DATABASE_BYTES:-0},
  "observations_relation_bytes": ${OBSERVATION_BYTES:-0},
  "dimension_side": ${SIDE}
}
JSON

echo
echo "== resources"
cat "${REPORT_DIR}/resources.json"

if [ "$gate_status" -ne 0 ]; then
  echo
  echo "million-cell gate FAILED"
  exit "$gate_status"
fi
echo
echo "million-cell gate passed"
