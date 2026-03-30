# Implementation Plan: Go File Service

**Branch**: `001-file-service-init` | **Date**: 2026-03-30 | **Spec**: [spec.md](spec.md)
**Input**: Feature specification from `/specs/001-file-service-init/spec.md`

## Summary

Universal file I/O gateway and document lifecycle manager for the Alkemio platform, replacing the existing TypeScript file-service. Serves files to authenticated users (public endpoint via Oathkeeper + NATS auth), provides internal document CRUD endpoints (create, read content/metadata, update, delete), handles image processing (HEIC→JPEG, compression), and MIME detection from magic bytes. The file-service owns the `document` table (full CRUD); the server becomes read-only. ExternalID is never exposed in any API. Built with hexagonal architecture using Go 1.25, chi v5, pgx/sqlc, NATS, and govips.

## Technical Context

**Language/Version**: Go 1.25 (constitution-mandated)
**Primary Dependencies**: chi v5.2.5 (HTTP), pgx v5.9.1 (DB), sqlc v1.30.0 (codegen), zap v1.27.1 (logging), nats.go v1.50.0 (messaging), govips v2.17.0 (image processing), mimetype v1.4.13 (MIME detection), x/crypto v0.49.0 (SHA3-256), google/uuid (UUIDv7)
**Storage**: PostgreSQL (Alkemio DB, full CRUD on document table, read-only on all others), local filesystem (file bytes)
**Testing**: `go test` with race detector, 95% unit coverage target, integration tests against real DB/storage
**Target Platform**: Linux container (K8s), macOS for local dev
**Project Type**: Web service (HTTP microservice)
**Performance Goals**: No invented SLAs (constitution XIII). Must handle existing Alkemio traffic patterns.
**Constraints**: Drop-in replacement for TS file-service (same port 4003, same storage layout, same public API). libvips runtime dependency. Server-to-file-service communication via HTTP (not NATS). UUIDv7 for all generated IDs.
**Scale/Scope**: Single instance (PVC-bound). Same scale as existing TS file-service.

## Constitution Check

*GATE: Must pass before Phase 0 research. Re-check after Phase 1 design.*

| Principle | Status | Evidence |
|-----------|--------|----------|
| I. Hexagonal Architecture | PASS | Domain ports (StoragePort, AuthPort, DocumentRepo, ImageProcessor), adapters (HTTP, DB, NATS, local storage) |
| II. Storage Abstraction | PASS | StoragePort interface with local adapter; S3 pluggable via config |
| III. Alkemio Integration First | PASS | NATS auth.evaluate, Oathkeeper JWT, document table (full CRUD), HTTP internal endpoints |
| IV. Type-Safe Database Access | PASS | sqlc for query generation, pgx v5 driver |
| V. Security by Design | PASS | JWT validation, NATS auth on every public request, MIME detection from content, no auth bypass |
| VI. Test-First Development | PASS | TDD workflow, in-memory adapters for unit tests, integration tests for adapters |
| VII. Root Cause Analysis | PASS | Process principle — enforced during implementation |
| VIII. DRY | PASS | Single image processing pipeline shared by document create and store-and-link |
| IX. Lint on Completion | PASS | golangci-lint run before every commit |
| X. No Legacy Code | PASS | Greenfield service, no dead code |
| XI. No Busywork | PASS | All artifacts serve implementation needs |
| XII. Meaningful Tests (95%) | PASS | Coverage target set; tests defend invariants (hash compat, MIME detection, auth flow, document CRUD atomicity) |
| XIII. Meaningful Success Criteria | PASS | All SC items testable within this service |
| XIV. Latest Dependencies | PASS | All versions verified online 2026-03-30 (see research.md) |
| XV. No Assumptions | PASS | All unknowns researched; NATS contract verified from source; document table ownership confirmed |

## Project Structure

### Documentation (this feature)

```text
specs/001-file-service-init/
├── plan.md              # This file
├── spec.md              # Feature specification (40 FRs, 12 SCs, 6 user stories)
├── research.md          # Dependency versions, library decisions
├── data-model.md        # Entity definitions, queries, state transitions
├── quickstart.md        # Dev setup, env vars, key flows
├── contracts/
│   ├── public-api.md    # GET /rest/storage/document/:id, GET /health
│   ├── private-api.md   # Internal document CRUD + store-and-link
│   ├── nats-messages.md # auth.evaluate request/response
│   └── k8s-manifest.md  # Example K8s deployment
└── checklists/
    └── requirements.md  # Spec quality checklist
```

### Source Code (repository root)

```text
cmd/
  server/
    main.go                 # Entry point: config → wiring → HTTP server

internal/
  adapter/
    inbound/
      http/
        router.go           # chi router: public (/rest/storage/), internal (/internal/), health, debug
        middleware.go        # JWT extraction, request logging, request ID
        public_handler.go   # GET /rest/storage/document/:id (authenticated)
        document_handler.go # GET/POST/DELETE/PATCH /internal/document/... + PUT .../content
        health_handler.go   # GET /health
        metrics.go          # expvar setup for /debug/vars
    outbound/
      alkemiodb/
        adapter.go          # DocumentRepo implementation via pgx/sqlc
        queries/             # sqlc generated code
      nats/
        auth_client.go      # AuthPort implementation: auth.evaluate request-reply
      storage/
        local/
          adapter.go        # StoragePort implementation: local filesystem

  config/
    config.go               # Env var loading, validation, defaults
    logger.go               # Zap logger setup

  domain/
    model/
      document.go           # Document, CreateDocumentInput, StoredFile, AuthResult, DeletedDocument
    port/
      storage.go            # StoragePort interface (save, read, delete, exists)
      auth.go               # AuthPort interface (CheckPrivilege)
      document_repo.go      # DocumentRepo interface (GetByID, Create, UpdateFile, UpdateLocation, Delete, CountByExternalID)
      image_processor.go    # ImageProcessor interface
    service/
      file_service.go       # Core orchestration: serve, create, delete, update, store-and-link
      hash.go               # SHA3-256 content hashing

  imaging/
    processor.go            # govips wrapper: detect MIME, convert HEIC, compress, resize

  resilience/
    breaker.go              # Circuit breaker (NATS, DB)
    nats_conn.go            # NATS connection with reconnect handlers

db/
  queries/
    document.sql            # sqlc query definitions (6 queries per data-model.md)
  schema/
    document.sql            # Document table DDL (for sqlc codegen)
  sqlc.yaml                 # sqlc configuration

Dockerfile                  # Multi-stage: build + distroless runtime with libvips
Makefile                    # build, test, lint, sqlc-generate, run
go.mod
go.sum
.golangci.yml               # Linter configuration
```

**Structure Decision**: Follows WOPI service hexagonal layout. Consolidated all internal endpoints into `document_handler.go` (was split across private_handler, storelink_handler, document_handler — now one file since all internal endpoints are document-centric). `imaging/` is domain-adjacent, not a port/adapter. `resilience/` mirrors auth-eval-service pattern.

## Complexity Tracking

No constitution violations — no entries needed.
