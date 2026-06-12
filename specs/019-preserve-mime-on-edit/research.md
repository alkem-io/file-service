# Research: Preserve Document MIME Type Across Content Edits

All unknowns from Technical Context resolved. Sources: codebase reads of
`file-service` (this repo), `wopi-service`, `server` (sibling repos), the 2026-06-09
root-cause debugging session, Collabora forum/SDK references recorded in spec.md.

## R1 — Reconciliation function shape

**Decision**: Add `reconcileReplaceMIME(knownMIME string, content []byte)` to the
domain service, used only by `StoreAndLink`. Do **not** reuse `resolveMIME` directly.

**Rationale**: `resolveMIME` (create path, `file_service.go:221`) answers "what type
is this new content, validated against a caller-supplied allow-list?" — for empty
content it *trusts the declared type*, which on replace would silently accept empty
bodies (the exact failure we're fixing). Replace answers a different question: "is
this content compatible with the type this document already has?" Sharing the
generic-set logic via small helpers keeps DRY without contorting `resolveMIME`'s
contract or its create-path semantics.

**Alternatives considered**: (a) Extend `resolveMIME` with a mode flag — rejected:
two behaviors under one signature, harder to test, violates the spec's "empty on
replace = reject" vs "empty on create = trust declared" asymmetry. (b) Sniff-then-
patch in the handler — rejected: business rule belongs in the domain core
(constitution I).

## R2 — Generic/ambiguous MIME set

**Decision**: `{application/zip, application/octet-stream, text/plain}` exactly, per
spec FR-002, compared after `normalizeMIME` (lowercase, parameters stripped).

**Rationale**: These are the only values `gabriel-vasile/mimetype` v1.4.13 degrades
to for the inputs in scope: reordered OOXML zips → `application/zip` (its `msoxml`
matcher needs `[Content_Types].xml` near the front of the archive), empty/degenerate
bodies → `text/plain`, unknown binary → `application/octet-stream`.

**Alternatives considered**: Adding `application/x-zip-compressed` and friends —
deferred until observed; the fallback log (FR-008) will surface any new generic value
in the wild, which is exactly what the observability requirement is for.

## R3 — Bucket allow-list enforcement on replace

**Decision**: Enforce **by invariant**, not by lookup: since replace can no longer
change the stored type (generic → keep known; mismatch → reject), the stored type is
always one that passed `allowedMimeTypes` validation at creation. FR-005's
"consistent validation" is satisfied with no policy read.

**Rationale (and constraint)**: file-service does not *have* the bucket policy:
`allowedMimeTypes` arrives as a request parameter on create (sent by the server
adapter) and is not persisted locally; the `storage_bucket` table is server-owned.
The replace caller (wopi-service) cannot supply it. The invariant route is therefore
not just cleaner — it is the only implementable one without a cross-service call.

**Alternatives considered**: (a) wopi-service forwards a policy — rejected: wopi
doesn't know it either. (b) file-service queries the server's table — rejected:
crosses ownership boundary (constitution III) and adds a runtime coupling for zero
benefit given the invariant.

## R4 — HTTP error semantics for rejections

**Decision**: Both rejections return **422 Unprocessable Entity** with a named
response struct (`code`: `EMPTY_CONTENT` | `MIME_MISMATCH`, plus human-readable
`error`). 409 stays reserved for the existing dedup conflict.

**Rationale**: 422 is already the handler's vocabulary for "content understood but
unacceptable" (`ErrImageProcessing`). wopi-service's `PutFile` treats any non-2xx as
a failed save and Collabora then shows its native save-failed notification with the
session intact — exactly FR-009 — with **zero wopi-service changes** (verified:
`wopi_handler.go:112-124` logs and returns 500 to Collabora; Collabora keeps the
session open on failed PutFile).

**Alternatives considered**: 400 — wrong, the request is well-formed; 415 — describes
the *request* media type, not a content/identity mismatch; 409 — already means dedup
conflict here, overloading it would be ambiguous for the server adapter's
`fromHttpError` mapping.

## R5 — Zero-byte rows: prior-version restore feasibility

**Decision**: The repair job declares zero-byte rows unrecoverable after confirming
the current blob is empty. No restore attempt is made.

**Rationale**: Restoration requires the prior blob's hash (`externalID` is the
content hash), and **no record of it exists anywhere in this service** — the row's
`externalID` was overwritten in place, there is no history table, and
`StoreAndLink` deletes the orphaned old blob on content change
(`file_service.go:377-387`). The spec's "check whether a prior content version still
exists" (FR-006) therefore resolves to: verify emptiness, report. The spec already
sets this expectation ("recovery will rarely succeed").

**Alternatives considered**: Scanning object storage for orphan blobs and matching by
upload time — rejected: content-addressed keys carry no document linkage; any match
would be a guess, and writing guessed content into a user document is worse than
reporting honestly.

## R6 — Repair job delivery & office-MIME vocabulary

**Decision**: A goroutine launched from `cmd/server/app.go` after DB connect, running
once per boot; suspect rows selected by SQL (`mimeType` ∈ generic set AND lowercased
`displayName` LIKE one of the office extensions), content verified (non-empty + `PK`
zip magic) before relabeling via an `UpdateMimeType` repo method. The
extension↔canonical-MIME map lives once in `internal/domain/model/mime.go` covering
OOXML (`.docx/.xlsx/.pptx`) and OpenDocument (`.odt/.ods/.odp`) — the formats the
Collabora flow stores (acc data shows all of `presentationml`, `wordprocessingml`,
`spreadsheet` ODS rows).

**Rationale**: Runs automatically in every environment on deploy (the clarified
FR-006 delivery decision — the bug is labeled `production`); idempotent because
repaired rows stop matching the suspect predicate; data-only so it doesn't touch
server-owned schema migrations. sqlc keeps the queries type-safe (constitution IV).
Reusing `UpdateFile` for relabeling was rejected — it requires externalID/size/
content_metadata and re-stating them invites drift; a single-column update says what
it means.

**Alternatives considered**: (a) Server TypeORM migration — rejected in
clarification (would make the fix cross-repo). (b) Manual SQL — rejected in
clarification (relies on an operator per environment). (c) Blocking startup until
repair completes — rejected: a long storage read loop would delay readiness; the
job is independent of request serving.

## Post-review deltas (backfilled 2026-06-12 after PR #29 merged)

Three implementation facts diverged from the text above during CodeRabbit
review and are authoritative as shipped:

1. **Repair relabel is compare-and-set.** `UpdateMimeType(ctx, id,
   expectedExternalID, mimeType) (bool, error)` only applies while the row's
   `externalID` still equals the blob the repair job sniffed. The repair
   goroutine runs concurrently with request serving, so a Replace landing in
   the scan→relabel window must win: the guard failing returns false and the
   job skips the row (regression test
   `TestRunMimeRepair_ConcurrentReplaceLosesGuardSkipsRelabel`).
2. **Suspect scan splits SQL and domain filtering.** SQL selects by generic
   MIME only (`ListDocumentsByMimeTypes`); the office-extension check runs in
   the domain via `model.OfficeMIMEForName`, keeping the office vocabulary
   single-sourced (constitution VIII) instead of duplicating it in a SQL
   regex.
3. **Outcome counters live at the adapter.** The domain emits structured
   logs and reports the outcome on `StoredFile.ReplaceOutcome`; the HTTP
   adapter increments `content_replace_outcomes_total` (hexagonal rule — the
   domain never imports adapter metrics).
