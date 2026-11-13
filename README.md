# InTouch API

Backend for InTouch, a mobile app where one account can belong to many
communities, and each community's content is private to its joined members.

This repository holds:

- the database schema (Supabase Postgres) for profiles, communities,
  memberships with per-community roles (owner, admin, moderator, member),
  join questions and answers, and invitation codes — with row level security
  so a user only sees the communities and rosters they belong to;
- database functions for creating a community (the creator becomes its owner
  in the same transaction), transferring ownership, requesting to join,
  reviewing join requests, and opening and accepting invitations (expiry,
  maximum uses, revocation and open/accept tracking);
- sign-in security: a log of OTP sends, OTP verifies and password sign-ins
  (stored only as hashes of the email or phone) with database functions that
  enforce a 30-second resend wait, at most 5 verifies per code, 10-minute
  code validity, and a 15-minute lockout after 10 failed passwords in
  15 minutes (a successful sign-in resets the count);
- the list of devices each user is signed in from, with new-device login
  alerts and "sign out of all other devices";
- the in-app notifications inbox behind the bell, and the list of joined
  communities shown in the community switcher (pending requests excluded);
- the social graph: connection requests (one row per pair of people, with
  accept, ignore, undo and withdraw), blocking (which removes any connection
  and follows and leaves the pair unconnected after an unblock, and is never
  revealed to the blocked person), a blocked-accounts list, one-way follows of
  public profiles with follower and following counts, and mentorship requests
  (a topic is required; the recipient gets a notification);
- the chat data store: direct and group conversations scoped to one community
  (the same two people have a separate DM in each community they share),
  participants with unread counts, read and delivered marks, mute, alert,
  clear and delete-chat settings, messages (whitespace-only text rejected,
  disappearing messages, delete for me and delete for everyone, pins), private
  stars, shared reactions and a call log. Only participants who are still
  joined to the community can read a conversation, and new rows are published
  to Supabase Realtime for live delivery;
- the events and fundraising data store: community events (a paid event needs
  a price and an http(s) payment link; the join link is shown only to the
  host and to confirmed attendees), RSVPs with seat-cap enforcement (the last
  seat goes to exactly one person; "Event full" after that), reminders
  scheduled from RSVP preferences (push 1 day and 1 hour before, a day-of
  email), raises with UPI or bank payout details that only the organiser and
  community admins can read (members see the last 4 account digits), and
  contributions recorded once per gateway payment, so a repeated payment
  webhook never counts twice;
- the feed and engagement data store: one post published to one or more of
  the author's joined communities (text 10–1200 characters, at most one
  media item, polls with 2–10 options), comments with replies, likes with
  live counts (liking twice counts once), one replaceable poll vote per
  member, bookmarks (deleted posts drop out of Saved), per-viewer hidden
  posts and muted authors, and abuse reports that only platform operators
  can read and that never reveal who filed them. Feed pages are read 20 at
  a time by a keyset cursor, newest or oldest first, with a "newer than"
  read for pull-to-refresh, and leave out hidden posts, muted authors and
  blocked users;
- account creation with one-time codes, in front of Supabase Auth:
  - `POST /auth/sign-up` checks the form (name 2–80 characters, email up to
    255, password 8–72, matching confirmation) and has Supabase Auth email a
    6-digit code. An email that cannot be registered gets a generic 422 that
    says nothing about the account. A failed email send returns 502, and
    the 30-second resend wait does not start, so the app can offer a retry.
  - `POST /auth/otp?purpose=signup|recovery|phone|two_factor&identifier=...`
    sends or resends a code by email, or by SMS to a 10-digit Indian mobile
    number. It answers 429 within 30 seconds of the previous send, and gives
    the same answer whether or not the account exists.
  - `POST /auth/otp/verify?purpose=...&identifier=...&code=123456` checks the
    code. A wrong code gets 401 "Invalid code. Try again.", a code older than
    10 minutes gets 410, and the 6th try on one code gets 429. A correct code
    returns the profile, which is created on the first verification with
    `onboarding_complete: false`, plus a `session` object (access and refresh
    token). It also records the device;
- password sign-in and session management:
  - `POST /auth/sign-in?email=...` with `{"password": "..."}` in the body.
    A wrong password and an unknown email get the same 401 "Incorrect email
    or password.". After 10 failures in a row within 15 minutes the account
    is locked (423) for 15 minutes; a successful sign-in resets the count.
    Without two-factor sign-in the answer is 200 with the profile and a
    `session`. With it, the answer is 202 (`two_factor_required`, the phone
    `identifier`) and an SMS code is sent, which the app confirms with
    `POST /auth/otp/verify?purpose=two_factor`. Signing in from a device the
    account has not used before stores a `login_alert` notification and
    emails an alert when the user has login alerts on.
  - `POST /auth/password` with `{"current_password", "new_password"}` (a
    session issued by a password-recovery code does not need the current
    password). The new password needs 8–72 characters with upper- and
    lower-case letters and a digit (400 otherwise); a wrong current password
    gets 401. On success (204) every other session is signed out.
  - `POST /auth/sign-out?scope=local|others` signs out this session or every
    other one (204). Signed-out access tokens are rejected by the API at
    once (401), not only when they expire.
  - `GET /me/devices` lists the devices the user is signed in on, with
    `current: true` on the caller's;
- the notifications inbox (bearer): `GET /me/notifications` lists the
  user's own notifications, newest first (`id`, `user_id`, `kind`, `payload`
  with the referenced ids, `read_at` — null while unread — and `created_at`);
  an empty inbox is `[]`. The `X-Unread-Count` header carries the unread total
  for the bell badge. Optional `unread=true` lists only unread ones, `limit`
  (1–100, default 50) and `before=<created_at of the last row seen>` page
  through older ones; other values answer 400;
- the signed-in user's profile and account:
  - `GET /me` returns the profile with `onboarding_complete` and
    `deletion_requested_at`, so the app can route to Onboarding or Home and
    offer to cancel a pending deletion. After a first "Continue with Google"
    sign-in it creates the profile (name and photo from Google,
    `onboarding_complete: false`). Without a valid session it answers 401.
  - `PATCH /me` saves any of `username`, `full_name`, `headline`, `company`,
    `city`, `bio`, `avatar_url`, `skills`, `interests`,
    `onboarding_complete`, `profile_visibility` (`public`|`connections`),
    `appear_in_search`, `hide_city`, `activity_status`, `login_alerts` and
    `two_factor_enabled`. Usernames are 3–30 lowercase letters, digits, dots
    or underscores; a username someone else already has gets 409 (also when
    two people save it at the same moment). Bios are at most 160 characters.
    Skills and interests are stored once each, however often they are sent.
    Two-factor sign-in can only be turned on once Supabase Auth has
    confirmed a phone number for the account (400 otherwise). Field errors
    come back as 400 with `error.fields`.
  - `POST /me/deletion?reason=...&confirm=DELETE` schedules the account for
    permanent deletion 14 days later (400 unless `confirm` is exactly
    `DELETE` and a reason is given); `DELETE /me/deletion` cancels it (404
    when nothing is pending). The database function
    `accounts_due_for_purge()` lists accounts whose 14 days are over, for
    the scheduled purge job;
  - community admission and roster management (bearer token required):
    `POST /communities/{id}/join-requests` asks to join a public community
    with answers to its questions (201 pending; 400 blank required answer;
    403 private, invite only; 409 already pending or joined);
    `POST /communities/{id}/join-requests/{userId}/decision?decision=approve|reject`
    lets a joined owner or admin review a request, never their own;
    `GET /communities/{id}/members?status=joined|pending` is the admin
    panel's Members and Requests list, with role labels and answers (403
    'Admins only' for everyone else);
    `PATCH /communities/{id}/members/{userId}` with `{"role": "admin|moderator|member"}`
    (owner only); `DELETE /communities/{id}/members/{userId}` removes a member
    (owners and admins; never self or the owner; only the owner removes an
    admin); `DELETE /me/memberships/{communityId}` leaves a community or
    withdraws a request (409 for the owner); and
    `POST /communities/{id}/transfer?to_user_id=...` hands ownership to a
    joined member and makes the previous owner an admin;
  - invitations: `POST /communities/{id}/invitations` (bearer) issues
    invitations for a `link`, `whatsapp`, `email` or `in_app` channel —
    `{"channel": "link"}`, `{"channel": "email", "emails": [...], "note": "..."}`
    or `{"channel": "in_app", "invitee_user_ids": [...]}` (single recipients:
    `invitee_email` / `invitee_user_id`), with optional `expires_at` (default
    14 days) and `max_uses` (default 1). Each recipient gets its own random
    128-bit code; only roles allowed by the community's invite setting may
    invite (403); a malformed address, yourself or an existing member is
    400 and nothing is created. Email invitations are sent by the platform
    with a link `<APP_LINK_BASE>communities/{id}/invite/{code}`; a failed
    send marks the invitation `failed`.
    `GET /communities/{id}/invitations/{code}` (no sign-in) returns the
    invitation with a community preview and its join questions and records
    the open; expired, revoked, used-up or mismatched codes get 404 "This
    invitation is not valid. Ask a community admin for an invite.".
    `POST /communities/{id}/invitations/{code}/accept` (bearer, answers as
    for join requests) makes the caller a joined member at once (410 when
    expired, revoked or used up; an existing member gets their membership
    back unchanged). `DELETE /communities/{id}/invitations/{code}` revokes
    (204; 403 for roles that cannot invite);
  - uploads: `POST /uploads?purpose=&mime=&bytes=` (bearer) checks the file
    type and size for its purpose and only then returns 201 with a signed
    upload URL (`upload_url`, valid 2 hours; the app `PUT`s the file there
    with the returned `mime` as `Content-Type`) and the object's `bucket` and
    `path`. Rules per purpose:

    | purpose | bucket | max size | types |
    |---|---|---|---|
    | `avatar` | `avatars` (public) | 3 MB ("Image too large (max 3MB)") | JPEG, PNG, WebP, HEIC |
    | `logo`, `cover` | `community-media` (public) | 5 MB | images ("Logo must be an image") |
    | `post_image` | `post-media` | 5 MB | images |
    | `post_video` | `post-media` | 50 MB ("Video must be under 50 MB") | MP4, MOV, WebM, 3GP |
    | `document` | `post-media` | 25 MB | PDF, Word, Excel, PowerPoint, text, CSV |
    | `chat` | `chat-media` | 25 MB | images, videos, voice notes, documents, vCard contacts |
    | `resume` | `resumes` | 10 MB | PDF, DOCX ("Resume must be a PDF or DOCX") |

    A type the purpose does not allow is 415, an oversized file 413, an
    unknown purpose or a missing size 400 — nothing is signed in those cases.
    The storage buckets enforce the same limits, so an upload that skips this
    check is still refused. Without `SUPABASE_URL` and
    `SUPABASE_SERVICE_ROLE_KEY` the endpoint answers 503;
  - GIF search: `GET /gifs?q=&limit=&offset=` (bearer) searches GIPHY with
    the server's `GIF_API_KEY` (never sent to the app), rating `pg`, and
    returns `{query, results: [{id, title, url, preview_url, width, height}],
    next_offset, attribution}` (`limit` 1–50, default 24; `next_offset` is
    null on the last page). Send a picked GIF as a chat message with
    `kind: "gif"` and its `url` as `attachment_url`. A blank or >50-character
    `q` is 400. When the key is unset or GIPHY fails or times out (5 s) the
    answer is 503 "GIF search is unavailable right now. You can still send
    text." — message sending never depends on it;
  - blocking (bearer): `POST /me/blocks {"blocked_id"}` blocks a user (201,
    idempotent; 400 for yourself, 404 unknown user) and removes any
    connection and follows between you, so the relationship is Idle and
    neither of you can message the other; `DELETE /me/blocks/{userId}`
    unblocks (204; you stay unconnected); `GET /me/blocks` lists the people
    you blocked, newest first, with a `profile` card. Blocked users are never
    told;
  - connections (bearer): `POST /connections {"addressee_id"}` sends a
    request (201 pending; 403 when a block exists; 409 already pending or
    connected); `PATCH /connections/{id}?action=accept|decline|undo` lets the
    recipient accept, ignore or undo an ignore (403 for the sender);
    `DELETE /connections/{id}` withdraws, cancels or removes (204, nobody is
    notified); `GET /me/connections?tab=incoming|sent|connected` returns
    `{tab, counts: {incoming, sent, connected}, items: [connection + peer]}`.
    An ignored request still shows as pending to its sender. Connection
    changes are published to Supabase Realtime (`connections` table), so
    both people's screens update without a reload;
  - people (bearer): `GET /people?mode=connections|members|suggested|invitable
    &community_id=&q=&filter=everyone|same_city|near_me|same_company&limit=`
    returns person cards with `connection_state`. `members` and `invitable`
    need `community_id`; `connections` with a community lists only
    connections who are members there (new chat, sharing); `suggested` ranks
    people you share a community with (not yet connected or pending) by
    shared communities, city, company and skills; `invitable` lists people
    not yet in the community (inviters only). `q` matches name, username,
    headline, company and skills. City and company filters use your profile
    (no location access); with no city or company set they return nothing.
    Blocked people (either way), yourself and people who turned off "appear
    in search" are never listed (except in your own connections);
  - member profile (bearer): `GET /users/{id}` returns the profile with
    `shared_communities` (only communities you both belong to),
    `mutual_connections {count, items}`, `connection_state`, `connection_id`,
    `primary_action` (`message` once connected, otherwise `connect`,
    `pending` or `respond`) and `follow {followers, following, is_following,
    can_follow}`. City is left out when hidden; a connections-only profile
    shows only its card (`limited: true`) to non-connections. 404 when either
    person blocked the other or you share no community;
  - mentorship (bearer): `POST /users/{id}/mentorship-requests {"topic",
    "message"}` with topic `career|system_design|referral|resume|grad_school`
    (400 without one) stores the request and notifies the recipient (in-app
    notification and push);
  - follow (bearer): `PUT /users/{id}/follow` follows a public profile (200,
    repeat is a no-op; 403 connections-only profile or a block);
    `DELETE /users/{id}/follow` unfollows (204). The app keeps Follow hidden
    behind its own feature flag for now;
  - chat conversations (bearer; all scoped to one community):
    - `GET /communities/{id}/threads?filter=all|unread|groups|calls&q=` lists
      the caller's conversations in that community only, most recent first,
      with title, subtitle, avatar, preview, unread count, `muted` and the
      DM peer (403 when not a joined member). Deleted chats, exited groups
      and DMs with blocked people are left out; `q` matches title, preview,
      subtitle and the community name; `calls` lists conversations with a
      call, with the latest as `last_call`.
    - `POST /communities/{id}/threads/direct?user_id=` opens the existing DM
      with a member or creates it (200; 403 when either person is not a
      joined member or one has blocked the other).
    - `POST /communities/{id}/threads/groups?member_ids=a,b` with
      `{"name","description"}` creates a group (201; 400 without a name or
      members; 403 when someone is not a member of the community).
    - `POST /threads/{id}/participants` with `{"user_id"}` (or
      `{"user_ids":[...]}`) adds members to a group (201; 403 for people
      outside the community).
    - `PATCH /threads/{id}/me` changes the caller's own settings: `mute`
      (`off|1h|8h|1w|always`) or `muted_until` (a mute simply ends at that
      time), `alert_mentions`, `alert_replies`, `alert_reactions`, `sound`,
      and `action` `clear` (hides earlier messages for the caller only),
      `delete` (also hides the chat until a new message) or `exit` (groups).
      `?disappearing_seconds=0|86400|604800|7776000` turns disappearing
      messages off or on for later messages. 403 when not a participant.
    - `GET /threads/{id}` returns the info page: participants, member count,
      the DM peer, shared `media`/`links`/`documents` counts and the caller's
      settings. It makes no encryption claim.
    - `POST /threads/{id}/calls` with `{"media":"audio|video","outcome":
      "completed|missed|declined|cancelled","duration_seconds"}` records a
      call attempt (201; a duration only for completed calls);
  - messages:
    - `GET /threads/{id}/messages?view=all|pinned|starred|media|links|documents&q=&after=&before=&limit=`
      pages a conversation (newest 50 by default, oldest first; `after` =
      everything newer, for catching up after a reconnect). Each message
      carries `status` (sent/delivered/read, own messages only), `reactions`,
      `starred` (the caller's own star only) and `can_edit`. Opening the
      conversation (`view=all`, no search) marks it read and resets its
      unread count. 403 when not a participant.
    - `POST /threads/{id}/messages` with header `scope_community_id` (the
      community the app is currently showing) and `{"kind","body",
      "attachment_url","attachment_bytes","reply_to_id","forwarded"}`, or
      `{"forward_of_id"}` to forward a copy of a visible message. 201; the
      conversation moves to the top and the sender's unread resets. 400 for
      whitespace-only text, 403 when scoped to another community, a block
      exists either way or the DM partner left, 413 over 25 MB. Unmuted
      participants get a push alert (an expired mute alerts again).
    - `PATCH /messages/{id}` `{"body"}` edits one's own message (blank text
      leaves it unchanged; 403 not the sender, 409 deleted).
    - `DELETE /messages/{id}?scope=me|everyone` (204): `me` hides it for the
      caller only; `everyone` (sender only, 409 if already deleted) shows
      "This message was deleted" and clears attachment, reply, reactions,
      pin and stars.
    - `PUT /messages/{id}/marks?action=react|unreact|star|unstar|pin|unpin`
      (`{"emoji"}` for react; 409 on a deleted message).
- event RSVPs (bearer):
  - `PUT /events/{id}/rsvp` with `{"status":"going|not_sure|declined",
    "payment_status":"pay_later|verification_pending", "decline_reason":
    "schedule|not_relevant|too_far|next_time|other", "note", "remind_push",
    "remind_email"}` (everything but `status` optional). The last seat goes
    to exactly one member: a full event answers 409 "Event full". A free
    "going" confirms the seat; on a paid event "I'll pay later" holds an
    unconfirmed seat (badge "Payment pending") and "I've paid" waits for the
    host (badge "Verification pending"). The answer is the RSVP plus
    `badge`, `seats_left` / `seats_left_label` ("3 seats left"), `join_url`
    (only with a confirmed seat), the `event` summary and `calendar` links
    (Google, Outlook and an iCalendar file for Apple). Going schedules push
    reminders 1 day and 1 hour before and, when email reminders are on, a
    day-of email; becoming "going" with email on sends a confirmation email.
    Declining or "not sure" tells the host quietly (in-app notification, no
    push). 403 not a member, 404 unknown event.
  - `POST /events/{id}/rsvps/{userId}/verify` — the event's host confirms an
    attendee's payment (seat confirmed, attendee gets a push); 403 not the
    host, 409 event full or the attendee is not going.
- a Go HTTP service with the shared plumbing every endpoint uses: Supabase access-token validation (requests
  without a valid session get 401), an API-key guard for webhooks, queries
  that run as the signed-in user so the database's row level security decides
  what they may read or change, and one JSON error format;
- a deploy pipeline that runs the tests, applies the migrations, and refuses
  to deploy when any table has row level security turned off.

## Stack

Go (version per go.mod) with Gin; Postgres 17 on Supabase; pgx for database access in tests.

## Requirements

- Go, at the version in `go.mod` or newer.
- For the database tests: a Postgres 17 server and a superuser connection URL.
- To apply the schema to a Supabase project: the Supabase CLI.

Environment variables (see `.env.example`):

| variable | used by | meaning |
|---|---|---|
| `PORT` | API | port to listen on, default `8080` |
| `APP_ENV` | API | `production` makes missing required settings fatal at start-up |
| `DATABASE_URL` | API, deploy | Postgres URL of a login that can switch to the `anon`, `authenticated` and `service_role` roles (required in production) |
| `SUPABASE_URL` | API | Supabase project URL (required in production) |
| `SUPABASE_JWT_SECRET` | API | project JWT secret, ≥ 32 characters, used to check access tokens (required in production) |
| `WEBHOOK_API_KEY` | API | key callers send as `X-API-Key` on `/internal/...` routes (required in production) |
| `SUPABASE_ANON_KEY`, `SUPABASE_SERVICE_ROLE_KEY` | API | Supabase keys for auth and storage calls (the service-role key signs upload URLs) |
| `EMAIL_API_KEY`, `EMAIL_FROM`, `PUSH_API_KEY`, `GIF_API_KEY`, `RAZORPAY_KEY_ID`, `RAZORPAY_KEY_SECRET`, `RAZORPAY_WEBHOOK_SECRET` | API | optional provider keys; empty keeps the feature off |
| `APP_LINK_BASE` | API | prefix of app links in emails, default `intouch://` (invite links are `<base>communities/<id>/invite/<code>`) |
| `SUPABASE_AUTH_EXTERNAL_GOOGLE_CLIENT_ID`, `SUPABASE_AUTH_EXTERNAL_GOOGLE_SECRET` | Supabase CLI | Google OAuth client for "Continue with Google" (`supabase/config.toml`); on a hosted project set them under Authentication → Providers → Google, with redirect URI `<SUPABASE_URL>/auth/v1/callback` and `intouch://auth-callback` as an allowed app redirect |
| `TEST_DATABASE_URL` | tests | superuser Postgres URL; tests create and drop their own databases |
| `SUPABASE_PROJECT_REF`, `SUPABASE_ACCESS_TOKEN`, `SUPABASE_DB_PASSWORD`, `DEPLOY_COMMAND` | deploy | target project and the command that rolls out the built image (`$IMAGE`) |

Copy `.env.example` to `.env` and fill it in; secrets are never logged.
The `/auth/*` endpoints need `DATABASE_URL`, `SUPABASE_URL` and
`SUPABASE_ANON_KEY`; without them they answer 503. No email provider is
wired in yet: new-device alert emails are only logged (recipient domain and
subject), while the in-app `login_alert` notification is always stored.
With a database configured, every signed-in request also checks that its
session has not been signed out.

Supabase Auth setup for the code flows (`supabase/config.toml` holds the
local equivalents):

- Email confirmations on, OTP length 6, OTP expiry 600 s. The "Confirm
  signup" and "Reset password" email templates must show `{{ .Token }}`
  (the 6-digit code), not a link.
- Configure an SMS provider for phone codes. Phone sign-up stays off, so a
  phone code only signs in an existing account.
- Keep Auth's own rate limits on. They are a backstop for clients that call
  Supabase Auth directly instead of going through this API.

## Install

```sh
go mod download
```

## Run locally

```sh
go run ./cmd/api
```

The API listens on http://localhost:8080 (`PORT` changes it):

```sh
curl http://localhost:8080/healthz
curl -X POST http://localhost:8080/auth/sign-up \
  -d '{"full_name":"Asha Rao","email":"asha@example.com","password":"Secret123","confirm_password":"Secret123"}'
curl -X POST 'http://localhost:8080/uploads?purpose=avatar&mime=image/jpeg&bytes=204800' \
  -H "Authorization: Bearer $ACCESS_TOKEN"
```

## Database

Apply the migrations to a local Supabase stack:

```sh
supabase start
supabase db reset
```

## Test

Unit tests (database tests are skipped without a database):

```sh
go test ./...
```

Database schema, policy and access-boundary tests against any Postgres 17 superuser URL:

```sh
TEST_DATABASE_URL=postgres://postgres@localhost:5432/postgres go test -count=1 ./internal/dbtest/... ./internal/db/...
```

This includes two load checks (skip them with `-short`): chat delivery, and
the feed check, which seeds 25,000 members and 50,000 posts and asserts that a
feed page is an index scan that runs in under 50 ms (about 1 ms measured).

The pgTAP suites in `supabase/tests/database` also run under the Supabase CLI:

```sh
supabase test db
```

## Build

```sh
go build -o bin/api ./cmd/api
```

A container image (needs Docker):

```sh
docker build -t intouch-api .
```

## Deploy

Check that every table in a database has row level security on (exits 1 and
names the tables otherwise):

```sh
go run ./cmd/rlscheck "$DATABASE_URL"
```

Full pipeline — vet and unit tests, database tests, `supabase db push`, the
row-level-security check on the deployed database, image build, then
`DEPLOY_COMMAND`. Any failing step stops the deploy:

```sh
./scripts/deploy.sh
```

Local rehearsal without touching any remote system (needs `TEST_DATABASE_URL`):

```sh
DRY_RUN=1 ./scripts/deploy.sh
```
