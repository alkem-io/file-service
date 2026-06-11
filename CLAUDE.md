# Alkemio File Service (Go)

Go microservice replacing the existing TypeScript file-service. Serves
as the universal file I/O gateway for the Alkemio platform — handles
file read, write, delete, and existence checks with pluggable storage
backends.

## Tech Stack

- **Language**: Go 1.26
- **Database**: PostgreSQL (Alkemio DB, full CRUD on document table), pgx v5, sqlc
- **Authorization**: h2c HTTP/2 (preferred, via `AUTH_SERVICE_URL`) or NATS (fallback, via `NATS_URL`) to authorization-evaluation-service
- **Circuit Breaker**: sony/gobreaker v2 (shared `AUTH_BREAKER_*` config)
- **Identity**: Oathkeeper JWT (`alkemio_actor_id` claim) on public endpoints
- **Logging**: Zap (structured)
- **HTTP Router**: chi v5
- **Architecture**: Hexagonal (ports and adapters)
- **Replaces**: `/Users/antst/work/alkemio/file-service` (TypeScript/NestJS)
- **Reference services**:
  - Alkemio Server: `/Users/antst/work/alkemio/server`
  - WOPI Service: `/Users/antst/work/alkemio/wopi-service`
  - Auth Evaluation: `/Users/antst/work/alkemio/authorization-evaluation-service`
  - Matrix Adapter: `/Users/antst/work/alkemio/matrix-adapter-go`
  - OIDC Service: `/Users/antst/work/alkemio/oidc-service`

## Architecture

Two classes of endpoints:

**Public** (behind Oathkeeper):
- `GET /rest/storage/document/:id` — serve file with auth check
  (drop-in replacement for existing TS file-service)

**Private** (cluster-internal, no auth):
- `POST /internal/document` — create document (multipart upload)
- `GET /internal/document/:id/meta` — document metadata
- `GET /internal/document/:id/content` — read file content
- `PUT /internal/document/:id/content` — replace file content
- `PATCH /internal/document/:id` — update document location
- `DELETE /internal/document/:id` — delete document

Storage is abstracted behind a port interface. File content is
addressed by SHA3-256 content hash, consistent with existing
Alkemio convention.

## Anti-Patterns — Strictly Prohibited

1. Do not apply speculative fixes — find root cause first
2. Do not keep code "just in case" or for backward compatibility
   unless explicitly requested
3. Do not duplicate logic — find or create a single shared
   implementation
4. Do not add superficial tests for coverage padding
5. Do not invent performance SLAs without evidence
6. Do not create abstractions for hypothetical future needs
7. Do not add comments explaining obvious code
8. Do not rely on training data for dependency versions — check
   online
9. Do not create documentation files unless explicitly requested
10. Do not assume — ask or search when something is unclear

## Development Workflow

- Always run `golangci-lint run` before committing
- Tests must defend real invariants — no coverage-padding tests
- Root cause analysis is mandatory before any bug fix; document the
  cause with evidence
- Verify latest dependency versions online (pkg.go.dev, GitHub
  releases) — never trust training data
- If something is unclear, ask or search — do not guess
- Use `actorId` internally, never `userId`

## Configuration (env vars)

Alkemio DB (full CRUD on document table):
- `ALKEMIO_DATABASE_HOST/PORT/USERNAME/PASSWORD/NAME`

Auth transport (set at least one; h2c preferred if both set):
- `AUTH_SERVICE_URL` — h2c HTTP/2 URL for auth-evaluation-service (e.g. `http://auth-service:6060`)
- `NATS_URL` — NATS server URL (fallback when AUTH_SERVICE_URL is not set)

Circuit breaker (shared by both auth transports):
- `AUTH_BREAKER_FAILURE_THRESHOLD` (default 3)
- `AUTH_BREAKER_TIMEOUT_SECONDS` (default 15)
- `AUTH_BREAKER_HALF_OPEN_MAX_REQUESTS` (default 2)

Storage:
- `STORAGE_TYPE` — `local` or `s3` (future)
- `LOCAL_STORAGE_PATH` — filesystem path for local storage

Service:
- `PORT` — HTTP listen port (default 4003)
- `DOCUMENT_MAX_AGE` — Cache-Control max-age in seconds (default 86400)

## Integration Context

- Auth checks on public endpoints via h2c HTTP/2 (preferred)
  or NATS `auth.evaluate` (fallback) — both carry
  actorId + privilege + authorizationPolicyId
- Document table (full CRUD) in Alkemio's PostgreSQL
- Actor identity from Oathkeeper JWT (`alkemio_actor_id` claim)
- Oathkeeper config at
  `/Users/antst/work/alkemio/server/.build/ory/oathkeeper/`
- Existing TS file-service at `/Users/antst/work/alkemio/file-service`

## Full Constitution

See `.specify/memory/constitution.md` for the complete set of
principles and governance rules.

## Active Technologies
- Go 1.26 (constitution-mandated) + govips v2.18.0 (libvips bindings — `Orientation()`, `RemoveMetadata()`, format exports), mimetype v1.4.13, chi v5.2.5, pgx v5.9.1, sqlc, zap v1.27.1
- PostgreSQL (Alkemio shared DB; this service owns the `file` table and adds `content_metadata` JSONB) and local filesystem (file bytes; content-addressed by SHA3-256)

## Recent Changes
- 019-preserve-mime-on-edit: MIME type preservation on content replace (reconcile + 422 rejections) and boot-time MIME repair job
- 018-image-orient-dims: Added Go 1.26 (constitution-mandated) + govips v2.18.0 (libvips bindings), mimetype v1.4.13, chi v5.2.5, pgx v5.9.1, sqlc, zap v1.27.1
