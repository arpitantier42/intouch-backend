# InTouch API — guide for coding sessions

Backend for the InTouch multi-community app. Postgres (Supabase) with RLS is the
authorisation boundary; role-sensitive multi-row writes are SECURITY DEFINER
RPCs in SQL; the Go/Gin service wraps them as HTTP endpoints and calls third
parties. The OpenAPI contract in the tickets is frozen — implement it exactly.

## Stack
- Go (version in go.mod; module `github.com/intouch/api`), Gin for HTTP, pgx v5 for Postgres.
- Supabase: Postgres 17, Auth, Storage, Realtime. SQL migrations in `supabase/migrations`.

## Layout
- `cmd/api/main.go` — HTTP entry point: `config.Load()`, opens `db.Open(DATABASE_URL)`,
  `server.New(server.Deps{...})`. `APP_ENV=production` makes missing settings fatal.
- `cmd/rlscheck` — deploy gate: exits 1 if any public table has RLS off (`internal/rlsgate`).
- `internal/server` — Gin router. `server.New(deps)` builds route groups and calls
  `register(g Groups, d Deps)`: add endpoints there (or a `registerX` it calls) on
  `g.Public` (no auth; a sent bearer must still be valid), `g.User` (bearer required → 401),
  `g.Internal` (`/internal`, `X-API-Key` = `WEBHOOK_API_KEY`). `server.New()` with no deps still works.
- `internal/config` — every env setting/provider secret (`Config`, `Validate`,
  `PaymentsEnabled`; `String()` redacts). Add new secrets here and to `.env.example`.
- `internal/auth` — HS256 Supabase access-token check (`Verifier.Verify`: signature,
  exp, role=authenticated, aud contains authenticated), middleware `RequireUser`,
  `OptionalUser`, `RequireAPIKey`; `auth.FromContext(c)` → `*Claims` (`Subject`,
  `SessionID`, `Raw` payload). `auth.Sign(secret, claims)` mints tokens for tests.
- `internal/db` — **every request query runs through** `d.AsUser(ctx, claims.Raw, fn)`
  (role `authenticated`, `request.jwt.claims` = the JWT, so RLS/`auth.uid()` decide),
  `d.AsAnon` (public endpoints) or `d.AsService` (service_role functions: auth
  wrappers, webhooks). Never query the pool directly in handlers: the pool login bypasses RLS.
- `internal/apierr` — shared error mapping and body `{"error":{"status","code","message"}}`.
  `apierr.Write(c, err)`: PTnnn → nnn with the RPC message; 23514/23502/22001/22P02/… → 400;
  23505 → 409; 23503 → 404; 42501 → 403; 40001/40P01 → 409; pgx.ErrNoRows → 404;
  `*apierr.Error` (`apierr.New(415, "...")`) → its own status; else 500 (generic text).
  `apierr.Abort(c, status, msg)` for handler-level validation.
- `scripts/deploy.sh` — deploy pipeline (vet/unit → DB tests → `supabase db push` →
  rlscheck on the deployed DB → docker build → `DEPLOY_COMMAND`); `DRY_RUN=1` is local-only.
  `Dockerfile` builds `api` + `rlscheck` (distroless). CI (`.github/workflows`) is not in
  the repo: a person adds a job that runs `make deploy-dry-run` with a Postgres service.
- `internal/profile` — profile field validation mirroring the DB CHECKs; `signup.go`:
  `SignUp.Validate()` (field → client message), `ValidateEmail`, `PasswordLength`,
  `ValidateNewPassword` (8–72 + upper/lower/digit — use it for setPassword, OP-005).
- `internal/gotrue` — Supabase Auth REST client behind interface `gotrue.Client`
  (SignUp, Resend, Recover, SendSMS, Verify); `gotrue.StatusOf(err)` = Auth HTTP status
  (0 = transport). `server.Deps.Auth` takes a fake in tests; `server.New` builds
  `gotrue.New(SUPABASE_URL, SUPABASE_ANON_KEY)` when nil and the URL is set.
- `internal/server/auth_otp.go` — OP-001..003 (`registerAuthOTP`). Patterns to reuse
  (sign-in, password): sends run `auth_otp_send` and the Auth call in ONE service tx
  (`throttledSend`) so a failed send rolls back the cooldown; verify commits
  `auth_otp_verify_begin` first (counts even wrong codes), then Auth verify, then
  `auth_attempt_succeeded` + `ensure_profile` + `record_sign_in` (session id from the
  issued token's `session_id` claim via `Deps.Verifier`). Auth 4xx on send = swallowed
  (identical response), 429 → 429, else 502; Auth 4xx on verify → 401
  'Invalid code. Try again.'. Verify 200 = Profile JSON + `session` object.
  Field errors: 400 body `error.fields` {field: message}, `error.message` = first.
  Device fingerprint = `X-Device-Id` header, else User-Agent (hashed).
  Endpoint tests: `auth_otp_test.go` (`fakeAuth`, `newOTPEnv`, `age()` to backdate attempts).
- `internal/server/auth_sessions.go` — OP-004/005/006/011 (`registerAuthSessions`).
  Sign-in: `checkPassword` runs `auth_password_begin` + Auth password grant in ONE
  service tx (wrong password commits the failure → 401 generic; success marks the
  attempt → count resets; Auth 5xx rolls back → 502; PT423 → 423). Then
  `ensure_profile`, `sign_in_settings`; 2FA on → revoke the password session (Auth
  logout local + `revoke_session`), SMS via `throttledSend`, 202 with `identifier`
  (client finishes with verifyOtp purpose=two_factor); else `record_sign_in` and,
  when `alerted`, `Deps.Mailer` email in a goroutine. setPassword: policy 400 →
  current password via `checkPassword` (skipped when the JWT `amr` has `recovery`,
  `Claims.HasAMR`) → `UpdatePassword` → `revoke(..., "others")`. Sign-out:
  `revoke()` = Auth logout first (5xx → 502, 4xx ignored), then the DB.
  Tests: `auth_sessions_test.go` (`newSessEnv`, `fakeMailer`, fake `token()`).
- **Session revocation** (migration `..._session_revocation.sql`): `RequireUser`
  calls `Verifier.Revoked` (wired by `server.New` to `session_revoked(user,
  session, iat)` when `Deps.DB` is set; DB error → 503). `revoke_session(user, sid)`
  (local) and `revoke_other_sessions(user, keep_sid)` (rejects tokens with
  iat < now() except the kept session; forgets other devices). Any later
  feature that must kill sessions (account deletion) should call these.
  Tables `revoked_sessions`, `session_revocations` have no client grants.
- `internal/mailer` — `Mailer` interface (`Send(ctx, Message)`); `mailer.Log` is the
  default until a provider is wired (logs domain + subject only). `Deps.Mailer`.
- `internal/gotrue` also has `SignInPassword`, `UpdatePassword(token, pw)`,
  `Logout(token, scope)`; fakes must implement them.
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
- DB tests: `TEST_DATABASE_URL=postgres://postgres@localhost:5432/postgres go test -count=1 ./internal/dbtest/... ./internal/db/...`
  (superuser URL; each test creates and drops a database). Packages run in
  parallel, so the stub tolerates concurrent role creation. Endpoint HTTP tests
  can use `dbtest.New(t)` + `&db.DB{Pool: td.Pool}` + `auth.Sign` (see `internal/db/db_test.go`).
- Deploy: `make deploy` / `make deploy-dry-run`; gate alone: `make rls-check DATABASE_URL=...`.
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
- `ensure_profile(user, full_name)` (migration `..._otp_auth.sql`, service_role only):
  creates the profile on first OTP verification (idempotent; invalid name →
  'New member'; PT404 unknown auth user), returns the `profiles` row.
- The pgTAP shim grants execute only on its own functions (it used to grant every
  public function to PUBLIC, which masked execute revokes) — `throws_ok(..., '42501')`
  on a revoked RPC now works in pgTAP files.
- Supabase Auth settings the wrappers rely on are in `supabase/config.toml` `[auth*]`
  (OTP length 6, expiry 600 s, 30 s frequency, phone sign-up off, rate limits as
  backstop). Email templates must render `{{ .Token }}`.

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
## Chat (migration `..._chat.sql`)
- Tables `threads`, `thread_participants`, `messages`, `message_marks`,
  `call_logs`. No client inserts on any of them: the chat RPCs (later tickets,
  SECURITY DEFINER) insert threads/participants/messages/marks/call logs and
  set `pinned_at`/`edited_at`/`deleted_at`. Clients may only
  `update (last_delivered_at, last_read_at, muted_until, alert_*, sound,
  cleared_at, hidden_at)` of their own participant row.
- `dm_key` = `_dm_key(community, a, b)` = `<community>:<_pair_key(a,b)>`
  (110 chars, unique) — open-DM RPC upserts on it. Groups: `name` required,
  `dm_key` null (CHECK `threads_dm_shape`).
- Access helper: `is_thread_participant(tid)` (caller) /
  `is_active_participant(tid, uid)` (service/definer only): participant row
  with `left_at is null` AND joined to the thread's live community. Every chat
  policy uses it. Send RPCs must also check `blocked_between`.
- Triggers do the bookkeeping, so RPCs just insert/update:
  insert message → `expires_at` from `threads.disappearing_seconds`, thread
  `last_message_at`/`last_message_preview` updated (list order =
  `coalesce(last_message_at, created_at) desc`), others' `unread_count`+1 and
  `hidden_at` cleared, sender's unread 0 and `last_read_at` = message time.
  Advancing `last_read_at` recomputes `unread_count` (marking read = update
  `last_read_at`); read/delivered marks never move backwards.
  Setting `messages.deleted_at` tombstones (body/attachment/reply/pin nulled,
  star+reaction marks deleted, preview 'This message was deleted'); a
  tombstone cannot be undone.
- Message RLS hides expired rows, rows older than my `cleared_at`, and rows I
  marked `hidden_for_me` (Delete for me = insert that mark). Marks: own
  marks visible to me only (stars private); reactions visible to participants.
  One mark per (message, user, kind) — re-react = update emoji.
- CHECKs: whitespace-only body (`body ~ '\S'`) → 23514; text needs body,
  media kinds need `attachment_url`; `attachment_bytes` ≤ 26214400;
  `disappearing_seconds` in 86400|604800|7776000; call `duration_seconds` only
  for `completed`.
- `message_status(mid)` → sent|delivered|read for the sender's ticks (read =
  every other active participant's `last_read_at` ≥ created_at).
- Realtime: all five tables are in publication `supabase_realtime` (created
  if missing on plain Postgres); Realtime applies the RLS per subscriber.
  Reconnect catch-up is `created_at > after` on messages.

## Events and raises (migration `..._events_raises.sql`)
- Tables `events`, `rsvps`, `reminders`, `raises`, `contributions`. No client
  inserts on events/rsvps/raises/contributions: the RPCs (later tickets,
  SECURITY DEFINER or service_role) insert them. CHECK helpers
  `_is_http_url(text)`, `_is_string_array(jsonb)` — reuse for jobs' apply_url.
- `events`: CHECKs = publish rules (non-blank title, ends > starts, in_person
  needs venue, online needs http(s) join_url, seat_cap > 0, paid ⇒ price > 0
  and http(s) payment_url, free ⇒ both null; `events_seats_check`
  confirmed_count ≤ seat_cap) → 23514 (map to 400). Select = joined members,
  not deleted, **column-granted without `join_url`** (no `select *`); read it
  via `event_join_url(id)` (creator or confirmed seat). Creator may `update`
  the client columns; creator_id/community/confirmed_count/deleted_at/
  created_at are server-set. `seats_left(id)` → "N seats left" (null = unlimited).
  `can_see_event(id)` is the access helper.
- `rsvps` (unique event,user): just insert/upsert `status`, `payment_status`,
  prefs; triggers derive `seat_confirmed` (going and free, or verified), force
  payment_status `none` on free events (`pay_later` for paid going without
  one), keep `confirmed_count` and raise **PT409 'Event full'** (AFTER trigger
  locks the event row `for no key update`; upserts are safe). Deleted event →
  PT404. Verify payment = `update rsvps set payment_status='verified'`.
- Reminders: a going RSVP schedules push at start−1 day and −1 hour
  (`remind_push`) and a day-of email at 08:00 IST (or start−1 h if earlier)
  (`remind_email`); unique (user,event,channel,send_at); leaving going or
  turning a pref off removes pending ones; changing `starts_at` reschedules.
  Clients insert own "Remind me" rows (`event_id, channel, send_at`; user_id
  defaults to auth.uid()) and delete pending ones. Scheduler:
  `claim_due_reminders(limit)` (service_role) marks due rows sent and returns them.
- `raises`: base table denied to clients (restrictive `using (false)` policy,
  no grants). Clients read `raises_masked` (security_barrier view, owner
  rights, filters `can_see_raises(community)` = joined): `upi_id`,
  `account_number`, `ifsc`, `qr_url` null unless `can_view_payout(raise)`
  (organiser or owner/admin); plus `payout_visible`. Trigger normalises IFSC
  to upper case, derives `account_last4`, zeroes totals on insert, and rejects
  a past `open_until` with 23514. upi ⇒ upi_id only; bank ⇒ account+IFSC only;
  IFSC `^[A-Z0-9]{11}$` (too long → 22001).
- `contributions`: unique `gateway_payment_id`. Webhook calls
  `capture_contribution(raise, donor, amount, payment_id)` (service_role) →
  `(contribution_id, applied)`; totals move once per capture (trigger),
  `donor_count` counts distinct donors. Send the receipt only when `applied`,
  then `mark_receipt_sent(id)`. Donor and organiser/admins can select.

- Embedded Postgres note: the `pgserver` wheel build has no pgcrypto; use the
  `@embedded-postgres` package (it ships pgcrypto).
