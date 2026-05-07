# Feature Specification: Canonicalize image orientation and return post-rotation dimensions

**Feature Branch**: `018-image-orient-dims`
**Created**: 2026-05-07
**Status**: Draft
**Input**: User description: "Canonicalize image orientation + return post-rotation dimensions"

## Clarifications

### Session 2026-05-07

- Q: When canonicalization is required but the encoder isn't available (e.g., BMP with `Orientation != 1` on a libvips build without ImageMagick support), what should the upload pipeline do? → A: Reject the upload with HTTP 422 (`ErrImageProcessing`). Maintains the canonical-bytes contract strictly; silent passthrough would break the guarantee that bytes leaving the service are renderable as-is.
- Q: Beyond `POST /internal/file`, which other endpoints should surface `imageWidth` / `imageHeight` on their response? → A: Persist the computed dims on the file row in a single `content_metadata` JSONB column, computed once at content-change time. Surface `imageWidth` / `imageHeight` on every endpoint that returns file metadata: `POST /internal/file` (create), `POST /internal/file/copy`, `PUT /internal/file/{id}/content` (replace), `PATCH /internal/file/{id}`. Copy and PATCH read from the source/current row — zero re-decode. JSONB shape is forward-fit for future per-content-type fields (video duration, PDF page count, etc.) without further schema migrations.
- Q: How should existing image rows in production (and on dev hosts with stale databases) be backfilled? → A: Lazy backfill on metadata read. When a request that returns file metadata (Create dedup hit, Copy, PATCH) reads a row whose `content_metadata` is empty AND whose `mimeType` is an image, do a **header-only** decode (`vips.NewImageFromBuffer` parses metadata, not pixels — microseconds even for a 50MB image), compute dims (apply orientation swap if 5/6/7/8), persist the result, and include it in the response. Use a dedicated `BackfillContentMetadata(id, jsonb)` query that does **not** bump `version`, so it doesn't race with optimistic-locked PATCH. No background worker, no framework, no env vars, no metrics — the work piggybacks on natural reads and is bounded by the cheap header parse. Self-healing: legacy rows backfill on first access; rows never accessed never backfill (and don't need to). Trade-off: legacy bytes on storage stay as-they-are (orientation flag preserved); only NEW uploads (Create, Replace) emit physically-rotated canonical bytes. Server-side renderers that respect EXIF orientation see the same dims the file service reports.
- Q: How should the lazy-backfill handle non-decode failures (Storage.Read error, DB persist error)? → A: Best-effort, type-aware. Corrupt-bytes (vips decode failure) → persist `{"_decodeFailed": true}` sentinel + response without dims. Storage.Read error (transient) → log warning, response without dims, leave `content_metadata` empty so the next read retries. DB-persist error (transient) → log warning, response WITH dims (we computed them) but skip persistence, leave `content_metadata` empty so the next read recomputes and retries persistence. The underlying request (Copy/PATCH/Create-dedup) NEVER fails because of a backfill issue — image-dim measurement is decoupled from the request's primary purpose.
- Q: Should SVG and GIF be excluded from dim measurement (as previously deferred), or measured uniformly with raster formats? → A: Measure them. libvips reads both (SVG via `viewBox` / width / height attributes, GIF via canvas dimensions in the Logical Screen Descriptor); the cost is the same cheap header parse. Treating every `image/*` MIME consistently removes a wire-contract carve-out, simplifies FR-005, and gives downstream callers uniform validation logic. SVG and GIF still don't undergo byte canonicalization (no orientation concept; no re-encode), but they DO get `imageWidth` / `imageHeight` reported on every metadata-returning response.

## User Scenarios & Testing *(mandatory)*

### User Story 1 - Phone-photo upload validates against the dimensions a user actually sees (Priority: P1)

A platform user uploads a phone photo as their avatar (or as a banner/card image). Many phone cameras save portrait shots as landscape bytes plus an EXIF `Orientation=6` flag — a renderer applies the flag to display the photo upright, but a naive byte-level dimension reader sees the unrotated landscape dimensions. Today the upstream server validates the file against a visual's allowed pixel range using the unrotated dimensions, so a perfectly valid portrait photo is rejected for being "too wide and not tall enough." The user sees a confusing rejection for a photo that visibly fits the requirement.

After this change, the file service is the authority for both the canonical bytes and the canonical (post-rotation) dimensions. Consumers receive bytes that are already physically rotated and EXIF-stripped, and the upload response carries `imageWidth` and `imageHeight` matching what a renderer would actually draw. The upstream server stops decoding the bytes locally and validates against the dimensions the file service reports.

**Why this priority**: This is the regression that motivated the work. It's currently breaking real avatar/banner uploads for mobile users on the platform. Fixing it restores parity with the pre-migration behavior.

**Independent Test**: Upload a JPEG with `Orientation=6` and raw bytes 1082×127. Verify the upload response reports `imageWidth=127, imageHeight=1082` and the returned bytes have no EXIF orientation tag (or `Orientation=1`). The upstream server can then validate against `127×1082` and accept the photo as a portrait avatar.

**Acceptance Scenarios**:

1. **Given** a JPEG with raw 1082×127 bytes and EXIF `Orientation=6`, **When** uploaded, **Then** the response reports post-rotation dimensions `127×1082` and the returned bytes are physically rotated with no EXIF orientation tag.
2. **Given** a JPEG with `Orientation=1` (already canonical), **When** uploaded, **Then** the response reports the same dimensions as the input bytes and behavior is unchanged from today.
3. **Given** the same byte stream uploaded twice, **When** processed, **Then** content dedup hits on the second upload (canonicalization is deterministic).

---

### User Story 2 - All raster formats produce byte-canonical output for new uploads (Priority: P2)

Some phone exports and screen-capture tools embed EXIF orientation flags in PNG and AVIF (rare but present in the wild). Today the file service passes those formats through untouched, so downstream consumers either ignore the orientation flag and render the image upside-down/sideways, or apply it locally and disagree with the file service on dimensions. After this change, **new uploads** of all raster formats emit byte-canonical output: anything written to storage from now on is "what you see is what's stored." Pre-existing rows on storage are not re-canonicalized — they keep their original bytes — but their reported dimensions account for orientation, so consumers can still validate them correctly.

**Why this priority**: Lower frequency than P1 (most PNGs don't carry EXIF), but it closes the loophole going forward. Without this, a future tool that emits oriented PNGs would re-introduce the same class of bug under a different format.

**Independent Test**: Upload a PNG with EXIF `Orientation=6` (e.g., produced by a screen capture utility). Verify the response bytes are physically rotated, EXIF orientation is absent in the output, and the response dimensions match what a renderer would draw. Upload an orientation-1 PNG of the same source and verify the bytes are passed through unchanged (no re-encode) but dimensions are still reported.

**Acceptance Scenarios**:

1. **Given** a PNG with EXIF `Orientation != 1`, **When** uploaded, **Then** the response bytes are re-encoded with rotation applied and EXIF stripped, and the response reports post-rotation dimensions.
2. **Given** a PNG with EXIF `Orientation == 1` or no orientation tag, **When** uploaded, **Then** the bytes are returned byte-identical to the input (no lossy re-encode), and the response reports the input dimensions.
3. **Given** an AVIF or BMP with EXIF `Orientation != 1`, **When** uploaded, **Then** behavior matches the PNG case (re-encode in the same format, strip metadata, report rotated dimensions).

---

### User Story 3 - Wide-gamut images keep color fidelity through canonicalization (Priority: P2)

Designers and photographers upload images with embedded ICC color profiles (wide-gamut sRGB, Display P3, Adobe RGB, CMYK). Stripping all metadata as part of canonicalization would silently destroy the profile and shift colors when the image is later rendered. The file service must preserve the ICC profile across every re-encode path while still dropping EXIF, XMP, and any other non-color metadata.

**Why this priority**: Equal P2 with US2. Affects a smaller user population (designers/photographers) but breaks their workflow severely when triggered — colors shift visibly, brand assets render wrong, and there's no obvious recovery path.

**Independent Test**: Upload a JPEG and a PNG that each carry an embedded ICC profile (e.g., Display P3). Extract the ICC profile from the response bytes and verify it matches the input profile byte-for-byte. Repeat with an EXIF-rotated variant of each — re-encoding must not strip the profile.

**Acceptance Scenarios**:

1. **Given** a JPEG with embedded ICC profile (any orientation), **When** uploaded, **Then** the response bytes carry the same ICC profile.
2. **Given** an EXIF-rotated PNG with embedded ICC profile, **When** uploaded, **Then** the re-encoded output retains the ICC profile.
3. **Given** an image with EXIF, XMP, and ICC metadata, **When** uploaded, **Then** EXIF and XMP are stripped from the output; only ICC survives.

---

### Edge Cases

- **SVG**: No raster pixels, no EXIF orientation, no byte canonicalization needed — bytes pass through unchanged. Dims ARE reported: `imageWidth` / `imageHeight` are derived by libvips from the SVG's `viewBox` / `width` / `height` attributes (or default-rendered pixel size for percentage units). Both Process at create time and the lazy-backfill measure them via the same cheap header parse.
- **GIF**: Multi-frame; bytes pass through unchanged (no canonicalization). Dims ARE reported: `imageWidth` / `imageHeight` are the canvas dimensions from the Logical Screen Descriptor, which libvips returns from `Width()`/`Height()`. Frame-level dims (when frames differ from the canvas) are not surfaced.
- **Non-image uploads** (PDF, documents, video, audio, archives): Pass through unchanged. Width/height fields are omitted.
- **Re-upload of an already-canonicalized image**: The same input bytes produce the same output bytes (deterministic canonicalization). Content dedup hits on the second upload.
- **Image with orientation tag absent** (no EXIF block at all): Treated as orientation=1. Pass through unchanged for PNG/BMP/AVIF; JPEG/WebP/HEIC continue their current re-encode path.
- **Image with malformed or unreadable EXIF**: Treat as orientation=1. Do not fail the upload over EXIF parse errors.
- **BMP with `Orientation != 1` on a libvips build without ImageMagick support**: Reject the upload with HTTP 422 (`ErrImageProcessing`). The canonical-bytes contract is non-negotiable; if the deployment can't honor it for a given input, the upload fails loudly rather than silently storing non-canonical bytes. Operators are expected to ship libvips with Magick support (the production image does); this branch only fires on stripped builds.
- **Copy of a row whose `content_metadata` has dims**: Dims propagate to the destination row verbatim and are surfaced on the copy response. No image bytes are decoded.
- **Copy of a legacy row (`content_metadata` empty)**: When the copy handler reads the source row's `content_metadata`, the lazy-backfill (FR-018) kicks in: header-decode the source row's stored bytes, compute dims, persist them on the source row, then propagate to the new row and surface on the response. So copies of legacy images self-heal both rows (source and destination) on first copy.
- **Dedup hit on `POST /internal/file` for a legacy row**: The matched row is returned with its stored `content_metadata`. If the matched row's `content_metadata` is empty and its `mimeType` is an image, the lazy-backfill runs (FR-018) — header-decode, compute dims, persist on the matched row — and the response carries the freshly computed dims. The just-Process'd dims for the incoming bytes match the lazy-backfill's dims (same content), so either source produces the same result.
- **PATCH on a row with stored dims**: The patch response carries the row's existing `imageWidth`/`imageHeight` from `content_metadata`. PATCH does not change content bytes.
- **PATCH on a legacy image row**: Same lazy-backfill as Copy/Create-dedup. The PATCH handler reads the row, sees empty `content_metadata` for an image, performs the header decode + persist, and the response carries the dims.
- **Concurrent reads of the same legacy row**: Each reader performs the cheap header decode independently. Postgres serializes the `BackfillContentMetadata` writes; the result is idempotent (identical dims from identical bytes). Some wasted decode work in this race, bounded by replica count, no correctness issue.
- **Lazy-backfill encounters an undecodable image (corrupt stored bytes)**: Persist `{"_decodeFailed": true}` to `content_metadata` (FR-019). The response omits dims. Subsequent reads of the same row see the sentinel and skip the decode attempt entirely. Operators can list these via `WHERE content_metadata @> '{"_decodeFailed": true}'::jsonb` to investigate.
- **Lazy-backfill encounters a `Storage.Read` failure (file missing, S3 transient error)**: Log a warning, do NOT persist a sentinel (the failure may be transient), leave `content_metadata` empty. Response omits dims. The underlying request (Copy/PATCH/Create-dedup) succeeds normally. The next metadata-returning read of the same row will retry the backfill from scratch (FR-020).
- **Lazy-backfill `BackfillContentMetadata` SQL persist failure (DB transient)**: Log a warning, leave `content_metadata` empty. Response INCLUDES the just-computed dims (we have them in memory). The underlying request succeeds normally. The next metadata-returning read of the same row will recompute the dims and retry persistence (FR-020).
- **Lazy-backfill write contends with PATCH on the same row**: The backfill query updates only `content_metadata` and does NOT bump `version`. PATCH operates on `version` for optimistic locking. They write disjoint columns on the same row; Postgres serializes the writes; both succeed. No 409 conflict from the backfill.
- **Image whose post-rotation dimensions exceed the existing 4096px cap**: The 4096px JPEG cap behavior is unchanged. Out of scope for this feature.
- **Caller passes a `Content-Type` that disagrees with the actual format**: MIME sniffing already runs upstream of this code path; canonicalization uses the sniffed format, not the caller-declared one.

## Requirements *(mandatory)*

### Functional Requirements

- **FR-001**: For raster image formats (JPEG, WebP, HEIC/HEIF, PNG, BMP, AVIF), the upload pipeline MUST emit bytes whose effective EXIF orientation is `1` (or who carry no orientation tag), so the bytes match what a renderer draws.
- **FR-002**: For PNG, BMP, and AVIF whose input EXIF orientation is `1` or absent, the upload pipeline MUST return the input bytes unchanged (no lossy re-encode) — re-encoding only triggers when orientation rewriting is required.
- **FR-003**: For PNG, BMP, and AVIF whose input EXIF orientation is not `1`, the upload pipeline MUST physically rotate the pixels, strip EXIF, and re-encode in the input format.
- **FR-004**: For all `image/*` MIMEs whose dimensions libvips can resolve (rasters, SVG, GIF), every response that returns file metadata (create, copy, replace-content, patch) MUST carry post-rotation pixel dimensions (`imageWidth`, `imageHeight`).
- **FR-005**: For non-image uploads (PDF, documents, video, audio, archives), file-metadata responses MUST omit `imageWidth` / `imageHeight`. (FR-004 covers the converse — that all measurable `image/*` MIMEs, including SVG via `viewBox` / `width` / `height` and GIF via the Logical Screen Descriptor canvas dims, MUST report them.)
- **FR-006**: Every re-encode path (existing JPEG/WebP/HEIC compression plus the new PNG/BMP/AVIF rotation paths) MUST preserve the embedded ICC color profile.
- **FR-007**: The upload pipeline MUST NOT synthesize new EXIF or metadata fields. The metadata policy is "strip everything except ICC."
- **FR-008**: The OpenAPI specification MUST advertise the new optional `imageWidth` / `imageHeight` fields on every file-metadata response (`POST /internal/file`, `POST /internal/file/copy`, `PUT /internal/file/{id}/content`, `PATCH /internal/file/{id}`) so generated clients can pick them up.
- **FR-009**: Existing JPEG/WebP/HEIC behavior MUST remain functionally identical to today (auto-rotate, re-encode JPEG, strip metadata) — the only addition is reporting dimensions on the response.
- **FR-010**: Canonicalization MUST be deterministic — identical input bytes produce identical output bytes — so content-addressed dedup remains stable for re-uploads.
- **FR-011**: Malformed or unreadable EXIF metadata MUST be treated as orientation=1 (pass through) rather than failing the upload.
- **FR-012**: When canonicalization of a raster format is required (input EXIF orientation `!= 1`) but the deployed image-processing stack cannot produce canonical bytes in the input format (e.g., BMP rotation on a libvips build without ImageMagick support), the upload pipeline MUST reject the upload with HTTP 422 rather than store non-canonical bytes. The canonical-bytes guarantee must not be silently broken.
- **FR-013**: Computed image dimensions MUST be persisted on the file row in a single JSON-typed `content_metadata` column. The column carries `imageWidth` and `imageHeight` for any `image/*` upload whose dimensions libvips can resolve (rasters, SVG, GIF); it is empty (`{}`) for non-image rows and for image rows whose dimensions could not be measured. Persistence is one-shot at content-change time — `Process` measures dims for ALL `image/*` MIMEs (not just rasters) and writes them at Create and Replace-content. Copy and PATCH read from the existing row without re-decoding bytes. Legacy rows with empty `content_metadata` are populated lazily on first metadata read (FR-018).
- **FR-014**: `POST /internal/file/copy` MUST propagate `content_metadata` from the source row to the new row verbatim, and surface the resulting `imageWidth` / `imageHeight` on the response. No image bytes are decoded by the copy path.
- **FR-015**: `PATCH /internal/file/{id}` MUST surface the file's current `imageWidth` / `imageHeight` (read from the row's `content_metadata`) on its response, since PATCH never changes content bytes and the row already carries the canonical values.
- **FR-016**: Legacy file rows that pre-date this feature have an empty `content_metadata` column. The first metadata-returning read of such a row (Create dedup hit, Copy, PATCH) lazily computes dims via a header-only image decode and persists them on the row, so the response carries the dims and subsequent reads of that row hit the populated value (FR-018). Legacy bytes on storage are not re-canonicalized — only the metadata column is enriched.
- **FR-017**: The `content_metadata` JSON shape MUST allow forward-extension with additional per-content-type fields (e.g., future `videoDuration`, `pageCount`, `audioBitrate`) without a further schema migration. Unknown JSON keys MUST be tolerated on read.
- **FR-018**: When an endpoint about to return file metadata (Create dedup hit, Copy, PATCH) reads a row whose `content_metadata` is empty AND whose `mimeType` starts with `image/`, the service MUST perform a header-only decode of the row's stored bytes (using libvips' lazy-loading API: `NewImageFromBuffer` + `Width`/`Height`/`Orientation` — no pixel decode, microseconds even for large files), compute post-rotation dims (swap width/height when EXIF orientation is 5/6/7/8), persist the result to the row via a dedicated query that updates `content_metadata` only and does NOT bump the row's `version` (so it does not race with optimistic-locked PATCH), and include the resulting `imageWidth` / `imageHeight` on the response. Concurrent reads of the same legacy row each perform the cheap header decode independently; Postgres serializes the writes; the result is idempotent (same dims → same final value), so concurrent races are wasted work but not a correctness issue.
- **FR-019**: When the lazy-backfill header decode fails because the stored bytes are themselves unreadable (corrupt bytes, unsupported codec, libvips parse error), the service MUST persist a sentinel `{"_decodeFailed": true}` to the row's `content_metadata` so subsequent reads short-circuit on the predicate `content_metadata = '{}'::jsonb` and stop attempting to decode the same row repeatedly. The response in this case omits `imageWidth` / `imageHeight`. The marker key is reserved (leading underscore) and namespaced separately from real per-content-type fields. Operators investigating dim-less image rows can list these via `WHERE content_metadata @> '{"_decodeFailed": true}'::jsonb`.
- **FR-020**: The lazy-backfill MUST NOT fail the underlying request (Copy/PATCH/Create-dedup) for any failure mode of the backfill itself. Failure handling is type-aware: (a) **vips decode failure** (corrupt bytes) → persist sentinel per FR-019 + response omits dims; (b) **`Storage.Read` failure** (file missing, transient I/O error) → log a warning, leave `content_metadata` empty (no sentinel — the failure may be transient and a future read should retry), response omits dims; (c) **`BackfillContentMetadata` SQL failure** (DB transient error) → log a warning, leave `content_metadata` empty, response INCLUDES the just-computed dims (we have them; only persistence failed; the next read will recompute and retry persistence). Image-dim measurement is decoupled from the request's primary purpose — a backfill issue must never cascade into a 5xx for an unrelated PATCH or Copy.

### Key Entities *(include if feature involves data)*

- **`file.content_metadata` (new column, JSON-typed)**: A single per-row JSON object holding content-type-specific metadata. For this feature it carries `{ "imageWidth": <int>, "imageHeight": <int> }` for any measurable `image/*` upload (rasters, SVG, GIF), or `{ "_decodeFailed": true }` for image rows whose header parse failed. Empty (`{}`) for non-image rows and for legacy image rows that haven't yet been read via a metadata-returning endpoint. Forward-fit for future fields (`videoDuration`, `pageCount`, etc.) without further schema migrations. Populated by two paths: (1) at content-change time during Process (Create, Replace) for ALL `image/*` MIMEs, and (2) lazily on first metadata read for legacy image rows (FR-018).
- **File-metadata response payload (Create / Copy / Replace-content / Patch)**: All four endpoints' responses gain two optional integer top-level fields, `imageWidth` and `imageHeight`, derived from `content_metadata`. Both omitted when not measured. No other response fields change.

## Success Criteria *(mandatory)*

### Measurable Outcomes

- **SC-001**: A phone photo with raw bytes 1082×127 and EXIF `Orientation=6` produces an upload response reporting `imageWidth=127, imageHeight=1082` and bytes whose effective orientation is canonical. The exact regression that motivated this work no longer reproduces.
- **SC-002**: For each supported raster format (JPEG, WebP, HEIC, PNG, BMP, AVIF), an EXIF-rotated test image and an orientation-1 test image both produce a response whose dimensions match what a renderer would draw, verified by an automated test.
- **SC-003**: An image carrying an embedded ICC profile produces a response whose bytes still carry that ICC profile byte-for-byte, for every format on every re-encode path. Verified by an automated round-trip test.
- **SC-004**: An orientation-1 PNG (or BMP, or AVIF) produces a response whose bytes are byte-identical to the input — confirming the no-re-encode short-circuit. Verified by an automated equality test.
- **SC-005**: The same byte stream uploaded twice hits content dedup on the second upload, confirming canonicalization is deterministic.
- **SC-006**: The upstream server can validate image dimensions against visual constraints using only the file service response, without decoding the bytes locally. Validated by removing the local decode in the follow-up server PR and observing tests still pass there.
- **SC-007**: A `POST /internal/file/copy` of a previously-uploaded image row returns a response carrying the same `imageWidth` / `imageHeight` as the source row, without the file service touching the image bytes (verifiable by absence of any `vips.NewImageFromBuffer` call on the copy code path under instrumentation).
- **SC-008**: A `PATCH /internal/file/{id}` request returns a response carrying the row's stored `imageWidth` / `imageHeight` for image rows (and omits the fields for non-image rows). The first PATCH on a legacy image row triggers the lazy-backfill (FR-018) and the response carries the freshly computed dims; subsequent PATCHes on the same row hit the populated value with no decode.
- **SC-009**: A metadata-returning request (Create dedup hit, Copy, PATCH) on a legacy image row whose `content_metadata` is empty triggers a header-only decode and persists the result. Verifiable by seeding a legacy row with `content_metadata = '{}'::jsonb`, issuing one of the metadata-returning requests against it, and observing `content_metadata` change to a populated `{ "imageWidth": ..., "imageHeight": ... }` value within the same request — and that the response body carries the dims.
- **SC-010**: A metadata-returning request on a legacy row with corrupt stored bytes persists `{"_decodeFailed": true}` to `content_metadata` and the response omits dims. A subsequent metadata-returning request on the same row does NOT attempt to decode again — the sentinel short-circuits.
- **SC-011**: The lazy-backfill write does not race with concurrent PATCH on the same row. Verifiable by issuing a metadata-returning request on a legacy image row simultaneously with a PATCH on the same row; both succeed (no 409 from the backfill side) because the backfill updates a disjoint column (`content_metadata`) without touching `version`.
- **SC-012**: When `Storage.Read` fails during lazy-backfill (e.g., the underlying blob is missing or the storage adapter returns a transient error), the underlying request (Copy/PATCH/Create-dedup) still returns its normal 2xx response with dims omitted, and `content_metadata` remains `{}`. Verifiable with a mock storage adapter that returns an error for one targeted externalID — the request succeeds, no sentinel is persisted, a follow-up read with healthy storage backfills the row normally.
- **SC-013**: When `BackfillContentMetadata` SQL persistence fails during lazy-backfill (e.g., DB transient), the request returns the just-computed dims in the response anyway, and `content_metadata` remains `{}` for retry. Verifiable with a fault-injected DB that fails the BackfillContentMetadata UPDATE once — the response carries `imageWidth`/`imageHeight`, the row is unchanged, and the next read recomputes and persists.
- **SC-014**: Uploads of SVG and GIF surface `imageWidth` / `imageHeight` on their create response, computed at Process time from libvips' header read. Verifiable by uploading a known-dimensioned SVG (e.g., `viewBox="0 0 200 100"`) and a known-dimensioned GIF and asserting the response carries `200×100` (or canvas dims, respectively). Lazy-backfill of legacy SVG/GIF rows produces the same result.

## Assumptions

- The 4096px maximum-dimension cap inside the JPEG compression path is left unchanged. Resizing policy is not part of this feature.
- MIME validation against `allowedMimeTypes` (a separate input policy) is not part of this feature and stays where it is.
- File-service-go remains content-agnostic about visual policy. Pixel-range constraints (e.g., AVATAR is 190–410 wide) live on the upstream server. The file service only reports the dimensions; the server enforces the policy.
- The govips binding pinned in this repository (`v2.18.0`) exposes `ImageRef.RemoveMetadata()`, whose contract is documented to drop EXIF/IPTC/XMP while keeping ICC profile, orientation, and pages metadata. Re-encode paths use this method before exporting; ICC preservation does not require a binding bump or manual reattach.
- Dimension reporting for SVG and GIF is included via the same libvips header-read path used for raster formats. SVG dims come from `viewBox` / `width` / `height` (libvips applies a default DPI for percentage units); GIF dims are the canvas dimensions. Bytes for SVG and GIF still pass through unchanged — only the metadata column is populated.
- The upstream server's `CreateDocumentResult` mirror and its `visual.service.ts:uploadImageOnVisual` consumer will be updated in a separate, paired PR in the alkem-io/server repo. That PR is out of scope here but is what closes the user-visible loop.
- The single schema change in this PR is `ALTER TABLE file ADD COLUMN content_metadata JSONB NOT NULL DEFAULT '{}'`. No new tables are introduced; no background workers; no environment variables; no operational ceremony. Backfill of legacy rows is amortized across natural metadata reads via FR-018. This honors the constitution's "owns the file table; all other tables read-only" boundary.
- Legacy bytes on storage are not re-canonicalized by this feature — they remain as they were uploaded. Only NEW uploads (Create, Replace) emit physically-rotated, EXIF-stripped, ICC-preserved canonical bytes. The dims reported on responses for legacy rows account for orientation (post-rotation values), so consumers that respect EXIF orientation see the same dims the file service reports.
- Surfacing dims on a future `GET /internal/file/{id}/meta` extension is a natural follow-on (the data is already on the row). It is not part of this feature's wire-contract changes; the metadata endpoint stays as-is for now.

## Out of Scope

- Computing or surfacing per-type metadata other than image dimensions (PDF page count, video duration, audio bitrate, etc.). The JSON column is shaped to accept those keys, but no producer or reader for non-image content types is wired up here.
- Surfacing dims on `GET /internal/file/{id}/meta` (out of scope; data will be on the row, but the endpoint shape stays as-is for this feature).
- Resizing or aspect-ratio coercion based on visual policy.
- Changing the existing 4096px cap in JPEG compression.
- Changing MIME validation behavior.
- Updating the upstream alkem-io/server repository (paired PR, separate scope).
