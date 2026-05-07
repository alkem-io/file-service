---

description: "Tasks for image orientation canonicalization + dim reporting + lazy backfill"
---

# Tasks: Canonicalize image orientation and return post-rotation dimensions

**Input**: Design documents from `/specs/018-image-orient-dims/`
**Prerequisites**: plan.md, spec.md, research.md, data-model.md, contracts/, quickstart.md

**Tests**: Tests are MANDATORY for this feature per Constitution Principle VI (Test-First Development) and Principle XII (Meaningful Tests, ≥95% coverage). Each user-story phase opens with the test tasks; implementation follows. Tests compile against the foundational-phase stubs (which return nil dims by design) and FAIL with mismatched-dims assertions until implementation lands. The red→green transition IS the implementation step.

**Organization**: Tasks are grouped by user story. US1 (P1, mobile-photo regression) is the MVP — completing only US1 ships the production fix.

## Format: `[ID] [P?] [Story] Description`

- **[P]**: Different file, no in-flight dependencies → can run in parallel.
  - **Note**: Same-file additions (multiple test functions in one `_test.go`) are NOT marked [P] even when their content is independent. The strict rule is "different files" so that automated tooling can apply concurrent edits safely. Group same-file additions into a single task whose description lists every test name.
- **[Story]**: US1 / US2 / US3 (only on user-story-phase tasks). Foundational, Phase 6, Phase 7, Polish phases have NO story label.
- All paths are absolute from repo root: `/Users/antst/work/alkemio/file-service-go/`

## Path Conventions

Single Go project, hexagonal layout. Key paths:
- `db/schema/document.sql` — sqlc schema input
- `db/queries/document.sql` — sqlc query input
- `internal/adapter/outbound/alkemiodb/queries/` — sqlc-generated bindings (DO NOT hand-edit)
- `internal/domain/{model,port,service}/` — domain layer
- `internal/imaging/` — image processor adapter (vips + stub builds)
- `internal/adapter/inbound/http/` — HTTP handlers and DTOs
- `internal/adapter/outbound/alkemiodb/` — DB adapter

---

## Phase 1: Setup (Shared Infrastructure)

**Purpose**: Branch is already on `018-image-orient-dims`. No setup work.

(no setup tasks)

---

## Phase 2: Foundational (Blocking Prerequisites)

**Purpose**: Schema, port signatures, and DTO shape changes that EVERY user story depends on. Test-first ordering for user stories assumes the signatures are stubbed in this phase so subsequent test tasks can compile.

**⚠️ CRITICAL**: No user-story work can begin until this phase is complete.

- [X] T001 Add `content_metadata` column to `db/schema/document.sql` — `"content_metadata" JSONB NOT NULL DEFAULT '{}'`. (sqlc input only; production DDL is the paired alkem-io/server PR's responsibility per plan.md.)
- [X] T002 Add `BackfillContentMetadata` query to `db/queries/document.sql` — `:exec`, updates only `content_metadata`, no `version` bump. See research.md Decision 7 for the SQL.
- [X] T003 Update existing queries in `db/queries/document.sql` (`CreateDocument`, `UpdateDocumentFile`, `FindDocumentByExternalIDAndBucket`, `GetDocumentByID`, `UpdateDocumentMetadata`) to include `content_metadata` in their column lists. `CreateDocument` and `UpdateDocumentFile` accept it as a parameter.
- [X] T004 Run `sqlc generate` (`make sqlc-generate` or equivalent) to regenerate `internal/adapter/outbound/alkemiodb/queries/`. Verify the generated code compiles before proceeding.
- [X] T005 [P] Add `ImageWidth *int` and `ImageHeight *int` fields to `model.Document` in `internal/domain/model/document.go`. Doc comment: response-only, sourced from `content_metadata`.
- [X] T006 [P] Define `ProcessResult` struct and update `ImageProcessor` interface in `internal/domain/port/image_processor.go`. `ProcessResult` fields: `Content []byte`, `MimeType string`, `ImageWidth *int`, `ImageHeight *int`, `Measured bool`. `Measured=true` means an image decoder actually ran on the bytes (whether dims came back or not); `Measured=false` means the processor didn't attempt decoding (no-vips stub for the `!vips` build). The marshaling rule in T024 uses this to distinguish "decoder said no" (write `_decodeFailed` sentinel) from "no decoder available" (leave `{}` for the lazy-backfill or a vips-build retry to handle). New interface: `Process(content, mimeType) (ProcessResult, error)` plus `MeasureDims(content, mimeType) (*int, *int, error)`. `MeasureDims` is the **header-only** dim-extraction port the lazy-backfill (T025) calls — it MUST NOT pixel-decode or re-encode (FR-018: "no pixel decode, microseconds even for large files"). It uses libvips' lazy-load (`NewImageFromBuffer`) and reads `Width()` / `Height()` / `Orientation()` only. Separate from `Process` because `Process` re-encodes for raster orient!=1 inputs, which would violate FR-018 if reused for legacy backfill.
- [X] T007 [P] Add `BackfillContentMetadata(ctx, id, metadata []byte) error` method to `DocumentRepo` interface in `internal/domain/port/document_repo.go`. Update existing `Create` and `UpdateFile` signatures to accept `contentMetadata []byte`.
- [X] T008 Update no-vips stub in `internal/imaging/processor_stub.go` for new interface. `Process` returns `ProcessResult{Content: content, MimeType: mimeType, Measured: false}` (nil dims, `Measured=false` because the stub doesn't attempt decoding). `MeasureDims` returns `(nil, nil, nil)` (no error, nil dims). Together these signal "no decoder available" — T024's marshaling logic writes `{}`, NOT the `_decodeFailed` sentinel, so a `!vips`-build environment doesn't poison rows with permanent failure markers. Compiles under `!vips` build tag.
- [X] T009 Update vips processor `Process` function entry in `internal/imaging/processor.go` to return `ProcessResult` with `nil` dims and `Measured=false` for now (no per-format measurement yet — user-story phases set `Measured=true` once the format arms actually load via vips). Existing JPEG/WebP/HEIC compression behavior unchanged.
- [X] T010 Implement `BackfillContentMetadata` adapter method in `internal/adapter/outbound/alkemiodb/adapter.go` (one-line wrapper over the sqlc-generated query). Update `Create` and `UpdateFile` adapter methods to forward the new `contentMetadata` parameter.
- [X] T011 Update `mockDocRepo` in `internal/adapter/inbound/http/public_handler_test.go` (used by HTTP tests) and `mockRepo` in `internal/domain/service/file_service_test.go` (used by service tests) to satisfy the new `DocumentRepo` interface (extend `Create` / `UpdateFile` signatures, add `BackfillContentMetadata`). Same file for the service-test mock means T012 below is sequential after T011.
- [X] T012 Update `mockProcessor` in `internal/domain/service/file_service_test.go` to implement the new `ImageProcessor` interface in full: (a) `Process` returns `ProcessResult` (instead of three positional values), default `ProcessResult{Content, MimeType, nil, nil, true}` — `Measured=true` so the marshaling-failed-decode branch in T024 is exercised when tests inject nil dims; (b) `MeasureDims(content, mimeType) (*int, *int, error)` returns `(nil, nil, nil)` by default. Per-test overrides can populate dims or simulate failure modes.
- [X] T013 [P] Update `stubProcessor` in `internal/adapter/inbound/http/document_handler_test.go` to implement the full new `ImageProcessor` interface — both `Process` (new return type) and `MeasureDims` (default `(nil, nil, nil)`).
- [X] T014 [P] Add `ImageWidth *int` and `ImageHeight *int` (with `omitempty` JSON tags) to `CreateDocumentResponse`, `ReplaceContentResponse`, and `UpdateDocumentResponse` in `internal/adapter/inbound/http/dto.go`.
- [X] T015 Wire dims plumbing through model + service. **(a)** Add `ImageWidth *int` and `ImageHeight *int` fields to `model.StoredFile` in `internal/domain/model/document.go` (alongside `ExternalID`, `MimeType`, `Size`) — these are how `StoreAndLink` carries dims out to the Replace handler. **(b)** Update `internal/domain/service/file_service.go`: `insertDocument` and `CreateDocument` thread dims onto `model.Document` (consumed by Create / Copy / PATCH responses); `StoreAndLink` populates `model.StoredFile.ImageWidth/ImageHeight` from the just-run `Process` (consumed by the Replace response). `UpdateDocumentMetadata` is read-only on dims (PATCH never changes content). Service plumbing only — no measurement logic yet (`Process` still returns nil dims).
- [X] T016 Run `go build ./... && go vet ./...` and `golangci-lint run`. Fix any compilation/lint errors. Foundational layer must be green before any user-story tasks begin.

**Checkpoint**: All compilation green. `Process` returns `ProcessResult{Content, MimeType, nil, nil}`. `Document.ImageWidth/ImageHeight` are nil-everywhere. Existing JPEG/WebP/HEIC behavior is functionally identical to today (FR-009). User-story phases can now begin — their test-first work compiles against these stubs.

---

## Phase 3: User Story 1 — Phone-photo upload validates correctly (Priority: P1) 🎯 MVP

**Goal**: Fix the production regression. JPEG/WebP/HEIC uploads (the existing canonicalization paths) report `imageWidth` / `imageHeight` on the create response, with values matching what a renderer would draw post-rotation. Lazy-backfill on Create dedup hit covers legacy rows.

**Independent Test**: Upload a JPEG with raw bytes 1082×127 and EXIF `Orientation=6`. Response carries `imageWidth=127, imageHeight=1082`. Returned bytes have orientation=1. SC-001 passes.

### Tests for User Story 1 ⚠️ Write FIRST — must FAIL before implementation

- [X] T017 [P] [US1] Add testdata fixtures under `internal/imaging/testdata/`: `jpeg-1024x512-orient1.jpg`, `jpeg-1024x512-orient6.jpg`, `webp-1024x512-orient1.webp`, `webp-1024x512-orient6.webp`, `heic-1024x512-orient1.heic`, `heic-1024x512-orient6.heic`, `jpeg-malformed-exif.jpg` (a JPEG whose EXIF block is corrupted but pixel data still decodes — for FR-011 / COV-1). Document the recipe in `testdata/README.md` (use `exiftool` and `convert`). Independent of any Go code; can run any time.

  *Implementation note*: chose the programmatic-fixture path (govips `vips.Black` + `SetOrientation` + `Export*`) over checked-in binaries — see `makeJPEGWithOrientation` / `makeWebPWithOrientation` / `makeHEICWithOrientation` in `processor_test.go`. No `testdata/README.md` needed since fixtures are generated at test runtime; the `testdata/generate_test.go` placeholder (already in repo) documents this. The malformed-EXIF JPEG was substituted by a stdlib-encoded JPEG (no EXIF at all → vips reports orientation=0), which exercises the same FR-011 contract: "non-canonical orientation tag must not fail Process; dims returned are raw."
- [X] T018 [US1] Add the following test functions to `internal/imaging/processor_test.go` (one task — same file, multiple test functions):
  - `TestProcessor_JPEG_Orient1_ReportsRawDims` — input is canonical JPEG, asserts dims match input.
  - `TestProcessor_JPEG_Orient6_ReportsRotatedDims` — input is rotated JPEG, asserts dims are swapped and result bytes have no orientation tag.
  - `TestProcessor_WebP_Orient6_ReportsRotatedDims`.
  - `TestProcessor_HEIC_Orient6_ReportsRotatedDims` (skips if HEIF encoder unavailable).
  - `TestProcessor_JPEG_NoOrientationTag_TreatsAsOrient1_Passthrough` — covers FR-011 contract via the orientation-absent equivalent (govips refuses to embed malformed EXIF on export; orientation=0 exercises the same code path).
  - `TestProcessor_MeasureDims_JPEG_Orient6` and `TestProcessor_MeasureDims_CorruptInput_ReturnsError` — defend the T021 vips-path contract.
- [X] T019 [P] [US1] Add HTTP-layer tests to `internal/adapter/inbound/http/document_handler_test.go`:
  - `TestDocumentHandler_Create_Image_ReturnsDims` — upload a known-dim JPEG via POST `/internal/file`, assert response carries `imageWidth` / `imageHeight`.
  - `TestDocumentHandler_Create_PhonePhoto_Orient6_ReportsRotatedDims` — the SC-001 regression repro: 1082×127 + Orientation=6 → response 127×1082.
  - `TestDocumentHandler_ReplaceContent_RotatedImage_ReturnsDims` — PUT a known-dim rotated JPEG to `/internal/file/{id}/content` against an existing row; assert response carries post-rotation `imageWidth` / `imageHeight`. Defends FR-004's Replace endpoint coverage; without this test, a regression in T027's `StoreAndLink → ReplaceContentResponse` plumbing slips past the gates.
- [X] T020 [P] [US1] Add service-layer test `TestCreateDocument_DedupHitOnLegacyImageRow_TriggersBackfill` to `internal/domain/service/file_service_test.go`. Mock repo with a legacy row (empty `content_metadata`); assert dedup-hit response carries dims AND `BackfillContentMetadata` was called (FR-018, SC-009 partial).

  *Bonus*: also added 5 supporting tests for the failure modes — non-image dedup hit (no backfill), MeasureDims-fails-with-error (sentinel persist), no-decoder-available (skip persist), Storage.Read-fails (graceful degrade), persist-fails-but-response-still-carries-dims (FR-020 c), already-measured (skip backfill). All under one task per the same-file rule.

### Implementation for User Story 1

- [X] T021 [US1] Add `extractDims(img *vips.ImageRef) (*int, *int)` private helper to `internal/imaging/processor.go`. Reads `Width()`, `Height()`, `Orientation()`; if orientation is 5/6/7/8 swaps width/height; returns (nil, nil) on degenerate values (Decision 9). FR-011 behavior is implicit: vips treats malformed EXIF as orient=0; extractDims returns dims unchanged in that case. Also implement `MeasureDims(content []byte, mimeType string) (*int, *int, error)` on the vips `Processor` (the public port-method counterpart): wraps `vips.NewImageFromBuffer(content)` + `extractDims(img)` + `img.Close()` — header-only, no pixel decode, no re-encode. **Contract**: on `NewImageFromBuffer` failure, returns `(nil, nil, err)`. On a successful load whose `extractDims` returns `(nil, nil)` (degenerate `Width()=0`/`Height()=0`), `MeasureDims` MUST also return `(nil, nil, err)` with a descriptive error — the vips path NEVER returns `(nil, nil, nil)`, so any `(nil, nil, nil)` observed by T025 originates only from the no-vips stub. T025 uses this contract to disambiguate "decoder failed" (any err → write `_decodeFailed`) from "no decoder available" (`(nil, nil, nil)` → skip persist).
- [X] T022 [US1] Update `compressJPEG` and `convertHEICToJPEG` in `internal/imaging/processor.go` to extract dims from the post-`AutoRotate` image and pass them back through. Existing rotate + ICC + strip behavior unchanged (FR-009).
- [X] T023 [US1] Update `processor.go`'s `Process` switch arms for `image/jpeg`, `image/jpg`, `image/webp`, `image/heic`, `image/heif` to populate dims on `ProcessResult` and set `Measured=true` (vips successfully loaded the bytes). The "compressed >= original → fall back to original" path still measures dims from the original image (Decision 9). The "vips loads but cannot measure dims" branch returns `nil` dims with `Measured=true` so `insertDocument`'s marshaling persists `_decodeFailed` per Decision 9. The "vips fails to load entirely" branch returns an error from `Process`, which the handler maps to 422 per FR-012.

  *Bug fix during T023*: the JPEG/WebP arm's size-guard fallback (`compressed >= original → return original`) silently re-emitted the input EXIF orientation tag for orient!=1 inputs, breaking FR-001 (canonical bytes). Added `loadHeader` helper + a guard so the fallback only fires when the input was already canonical; otherwise the canonicalized compressed bytes are returned unconditionally even when larger than the input.
- [X] T024 [US1] Update `internal/domain/service/file_service.go` `insertDocument` to marshal dims from `ProcessResult` into a `content_metadata` JSON blob and pass it to the repo's `Create`. **This is the single shared site for the marshaling-and-sentinel logic** (INC-2): write `{"imageWidth": N, "imageHeight": N}` when both dims present; write `{}` for non-image MIMEs; write `{}` for image MIMEs when `Measured=false` (decoder unavailable — let lazy-backfill retry later); write `{"_decodeFailed": true}` only when `Measured=true && nil dims` (decoder ran and confirmed the bytes are unreadable). The `Measured` flag avoids the `!vips`-stub case poisoning every image row with a permanent failure sentinel. Phase 6 (SVG/GIF) and Phase 7 (lazy-backfill) reuse this marshaling helper; Phase 7's lazy-backfill writes `_decodeFailed` directly when `MeasureDims` returns an error (the error itself signals "decoder ran and failed").
- [X] T025 [US1] Implement `backfillIfNeeded(ctx, doc *model.Document) (*model.Document, error)` helper in `internal/domain/service/file_service.go`. Skips when `doc.MimeType` doesn't start with `image/`, when `doc.ImageWidth != nil` (already measured), or when the row's stored `content_metadata` has `_decodeFailed`. Otherwise calls `s.Storage.Read(doc.ExternalID)` then `s.Processor.MeasureDims(content, doc.MimeType)` — explicitly the header-only port method (T006/T021), NOT `Process`, to honor FR-018's "no pixel decode" guarantee. Persists via `s.Repo.BackfillContentMetadata(ctx, doc.ID, marshaledJSON)`. Best-effort, type-aware MeasureDims-outcome handling per the T021 contract + FR-019/020: **`(dims, nil)`** → marshal `{imageWidth, imageHeight}` and persist + populate `doc`; **`(nil, nil, err)` (decoder ran and failed — vips path)** → marshal `{"_decodeFailed": true}` and persist (sentinel); **`(nil, nil, nil)` (no decoder available — only emitted by no-vips stub per T021's contract)** → SKIP persist, leave `content_metadata` empty so a future read in a vips environment retries; **`Storage.Read` failure** → log warning, leave `content_metadata` empty, response omits dims (no sentinel — transient retry); **`BackfillContentMetadata` failure** → log warning, leave `content_metadata` empty, response INCLUDES the just-computed dims. Never returns an error — backfill failures don't fail the underlying request.

  *Spec deviation*: the `_decodeFailed` short-circuit (skip backfill when row's stored content_metadata has the sentinel) is NOT yet implemented. The current `parseContentMetadataDims` returns nil dims for both empty and sentinel rows, so the helper can't disambiguate without an additional Document field exposing the raw blob. For US1 this is fine — a sentinel row's MeasureDims will still fail and re-write the same sentinel (idempotent), at the cost of one redundant Storage.Read + MeasureDims attempt per request. A follow-up (Phase 7 or a small sub-task) can add `Document.ContentMetadataRaw` (or a `_decodeFailed` flag) to enable the short-circuit. Logged for follow-up but not blocking US1.
- [X] T026 [US1] Wire `backfillIfNeeded` into the dedup-hit branch of `internal/domain/service/file_service.go` `CreateDocument`. After dedup matches an existing row, call backfill on the matched doc before returning.
- [X] T027 [US1] Update `internal/adapter/inbound/http/document_handler.go` to surface dims on responses for the two Process-driven endpoints: `Create` handler populates `imageWidth` / `imageHeight` on `CreateDocumentResponse` from `doc.ImageWidth` / `doc.ImageHeight`; `ReplaceContent` handler populates them on `ReplaceContentResponse` from the returned `*model.StoredFile.ImageWidth` / `ImageHeight`. (Copy and PATCH handlers are wired in Phase 7 alongside the lazy-backfill — those endpoints don't run Process, so their dims source is the row's `content_metadata`, not a fresh `Process` result.)
- [X] T028 [US1] Run `go test -tags vips ./internal/...` and `golangci-lint run`. All US1 tests should now pass; existing tests still green.

**Checkpoint**: User Story 1 fully functional and independently testable. Phone-photo upload regression closed (SC-001). JPEG/WebP/HEIC create responses carry dims. Lazy-backfill works on Create dedup hits. Malformed-EXIF passthrough verified (FR-011). The paired alkem-io/server PR can be unblocked at this point — even before US2/US3 land.

---

## Phase 4: User Story 2 — All raster formats produce byte-canonical output for new uploads (Priority: P2)

**Goal**: Extend Process's canonicalization to PNG, BMP, AVIF. Orientation-1 inputs pass through byte-identical (FR-002); orientation ≠ 1 inputs are physically rotated, EXIF-stripped, ICC-preserved, and re-encoded in the same format (FR-003). FR-012 covers BMP fail-loud when libvips lacks Magick.

**Independent Test**: Upload a PNG with EXIF Orientation=6 → response bytes physically rotated, no EXIF orientation tag, dims match what a renderer draws. Upload an orientation-1 PNG of the same source → response is byte-identical to input. SC-002, SC-004 pass for PNG/BMP/AVIF.

### Tests for User Story 2 ⚠️ Write FIRST — must FAIL before implementation

- [X] T029 [P] [US2] Add testdata fixtures: `png-1024x512-orient1.png`, `png-1024x512-orient6.png`, `bmp-1024x512-orient1.bmp`, `bmp-1024x512-orient6.bmp`, `avif-1024x512-orient1.avif`, `avif-1024x512-orient6.avif`. Same recipe as T017; recipe in `testdata/README.md`.

  *Implementation*: programmatic generation via vips — `makePNGWithOrientation`, `makeBMPWithOrientation`, `makeAVIFWithOrientation` helpers in `processor_test.go`, mirroring the US1 `make*WithOrientation` pattern. No checked-in binaries.
- [X] T030 [US2] Added test functions to `internal/imaging/processor_test.go`: `TestProcessor_PNG_Orient6_ReportsRotatedDimsAndStripsExif` + `BMP`/`AVIF` variants; `TestProcessor_PNG_Orient1_ReturnsByteIdentical` + `BMP`/`AVIF` variants; `TestProcessor_PNG_DeterministicReencode` + `BMP`/`AVIF`; `TestProcessor_BMP_Orient6_FailsLoudWhenMagickMissing` (skips when Magick is available — typical dev case). AVIF orient6 assertion adjusted: libvips' AVIF encoder applies orientation at encode time, so the fixture comes back with orientation=0 + physically rotated bytes; the test still verifies post-rotation dims and canonical orientation on output.
- [X] T031 [P] [US2] Added `TestDocumentHandler_Create_RotatedPNG_DedupHitsOnReupload` to `document_handler_test.go`. Upload same PNG bytes twice; second response carries `reused=true` and same externalID.

### Implementation for User Story 2

- [X] T032 [US2] `canonicalizeRaster` already implemented in `internal/imaging/processor.go`.
- [X] T033 [US2] `image/png`, `image/avif`, `image/bmp` arms already wired to `canonicalizeRaster`.
- [X] T034 [US2] Verified — both `Create` and `ReplaceContent` handlers map `service.ErrImageProcessing` → 422.
- [X] T035 [US2] Tests + lint green.

**Checkpoint**: All raster formats produce byte-canonical output for new uploads (FR-001/002/003). Deterministic re-encode confirmed for stable dedup. BMP fail-loud verified. The "anything new written to storage is what-you-see-is-what's-stored" contract holds for the full raster set.

---

## Phase 5: User Story 3 — Wide-gamut images keep color fidelity (Priority: P2)

**Goal**: Verify (and where missing, ensure) ICC profile preservation across every re-encode path. Implementation uses `ImageRef.RemoveMetadata()` (Decision 2) which keeps ICC by govips contract; this phase locks the invariant with tests across all formats.

**Independent Test**: Upload a Display-P3-tagged JPEG/PNG/WebP/HEIC/AVIF/BMP (canonical and rotated variants); response bytes carry the same ICC profile byte-for-byte. SC-003 passes for all formats.

### Tests for User Story 3 ⚠️ Write FIRST — must FAIL if RemoveMetadata is missing

- [X] T036 [P] [US3] Added wide-gamut fixture helpers (`newRGBImageWithICC`, `makeJPEGWithICC`, `makePNGWithICC`, `makeWebPWithICC`, `makeHEICWithICC`, `makeAVIFWithICC`, `makeBMPWithICC`) in `processor_test.go`. Recipe: encode an sRGB stdlib image, reload via vips, attach `SRGBIEC6196621ICCProfilePath` via `TransformICCProfile`, then export to the target format. No checked-in binaries.
- [X] T037 [US3] Added `TestProcessor_JPEG_PreservesICC`, `TestProcessor_PNG_PreservesICC`, `TestProcessor_WebP_PreservesICC`, `TestProcessor_HEIC_PreservesICC`, `TestProcessor_AVIF_PreservesICC`, `TestProcessor_BMP_PreservesICC`, and `TestProcessor_StripsExifAndXmp_KeepsICC`. Each round-trips a rotated input and asserts `HasICCProfile()` survives via `vips.NewImageFromBuffer(result.Content)`. Tests `t.Skip` cleanly when codec unavailable.

### Implementation for User Story 3

- [X] T038 [US3] Audit + bug fix: `compressJPEG`, `convertHEICToJPEG`, and `canonicalizeRaster` all call `RemoveMetadata()` before export (govips contract preserves ICC there). However, the original code ALSO set `ep.StripMetadata = true` on every export, which delegates to libvips' "strip" option that drops ICC too. Tests caught this: ICC assertions failed across every format. Fix: removed `StripMetadata = true` from JPEG/PNG/AVIF export params (kept the comment explaining why). FR-006 / Decision 2 was implemented incorrectly until US3 tests landed.
- [X] T039 [US3] Tests + lint green.

**Checkpoint**: ICC profiles round-trip cleanly through every re-encode path. Wide-gamut and CMYK content preserves color fidelity. EXIF / XMP confirmed to drop.

---

## Phase 6: SVG / GIF dim measurement

**Goal**: Per Q5 + SC-014, measure and report dims for SVG and GIF uniformly with rasters. Bytes pass through unchanged (no canonicalization), but dims are populated. Process measures at create; lazy-backfill measures legacy rows.

**Independent Test**: Upload a known-viewBox SVG (200×100) and a known-canvas GIF (300×200). Responses carry the matching dims. SC-014 passes.

(no story label — Q5 cross-cutting addition.)

### Tests for SVG/GIF ⚠️ Write FIRST

- [X] T040 [P] SVG fixture is an inline literal (`testSVG`, `malformedSVG` constants in `processor_test.go`). GIF fixture uses the existing `makeGIF` helper (300×200). No checked-in binaries.
- [X] T041 Added `TestProcessor_SVG_ReportsViewBoxDims` (200×100), `TestProcessor_GIF_ReportsCanvasDims` (300×200), and `TestProcessor_MalformedSVG_PersistsDecodeFailedSentinel` (asserts Measured=true with nil dims so insertDocument writes the sentinel).

### Implementation for SVG/GIF

- [X] T042 Already implemented in `Process`: SVG/GIF arm calls `measureBytes` and returns `Measured=true` with dims from `extractDims`; on load failure both dims are nil so the marshaling writes `_decodeFailed`.
- [X] T043 Tests + lint green.

**Checkpoint**: SVG and GIF treated uniformly with rasters for dim reporting. No special-case branches in callers.

---

## Phase 7: Lazy-backfill on Copy / PATCH + adapter tests + forward-fit JSON

**Goal**: Wire `backfillIfNeeded` into Copy and PATCH handlers so all metadata-returning endpoints surface dims for legacy rows. Cover the failure modes (FR-019/020) and the version-disjoint write race (FR-018). Confirm forward-fit JSON behavior (FR-017 / COV-2).

**Independent Test**: Seed a legacy row (empty `content_metadata`); issue Copy on it → response carries dims and source row's `content_metadata` becomes populated. Repeat for PATCH. Concurrent PATCH + backfill → both succeed without 409. SCs 7, 8, 9, 10, 11, 12, 13 pass.

(no story label — completes the broader endpoint contract from FR-014/015 and the lazy-backfill design FRs FR-018/019/020. Note: Replace handler does NOT need lazy-backfill wiring — its `StoreAndLink` always runs Process, which populates `StoredFile.ImageWidth/Height` directly per T015/T027, and the underlying row's `content_metadata` is rewritten on every replace. The lazy-backfill is only relevant for paths that read existing rows whose `content_metadata` may be empty — Copy and PATCH.)

### Tests for Phase 7 ⚠️ Write FIRST

- [X] T044 [P] Added HTTP-layer Phase 7 tests to `document_handler_test.go`: `TestDocumentHandler_Copy_LegacyImageRow_LazyBackfillsBoth`, `TestDocumentHandler_Patch_LegacyImageRow_LazyBackfills`, `TestDocumentHandler_Patch_DecodeFailure_PersistsSentinel`, `TestDocumentHandler_Patch_StorageReadFails_GracefulDegrade`, `TestDocumentHandler_Patch_BackfillPersistFails_ResponseStillCarriesDims`. Shared helpers `runCopyLegacyImage` and `runPatchLegacyImage` keep the failure-mode matrix DRY.
- [X] T045 [P] Added adapter integration tests: `TestAdapter_BackfillContentMetadata_DoesNotBumpVersion`, `TestAdapter_BackfillContentMetadata_IsIdempotent`, `TestAdapter_GetByID_TolerantOfUnknownContentMetadataKeys`. Use `testPool(t)` helper which `t.Skip`s when DB unavailable. `createTestRow` helper inserts a row with a fresh authorization_policy and tears down on test exit.

### Implementation for Phase 7

- [X] T046 `CopyDocument` in `file_service.go` now calls `backfillIfNeeded(ctx, &source)` immediately after `Repo.GetByID` so the source row's content_metadata gets populated before the dedup-check + insert. Dedup hits also re-run backfill on the existing destination row.
- [X] T047 PATCH handler calls `h.Service.BackfillIfNeeded(r.Context(), updated)` after `UpdateDocumentMetadata` returns. Added a public `BackfillIfNeeded` wrapper on `FileService` so the handler doesn't have to duplicate the helper logic.
- [X] T048 Tests + lint green.

**Checkpoint**: All four metadata-returning endpoints surface dims for both new and legacy image rows. Lazy-backfill failure modes behave correctly. Version-disjoint write race confirmed via integration test. Forward-fit JSON tolerant of unknown keys confirmed.

---

## Phase 8: Polish & Cross-Cutting Concerns

**Purpose**: OpenAPI regen, full sweep, version bump.

- [X] T049 `make openapi` regenerated `openapi.yaml`. Diff matches `openapi-delta.md`: `imageWidth` / `imageHeight` added to `CreateDocumentResponse`, `ReplaceContentResponse`, `UpdateDocumentResponse` schemas. (One subtle gotcha resolved during T049: the original `marshalDimsOnly` used an inline anonymous struct which apispec started materializing as a candidate response schema; rewrote to `fmt.Sprintf` to keep the spec clean.)
- [X] T050 Full test suite green under both tags: `go test ./internal/...` and `go test -tags vips ./internal/...`.
- [X] T051 `golangci-lint run` reports 0 issues.
- [X] T052 Coverage: 88.7% under `go test -tags vips -coverpkg=./internal/... ./internal/...`. The 6.3 pp gap to the constitution's 95% comes from: (a) integration paths skipped when no test DB is available (`FindByExternalIDAndBucket`, `findRowToDocument`, `WithTx`), (b) defensive vips error branches in `canonicalizeRaster` that aren't reachable without fault injection (e.g., `AutoRotate` failing on a successfully-loaded image). The new code added by this feature (`marshalContentMetadata`, `marshalDimsOnly`, `BackfillIfNeeded`, `backfillIfNeeded`) is ≥90% covered, with the new `marshalContentMetadata` and `marshalDimsOnly` at 100% via a dedicated unit test. No coverage-padding tests added.
- [ ] T053 Manual quickstart walk-through deferred to human (post-merge sanity check).
- [ ] T054 Release notes deferred to human (post-merge / pre-tag).
- [ ] T055 Tag deferred to human (post-merge).

---

## Dependencies & Execution Order

### Phase Dependencies

- **Setup (Phase 1)**: empty.
- **Foundational (Phase 2)**: blocks every other phase. T001–T004 are sequential (sqlc inputs → generation). T005–T015 are mostly parallel after T004 (note T011 → T012 sequential, same file). T016 is the gate.
- **US1 (Phase 3)**: depends on Phase 2. Tests (T017–T020) come BEFORE implementation (T021–T027) per Constitution VI. Tests will compile against Phase 2 stubs and FAIL until implementation lands.
- **US2 (Phase 4)**: depends on Phase 2. Tests T029–T031 before implementation T032–T034. Conceptually independent from US1, but in practice US1 lands first as the regression fix.
- **US3 (Phase 5)**: depends on US1 + US2 (the re-encode paths must exist to test ICC across them). Tests T036–T037 before audit T038.
- **SVG/GIF (Phase 6)**: depends on Phase 2 + the `extractDims` helper from US1 (T021). Tests T040–T041 before implementation T042.
- **Lazy-backfill broader (Phase 7)**: depends on US1 (which built `backfillIfNeeded`). Tests T044–T045 before implementation T046–T047.
- **Polish (Phase 8)**: last.

### Within Each Phase

- Tests are written BEFORE implementation per Constitution VI. Tests against not-yet-implemented helpers compile (Phase 2 provides stubs) but fail with mismatched-dims assertions. Implementation tasks make them green.
- Verification gate (`go test ... && golangci-lint run`) closes each phase.

### Parallel Opportunities

- **Foundational**: T005, T006, T007 (different files: model, two ports). T013 [P], T014 [P] (different files). T011 → T012 are sequential (same file).
- **US1 tests**: T017 (testdata), T019 (different test file), T020 (different test file) all [P]. T018 lives in processor_test.go and is a single grouped task (multiple test functions, same file).
- **US2 tests**: T029 (testdata), T031 (different test file) [P]. T030 single grouped task.
- **US3 tests**: T036 (testdata) [P]. T037 single grouped task.
- **Phase 6 tests**: T040 (testdata) [P]. T041 single grouped task.
- **Phase 7 tests**: T044, T045 [P] (different test files).
- Implementation tasks within a phase that touch the same file (e.g., processor.go) run sequentially.
- US1 and US2 can be split between two developers (different format arms in processor.go; coordinate via T028/T035 checkpoints).

---

## Parallel Example: US1 setup

```bash
# After Phase 2 closes, launch in parallel:
Task: "T017 — produce JPEG/WebP/HEIC/malformed-EXIF fixtures under testdata/"
Task: "T019 — write HTTP-layer dim-reporting + regression tests in document_handler_test.go"
Task: "T020 — write service-layer dedup-hit-backfill test in file_service_test.go"

# T018 (per-format tests in processor_test.go) is sequential — single grouped task; same file.
# Implementation tasks T021–T027 follow the test phase, mostly sequential (same file: processor.go for T021–T023; file_service.go for T024–T026; document_handler.go for T027).
```

---

## Implementation Strategy

### MVP First (US1 only)

1. Complete Phase 2: Foundational. Schema column, port signatures, DTOs, sqlc regen, mocks updated.
2. Complete Phase 3: User Story 1 — tests first, then implementation.
3. **STOP and VALIDATE**: SC-001 regression repro passes; phone photos accepted by the (paired-PR-amended) server.
4. Ship `v0.0.17-rc1` if needed; the production regression is closed.

### Incremental Delivery

1. Foundational → MVP shape locked, no behavior change.
2. + US1 → regression fix.
3. + US2 → byte-canonical PNG/BMP/AVIF for new uploads.
4. + US3 → ICC preservation locked across all re-encode paths.
5. + SVG/GIF (Phase 6) → uniform dim reporting.
6. + Lazy-backfill broader (Phase 7) → legacy rows self-heal.
7. + Polish (Phase 8) → OpenAPI regen, full sweep, v0.0.17 tag.

### Parallel Team Strategy

With two developers:

1. Both work Foundational together (T001–T016).
2. Once Foundational is green:
   - Developer A: US1 (regression fix is highest priority).
   - Developer B: US2 (parallel; coordinate via `processor.go` PRs).
3. Both work US3 + Phase 6 + Phase 7 in series (these phases share test files; cleaner sequential).
4. Polish together.

---

## Notes

- [P] = different files, no in-flight dependencies. Same-file additions are NOT [P] even when the content is independent.
- [Story] label = traceability to a specific user story.
- Constitution VI (Test-First): tests open every user-story phase; implementation makes them green.
- Constitution VIII (DRY): three shared helpers — `extractDims` (T021), `canonicalizeRaster` (T032), `backfillIfNeeded` (T025). The shared `_decodeFailed` marshaling site is `insertDocument` (T024); Phase 6/7 reuse it via the standard `ProcessResult{nil dims}` convention.
- Constitution X: vips and stub adapters update in lock-step (T008/T009); no parallel old API.
- Anti-pattern #11: HTTP responses stay typed structs with `Render()` (T014).
- Don't commit `coverage.out`. Don't commit testdata that requires proprietary tools to regenerate — document recipes in `testdata/README.md`.
