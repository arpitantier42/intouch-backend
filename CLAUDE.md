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
- `internal/server/me.go` — OP-007..010 (`registerMe`), migration `..._profile_me.sql`.
  Every /me handler first runs `ensure_my_profile(name, avatar)` (authenticated,
  definer; name/picture from the JWT `user_metadata` — Google's `full_name`/`name`,
  `avatar_url`/`picture`), so the first request after Google OAuth creates the
  profile. Responses are `to_jsonb(profiles row)`. PATCH: whitelisted columns
  (`parsePatch`), read-only/unknown keys ignored (incl. `phone`); chips trimmed and
  deduped case-insensitively; 23505 on `profiles_username_key` → 409 with
  `error.fields.username`. `two_factor_enabled` goes through `set_two_factor(bool)`
  (PT400 unless a profile phone or an Auth-confirmed `auth.users.phone` exists;
  copies it as `+<digits>`). Deletion: `request_account_deletion(reason)` (keeps the
  first timestamp), `cancel_account_deletion()` (PT404), and service-only
  `accounts_due_for_purge(limit)` for the scheduler (skips owners of live
  communities; the purge deletes the auth user via the Auth admin API → cascade).
  `abortFields(c, fields, order...)` takes the display order of fields.
  Google OAuth: `supabase/config.toml` `[auth.external.google]` + env
  `SUPABASE_AUTH_EXTERNAL_GOOGLE_CLIENT_ID/SECRET`; app redirect `intouch://auth-callback`.
  The test stub's `auth.users` has `phone`, `phone_confirmed_at`. Tests: `me_test.go`
  (`googleToken`, `meUser`).
- `internal/server/admission.go` — OP-016..022 (`registerAdmission`), migration
  `..._admission_roster.sql`. Handlers are thin: parse input, call one RPC as the
  user, map PTnnn. RPCs: `request_join` (handler first runs `ensure_my_profile`),
  `review_join_request`, `transfer_ownership` (identity migration), plus
  `set_member_role(cid, uid, role)` (PT400 role ∉ admin|moderator|member, checked
  first; PT403 unless caller is joined owner; PT403 target is owner; PT404 not
  joined), `remove_member(cid, uid)` (owner/admin; PT403 self, owner, or admin
  removed by a non-owner; PT404 none; also removes pending rows),
  `leave_community(cid)` (PT409 owner, PT404 none), `list_members(cid, status)` →
  jsonb array of membership rows + `role_label`, `profile{...}`, `answers[...]`
  (PT403 'Admins only'). Join answers body: list, single object, or `{"answers": [...]}`.
  Tests: `admission_test.go` (`admUser`, `admCommunity`, `e.raw` for array bodies),
  pgTAP `admission_roster.test.sql`.
- `internal/server/invitations.go` — OP-025..028 (`registerInvitations`), migration
  `..._invitations.sql`. `create_invitations(cid, channel, emails[], user_ids[],
  expires_at, max_uses)` → jsonb array (PT403 unless `can_invite`; PT400 bad
  channel/past expiry/max_uses<1/no or >100 recipients/malformed email/self/joined
  member; PT404 unknown in-app user; emails deduped case-insensitively).
  Handler validates emails first (`error.fields["emails[i]"]`, nothing created),
  answers a single object for singular bodies (`invitee_email`, `invitee_user_id`,
  link/whatsapp) and an array for `emails`/`invitee_user_ids`; email invites are
  sent after commit in a goroutine via `Deps.Mailer`, a send error calls
  service-only `mark_invitation_failed(id)` (email + still `sent` only — the
  bounce webhook should call it too). GET is public (`AsAnon`, `open_invitation`
  flattened: invitation fields + `community` + `questions`; bad uuid → 404).
  Accept runs `ensure_my_profile` then `accept_invitation` (404 unknown, 410
  unusable). `revoke_invitation(cid, code)` (PT403/PT404, keeps first revoked_at).
  Config `APP_LINK_BASE` (default `intouch://`) builds `<base>communities/<id>/invite/<code>`.
  Tests: `invitations_test.go`, pgTAP `invitations.test.sql`. `e.raw` with token ""
  sends no Authorization header.
- `internal/server/uploads.go` — OP-091 `POST /uploads?purpose&mime&bytes`
  (`registerUploads`, no DB). Rules in `internal/uploads` (`Lookup(purpose)`
  → `Policy{Bucket, MaxBytes, MIME→ext, TooLarge, BadType}`; `Check` = 415
  type first, then 413 size; `NormalizeMIME`). 400 unknown purpose / empty
  mime / bytes ≤ 0 (`error.fields`); 503 when `Deps.Storage` is nil; 502 on
  sign failure. Object path `<uid>/<purpose>/<32 hex>.<ext>`; 201 body
  `{purpose,bucket,path,mime,bytes,max_bytes,upload_url,token,method:"PUT",expires_at}`.
  `internal/storage` — `Signer` interface (`CreateSignedUploadURL`), HTTP impl
  with the service-role key; `server.New` builds it when `SUPABASE_URL` +
  `SUPABASE_SERVICE_ROLE_KEY` are set; tests pass `fakeSigner`. Buckets
  (avatars 3 MB, community-media 5, post-media 50, chat-media 25, resumes 10)
  are created by migration `..._storage_buckets.sql` (DO block, no-op without
  `storage.buckets`); `TestBucketMigrationMatchesPolicy` fails if it drifts
  from the policy — change both. Later tickets that store `avatar_url`,
  `logo_url`, `media_url`, `attachment_url`, `resume_url` can call
  `uploads.Lookup(...).Ext(mime)` to reuse the type rules (e.g. 415 for a
  non-image logo in updateCommunity, a non-PDF/DOCX resume in job interest).
- `internal/server/threads.go` — OP-046..049, 055..057 (`registerThreads`), migration
  `..._chat_threads.sql`; handlers reuse `admissionHandlers.rpcJSON`. RPCs (authenticated,
  definer): `list_threads(cid, filter, q)` → jsonb array (PT403 not joined; thread row +
  `title`, `subtitle`, `avatar_url`, `community_label`, `unread_count`, `muted_until`,
  `muted`, `member_count`, `peer`, `last_call`; hides `hidden_at`/left rows and DMs with
  a block), `open_dm(cid, uid)` (upsert on dm_key; PT400 self, PT403 not joined/block;
  clears caller's hidden_at), `create_group(cid, name, desc, uuid[])`,
  `add_participants(tid, uuid[])` (re-adding an exited member resets joined_at and sets
  cleared_at = now()), `update_thread_settings(tid, jsonb, set_disappearing, secs)`
  (keys muted_until, mute off|1h|8h|1w|always, alert_*, sound, action clear|delete|exit;
  left_at only via exit), `get_thread(tid)` (participants, member_count, peer,
  `shared_counts` {media, links, documents} over `_visible_messages(tid)`, `me`; no
  encryption field — REQ-117), `log_call(tid, media, outcome, duration, started_at)`.
  `_joined_member(cid, uid)` = joined to a live community. The send RPC (WI-022) should
  reuse `_joined_member` + `blocked_between`. Tests: `threads_test.go` (`chatJoin`,
  `chatSend` insert directly as superuser).
- `internal/server/messages.go` — OP-050..054 (`registerMessages`), migration
  `..._messaging.sql`. RPCs (authenticated, definer): `send_message(tid, scope_cid,
  kind, body, url, bytes, reply_to, forwarded, forward_of)` (PT403 not active
  participant / scope ≠ thread community / DM peer not joined / `blocked_between`
  (DMs only); PT400 content; PT413 > 25 MB; forward_of copies a visible message,
  forwarded=true), `list_messages(tid, view, q, after, before, limit)` (view=all
  without q also sets own last_read_at/last_delivered_at = now()),
  `edit_message(mid, body)` (403 → 409 → blank = unchanged), `delete_message(mid,
  'me'|'everyone')`, `mark_message(mid, action, emoji)` (pin → kind 'pin'
  pseudo-mark), `message_push(mid)` → push payload (unmuted, unblocked, joined).
  `_my_message(mid)` = visible-to-caller or PT404; `_message_json(m)` adds
  `mine,status,can_edit,starred,reactions` — reuse both. Scope header
  `scope_community_id` (also `Scope-Community-Id`). Tests: `messages_test.go`
  (`newMsgEnv`, `send`, `fakePush`), load check `messages_load_test.go`
  (skipped with `-short`), pgTAP `messaging.test.sql`.
- `internal/server/gifs.go` — OP-058 `GET /gifs?q&limit&offset` (`registerGifs`, no DB).
  `internal/gifs`: `Searcher` interface, `Giphy` HTTP impl (5 s timeout, rating `pg`,
  errors never include the key-bearing URL; `*gifs.Error` = provider status).
  `server.New` builds it when `GIF_API_KEY` is set; `Deps.Gifs` takes `fakeGifs` in tests.
  400 blank/>50-char q, limit ∉ 1..50, offset ∉ 0..4999 (provider not called);
  nil searcher or any provider error → 503 `gifUnavailable`. 200 body
  `{query, results[{id,title,url,preview_url,width,height}], next_offset|null, attribution}`.
  The send path never touches the provider (`TestTextSendingSurvivesGifOutage`).
- `internal/server/social.go` — OP-059..061, 082..085 (`registerSocial`), migration
  `..._blocks_connections.sql`. Handlers wrap `block_user`/`unblock_user`/
  `send_connection_request`/`respond_to_connection` (social graph migration) plus
  `list_blocks()` (rows + `profile` card), `delete_connection(id)` (either party;
  PT404 otherwise; no notification), `list_connections(tab)` → `{tab, counts{incoming,
  sent,connected}, items[row + peer]}` (sent rows always read `pending` — silent decline;
  blocked pairs excluded). Block/connect run `ensure_my_profile` first. Body ids:
  `blocked_id` / `addressee_id` (or `user_id`). `_profile_card(uid)` (service/definer
  only) is the small profile JSON — reuse it. `connections` is in publication
  `supabase_realtime`. Tests: `social_test.go` (`connState`).
- `internal/server/people.go` — OP-081, 086..089 (`registerPeople`), migration
  `..._people.sql`. `search_people(mode, cid, q, filter, limit)` → jsonb array of
  person cards (+ `connection_state`; city null when hidden). Modes:
  connections (scoped to joined members when cid given), members (cid required),
  suggested (no pending/connected row, shares a live community, ranked
  3×shared communities + 2 city + 2 company + shared skills), invitable (cid
  required, `can_invite`, excludes joined). Always excludes self + blocks;
  `appear_in_search=false` excluded except in connections. Filters near_me =
  same_city (profile city, hidden cities skipped); no own city/company → [].
  PT400 mode/filter/missing cid, PT403 not joined / cannot invite.
  `member_profile(uid)` → PT404 unknown, block either way, or no shared live
  community (self allowed); `limited` card for connections-only non-connections;
  `city` key omitted when hidden; adds shared_communities, mutual_connections,
  connection_state, connection_id, primary_action, can_message, follow{}.
  Helpers (service/definer only): `_shares_live_community(a, b, cid?)`,
  `_connected_ids(uid)`. Mentorship handler validates topic (400
  `error.fields.topic`), calls `request_mentorship`, pushes to the recipient.
  Follow = `follow_user`; unfollow = RLS delete on `follows` (204 always).
  Follow ships enabled server-side (DEC-010 flag is client-side).
  Tests: `people_test.go` (`setProfile`, `connect`, `hasID`), pgTAP `people.test.sql`.
- `internal/server/notifications.go` — OP-090 `GET /me/notifications`
  (`registerNotifications`, no migration): plain RLS select on `notifications`
  as the user, newest first (`created_at desc, id desc`), body = JSON array of
  rows (`[]` when empty), header `X-Unread-Count`. Optional `unread=true`,
  `limit` 1..100 (default 50), `before` (RFC 3339, keyset on created_at);
  bad values → 400 `error.fields`. Features that notify (job interest
  replies, event deletion, …) just call `notify(user, kind, payload)` and the
  row shows here. Tests: `notifications_test.go` (`addNotification`).
- `internal/server/rsvp.go` — OP-068/069 (`registerRsvp`), migration `..._rsvp.sql`.
  `set_rsvp(eid, status, payment, reason, note, push, email)` (authenticated,
  definer): locks the event `for no key update` FIRST, PT404/PT403 'Members
  only'/PT400/PT409 'Event full' (not-yet-going member when confirmed ≥ cap,
  paid pay_later included); payment null keeps the current choice, 'verified'
  is host-only (PT400), verified is sticky; null reminder flags keep the row
  value (default on). Declined/not_sure → `notify(creator, 'rsvp_response')`
  (no push). `verify_rsvp_payment(eid, uid)` (creator only PT403, free PT400,
  not going PT409, full PT409, idempotent). Both return `_rsvp_json(row,
  confirmation)` = row + badge, seats_left, seats_left_label, join_url
  (confirmed only), event{...}, confirmation_email — reuse it for event
  reads (WI-027). Handler adds `calendar{google,outlook,ics}` and, when
  `confirmation_email`, mails the JWT `email` claim (`tokenEmail`) in a
  goroutine. Tests: `rsvp_test.go` (`newRsvpEnv`, `newEvent`, `fillSeats`,
  last-seat race), pgTAP `rsvp.test.sql`.
- `internal/push` — `Sender` interface (`Send(ctx, Notification{UserIDs, Title, Body,
  Data})`); `push.Log` is the default until APNs/FCM and device-token storage
  exist. `Deps.Push`; sends run in a goroutine after commit.
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
  `accept_invitation(cid, code, answers)`, `create_invitations`, `revoke_invitation`. Answers jsonb: `[{"question_id","value"}]`.
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

## Feed, engagement and reports (migration `..._feed_engagement.sql`)
- Tables `posts`, `post_targets`, `comments`, `likes`, `poll_votes`,
  `bookmarks`, `content_filters`, `reports`. Posts/targets: NO client inserts —
  the create-post / create-event / create-raise RPCs (definer) insert them.
  Trigger on `post_targets` insert: author must be joined to a live community
  (PT403 'You can only post to communities you have joined.'), and copies the
  post's `created_at` into `post_targets.created_at` (feed keyset column).
  `posts.created_at` is immutable (update trigger); body change stamps `edited_at`.
- Post CHECKs (→ 23514 → 400): kind list; body ≥10 trimmed chars unless
  event/raise; body or media or event/raise required; media_url⇔media_kind;
  image/video/document kinds need matching media_kind; poll ⇔
  `_poll_options_ok` (2–10 non-blank strings); event⇔event_id, raise⇔raise_id;
  kind announcement ⇒ is_announcement. Moderator+-only announcements and the
  413 size checks are the RPC's job (use `uploads.Lookup`).
- Access: `can_see_post(pid)` (live post, caller joined to a live target
  community) — posts/comments select, likes/votes/bookmarks/filters insert.
  Author may `update (body, is_urgent, job_tag, pinned_to_profile)`.
  `post_targets` readable by joined members of that community and the author.
  Soft delete = set `deleted_at` (RPC, WI-017) — hides from feed, can_see_post, Saved.
- Clients write directly (column grants, `user_id`/`author_id` default
  `auth.uid()`): comments insert `(post_id, parent_id, body)` (blank → 23514;
  parent on another post → PT400); likes insert `(post_id|comment_id)` with
  `on conflict (user_id, post_id) do nothing` (or `(user_id, comment_id)`),
  delete own; poll_votes upsert `on conflict (post_id, user_id) do update set
  option_index` (trigger PT400 not a poll / out of range); bookmarks insert
  `(post_id)` / delete own (select hides deleted posts); content_filters insert
  `(kind, post_id | muted_user_id)` / delete own (= Undo). Counters
  (`like_count`, `comment_count`) by definer triggers. `poll_results(pid)` →
  jsonb int array per option.
- `bookmarks.job_id` has NO FK yet and `bookmarks_insert_own` only allows
  posts: the jobs migration (WI-008) must add the FK to `jobs` (on delete
  cascade), replace `bookmarks_insert_own` / `bookmarks_select_own` to accept
  live jobs.
- Feed: `feed_page(cid, sort newest|oldest, cursor_at, cursor_id,
  newer_than, since, limit 1..50 default 20)` → setof posts (definer; PT403
  'Members only'; PT400 sort). Keyset on (post_targets.created_at, post_id);
  `newer_than` = pull-to-refresh; excludes deleted, hidden posts, muted authors,
  blocks either way. WI-017 adds chip/type filters as extra predicates (keep
  the `post_targets_feed_idx` order by) and decorations (target count for
  'Posted to all N communities').
- Reports: insert via `create_report(kind, target_id, reason, details)` →
  jsonb without `reporter_id` (PT400 'Choose a reason.'); `list_reports(status)`
  → jsonb array (PT403 'Operators only'). `reporter_id` has no select grant
  for anyone; RLS select = `is_operator()`.
- Realtime: `posts`, `post_targets`, `comments` in `supabase_realtime` (live counts).
- Perf check `TestFeedPerformance` (dbtest, skipped with `-short`): 25k
  members, 50k posts → index scan on `post_targets_feed_idx`, ~1 ms per page.
  Tests: `feed_engagement_test.go` (`newPost` inserts as superuser, `feed`).

- Embedded Postgres note: the `pgserver` wheel build has no pgcrypto; use the
  `@embedded-postgres` package (it ships pgcrypto).
