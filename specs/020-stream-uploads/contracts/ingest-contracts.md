# Contracts: Streaming Ingest (spec 020)

## HTTP surface — shapes unchanged, behavior amended

### POST /internal/file (create)

- Multipart framing unchanged. **Documented constraint** (matches the sole
  caller, the server adapter): the `file` part precedes the metadata
  fields. The service streams the file part under the global
  `MAX_UPLOAD_SIZE`; bucket-level `maxFileSize`/`allowedMimeTypes` are
  validated after all parts arrive — violations reject exactly as today
  (413 / 415), with the staged bytes discarded.
- New rejection: 413 when the global cap is crossed mid-stream (connection
  may be closed without draining the remainder).
- New rejection: 422 `PIXEL_BUDGET_EXCEEDED` (named struct, same
  `RejectedContentResponse` family as 019) for images whose header
  dimensions exceed `IMAGE_PIXEL_BUDGET`.
- Stalled uploads (no bytes for `UPLOAD_IDLE_TIMEOUT_MS`) are aborted; the
  client observes a connection error, the service counts `stalled`.
- **Metadata field limit** (added in review, PR #31): each non-file
  multipart field is capped at 16 KiB — the largest legitimate value (a
  long `allowedMimeTypes` list) is ~2.5 KiB. An oversized field is
  **rejected with 400** naming the field, never silently truncated (a
  truncated value would parse as a *different* request); the already-staged
  file part is aborted (FR-006).
- Success responses byte-for-byte unchanged.

### PUT /internal/file/{id}/content (replace)

- All 019 semantics preserved verbatim: 422 `EMPTY_CONTENT` (now defined as
  "empty stream prefix"), 422 `MIME_MISMATCH`, generic-sniff fallback,
  rejection-before-side-effects, `content_replace_outcomes_total` keys.
- Gains the same streaming guards: global cap (413), idle timeout, pixel
  budget (422) — wopi-service treats any non-2xx as a failed save (verified
  in 019; no wopi change).

## Port contracts (internal)

### StoragePort (extended)

```go
// OpenStage begins a streaming ingestion into not-yet-published storage.
OpenStage(ctx context.Context) (StageWriter, error)

type StageWriter interface {
    io.Writer                       // hashes internally while writing
    Commit() (model.StoredFile, error) // publish: dedup-aware, backend-specific
    Abort() error                   // destroy staging; idempotent
}
```

Guarantees: nothing observable as a permanent object before `Commit`
returns; `Commit` is the only publish point; `Abort` after failed `Commit`
is safe. FS adapter: temp file in the blob directory + rename.
**Forward-compatibility**: the contract deliberately permits S3-style
implementations (multipart upload + server-side copy; abort-multipart) —
no atomic-rename assumption (spec assumption, 2026-06-12).

### ImageProcessor (extended)

```go
// TranscodeStream converts r into the canonical encoding for mimeType,
// writing encoded chunks to w as produced. Returns final MIME and
// header-derived, orientation-corrected dimensions.
TranscodeStream(r io.Reader, w io.Writer, mimeType string) (TranscodeResult, error)
```

Implementation notes (vips build): sequential header load → pixel budget →
materialize iff EXIF orientation ≥ 3 (fork disc threshold governs RAM vs
scratch) → AutoRotate → SaveToWriter. JPEG exports use `Interlace: false`
(research R7). Stub build: pass-through copy, `Measured=false` semantics
preserved.

## Compatibility matrix

| Consumer | Impact |
|---|---|
| server adapter (create/copy/read) | none — shapes unchanged; field order it already uses is now the documented contract |
| wopi-service (replace) | none — non-2xx already = failed save |
| 019 behaviors & tests | preserved; only constructor/transport signatures change |
| openapi.yaml | regenerate (`make openapi`); new 422 code joins the existing RejectedContentResponse enum — note the known generator attribution issue (antst/go-apispec#30) |
