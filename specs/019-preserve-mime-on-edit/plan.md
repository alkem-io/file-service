# Implementation Plan: Preserve Document MIME Type Across Content Edits

**Branch**: `019-preserve-mime-on-edit` | **Date**: 2026-06-11 | **Spec**: [spec.md](./spec.md)
**Input**: Feature specification from `/specs/019-preserve-mime-on-edit/spec.md`
**Backlog Story**: https://github.com/alkem-io/client-web/issues/9849

## Summary

The content-replace path (`PUT /internal/file/{id}/content` → `FileService.StoreAndLink`)
blindly re-sniffs the MIME type from raw bytes and overwrites the stored `mimeType`,
corrupting office documents saved through Collabora (reordered OOXML zips sniff as
`application/zip`; empty bodies sniff as `text/plain`), after which WOPI editor
resolution fails permanently. The fix makes the replace path reconcile against the
document's **known** type instead of trusting the sniff: generic/ambiguous sniffs keep
the known type, empty bodies are rejected, unambiguous type mismatches are rejected,
and every protective action is logged and counted. A startup self-repair job relabels
already-corrupted rows. Only `StoreAndLink` and its handler change behavior; the
create path is already correct and is the consistency reference.

Key scoping fact (verified): the **only writer** through the content-replace endpoint
is wopi-service (Collabora save-back). The server adapter only GETs content. So
tightening replace semantics cannot regress any other flow.

## Technical Context

**Language/Version**: Go 1.26.1
**Primary Dependencies**: chi v5 (HTTP), pgx/v5 + sqlc (DB), zap (logging),
`gabriel-vasile/mimetype` v1.4.13 (content sniffing), govips (image processing),
expvar (metrics — existing convention, no Prometheus dependency)
**Storage**: PostgreSQL `file` table (shared Alkemio DB, full CRUD owned here per
constitution III); content-addressed blob storage behind `port.StoragePort`
(Save/Read/Delete/Exists)
**Testing**: `go test` table-driven unit tests; 95% unit coverage target
(constitution XII); `make lint` (golangci-lint) on completion
**Target Platform**: Linux container (k8s), local via Makefile
**Project Type**: Single Go web service (hexagonal)
**Performance Goals**: No change — replace path adds O(1) string comparisons; repair
job is a one-shot startup scan bounded by corrupted-row count
**Constraints**: No schema change (the `file` table's migrations are server-owned);
repair is data-only via existing CRUD ownership. No new external dependencies.
**Scale/Scope**: ~460 office docs on acc, ~10 corrupted; repair job must be
idempotent and safe to run on every boot in every environment

## Constitution Check

*GATE: evaluated against `.specify/memory/constitution.md` v1.3.0*

| Principle | Status | Notes |
|---|---|---|
| I. Hexagonal Architecture | ✅ | All logic in `internal/domain/service`; sniffing stays behind `port.ImageProcessor`; repair job is domain service code wired from `cmd/server/app.go`; no adapter-to-adapter imports |
| II. Storage Abstraction | ✅ | Repair uses existing `StoragePort.Read/Exists`; no backend-specific code |
| III. Alkemio Integration First | ✅ | `file` table full CRUD is this service's right; data repair, not schema change |
| IV. Type-Safe Database Access | ✅ | New queries (`ListSuspectMimeRows`, `UpdateMimeType`) added in `db/queries/document.sql` and generated via sqlc — no hand-written SQL in Go |
| V. Security by Design | ✅ | Replace path gains validation it lacked (rejects smuggled type changes); internal endpoint unchanged otherwise |
| VI. Test-First Development | ✅ | Reconciliation matrix and repair-job tests written first (see tasks) |
| VII. Root Cause Analysis | ✅ | Root cause (sniff-as-source-of-truth) fixed at source, not patched downstream |
| VIII. DRY | ✅ | One reconciliation function; one office-MIME map in `internal/domain/model` |
| XI. No Busywork / XII. Meaningful Tests | ✅ | Tests assert the corruption matrix from the spec, not boilerplate |
| Anti-Pattern #11 (no `map[string]any` responses) | ✅ | New 422 error body uses a named struct with `Render()` |

**Violations**: none. Complexity Tracking not required.

## Project Structure

### Documentation (this feature)

```text
specs/019-preserve-mime-on-edit/
├── spec.md
├── plan.md              # This file
├── research.md          # Phase 0
├── data-model.md        # Phase 1
├── quickstart.md        # Phase 1
├── contracts/
│   └── openapi-delta.md # Phase 1 (repo convention, cf. 018)
└── tasks.md             # /speckit-tasks output (not created here)
```

### Source Code (repository root)

```text
internal/
├── domain/
│   ├── model/
│   │   └── mime.go                # NEW: office MIME map (ext ↔ canonical), generic-MIME set
│   ├── port/
│   │   └── document_repo.go       # +ListSuspectMimeRows, +UpdateMimeType
│   └── service/
│       ├── file_service.go        # StoreAndLink: reconcile instead of raw sniff; new errors
│       ├── file_service_test.go   # reconciliation matrix tests
│       ├── mime_repair.go         # NEW: startup self-repair job
│       └── mime_repair_test.go    # NEW
├── adapter/
│   ├── inbound/http/
│   │   ├── document_handler.go    # ReplaceContent: map new errors → 422 named-struct body
│   │   ├── dto.go                 # error-response struct
│   │   └── metrics.go             # +ReplaceOutcomes, +MimeRepairOps expvar maps
│   └── outbound/alkemiodb/
│       ├── document_repo.go       # implement new port methods
│       └── queries/               # sqlc-generated (from db/queries/document.sql)
db/queries/document.sql            # +ListSuspectMimeRows, +UpdateMimeType
cmd/server/app.go                  # wire repair job at startup (goroutine, post-DB)
openapi.yaml                       # 422 responses for PUT …/content
```

**Structure Decision**: single hexagonal Go service; all behavior change is inside the
existing domain service + its HTTP adapter, plus one new domain file each for the MIME
vocabulary and the repair job.

## Design Outline

### 1. Reconciliation in `StoreAndLink` (FR-001..005, FR-007)

Replace the raw `DetectMIME` with `reconcileReplaceMIME(knownMIME string, content []byte)`:

| Input | Outcome |
|---|---|
| `len(content) == 0` | `ErrEmptyContent` → 422 (FR-003a) |
| sniff ∈ generic set (`application/zip`, `application/octet-stream`, `text/plain`) | keep `knownMIME`, log+count `fallback` (FR-002/003) |
| sniff == `knownMIME` (normalized) | keep, count `accepted` |
| sniff concrete ≠ known | `ErrMimeMismatch` → 422 (FR-004) |

All validation happens **before** `Storage.Save` — a rejection has zero side effects,
and the existing Save→UpdateFile→cleanup ordering already leaves the old row intact on
later failures (FR-007). Bucket allow-list on replace is enforced **by invariant**:
the stored type can no longer change on replace, and it passed the allow-list at
creation (see research.md R3 — the policy is not locally readable, and doesn't need
to be).

### 2. HTTP contract (FR-009)

`ReplaceContent` maps `ErrEmptyContent`/`ErrMimeMismatch` to **422** with a named
error struct carrying a machine-readable `code` (`EMPTY_CONTENT`, `MIME_MISMATCH`).
wopi-service already turns any file-service error into a PutFile failure → Collabora
shows its native save-failed UI. **No wopi-service change required.**

### 3. Observability (FR-008)

Two expvar maps (existing metrics convention): `content_replace_outcomes_total`
(keys: `accepted`, `fallback_generic_sniff`, `rejected_empty`, `rejected_mismatch`)
and `mime_repair_total` (keys: `relabeled`, `unrecoverable`, `skipped_not_office`,
`errors`). Every fallback/rejection/repair emits a structured zap log with document
ID, known MIME, sniffed MIME, and reason. Alerting keys off the `rejected_*` counters.

### 4. Startup self-repair job (FR-006)

`mime_repair.go`, launched as a goroutine from `app.go` after DB connect:

1. `ListSuspectMimeRows`: rows where `mimeType` ∈ generic set **and** `displayName`
   ends in a known office extension.
2. Per row: read content via `StoragePort`.
   - Non-empty + zip magic (`PK`) → `UpdateMimeType` to the extension's canonical
     office MIME. (Content verified, not just name-trusted.)
   - Empty → log + count `unrecoverable` (prior-version restore is impossible — no
     record of the prior blob hash exists; see research.md R5).
3. Idempotent by construction: repaired rows no longer match the suspect query.

## Phase Outputs

- **Phase 0**: [research.md](./research.md) — 6 decisions, no NEEDS CLARIFICATION left
- **Phase 1**: [data-model.md](./data-model.md), [contracts/openapi-delta.md](./contracts/openapi-delta.md), [quickstart.md](./quickstart.md)

## Post-Design Constitution Re-Check

Re-evaluated after Phase 1: still no violations. The design adds no projects, no new
dependencies, no schema changes, and keeps all new logic in the domain core behind
existing ports.
