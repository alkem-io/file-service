# Implementation Plan: Canonicalize image orientation and return post-rotation dimensions

**Branch**: `018-image-orient-dims` | **Date**: 2026-05-07 | **Spec**: [spec.md](./spec.md)
**Input**: Feature specification from `/specs/018-image-orient-dims/spec.md`

## Summary

Make the file-service the authoritative source for both canonical image bytes and post-rotation pixel dimensions, for every `image/*` upload it accepts. Three focused changes cover it:

1. **`Process` extension** — Add EXIF-orientation handling for PNG, BMP, AVIF (today only JPEG/WebP/HEIC are canonicalized). Read orientation cheaply via libvips header parse; pass through unchanged when orientation is `1`/absent; rotate + strip EXIF + re-encode in same format when orientation is rewriting; preserve ICC across every re-encode path via `ImageRef.RemoveMetadata()`. Measure dims for ALL `image/*` MIMEs (rasters + SVG + GIF) using the same cheap header parse.
2. **`content_metadata` JSONB column** — Add a single per-row JSON column on `file` carrying `{ "imageWidth": N, "imageHeight": N }` for measurable image rows, `{ "_decodeFailed": true }` for unreadable ones, `{}` for non-image / not-yet-measured rows. Forward-fit for future per-content-type fields.
3. **Lazy backfill on metadata read** — When a metadata-returning endpoint (Create dedup hit, Copy, PATCH) reads a row whose `content_metadata` is empty AND whose `mimeType` is `image/*`, perform a header-only decode, persist the result via a dedicated `BackfillContentMetadata(id, jsonb)` query that does NOT bump `version` (so it doesn't race with optimistic-locked PATCH), and include the dims on the response. Best-effort, type-aware failure handling per FR-019/020.

The four metadata-returning endpoints (`POST /internal/file`, `POST /internal/file/copy`, `PUT /internal/file/{id}/content`, `PATCH /internal/file/{id}`) all surface `imageWidth` / `imageHeight` on their response.

No background workers. No env vars. No new tables. ICC profiles preserved on every re-encode. EXIF/IPTC/XMP stripped on re-encode. Legacy bytes on storage are not re-canonicalized (only new uploads are byte-canonical) — but their dims are measured and reported.

## Technical Context

**Language/Version**: Go 1.26 (constitution-mandated)
**Primary Dependencies**: govips v2.18.0 (libvips bindings — uses `ImageRef.Orientation()`, `RemoveMetadata()`, `ExportPng`/`ExportAvif`/`ExportMagick`), mimetype v1.4.13, chi v5.2.5, pgx v5.9.1, sqlc, zap v1.27.1
**Storage**: PostgreSQL (Alkemio shared DB; this service owns the `file` table — adds one JSONB column) and local filesystem (file bytes; content-addressed by SHA3-256, unchanged)
**Testing**: `go test -tags vips ./...` for the libvips-backed path. The no-vips stub (`internal/imaging/processor_stub.go`) updates in lock-step with the new return type; lazy-backfill is a no-op there.
**Target Platform**: Linux server, containerized; libvips with ImageMagick support (already in the production image — needed for BMP write path; FR-012 covers the loud-fail case if a stripped libvips is deployed)
**Project Type**: Hexagonal Go service, single project
**Performance Goals**: No new SLA. JPEG/WebP/HEIC behavior unchanged. New dim-measurement step is a libvips lazy header read (microseconds even for 50MB files). Lazy-backfill is amortized across natural reads, not a synchronous bulk operation.
**Constraints**: ICC profile MUST survive every re-encode (FR-006). No EXIF synthesis (FR-007). Deterministic re-encode for stable content dedup (FR-010). Legacy `BackfillContentMetadata` writes MUST NOT bump `version` (FR-018). Underlying request MUST NOT fail on backfill errors (FR-020).
**Scale/Scope**: Same image traffic the service handles today. Schema change is one column. Code change touches 6 areas (model, port, service, imaging adapter, alkemiodb adapter, HTTP handlers/DTOs).

**Schema migration coordination**: The actual `ALTER TABLE file ADD COLUMN content_metadata JSONB NOT NULL DEFAULT '{}'` runs in the paired alkem-io/server PR (server owns the DB lifecycle). This PR updates `db/schema/document.sql` (sqlc's source-of-truth for code generation) so the generated Go bindings match. File-service-go itself has no runtime DDL execution.

## Constitution Check

*GATE: Must pass before Phase 0 research. Re-checked after Phase 1 design.*

| Principle | Compliance |
|-----------|------------|
| **I. Hexagonal Architecture** | ✅ Domain types stay clean. `port.ImageProcessor` gains: `Process` returning `ProcessResult` (bytes, MIME, optional dims, `Measured` flag) and `MeasureDims` (header-only dim extraction for the lazy-backfill path, separate from `Process` so it can satisfy FR-018's "no pixel decode" guarantee). New repo port method `BackfillContentMetadata` follows the existing pattern. No adapter-to-adapter imports introduced. |
| **II. Storage Abstraction** | ✅ Untouched. SHA3-256 content addressing unchanged. The lazy-backfill calls `Storage.Read(externalID)` through the existing port. |
| **III. Alkemio Integration First** | ✅ Public/private endpoint split unchanged. Auth flow unchanged. The new `imageWidth`/`imageHeight` fields ride on existing private-endpoint responses. The `file` table schema change is coordinated with the paired alkem-io/server PR. |
| **IV. Type-Safe Database Access** | ✅ All DB access via sqlc. New sqlc query `BackfillContentMetadata` added to `db/queries/document.sql`. Existing `CreateDocument` / `UpdateDocumentFile` queries gain a `content_metadata` parameter. Schema change is reflected in `db/schema/document.sql` (sqlc input). |
| **V. Security by Design** | ✅ Strengthens hygiene: physical EXIF strip on every raster format eliminates GPS / IPTC leakage. ICC profiles are color metadata only, not identifying — preserving them is safe. The new `_decodeFailed` sentinel is purely diagnostic and contains no user data. |
| **VI. Test-First Development** | ✅ Test plan in [research.md](./research.md). Per-format orientation pairs (raster) + dim-measurement tests (all image MIMEs including SVG/GIF) + ICC round-trip + deterministic-bytes + lazy-backfill paths (success + 3 failure modes) + version-disjoint-write race test. |
| **VII. Root Cause Analysis** | ✅ The motivating regression is documented in the spec (server-side `image-size` ignores EXIF orientation; phone photos rejected). The fix matches the cause. |
| **VIII. DRY** | ✅ Lazy-backfill is a single helper called from Create-dedup-hit, Copy, and PATCH paths. Dim extraction is a single helper used by both Process (create-time) and the lazy-backfill (read-time). The "rotate + RemoveMetadata + re-encode in same format" path is shared across PNG/BMP/AVIF. |
| **IX. Lint on Completion** | ✅ Plan ends with `golangci-lint run` clean before commit. |
| **X. No Legacy Code** | ✅ `Process` signature changes from `(bytes, MIME, error)` to `(ProcessResult, error)` in lock-step across the vips and stub adapters; no parallel old API kept. |
| **XI. No Busywork** | ✅ Every artifact (research, data-model, contracts, quickstart) is tied to a specific FR or SC in the spec. |
| **XII. Meaningful Tests Only (95% coverage)** | ✅ Each test in the plan defends a real invariant (orientation in output, dim correctness, ICC byte-for-byte, deterministic dedup, response shape, failure semantics, version-disjoint write). No coverage padding. |
| **XIII. Meaningful Success Criteria** | ✅ All 14 SCs in the spec are testable inside this service. SC-006 (server stops local decode) is observed from this side as "the response shape is sufficient" — the actual server-side validation lives in the paired PR. |
| **XIV. Latest Dependencies Always** | ✅ govips already at v2.18.0 (latest stable; verified in module cache). No new deps. |
| **XV. No Assumptions** | ✅ All open questions resolved across the five clarifications recorded in the spec, plus the design decisions captured in [research.md](./research.md). |

**Anti-Pattern Check**:
- ✅ #1 Speculative fixes: each line traces to a spec FR/SC.
- ✅ #2 "Just in case": none.
- ✅ #3 Duplication: shared helpers for backfill, dim extraction, rotation.
- ✅ #4 Coverage padding: tests defend real invariants.
- ✅ #5 Invented SLAs: none.
- ✅ #6 Speculative abstractions: `content_metadata` is JSON-flexible because the user explicitly asked for forward-fit (Q2); not invented.
- ✅ #7 Obvious comments: only WHY-level comments where invariant is non-obvious (e.g., "no version bump to avoid PATCH race").
- ✅ #8 Training-data versions: govips verified live in module cache.
- ✅ #9 New doc files: only the workflow-prescribed plan/research/data-model/contracts/quickstart.
- ✅ #10 Assumptions: all open questions clarified in the spec.
- ✅ #11 `map[string]any` in HTTP: each response remains a typed struct with `Render()`. The new fields are added to existing structs.

**Result**: Constitution check passes. No complexity-tracking entries needed.

## Project Structure

### Documentation (this feature)

```text
specs/018-image-orient-dims/
├── plan.md              # This file
├── research.md          # Phase 0 — design decisions (govips API, BMP path, ICC, signatures, lazy-backfill placement)
├── data-model.md        # Phase 1 — Document, ProcessResult, content_metadata JSON shape
├── quickstart.md        # Phase 1 — local verification flows
├── contracts/
│   └── openapi-delta.md # Phase 1 — schema deltas on Create/Copy/Replace/PATCH responses
├── checklists/
│   └── requirements.md  # Spec-quality validation
└── tasks.md             # Phase 2 (created by /speckit.tasks)
```

### Source Code (repository root)

Single hexagonal Go project. Change touches six layers:

```text
db/
├── schema/
│   └── document.sql                   # +content_metadata JSONB NOT NULL DEFAULT '{}'
└── queries/
    └── document.sql                   # +content_metadata in CreateDocument/UpdateDocumentFile;
                                       # +BackfillContentMetadata (UPDATE only content_metadata, NO version bump)

internal/
├── domain/
│   ├── model/
│   │   └── document.go                # +ImageWidth *int, +ImageHeight *int (response-only)
│   ├── port/
│   │   ├── image_processor.go         # ImageProcessor: Process → ProcessResult (bytes/MIME/dims/Measured); MeasureDims (header-only) for lazy-backfill
│   │   └── document_repo.go           # +BackfillContentMetadata(ctx, id, jsonb) error
│   └── service/
│       ├── file_service.go            # +backfillIfNeeded helper; called from Create-dedup-hit, Copy, PATCH paths;
│       │                              # CreateDocument/StoreAndLink propagate dims into model.Document
│       └── file_service_test.go       # mock processor returns dims; tests for lazy-backfill success + 3 failure modes
├── imaging/
│   ├── processor.go                   # PNG/BMP/AVIF rotation paths; dim measurement for all image/*;
│   │                                  # ICC preservation via RemoveMetadata()
│   ├── processor_stub.go              # Signature parity (returns ProcessResult{bytes, MIME, nil dims})
│   └── processor_test.go              # Per-format orientation pairs; ICC round-trip; deterministic bytes; SVG/GIF dim
└── adapter/
    ├── inbound/http/
    │   ├── dto.go                     # +ImageWidth/ImageHeight on CreateDocumentResponse, CopyResponse,
    │   │                              # ReplaceContentResponse, UpdateDocumentResponse (all *int omitempty)
    │   ├── document_handler.go        # Surface dims on all 4 responses; trigger lazy-backfill on read
    │   └── document_handler_test.go   # Response-shape tests; lazy-backfill end-to-end; failure modes
    └── outbound/alkemiodb/
        ├── adapter.go                 # +BackfillContentMetadata method
        └── adapter_test.go            # Mock + real-DB integration tests for the new query
openapi.yaml                            # Regenerated by `make openapi`
```

**Structure Decision**: Single hexagonal Go project, no new packages. The change is purely additive in each existing layer. No `internal/datamigrate/` package (data-migrator framework was dropped after clarification Q3 + design pivot to lazy-backfill).

## Complexity Tracking

> **Fill ONLY if Constitution Check has violations that must be justified**

(none — all constitution checks pass cleanly.)
