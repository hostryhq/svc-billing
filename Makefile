# svc-billing. The hostryhq modules it imports are public; for local development against sibling checkouts use an
# uncommitted go.work (go-common, go-auth, api-specs).
SHELL := /bin/bash
COMPOSE ?= docker compose -p billing -f ../infra/dev/compose.yaml
export HOSTRY_PG_PORT ?= 55446
export BILLING_TEST_PG_ADMIN_DSN ?= postgres://hostry:hostry-dev-only@127.0.0.1:$(HOSTRY_PG_PORT)/hostry?sslmode=disable

.PHONY: gen vet lint test race integration contract dev-up dev-down tidy build

# Regenerate internal/gen, the seed examples and the embedded tax vectors from ../api-specs (override API_SPECS_DIR);
# records the spec commit in internal/gen/SPEC_VERSION.
gen:
	./scripts/gen.sh

build:
	go build ./...

vet:
	go vet ./... && go vet -tags integration ./...

# golangci-lint when installed (CI runs it).
lint: vet
	@command -v golangci-lint >/dev/null && golangci-lint run ./... || echo "golangci-lint not installed; CI runs it"

# Unit tests and the contract suite against the in-memory double (no database needed).
test:
	go test -count=1 ./...

race:
	go test -race -count=1 ./...

# Integration tests: real Postgres (dev stack, see dev-up), real service, shared contract suite, isolation suites.
integration:
	go test -tags integration -race -count=1 ./...

contract:
	go test -count=1 ./double/ -run Contract
	go test -tags integration -count=1 ./api/ -run RealServiceContract

# Postgres for integration tests from the shared dev stack, under its own compose project and host port.
dev-up:
	$(COMPOSE) up -d --wait postgres

dev-down:
	$(COMPOSE) down -v

tidy:
	go mod tidy
