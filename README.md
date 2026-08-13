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

When enabled, every committed **non-temporary** document create / content-replace, plus every
temporary→permanent promotion, commits a `file_backup_outbox` row **in the same transaction**
as the `file` write (so there is never a committed file without its backup hint, and never an
outbox row without a file). After the commit the service emits a best-effort
`NOTIFY file_backup_outbox` to wake the downstream backup worker; the durable table plus the
worker's poll floor cover a lost notification. Temporary-location objects, and the flag-off
path, enqueue nothing.

`FILE_BACKUP_HOT_MIME_PREFIXES` marks user-authored, non-reconstructable classes
(office/OOXML, ODF, legacy Word/Excel/PowerPoint, Yjs) as priority `1` (hot) for the
lowest effective RPO; everything else is priority `0`.

**Table ownership:** the `file_backup_outbox` DDL is a **server-owned migration** — it is
*not* created here. file-service only performs the transactional DML (enqueue) and an
hourly prune of `done` rows older than `FILE_BACKUP_OUTBOX_DONE_RETENTION_HOURS`, keeping
the shared outbox bounded. `db/schema/outbox.sql` is a sqlc codegen mirror only.

Producer activity is counted on the expvar endpoint (`/internal/debug/vars`):
`file_backup_outbox_enqueued_total`, `file_backup_outbox_pruned_total`, and
`file_backup_outbox_orphaned_total`.

## Maintenance subcommands (one-shot Jobs)

The binary dispatches on its first argument. The default is `serve` (the long-running
HTTP server); the rest are finite, converging migrations run as **manually-triggered
k8s Jobs**, never on a schedule and never at service start-up.

```bash
file-service serve                              # default
file-service sweep-dims                         # backfill image dimensions (spec 019/020)
file-service sweep-cids [--dry-run] [--rate N]  # normalize legacy blob names (spec 018)
```

### `sweep-cids` — legacy IPFS-CID → SHA3-256 blob names

> Summarized here for a reader already in this repo. The **contract** is
> `agents-hq/specs/018-legacy-cid-normalization/contracts/sweep-cids-cli.md`; it wins
> on any disagreement.

Objects written before content addressing are named by an IPFS CID, so SHA3-256 of
their bytes can never equal their name and file-backup-service refuses them — they are
**unbackable, therefore data-at-risk** (see `alkem-io/file-service#63`, bucket A). This
sweep re-addresses them under the digest of whatever bytes are on the store, repoints
every referencing record, and reclaims the legacy blob once nothing names it.

Per object the order is **publish → repoint → reclaim**, which is what makes it safe
against live traffic: at no instant does a record name a blob that is absent. Every
record write is a compare-and-set on `(id, externalID, version)`, so a concurrent
content Replace or a temporary→permanent promotion wins and the sweep skips.

- **Irreversible.** The legacy blob is reclaimed in the same pass. Run `--dry-run`
  first — it enumerates through the same predicate and changes nothing at all.
- **It interacts with the other externalID-guarded writers, harmlessly.** `sweep-dims`
  and the boot-time MIME repair both compare-and-set on a record's *current* name, so any
  row this sweep renames underneath them fails that guard and is skipped. Nothing
  corrupts, and nothing is lost: both re-derive their work-lists from self-clearing
  predicates, so a skipped row is simply picked up on their next run (MIME repair runs on
  every serve-pod boot; `sweep-dims` on its next Job). Sequencing them is therefore an
  efficiency choice, not a correctness requirement — which matters, because MIME repair
  starts whenever a pod boots and no operator controls that.
- `--rate` bounds objects/second (default: a conservative built-in `5`). Anything that is
  not a positive, finite number is rejected rather than read as "unlimited" — `NaN` and
  `Inf` parse successfully and would otherwise slip through.
- **No positional arguments.** The flag parser stops at the first non-flag operand, so
  `sweep-cids prod --dry-run` would parse cleanly with `--dry-run` silently dropped. Any
  positional argument exits `2`.
- Exit `1` means the pass ended early, a record genuinely failed, the run report could not
  be written, or the content store failed the pre-flight. A record whose content is absent
  is a legitimate skip and never fails the run.
- A pass whose store is **missing or empty refuses to run**: an unmounted volume makes
  every read look like permanently-absent content, which would otherwise exit `0` and brand
  the whole corpus unrecoverable in the one durable record of the migration.
- **Every affected object's ETag changes.** The public read path derives the validator from
  the blob name, so renaming the cohort invalidates every cached and CDN copy of content
  whose bytes never changed. Expect revalidation traffic proportional to how much of the
  cohort is hot; `--rate` bounds the sweep's own load, not the cache churn that follows.
- **The renamed objects are not enqueued for backup by this pass.** It performs no content
  write, so it never touches the backup queue. Making them backable is the point; backing
  them up is the `file-backup-service` backfill that runs *after* this sweep and enumerates
  the `file` table directly. Run the sweep first — a backfill before it repeats the
  acceptance mass-failure at production scale.
- **One pre-existing race is narrowed, not closed.** Reclamation counts references and then
  deletes; a copy that read its source row before the repoint can insert a row naming the
  legacy blob inside that window. The standing fix for that class is atomic GC. The sweep
  re-reads the count after deleting and logs loudly if a reference appeared — and the bytes
  stay recoverable under the new digest on the journal line above.
- Each real pass writes a JSON **run report** to `<LOCAL_STORAGE_PATH>/_sweep-reports/`,
  recording the previous and new name of every record it changed. Since the legacy blob
  is gone afterwards, this is the only way to reconstruct the mapping. The reserved
  directory name is one the store's key rules can never produce, so no enumeration of
  the store can mistake a report for content. The absolute path is the last line the
  Job logs. Alongside it, a `.ndjson` **journal** records each mapping *before* the blob
  it names is reclaimed, so a pass killed mid-corpus still leaves a complete record of
  everything it destroyed. A run that cannot write either **exits 1**.

An in-repo Job manifest ships at `manifests/36-file-service-sweep-cids-job.yaml`. It ships
with `--dry-run` set and three fail-closed placeholders (image, PVC claim, and the flag
itself), so a verbatim apply can never run the irreversible pass against the wrong build
or the wrong volume.

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
