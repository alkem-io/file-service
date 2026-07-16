# Alkemio File Service (Go)

Universal file I/O gateway and document lifecycle manager for the
[Alkemio](https://alkem.io) platform. Replaces the TypeScript file-service
as a drop-in replacement with additional write/delete capabilities.

## Features

- **Public file serving** with Oathkeeper JWT auth and authorization checks
- **Document CRUD** (create, read, update, delete) via internal endpoints
- **Image processing**: HEIC-to-JPEG conversion, compression, physical EXIF-orientation rotation for all raster formats, ICC profile preservation (libvips)
- **Image dimensions on the wire**: Create / Copy / Replace-content / Patch responses carry post-rotation `imageWidth` / `imageHeight` for every `image/*` upload (rasters, SVG, GIF) — consumers don't decode bytes locally
- **Content-addressable storage** (SHA3-256 hash as externalID)
- **h2c HTTP/2 auth** (preferred) with circuit breaker, NATS fallback
- **Hexagonal architecture** (ports and adapters)
- **Multi-arch Docker** (amd64/arm64)

## Endpoints

| Method | Path | Auth | Description |
|--------|------|------|-------------|
| GET | `/rest/storage/document/:id` | Oathkeeper JWT | Serve file (public) |
| POST | `/internal/document` | none (cluster) | Create document |
| GET | `/internal/document/:id/meta` | none | Document metadata |
| GET | `/internal/document/:id/content` | none | Read file content |
| PUT | `/internal/document/:id/content` | none | Replace file content |
| PATCH | `/internal/document/:id` | none | Update document location |
| DELETE | `/internal/document/:id` | none | Delete document |
| GET | `/health` | none | Health check |

## Prerequisites

- Go 1.26+
- PostgreSQL (Alkemio DB with document table)
- Auth service via h2c HTTP/2 (`AUTH_SERVICE_URL`) or NATS (`NATS_URL`)
- libvips >= 8.10 (`brew install vips` / `apt install libvips-dev`)

## Quick Start

```bash
cp .env.example .env
# Edit .env with your database and auth service settings

# Run
go run ./cmd/server/

# Or build and run
make build
./bin/file-service
```

## Configuration

### Required

| Variable | Example | Description |
|----------|---------|-------------|
| `AUTH_SERVICE_URL` or `NATS_URL` | `http://localhost:6060` | Auth transport (at least one; h2c preferred) |
| `ALKEMIO_DATABASE_HOST` | `localhost` | Alkemio DB host |
| `ALKEMIO_DATABASE_PORT` | `5432` | Alkemio DB port |
| `ALKEMIO_DATABASE_USERNAME` | `alkemio_file_svc` | DB user |
| `ALKEMIO_DATABASE_PASSWORD` | `secret` | DB password |
| `ALKEMIO_DATABASE_NAME` | `alkemio` | Database name |

### Optional

| Variable | Default | Description |
|----------|---------|-------------|
| `PORT` | `4003` | HTTP listen port |
| `LOCAL_STORAGE_PATH` | `../server/.storage` | File storage root |
| `STORAGE_TYPE` | `local` | Storage backend |
| `DOCUMENT_MAX_AGE` | `86400` | Cache-Control max-age (seconds) |
| `AUTH_BREAKER_FAILURE_THRESHOLD` | `3` | Circuit breaker threshold (h2c) |
| `AUTH_BREAKER_TIMEOUT_SECONDS` | `15` | Circuit breaker open duration |
| `AUTH_BREAKER_HALF_OPEN_MAX_REQUESTS` | `2` | Half-open test requests |
| `FILE_BACKUP_OUTBOX_ENABLED` | `false` | Turn the continuous-backup outbox producer on (see below) |
| `FILE_BACKUP_HOT_MIME_PREFIXES` | office/ODF/Yjs prefixes | CSV of MIME prefixes enqueued at hot priority (=1) |
| `FILE_BACKUP_OUTBOX_DONE_RETENTION_HOURS` | `24` | How long consumer-finished (`done`) outbox rows are kept before the prune drops them |

### NATS-specific (only when NATS transport is active)

| Variable | Default | Description |
|----------|---------|-------------|
| `NATS_SUBJECT` | `auth.evaluate` | Auth subject |
| `NATS_RECONNECT_WAIT_MS` | `1000` | Initial reconnect delay |
| `NATS_RECONNECT_MAX_WAIT_MS` | `30000` | Max reconnect delay |

## Continuous-backup outbox producer (`FILE_BACKUP_OUTBOX_ENABLED`)

Part of the cross-repo `008-continuous-file-backup` feature. **Off by default** — a
pure opt-in that leaves the original write paths byte-for-byte unchanged.

When enabled, every committed **non-temporary** document create / content-replace also
commits a `file_backup_outbox` row **in the same transaction** as the `file` write (so
there is never a committed file without its backup hint, and never an outbox row without
a file). After the commit the service emits a best-effort `NOTIFY file_backup_outbox` to
wake the downstream backup worker; the durable table plus the worker's poll floor cover a
lost notification. Temporary-location objects, and the flag-off path, enqueue nothing.

`FILE_BACKUP_HOT_MIME_PREFIXES` marks user-authored, non-reconstructable classes
(office/OOXML, ODF, legacy Word/Excel/PowerPoint, Yjs) as priority `1` (hot) for the
lowest effective RPO; everything else is priority `0`.

**Table ownership:** the `file_backup_outbox` DDL is a **server-owned migration** — it is
*not* created here. file-service only performs the transactional DML (enqueue) and an
hourly prune of `done` rows older than `FILE_BACKUP_OUTBOX_DONE_RETENTION_HOURS`, keeping
the shared outbox bounded. `db/schema/outbox.sql` is a sqlc codegen mirror only.

Producer activity is counted on the expvar endpoint (`/internal/debug/vars`):
`file_backup_outbox_enqueued_total` and `file_backup_outbox_pruned_total`.

## Development

```bash
make lint          # golangci-lint
make test          # unit tests (no libvips)
make test-vips     # full tests (with libvips)
make openapi       # regenerate OpenAPI spec
make sqlc-generate # regenerate sqlc code
make build         # build with vips support
make build-stub    # build without vips (for CI)
```

## Project Structure

```
cmd/server/              Entry point, dependency wiring
internal/
  adapter/
    inbound/http/        Chi router, handlers, middleware, DTOs
    outbound/
      alkemiodb/         pgx/sqlc adapter (document table CRUD)
      authhttp/          h2c HTTP/2 auth client + gobreaker
      nats/              NATS auth client (fallback)
      storage/local/     Local filesystem storage
  config/                Env var loading and validation
  domain/
    model/               Document, StoredFile, AuthResult
    port/                Interfaces (StoragePort, AuthPort, DocumentRepo)
    service/             Business logic orchestration
  imaging/               govips: HEIC conversion, compression, MIME detection
  resilience/            NATS reconnection with exponential backoff
db/
  queries/               sqlc SQL files
  schema/                SQL schema for sqlc
```

## Routing

```
Public (through Oathkeeper):
  /api/private/rest/storage/document/:id -> /rest/storage/document/:id

Internal (Traefik strips /api/storage in Docker dev):
  /api/storage/internal/document/...     -> /internal/document/...

K8s (direct):
  http://storage-service:4003/internal/document/...
```

## Tech Stack

Go 1.26 | chi v5 | pgx v5 + sqlc | zap | govips | sony/gobreaker v2

## License

[EUPL-1.2](https://opensource.org/licenses/EUPL-1.2)
