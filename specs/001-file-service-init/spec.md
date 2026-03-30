# Feature Specification: Go File Service Implementation

**Feature Branch**: `001-file-service-init`
**Created**: 2026-03-30
**Status**: Draft
**Input**: User description: "Implement Go file service replacing TypeScript file-service with public and private endpoints"

## User Scenarios & Testing *(mandatory)*

### User Story 1 - Serve Files to Authenticated Users (Priority: P1)

A user in the Alkemio platform clicks to download or view a document.
The request passes through Oathkeeper (which injects a JWT with the
actor's identity) and reaches the file service's public endpoint. The
service looks up the document in Alkemio's database to find its
externalID and authorizationPolicyId, calls the authorization-evaluation-service
via NATS to verify the actor has READ privilege, then reads the file
from the storage backend and streams it to the client with appropriate
Content-Type and caching headers.

**Why this priority**: This is the core use case — the existing TS
file-service does exactly this. Without it, nothing else works.

**Independent Test**: Can be tested by sending a GET request with a
valid Oathkeeper JWT for a document the actor has READ access to and
verifying the file content is returned with correct headers.

**Acceptance Scenarios**:

1. **Given** a valid JWT with actor ID and a document the actor has READ permission for, **When** a GET request is sent to `/rest/storage/document/:id`, **Then** the service returns the file content with correct Content-Type, Cache-Control, and ETag headers.
2. **Given** a valid JWT but the actor lacks READ permission, **When** a GET request is sent, **Then** the service returns 403 Forbidden.
3. **Given** a valid JWT but the document does not exist, **When** a GET request is sent, **Then** the service returns 404 Not Found.
4. **Given** no JWT or an invalid JWT, **When** a GET request is sent, **Then** the service returns 401 Unauthorized.
5. **Given** the file exists on disk and has been served before, **When** the client sends an ETag-based conditional request, **Then** the service returns 304 Not Modified.
6. **Given** a file previously stored by the existing TypeScript file-service at `{LOCAL_STORAGE_PATH}/{externalID}`, **When** the Go service receives a request for that document, **Then** it serves the file correctly — zero-migration drop-in replacement.

---

### User Story 2 - Internal Document Access (Priority: P2)

Internal Alkemio services (WOPI service, Alkemio Server) need to
read document content and metadata by document ID without
authorization overhead. These endpoints are cluster-internal only
(not routed through Oathkeeper). In the Docker dev setup, they are
accessible via Traefik at `/api/storage/internal/...` (Traefik strips
`/api/storage`, service receives `/internal/...`). In K8s production,
internal services call `http://storage-service:4003/internal/...`
directly via K8s service DNS.

File writes, deletes, and existence checks are handled through the
document-level endpoints (US6) — there is no need for file-only
write/delete since every file in Alkemio is a Document. ExternalID
is a purely internal implementation detail of the file-service, never
exposed in any API.

**Why this priority**: Internal services need to read file content
and document metadata without auth overhead.

**Independent Test**: Can be tested by creating a document via
`POST /internal/document`, then reading content via
`GET /internal/document/:id/content` and metadata via
`GET /internal/document/:id/meta`.

**Acceptance Scenarios**:

1. **Given** a valid document ID, **When** GET is sent to `/internal/document/:id/content`, **Then** the service looks up the document, reads the file by externalID from storage, and returns the file content with Content-Type from the document's mimeType.
2. **Given** a document ID that does not exist, **When** GET is sent to `/internal/document/:id/content`, **Then** the service returns 404 Not Found.
3. **Given** a valid document ID, **When** GET is sent to `/internal/document/:id/meta`, **Then** the service returns JSON with all document metadata fields.
4. **Given** a document ID that does not exist, **When** GET is sent to `/internal/document/:id/meta`, **Then** the service returns 404 Not Found.

---

### User Story 3 - Storage Backend Abstraction (Priority: P3)

The service supports pluggable storage backends via configuration.
The initial implementation provides a local filesystem backend. The
architecture ensures that adding S3 or other backends requires only
a new adapter — no business logic or API changes.

**Why this priority**: Local filesystem is sufficient for initial
deployment. S3 support is a future need but the abstraction must be
in place from day one.

**Independent Test**: Can be tested by running the same test suite
against the local filesystem adapter and verifying all operations
pass.

**Acceptance Scenarios**:

1. **Given** `STORAGE_TYPE=local` and `LOCAL_STORAGE_PATH=/storage`, **When** a file is written via the private endpoint, **Then** it is stored at `{LOCAL_STORAGE_PATH}/{externalID}`.
2. **Given** a file stored locally, **When** it is read via the private endpoint, **Then** the content matches what was written.

---

### User Story 4 - Store and Link File to Document (Priority: P2)

The WOPI service (PutFile operation) needs to write new file content
AND update the corresponding Document record in Alkemio's database in
a single atomic operation. Today the WOPI service would need to call
two separate services (file write + DB update) with no transactional
guarantee. The store-and-link endpoint solves this by atomically
storing the file and updating the Document record's externalID and
size.

Alkemio Server may also adopt this endpoint in the future to replace
its current pattern of direct storageService.save + document update.

**Why this priority**: Same as User Story 2 — WOPI service needs this
for correct PutFile behavior. A partial failure (file stored but
document not updated, or vice versa) would leave the system in an
inconsistent state.

**Independent Test**: Can be tested by calling the endpoint with
binary content and a known document ID, then verifying both the file
on disk and the document record in the database reflect the new
content.

**Acceptance Scenarios**:

1. **Given** binary file content and a valid document ID, **When** PUT is sent to `/internal/document/:id/content`, **Then** the file is processed (image conversion/compression if applicable), stored on the storage backend, and the Document record's externalID, mimeType, and size are updated in Alkemio's database. The response contains the new externalID, mimeType, and size.
2. **Given** the same endpoint called twice with different content for the same document, **When** the second call completes, **Then** the Document record reflects the latest externalID and size, and the new file is on the storage backend.
3. **Given** a document ID that does not exist in Alkemio's database, **When** PUT is sent, **Then** the service returns 404 and no file is stored on the backend.
4. **Given** binary content and a valid document ID, **When** the file is stored successfully but the database update fails, **Then** the stored file is cleaned up and an error is returned — the system is not left in an inconsistent state.
5. **Given** binary content and a valid document ID, **When** the database update succeeds but the file write fails, **Then** the database change is rolled back and an error is returned.

---

### User Story 5 - Image Processing and MIME Validation (Priority: P2)

When Alkemio Server receives a file upload (via GraphQL), it delegates
file processing to the file-service. The file-service detects the
actual MIME type from file content (magic bytes), validates it against
a caller-provided allowlist, processes images (HEIC→JPEG conversion,
JPEG/WebP compression and resize), stores the result, and returns
the externalID, final size, and detected/final MIME type.

This replaces the server's current `ImageConversionService`,
`ImageCompressionService`, and `LocalStorageAdapter` — the server's
upload flow becomes: create auth policy + tagset → call file-service
`POST /internal/document` with file + metadata + IDs → get back
document ID + externalID + mimeType + size.

**Why this priority**: The server currently trusts client-provided
MIME types with no content-based verification. Moving MIME detection
and image processing into the file-service fixes this security gap
and centralizes all file handling in one place.

**Independent Test**: Can be tested by uploading files with
mismatched MIME claims (e.g., a JPEG file claimed as `image/png`)
and verifying the service detects the real type, or by uploading a
HEIC image and verifying the response contains a JPEG externalID.

**Acceptance Scenarios**:

1. **Given** a JPEG file and a caller-provided allowlist including `image/jpeg`, **When** POST is sent to the upload-and-process endpoint, **Then** the service detects `image/jpeg` from content, compresses the image, stores it, and returns the externalID, final size, and `image/jpeg` as the MIME type.
2. **Given** a HEIC file and an allowlist including `image/heic`, **When** POST is sent, **Then** the service converts to JPEG, compresses, stores, and returns the externalID with `image/jpeg` as the final MIME type.
3. **Given** a PDF file and an allowlist including `application/pdf`, **When** POST is sent, **Then** the service stores the file as-is (no image processing) and returns the detected MIME type.
4. **Given** a file whose detected MIME type is not in the caller-provided allowlist, **When** POST is sent, **Then** the service returns 415 Unsupported Media Type and does not store the file.
5. **Given** a file whose client-claimed MIME type differs from the content-detected MIME type, **When** POST is sent, **Then** the service uses the content-detected MIME type (not the client claim) for validation and storage.
6. **Given** an image larger than 4096px on its longest side, **When** POST is sent, **Then** the service resizes it (preserving aspect ratio) before storing.
7. **Given** an SVG, GIF, or PNG file, **When** POST is sent, **Then** the service stores it as-is without compression (these formats are not re-compressed).

---

### User Story 6 - Document Lifecycle Management (Priority: P2)

The file-service owns the `document` table (full CRUD). Alkemio Server
and other services create, read, update, and delete Document records
via private HTTP endpoints. The server remains the owner of
`authorization_policy` and `tagset` tables — it creates those rows
first, then passes their IDs to the file-service when creating a
Document.

This replaces the server's current `DocumentService.createDocument()`
and `DocumentService.deleteDocument()` flows. The server becomes
read-only on the `document` table.

**Why this priority**: Centralizing document record management in the
file-service enables atomic file+record operations, proper file
deletion (fixing the existing `DELETE_FILE=false` gap), and future
orphan cleanup.

**Independent Test**: Can be tested by calling the create endpoint
with file content + metadata, verifying the document row exists in
the database, then calling delete and verifying both the row and file
are removed.

**Acceptance Scenarios**:

1. **Given** file content, metadata (displayName, storageBucketId, createdBy, temporaryLocation), and pre-created authorizationId + tagsetId, **When** POST is sent to `/internal/document`, **Then** the file is processed and stored, and a Document record is created with all provided fields plus the computed externalID, mimeType, and size. The response returns the document ID, externalID, mimeType, and size.
2. **Given** a valid document ID, **When** DELETE is sent to `/internal/document/:id`, **Then** the Document record is deleted from the database, the underlying file is deleted from storage (if no other documents reference the same externalID), and the response returns the deleted document's authorizationId and tagsetId so the caller can clean up those rows.
3. **Given** a valid document ID and new storageBucketId + temporaryLocation=false, **When** PATCH is sent to `/internal/document/:id`, **Then** the Document record's storageBucketId and temporaryLocation are updated (used for moving temporary documents to their final bucket).
4. **Given** an authorizationId that doesn't exist in the authorization_policy table, **When** POST is sent to create a document with that ID, **Then** the service returns 400 (FK constraint) and no file is stored.
5. **Given** a document ID that does not exist, **When** DELETE or PATCH is sent, **Then** the service returns 404.

---

### Edge Cases

- What happens when the storage backend is full or unavailable during a write? → Returns 500 Internal Server Error with structured error body describing the storage failure.
- How does the service handle a concurrent read of a file being written? → Safe due to content-addressable storage: write to temp file then atomic rename to `{externalID}`. Readers see either the complete file or 404 — never a partial write.
- What happens when DELETE is called for a document whose file doesn't exist on storage? → Log a warning but proceed with deleting the document row. The DB record is the primary artifact; missing files are an inconsistency to log, not a blocker.
- How does the service behave when the Alkemio database is unreachable during a public endpoint request? → Returns 503 Service Unavailable; circuit breaker fails fast.
- What happens when NATS (auth-evaluation-service) is unavailable? → Returns 503 Service Unavailable; circuit breaker fails fast. Files are never served without auth checks.
- What happens when the store-and-link endpoint is called concurrently for the same document ID? → Last-writer-wins via DB row-level locking. Both callers succeed but the final state reflects the last write. No application-level locking needed.
- What happens to the old file on storage when a document's externalID is updated via store-and-link? (Orphan cleanup is out of scope for this service — other documents may reference the same externalID.)
- What happens when image compression produces a larger file than the original? (Return the original uncompressed.)
- What happens when HEIC conversion fails? (Return an error — do not silently store the unconverted HEIC.)
- What if the detected MIME type is not in the MimeFileType enum but is in the caller's allowlist? (Accept it — the enum is a server concern, not a file-service concern.)
- What happens when deleting a document whose externalID is shared with other documents? → Do not delete the file from storage; only delete the document row. File orphan cleanup is a separate concern.
- What happens if the server creates an authorization_policy row but the file-service create call fails? → Server must clean up the orphaned auth_policy + tagset rows. This is the server's responsibility since it created them.

## Requirements *(mandatory)*

### Functional Requirements

- **FR-01**: System MUST expose a public endpoint `GET /rest/storage/document/:id` that serves file content to authenticated and authorized users (drop-in replacement for existing TS file-service).
- **FR-02**: System MUST validate actor identity from Oathkeeper-injected JWT (`alkemio_actor_id` claim) on all public endpoints.
- **FR-03**: System MUST authorize file access by calling the authorization-evaluation-service via NATS (`auth.evaluate` subject) with the actor's agentId, `read` privilege, and the document's authorizationPolicyId.
- **FR-04**: System MUST look up document metadata (externalID, authorizationPolicyId, mimeType) from Alkemio's PostgreSQL database. The file-service has full CRUD access to the document table (FR-27).
- **FR-05**: System MUST expose internal cluster endpoints under the `/internal/` prefix (no auth). Endpoints: `GET /internal/document/:id/meta` (FR-28), `GET /internal/document/:id/content` (FR-27), `PUT /internal/document/:id/content` (FR-14), `POST /internal/document` (FR-22), `DELETE /internal/document/:id` (FR-23), `PATCH /internal/document/:id` (FR-24). ExternalID is never exposed in any API — all access is by document ID. In Docker dev, Traefik routes `/api/storage/internal/...` and strips `/api/storage` so the service receives `/internal/...`. In K8s production, internal services call directly via K8s service DNS at `/internal/...`.
- **FR-06**: System MUST NOT require authorization on internal endpoints (`/internal/*`) — callers are trusted services within the K8s cluster.
- **FR-07**: System MUST compute SHA3-256 hash of file content and use it as the externalID (filename in storage). The hash output MUST be byte-identical to the existing TypeScript implementation (`createHash('sha3-256').update(data).digest('hex')` in `calculate.buffer.hash.ts`) so that existing files stored by the TS service are readable without migration.
- **FR-08**: System MUST abstract the storage backend behind a port interface supporting save, read, delete, and exists operations.
- **FR-09**: System MUST implement a local filesystem storage adapter as the default backend.
- **FR-10**: System MUST return appropriate Content-Type headers based on document mimeType from the database (public endpoint) or from the document record (internal endpoint).
- **FR-11**: System MUST support ETag-based conditional requests (If-None-Match) on the public endpoint, returning 304 Not Modified when content hasn't changed.
- **FR-12**: System MUST set Cache-Control headers on public endpoint responses (configurable max-age, default 24 hours).
- **FR-13**: System MUST expose a health check endpoint at `GET /health`.
- **FR-14**: System MUST expose an internal endpoint `PUT /internal/document/:id/content` that atomically stores a file and updates the corresponding Document record. The endpoint accepts binary file content, applies image processing (HEIC→JPEG conversion, JPEG/WebP compression per FR-17/FR-18), computes the SHA3-256 hash of the processed content as the new externalID, stores the file via the storage backend, and updates the Document record's externalID, mimeType, size, and updated timestamp. Returns the new externalID, mimeType, and size. Returns 404 if the document ID does not exist. The operation MUST be atomic: if the DB update fails the stored file is cleaned up; if the file write fails the DB change is rolled back.
- **FR-15**: The Alkemio database connection MUST have full CRUD access to the `document` table. All other tables remain read-only from the file-service's perspective. The database user credentials are configured via the existing `ALKEMIO_DATABASE_*` env vars.
- **FR-16**: System MUST detect the actual MIME type from file content using magic-byte analysis (not trusting client-provided MIME headers). The detected MIME type MUST be used for all validation and storage decisions.
- **FR-17**: System MUST convert HEIC/HEIF images to JPEG before storage. The conversion MUST produce a JPEG with maximum quality. The returned MIME type MUST reflect the final format (`image/jpeg`), not the original.
- **FR-18**: System MUST compress JPEG and WebP images before storage using MozJPEG-equivalent quality (82). Images with longest side exceeding 4096px MUST be resized (preserving aspect ratio) to fit within that limit. EXIF metadata MUST be stripped and orientation auto-corrected. SVG, GIF, and PNG MUST NOT be re-compressed. If compression produces a larger file than the original, the original MUST be stored instead.
- **FR-19**: System MUST accept an optional `maxFileSize` parameter (integer, bytes) on the `POST /internal/document` endpoint. If provided and the file exceeds this limit (checked before image processing), the service returns 413 Payload Too Large without storing the file.
- **FR-20**: All UUIDs generated by this service (document IDs) MUST be UUIDv7 (RFC 9562). UUIDv7 provides time-ordered, sortable identifiers. Existing UUIDs from other systems (authorizationId, tagsetId, storageBucketId) are accepted as-is regardless of version.
- **FR-21**: System MUST use structured JSON logging via Zap. Log entries MUST include request ID, endpoint, duration, and error context where applicable.
- **FR-22**: System MUST expose an internal endpoint `POST /internal/document` that atomically stores a file and creates a Document record. The endpoint accepts multipart form data: file content (binary part) plus form fields (displayName, storageBucketId, createdBy, temporaryLocation, authorizationId, tagsetId, and optional allowedMimeTypes/maxFileSize). The service processes the file (MIME detection, image processing per FR-16/FR-17/FR-18), stores it, and creates the Document record with the computed externalID, detected mimeType, and size. Returns the document ID, externalID, mimeType, and size. Returns 400 if required fields are missing or FK constraints fail.
- **FR-23**: System MUST expose an internal endpoint `DELETE /internal/document/:id` that deletes the Document record and the underlying file from storage. If other Document records reference the same externalID, the file MUST NOT be deleted from storage (shared content-addressable reference). The response MUST include the deleted document's authorizationId and tagsetId so the caller (server) can clean up those rows.
- **FR-24**: System MUST expose an internal endpoint `PATCH /internal/document/:id` that updates mutable Document fields. Initially supports updating `storageBucketId` and `temporaryLocation` (used by the server to move temporary documents to their final bucket). Returns 404 if the document does not exist.
- **FR-25**: System MUST expose expvar metrics at `GET /debug/vars` (cluster-internal), including: NATS connection state, NATS reconnect count, NATS disconnect count, storage operations count, and circuit breaker state. This matches the authorization-evaluation-service observability pattern.
- **FR-26**: When a required dependency (NATS, Alkemio DB) is unavailable, the system MUST return 503 Service Unavailable with a structured JSON error body. Circuit breakers MUST be used to fail fast and prevent cascading failures. The system MUST NOT serve files without completing authorization checks — no graceful degradation that bypasses security.
- **FR-27**: System MUST expose an internal endpoint `GET /internal/document/:id/content` that serves file content by document ID without authorization. The endpoint looks up the document record, reads the file from storage by externalID, and streams it with appropriate Content-Type header. Returns 404 if the document or file does not exist. This is the internal equivalent of the public `GET /rest/storage/document/:id` — same behavior but no JWT/NATS auth check.
- **FR-28**: System MUST expose an internal endpoint `GET /internal/document/:id/meta` that returns document metadata (JSON) without authorization. Returns the document record fields: id, externalID, mimeType, size, displayName, createdBy, temporaryLocation, storageBucketId, authorizationId, tagsetId, createdDate, updatedDate. Returns 404 if the document does not exist.
- **FR-29**: The file-service MUST have full CRUD access to the `document` table in Alkemio's PostgreSQL database. The Alkemio Server MUST have read-only access to the `document` table. Authorization policy and tagset tables remain owned by the server.

### Backward Compatibility Requirements (Drop-in Replacement)

This service replaces the existing TypeScript file-service at
`/Users/antst/work/alkemio/file-service`. It MUST be deployable as a
drop-in replacement with zero migration of stored files and minimal
configuration changes.

- **FR-30**: System MUST read files from the same storage volume and path layout as the existing TS file-service. Files are stored at `{LOCAL_STORAGE_PATH}/{externalID}` — the Go service MUST serve files already on disk without any migration or re-indexing.
- **FR-31**: System MUST accept the `LOCAL_STORAGE_PATH` environment variable for configuring the storage root, matching the existing TS file-service convention (default: `../server/.storage` for local dev, `/storage` in K8s).
- **FR-32**: System MUST accept the `PORT` environment variable for the HTTP listen port, defaulting to `4003` to match the existing TS file-service.
- **FR-33**: System MUST accept the `DOCUMENT_MAX_AGE` environment variable (integer, seconds) for the Cache-Control max-age value, defaulting to `86400` (24 hours), matching the existing TS file-service.
- **FR-34**: System MUST return response headers on the public endpoint that match the existing TS file-service behavior: `Cache-Control: public, max-age={DOCUMENT_MAX_AGE}`, `Pragma: public`, `Expires` header (current time + max-age as UTC timestamp), and `ETag` set to the document ID.
- **FR-35**: System MUST return the same HTTP status codes as the existing TS file-service for equivalent error conditions: 401 for missing/invalid identity, 403 for insufficient permissions, 404 for document or file not found, 500 for internal errors.
- **FR-36**: System MUST be deployable into the existing K8s environment using the same PVC mount (`file-storage-pvc2` at `/storage`, read-write) and the same service port (4003), requiring only a container image swap and mount mode change in the deployment manifest.
- **FR-37**: System MUST use the same NATS environment variable names as the authorization-evaluation-service so that both services can share the `alkemio-config` ConfigMap. Required: `NATS_URL` (server URL, required). Optional with matching defaults: `NATS_RECONNECT_WAIT_MS` (default 1000), `NATS_RECONNECT_MAX_WAIT_MS` (default 30000), `NATS_FAILURE_THRESHOLD` (default 3), `NATS_BREAKER_TIMEOUT_SECONDS` (default 15), `NATS_HALF_OPEN_MAX_REQUESTS` (default 2).
- **FR-38**: System MUST use `auth.evaluate` as the default NATS subject for authorization requests, matching the authorization-evaluation-service's `NATS_SUBJECT` default. This subject SHOULD be configurable via `NATS_SUBJECT` env var.

### Key Entities

- **Document** (Alkemio DB, full CRUD by file-service): Represents a file in Alkemio. Key attributes: id (UUID), externalID (SHA3-256 hash), mimeType, size, displayName, createdBy (UUID, nullable), temporaryLocation (boolean), storageBucketId (UUID FK), authorizationId (UUID FK to authorization_policy, managed by server), tagsetId (UUID FK to tagset, managed by server), createdDate, updatedDate. The file-service creates, reads, updates, and deletes document records. The server has read-only access.
- **StoredFile**: A file on the storage backend. Addressed by externalID (content hash). No database record in this service — the storage backend is the source of truth for file content.

## Success Criteria *(mandatory)*

### Measurable Outcomes

- **SC-001**: The existing TS file-service public endpoint (`GET /rest/storage/document/:id`) can be replaced by this service with no change to frontend behavior — same URL, same response format, same caching.
- **SC-002**: Internal services (WOPI, Alkemio Server) can read and write files via private endpoints without authorization overhead.
- **SC-003**: Unauthorized requests to public endpoints are rejected and never return file content.
- **SC-004**: The service starts, connects to databases and NATS, and responds to health checks without manual intervention.
- **SC-005**: Writing the same file content twice returns the same externalID (content-addressable, idempotent writes).
- **SC-006**: Files previously stored by the existing TypeScript file-service are served correctly without any data migration — the Go service is a drop-in replacement for existing data.
- **SC-007**: The Go service can be deployed into the existing K8s environment by swapping only the container image — same PVC, same port, same env vars — with no changes to surrounding infrastructure or client code.
- **SC-008**: The WOPI service can replace its current file-write + document-update sequence with a single call to the store-and-link endpoint, with no risk of partial failure leaving inconsistent state.
- **SC-009**: Alkemio Server can remove its `LocalStorageAdapter`, `ImageConversionService`, and `ImageCompressionService` — all file I/O and image processing is delegated to this service.
- **SC-010**: Files with mismatched client-claimed MIME types are correctly identified by content detection — a renamed `.exe` claimed as `image/jpeg` is rejected when the allowlist only permits images.
- **SC-011**: Alkemio Server can remove its `DocumentService.createDocument()` and `DocumentService.deleteDocument()` — all document CRUD is delegated to the file-service via HTTP. The server becomes read-only on the document table.
- **SC-012**: Deleting a document via the file-service also deletes the underlying file from storage (fixing the existing `DELETE_FILE=false` gap in the TS service), unless other documents share the same externalID.

## Clarifications

### Session 2026-03-30

- Q: How does the service authenticate users on public endpoints? → A: Oathkeeper injects a JWT with `alkemio_actor_id` claim. Service validates JWT via JWKS and extracts actor ID.
- Q: How does the service authorize file access? → A: NATS call to authorization-evaluation-service (`auth.evaluate`) with agentId, privilege, and authorizationPolicyId.
- Q: Where does the service get document metadata? → A: From the `document` table in Alkemio's PostgreSQL database. The file-service has full CRUD access to this table (FR-29).
- Q: What storage backends are supported initially? → A: Local filesystem only. S3 is a future addition; the port interface must be in place.
- Q: Do private endpoints require auth? → A: No. They are cluster-internal, protected by K8s network policy.
- Q: The old TS file-service uses RabbitMQ for auth — why is the Go service using NATS? → A: The authorization-evaluation-service now exposes NATS (`auth.evaluate`), which is the preferred pattern for new Alkemio services (see matrix-adapter-go). RabbitMQ auth delegation is legacy. This is an intentional architecture change, not a compatibility requirement.
- Q: The old service delegates auth entirely via RabbitMQ (passes cookies/tokens). How does the Go service differ? → A: The Go service reads actor identity from Oathkeeper JWT directly and calls auth-evaluation-service via NATS with agentId + privilege + policyId. This is the same pattern as other new Go services. Oathkeeper was already in place; the TS service just didn't use it for identity extraction.
- Q: Why does the file-service need write access to Alkemio's DB? → A: The file-service owns the `document` table (full CRUD per FR-29). It creates document records (FR-22), updates them (FR-14, FR-24), and deletes them with file cleanup (FR-23). This centralizes all document + file operations in one service.
- Q: What happens to the old file when a document's externalID changes? → A: Orphan file cleanup is out of scope. Multiple documents may reference the same externalID (content-addressable), so the old file cannot be safely deleted at write time.
- Q: Who determines the MIME type — client or file-service? → A: The file-service detects the actual MIME type from file content (magic bytes). Client-provided MIME is ignored for validation purposes. This fixes the current server behavior where client MIME is trusted without verification.
- Q: Who owns image processing (HEIC conversion, compression)? → A: The file-service. Alkemio Server's `ImageConversionService` and `ImageCompressionService` will be removed. The server sends raw bytes; the file-service returns processed bytes + metadata.
- Q: Who owns MIME allowlist configuration? → A: The server. Each StorageBucket has its own allowedMimeTypes list. The server passes this list to the file-service on each upload request. The file-service validates but does not store the allowlist.
- Q: What replaces LocalStorageAdapter in the server? → A: HTTP calls to the file-service's private endpoints. The server's `StorageService` interface (save/read/delete/exists) is replaced by a client adapter that calls the file-service.
- Q: Should store-and-link (FR-14) apply image processing and update mimeType? → A: Yes. Store-and-link applies full image processing (HEIC→JPEG, compression) and updates the Document's mimeType column alongside externalID, size, and updatedDate.
- Q: What observability is required? → A: Structured JSON logs via Zap + expvar metrics at `/debug/vars`, matching the authorization-evaluation-service pattern. No Prometheus or distributed tracing.
- Q: How should the service behave when dependencies (NATS, Alkemio DB) are unavailable? → A: Return 503 Service Unavailable with structured error body. Use circuit breakers to fail fast. No graceful degradation — never serve files without auth checks.
- Q: How should POST /internal/document pass optional parameters (allowedMimeTypes, maxFileSize) alongside binary content? → A: Multipart form — file content as binary part, parameters as form fields.
- Q: Who owns the document table? → A: The file-service (full CRUD). The server becomes read-only on this table. Authorization policies and tagsets remain owned by the server.
- Q: How does document creation work with server-owned auth policies? → A: Option A — server creates authorization_policy + tagset rows first, then passes their IDs to the file-service in the create request. The file-service stores the FKs but never interprets auth policies.
- Q: How does document deletion work? → A: File-service deletes the document row + underlying file. Response includes authorizationId and tagsetId so the server can clean up those rows. File is only deleted from storage if no other documents reference the same externalID.
- Q: How do server-to-file-service calls work? → A: HTTP (not NATS). NATS has message size limits (~1MB default) that make binary file transfer awkward. HTTP handles multipart/streaming natively. NATS stays for lightweight auth.evaluate calls only.
- Q: What is temporaryLocation? → A: A boolean flag for two-phase uploads. Documents upload to a temporary bucket first (temporaryLocation=true), then the server moves them to the final bucket when the parent entity is saved (sets storageBucketId + temporaryLocation=false via PATCH endpoint).
- Q: Are file-only upload/delete/exists endpoints needed? → A: No. With US6 (document lifecycle), all file mutations go through document-level endpoints. ExternalID is never exposed in any API — all access is by document ID.
- Q: What URL prefix do internal endpoints use? → A: The file-service sees `/internal/...` paths. In Docker dev, Traefik routes `/api/storage/internal/...` and strips `/api/storage`. In K8s production, services call `/internal/...` directly via K8s service DNS. The public authenticated endpoint stays at `/api/private/rest/storage/document/:id` (through Oathkeeper, backward compat). Convention: `private` = through Oathkeeper (authenticated), `internal` = cluster-internal (no auth).
- Q: How are document sub-resources structured? → A: `/document/:id/content` for file bytes, `/document/:id/meta` for metadata JSON. The legacy public path `/rest/storage/document/:id` serves content directly (backward compat, cannot change).

## Assumptions

- Oathkeeper is configured to route public file-service requests and inject JWTs, same pattern as other Alkemio services.
- The authorization-evaluation-service is deployed and reachable via NATS at `auth.evaluate`.
- Alkemio's PostgreSQL database is accessible with a database user that has full CRUD access to the `document` table. All other tables remain read-only from the file-service's perspective. The server retains read-only access to the document table and full access to `authorization_policy` and `tagset` tables.
- The shared storage volume (or equivalent) is mounted at `LOCAL_STORAGE_PATH` and accessible to this service.
- The existing TS file-service's public API contract (`GET /rest/storage/document/:id`) is the compatibility target.
- File content uses SHA3-256 hashing for externalID, consistent with `calculateBufferHash()` in Alkemio Server.
- This service owns the `document` table (full CRUD). Document creation and deletion are performed by the file-service. The server creates `authorization_policy` and `tagset` rows first, passes their IDs to the file-service, and cleans them up on deletion. Authorization policy cascade logic remains in the server — it updates `authorization_policy` rows directly without file-service involvement.
- Server-to-file-service communication uses HTTP (not NATS). NATS is reserved for lightweight auth.evaluate calls. File transfer over NATS is impractical due to message size limits.
- NATS is already deployed in the cluster (used by authorization-evaluation-service).
- The existing TS file-service uses RabbitMQ for auth delegation; the Go service intentionally switches to NATS + JWT, matching newer Alkemio services. This is a deliberate architecture improvement, not a compatibility gap.
- The existing K8s deployment mounts `file-storage-pvc2` at `/storage`. The Go service must work with this same volume and mount point. The mount must be read-write (not read-only) since the file-service now handles writes that previously went through the server's LocalStorageAdapter.
- The Go service must accept the same core env vars (`LOCAL_STORAGE_PATH`, `PORT`, `DOCUMENT_MAX_AGE`) so that existing ConfigMaps and Secrets require minimal changes.
- NATS env var names (`NATS_URL`, `NATS_RECONNECT_WAIT_MS`, etc.) are shared with the authorization-evaluation-service via the `alkemio-config` ConfigMap. The file-service must not introduce differently-named NATS vars.
- Alkemio Server's `LocalStorageAdapter`, `ImageConversionService`, and `ImageCompressionService` will be removed once the server is migrated to use the file-service's private endpoints. The server will implement a thin HTTP client adapter in place of its current `StorageService` interface.
- The file-service owns file bytes, content hashing, MIME detection, image processing, and the document table (full CRUD). The server owns authorization policies, tagsets, StorageBucket/Aggregator hierarchy, and MIME allowlist configuration. The server has read-only access to the document table.
- Image compression settings (MozJPEG quality 82, max dimension 4096px, EXIF strip) match the current server implementation to ensure visual consistency during migration.
- The current server has no content-based MIME detection (client MIME is trusted). The file-service fixes this gap by detecting MIME from magic bytes on every upload.
