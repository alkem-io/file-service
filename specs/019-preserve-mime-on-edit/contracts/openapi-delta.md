# OpenAPI Delta: PUT /internal/file/{id}/content

Changes to the existing replace-content operation (`openapi.yaml`). Success shape is
unchanged; two new 422 rejection outcomes are added. Sole production caller:
wopi-service `PutFile` (verified — the server adapter only GETs this path).

## Request (unchanged)

```
PUT /internal/file/{id}/content
Content-Type: application/octet-stream   (raw bytes, max 32 MiB)
```

## Responses

| Status | When | Body |
|---|---|---|
| 200 | accepted (incl. generic-sniff fallback — stored type kept) | `ReplaceContentResponse` (unchanged: externalID, mimeType, size, imageWidth, imageHeight). `mimeType` now always equals the document's pre-existing type. |
| **422** *(new semantics)* | empty body | `ErrorResponse{ code: "EMPTY_CONTENT", error: "..." }` |
| **422** *(new semantics)* | content unambiguously a different concrete type than the stored type | `ErrorResponse{ code: "MIME_MISMATCH", error: "...", detail: { knownMime, detectedMime } }` |
| 422 *(existing)* | image processing failure | unchanged |
| 409 *(existing)* | dedup conflict (new content hash collides with another row in bucket) | unchanged |
| 404 / 413 / 500 | unchanged | unchanged |

`ErrorResponse` is a named struct with `Render()` (constitution anti-pattern #11);
`code` is machine-readable and stable — wopi-service and the server adapter's
`fromHttpError` can branch on it without parsing prose.

## Behavioral contract changes

1. `mimeType` is **never** modified by this operation. The response's `mimeType`
   reports the (unchanged) stored type.
2. A 422 rejection has **no side effects**: no blob written, no row touched, no
   old-blob cleanup.
3. Every fallback/rejection is observable: structured log + `content_replace_outcomes_total`
   expvar counter (`accepted` / `fallback_generic_sniff` / `rejected_empty` /
   `rejected_mismatch`).

## Consumer impact

- **wopi-service**: no change required. Non-2xx already maps to a failed PutFile;
  Collabora shows native save-failed UI and keeps the session open (FR-009).
- **server adapter**: no change required (does not call this operation).

## Internal interface (not HTTP): startup self-repair job

No external contract. Observable via `mime_repair_total` expvar map
(`relabeled` / `unrecoverable` / `skipped_not_office` / `errors`) and structured
logs per touched row (document id, old MIME, new MIME or reason).
