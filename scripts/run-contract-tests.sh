#!/usr/bin/env bash
# Runs the misskey_dart contract suite against a real local server. The
# MiAuth session is approved through the same host-local CLI operators use.
#
# Issue #13 (release gate) AC1/AC2 also needs restart-persistence
# evidence, so this script creates one note, kills and relaunches
# bin/server against the same DB_PATH, and passes that note's id/text to
# the dart suite via TEST_PRE_RESTART_NOTE_ID/TEST_PRE_RESTART_NOTE_TEXT
# (see restart_persistence_test.dart) before running the rest of the
# suite against the restarted process.
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

start_server() {
  APP_ENV=development \
  DB_PATH="$db_path" \
  HTTP_PORT="$local_port" \
  LOCAL_ORIGIN="http://localhost:$local_port" \
    ./bin/server &
  server_pid=$!
}

# wait_ready polls /readyz, failing fast (with the given label in the
# error message) if bin/server exits first or never becomes ready.
wait_ready() {
  local label="$1"
  local ready=false
  for _ in $(seq 1 100); do
    if curl -sf "http://localhost:$local_port/readyz" >/dev/null 2>&1; then
      ready=true
      break
    fi
    if ! kill -0 "$server_pid" 2>/dev/null; then
      echo "bin/server exited before becoming ready ($label)" >&2
      exit 1
    fi
    sleep 0.2
  done
  if [ "$ready" != true ]; then
    echo "bin/server did not become ready in time ($label)" >&2
    exit 1
  fi
}

echo "==> starting bin/server on :$local_port"
start_server
wait_ready "initial boot"

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

echo "==> creating a note before restart (Issue #13 AC1/AC2 restart persistence)"
pre_restart_note_text="contract-test restart persistence $$-$(date +%s)"
create_response="$(jq -n --arg text "$pre_restart_note_text" --arg token "$token" '{text: $text, i: $token}' |
  curl -sS -X POST "http://localhost:$local_port/api/notes/create" \
    -H 'Content-Type: application/json' --data-binary @-)"
pre_restart_note_id="$(printf '%s' "$create_response" | jq -r '.createdNote.id // empty')"
if [ -z "$pre_restart_note_id" ]; then
  echo "failed to create the pre-restart note: $create_response" >&2
  exit 1
fi

echo "==> restarting bin/server against the same DB_PATH"
kill "$server_pid"
wait "$server_pid" 2>/dev/null || true
server_pid=""
start_server
wait_ready "after restart"

echo "==> running dart test against http://localhost:$local_port/api/"
(
  cd contract/aria_client
  dart pub get
  TEST_API_URL="http://localhost:$local_port/api/" \
  TEST_TOKEN="$token" \
  TEST_PRE_RESTART_NOTE_ID="$pre_restart_note_id" \
  TEST_PRE_RESTART_NOTE_TEXT="$pre_restart_note_text" \
    dart test --concurrency=1
)
