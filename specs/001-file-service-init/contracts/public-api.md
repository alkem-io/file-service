# Public API Contract

## GET /rest/storage/document/:id

Serve a file to an authenticated and authorized user.

### Request

| Component | Value |
|-----------|-------|
| Method | GET |
| Path | `/rest/storage/document/:id` |
| Path Param | `id` — Document UUID |
| Auth | Oathkeeper-injected JWT with `alkemio_actor_id` claim |

**Conditional request headers**:
- `If-None-Match: "<etag>"` — returns 304 if ETag matches

### Response — 200 OK

| Header | Value |
|--------|-------|
| `Content-Type` | Document's mimeType from Alkemio DB |
| `Cache-Control` | `public, max-age={DOCUMENT_MAX_AGE}` |
| `Pragma` | `public` |
| `Expires` | UTC timestamp (now + DOCUMENT_MAX_AGE) |
| `ETag` | Document ID (UUID string) |

**Body**: Raw file content (binary stream)

### Response — 304 Not Modified

Returned when `If-None-Match` matches the document's ETag. No body.

### Error Responses

| Status | Condition |
|--------|-----------|
| 401 Unauthorized | Missing or invalid JWT |
| 403 Forbidden | Actor lacks READ privilege (NATS auth.evaluate returned `allowed: false`) |
| 404 Not Found | Document ID not in Alkemio DB, or file not on storage backend |
| 500 Internal Server Error | Unexpected error |
| 503 Service Unavailable | NATS or Alkemio DB unavailable (circuit breaker open) |

**Error body** (JSON):
```json
{
  "error": "<error code>",
  "message": "<human-readable description>"
}
```

**Note**: Health check (`GET /health`) and metrics (`GET /debug/vars`) are documented in [private-api.md](private-api.md) — they are root-level endpoints, not behind any prefix.
