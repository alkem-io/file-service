# Feature Specification: Preserve Document MIME Type Across Content Edits

**Feature Branch**: `019-preserve-mime-on-edit`
**Created**: 2026-06-11
**Status**: Draft
**Backlog Story**: https://github.com/alkem-io/client-web/issues/9849 (#9849)
**Input**: User description: "Preserve a document's MIME type across content edits, so office documents stay openable after editing in Collabora."

## Problem Statement

Office documents (.pptx/.docx/.xlsx) edited through the Collabora/WOPI editor become
permanently unopenable after their first save — the user sees "document temporarily
unavailable," and the failure persists across refreshes.

When the editor saves a document back, the content-replace operation re-detects the
MIME type from the raw uploaded bytes and overwrites the document's stored type.
Content-sniffing is unreliable for exactly these inputs:

- Collabora writes OOXML zip archives with `[Content_Types].xml` not as the first
  entry, so detection cannot recognize them as office formats and falls back to the
  generic `application/zip`.
- Newly created blank documents and failed/empty saves have 0-byte content, which is
  detected as `text/plain`.

Once the stored type is a non-office value, no editor can be resolved for the document
(editor-session issuance fails), so it can never be opened again. The creation path
already avoids this — it reconciles the declared type, the content, and the storage
bucket's allowed types, and trusts the declared type for empty content — but the
content-replace path does not: it sniffs and overwrites, and performs no allowed-type
policy check. The two paths are inconsistent. On the acceptance environment there are
currently ~10 corrupted records (6 stored as `application/zip`, 4 zero-byte stored as
`text/plain`), all office documents that were edited or newly created.

## Clarifications

### Session 2026-06-11

- Q: What should content-replace do when replacement content is unambiguously a different concrete type than the stored type? → A: Reject the replacement with an explicit error; stored content and type stay unchanged.
- Q: What should happen when a 0-byte (empty) replacement is received for a document that already has content (e.g., a failed editor save)? → A: Reject it with an explicit error; existing content and type are preserved. A valid office file is never 0 bytes, so an empty body always signals a failed save.
- Q: Could the zero-content files be results of failed writes (not just never-saved placeholders)? → A: Yes. New documents are created as 0-byte placeholders with the *correct* canonical MIME; the four corrupted rows are `text/plain`, which only occurs when the replace path actually accepted and re-sniffed an empty body. They are the residue of empty/failed writes being accepted. Consequences: replacement must be atomic (no partial/empty content persisted on failure), and remediation must first check whether prior content survives in content-addressed storage before declaring it unrecoverable.
- Q: How is the corrupted-row remediation delivered? → A: Shipped with the service and run automatically in every environment (the bug is labeled `production`, so acceptance is unlikely to be the only damaged environment; counts are re-verified at run time).
- Q: Shipped how — the `file` table's schema migrations are owned by the server repo; does this become a multi-repo fix? → A: No. Remediation is delivered as an idempotent self-repair job inside this service (which already owns runtime writes to the stored type — that is the bug). It is a data repair, not a schema change, so server-side migration ownership is not violated and the fix stays single-repo.
- Q: What observability is required for the new protective behaviors (sniff fallbacks, rejected saves, repair actions)? → A: Every fallback, rejection, and repair action emits a structured log with document identity and reason; per-outcome metrics are exposed; rejected saves produce an alertable signal (a spike means an editor integration broke). The original corruption went unnoticed for months precisely because it was silent.
- Q: What does the end user in the editor experience when a save is rejected? → A: The rejection propagates as a proper error so the editor shows its native "save failed" notification; the editing session stays open with the user's changes intact, allowing retry or export. Explicit failure beats silent data corruption — and silent-discard is explicitly ruled out.

## User Scenarios & Testing *(mandatory)*

### User Story 1 - Edited office document stays openable (Priority: P1)

A user opens a presentation (.pptx) in the collaborative editor, makes a change, and
the editor saves it back. The document keeps its presentation type and reopens in the
editor every subsequent time.

**Why this priority**: This is the core corruption: a single edit permanently bricks
the document. It causes data inaccessibility for end users and is the direct cause of
the reported outage.

**Independent Test**: Replace an existing office document's content with a
Collabora-style OOXML zip (with `[Content_Types].xml` not as the first archive entry)
and verify the stored type is unchanged and the document remains openable.

**Acceptance Scenarios**:

1. **Given** a stored .pptx document, **When** the editor saves replacement content
   whose zip entries are reordered so generic detection sees only `application/zip`,
   **Then** the stored type remains the presentation type and the document reopens
   normally.
2. **Given** a stored .docx or .xlsx document, **When** its content is replaced with a
   valid same-type office file, **Then** the stored type is preserved as the canonical
   office type.
3. **Given** any office document, **When** content replacement completes, **Then** the
   stored type is never one of the generic/ambiguous values `application/zip`,
   `application/octet-stream`, or `text/plain`.

---

### User Story 2 - New blank document survives its first save (Priority: P2)

A user creates a new (blank) document, which is initially stored as a 0-byte
placeholder. The first save from the editor writes real content, and the document keeps
its canonical office type throughout.

**Why this priority**: New-document creation is a common flow and currently produces
`text/plain` records that can never be opened; it is the second-most frequent
corruption observed.

**Independent Test**: Create a 0-byte placeholder with an office type, push a first
real save through content replacement, and verify the type stays the office type.

**Acceptance Scenarios**:

1. **Given** a 0-byte placeholder document with an office type, **When** the first
   editor save writes real content, **Then** the stored type remains the canonical
   office type and never becomes `text/plain`.
2. **Given** a document with an office type, **When** an empty (0-byte) replacement is
   received (e.g., a failed save), **Then** the replacement is rejected and the
   document's existing content and type are preserved — never relabeled `text/plain`.

---

### User Story 3 - Genuinely different content is rejected, not silently relabeled (Priority: P3)

Replacement content that is unambiguously a different document type (e.g., a word
processing document pushed into a presentation slot) is rejected with an explicit
error — the stored content and type stay unchanged; never silently relabeled.

**Why this priority**: A real type change must not be a silent side effect of a content
save. This protects type integrity but occurs far less often than the corruption above.

**Independent Test**: Push a valid .docx body into an existing .pptx document and
verify the operation is rejected and the stored content and type are unchanged.

**Acceptance Scenarios**:

1. **Given** an existing presentation document, **When** replacement content is
   unambiguously detected as a different concrete type, **Then** the replacement is
   rejected with a clear error and the document's stored content and type are
   unchanged.
2. **Given** a bucket whose policy disallows the detected replacement type, **When**
   such content is pushed, **Then** the replacement is rejected with a clear error.

---

### Edge Cases

- Replacement content detected only as a generic/ambiguous type (`application/zip`,
  `application/octet-stream`, `text/plain`): the document's existing type is
  authoritative.
- 0-byte replacement content: rejected with an explicit error; existing content and
  type preserved; never re-detected from empty bytes.
- Replacement of a non-office document (e.g., an image) follows the same reconciliation
  rules without regression to current behavior.
- Already-corrupted records (generic type stored for an office document): remediation
  relabels recoverable records; zero-byte records are checked for a surviving prior
  content version in storage, restored where possible, and otherwise identified and
  reported rather than silently relabeled.
- A write failure mid-replacement (e.g., interrupted upload to object storage): the
  document's previously stored content and type remain intact (atomic replacement).

## Requirements *(mandatory)*

### Functional Requirements

- **FR-001**: A document's type MUST be established at creation and remain stable
  across content edits; editing a document of a given office type leaves it that type.
- **FR-002**: On content replacement, the stored type MUST never be downgraded to a
  generic/ambiguous value (`application/zip`, `application/octet-stream`, `text/plain`)
  on the basis of content detection.
- **FR-003**: When content detection yields only a generic/ambiguous result, the
  document's existing/known type MUST be treated as authoritative and the replacement
  accepted under that type.
- **FR-003a**: An empty (0-byte) replacement MUST be rejected with an explicit error;
  the document's existing content and type are preserved. Empty content is never
  written over existing content and never re-detected as a type.
- **FR-004**: Replacement content that is unambiguously a different concrete type than
  the document's stored type MUST be rejected with an explicit error; the stored
  content and type remain unchanged. The stored type is never silently relabeled.
- **FR-005**: The creation path and the content-replacement path MUST behave
  consistently in how they determine and validate the stored type, including enforcing
  the bucket's allowed-type policy on replacement.
- **FR-006**: Existing corrupted records MUST be remediated by an idempotent
  self-repair job that ships with the service and runs automatically in every
  environment on deploy: documents stored as `application/zip` with intact office
  content are relabeled to their correct office type. For zero-byte `text/plain`
  records (the residue of accepted empty/failed writes), remediation MUST first check
  whether a prior content version still exists in content-addressed storage and
  restore it if found; only records with no recoverable prior content are reported as
  unrecoverable. The job re-verifies affected records at run time rather than relying
  on counts observed at spec time.
- **FR-007**: Content replacement MUST be atomic: if a replacement fails at any point
  (validation rejection or write failure), the previously stored content and type
  MUST remain intact and retrievable. Partial or empty content is never persisted as
  a side effect of a failed save.
- **FR-008**: The protective behaviors MUST be observable: every generic-sniff
  fallback to the known type, every rejected replacement (type mismatch, empty body,
  bucket policy), and every self-repair action MUST emit a structured log carrying
  the document identity and reason, and MUST be counted in per-outcome metrics.
  Rejected saves MUST produce an alertable signal, since a spike indicates a broken
  editor integration rather than user error.
- **FR-009**: A rejected replacement MUST surface to the caller as an explicit error
  so the editor can show its native save-failure notification; the user's editing
  session remains open with their changes intact (retry and export remain possible).
  Reporting a rejected save as successful while discarding its content is explicitly
  forbidden.

### Key Entities

- **Document**: A stored file with identity, content, and an authoritative MIME type
  established at creation; referenced by editors via its type to resolve editing
  capability.
- **Bucket**: A storage destination with a policy defining which MIME types it accepts.
- **Content replacement**: The operation that overwrites a document's content (editor
  save-back); currently the only path that mutates the stored type as a side effect.

## Success Criteria *(mandatory)*

### Measurable Outcomes

- **SC-001**: An office document edited and saved through the collaborative editor
  reopens successfully 100% of the time, across any number of edit/save cycles.
- **SC-002**: After the fix, zero office-named documents are stored with
  `application/zip`, `application/octet-stream`, or `text/plain` types.
- **SC-003**: Corrupted `application/zip` office documents are relabeled to their
  correct office types and reopen in the editor, in every environment (6 known on
  acceptance at spec time; production to be verified by the repair job).
- **SC-004**: The 4 known zero-byte `text/plain` records are checked for a surviving
  prior content version and restored where one exists; the rest are reported as
  unrecoverable. No new zero-byte office records occur after the fix.
- **SC-005**: Replacing a document's content with a genuinely different document type
  is rejected with an explicit error — never a silent type change, never a silent
  content swap.
- **SC-006**: When a save is rejected, the user sees a save-failure notification in
  the editor and their editing session remains usable (changes still on screen,
  retry/export possible); rejected saves are visible in service metrics.

## Assumptions

- The document's stored type at creation time is trustworthy (the creation path already
  reconciles declared type, content, and bucket policy).
- Content detection remains in use as a *guard* ("does this look like what it claims?")
  but is no longer the *source of truth* for a document's type on replacement.
- Remediation is a data repair within this service's own data, shipped as an
  idempotent self-repair job — not a schema migration (schema migrations for this
  table are owned elsewhere; this service already owns runtime writes to the stored
  type). The 4 known zero-byte records resulted from accepted empty/failed writes;
  their content is recoverable only if a prior version still exists in
  content-addressed storage — otherwise those documents need re-creating by users.
- No changes are required in any other service or client: the editor-URL/extension
  fallback in the WOPI service is optional defense-in-depth and out of scope; the
  original bug report against the web client is mis-filed — no client change is needed.

## Out of Scope

- Editor-URL/extension fallback in the WOPI service (separate, optional
  defense-in-depth).
- Any web-client change.
- Changes to how document types are established at creation time.
