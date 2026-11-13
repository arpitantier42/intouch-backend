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
- a Go HTTP service, currently serving a health check.

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
| `TEST_DATABASE_URL` | tests | superuser Postgres URL; tests create and drop their own databases |

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

Database schema and policy tests against any Postgres 17 superuser URL:

```sh
TEST_DATABASE_URL=postgres://postgres@localhost:5432/postgres go test -count=1 ./internal/dbtest/...
```

The pgTAP suites in `supabase/tests/database` also run under the Supabase CLI:

```sh
supabase test db
```

## Build

```sh
go build -o bin/api ./cmd/api
```
