#!/usr/bin/env bash
# run-contract-tests.sh drives Issue #7's misskey_dart contract test
# suite (contract/aria_client) against a real bin/server instance,
# substituting for an unautomated Aria v1.5.11 end-to-end run (see
# fetch_doc key='plan' section 9 and docs/compat/aria-v1.5.11.md's
# "Issue #7 implementation notes").
#
# It:
#   1. Builds bin/server and bin/fakemisskey (a test-only stand-in for
#      the upstream Misskey instance MiAuth verifies against).
#   2. Starts both against a scratch SQLite database, with
#      IDENTITY_ORIGIN pointed at fakemisskey.
#   3. Drives a real MiAuth HTTP round trip (GET /miauth/{session}
#      followed by /api/miauth/{session}/check) to obtain one local API
#      token, exactly the way Aria does.
#   4. Runs `dart test` in contract/aria_client with that token.
#
# Requires: go, dart, curl, jq. Everything here is local-only —
# fakemisskey never makes an outbound network call — so no real
# credentials or network access are required (AGENTS.md: tests must not
# require live credentials or network access).
set -euo pipefail

repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$repo_root"

local_port=18080
identity_port=18081
route_session_id="contract-test-$$"
fixed_user_id="contract-test-owner"

scratch_dir="$(mktemp -d)"
db_path="$scratch_dir/contract.db"

server_pid=""
fake_pid=""
cleanup() {
  # wait, not just kill: bin/server honors HTTP_SHUTDOWN_GRACE_PERIOD and
  # exits asynchronously, so without waiting this function (and the
  # script) can return while it is still shutting down — a problem for a
  # caller that immediately re-runs this script and hits the still-bound
  # port.
  if [ -n "$server_pid" ]; then
    kill "$server_pid" 2>/dev/null || true
    wait "$server_pid" 2>/dev/null || true
  fi
  if [ -n "$fake_pid" ]; then
    kill "$fake_pid" 2>/dev/null || true
    wait "$fake_pid" 2>/dev/null || true
  fi
  rm -rf "$scratch_dir"
}
trap cleanup EXIT

echo "==> building bin/server and bin/fakemisskey"
go build -o bin/server ./cmd/server
go build -o bin/fakemisskey ./cmd/fakemisskey

echo "==> starting fakemisskey on :$identity_port"
./bin/fakemisskey -addr ":$identity_port" -fixed-user-id "$fixed_user_id" &
fake_pid=$!

echo "==> starting bin/server on :$local_port"
# ARIA_CLIENT_CALLBACKS and OWNER_DISPLAY_NAME are intentionally left
# unset, not set to an empty string: config.Load rejects an
# explicitly-empty optional value and requires the variable be unset
# instead (internal/config/config.go's parseOptionalString/validate).
APP_ENV=development \
DB_PATH="$db_path" \
HTTP_PORT="$local_port" \
LOCAL_ORIGIN="http://localhost:$local_port" \
IDENTITY_ORIGIN="http://localhost:$identity_port" \
ALLOWED_MISSKEY_USER_ID="$fixed_user_id" \
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

echo "==> driving the pseudo-MiAuth flow (permission=read:account,write:notes)"
# -L follows the two-hop redirect this service -> fakemisskey ->
# this service's /miauth/callback performs; the real
# internal/provider/misskey.Client.Check call to fakemisskey happens
# inside that callback, exactly as it would against a real Misskey.
curl -sS -L \
  "http://localhost:$local_port/miauth/$route_session_id?permission=read:account,write:notes" \
  -o /dev/null

check_response="$(curl -sS -X POST "http://localhost:$local_port/api/miauth/$route_session_id/check")"
token="$(printf '%s' "$check_response" | jq -r '.token // empty')"
if [ -z "$token" ]; then
  echo "failed to obtain a local API token: $check_response" >&2
  exit 1
fi

echo "==> running dart test against http://localhost:$local_port/api/"
# --concurrency=1: every test file shares the one running server/database
# instance, so file-level test runs must not interleave (each test
# reasons about its own before/after deltas, e.g. i_test.dart's
# notesCount assertion, which is only reliable without concurrent
# mutation from another file).
(
  cd contract/aria_client
  dart pub get
  TEST_API_URL="http://localhost:$local_port/api/" TEST_TOKEN="$token" \
    dart test --concurrency=1
)
