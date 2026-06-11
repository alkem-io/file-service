# Tasks: Preserve Document MIME Type Across Content Edits

**Input**: Design documents from `/specs/019-preserve-mime-on-edit/`
**Prerequisites**: plan.md, spec.md, research.md, data-model.md, contracts/openapi-delta.md, quickstart.md

**Tests**: Included — constitution VI (Test-First Development) is non-optional in this
repo; every behavior task is preceded by a failing-test task. Coverage target 95% on
touched packages (constitution XII).

**Organization**: Phases 3–5 map 1:1 to the spec's prioritized user stories. The
remediation job (FR-006) is a cross-cutting deliverable serving SC-003/SC-004 and has
its own phase (no story label per format rules).

## Format: `[ID] [P?] [Story] Description`

## Phase 1: Setup

- [ ] T001 Baseline: `make test && make lint` pass clean on branch `019-preserve-mime-on-edit` before any change (records the pre-fix green state)

## Phase 2: Foundational (blocking prerequisites for all stories)

- [ ] T002 [P] Failing tests for the MIME vocabulary: generic-set membership (`application/zip`, `application/octet-stream`, `text/plain`, case/parameter variants via `normalizeMIME`) and office extension→canonical-MIME lookups (`.docx/.xlsx/.pptx/.odt/.ods/.odp`) in `internal/domain/model/mime_test.go`
- [ ] T003 Implement MIME vocabulary: `GenericMIMEs` set + `IsGenericMIME()`, `OfficeExtToMIME` map + `OfficeMIMEForName(displayName)` in `internal/domain/model/mime.go` (single source of truth, constitution VIII)
- [ ] T004 [P] Add expvar maps `ReplaceOutcomes` (`content_replace_outcomes_total`) and `MimeRepairOps` (`mime_repair_total`) to `internal/adapter/inbound/http/metrics.go`, initialized in `InitMetrics()`
- [ ] T005 [P] Add sentinel errors `ErrEmptyContent`, `ErrMimeMismatch` (with known/detected fields via a typed error) in `internal/domain/service/file_service.go`; add named `ErrorResponse{code, error, detail}` struct with `Render()` in `internal/adapter/inbound/http/dto.go` (anti-pattern #11 — no `map[string]any`)

**Checkpoint**: vocabulary + plumbing exist; nothing behavioral changed yet.

## Phase 3: User Story 1 — Edited office document stays openable (P1) 🎯 MVP

**Goal**: A Collabora save whose OOXML zip sniffs as `application/zip` keeps the
stored office type; the document reopens forever (FR-001/002/003, SC-001/002).

**Independent test**: `PUT /internal/file/{id}/content` with a reordered-zip .pptx →
200, stored `mimeType` unchanged, `fallback_generic_sniff` counter incremented.

- [ ] T006 [US1] Failing table-driven tests for `reconcileReplaceMIME` accept paths in `internal/domain/service/file_service_test.go`: (a) reordered-OOXML fixture (build a zip with `[Content_Types].xml` not first — helper in test file) sniffs generic → returns known pptx MIME; (b) sniff == known → known; (c) octet-stream/text-plain sniffs over docx/xlsx → known; include a non-office control (PNG over PNG → accepted). Plus two cross-cutting guards: (d) FR-007 mid-write atomicity — mock repo `UpdateFile` fails after a successful `Storage.Save` → stored row (content + type) unchanged, no old-blob cleanup runs; (e) FR-005 invariant property — across every accept outcome of the matrix, the persisted `mimeType` always equals the pre-existing stored type
- [ ] T007 [US1] Implement `reconcileReplaceMIME(knownMIME string, content []byte)` accept branches (generic-fallback, equal) in `internal/domain/service/file_service.go`
- [ ] T008 [US1] Rewire `StoreAndLink` (`internal/domain/service/file_service.go:344`): call `reconcileReplaceMIME(doc.MimeType, content)` **before** `Storage.Save`; persist the reconciled (known) type in `UpdateFile`; never persist the raw sniff. Existing failure-ordering (old row intact, orphan blob GC-able) untouched — FR-007 accept-path
- [ ] T009 [US1] Structured zap logs + `ReplaceOutcomes` counters for `accepted` and `fallback_generic_sniff` (fields: documentID, knownMime, detectedMime, outcome) in `internal/domain/service/file_service.go` (FR-008)
- [ ] T010 [US1] Handler-level test in `internal/adapter/inbound/http/document_handler_test.go`: reordered-pptx PUT → 200, response `mimeType` equals pre-existing stored type (contract: response reports unchanged type)
- [ ] T011 [US1] Checkpoint: `make test && make lint` green; run quickstart step 2 manually against local stack

**Checkpoint**: US1 alone is a shippable MVP — it kills the entire `application/zip`
corruption class (the #9849 cause).

## Phase 4: User Story 2 — New blank document survives its first save (P2)

**Goal**: Empty (0-byte) replacements are rejected with no side effects; blank-create
placeholders can never be relabeled `text/plain` (FR-003a, FR-007, FR-009).

**Independent test**: `PUT …/content` with empty body → 422 `EMPTY_CONTENT`; row and
blob untouched; `rejected_empty` counter incremented.

- [ ] T012 [US2] Failing tests in `internal/domain/service/file_service_test.go`: empty content → `ErrEmptyContent`; assert **zero side effects** (mock `StoragePort` records no `Save`, repo records no `UpdateFile`)
- [ ] T013 [US2] Implement empty-body branch (first check, before sniff) in `reconcileReplaceMIME`; log + count `rejected_empty` in `internal/domain/service/file_service.go`
- [ ] T014 [US2] Map `ErrEmptyContent` → 422 `ErrorResponse{code:"EMPTY_CONTENT"}` in `ReplaceContent` (`internal/adapter/inbound/http/document_handler.go`); handler test asserts status, body code, and that a subsequent GET returns the original content

**Checkpoint**: empty saves now fail loudly (Collabora shows native save-failed —
verified no wopi-service change needed, research R4).

## Phase 5: User Story 3 — Different content rejected, not relabeled (P3)

**Goal**: Unambiguously different concrete content (docx into a pptx slot) is
rejected; stored content and type unchanged (FR-004, FR-005-by-invariant, SC-005).

**Independent test**: `PUT …/content` with a valid .docx body on a .pptx document →
422 `MIME_MISMATCH` with `{knownMime, detectedMime}`; row untouched.

- [ ] T015 [US3] Failing tests in `internal/domain/service/file_service_test.go`: well-formed docx fixture (with `[Content_Types].xml` first, so it sniffs concretely) into pptx-typed doc → `ErrMimeMismatch` carrying known+detected; concrete image mismatch (JPEG into PNG doc) also rejects; zero side effects asserted
- [ ] T016 [US3] Implement concrete-mismatch branch in `reconcileReplaceMIME`; log + count `rejected_mismatch` in `internal/domain/service/file_service.go`; re-run the FR-005 invariant property test (T006e) — it must now also hold across reject outcomes (type unchanged on every path)
- [ ] T017 [US3] Map `ErrMimeMismatch` → 422 `ErrorResponse{code:"MIME_MISMATCH", detail:{knownMime, detectedMime}}` in `internal/adapter/inbound/http/document_handler.go`; handler test asserts status, codes, detail fields

**Checkpoint**: all three replace-path behaviors live; `mimeType` is now immutable
post-creation on every request path.

## Phase 6: Remediation — startup self-repair job (FR-006, SC-003, SC-004)

**Goal**: Idempotent boot-time job relabels content-verified corrupted office rows and
reports zero-byte rows as unrecoverable, in every environment.

- [ ] T018 [P] Add sqlc queries `ListSuspectMimeRows` (mimeType ∈ generic set AND `lower("displayName")` matches office-extension suffix) and `UpdateMimeType` (single-column + `updatedDate`) to `db/queries/document.sql`; run `sqlc generate -f db/sqlc.yaml`
- [ ] T019 Add `ListSuspectMimeRows`/`UpdateMimeType` to `internal/domain/port/document_repo.go` and implement in `internal/adapter/outbound/alkemiodb/document_repo.go` using the generated queries
- [ ] T020 Failing tests in `internal/domain/service/mime_repair_test.go`: (a) suspect row with non-empty `PK`-magic blob → relabeled to `OfficeExtToMIME[ext]`; (b) zero-byte blob → `unrecoverable`, row untouched; (c) non-zip content named `.pptx` → `skipped_not_office`, row untouched; (d) second run touches nothing (idempotency); (e) storage read error → `errors` counted, job continues
- [ ] T021 Implement `RunMimeRepair(ctx)` in `internal/domain/service/mime_repair.go`: list suspects → `StoragePort.Read` → verify non-empty + zip magic → `UpdateMimeType`; per-row structured log (documentID, oldMime, newMime|reason) + `MimeRepairOps` counters (FR-008)
- [ ] T022 Wire repair job in `cmd/server/app.go`: goroutine launched after DB connect, before/independent of HTTP serving; completion summary log (relabeled/unrecoverable/skipped/errors counts)

**Checkpoint**: deploy to a seeded dev DB → corrupted rows fixed on boot, re-boot is a
no-op (quickstart "Verify the repair job").

## Phase 7: Polish & Cross-Cutting

- [ ] T023 [P] Update `openapi.yaml`: 422 responses with `ErrorResponse` schema (codes `EMPTY_CONTENT`, `MIME_MISMATCH`) on `PUT /internal/file/{id}/content`, per `contracts/openapi-delta.md`
- [ ] T024 [P] Coverage gate: ≥95% unit coverage on `internal/domain/service` and touched adapter packages; `make lint` clean (constitution IX, XII)
- [ ] T025 End-to-end acceptance per `quickstart.md` against the local Collabora stack: ≥3 edit/save/close/reopen cycles on a .pptx (SC-001); DB assert `mimeType` stable throughout; empty-save and smuggle curls return the new 422s; `/debug/vars` shows all four replace outcomes and repair counters (SC-002, SC-005, SC-006, FR-008). **Post-deploy follow-through (record in the PR body — completes SC-003/SC-004)**: after the acceptance-environment deploy, verify `mime_repair_total` shows the 6 known rows relabeled (and they reopen in Collabora) + 4 reported unrecoverable; repeat the check after the production deploy

## Dependencies

```
Phase 1 (T001)
  └─→ Phase 2 (T002→T003; T004, T005 parallel)
        └─→ Phase 3 US1 (T006→T007→T008→T009→T010→T011)
              ├─→ Phase 4 US2 (T012→T013→T014)   # extends reconcileReplaceMIME
              │     └─→ Phase 5 US3 (T015→T016→T017)
              └─→ Phase 6 Remediation (T018 ∥ T019-prep → T020 → T021 → T022)
                    # independent of US2/US3; needs only Phase 2 vocabulary + US1's
                    # function file existing to avoid merge conflicts in file_service.go
Phase 7 (T023 ∥ T024 after all code; T025 last)
```

- US2 and US3 both edit `reconcileReplaceMIME` and the handler — sequential by design
  (same files), though each remains independently testable per its phase checkpoint.
- Phase 6 touches disjoint files (mime_repair.go, queries, app.go) → can run in
  parallel with Phases 4–5 if worked by a second agent.

## Parallel execution examples

- **Phase 2**: T004 (metrics.go) ∥ T005 (errors+dto) while T002→T003 (mime.go) proceed.
- **After US1**: one agent runs Phase 4→5 (replace-path files), another runs Phase 6
  (repair-job files) concurrently — no file overlap.
- **Phase 7**: T023 (openapi.yaml) ∥ T024 (coverage/lint) — then T025 alone.

## Implementation strategy

**MVP = Phase 1–3 (US1)**: kills the `application/zip` corruption class — the entire
originally-reported bug — and is independently shippable. Phases 4–5 close the empty
and smuggle holes; Phase 6 heals existing damage automatically on the next deploy;
Phase 7 locks the contract and proves acceptance. Total: 25 tasks
(US1: 6, US2: 3, US3: 3, foundational: 4, remediation: 5, setup: 1, polish: 3).
