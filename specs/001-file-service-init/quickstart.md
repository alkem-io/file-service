# Quickstart: Go File Service

**Branch**: `001-file-service-init`

## Prerequisites

- Go 1.25+
- PostgreSQL (Alkemio DB with document table)
- NATS server
- libvips >= 8.10 (`brew install vips` on macOS, `apt install libvips-dev` on Debian/Ubuntu)
- golangci-lint
- sqlc CLI v1.30.0

## Environment Variables

### Required

| Variable | Example | Description |
|----------|---------|-------------|
| `NATS_URL` | `nats://localhost:4222` | NATS server URL |
| `ALKEMIO_DATABASE_HOST` | `localhost` | Alkemio DB host |
| `ALKEMIO_DATABASE_PORT` | `5432` | Alkemio DB port |
| `ALKEMIO_DATABASE_USERNAME` | `alkemio_file_svc` | DB user with full CRUD on document table, read-only on others |
| `ALKEMIO_DATABASE_PASSWORD` | `secret` | DB password |
| `ALKEMIO_DATABASE_NAME` | `alkemio` | Database name |

### Optional (with defaults)

| Variable | Default | Description |
|----------|---------|-------------|
| `PORT` | `4003` | HTTP listen port |
| `LOCAL_STORAGE_PATH` | `../server/.storage` | File storage root |
| `STORAGE_TYPE` | `local` | Storage backend (`local` or `s3` future) |
| `DOCUMENT_MAX_AGE` | `86400` | Cache-Control max-age (seconds) |
| `NATS_SUBJECT` | `auth.evaluate` | NATS auth subject |
| `NATS_RECONNECT_WAIT_MS` | `1000` | Initial reconnect delay |
| `NATS_RECONNECT_MAX_WAIT_MS` | `30000` | Max reconnect delay |
| `NATS_FAILURE_THRESHOLD` | `3` | Circuit breaker failure threshold |
| `NATS_BREAKER_TIMEOUT_SECONDS` | `15` | Circuit breaker open duration |
| `NATS_HALF_OPEN_MAX_REQUESTS` | `2` | Half-open test requests |

## Local Development

```bash
# Install system dependencies (macOS)
brew install vips

# Generate sqlc code
sqlc generate

# Run linter
golangci-lint run

# Run tests
go test ./... -race -cover

# Run service
go run ./cmd/server/
```

## Project Structure

```
cmd/server/main.go          — entry point, wiring
internal/
  adapter/
    inbound/http/           — chi router, handlers, middleware (JWT, logging)
    outbound/
      alkemiodb/            — pgx/sqlc adapter for document table
      nats/                 — NATS auth client (auth.evaluate)
      storage/local/        — local filesystem storage adapter
  config/                   — env var loading, validation
  domain/
    model/                  — Document, StoredFile, AuthResult
    port/                   — StoragePort, AuthPort, DocumentRepo, ImageProcessor
    service/                — file upload, serve, store-and-link, document CRUD orchestration
  imaging/                  — govips wrapper: HEIC→JPEG, compress, resize, MIME detect
  resilience/               — circuit breaker, NATS connection management
db/
  queries/                  — sqlc .sql files
  schema/                   — SQL schema (document table DDL for sqlc)
```

## Key Flows

### Public file serve (GET /rest/storage/document/:id)
1. Extract actor ID from JWT
2. Query document by ID → get externalID, authorizationPolicyId, mimeType
3. NATS auth.evaluate(agentId, "read", authorizationPolicyId)
4. If allowed → read file from storage → stream with headers
5. If denied → 403

### Internal file serve (GET /internal/document/:id/content)
1. Lookup document by ID (404 if not found)
2. Read file from storage by externalID
3. Stream with Content-Type from document's mimeType

### Internal metadata (GET /internal/document/:id/meta)
1. Lookup document by ID (404 if not found)
2. Return all document fields as JSON

### Document create (POST /internal/document)
1. Server creates authorization_policy + tagset rows → gets authorizationId, tagsetId
2. Server calls file-service with file + metadata + authorizationId + tagsetId
3. File-service: detect MIME → validate → process image → hash → store file
4. File-service: create document row with all fields + computed externalID, mimeType, size
5. Return document ID, externalID, mimeType, size

### Document delete (DELETE /internal/document/:id)
1. Lookup document by ID (404 if not found)
2. Count documents with same externalID
3. Delete document row from database
4. If count was 1 → delete file from storage (no other references)
5. Return authorizationId + tagsetId → server cleans up those rows

### Replace file content (PUT /internal/document/:id/content)
1. Lookup document by ID (404 if not found)
2. Process image + compute hash + store new file
3. Update document record: externalID, mimeType, size, updatedDate
4. Atomic: rollback on partial failure
5. Return externalID, mimeType, size

### Document move (PATCH /internal/document/:id)
1. Lookup document by ID (404 if not found)
2. Update storageBucketId + temporaryLocation
3. Return updated document

## Routing

```
Public (through Oathkeeper, Traefik strips /api/private):
  /api/private/rest/storage/document/:id → service sees /rest/storage/document/:id

Internal (Traefik strips /api/storage in Docker dev):
  /api/storage/internal/document/...     → service sees /internal/document/...

K8s production (direct, no Traefik):
  http://storage-service:4003/internal/document/...
```
