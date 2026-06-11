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
   received (e.g., a failed save), **Then** the document's existing type is preserved
   (or the replacement is rejected) — never relabeled `text/plain`.

---

### User Story 3 - Genuinely different content is reconciled, not silently relabeled (Priority: P3)

Replacement content that is unambiguously a different document type (e.g., a word
processing document pushed into a presentation slot) is reconciled against the
document's known type and the destination bucket's allowed types — rejected or handled
explicitly, never silently relabeled.

**Why this priority**: A real type change must not be a silent side effect of a content
save. This protects type integrity but occurs far less often than the corruption above.

**Independent Test**: Push a valid .docx body into an existing .pptx document and
verify the operation is rejected (or explicitly reconciled per bucket policy) rather
than the stored type being silently switched.

**Acceptance Scenarios**:

1. **Given** an existing presentation document, **When** replacement content is
   unambiguously detected as a different concrete type, **Then** the system reconciles
   against the document's known type and the bucket's allowed types and does not
   silently relabel the document.
2. **Given** a bucket whose policy disallows the detected replacement type, **When**
   such content is pushed, **Then** the replacement is rejected with a clear error.

---

### Edge Cases

- Replacement content detected only as a generic/ambiguous type (`application/zip`,
  `application/octet-stream`, `text/plain`): the document's existing type is
  authoritative.
- 0-byte replacement content: existing type preserved (or replacement rejected); never
  re-detected from empty bytes.
- Replacement of a non-office document (e.g., an image) follows the same reconciliation
  rules without regression to current behavior.
- Already-corrupted records (generic type stored for an office document): remediation
  relabels recoverable records; zero-byte placeholders with no recoverable content are
  identified and reported rather than silently relabeled.

## Requirements *(mandatory)*

### Functional Requirements

- **FR-001**: A document's type MUST be established at creation and remain stable
  across content edits; editing a document of a given office type leaves it that type.
- **FR-002**: On content replacement, the stored type MUST never be downgraded to a
  generic/ambiguous value (`application/zip`, `application/octet-stream`, `text/plain`)
  on the basis of content detection.
- **FR-003**: When content detection is ambiguous, or the replacement content is empty,
  the document's existing/known type MUST be treated as authoritative.
- **FR-004**: Replacement content that is unambiguously a different concrete type MUST
  be reconciled against the document's known type and the destination bucket's allowed
  types — rejected or handled explicitly, never silently relabeled.
- **FR-005**: The creation path and the content-replacement path MUST behave
  consistently in how they determine and validate the stored type, including enforcing
  the bucket's allowed-type policy on replacement.
- **FR-006**: Existing corrupted records MUST be remediated: documents stored as
  `application/zip` with intact office content are relabeled to their correct office
  type; zero-byte `text/plain` placeholders are identified and reported as
  unrecoverable (their content was never written).

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
- **SC-003**: The 6 known corrupted `application/zip` documents on the acceptance
  environment are relabeled to their correct office types and reopen in the editor.
- **SC-004**: The 4 known zero-byte `text/plain` placeholders are identified and
  reported as unrecoverable, with no new occurrences after the fix.
- **SC-005**: Replacing a document's content with a genuinely different document type
  results in an explicit rejection or reconciliation outcome — never a silent type
  change.

## Assumptions

- The document's stored type at creation time is trustworthy (the creation path already
  reconciles declared type, content, and bucket policy).
- Content detection remains in use as a *guard* ("does this look like what it claims?")
  but is no longer the *source of truth* for a document's type on replacement.
- Remediation of corrupted records is a one-off data correction within this service's
  own data; the 4 zero-byte placeholders have no recoverable content and would need to
  be re-created by users.
- No changes are required in any other service or client: the editor-URL/extension
  fallback in the WOPI service is optional defense-in-depth and out of scope; the
  original bug report against the web client is mis-filed — no client change is needed.

## Out of Scope

- Editor-URL/extension fallback in the WOPI service (separate, optional
  defense-in-depth).
- Any web-client change.
- Changes to how document types are established at creation time.
