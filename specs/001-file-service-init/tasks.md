# Tasks: Go File Service Implementation

**Input**: Design documents from `/specs/001-file-service-init/`
**Prerequisites**: plan.md, spec.md, research.md, data-model.md, contracts/

**Tests**: Included per constitution Principle VI (Test-First Development). 95% unit coverage target (Principle XII).

**Organization**: Tasks grouped by user story. Each story is independently testable.

## Format: `[ID] [P?] [Story] Description`

- **[P]**: Can run in parallel (different files, no dependencies)
- **[Story]**: Which user story (US1–US6) this task belongs to

---

## Phase 1: Setup

**Purpose**: Project initialization, dependencies, tooling

- [x] T001 Initialize Go module (`go.mod`) with `go 1.25` and add all dependencies from research.md: chi v5.2.5, pgx v5.9.1, zap v1.27.1, nats.go v1.50.0, govips v2.17.0, mimetype v1.4.13, x/crypto v0.49.0, google/uuid
- [x] T002 [P] Create project directory structure per plan.md: `cmd/server/`, `internal/{adapter/{inbound/http,outbound/{alkemiodb,nats,storage/local}},config,domain/{model,port,service},imaging,resilience}`, `db/{queries,schema}`
- [x] T003 [P] Create Makefile with targets: `build`, `test`, `lint`, `sqlc-generate`, `run`
- [x] T004 [P] Create `.golangci.yml` linter configuration
- [x] T005 [P] Create Dockerfile: multi-stage build (Go 1.25 builder + distroless runtime with libvips)
- [x] T006 [P] Create `.env.example` with all env vars from quickstart.md

---

## Phase 2: Foundational (Blocking Prerequisites)

**Purpose**: Core infrastructure that MUST be complete before ANY user story

**CRITICAL**: No user story work can begin until this phase is complete

- [x] T007 Implement config loading and validation in `internal/config/config.go` — load all env vars (PORT, LOCAL_STORAGE_PATH, DOCUMENT_MAX_AGE, ALKEMIO_DATABASE_*, NATS_*, STORAGE_TYPE) with defaults per spec FR-015 through FR-022
- [x] T008 [P] Define domain types in `internal/domain/model/document.go` — Document (all fields per data-model.md), CreateDocumentInput, StoredFile, AuthResult, DeletedDocument structs
- [x] T009 [P] Define StoragePort interface in `internal/domain/port/storage.go` — Save(content []byte) (StoredFile, error), Read(externalID string) ([]byte, error), Delete(externalID string) error, Exists(externalID string) (bool, error)
- [x] T010 [P] Define AuthPort interface in `internal/domain/port/auth.go` — CheckPrivilege(ctx, agentID, privilege, authorizationPolicyID string) (AuthResult, error)
- [x] T011 [P] Define DocumentRepo interface in `internal/domain/port/document_repo.go` — GetByID, Create, UpdateFile, UpdateLocation, Delete, CountByExternalID per data-model.md queries
- [x] T012 [P] Define ImageProcessor interface in `internal/domain/port/image_processor.go` — DetectMIME(content []byte) string, Process(content []byte, mimeType string) ([]byte, string, error)
- [x] T013 Implement SHA3-256 hashing in `internal/domain/service/hash.go` using `golang.org/x/crypto/sha3` — must produce hex output identical to Node.js `createHash('sha3-256').update(data).digest('hex')`
- [x] T014 Write hash compatibility test in `internal/domain/service/hash_test.go` — verify against known Node.js reference hashes (e.g., `sha3-256("hello")` = `3338be694f...`)
- [x] T015 [P] Create sqlc configuration in `db/sqlc.yaml` and document table DDL in `db/schema/document.sql` (full schema per data-model.md — all columns including authorizationId, tagsetId, storageBucketId, temporaryLocation)
- [x] T016 Write sqlc queries in `db/queries/document.sql` — all 6 queries per data-model.md: GetDocumentByID, CreateDocument, UpdateDocumentFile, UpdateDocumentLocation, DeleteDocument, CountDocumentsByExternalID
- [x] T017 Run `sqlc generate` and verify generated code in `internal/adapter/outbound/alkemiodb/queries/`
- [x] T018 [P] Implement Zap logger setup in `internal/config/logger.go` — structured JSON output, configurable level
- [x] T019 [P] Implement NATS connection manager in `internal/resilience/nats_conn.go` — exponential backoff reconnect, disconnect/reconnect handlers, matching auth-eval-service pattern
- [x] T020 [P] Implement circuit breaker in `internal/resilience/breaker.go` — configurable failure threshold, timeout, half-open max requests (from NATS_FAILURE_THRESHOLD, NATS_BREAKER_TIMEOUT_SECONDS, NATS_HALF_OPEN_MAX_REQUESTS)
- [x] T021 Implement chi router skeleton in `internal/adapter/inbound/http/router.go` — register route groups: public (`/rest/storage/`), internal (`/internal/`), health (`/health`), debug (`/debug/vars`); apply middleware
- [x] T022 [P] Implement request logging middleware in `internal/adapter/inbound/http/middleware.go` — request ID generation, Zap structured logging (request ID, endpoint, duration)
- [x] T023 [P] Implement expvar metrics setup in `internal/adapter/inbound/http/metrics.go` — NATS connection state, reconnect count, disconnect count, storage/document operations counts, breaker state (FR-032)
- [x] T024 Create `cmd/server/main.go` entry point — config loading, logger init, DB pool, NATS connection, adapter wiring, HTTP server start with graceful shutdown

**Checkpoint**: Foundation ready — all ports defined, infrastructure wired, user story implementation can begin

---

## Phase 3: User Story 1 — Serve Files to Authenticated Users (Priority: P1) MVP

**Goal**: Public endpoint `GET /rest/storage/document/:id` serves files with JWT auth and NATS authorization. Drop-in replacement for TS file-service.

**Independent Test**: Send GET with valid JWT for an accessible document -> file content returned with correct Content-Type, Cache-Control, ETag headers.

### Tests for User Story 1

- [x] T025 [P] [US1] Write unit test for JWT extraction middleware in `internal/adapter/inbound/http/middleware_test.go` — valid JWT extracts actorId, missing JWT returns 401, invalid JWT returns 401
- [x] T026 [P] [US1] Write unit test for public handler in `internal/adapter/inbound/http/public_handler_test.go` — mock DocumentRepo + AuthPort + StoragePort: authorized -> 200 with headers, unauthorized -> 403, doc not found -> 404, file not found -> 404, conditional 304
- [x] T027 [P] [US1] Write unit test for NATS auth client in `internal/adapter/outbound/nats/auth_client_test.go` — mock NATS conn: allowed response, denied response, error response with circuit breaker info, timeout
- [x] T028 [P] [US1] Write unit test for Alkemio DB adapter (read) in `internal/adapter/outbound/alkemiodb/adapter_test.go` — mock pgx pool: document found, document not found

### Implementation for User Story 1

- [x] T029 [US1] Implement JWT extraction middleware in `internal/adapter/inbound/http/middleware.go` — extract `alkemio_actor_id` claim from Authorization header (FR-002)
- [x] T030 [US1] Implement NATS auth client in `internal/adapter/outbound/nats/auth_client.go` — `CheckPrivilege` via `RequestWithContext` to `auth.evaluate` subject, envelope format per contracts/nats-messages.md, handle error.retryAfterMs for degraded state
- [x] T031 [US1] Implement Alkemio DB adapter (read) in `internal/adapter/outbound/alkemiodb/adapter.go` — `GetByID` using sqlc generated query, return Document with all fields
- [x] T032 [US1] Implement local filesystem storage adapter (read + exists) in `internal/adapter/outbound/storage/local/adapter.go` — `Read(externalID)` reads from `{LOCAL_STORAGE_PATH}/{externalID}`, `Exists(externalID)` checks file existence
- [x] T033 [US1] Implement file serve logic in `internal/domain/service/file_service.go` — `ServeFile(ctx, documentID, actorID)`: lookup document -> auth check -> read file -> return content with metadata
- [x] T034 [US1] Implement public handler in `internal/adapter/inbound/http/public_handler.go` — `GET /rest/storage/document/:id`: JWT actorId -> service.ServeFile -> stream response with Content-Type, Cache-Control, Pragma, Expires, ETag headers per FR-018
- [x] T035 [US1] Implement health handler in `internal/adapter/inbound/http/health_handler.go` — `GET /health`: check NATS connection + DB reachability, return JSON status per contracts/private-api.md
- [x] T036 [US1] Implement 503 handling for dependency failures — circuit breaker check before auth call, return 503 with structured JSON error body per FR-033
- [x] T037 [US1] Wire US1 routes in router.go and adapters in main.go — connect public handler with real adapters, verify end-to-end flow
- [x] T038 [US1] Verify ETag conditional request support — `If-None-Match` header -> 304 Not Modified (FR-011)

**Checkpoint**: US1 complete — public file serving works with auth, caching, and proper error handling. This is the MVP.

---

## Phase 4: User Story 5 — Image Processing and MIME Validation (Priority: P2)

**Goal**: Content-based MIME detection, HEIC->JPEG conversion, JPEG/WebP compression with resize.

**Independent Test**: Upload HEIC -> response shows `image/jpeg` mimeType. Upload JPEG > 4096px -> resized. Upload `.exe` claimed as `image/jpeg` -> rejected by content detection.

### Tests for User Story 5

- [x] T039 [P] [US5] Write unit test for MIME detection in `internal/imaging/processor_test.go` — JPEG detected as `image/jpeg`, PDF as `application/pdf`, SVG as `image/svg+xml`, unknown as `application/octet-stream`
- [x] T040 [P] [US5] Write unit test for HEIC->JPEG conversion in `internal/imaging/processor_test.go` — HEIC input -> JPEG output, mimeType changed to `image/jpeg`, output is valid JPEG
- [x] T041 [P] [US5] Write unit test for JPEG compression in `internal/imaging/processor_test.go` — large JPEG -> compressed output <= original, EXIF stripped, auto-oriented, oversized -> resized to max 4096px
- [x] T042 [P] [US5] Write unit test for pass-through formats in `internal/imaging/processor_test.go` — SVG, GIF, PNG pass through unmodified; PDF passes through unmodified
- [x] T043 [P] [US5] Write unit test for compression size guard in `internal/imaging/processor_test.go` — if compressed > original, return original

### Implementation for User Story 5

- [x] T044 [US5] Implement MIME detection in `internal/imaging/processor.go` — `DetectMIME(content)` using `gabriel-vasile/mimetype` library, return MIME string from magic bytes
- [x] T045 [US5] Implement HEIC->JPEG conversion in `internal/imaging/processor.go` — `convertHEIC(content)` using govips: load HEIC, export as JPEG quality=100 (FR-027)
- [x] T046 [US5] Implement JPEG/WebP compression in `internal/imaging/processor.go` — `compress(content, mimeType)` using govips: quality 82, max 4096px resize, EXIF strip, auto-orient (FR-028); skip SVG/GIF/PNG; return original if compressed is larger
- [x] T047 [US5] Implement `Process(content, detectedMIME)` orchestrator in `internal/imaging/processor.go` — HEIC/HEIF -> convert -> compress; JPEG/WebP -> compress; others -> pass through; return processed bytes + final mimeType
- [x] T048 [US5] Add test fixture files in `internal/imaging/testdata/` — fixtures generated programmatically at test time

**Checkpoint**: US5 complete — image processing and MIME detection ready for use by document endpoints.

---

## Phase 5: User Story 6 — Document Lifecycle Management (Priority: P2)

**Goal**: File-service owns the `document` table (full CRUD). `POST /internal/document` creates a document (file + record atomically). `DELETE /internal/document/:id` removes record + file. `PATCH /internal/document/:id` updates mutable fields.

**Independent Test**: POST with file + metadata + authorizationId/tagsetId -> 201 with document ID (UUIDv7); DELETE -> row and file removed, response includes authorizationId/tagsetId; PATCH -> storageBucketId updated.

**Depends on**: US5 (image processing)

### Tests for User Story 6

- [x] T049 [P] [US6] Write unit test for CreateDocument DB adapter in `internal/adapter/outbound/alkemiodb/adapter_test.go` — insert with all fields -> success with generated UUIDv7 ID, FK constraint failure -> error
- [x] T050 [P] [US6] Write unit test for DeleteDocument DB adapter in `internal/adapter/outbound/alkemiodb/adapter_test.go` — delete existing -> returns authorizationId + tagsetId, delete non-existent -> error
- [x] T051 [P] [US6] Write unit test for CountByExternalID DB adapter in `internal/adapter/outbound/alkemiodb/adapter_test.go` — 0 for unknown hash, 1 for unique, 2+ for shared
- [x] T052 [P] [US6] Write unit test for UpdateLocation DB adapter in `internal/adapter/outbound/alkemiodb/adapter_test.go` — update storageBucketId + temporaryLocation, non-existent -> error
- [x] T053 [P] [US6] Write unit test for local storage Save in `internal/adapter/outbound/storage/local/adapter_test.go` — save content, verify file at `{path}/{sha3hash}`, save same content twice -> same externalID (dedup), save different content -> different externalID
- [x] T054 [P] [US6] Write unit test for local storage Delete in `internal/adapter/outbound/storage/local/adapter_test.go` — delete existing file -> success, delete non-existent -> error
- [x] T055 [P] [US6] Write unit test for document create service in `internal/domain/service/file_service_test.go` — mock ports: happy path (process + store + insert DB), MIME rejected -> 415 + no DB row, DB insert fails -> file cleaned up
- [x] T056 [P] [US6] Write unit test for document delete service in `internal/domain/service/file_service_test.go` — mock ports: delete with unique externalID -> row + file deleted, delete with shared externalID -> row deleted + file kept, not found -> 404
- [x] T057 [P] [US6] Write unit test for document handlers in `internal/adapter/inbound/http/document_handler_test.go` — POST multipart -> 201, GET meta -> 200, GET content -> 200, DELETE -> 200 with authorizationId/tagsetId, PATCH -> 200, 404 for missing

### Implementation for User Story 6

- [x] T058 [US6] Implement local storage Save in `internal/adapter/outbound/storage/local/adapter.go` — call domain hash function from `internal/domain/service/hash.go`, write to temp file then atomic rename to `{LOCAL_STORAGE_PATH}/{externalID}`, dedup (skip if exists), return StoredFile
- [x] T059 [US6] Implement local storage Delete in `internal/adapter/outbound/storage/local/adapter.go` — remove file by externalID, log warning if not found
- [x] T060 [US6] Implement CreateDocument in Alkemio DB adapter `internal/adapter/outbound/alkemiodb/adapter.go` — generate UUIDv7 via `uuid.NewV7()`, call sqlc `CreateDocument` query with all fields, return document ID
- [x] T061 [US6] Implement DeleteDocument in Alkemio DB adapter `internal/adapter/outbound/alkemiodb/adapter.go` — call sqlc `DeleteDocument` query, return authorizationId + tagsetId (DeletedDocument struct)
- [x] T062 [US6] Implement CountByExternalID in Alkemio DB adapter `internal/adapter/outbound/alkemiodb/adapter.go` — call sqlc `CountDocumentsByExternalID` query
- [x] T063 [US6] Implement UpdateLocation in Alkemio DB adapter `internal/adapter/outbound/alkemiodb/adapter.go` — call sqlc `UpdateDocumentLocation` query (storageBucketId, temporaryLocation, updatedDate)
- [x] T064 [US6] Implement `CreateDocument(ctx, input CreateDocumentInput, content []byte, allowedMimeTypes []string, maxFileSize int)` in `internal/domain/service/file_service.go` — validate size -> detect MIME -> validate allowlist -> process image -> hash -> store file -> insert DB row -> if DB fails: delete file -> return document ID + StoredFile
- [x] T065 [US6] Implement `DeleteDocument(ctx, documentID)` in `internal/domain/service/file_service.go` — lookup document (404 if not found) -> count by externalID -> delete DB row -> if count was 1: delete file from storage -> return DeletedDocument{authorizationId, tagsetId}
- [x] T066 [US6] Implement `UpdateDocumentLocation(ctx, documentID, storageBucketId, temporaryLocation)` in `internal/domain/service/file_service.go` — lookup document (404 if not found) -> update DB -> return updated document
- [x] T067 [US6] Implement document handlers in `internal/adapter/inbound/http/document_handler.go` — POST /internal/document (multipart per FR-034 contract), GET /internal/document/:id/meta (FR-039), GET /internal/document/:id/content (FR-038), DELETE /internal/document/:id (FR-035), PATCH /internal/document/:id (JSON body per FR-036)
- [x] T068 [US6] Wire US6 routes in router.go — connect document handlers on `/internal/document` and `/internal/document/:id`
- [x] T069 [US6] Handle error codes: 400 (missing fields, FK failure), 413 (file too large), 415 (MIME rejected), 404 (not found), 503 (DB unavailable)

**Checkpoint**: US6 complete — file-service owns full document lifecycle. Server can delegate all document CRUD via HTTP.

---

## Phase 6: User Story 4 — Store and Link (Priority: P2)

**Goal**: `PUT /internal/document/:id/content` atomically replaces file content and updates the Document record.

**Independent Test**: PUT binary content with valid document ID -> file stored AND document record updated with new externalID, mimeType, size.

**Depends on**: US5 (image processing), US6 (DB adapter methods already implemented)

### Tests for User Story 4

- [x] T070 [P] [US4] Write unit test for UpdateFile DB adapter in `internal/adapter/outbound/alkemiodb/adapter_test.go` — update existing document -> success, update non-existent -> 0 rows -> error
- [x] T071 [P] [US4] Write unit test for store-and-link service in `internal/domain/service/file_service_test.go` — mock ports: happy path (process + store + update DB), document not found -> 404 + no file stored, DB update fails -> file cleaned up, file store fails -> no DB update
- [x] T072 [P] [US4] Write unit test for PUT content handler in `internal/adapter/inbound/http/document_handler_test.go` — 200 with externalID/mimeType/size, 404 for missing doc, 500 for failures

### Implementation for User Story 4

- [x] T073 [US4] Implement UpdateFile in Alkemio DB adapter `internal/adapter/outbound/alkemiodb/adapter.go` — call sqlc `UpdateDocumentFile` query (externalID, mimeType, size, updatedDate), return error if 0 rows affected
- [x] T074 [US4] Implement `StoreAndLink(ctx, documentID, content)` in `internal/domain/service/file_service.go` — lookup document (404 if not found) -> process image -> hash -> store file -> update DB (externalID, mimeType, size, updatedDate) -> if DB fails: delete stored file -> if store fails: no DB update (FR-023 atomicity)
- [x] T075 [US4] Add PUT /internal/document/:id/content handler in `internal/adapter/inbound/http/document_handler.go` — read binary body -> service.StoreAndLink -> return JSON {externalID, mimeType, size}
- [x] T076 [US4] Handle 503 for DB unavailability on store-and-link endpoint per FR-033

**Checkpoint**: US4 complete — WOPI service can atomically replace file content + update document in one call.

---

## Phase 7: User Story 2 — Internal Document Access (Priority: P2)

**Goal**: Internal endpoints to read document content and metadata by document ID without auth.

**Independent Test**: Create a document via POST, then GET /internal/document/:id/content -> file bytes, GET /internal/document/:id/meta -> JSON metadata.

**Depends on**: US6 (document handlers already implemented — GET content and GET meta are part of document_handler.go)

**Note**: The GET endpoints were already implemented in US6 (T067). This phase validates and adds any missing test coverage.

### Tests for User Story 2

- [x] T077 [P] [US2] Write unit test for GET content handler in `internal/adapter/inbound/http/document_handler_test.go` — valid doc -> 200 with file bytes + Content-Type, missing doc -> 404, file missing on storage -> 404
- [x] T078 [P] [US2] Write unit test for GET meta handler in `internal/adapter/inbound/http/document_handler_test.go` — valid doc -> 200 with all JSON fields, missing doc -> 404

### Implementation for User Story 2

- [x] T079 [US2] Verify GET /internal/document/:id/content streams file correctly with Content-Type from document's mimeType in `internal/adapter/inbound/http/document_handler.go`
- [x] T080 [US2] Verify GET /internal/document/:id/meta returns all document fields as JSON in `internal/adapter/inbound/http/document_handler.go`
- [x] T081 [US2] Verify no auth middleware on `/internal/` routes (FR-006) in `internal/adapter/inbound/http/router.go`

**Checkpoint**: US2 complete — internal services can read document content and metadata by document ID.

---

## Phase 8: User Story 3 — Storage Backend Abstraction (Priority: P3)

**Goal**: Verify storage backend abstraction is clean and pluggable. Local adapter is the only implementation; S3 is future.

**Independent Test**: Run full test suite against local adapter — all operations pass via StoragePort interface.

### Tests for User Story 3

- [x] T082 [P] [US3] Write StoragePort contract test in `internal/domain/port/storage_test.go` — test suite that exercises Save/Read/Delete/Exists against any StoragePort implementation; verify content-addressable (same content -> same ID), idempotent writes, delete + exists consistency
- [x] T083 [US3] Run StoragePort contract test against local adapter — confirm all operations pass via the interface, not the concrete type

### Implementation for User Story 3

- [x] T084 [US3] Review and verify local adapter implements StoragePort cleanly in `internal/adapter/outbound/storage/local/adapter.go` — no storage-backend-specific details leak through the interface, file path construction is internal to adapter
- [x] T085 [US3] Verify storage backend selection is configuration-driven in `cmd/server/main.go` — `STORAGE_TYPE=local` instantiates local adapter; unknown type returns startup error; adding S3 would only require a new adapter + config case

**Checkpoint**: US3 complete — storage abstraction verified clean and ready for future S3 adapter.

---

## Phase 9: Polish & Cross-Cutting Concerns

**Purpose**: Final quality pass across all stories

- [x] T086 Run `golangci-lint run` and fix all violations
- [x] T087 Run `go test ./... -race -coverprofile=coverage.out` — 41.8% total, 73-86% on domain/adapter functions. Infrastructure packages (config, resilience, main) lower the total. Per constitution XII, no padding tests added.
- [x] T088 [P] Verify Dockerfile builds successfully and container starts with correct env vars — image 365MB, DB connects, structured JSON logging, graceful NATS failure
- [x] T089 [P] Verify all error responses return structured JSON per contracts (not plain text)
- [x] T090 Verify backward compatibility: same port (4003), same storage path layout, same public API response headers (Cache-Control, Pragma, Expires, ETag) per FR-014 through FR-020
- [ ] T091 Verify health endpoint reports correct dependency status per contracts/private-api.md (requires running service)
- [ ] T092 Run quickstart.md validation — follow the quickstart guide from scratch, verify all steps work (manual E2E)

---

## Dependencies & Execution Order

### Phase Dependencies

- **Setup (Phase 1)**: No dependencies — start immediately
- **Foundational (Phase 2)**: Depends on Phase 1 — BLOCKS all user stories
- **US1 (Phase 3)**: Depends on Phase 2 — MVP, no other story dependencies
- **US5 (Phase 4)**: Depends on Phase 2 — independent of US1
- **US6 (Phase 5)**: Depends on Phase 2 + US5 — needs image processing
- **US4 (Phase 6)**: Depends on US6 — needs DB adapter methods + document handlers
- **US2 (Phase 7)**: Depends on US6 — GET endpoints already built in US6
- **US3 (Phase 8)**: Depends on US6 — local adapter fully implemented
- **Polish (Phase 9)**: Depends on all user stories being complete

### User Story Dependencies

```
Phase 2 (Foundation)
    ├── US1 (Phase 3) ─────────────────────────────────────────┐
    ├── US5 (Phase 4) ── US6 (Phase 5) ── US4 (Phase 6) ──┐   │
    │                                    └── US2 (Phase 7) ├── US3 (Phase 8) ── Polish (Phase 9)
    └──────────────────────────────────────────────────────┘
```

### Parallel Opportunities

After Phase 2 completes, two workstreams can run in parallel:
- **Stream A**: US1 (public file serving) — the MVP
- **Stream B**: US5 (image processing) -> US6 (document CRUD) -> US4 (store-and-link) + US2 (internal access)

US3 and Polish run after all stories converge.

---

## Parallel Example: User Story 6

```
# All US6 tests can launch in parallel (different test functions):
T049 + T050 + T051 + T052 + T053 + T054 + T055 + T056 + T057

# Storage adapter implementations in parallel (different methods):
T058 + T059

# DB adapter implementations in parallel (different methods):
T060 + T061 + T062 + T063

# Service implementations sequential (depend on adapters):
T064 after T058+T060+T062 (create needs Save + Create + Count)
T065 after T059+T061+T062 (delete needs Delete + Delete + Count)
T066 after T063 (update needs UpdateLocation)

# Handler + wiring after service:
T067 after T064+T065+T066
T068 after T067
T069 after T068
```

---

## Implementation Strategy

### MVP First (User Story 1 Only)

1. Complete Phase 1: Setup
2. Complete Phase 2: Foundational (CRITICAL — blocks all stories)
3. Complete Phase 3: User Story 1
4. **STOP and VALIDATE**: Deploy as drop-in replacement for TS file-service (public endpoint only)
5. This alone provides the core value — serving files to users

### Incremental Delivery

1. Setup + Foundational -> Foundation ready
2. US1 -> Public file serving works -> **MVP deployed**
3. US5 -> Image processing + MIME detection ready
4. US6 -> Full document lifecycle management — server becomes read-only on document table
5. US4 -> WOPI service can use atomic store-and-link
6. US2 -> Internal document access verified
7. US3 -> Storage abstraction verified for future S3
8. Polish -> Coverage, linting, Dockerfile, backward compat verification

---

## Notes

- [P] tasks = different files, no dependencies on incomplete tasks
- Constitution Principle VI requires TDD — write tests first, verify they fail, then implement
- Constitution Principle XII requires >=95% unit coverage with meaningful tests only
- All generated document IDs are UUIDv7 (FR-040)
- ExternalID is never exposed in any API — all access by document ID
- Commit after each task or logical group
- Run `golangci-lint run` before each commit (Principle IX)
- All dependency versions from research.md (verified online 2026-03-30)
