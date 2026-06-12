# Feature Specification: Stream Uploads to Permanent Storage

**Feature Branch**: `020-stream-uploads`
**Created**: 2026-06-12
**Status**: Draft
**Backlog Story**: https://github.com/alkem-io/file-service/issues/30 (#30)
**Input**: User description: "file-service currently reads the whole uploaded file into memory (in case it needs transcoding), then transcodes, then writes. We want stream-conversion: uploads stream directly to permanent storage, bypassing the memory copy."

## Problem Statement

Every file ingested by the service today is materialized **fully in memory** —
the request body is read whole, optionally transcoded in memory (image
canonicalization such as HEIC/WebP → JPEG and orientation fixing), and only
then written to storage. Consequences:

- **Per-upload memory cost is proportional to file size**, so concurrent
  uploads multiply: a handful of large simultaneous uploads can exhaust the
  service's memory budget and destabilize unrelated requests.
- **The maximum upload size is effectively bounded by RAM**, currently pinned
  by a hard 32 MiB request cap. Raising the cap is unsafe under the buffering
  design, yet legitimate use cases (video, large decks, archives) need it.
- **Latency includes a full extra copy**: bytes are accumulated, then walked
  again for hashing/transcoding, then written.

The enabling capability for the image path — streaming decode from a reader
and streaming encode to a writer — exists in the project's image-processing
library fork (antst/govips#2) and will be upstreamed once this feature has
proven it in production use.

## Clarifications

### Session 2026-06-12

- Q: How should US3 (replace-path streaming) be sequenced against feature 019, which also reworks the replace path and is open on PR #29? → A: Implement against the 019 branch now: this feature's branch is rebased onto `019-preserve-mime-on-edit` and developed as a stacked branch, with US3 built directly on the final 019 code. The PR stacks on PR #29 (retargeted to `develop` once #29 merges).
- Q: What default upload cap ships, and what ceiling must the feature be validated at? → A: Default stays 32 MiB (no rollout behavior change; raising it later is config-only per environment, paired with the matching ingress bump as an infra-ops chore). The streaming pipeline is validated up to 1 GiB.
- Q: What timeout policy governs streaming uploads (the server currently enforces a global 30 s read timeout)? → A: Progress-based idle timeout: an upload stays alive while bytes keep arriving and is aborted after ~30 s without progress; total duration is implicitly bounded by cap ÷ minimum rate. Non-upload endpoints keep today's strict timeouts. A stalled/slowloris connection is killed quickly; a slow-but-moving large upload is never penalized.
- Q: The image library fork decodes the full frame to RAM (by design, for early source release) — does the transcode path need an explicit decode guard? → A: Yes, two-track: (1) this feature ships a configurable pixel-dimension budget, checked from the stream header before any decode — images above it are rejected; (2) in parallel, the govips fork gains sequential-streaming and disk-backed-decode modes (separate fork workstream, prompt handed off), after which the budget can be relaxed. The fork enhancement is NOT a dependency of this feature.
- Q: The fork workstream landed (TranscodeStream with sequential fast path + disc-backed materialization, threshold/scratch knobs, backpressure-safe chunked output) — does that change our requirements? → A: The fork is sufficient; no further fork changes needed. Three codec realities adjust this spec's framing: (1) HEIC decodes whole-frame inside the codec regardless of access mode, so the FR-010 pixel budget remains permanently load-bearing for such formats (not merely transitional) — it is the only guard on codec-internal decode memory, and also defends CPU and scratch disk; (2) interlaced/progressive JPEG cannot stream — whether the service keeps its current (interlaced) export params for byte-identity or switches to non-interlaced for true output streaming is a plan decision; (3) the library's pipe read limit, disc threshold, and scratch directory become service configuration, and scratch disk becomes a deployment sizing concern.
- Q: Interlaced (progressive) JPEG output cannot stream on encode — keep current export params for byte-identity, or switch? → A (plan decision R7): switch JPEG exports to non-interlaced (baseline). Streaming is the feature's purpose; keeping progressive would buffer the whole compressed output and silently void US2 for the most common transcode target. SC-002 amended: byte-identity holds for non-transcoded content; transcoded outputs are byte-identical to the buffered export with the new parameters (goldens re-baselined). Quality and decoded pixels unchanged (same Q82).
- Q: Streaming cannot replicate today's "recompress JPEG, keep the original if recompression didn't shrink it" size guard (it needs both copies whole). What happens to canonical (non-rotated) JPEG uploads? → A: Always recompress to quality 82 (baseline). Every JPEG streams through the encoder; predictable storage behavior is retained at the cost of occasionally storing a larger-than-original file for already-optimized inputs (the dropped size guard) — accepted. Pass-through is reserved for formats the service never re-encoded (GIF, SVG) and non-images.

## User Scenarios & Testing *(mandatory)*

### User Story 1 - Large non-image upload streams with constant memory (Priority: P1)

A user uploads a large non-image file (PDF, video, archive). The service
streams it from the request to permanent storage — hashing and type-sniffing
on the fly — holding only a small fixed-size buffer in memory regardless of
file size.

**Why this priority**: The pass-through path covers the majority of bytes
ingested and is where unbounded buffering hurts most; it requires no
transcoding and therefore delivers the full memory win first.

**Independent Test**: Upload a file much larger than the service's per-request
memory budget (e.g. hundreds of MiB with a budget of a few MiB) and verify it
succeeds, the stored content is byte-identical, the content hash and dedup
behavior are unchanged, and peak per-request memory stays within the fixed
budget.

**Acceptance Scenarios**:

1. **Given** a non-image file of arbitrary size within the configured limit,
   **When** it is uploaded, **Then** it is persisted byte-identical with the
   same content hash (and dedup outcome) the buffering implementation would
   have produced, while per-request memory stays bounded by a fixed budget
   independent of file size.
2. **Given** two concurrent large uploads, **When** both stream, **Then**
   combined memory use is the sum of two fixed budgets — not of the two file
   sizes.
3. **Given** the size limit, **When** an upload exceeds it, **Then** the
   upload is rejected as soon as the limit is crossed (not after the whole
   body is consumed) and no permanent object remains.

---

### User Story 2 - Image needing canonicalization streams through the converter (Priority: P2)

A user uploads an image that the service canonicalizes (HEIC/WebP → JPEG,
EXIF orientation fix, recompression). The compressed input is pulled by the
converter on demand and the converted output is encoded directly to storage —
neither the encoded input nor the encoded output is ever held whole in
memory.

**Why this priority**: Images are the transcoding case the buffering design
existed for; streaming them closes the original reason for whole-file reads.
Decoded pixel data inherently occupies memory during conversion, so the win
here is bounding memory by the *decoded working set* instead of additionally
holding both compressed copies.

**Independent Test**: Upload a large transcodable image and verify the stored
output is identical to what the buffered implementation produces **with the
chosen streaming parameters (baseline JPEG — SC-002 as amended)**, dimensions
metadata is still captured, and peak memory excludes the compressed input and
output copies.

**Acceptance Scenarios**:

1. **Given** a transcodable image (HEIC, WebP, rotated JPEG, canonical
   JPEG — clarified 2026-06-12), **When** uploaded, **Then** the stored
   bytes, stored MIME type, and recorded dimensions are identical to the
   buffering implementation's output with the chosen streaming parameters.
2. **Given** an image format the service does not re-encode through the
   streaming saver (GIF, SVG — and BMP/AVIF, which the image library cannot
   stream-encode; implementation delta, see research R5), **When** uploaded,
   **Then** it follows the pass-through streaming path (US1) without a
   decode, with dimensions arriving via the existing lazy backfill.
   JPEG/PNG/WebP/HEIC always stream through the encoder (clarified
   2026-06-12: the recompress size guard is dropped; recompressed output may
   occasionally exceed the original — accepted).
3. **Given** a truncated or corrupt image stream, **When** conversion fails,
   **Then** the upload fails with the existing image-processing error and no
   permanent object remains.

---

### User Story 3 - Editor save-back (content replace) streams the same way (Priority: P3)

A document saved back through the collaborative editor (content-replace
operation) is ingested through the same streaming pipeline, with the MIME
reconciliation and rejection semantics of feature 019 intact.

**Why this priority**: The replace path shares the ingest machinery; leaving
it buffered would keep the memory ceiling for editor saves and fork the
pipeline into two behaviors. It is third because editor saves are bounded in
practice by document sizes today.

**Independent Test**: Replace a document's content with a large body and
verify constant-memory ingestion plus unchanged 019 semantics (generic-sniff
fallback, EMPTY_CONTENT and MIME_MISMATCH rejections, atomicity).

**Acceptance Scenarios**:

1. **Given** an existing document, **When** its content is replaced via the
   internal replace operation, **Then** ingestion is streaming (fixed memory
   budget) and all feature-019 reconciliation outcomes are preserved.
2. **Given** a replace that is rejected (empty body, type mismatch), **When**
   the rejection occurs, **Then** no partial bytes have reached the permanent
   object for that document and the stored content/type are unchanged.

---

### Edge Cases

- Client aborts mid-upload: the partially received bytes never become a
  permanent object; any staging artifact is cleaned up.
- Stalled upload (no bytes arriving): aborted by the idle timeout (~30 s of
  no progress), counted distinctly from client aborts; same cleanup
  guarantee. A slow-but-progressing upload is never killed for duration
  alone.
- Storage failure mid-write: same guarantee — no partial permanent object, no
  document row referencing one.
- Hash collision/dedup: the content hash is only final at end-of-stream;
  dedup decisions (existing row reuse, skip-dedup flows) must produce the
  same outcomes as today, including when the duplicate is detected only
  after bytes were staged.
- Type sniffing requires only the head of the stream; the sniff must not
  force buffering beyond a small fixed prefix.
- Pixel bomb (tiny compressed image declaring enormous dimensions): rejected
  by the pixel-dimension budget before any decode; never OOMs the service.
- Already-optimized JPEG input: recompression may produce a slightly larger
  stored file than the original (the buffered size guard is gone under
  streaming) — accepted per clarification; storage remains content-addressed
  and correct.
- Zero-byte and tiny files: behave exactly as today (019 rules unchanged).
- Concurrent identical uploads: both stream; content-addressed storage must
  end with one blob and correct rows, as today.

## Requirements *(mandatory)*

### Functional Requirements

- **FR-001**: Uploaded content MUST flow from the request to permanent
  storage without the service ever holding the complete file in memory;
  per-request memory MUST be bounded by a fixed budget independent of file
  size (plus, for transcoded images only, the decoded working set).
- **FR-002**: Content hashing (the storage identity) MUST be computed during
  streaming; the resulting identity, dedup behavior, and stored bytes MUST be
  indistinguishable from the buffering implementation for the same input.
- **FR-003**: Type detection MUST operate on a bounded stream prefix and MUST
  NOT degrade the existing detection or (019) reconciliation outcomes.
- **FR-004**: Images requiring canonicalization MUST be converted in
  streaming fashion — compressed input pulled on demand, converted output
  written directly onward — producing output identical to the buffered
  conversion.
- **FR-005**: Size limits MUST be enforced incrementally during streaming;
  exceeding a limit aborts the upload promptly without consuming the
  remainder of the body. The maximum upload size MUST become a configuration
  value, defaulting to the current 32 MiB (no rollout behavior change) and
  supported — validated — up to 1 GiB; memory cost stays fixed at any
  setting. Raising the cap in an environment is a config-only operation
  (paired with the matching ingress limit, an infra-ops concern).
- **FR-006**: Ingestion MUST be atomic with respect to permanent storage: an
  upload that fails or is aborted at any point leaves no partial permanent
  object and no document row referencing one; staging artifacts are cleaned
  up.
- **FR-007**: The content-replace path MUST use the same streaming pipeline
  while preserving every feature-019 behavior (reconciliation matrix,
  EMPTY_CONTENT / MIME_MISMATCH rejections, rejection-before-side-effects,
  observability counters).
- **FR-008**: Streaming ingestion MUST be observable: per-outcome counters
  and structured logs for aborted, over-limit, stalled (idle-timeout), and
  failed-mid-stream uploads, sufficient to distinguish client aborts from
  service-side failures.
- **FR-009**: Upload requests MUST be governed by a progress-based idle
  timeout: the upload remains alive while bytes keep arriving and is aborted
  after a bounded period (~30 s) without progress. Non-upload endpoints
  retain the existing strict request timeouts. An aborted-for-stall upload
  follows the same atomicity guarantee as any failed upload (FR-006).
- **FR-010**: Images entering the transcode path MUST pass a configurable
  pixel-dimension budget, evaluated from the stream header before any
  decoding begins; images exceeding it are rejected with an explicit error
  (counted and logged per FR-008). With the image library's disc-backed
  decode landed, RAM for most formats is bounded by the configured
  threshold; the pixel budget remains permanently load-bearing for formats
  whose codecs decode whole-frame internally (HEIC), and defends CPU time
  and scratch-disk space for all formats.

### Key Entities

- **Upload stream**: the request body as a one-pass byte source; the unit the
  memory budget applies to.
- **Staging artifact**: any intermediate representation of not-yet-committed
  content (temporary object/file); MUST never be observable as a permanent
  object.
- **Document / content blob**: unchanged from today — content-addressed blob
  plus document row; only the path bytes take to get there changes.

## Success Criteria *(mandatory)*

### Measurable Outcomes

- **SC-001**: Peak per-request memory during a non-image upload is constant
  (fixed budget — pinned by the plan at 256 KiB copy buffer + 3 KiB sniff
  prefix, plus a 64 KiB image-header probe on the transcode route only) for
  files from 1 MiB up to 1 GiB (the validated ceiling of the configurable
  cap).
- **SC-002**: For every **non-transcoded** input in the regression corpus,
  stored bytes, content hash, stored MIME type, and dimensions metadata are
  identical to the pre-change implementation. For **transcoded** images,
  stored bytes are byte-identical to the buffered export with the chosen
  streaming-capable parameters (baseline JPEG — plan decision R7, goldens
  re-baselined); MIME type and dimensions are identical to pre-change.
- **SC-003**: An upload exceeding the configured size limit is rejected
  before the full body is transferred, and storage contains no artifact of it
  afterwards.
- **SC-004**: A client abort or mid-stream failure leaves zero partial
  permanent objects across the regression suite's fault-injection runs.
- **SC-005**: The service sustains **8 concurrent** maximum-size uploads
  within 8 × (fixed budget) + transcode working sets of memory — a load that
  would OOM the buffering implementation (8 × 1 GiB buffered ≫ any sane
  container limit).
- **SC-006**: All feature-019 replace-path tests continue to pass unchanged.

## Assumptions

- Streaming decode/encode for image conversion is provided by the project's
  image-processing library fork (antst/govips#2: reader-fed load, writer-fed
  save, one-shot reader→writer transcode with automatic path selection,
  sequential fast path, disc-backed materialization with configurable
  threshold and scratch directory, bounded header buffering). The service
  consumes the fork until the change is accepted upstream; upstreaming
  happens after this feature has hardened it (explicitly out of scope here).
- Decode memory for most formats is bounded by the library's configured
  disc threshold (frames above it materialize to scratch disk); formats
  whose codecs decode whole-frame internally (HEIC) and interlaced inputs
  are instead bounded by the FR-010 pixel budget and the library's pipe
  read limit. The library's threshold, scratch directory, and pipe read
  limit are service configuration; scratch disk capacity is a deployment
  concern.
- Content-addressed storage naming (hash as identity) is unchanged; computing
  the identity at end-of-stream and committing a staged object is an
  acceptable storage-layer strategy. The staging/commit contract MUST be
  expressible per backend and MUST NOT assume an atomic rename primitive:
  a filesystem backend may commit via rename, while an object store (S3 —
  a planned backend) commits via complete-multipart-upload plus server-side
  copy to the content-addressed key (with abort-multipart as the free
  no-partial-object guarantee). The fixed memory budget (FR-001) is a
  per-backend constant (e.g. part-buffer × concurrency for multipart
  object stores), never a function of file size.
- The multipart upload framing of the existing API is unchanged — only the
  internal handling of the file part streams.
- This feature is developed as a stacked branch on `019-preserve-mime-on-edit`
  (PR #29): US3 builds on the final 019 replace-path code, and this PR merges
  after #29 does.

## Out of Scope

- Upstreaming the image-library streaming support (follow-up after this
  feature stabilizes).
- Further image-library fork changes — the sequential-streaming /
  disc-backed-decode workstream has landed on antst/govips#2 and is
  sufficient; streaming HEIC decode would require upstream libheif work and
  is explicitly not pursued (FR-010 covers it).
- Changing the public/internal API shapes, dedup semantics, or bucket
  policy semantics.
- Download/serving path changes (already streams from storage; not part of
  this feature).
- Resumable/chunked upload protocols (single-request uploads only).
