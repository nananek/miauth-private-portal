#!/usr/bin/env bash
# Runs the misskey_dart contract suite against a real local server. The
# MiAuth session is approved through the same host-local CLI operators use.
set -euo pipefail

repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$repo_root"

local_port=18080
route_session_id="contract-test-$$"
scratch_dir="$(mktemp -d)"
db_path="$scratch_dir/contract.db"
server_pid=""

cleanup() {
  if [ -n "$server_pid" ]; then
    kill "$server_pid" 2>/dev/null || true
    wait "$server_pid" 2>/dev/null || true
  fi
  rm -rf "$scratch_dir"
}
trap cleanup EXIT

echo "==> building bin/server and bin/miauthctl"
go build -o bin/server ./cmd/server
go build -o bin/miauthctl ./cmd/miauthctl

echo "==> starting bin/server on :$local_port"
APP_ENV=development \
DB_PATH="$db_path" \
HTTP_PORT="$local_port" \
LOCAL_ORIGIN="http://localhost:$local_port" \
  ./bin/server &
server_pid=$!

echo "==> waiting for /readyz"
ready=false
for _ in $(seq 1 100); do
  if curl -sf "http://localhost:$local_port/readyz" >/dev/null 2>&1; then
    ready=true
    break
  fi
  if ! kill -0 "$server_pid" 2>/dev/null; then
    echo "bin/server exited before becoming ready" >&2
    exit 1
  fi
  sleep 0.2
done
if [ "$ready" != true ]; then
  echo "bin/server did not become ready in time" >&2
  exit 1
fi

echo "==> creating and approving the MiAuth session"
curl -sS \
  "http://localhost:$local_port/miauth/$route_session_id?permission=read:account,write:notes" \
  -o /dev/null
APP_ENV=development \
DB_PATH="$db_path" \
LOCAL_ORIGIN="http://localhost:$local_port" \
  ./bin/miauthctl approve --yes "$route_session_id"

check_response="$(curl -sS -X POST "http://localhost:$local_port/api/miauth/$route_session_id/check")"
token="$(printf '%s' "$check_response" | jq -r '.token // empty')"
if [ -z "$token" ]; then
  echo "failed to obtain a local API token: $check_response" >&2
  exit 1
fi

echo "==> running dart test against http://localhost:$local_port/api/"
(
  cd contract/aria_client
  dart pub get
  TEST_API_URL="http://localhost:$local_port/api/" TEST_TOKEN="$token" \
    dart test --concurrency=1
)
