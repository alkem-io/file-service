# Alkemio File Service (Go)

Go microservice replacing the existing TypeScript file-service. Serves
as the universal file I/O gateway for the Alkemio platform — handles
file read, write, delete, and existence checks with pluggable storage
backends.

## Tech Stack

- **Language**: Go 1.25
- **Database**: PostgreSQL (Alkemio DB read-only + own DB if needed), pgx v5, sqlc
- **Authorization**: NATS via authorization-evaluation-service (`auth.evaluate`)
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

Database (matching oidc-service pattern):
- `FILE_SERVICE_DATABASE_HOST/PORT/USERNAME/PASSWORD/NAME/TIMEOUT`

Alkemio DB (read-only):
- `ALKEMIO_DATABASE_HOST/PORT/USERNAME/PASSWORD/NAME`

NATS:
- `NATS_URL` — NATS server URL

Storage:
- `STORAGE_TYPE` — `local` or `s3` (future)
- `LOCAL_STORAGE_PATH` — filesystem path for local storage

Service:
- `FILE_SERVICE_PORT` — HTTP listen port

## Integration Context

- Auth checks on public endpoints via NATS `auth.evaluate`
  (agentId + privilege + authorizationPolicyId)
- Document metadata (externalID, authorizationPolicyId) from
  Alkemio's PostgreSQL (read-only user)
- Actor identity from Oathkeeper JWT (`alkemio_actor_id` claim)
- Oathkeeper config at
  `/Users/antst/work/alkemio/server/.build/ory/oathkeeper/`
- Existing TS file-service at `/Users/antst/work/alkemio/file-service`

## Full Constitution

See `.specify/memory/constitution.md` for the complete set of
principles and governance rules.

## Active Technologies
- Go 1.25 (constitution-mandated) + chi v5.2.5 (HTTP), pgx v5.9.1 (DB), sqlc v1.30.0 (codegen), zap v1.27.1 (logging), nats.go v1.50.0 (messaging), govips v2.17.0 (image processing), mimetype v1.4.13 (MIME detection), x/crypto v0.49.0 (SHA3-256) (001-file-service-init)
- PostgreSQL (Alkemio DB, read + limited write on document table), local filesystem (file bytes) (001-file-service-init)
- PostgreSQL (Alkemio DB, full CRUD on document table, read-only on all others), local filesystem (file bytes) (001-file-service-init)
- Go 1.25 (constitution-mandated) + chi v5.2.5 (HTTP), pgx v5.9.1 (DB), sqlc v1.30.0 (codegen), zap v1.27.1 (logging), nats.go v1.50.0 (messaging), govips v2.17.0 (image processing), mimetype v1.4.13 (MIME detection), x/crypto v0.49.0 (SHA3-256), google/uuid (UUIDv7) (001-file-service-init)

## Recent Changes
- 001-file-service-init: Added Go 1.25 (constitution-mandated) + chi v5.2.5 (HTTP), pgx v5.9.1 (DB), sqlc v1.30.0 (codegen), zap v1.27.1 (logging), nats.go v1.50.0 (messaging), govips v2.17.0 (image processing), mimetype v1.4.13 (MIME detection), x/crypto v0.49.0 (SHA3-256)
