# InTouch API — guide for coding sessions

Backend for the InTouch multi-community app. Postgres (Supabase) with RLS is the
authorisation boundary; role-sensitive multi-row writes are SECURITY DEFINER
RPCs in SQL; the Go/Gin service wraps them as HTTP endpoints and calls third
parties. The OpenAPI contract in the tickets is frozen — implement it exactly.

## Stack
- Go (version in go.mod; module `github.com/intouch/api`), Gin for HTTP, pgx v5 for Postgres.
- Supabase: Postgres 17, Auth, Storage, Realtime. SQL migrations in `supabase/migrations`.

## Layout
- `cmd/api/main.go` — HTTP entry point (`PORT`, default 8080).
- `internal/server` — Gin router (`server.New()`); register routes here.
- `internal/profile` — profile field validation mirroring the DB CHECKs.
- `internal/dbtest` — DB test harness + schema/policy tests (`dbtest.New(t)`,
  `d.NewUser`, `d.As(uid, fn)` runs as role `authenticated` with that JWT sub,
  `uid==""` runs as `anon`; `SQLState(err)`).
- `supabase/migrations/*.sql` — schema; applied in filename order. Name new files
  `YYYYMMDDHHMMSS_<topic>.sql` with a later timestamp than existing ones.
- `supabase/tests/database/*.test.sql` — pgTAP suites (`supabase test db`). They
  are also executed by `go test` via `db/test/pgtap_shim.sql` (supports only
  plan, ok, is, results_eq single-column, throws_ok(sql, code, null, desc),
  lives_ok, finish) — stick to those or extend the shim.
- `db/test/supabase_stub.sql` — fake `auth` schema, `auth.uid()`, and the
  anon/authenticated/service_role roles, for plain Postgres. Test-only.

## Commands
- Unit tests: `go test ./...` (DB tests skip without `TEST_DATABASE_URL`).
- DB tests: `TEST_DATABASE_URL=postgres://postgres@localhost:5432/postgres go test -count=1 ./internal/dbtest/...`
  (superuser URL; each test creates and drops a database).
- Build: `go build -o bin/api ./cmd/api`. Run: `go run ./cmd/api`.
- A Makefile mirrors these (`make test`, `make test-db`, `make build`).
- No local Postgres? `npm pack @embedded-postgres/linux-x64@17.5.0-beta.15`
  outside the repo, recreate symlinks from `pg-symlinks.json`, set
  `LD_LIBRARY_PATH=native/lib`, then `initdb -U postgres -A trust` + `pg_ctl start`.

## Schema conventions
- Every public table: `enable row level security`; revoke all from anon and
  authenticated, then grant only what is needed (column-level `grant update (...)`
  for client-writable columns; server-set columns are never granted). Grant
  `select` to anon too, with policies `to authenticated`, so visitors read 0 rows.
  `TestRLSEnabledOnEveryPublicTable` fails if a public table lacks RLS.
- Helpers (SECURITY DEFINER, `search_path = ''`, fully qualified names):
  `is_joined(cid)`, `community_role(cid)` (null unless joined),
  `has_community_role(cid, roles[])`, `can_invite(cid)` (per invite_permission),
  `has_membership(cid)` (pending or joined), `shares_community(uid)`, `is_operator()`.
  Community-scoped tables in later tickets should key policies on `is_joined()`.
- RPC errors use SQLSTATE `PTnnn` (PostgREST maps to HTTP nnn): PT400, PT401,
  PT403, PT404, PT409, PT410, PT422. Constraint errors: 23514 check, 23505 unique
  (username taken → 409), 22001 too long, 42501 privilege/RLS.
- RPCs in place: `create_community(name, location_type, creation_key, city, visibility)`
  (idempotent on creation_key, handle slug with `-N` suffix, owner/joined/direct),
  `transfer_ownership(cid, to_user)`, `request_join(cid, answers)`,
  `review_join_request(cid, user, 'approve'|'reject')`, `open_invitation(cid, code)`
  (anon; returns jsonb preview + questions, marks opened),
  `accept_invitation(cid, code, answers)`. Answers jsonb: `[{"question_id","value"}]`.
  Required questions are enforced in `_save_join_answers`.
- Ownership: partial unique index `memberships_one_owner_key` (≤1 owner) plus
  deferred constraint triggers (≥1 owner at commit). Transfer demotes then promotes
  in one transaction. A profile that owns a live community cannot be deleted.
- `communities.member_count` is maintained by trigger on joined memberships.
- Usernames: `^[a-z0-9_.]{3,30}$`, unique index on `lower(username)`.
- Invitation codes: 32 hex chars from `gen_random_bytes(16)` (pgcrypto in `extensions`).
