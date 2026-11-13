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
  Block helpers: `blocked_user_ids()`, `is_blocked_with(uid)`, `blocked_between(a,b)` (see Social graph).
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

## Auth attempts, devices, notifications (migration `..._auth_devices_notifications.sql`)
- `auth_attempts` has NO client grants; the service calls these as `service_role`
  (all SECURITY DEFINER, execute revoked from anon/authenticated):
  `auth_otp_send(hash)` → otp_ref (PT429 < 30 s since last send);
  `auth_otp_verify_begin(hash)` → attempt id for the latest OTP (PT401 none sent,
  PT410 ≥ 10 min old, PT429 after 5 verifies); `auth_password_begin(hash)` →
  attempt id (PT423 while locked: 10 consecutive failures within 15 min, lock
  lasts 15 min after the 10th; blocked attempts are not logged);
  `auth_attempt_succeeded(id)` marks the attempt (a password success resets
  the failure count); `auth_password_failures(hash)`, `auth_password_locked_until(hash)`,
  `auth_attempts_purge(interval)`. Begin before calling Supabase Auth, mark on success.
- Hashes: `internal/authattempt` — `IdentifierHash` (sha256 of lower-cased email
  or `+91` + 10-digit mobile), `Hash` for device fingerprints. DB checks `^[0-9a-f]{64}$`.
- `known_devices`: clients select own rows by explicit columns (no `device_hash`
  grant, so `select *` fails). Service functions: `record_sign_in(user, session,
  device_hash, user_agent, alert default true)` → `{device_id,new_device,alerted}`
  (stores a `login_alert` notification when the device hash is new and
  `login_alerts` is on; the service emails when `alerted`; call it only once the
  session is issued, i.e. after the SMS code when 2FA is on);
  `forget_other_devices(user, keep_session)` → removed session ids (pair with
  Supabase sign-out scope=others); `forget_device(session)`.
- `notifications`: own rows readable, only `read_at` updatable by clients.
  Later tickets insert via `notify(user, kind, payload)` (service_role only;
  kind `^[a-z][a-z0-9_]*$`, payload a JSON object of referenced ids).
- `switcher_communities()` (authenticated): joined, live communities with role —
  pending requests excluded.
- Test helper in `internal/dbtest`: `service[T](d, sql, args...)` runs as service_role.

## Social graph (migration `..._social_graph.sql`)
- Tables `blocks`, `connections`, `follows`, `mentorship_requests`; all FKs to
  profiles `on delete cascade`, no self rows (CHECK). No client insert grants:
  inserts go through RPCs. Clients may `delete` own blocks (unblock), either
  party's connection row (withdraw/cancel/remove → Idle), own follows
  (unfollow); the recipient may `update (status)` of a mentorship request.
- `blocks` rows are visible to the blocker only (never reveal a block).
- **Block filtering — reuse in feed, people, chat:** `blocked_user_ids()`
  (setof uuid, caller's blocks either direction; e.g.
  `author_id not in (select public.blocked_user_ids())`),
  `is_blocked_with(other)` (caller, either way), and service/definer-only
  `blocked_between(a, b)` (execute revoked from clients). Chat's send/open-DM
  RPCs must reject with PT403 when `blocked_between(sender, peer)`.
- `connections.pair_key` is a generated column `least(a,b)::text || ':' ||
  greatest(a,b)::text` with a unique index; `_pair_key(a,b)` computes it in SQL.
  States pending|connected|declined; `connection_state(other)` →
  idle|sent|incoming|connected|blocked (declined reads `sent` to the requester —
  silent decline — and `idle` to the addressee; being blocked reads `idle`).
- RPCs (authenticated): `block_user(uid)` → blocks row (idempotent; deletes the
  pair's connection and both follows; PT400 self, PT404 unknown),
  `unblock_user(uid)` → bool; `send_connection_request(uid)` (PT403 block,
  PT409 pending/connected; replaces a request the caller had declined),
  `respond_to_connection(id, 'accept'|'decline'|'undo')` (addressee only:
  PT403 requester, PT404 not a party, PT409 bad transition);
  `follow_user(uid)` (idempotent; PT403 connections-only profile or block),
  `follow_counts(uid)` → (followers, following);
  `request_mentorship(uid, topic, message)` (topic
  career|system_design|referral|resume|grad_school, PT400 missing; PT404
  unknown/blocked; stores a `mentorship` notification with
  `{mentorship_request_id, requester_id}`).
- Embedded Postgres note: the `pgserver` wheel build has no pgcrypto; use the
  `@embedded-postgres` package (it ships pgcrypto).
