.PHONY: test test-db build run vet

# Unit tests; database tests run too when TEST_DATABASE_URL is set.
test:
	go test ./...

# Database schema and policy tests (needs TEST_DATABASE_URL, a superuser URL).
test-db:
	@test -n "$(TEST_DATABASE_URL)" || (echo "set TEST_DATABASE_URL" && exit 1)
	go test -count=1 ./internal/dbtest/...

vet:
	go vet ./...

build:
	go build -o bin/api ./cmd/api

run:
	go run ./cmd/api
