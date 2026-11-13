#!/usr/bin/env bash
# Deploy pipeline: every endpoint ships through these steps, in order.
# Any failing step stops the deploy.
#
#   1. go vet + unit tests
#   2. database tests on a scratch Postgres (TEST_DATABASE_URL), which include
#      the RLS gate on the freshly migrated schema and the pgTAP suites
#   3. supabase db push            (SUPABASE_PROJECT_REF, SUPABASE_ACCESS_TOKEN, SUPABASE_DB_PASSWORD)
#   4. RLS gate on the target DB   (DATABASE_URL): fails if any public table has RLS off
#   5. build the container image   (IMAGE, default intouch-api:<git sha>)
#   6. roll out                    (DEPLOY_COMMAND, run with IMAGE exported)
#
# DRY_RUN=1 runs steps 1, 2 and 5 only (no remote changes).
set -euo pipefail
cd "$(dirname "$0")/.."

step() { printf '\n==> %s\n' "$*"; }
need() { for v in "$@"; do [ -n "${!v:-}" ] || { echo "deploy: $v is required" >&2; exit 2; }; done; }

step "vet and unit tests"
go vet ./...
go test ./...

step "database tests (migrations, RLS gate, policies, pgTAP)"
need TEST_DATABASE_URL
go test -count=1 ./internal/dbtest/... ./internal/db/...

if [ "${DRY_RUN:-}" != "1" ]; then
  step "apply migrations to Supabase"
  need SUPABASE_PROJECT_REF SUPABASE_ACCESS_TOKEN SUPABASE_DB_PASSWORD
  supabase link --project-ref "$SUPABASE_PROJECT_REF" --password "$SUPABASE_DB_PASSWORD"
  supabase db push --password "$SUPABASE_DB_PASSWORD"

  step "RLS gate on the deployed database"
  need DATABASE_URL
  go run ./cmd/rlscheck "$DATABASE_URL"
fi

IMAGE="${IMAGE:-intouch-api:$(git rev-parse --short HEAD 2>/dev/null || echo dev)}"
export IMAGE
step "build image $IMAGE"
if command -v docker >/dev/null 2>&1; then
  docker build -t "$IMAGE" .
else
  echo "docker not found; building the binary only"
  go build -o bin/api ./cmd/api
fi

if [ "${DRY_RUN:-}" != "1" ]; then
  step "roll out"
  need DEPLOY_COMMAND
  bash -c "$DEPLOY_COMMAND"
fi
step "done"
