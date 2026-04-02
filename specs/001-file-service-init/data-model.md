# Data Model: Go File Service

**Branch**: `001-file-service-init` | **Date**: 2026-03-30

## Entities

### Document (Alkemio DB — full CRUD by file-service)

Source: Alkemio PostgreSQL `document` table. The file-service owns this table (full CRUD). The server has read-only access.

| Field | Type | Access | Notes |
|-------|------|--------|-------|
| `id` | UUID | create (generated), read | Primary key, used in public endpoint path |
| `externalID` | varchar(128) | create, read, update | SHA3-256 content hash, used as filename on storage backend |
| `mimeType` | varchar(128) | create, read, update | MIME type of stored file (post-processing) |
| `size` | integer | create, read, update | File size in bytes (post-processing) |
| `displayName` | varchar(512) | create, read | Original filename from upload |
| `createdBy` | UUID (nullable) | create, read | User who uploaded the document (passed by server) |
| `temporaryLocation` | boolean | create, read, update | Two-phase upload flag (default false) |
| `storageBucketId` | UUID | create, read, update | FK to storage_bucket table (managed by server) |
| `authorizationId` | UUID (unique) | create, read | FK to authorization_policy (created by server, passed to file-service) |
| `tagsetId` | UUID (unique, nullable) | create, read | FK to tagset (created by server, passed to file-service) |
| `createdDate` | timestamp | create, read | Auto-set on creation |
| `updatedDate` | timestamp | create, read, update | Auto-set on create/update |
| `version` | integer | create (default 1), read, update | Optimistic locking for PATCH — incremented on each update, checked in WHERE clause |

**Queries needed (sqlc)**:
1. `GetDocumentByID(id UUID)` → returns all fields
2. `CreateDocument(...)` → inserts full row, returns id
3. `UpdateDocumentFile(id UUID, externalID, mimeType string, size int, updatedDate timestamp)` → store-and-link update, returns row count
4. `UpdateDocumentLocation(id UUID, storageBucketId UUID, temporaryLocation bool, updatedDate timestamp, version int)` → move temp doc with optimistic locking (`WHERE id = $1 AND version = $5`), increments version, returns row count (0 = version mismatch)
5. `DeleteDocument(id UUID)` → deletes row, returns externalID + authorizationId + tagsetId (for file cleanup and server-side cleanup)
6. `CountDocumentsByExternalID(externalID string)` → count of documents referencing this hash (for safe file deletion)

### StoredFile (storage backend — no DB record)

A file on the storage backend. Addressed by `externalID` (content hash). The storage backend is the source of truth for file content.

| Attribute | Type | Notes |
|-----------|------|-------|
| `externalID` | string (64 hex chars) | SHA3-256 hash of file content, used as filename |
| `content` | []byte | Raw file bytes (post-processing) |

**Operations**: save, read, delete, exists — via StoragePort interface.

### Authorization (Alkemio DB — read only, indirectly via NATS)

Not directly queried by file-service. The `authorizationId` from Document resolves to an `authorization_policy` row managed by the server. The file-service passes `authorizationPolicyId` to the authorization-evaluation-service via NATS `auth.evaluate`.

| Field | Type | Notes |
|-------|------|-------|
| `authorizationPolicyId` | UUID | From Document record (authorizationId FK) |
| `agentId` | UUID | From JWT `alkemio_actor_id` claim |
| `privilege` | string | Always `"read"` for public file serving |

## Domain Types (Go)

```go
// Document represents a full document record from Alkemio's document table.
type Document struct {
    ID                  uuid.UUID
    ExternalID          string
    MimeType            string
    Size                int
    DisplayName         string
    CreatedBy           *uuid.UUID // nullable
    TemporaryLocation   bool
    StorageBucketID     uuid.UUID
    AuthorizationID     uuid.UUID
    TagsetID            *uuid.UUID // nullable
    CreatedDate         time.Time
    UpdatedDate         time.Time
    Version             int        // optimistic locking for PATCH
}

// CreateDocumentInput contains fields needed to create a new document.
type CreateDocumentInput struct {
    DisplayName       string
    CreatedBy         *uuid.UUID
    TemporaryLocation bool
    StorageBucketID   uuid.UUID
    AuthorizationID   uuid.UUID
    TagsetID          *uuid.UUID
}

// StoredFile represents the result of a file storage operation.
type StoredFile struct {
    ExternalID string  // SHA3-256 hex hash
    MimeType   string  // Detected/post-processing MIME type
    Size       int     // File size in bytes (post-processing)
    Created    bool    // true if a new file was written; false if dedup matched an existing file
}

// AuthResult represents the outcome of an authorization check.
type AuthResult struct {
    Allowed bool
    Reason  string
}

// DeletedDocument contains IDs the caller needs for cleanup after deletion.
type DeletedDocument struct {
    ExternalID      string     // needed for post-delete file cleanup (count-then-delete)
    AuthorizationID uuid.UUID
    TagsetID        *uuid.UUID
}
```

## State Transitions

### Document Create (POST /internal/document)

```text
Received → Validate Fields → MIME Detect → Validate (allowlist + size) → Image Process → Hash → Store File → Insert DB Row → Response
                ↓ (fail)                       ↓ (fail)                    ↓ (fail)        ↓ (fail)         ↓ (fail)
              400 error                     415/413 error              Error returned    500 + cleanup    500 + cleanup file
```

### Document Delete (DELETE /internal/document/:id)

```text
Received → Delete DB Row (returns externalID + IDs) → Count by ExternalID → Delete File (if count == 0) → Response {authorizationId, tagsetId}
              ↓ (not found)                                                       ↓ (fail)
            404 error                                                       Log warning, continue
```

Count is done AFTER delete (not before) to eliminate the TOCTOU race where two concurrent deletes both see count > 1 and skip file cleanup.

### Store and Link (PUT /internal/document/:id/content)

```text
Received → Document Lookup → Image Processed → Hashed → Stored → DB Updated → Cleanup Old File (if changed, count==0) → Response
              ↓ (not found)                               ↓ (fail)    ↓ (fail)
            404 error                                  500 error    Rollback file (if Created) + 500
```

## Helper Functions

### normalizeMIME

Strips parameters (e.g. `; charset=utf-8`) and lowercases a MIME type string. Used consistently across document create and store-and-link flows to ensure MIME comparisons are case-insensitive and parameter-free.

## Relationships

```text
Server creates:                    File-service creates:
  authorization_policy row    →      Document row (stores authorizationId FK)
  tagset row                  →      StoredFile on disk (via externalID)

Document (Alkemio DB) ──── externalID ────→ StoredFile (storage backend)
    │                                              │
    │ authorizationId (FK)                         │ addressed by SHA3-256 hash
    │ tagsetId (FK)                                │
    │ storageBucketId (FK)                         ↓
    ↓                                         {LOCAL_STORAGE_PATH}/{externalID}
AuthorizationPolicy ←── NATS auth.evaluate
Tagset (managed by server)
StorageBucket (managed by server)
```

- Multiple Document records can reference the same externalID (content-addressable deduplication)
- File deletion on document delete only happens when no other documents reference the same externalID
- Authorization cascade (server updates authorization_policy rows directly) does not involve the file-service
