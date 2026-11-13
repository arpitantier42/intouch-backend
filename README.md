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
- a Go HTTP service, currently serving a health check, with the shared
  plumbing every endpoint uses: Supabase access-token validation (requests
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
| `SUPABASE_ANON_KEY`, `SUPABASE_SERVICE_ROLE_KEY` | API | Supabase keys for auth and storage calls |
| `EMAIL_API_KEY`, `EMAIL_FROM`, `PUSH_API_KEY`, `GIF_API_KEY`, `RAZORPAY_KEY_ID`, `RAZORPAY_KEY_SECRET`, `RAZORPAY_WEBHOOK_SECRET` | API | optional provider keys; empty keeps the feature off |
| `TEST_DATABASE_URL` | tests | superuser Postgres URL; tests create and drop their own databases |
| `SUPABASE_PROJECT_REF`, `SUPABASE_ACCESS_TOKEN`, `SUPABASE_DB_PASSWORD`, `DEPLOY_COMMAND` | deploy | target project and the command that rolls out the built image (`$IMAGE`) |

Copy `.env.example` to `.env` and fill it in; secrets are never logged.

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
