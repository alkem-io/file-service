.PHONY: build build-sweep build-stub docker test test-vips lint generate sqlc-generate openapi setup-hooks run clean

BINARY := file-service
SWEEP_BINARY := sweep-ipfs-cids
GO := go
GOFLAGS := -race

# apispec is run via `go run <pkg>@version` (like matrix-adapter) so NO
# pre-installed binary on PATH is required — a globally-installed apispec fails to
# load packages under Go 1.26. Keep in sync with the org-pinned apispec (the
# shared alkem-io/github-workflows go-ci.yml@v1 default — currently v0.4.25).
APISPEC_VERSION ?= v0.4.25

build:
	mkdir -p bin/
	$(GO) build -tags vips -o bin/$(BINARY) ./cmd/server/

build-sweep:
	mkdir -p bin/
	$(GO) build -o bin/$(SWEEP_BINARY) ./cmd/sweep-ipfs-cids/

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

lint:
	golangci-lint run

generate:
	$(GO) generate ./...

sqlc-generate:
	sqlc -f db/sqlc.yaml generate

openapi:
	$(GO) run github.com/antst/go-apispec/cmd/apispec@$(APISPEC_VERSION) --dir . --output openapi.yaml --config apispec.yaml --skip-cgo

setup-hooks:
	git config core.hooksPath .githooks
	@echo "Git hooks configured"

run:
	$(GO) run ./cmd/server/

clean:
	rm -rf bin/ coverage.out
