# Research: Go File Service Implementation

**Branch**: `001-file-service-init` | **Date**: 2026-03-30

## Dependency Versions (verified online 2026-03-30)

| Package | Version | Notes |
|---------|---------|-------|
| Go | 1.25.8 | Latest patch for Go 1.25 (1.26.1 also available; constitution specifies 1.25) |
| `github.com/jackc/pgx/v5` | v5.9.1 | PostgreSQL driver |
| `github.com/sqlc-dev/sqlc` | v1.30.0 | SQL code generator (CLI tool) |
| `github.com/go-chi/chi/v5` | v5.2.5 | HTTP router (v5.2.4 had security vuln GO-2026-4316) |
| `go.uber.org/zap` | v1.27.1 | Structured logging |
| `github.com/nats-io/nats.go` | v1.50.0 | NATS client |
| `golang.org/x/crypto` | v0.49.0 | SHA3-256 via `golang.org/x/crypto/sha3` |
| `github.com/davidbyttow/govips/v2` | v2.17.0 | libvips bindings — HEIC→JPEG, compression, resize |
| `github.com/gabriel-vasile/mimetype` | v1.4.13 | MIME detection from magic bytes |

## Decision: SHA3-256 Hashing

- **Decision**: Use `golang.org/x/crypto/sha3` package
- **Rationale**: Standard Go extended library. `sha3.New256()` produces identical output to Node.js `createHash('sha3-256')` — both implement FIPS 202 / Keccak-based SHA3-256, producing 32 bytes (64 hex chars).
- **Alternatives considered**: `github.com/minio/sha256-simd` (SHA2 only, not SHA3), hand-rolled Keccak (unnecessary complexity).
- **Verification**: Must write a compatibility test comparing Go output to known Node.js hash of a reference buffer.

## Decision: Image Processing Library

- **Decision**: Use `github.com/davidbyttow/govips/v2` (libvips bindings)
- **Rationale**: libvips is the industry standard for server-side image processing — same engine used by sharp (Node.js). Supports HEIC/HEIF decoding, JPEG/WebP encoding, resize, EXIF strip, auto-orient. MozJPEG-quality output via `VipsForeignJpegSubsample` options. Single dependency covers all image processing needs (HEIC→JPEG, compression, resize).
- **Alternatives considered**:
  - `github.com/disintegration/imaging` — pure Go, no HEIC support
  - `github.com/h2non/bimg` — also libvips but less maintained than govips
  - Direct `image/jpeg` stdlib — no HEIC, no MozJPEG quality, limited resize options
- **Runtime dependency**: Requires `libvips` (>= 8.10) installed on the system. Dockerfile must install it.

## Decision: MIME Detection

- **Decision**: Use `github.com/gabriel-vasile/mimetype`
- **Rationale**: Detects MIME from magic bytes (file content), not file extension. Covers all 24 MIME types in Alkemio's allowlist (images, Office docs, ODF, PDF). Pure Go, no C dependencies. Falls back to `application/octet-stream` for unknown types.
- **Alternatives considered**:
  - `net/http.DetectContentType` — limited to ~40 types, misidentifies Office XML formats
  - `github.com/h2non/filetype` — fewer supported types, less accurate for documents

## Decision: NATS Auth Contract

- **Decision**: Use message envelope format matching WOPI service pattern
- **Rationale**: The authorization-evaluation-service accepts both bare `EvaluationRequest` and envelope-wrapped format. The WOPI service uses the envelope (`{pattern, data, id}`), which is the established client pattern in the Alkemio ecosystem.
- **Request format**:
  ```json
  {
    "pattern": "evaluate",
    "data": {
      "agentId": "<uuid>",
      "privilege": "read",
      "authorizationPolicyId": "<uuid>"
    }
  }
  ```
- **Response format**:
  ```json
  {
    "allowed": true|false,
    "reason": "<string>",
    "error": { "code": "<string>", "dependency": "<string>", "retryAfterMs": <int> }
  }
  ```
- **Client method**: `nats.Conn.RequestWithContext(ctx, "auth.evaluate", payload)` — synchronous request-reply with context timeout.
- **Circuit breaker**: File-service should implement its own NATS circuit breaker (matching auth-eval-service config: `NATS_FAILURE_THRESHOLD`, `NATS_BREAKER_TIMEOUT_SECONDS`, `NATS_HALF_OPEN_MAX_REQUESTS`).

## Decision: UUIDv7 for Generated IDs

- **Decision**: Use `github.com/google/uuid` v1.6.0+ for UUIDv7 generation (RFC 9562)
- **Rationale**: UUIDv7 provides time-ordered, sortable identifiers. Better DB index performance than UUIDv4. Go's `google/uuid` package supports v7 since v1.6.0 via `uuid.NewV7()`.
- **Alternatives considered**: UUIDv4 (random, not sortable), ULID (non-standard), custom snowflake (unnecessary complexity).

## Decision: Endpoint Layout

- **Decision**: Public at `/rest/storage/document/:id` (backward compat), internal at `/internal/...` (clean paths). ExternalID never exposed in any API.
- **Rationale**: The service sees clean paths. Traefik handles the `/api/storage` prefix in Docker dev (strips it). K8s services call `/internal/...` directly. No long prefixes baked into application code.
- **Sub-resources**: `/document/:id/content` for file bytes, `/document/:id/meta` for metadata JSON.
- **Routing convention**: `private` = through Oathkeeper (authenticated), `internal` = cluster-only (no auth).

## Decision: Document Table Ownership

- **Decision**: File-service owns `document` table (full CRUD). Server becomes read-only.
- **Rationale**: Centralizes document + file operations. Enables atomic create/delete, proper file cleanup (fixing `DELETE_FILE=false` gap), future orphan cleanup. Server creates auth_policy + tagset first, passes IDs to file-service.
- **Alternatives considered**: Server keeps document CRUD (current state) — rejected because it splits file+record operations across services with no transactional guarantee.

## Decision: Project Structure

- **Decision**: Follow WOPI service hexagonal layout (most recent Alkemio Go service)
- **Rationale**: Consistent with existing Alkemio Go services. Clean separation of domain (ports, models, services) from adapters (HTTP, DB, NATS, storage).
- **Layout**:
  ```
  cmd/server/           — main.go entry point
  internal/
    adapter/
      inbound/http/     — chi router, handlers, middleware
      outbound/
        alkemiodb/      — pgx/sqlc adapter for Alkemio DB (full CRUD on document table)
        nats/           — NATS auth client adapter
        storage/local/  — local filesystem storage adapter
    config/             — env var loading, validation
    domain/
      model/            — domain types (Document, StoredFile, etc.)
      port/             — port interfaces (StoragePort, AuthPort, DocumentRepo, etc.)
      service/          — business logic (file upload, serve, store-and-link, image processing)
    imaging/            — image processing (HEIC conversion, compression, resize)
    resilience/         — circuit breaker, retry logic
  db/
    queries/            — sqlc .sql query files
    schema/             — SQL schema for sqlc (read from Alkemio DB)
  ```
