# Internal API Contract (Cluster-Internal)

No authorization required. Protected by K8s network policy.

**Base path (as seen by the service)**: `/internal/`
**Docker dev access via Traefik**: `/api/storage/internal/...` (Traefik strips `/api/storage`)
**K8s production access**: `http://storage-service:4003/internal/...` (direct, no Traefik)

All access is by document ID. ExternalID is never exposed.

---

## GET /internal/document/:id/meta

Get document metadata by ID.

### Request

| Component | Value |
|-----------|-------|
| Method | GET |
| Path | `/internal/document/:id/meta` |
| Path Param | `id` — Document UUID |

### Response — 200 OK

```json
{
  "id": "a1b2c3d4-e5f6-7890-abcd-ef1234567890",
  "externalID": "3338be694f50c5f338814986cdf0686453a888b84f424d792af4b9202398f392",
  "mimeType": "image/jpeg",
  "size": 245760,
  "displayName": "photo.jpg",
  "createdBy": "b2c3d4e5-f6a7-8901-bcde-f12345678901",
  "temporaryLocation": false,
  "storageBucketId": "c3d4e5f6-a7b8-9012-cdef-123456789012",
  "authorizationId": "d4e5f6a7-b8c9-0123-def0-1234567890ab",
  "tagsetId": "e5f6a7b8-c9d0-1234-ef01-234567890abc",
  "createdDate": "2026-03-30T12:00:00Z",
  "updatedDate": "2026-03-30T12:00:00Z"
}
```

### Error Responses

| Status | Condition |
|--------|-----------|
| 404 Not Found | Document ID not found |

---

## GET /internal/document/:id/content

Serve file content by document ID (no auth). Same as public endpoint but without JWT/NATS auth check.

### Request

| Component | Value |
|-----------|-------|
| Method | GET |
| Path | `/internal/document/:id/content` |
| Path Param | `id` — Document UUID |

### Response — 200 OK

| Header | Value |
|--------|-------|
| `Content-Type` | Document's mimeType from DB |

**Body**: Raw file content (binary stream)

### Error Responses

| Status | Condition |
|--------|-----------|
| 404 Not Found | Document ID not found, or file not on storage backend |

---

## POST /internal/document

Create a document: store file + create Document record atomically.

### Request

| Component | Value |
|-----------|-------|
| Method | POST |
| Path | `/internal/document` |
| Content-Type | `multipart/form-data` |

**Multipart parts**:

| Field | Type | Required | Description |
|-------|------|----------|-------------|
| `file` | binary | yes | File content |
| `displayName` | string | yes | Original filename |
| `storageBucketId` | string (UUID) | yes | FK to storage_bucket |
| `authorizationId` | string (UUID) | yes | FK to authorization_policy (pre-created by server) |
| `tagsetId` | string (UUID) | no | FK to tagset (pre-created by server) |
| `createdBy` | string (UUID) | no | User who uploaded |
| `temporaryLocation` | string ("true"/"false") | no | Default "false" |
| `allowedMimeTypes` | string | no | Comma-separated MIME types |
| `maxFileSize` | string (integer) | no | Max file size in bytes |

### Response — 201 Created

```json
{
  "id": "019537a0-7890-7def-abcd-ef1234567890",
  "externalID": "3338be694f50c5f338814986cdf0686453a888b84f424d792af4b9202398f392",
  "mimeType": "image/jpeg",
  "size": 245760
}
```

Note: `id` is UUIDv7 (time-ordered).

### Error Responses

| Status | Condition |
|--------|-----------|
| 400 Bad Request | Missing required fields, invalid UUIDs, FK constraint failure |
| 413 Payload Too Large | File exceeds `maxFileSize` |
| 415 Unsupported Media Type | Detected MIME not in `allowedMimeTypes` |
| 500 Internal Server Error | Storage, processing, or DB failure (atomic cleanup) |
| 503 Service Unavailable | Alkemio DB unavailable |

---

## DELETE /internal/document/:id

Delete a document record and its underlying file (if not shared).

### Request

| Component | Value |
|-----------|-------|
| Method | DELETE |
| Path | `/internal/document/:id` |
| Path Param | `id` — Document UUID |

### Response — 200 OK

```json
{
  "authorizationId": "b2c3d4e5-f6a7-8901-bcde-f12345678901",
  "tagsetId": "c3d4e5f6-a7b8-9012-cdef-123456789012"
}
```

Returns the deleted document's FK IDs so the caller (server) can clean up authorization_policy and tagset rows.

**File deletion behavior**: The underlying file on storage is only deleted if no other Document records reference the same externalID (content-addressable dedup safety).

### Error Responses

| Status | Condition |
|--------|-----------|
| 404 Not Found | Document ID not found |
| 500 Internal Server Error | DB or storage failure |
| 503 Service Unavailable | Alkemio DB unavailable |

---

## PATCH /internal/document/:id

Update mutable Document fields (move temporary document to final bucket).

### Request

| Component | Value |
|-----------|-------|
| Method | PATCH |
| Path | `/internal/document/:id` |
| Path Param | `id` — Document UUID |
| Content-Type | `application/json` |

**Body**:

```json
{
  "storageBucketId": "d4e5f6a7-b8c9-0123-def0-1234567890ab",
  "temporaryLocation": false
}
```

All fields are optional — only provided fields are updated.

### Response — 200 OK

```json
{
  "id": "a1b2c3d4-e5f6-7890-abcd-ef1234567890",
  "storageBucketId": "d4e5f6a7-b8c9-0123-def0-1234567890ab",
  "temporaryLocation": false
}
```

### Error Responses

| Status | Condition |
|--------|-----------|
| 404 Not Found | Document ID not found |
| 400 Bad Request | Invalid field values |
| 500 Internal Server Error | DB failure |
| 503 Service Unavailable | Alkemio DB unavailable |

---

## PUT /internal/document/:id/content

Replace file content for an existing document (store-and-link). Atomically stores new file and updates Document record.

### Request

| Component | Value |
|-----------|-------|
| Method | PUT |
| Path | `/internal/document/:id/content` |
| Path Param | `id` — Document UUID |
| Content-Type | `application/octet-stream` |

**Body**: Raw binary file content

### Response — 200 OK

```json
{
  "externalID": "3338be694f50c5f338814986cdf0686453a888b84f424d792af4b9202398f392",
  "mimeType": "image/jpeg",
  "size": 245760
}
```

### Error Responses

| Status | Condition |
|--------|-----------|
| 404 Not Found | Document ID not in DB |
| 500 Internal Server Error | Storage, processing, or DB update failure (atomic rollback) |
| 503 Service Unavailable | Alkemio DB unavailable |

---

## GET /health

Health check endpoint.

### Response — 200 OK

```json
{
  "status": "healthy"
}
```

### Response — 503 Service Unavailable

```json
{
  "status": "unhealthy",
  "details": {
    "nats": "disconnected",
    "database": "unreachable"
  }
}
```

---

## GET /debug/vars

Expvar metrics endpoint (cluster-internal).

### Response — 200 OK

Standard expvar JSON output including:
- `resilience_nats_connection_state` (string)
- `resilience_nats_reconnect_attempts` (int)
- `resilience_nats_disconnects` (int)
- `resilience_breaker_state` (map)
- `storage_operations_total` (map: read/write/delete counts)
- `document_operations_total` (map: create/read/update/delete counts)
