.PHONY: build build-stub docker test test-vips lint generate sqlc-generate openapi setup-hooks run clean test-e2e

BINARY := file-service
GO := go
GOFLAGS := -race

build:
	mkdir -p bin/
	$(GO) build -tags vips -o bin/$(BINARY) ./cmd/server/

build-stub:
	mkdir -p bin/
	$(GO) build -o bin/$(BINARY) ./cmd/server/

docker:
	docker build -t alkemio/file-service:latest .

test:
	$(GO) test $(GOFLAGS) -coverprofile=coverage.out ./...
	$(GO) tool cover -func=coverage.out | tail -1

test-vips:
	$(GO) test -tags vips $(GOFLAGS) -coverprofile=coverage.out ./...
	$(GO) tool cover -func=coverage.out | tail -1

# End-to-end: drives the sweep through its REAL adapters against a real directory and
# a real database, rather than through fakes. Tag-gated because it needs Postgres.
#
# It is not redundant with `test`. The fakes share the implementation's blind spots —
# they assert what the code was written to do — so two defects reached a clean lint, a
# clean race-enabled suite and five review rounds before a manual run found them.
test-e2e:
	# The service-package end-to-end sweep AND the alkemiodb tests that need a real
	# database. The latter were the point of the exercise and were still skipping in
	# CI: testPool calls t.Skipf when no database is reachable, so TestRenameExternalID
	# — guarding the single UPDATE this whole migration turns on, where "an argument
	# swap compiles" — passed without running on every PR.
	$(GO) test -tags e2e $(GOFLAGS) ./internal/domain/service/ -run EndToEnd -v
	$(GO) test $(GOFLAGS) ./internal/adapter/outbound/alkemiodb/ -v -run 'TestRename|TestListLegacy|TestPark|TestLink'

lint:
	golangci-lint run
	# Tag-gated files are invisible to the run above, so they get their own pass —
	# otherwise e2e coverage can rot behind a green gate.
	golangci-lint run --build-tags e2e ./internal/domain/service/

generate:
	$(GO) generate ./...

sqlc-generate:
	sqlc -f db/sqlc.yaml generate

openapi:
	apispec --dir . --output openapi.yaml --config apispec.yaml --skip-cgo

setup-hooks:
	git config core.hooksPath .githooks
	@echo "Git hooks configured"

run:
	$(GO) run ./cmd/server/

clean:
	rm -rf bin/ coverage.out
