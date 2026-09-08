#!/usr/bin/env bash
set -euo pipefail

if [[ -n "${TEST_DATABASE_URL:-}" ]]; then
    exec make check-db GO="${1:-go}"
fi

task_dir=$(mktemp -d /tmp/kitchen-check-XXXXXXXX)
task_project=$(basename "$task_dir" | tr '[:upper:]' '[:lower:]')
cleanup() {
    docker compose -f compose.test.yaml -p "$task_project" down -v >&2
    rmdir "$task_dir"
}
trap cleanup EXIT
trap 'exit 130' INT
trap 'exit 143' TERM
docker compose -f compose.test.yaml -p "$task_project" up -d --wait
task_address=$(docker compose -f compose.test.yaml -p "$task_project" port db 5432)
export TEST_DATABASE_URL="postgres://kitchen:kitchen@${task_address}/kitchen_test?sslmode=disable"
make check-db GO="${1:-go}"
