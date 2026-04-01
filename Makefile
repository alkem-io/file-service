.PHONY: build build-stub test test-vips lint generate sqlc-generate openapi setup-hooks run clean

BINARY := file-service
GO := go
GOFLAGS := -race

build:
	mkdir -p bin/
	$(GO) build -tags vips -o bin/$(BINARY) ./cmd/server/

build-stub:
	mkdir -p bin/
	$(GO) build -o bin/$(BINARY) ./cmd/server/

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
	apispec --dir . --output openapi.yaml --config apispec.yaml --skip-cgo

setup-hooks:
	git config core.hooksPath .githooks
	@echo "Git hooks configured"

run:
	$(GO) run ./cmd/server/

clean:
	rm -rf bin/ coverage.out
