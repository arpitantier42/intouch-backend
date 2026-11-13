.PHONY: test test-db build run vet rls-check deploy deploy-dry-run

# Unit tests; database tests run too when TEST_DATABASE_URL is set.
test:
	go test ./...

# Database schema, policy and RLS-boundary tests (needs TEST_DATABASE_URL, a superuser URL).
test-db:
	@test -n "$(TEST_DATABASE_URL)" || (echo "set TEST_DATABASE_URL" && exit 1)
	go test -count=1 ./internal/dbtest/... ./internal/db/...

vet:
	go vet ./...

build:
	go build -o bin/api ./cmd/api

run:
	go run ./cmd/api

# Deploy gate: fails if any public table in DATABASE_URL has RLS disabled.
rls-check:
	@test -n "$(DATABASE_URL)" || (echo "set DATABASE_URL" && exit 1)
	go run ./cmd/rlscheck "$(DATABASE_URL)"

deploy:
	./scripts/deploy.sh

deploy-dry-run:
	DRY_RUN=1 ./scripts/deploy.sh
