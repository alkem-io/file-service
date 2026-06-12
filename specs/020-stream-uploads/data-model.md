# Data Model: Stream Uploads to Permanent Storage

**No schema changes.** The `file` table, content-addressed blobs, dedup
semantics, and 019's MIME invariants are untouched; only the byte transport
changes.

> **Terminology**: table `file` ↔ domain `Document` (pre-existing repo
> convention, as in spec 019).

## New domain concepts

### StageWriter (port-level, per-backend)

| Member | Contract |
|---|---|
| `Write(p []byte)` | Appends to the staging artifact; hashes internally (streaming sha3-256, digest-identical to `ComputeHash`). |
| `Commit() (StoredFile, error)` | Finalizes the hash; performs the backend publish: FS = dedup-stat → rename; (future S3 = complete-multipart → server-side copy). Returns `Created=false` on dedup hit, exactly like today's `Save`. |
| `Abort() error` | Destroys the staging artifact. Idempotent; safe after a failed `Commit`; deferred on every ingest path. |

**State machine**: `open → (writing)* → committed | aborted`. No partial
permanent object is observable in any state (FR-006); `committed` is the
only state that publishes.

### Ingest outcome (observability vocabulary, FR-008)

`accepted | fallback_generic_sniff | rejected_empty | rejected_mismatch`
(019, unchanged) **plus** `rejected_over_limit | rejected_pixel_budget |
rejected_bucket_policy | stalled | client_abort | failed_mid_stream`.
Counted in `ingest_outcomes_total` (expvar map, adapter-side); the 019
`content_replace_outcomes_total` map keeps its keys for replace.

### TranscodeResult (port-level)

`{MimeType string, ImageWidth, ImageHeight *int}` — replaces the dims fields
of today's `ProcessResult` for the streaming path; orientation-aware
(width/height swapped for EXIF 5–8 *before* the budget check and recording).

## Entity touch points (existing)

| Entity | Field | Change |
|---|---|---|
| Document (`file`) | `externalID` | Same hash algorithm (sha3-256), now computed during streaming — digests identical by construction (shared hasher) |
| Document | `mimeType` | Decision logic unchanged (019 reconcile / create resolve), now fed by the 3072 B sniff prefix instead of the whole body |
| Document | `size` | From the stage's byte count (was `len(content)`) |
| Document | `content_metadata` | Dims from `TranscodeResult` (header-derived) — same values the buffered decoder produced |

## Validation rules (from FRs)

- FR-001/005: limit reader enforces `MAX_UPLOAD_SIZE` incrementally; the
  copy buffer (256 KiB) + sniff prefix (3 KiB) is the entire pass-through
  budget.
- FR-002: dedup decision uses the stage's final hash; a dedup hit aborts the
  staging artifact and reuses the existing row (Reused=true), as today.
- FR-006: every non-commit exit path runs `Abort()`; `Commit` only after all
  multipart parts are consumed and validated (R4 — trailing fields).
- FR-009: `progressReader` bumps the read deadline per successful read.
- FR-010: pixel budget = `width × height ≤ IMAGE_PIXEL_BUDGET` from header
  metadata, checked before any pixel decode.

## Configuration (new)

| Env | Default | Governs |
|---|---|---|
| `MAX_UPLOAD_SIZE` | 33554432 (32 MiB) | incremental stream cap (FR-005) |
| `UPLOAD_IDLE_TIMEOUT_MS` | 30000 | progress idle abort (FR-009) |
| `IMAGE_PIXEL_BUDGET` | 100000000 (100 MP) | decode guard (FR-010) |
| `VIPS_STREAM_DISC_THRESHOLD` | library default (100 MB) | decoded-frame RAM→scratch spill |
| `VIPS_SCRATCH_DIR` | os.TempDir() | scratch location (emptyDir in k8s) |
| `VIPS_PIPE_READ_LIMIT` | library default | non-seekable compressed-input accumulation bound |
