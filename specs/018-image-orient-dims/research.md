# Phase 0 Research

This document resolves the design decisions before Phase 1. Most open questions were settled during the five clarification rounds (recorded in `spec.md` under `## Clarifications`); the items below capture decisions that aren't FR-level but are needed to lock the implementation.

---

## Decision 1: How to read EXIF orientation cheaply

**Decision**: Use `ImageRef.Orientation()` from govips v2.18.0.

**Rationale**:
- Public method on `ImageRef`. Returns the orientation tag (1–8) as it appears in EXIF, or `0` if no tag is present.
- govips uses libvips' lazy-load: `NewImageFromBuffer` reads metadata before pixel data, so reading the orientation does not force a full pixel decode.
- For PNG/BMP/AVIF whose orientation is `1` or `0`, we short-circuit and return the input bytes byte-identical (FR-002), never touching pixel data.
- Treating `0` as "no rotation" matches libvips' own `vips_autorot` behavior and is consistent with EXIF's "absent tag = upright" convention.

**Alternatives considered**:
- Manual EXIF parsing via a Go-only library (e.g., `rwcarlsen/goexif`) — would add a dependency, risk disagreeing with libvips' interpretation, and gain nothing.
- `ImageRef.GetInt("orientation")` — also exposed, but `Orientation()` is the dedicated accessor with the right C-level fallback. Use the named accessor.

---

## Decision 2: ICC profile preservation strategy

**Decision**: Call `ImageRef.RemoveMetadata()` before each export. govips' contract documents that this method drops EXIF/IPTC/XMP while keeping ICC profile, orientation, and pages metadata. As belt-and-suspenders, also pass `StripMetadata=true` on the export params.

**Rationale**:
- `RemoveMetadata` is the public method documented to do exactly what we need: drop EXIF/IPTC/XMP, keep ICC. The govips source is explicit ("won't remove the ICC profile, orientation and pages metadata because govips needs it to correctly display the image" — see `image_metadata.go:RemoveMetadata`).
- Independent of libvips version — the carve-out is in the govips wrapper layer, so we don't depend on libvips 8.15+'s newer "keep" semantics.
- After `AutoRotate()` clears the orientation tag, `RemoveMetadata()` drops EXIF/IPTC/XMP while ICC remains. Output is byte-canonical: one rotation, no GPS/IPTC leakage, color profile intact (FR-006, FR-007).

**Alternatives considered**:
- Manual ICC extract/reattach using internal helpers (`vipsGetICCProfile` is unexported) — brittle, more code, unnecessary given the govips carve-out.
- Trust `StripMetadata=true` alone — works on libvips 8.15+ where strip preserves ICC by default, but unreliable across libvips versions deployed in different environments. Don't depend on it.

**Verification**: Round-trip test — encode a PNG/JPEG/WebP/HEIC/AVIF with a known ICC profile (Display P3), run through `Process`, extract ICC blob from output, assert byte-equal to input.

---

## Decision 3: Encoder choice per format

**Decision**:

| Input MIME | Encoder | Notes |
|---|---|---|
| `image/jpeg`, `image/jpg`, `image/webp` | `ExportJpeg` (existing) | unchanged from today |
| `image/heic`, `image/heif` | `ExportJpeg` after HEIC→JPEG (existing) | unchanged from today |
| `image/png` | `ExportPng` (new path) | only triggered when orientation ≠ 1 |
| `image/avif` | `ExportAvif` (new path) | only triggered when orientation ≠ 1 |
| `image/bmp` | `ExportMagick` with format=`bmp` (new path) | libvips delegates BMP saving to ImageMagick; FR-012 covers loud-fail when Magick is missing |
| `image/svg+xml` | passthrough (no re-encode) | dims via libvips header read |
| `image/gif` | passthrough (no re-encode) | dims via libvips canvas dims |
| non-image | passthrough | no dim measurement |

**Rationale**:
- libvips natively supports PNG and AVIF read/write. BMP loads via the magick reader and saves via the magick writer; the existing Docker image has Magick support since BMP loading already works today.
- "Re-encode only when orientation rewriting needed" satisfies FR-002 (orientation-1 PNG/BMP/AVIF return byte-identical to input).

**Failure mode for BMP**: If the libvips build lacks Magick support, `ExportMagick` returns an error. Per FR-012 the upload fails with HTTP 422 (`ErrImageProcessing`). We do NOT silently passthrough — that would emit non-canonical bytes from a "canonical bytes" boundary.

---

## Decision 4: Determinism for stable dedup

**Decision**: Use `NewPngExportParams()` / `NewAvifExportParams()` defaults plus `StripMetadata=true`; do not let any caller-influenced field bleed into the encode parameters. Compress=6 / filter=PngFilterNone for PNG; default settings for AVIF.

**Rationale**:
- FR-010 requires deterministic canonicalization so re-uploads of the same raw bytes hit content dedup. Hash is over post-processing bytes; encoder must produce identical output for identical input.
- libvips PNG and AVIF encoders are deterministic given identical inputs and identical encoder parameters. Risk is a future commit changing defaults — pinning explicitly defends against that.
- govips' `NewPngExportParams()` already returns a deterministic default. We use it and only override `StripMetadata`.

**Verification**: Upload same PNG/AVIF/BMP twice, assert `reused=true` on the second response. Existing dedup test pattern; new fixtures with rotated images so the canonicalization path actually runs.

---

## Decision 5: `Process` signature shape

**Decision**: Return a result struct rather than positional values:

```go
type ProcessResult struct {
    Content     []byte
    MimeType    string
    ImageWidth  *int  // nil when not measured (non-image, decode failure, or stub no-op)
    ImageHeight *int
    Measured    bool  // true → an image decoder ran on these bytes; false → no decoder attempted (no-vips stub)
}

type ImageProcessor interface {
    DetectMIME(content []byte) string
    Process(content []byte, mimeType string) (ProcessResult, error)
    MeasureDims(content []byte, mimeType string) (*int, *int, error)  // header-only, for lazy-backfill
}
```

The `Measured` flag exists specifically so the marshaling rule in `insertDocument` (T024) can distinguish "decoder ran and confirmed nil dims" (write `_decodeFailed` sentinel) from "no decoder attempted" (write `{}` so a future vips run can retry). Without it, the no-vips build would poison every image row with a permanent failure marker.

`MeasureDims` is the dedicated header-only port (separate from `Process` because `Process` re-encodes for raster orient!=1 inputs, violating FR-018's "no pixel decode" guarantee for legacy backfill). See Decision 8.

**Rationale**:
- A struct return is cleaner than 5-positional values for adding optional fields. Future per-content-type fields (videoDuration, etc.) ride on this struct without breaking the signature.
- Both adapters update in lock-step: the vips build populates dims; the no-vips stub returns nil dims, signature parity.

**Alternatives considered**:
- `(bytes, mime, *int, *int, error)` — less readable, harder to extend.
- Side-channel via context — opaque, violates explicit-data-flow.

---

## Decision 6: How dims live in `model.Document`

**Decision**: Add `ImageWidth *int` and `ImageHeight *int` to `model.Document`. They are populated from `content_metadata` on read (and from `ProcessResult` at create/replace), and serialized on the four metadata-returning endpoint responses.

**Rationale**:
- Pointer-typed for "absent vs zero" semantics; matches `omitempty` JSON behavior on the wire.
- Mirrors how `Reused` is plumbed today (also a response-only-but-on-the-domain-type field). The existing pattern is the right pattern.
- `content_metadata` is the source of truth in the DB; `Document.ImageWidth`/`ImageHeight` is the in-memory projection.

**Alternatives considered**:
- A separate `CreateDocumentResult` / `CopyDocumentResult` wrapper at the service layer — duplicates plumbing across each handler/service path. Not worth the indirection.
- Persisting dims as scalar columns (`image_width`, `image_height`) on the `file` table — explicitly rejected during clarification Q2; JSONB is forward-fit for future per-content-type fields.

---

## Decision 7: BackfillContentMetadata SQL shape

**Decision**:

```sql
-- name: BackfillContentMetadata :exec
UPDATE file
SET content_metadata = $2
WHERE id = $1;
```

**Rationale**:
- Updates ONLY `content_metadata`. Does NOT touch `version`, `updatedDate`, or any other column. This is the explicit decision in FR-018: "uses a dedicated query that updates `content_metadata` only and does NOT bump the row's `version`."
- No optimistic-lock check — this is a derived-data write, not a user-initiated mutation. Concurrent backfill writes are idempotent (same content → same dims).
- No `RETURNING` — caller already has the value; we don't need a roundtrip.
- `:exec` (returns no rows / no rows-affected count) — we don't care if the row exists at write time. If two concurrent processes both decode, one wins, one writes the same thing redundantly. If the row was deleted in between, the UPDATE matches zero rows; that's fine.

**Alternatives considered**:
- `:execrows` returning rows-affected — would let us detect "row was deleted between read and write." But we'd just log + ignore it, no behavior change. `:exec` is enough.
- Optimistic-lock predicate (`AND version = $3`) — would defeat the purpose. The whole point is to write metadata without colliding with optimistic-locked operations. Don't gate on version.
- Adding a `WHERE content_metadata = '{}'::jsonb` predicate to avoid overwriting non-empty metadata — would protect against a weird race where a real PATCH wrote `_decodeFailed` and a backfill tried to overwrite, but this race doesn't actually occur (PATCH never writes `_decodeFailed`; only the lazy-backfill or Process do, and those only write to empty rows). Skip for simplicity.

---

## Decision 8: Lazy-backfill placement in handler flow

**Decision**: Wrap row reads with a `backfillIfNeeded(doc) → doc` service-layer helper. Each metadata-returning handler calls it on the `Document` it's about to render. The helper:
1. If `doc.MimeType` doesn't start with `image/`, return as-is.
2. If `doc.ImageWidth != nil` (already measured), return as-is.
3. If the row's `content_metadata` JSONB has `_decodeFailed`, return as-is (response will omit dims).
4. Otherwise: call `Storage.Read(doc.ExternalID)`, then `Processor.MeasureDims(content, mimeType)` — the dedicated header-only port method (separate from `Process`, which re-encodes for raster orient!=1 and would violate FR-018's "no pixel decode" wording for large legacy images). Populate dims on `doc`, persist via `BackfillContentMetadata`. All failure modes per FR-019/020 — never returns an error to the caller.

The helper lives in `internal/domain/service` since it orchestrates Storage + Repo + Processor (all ports), and the Document model only carries response-state, not orchestration logic.

**Rationale**:
- Single helper, called from at most 4 handler paths (Create-dedup-hit, Copy, Replace-content, PATCH). DRY (Constitution VIII).
- Service layer is the right home — domain ports plumb through cleanly.
- Failure handling is centralized, not duplicated across handlers.

**Alternatives considered**:
- Inline the logic in each handler — duplicated 4×, high regression risk.
- Make it a method on `Document` — would require Document to know about Storage / Repo / Processor, breaking hexagonal layering.

---

## Decision 9: Process-time decode failure parity with lazy-backfill

**Decision**: When `Process` runs at create/replace time and successfully canonicalizes bytes BUT cannot measure dims (e.g., vips loaded the image but `Width()` returns 0 / a degenerate result), return `ProcessResult` with `nil` dims AND `Measured=true` (the decoder ran). The marshaling rule in `insertDocument` (T024) sees `Measured=true && nil dims` for an `image/*` MIME and writes `{"_decodeFailed": true}` to `content_metadata`. Mirrors FR-019's lazy-backfill behavior.

When `Process` runs and the existing fallback path triggers ("compressed >= original → keep original" for JPEG/WebP, or HEIC compression fails), the original bytes ARE measurable (they were the input we successfully loaded). Return real dims with `Measured=true`. No `_decodeFailed` for that case.

The no-vips stub returns `Measured=false` (no decoder attempted) — the marshaling rule then writes `{}` instead of the sentinel, so a future vips environment can retry. Without the `Measured` flag, the marshaling logic couldn't distinguish "decoder said no" from "no decoder attempted" and would poison every image row in `!vips` builds with a permanent `_decodeFailed` marker.

**Rationale**:
- Consistency with FR-019: bytes are unreadable at decode-time → sentinel.
- The existing JPEG fallback path is a successful-decode case; we have dims.
- Tests cover both paths.

**Alternatives considered**:
- Fail the upload on dim-measurement failure — punitive; the user uploaded successfully, dims are an enrichment.
- Leave `content_metadata` empty so lazy-backfill retries — would recursively fail on the same row forever. Bad.

---

## Decision 10: SVG / GIF dim measurement — at create vs lazy-only

**Decision**: Measure at BOTH create time and lazy-backfill time. `Process` runs `vips.NewImageFromBuffer` for any `image/*` MIME (including SVG and GIF) and reads `Width()`/`Height()`. Bytes pass through unchanged for SVG and GIF (no canonicalization), but dims are populated in the create response.

**Rationale**:
- Per Q5 clarification + the user's follow-up: populate at create avoids every new SVG/GIF upload triggering lazy-backfill on first read.
- Both paths use the same dim-extraction helper (DRY).
- libvips reads SVG via librsvg and GIF natively; both expose `Width()`/`Height()` after `NewImageFromBuffer`.

**Implementation note**: The current `Process` for SVG/GIF returns content as-is. The new code adds a vips-load-and-read step before returning, but does NOT re-encode. If the vips load fails (malformed SVG XML, corrupt GIF header), persist `_decodeFailed` per Decision 9.

---

## Decision 11: How to express dims in `content_metadata` JSON

**Decision**:

```json
// measured raster image / SVG / GIF
{ "imageWidth": 1024, "imageHeight": 768 }

// undecodable bytes (corrupt image)
{ "_decodeFailed": true }

// non-image, never-read legacy, or just-created not-yet-Process'd
{}
```

Reserved keys (leading underscore convention): `_decodeFailed`. Future per-content-type keys land at the top level: `videoDuration`, `pageCount`, etc.

**Rationale**:
- Forward-fit per Q2 + FR-017.
- Unknown JSON keys MUST be tolerated on read (FR-017).
- Reserved-key prefix prevents collision with real per-content-type fields.

---

## Open questions (none)

All NEEDS CLARIFICATION items are resolved either in the spec's `## Clarifications` (Q1–Q5) or in Decisions 1–11 above.
