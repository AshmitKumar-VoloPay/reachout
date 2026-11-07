#!/usr/bin/env bash
# Starts reachout-server against the mock Linked API and runs the client's live check against it.
# Needs: Go, Node, and MONGODB_URI (a replica set). Uses a throwaway database and drops it afterwards
# (the drop needs mongosh on PATH; without it the database is left behind).
# Nothing here contacts LinkedIn, and the worker is switched off.
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
DB="reachout_live_$$"
API_PORT="${API_PORT:-18088}"
MOCK_PORT="${MOCK_PORT:-18090}"
TMP="$(mktemp -d)"
PIDS=()

cleanup() {
  for p in "${PIDS[@]:-}"; do kill "$p" 2>/dev/null || true; done
  wait 2>/dev/null || true
  mongosh "$MONGODB_URI" --quiet --eval "db.getSiblingDB('$DB').dropDatabase()" >/dev/null 2>&1 || true
  rm -rf "$TMP"
}
trap cleanup EXIT

: "${MONGODB_URI:?set MONGODB_URI to a replica-set connection string}"
cd "$ROOT/reachout-server"
go build -o "$TMP/server" ./cmd/server
go build -o "$TMP/mock" ./cmd/mocklinkedapi

"$TMP/mock" -addr ":$MOCK_PORT" >"$TMP/mock.log" 2>&1 & PIDS+=($!)
MONGODB_DB="$DB" ADDR=":$API_PORT" \
  LINKED_API_BASE_URL="http://localhost:$MOCK_PORT" LINKED_API_TOKEN=live-check \
  TOKEN_ENCRYPTION_KEY="$(openssl rand -base64 32)" WORKER_ENABLED=false \
  "$TMP/server" >"$TMP/server.log" 2>&1 & PIDS+=($!)

for _ in $(seq 1 50); do
  curl -sf "http://localhost:$API_PORT/healthz" >/dev/null && break
  sleep 0.2
done
curl -sf "http://localhost:$API_PORT/healthz" >/dev/null || { echo "server did not start:"; cat "$TMP/server.log"; exit 1; }

cd "$ROOT/reachout-client"
LIVE_API_URL="http://localhost:$API_PORT" npm run --silent test:live
