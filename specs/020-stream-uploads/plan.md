# Implementation Plan: Stream Uploads to Permanent Storage

**Branch**: `020-stream-uploads` (stacked on `019-preserve-mime-on-edit` / PR #29) | **Date**: 2026-06-12 | **Spec**: [spec.md](./spec.md)
**Backlog Story**: https://github.com/alkem-io/file-service/issues/30

## Summary

Ingestion (create + replace) currently materializes every file in memory:
`io.ReadAll` in the handlers, `[]byte` through `FileService`, `[]byte` into
`StoragePort.Save`. This feature converts the pipeline to one-pass streaming:
request part → bounded sniff prefix → (optional streaming image transcode via
the govips fork) → hash-while-writing into a staged storage object →
commit/discard at end-of-stream. Memory per request becomes a fixed budget;
the upload cap becomes configuration (default unchanged at 32 MiB, validated
to 1 GiB); uploads get a progress-based idle timeout; rejection/abort paths
leave no partial permanent object.

## Technical Context

**Language/Version**: Go 1.26.1
**Primary Dependencies**: chi v5, pgx/v5 + sqlc, zap, `gabriel-vasile/mimetype`,
govips v2 — **consumed from the fork** via `replace github.com/davidbyttow/govips/v2
=> github.com/antst/govips/v2 @<pinned commit 10498ea>` (fork keeps the upstream
module path; standard fork-replace workflow). New fork APIs used:
`LoadImageFromReader` (+`AccessSequential`), `SaveToWriter`,
`SetStreamDiscThreshold`, `SetStreamScratchDir`, `SetPipeReadLimit`.
**Storage**: PostgreSQL `file` table (unchanged); content-addressed blobs
behind `port.StoragePort` — port gains a **reader-based, rename-free
stage/commit contract** (S3 is a planned backend; see research R2)
**Testing**: `go test` (unit + vips build-tag suite), 95% coverage target,
golangci-lint; allocation-bound tests for the memory budget
**Target Platform**: Linux containers; scratch disk = emptyDir (ops note)
**Project Type**: single hexagonal Go service
**Performance Goals**: per-request ingest memory ≤ fixed budget (256 KiB
copy buffer + 3 KiB sniff prefix on the pass-through path); no throughput
regression on small files
**Constraints**: stacked on 019 (FR-007 preserves its replace semantics); no
schema change; no API-shape change (multipart framing unchanged; **file part
arrives before metadata fields** — verified in the server adapter — so
bucket-level limits validate post-stream, research R4)
**Scale/Scope**: uploads to 1 GiB validated; concurrent large uploads bounded
by N × budget

## Constitution Check

*GATE: evaluated against `.specify/memory/constitution.md` v1.3.0*

| Principle | Status | Notes |
|---|---|---|
| I. Hexagonal Architecture | ✅ | Streaming pipeline lives in the domain service; vips stays behind `port.ImageProcessor` (port grows streaming methods); no adapter-to-adapter imports |
| II. Storage Abstraction | ✅ | New `StageWriter` contract designed backend-neutral (FS rename today, S3 multipart tomorrow — spec assumption honored) |
| IV. Type-Safe DB Access | ✅ | No new SQL |
| V. Security by Design | ✅ | Incremental size cap, idle timeout (slowloris), pixel budget (decode bombs) — all new guards |
| VI. Test-First | ✅ | Memory-budget, atomicity, and equivalence tests precede implementation (tasks) |
| VIII. DRY | ✅ | One ingest pipeline shared by create + replace; 019's reconcile logic reused untouched |
| X. No Legacy Code | ✅ | `[]byte` ingest paths are migrated, not duplicated; `Save([]byte)` becomes a thin wrapper during transition and is removed once all callers stream |
| XIV. Latest Dependencies | ✅ | Fork pin is temporary by design; upstream PR planned (spec Out of Scope) |

**Violations**: none. Complexity Tracking not required.

## Project Structure

### Documentation (this feature)

```text
specs/020-stream-uploads/
├── spec.md, plan.md, research.md, data-model.md, quickstart.md
├── contracts/
│   └── ingest-contracts.md
└── tasks.md (next: /speckit-tasks)
```

### Source Code (repository root)

```text
internal/
├── domain/
│   ├── port/
│   │   ├── storage.go            # +OpenStage(ctx) (StageWriter, error); StageWriter{io.Writer; Commit() (model.StoredFile, error); Abort() error}
│   │   └── image_processor.go    # +TranscodeStream(r io.Reader, w io.Writer, mimeType string) (port.TranscodeResult, error)
│   └── service/
│       ├── ingest.go             # NEW: streaming pipeline (sniff prefix → route → stage → commit/abort)
│       ├── ingest_test.go        # NEW: budget, atomicity, equivalence, dedup-at-end tests
│       ├── file_service.go       # CreateDocument/StoreAndLink take io.Reader; 019 reconcile reused on the prefix
│       └── hash.go               # +NewHasher() — streaming sha3-256, same digest as ComputeHash
├── adapter/
│   ├── inbound/http/
│   │   ├── document_handler.go   # Create: r.MultipartReader (drops ParseMultipartForm); ReplaceContent: streams r.Body
│   │   ├── progress_reader.go    # NEW: idle-timeout + size-cap reader (http.ResponseController.SetReadDeadline)
│   │   └── metrics.go            # +ingest outcome counters
│   └── outbound/storage/local/
│       └── adapter.go            # OpenStage: temp file + internal hash tee; Commit: stat-dedup → rename; Abort: remove
├── imaging/
│   ├── processor.go              # TranscodeStream impl: header load → pixel-budget → materialize-if-rotation → AutoRotate → SaveToWriter; dims from header
│   └── processor_stub.go         # stub: pass-through io.Copy
internal/config/config.go         # +MAX_UPLOAD_SIZE (32 MiB), +UPLOAD_IDLE_TIMEOUT_MS (30000), +IMAGE_PIXEL_BUDGET (100 MP), +VIPS_STREAM_DISC_THRESHOLD, +VIPS_SCRATCH_DIR, +VIPS_PIPE_READ_LIMIT
cmd/server/app.go                 # ReadHeaderTimeout replaces global ReadTimeout (research R6); wire vips knobs
go.mod                            # replace directive → fork pin
```

**Structure Decision**: one new domain file (`ingest.go`) holds the shared
pipeline; both ingest handlers route through it. Ports grow capabilities;
adapters implement them; no new packages.

## Design Outline

1. **Pipeline** (`ingest.go`): `bufio.Reader` peek of 3072 B → MIME
   resolve/reconcile (create: `resolveMIME` on the prefix; replace: 019's
   `reconcileReplaceMIME` unchanged — the empty-body rejection becomes
   "prefix is empty") → route: transcodable image →
   `ImageProcessor.TranscodeStream` writing into the stage; everything else
   → fixed-buffer `io.Copy` into the stage. The stage hashes internally;
   `Commit()` finalizes (hash → dedup stat → rename on FS) and returns
   `StoredFile`; every error path calls `Abort()`.
2. **Field order** (verified fact): the server adapter sends the `file` part
   *first*, metadata fields after. The global `MAX_UPLOAD_SIZE` guards the
   stream incrementally; bucket `maxFileSize`/`allowedMimeTypes` validate
   after parts finish — violation ⇒ `Abort()`. Documented in the contract.
   Optional follow-up (server repo): reorder fields before the file to
   enable early bucket-cap cutoff — not required for correctness.
3. **Timeouts** (research R6): global `ReadTimeout` is replaced by
   `ReadHeaderTimeout` (10 s); upload handlers wrap the body in
   `progressReader`, which extends the connection read deadline via
   `http.ResponseController` on each successful read — > 30 s without bytes
   ⇒ abort with the `stalled` outcome (FR-009). Non-upload endpoints get a
   fixed per-request deadline equal to today's behavior.
4. **Transcode** (research R5): compose fork primitives rather than the
   one-shot `TranscodeStream` so dimensions survive: sequential header load
   → pixel-budget check (FR-010, header dims, orientation-aware swap) →
   orientation ≥ 3 ⇒ disc-threshold materialization → `AutoRotate` →
   `SaveToWriter` into the stage. JPEG export switches to
   `Interlace: false` (research R7 — streaming wins over byte-identity;
   SC-002 amended in the spec alongside this plan).
5. **019 compatibility** (FR-007): `StoreAndLink`'s outcome matrix is
   untouched; only byte transport changes. All 019 tests pass with at most
   signature-level updates.

## Phase Outputs

- **Phase 0**: [research.md](./research.md) — 8 decisions, no open unknowns
- **Phase 1**: [data-model.md](./data-model.md), [contracts/ingest-contracts.md](./contracts/ingest-contracts.md), [quickstart.md](./quickstart.md)

## Post-Design Constitution Re-Check

Re-evaluated after Phase 1: no violations. No new packages; ports extended
rather than bypassed; the fork pin is documented temporary state with an
upstreaming exit path.
