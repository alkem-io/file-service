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
output is identical to what the buffered implementation produces, dimensions
metadata is still captured, and peak memory excludes the compressed input and
output copies.

**Acceptance Scenarios**:

1. **Given** a transcodable image (HEIC, WebP, rotated JPEG), **When**
   uploaded, **Then** the stored bytes, stored MIME type, and recorded
   dimensions are identical to the buffering implementation's output.
2. **Given** a non-transcodable image (already-canonical JPEG/PNG within the
   size guard), **When** uploaded, **Then** it follows the pass-through
   streaming path (US1) without a decode.
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
- Storage failure mid-write: same guarantee — no partial permanent object, no
  document row referencing one.
- Hash collision/dedup: the content hash is only final at end-of-stream;
  dedup decisions (existing row reuse, skip-dedup flows) must produce the
  same outcomes as today, including when the duplicate is detected only
  after bytes were staged.
- Type sniffing requires only the head of the stream; the sniff must not
  force buffering beyond a small fixed prefix.
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
  remainder of the body. The maximum upload size MUST become configurable
  beyond the current fixed request cap, safely (memory cost stays fixed).
- **FR-006**: Ingestion MUST be atomic with respect to permanent storage: an
  upload that fails or is aborted at any point leaves no partial permanent
  object and no document row referencing one; staging artifacts are cleaned
  up.
- **FR-007**: The content-replace path MUST use the same streaming pipeline
  while preserving every feature-019 behavior (reconciliation matrix,
  EMPTY_CONTENT / MIME_MISMATCH rejections, rejection-before-side-effects,
  observability counters).
- **FR-008**: Streaming ingestion MUST be observable: per-outcome counters
  and structured logs for aborted, over-limit, and failed-mid-stream uploads,
  sufficient to distinguish client aborts from service-side failures.

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
  (fixed budget) for files from 1 MiB to at least 10× the current 32 MiB cap.
- **SC-002**: For every input in the regression corpus (non-images, canonical
  images, transcodable images), stored bytes, content hash, stored MIME type,
  and dimensions metadata are identical to the pre-change implementation.
- **SC-003**: An upload exceeding the configured size limit is rejected
  before the full body is transferred, and storage contains no artifact of it
  afterwards.
- **SC-004**: A client abort or mid-stream failure leaves zero partial
  permanent objects across the regression suite's fault-injection runs.
- **SC-005**: The service sustains N concurrent maximum-size uploads within
  N × (fixed budget) + transcode working sets of memory — demonstrated at a
  concurrency level that would OOM the buffering implementation.
- **SC-006**: All feature-019 replace-path tests continue to pass unchanged.

## Assumptions

- Streaming decode/encode for image conversion is provided by the
  project's image-processing library fork (antst/govips#2: reader-fed load,
  writer-fed save, bounded header buffering). The service consumes the fork
  until the change is accepted upstream; upstreaming happens after this
  feature has hardened it (explicitly out of scope here).
- Decoded pixel data during image conversion inherently occupies memory; the
  budget guarantee for transcoded images is "no whole compressed copies",
  not "no decode working set".
- Content-addressed storage naming (hash as identity) is unchanged; computing
  the identity at end-of-stream and committing/renaming a staged object is an
  acceptable storage-layer strategy.
- The multipart upload framing of the existing API is unchanged — only the
  internal handling of the file part streams.

## Out of Scope

- Upstreaming the image-library streaming support (follow-up after this
  feature stabilizes).
- Changing the public/internal API shapes, dedup semantics, or bucket
  policy semantics.
- Download/serving path changes (already streams from storage; not part of
  this feature).
- Resumable/chunked upload protocols (single-request uploads only).
