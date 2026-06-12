# Tasks: Stream Uploads to Permanent Storage

**Input**: Design documents from `/specs/020-stream-uploads/`
**Prerequisites**: plan.md, spec.md, research.md, data-model.md, contracts/ingest-contracts.md, quickstart.md

**Tests**: Included — constitution VI (Test-First) is non-optional; every behavior
task is preceded by failing tests. 95% coverage on touched packages.

**Branch**: stacked on `019-preserve-mime-on-edit` (PR #29). After #29 merges:
`git rebase --onto origin/develop 019-preserve-mime-on-edit 020-stream-uploads`.

## Format: `[ID] [P?] [Story] Description`

## Phase 1: Setup

- [X] T001 Baseline: `make test && make lint` green on the stacked branch before any change (verifies the 019 base in this worktree)
- [X] T002 Pin the govips fork: add `replace github.com/davidbyttow/govips/v2 => github.com/antst/govips/v2 <pseudo-version of 10498ea>` to `go.mod`, run `go mod tidy`, verify `go build ./...` and `go build -tags vips ./...` both compile (research R8)
- [X] T003 Add config knobs to `internal/config/config.go` (+ tests in `internal/config/config_test.go`): `MAX_UPLOAD_SIZE` (default 33554432), `UPLOAD_IDLE_TIMEOUT_MS` (30000), `IMAGE_PIXEL_BUDGET` (100000000), `VIPS_STREAM_DISC_THRESHOLD` (0 = library default), `VIPS_SCRATCH_DIR` (empty = os.TempDir), `VIPS_PIPE_READ_LIMIT` (0 = library default) — validation per existing getenvInt conventions

## Phase 2: Foundational (blocking prerequisites for all stories)

- [X] T004 [P] Failing tests for the streaming hasher in `internal/domain/service/hash_test.go`: `NewHasher()` digest equals `ComputeHash` for empty/small/multi-MiB inputs and across arbitrary write splits
- [X] T005 Implement `NewHasher()` (streaming sha3-256) in `internal/domain/service/hash.go`; `ComputeHash` delegates to it (single source of truth)
- [X] T006 [P] Failing contract tests for the FS stage in `internal/adapter/outbound/storage/local/adapter_test.go`: write→`Commit` publishes blob with correct hash/size/`Created=true`; dedup hit → `Created=false`, staging artifact gone; `Abort` removes staging; `Abort` after failed `Commit` is safe/idempotent; nothing observable in the blob directory before `Commit` (FR-006)
- [X] T007 Extend `internal/domain/port/storage.go` with `OpenStage(ctx) (StageWriter, error)` + `StageWriter{io.Writer; Commit() (model.StoredFile, error); Abort() error}`; implement in `internal/adapter/outbound/storage/local/adapter.go` (temp file in blob dir + internal hash tee + stat-dedup + rename); reimplement `Save([]byte)` as OpenStage+write+Commit (research R3, constitution X)
- [X] T008 Extend `internal/domain/port/image_processor.go` with `TranscodeStream(r io.Reader, w io.Writer, mimeType string) (TranscodeResult, error)` + `TranscodeResult{MimeType string; ImageWidth, ImageHeight *int}`; stub implementation (pass-through `io.Copy`, `Measured=false` semantics) in `internal/imaging/processor_stub.go`
- [X] T009 [P] Add `IngestOutcomes` expvar map (`ingest_outcomes_total`: accepted, rejected_over_limit, rejected_pixel_budget, rejected_bucket_policy, stalled, client_abort, failed_mid_stream) to `internal/adapter/inbound/http/metrics.go`, initialized in `InitMetrics()` (data-model vocabulary; 019's replace map keeps its keys)

**Checkpoint**: ports, hasher, stage, knobs exist; no behavior changed yet.

## Phase 3: User Story 1 — Large non-image upload streams with constant memory (P1) 🎯 MVP

**Goal**: create-path ingestion streams request → stage → commit with a fixed
budget; incremental cap; atomic abort paths (FR-001/002/005/006/008/009).

**Independent test**: upload a file ≫ budget (quickstart) — succeeds
byte-identical with unchanged hash/dedup; over-limit and aborted uploads
leave no artifact; RSS stays flat.

- [X] T010 [US1] Failing pipeline tests in `internal/domain/service/ingest_test.go`: (a) multi-MiB pass-through stream → stored bytes/hash/size/dedup outcome identical to the old `Save` path (mock stage records writes); (b) over-limit mid-stream → `ErrOverLimit`, `Abort` called, no `Commit`; (c) reader error mid-stream (client abort) → `Abort`, no `Commit`; (d) trailing bucket-policy violation (allowedMimeTypes/maxFileSize known only after the stream — research R4) → `Abort` + rejection; (e) dedup-at-end: stage `Commit` returns `Created=false` → existing-row reuse identical to today; (f) allocation bound: pass-through of 64 MiB synthetic stream allocates O(budget), not O(size); (g) SC-005 concurrency: 8 parallel synthetic ingests under one aggregate allocation bound of 8 × budget (+ slack), proving budgets don't multiply with size; (h) FR-008 logging: each terminal outcome (accepted, over-limit, client abort, mid-stream failure) emits exactly one structured log with outcome, reason, and byte count — client aborts distinguishable from service-side failures by field, not prose
- [X] T011 [US1] Implement `internal/domain/service/ingest.go` (3072 B prefix peek → `resolveMIME` → non-image route: fixed 256 KiB copy into stage → commit-after-validation; every error path defers `Abort`; per-outcome structured zap logs with outcome/reason/bytes fields satisfying T010(h) — FR-008) and migrate `CreateDocument` in `internal/domain/service/file_service.go` to `io.Reader` transport
- [X] T012 [US1] Rework `Create` handler in `internal/adapter/inbound/http/document_handler.go` to `r.MultipartReader()`: sequential part loop (file part streamed through the pipeline under the global cap; metadata fields collected per the contract's file-part-first order; trailing validation per R4); 413 on mid-stream cap; update create tests in `internal/adapter/inbound/http/document_handler_test.go` (multipart fixtures unchanged in shape)
- [X] T013 [US1] Implement `internal/adapter/inbound/http/progress_reader.go`: wraps the request body, enforces the byte cap incrementally, and extends the connection read deadline via `http.NewResponseController(w).SetReadDeadline` on each successful read (`UPLOAD_IDLE_TIMEOUT_MS`); stall surfaces as the `stalled` outcome with a structured log distinguishing stall from client abort (FR-008). Tests in `internal/adapter/inbound/http/progress_reader_test.go` MUST cover **both transports the server runs**: HTTP/1 (httptest) **and h2c** (`golang.org/x/net/http2` client against an h2c handler — `cmd/server/app.go:48` proves h2c is a production mode); if `SetReadDeadline` is unsupported on the h2c path (`ErrNotSupported`), implement the documented fallback — a per-read watchdog timer that cancels the request context on idle expiry, identical outcome semantics — and record the chosen mechanism in `specs/020-stream-uploads/research.md` R6
- [X] T014 [US1] Rework timeouts in `cmd/server/app.go`: `ReadHeaderTimeout: 10s` replaces global `ReadTimeout`; add fixed per-request read deadline middleware for non-upload routes in `internal/adapter/inbound/http/router.go` (preserves today's effective 30 s); wire vips knobs (`SetStreamDiscThreshold`/`SetStreamScratchDir`/`SetPipeReadLimit`) from config behind the vips build tag; update `cmd/server/app_test.go`
- [X] T015 [US1] Checkpoint: `make test && make lint` green; quickstart "memory budget" run against a local big file — record RSS observation in the PR notes

**Checkpoint**: US1 alone is shippable — pass-through uploads (most bytes) stream.

## Phase 4: User Story 2 — Transcodable image streams through the converter (P2)

**Goal**: HEIC/WebP/rotated-JPEG uploads transcode reader→writer with dims
recorded, pixel budget enforced, baseline-JPEG output (FR-004, FR-010, R5/R7).

**Independent test**: HEIC upload → stored bytes == buffered
`ExportJpeg(Q82, Interlace:false)`; pixel bomb → 422 before decode; rotated
JPEG dims swapped.

- [X] T016 [US2] Failing transcode tests (vips build tag) in `internal/imaging/transcode_stream_test.go`: (a) HEIC fixture → byte-identity vs buffered export with baseline params; (b) WebP → JPEG identity; (c) rotated JPEG (EXIF 6) → materialized path, dims swapped, orientation cleared; (d) pixel bomb (huge header dims, tiny body) → budget error before any pixel decode; (e) truncated stream → wrapped reader error; (f) canonical JPEG (orientation ≤ 1) IS recompressed to Q82 baseline on the sequential path — no size guard, output may exceed input (clarified 2026-06-12); (g) GIF/SVG are NOT routed to transcode (passthrough, no decode)
- [X] T017 [US2] Implement `TranscodeStream` in `internal/imaging/processor.go` (vips tag): `LoadImageFromReader(AccessSequential)` → header dims + orientation → pixel-budget check (orientation-aware swap) → orientation ≥ 3 ⇒ materialize (fork disc threshold) → `AutoRotate` → `SaveToWriter` with per-format params — JPEG `Interlace: false` Quality 82, HEIC→JPEG conversion params mirroring `convertHEICToJPEG` (research R5/R7); routing table: transcode for HEIC/HEIF/WebP/BMP/AVIF/PNG and ALL JPEGs (canonical JPEGs recompress on the sequential path — size guard dropped per clarification; rotated ones materialize); passthrough only for GIF/SVG and non-images
- [X] T018 [US2] Route images in `internal/domain/service/ingest.go` through `Processor.TranscodeStream` into the stage; persist dims via existing content_metadata marshaling; map the budget error to 422 `RejectedContentResponse{code:"PIXEL_BUDGET_EXCEEDED"}` in `internal/adapter/inbound/http/document_handler.go` + count `rejected_pixel_budget`; handler test for the 422 in `internal/adapter/inbound/http/document_handler_test.go`
- [X] T019 [US2] Re-baseline transcode goldens for the new baseline-JPEG params (SC-002 as amended) in `internal/imaging` golden fixtures; equivalence suite comparing streaming vs buffered outputs across the corpus
- [X] T020 [US2] Checkpoint: `make test && make test-vips && make lint` green

**Checkpoint**: image uploads stream; decode RAM bounded (threshold/budget).

## Phase 5: User Story 3 — Replace path streams identically (P3)

**Goal**: `StoreAndLink` uses the same pipeline; every 019 behavior intact
(FR-007, SC-006).

**Independent test**: 019's full replace suite green with reader transport;
large replace bodies stream; new guards (cap/stall/budget) active on replace.

- [X] T021 [US3] Adapt replace tests: `internal/domain/service/replace_mime_test.go` + `internal/domain/service/file_service_test.go` StoreAndLink tests drive `io.Reader` transport (semantics assertions unchanged — the 019 outcome matrix, zero-side-effect rejections via mock stage `Abort`); add over-limit and mid-stream-failure cases for replace in `internal/domain/service/ingest_test.go`
- [X] T022 [US3] Migrate `StoreAndLink` in `internal/domain/service/file_service.go` onto the ingest pipeline (reconcileReplaceMIME consumes the sniff prefix; EMPTY_CONTENT = empty prefix; mimeType/atomicity/outcome counters unchanged); stream `r.Body` through `progressReader` in `ReplaceContent` (`internal/adapter/inbound/http/document_handler.go`), removing `io.ReadAll`; extend replace handler tests for 413/stall outcomes
- [X] T023 [US3] Checkpoint: full 019 suites pass (`go test ./internal/... -run 'StoreAndLink|Reconcile|MimeRepair|ReplaceContent'`) — SC-006

## Phase 6: Polish & Cross-Cutting

- [X] T024 [P] Regenerate `openapi.yaml` (`make openapi`) — new 422 code joins the RejectedContentResponse family; verify drift-free; note the known generator attribution issue (antst/go-apispec#30) if it recurs in `specs/020-stream-uploads/contracts/ingest-contracts.md`
- [X] T025 [P] Coverage gate ≥95% on `internal/domain/service`, `internal/adapter/inbound/http`, `internal/adapter/outbound/storage/local` touched code; `make lint` clean
- [ ] T026 (REMAINING: needs running local stack + post-merge ops notes in PR) Quickstart end-to-end against the local stack: big-file RSS observation, **8-concurrent-uploads RSS check (SC-005: aggregate stays ≈ 8 × budget, not 8 × file size)**, over-limit/stall/abort guard checks with `/debug/vars` outcomes, 019-preservation run; record in the PR body the ops follow-ups — emptyDir scratch sizing, ingress body-size note for future cap raises, and the stacked-branch rebase step after PR #29 merges

## Dependencies

```
Phase 1 (T001 → T002 → T003)
  └─→ Phase 2 (T004→T005; T006→T007; T008; T009 — three [P] tracks)
        └─→ Phase 3 US1 (T010→T011→T012→T013→T014→T015)
              ├─→ Phase 4 US2 (T016→T017→T018→T019→T020)  # needs ingest.go + stage
              └─→ Phase 5 US3 (T021→T022→T023)             # needs ingest.go; independent of US2
Phase 6 (T024 ∥ T025 after all code; T026 last)
```

- US2 and US3 are independent of each other (different files: imaging vs
  replace path) → parallelizable by two agents after US1.
- T013/T014 (HTTP timeout plumbing) only touch adapter files → can proceed in
  parallel with T011 (domain pipeline) once T010's tests exist.

## Parallel execution examples

- **Phase 2**: T004→T005 (hash) ∥ T006→T007 (stage) ∥ T008 (processor port) ∥ T009 (metrics).
- **After US1**: agent A runs Phase 4 (imaging files), agent B runs Phase 5 (replace path) concurrently.
- **Phase 6**: T024 ∥ T025, then T026.

## Implementation strategy

**MVP = Phases 1–3 (US1)**: pass-through streaming covers most ingested bytes
and delivers the full memory win for non-images; independently shippable.
US2 adds the transcode path (fork-powered); US3 unifies replace. Total: 26
tasks (US1: 6, US2: 5, US3: 3, setup: 3, foundational: 6, polish: 3).
