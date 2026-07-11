# saige development commands

# List available recipes
default:
    @just --list

# Run all tests
test:
    go test ./...

# Run tests with coverage report
test-cover:
    go test -coverprofile=coverage.out -covermode=atomic ./...
    go tool cover -func=coverage.out

# Run golangci-lint
lint:
    golangci-lint run ./...

# Run go vet
vet:
    go vet ./...

# Format code
fmt:
    gofmt -w .

# Install CLI binary to $GOPATH/bin
install:
    CGO_ENABLED=0 go install -trimpath -ldflags="-s -w" ./cmd/saige

# Build CLI binary to bin/
build:
    CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o bin/saige ./cmd/saige

# Run govulncheck
vuln:
    govulncheck ./...

# Tidy modules
tidy:
    go mod tidy

# Run full CI locally (lint + vet + test)
check: lint vet test

# Run benchmarks
bench:
    go test -bench=. -benchmem ./...

# Run benchmarks and write the report into the validation results folder
bench-report:
    go test -run=^$ -bench=. -benchmem ./agent/ ./agent/provider/cache/ | tee examples/validation/results/benchmarks.txt

# Run the live validation harness against a real model (needs OPENAI_API_KEY)
validate:
    go run ./examples/validation

# Run fuzz tests for a specific package and function
fuzz PACKAGE FUNC DURATION="30s":
    go test -fuzz={{FUNC}} -fuzztime={{DURATION}} {{PACKAGE}}

# docker compose plugin or standalone docker-compose, whichever is installed
compose := `docker compose version >/dev/null 2>&1 && echo "docker compose" || echo "docker-compose"`

# Start local integration infra (pgvector Postgres on :5433)
integration-up:
    {{compose}} -f integration/docker-compose.yml up -d --wait postgres

# Stop integration infra and delete its data
integration-down:
    {{compose}} -f integration/docker-compose.yml down -v

# Run end-to-end integration tests (Ollama + Postgres + DBOS); see integration/README.md
test-integration:
    SAIGE_TEST_OLLAMA_HOST="${SAIGE_TEST_OLLAMA_HOST:-http://localhost:11434}" \
    SAIGE_TEST_POSTGRES_DSN="${SAIGE_TEST_POSTGRES_DSN:-postgres://postgres:test@localhost:5433/postgres?sslmode=disable}" \
    go test ./integration/ -v -count=1 -timeout 30m

# Build docker image
docker-build:
    docker build -t saige .

# Clean build artifacts
clean:
    rm -rf bin/ coverage.out
    go clean -cache -testcache
