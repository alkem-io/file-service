# Research: Stream Uploads to Permanent Storage

Sources: file-service code (this worktree, stacked on 019), the govips fork
(antst/govips#2 @ `10498ea` — implementation, README §16, tests), the server
adapter (`file.service.adapter.ts`), Go stdlib semantics.

## R1 — Pipeline ownership: one domain `ingest` path for create and replace

**Decision**: A single streaming pipeline in `internal/domain/service/ingest.go`
consumed by both `CreateDocument` and `StoreAndLink`; handlers deliver an
`io.Reader` (plus, for create, the metadata fields as they arrive).

**Rationale**: DRY (constitution VIII) and FR-007 — replace must keep 019's
exact semantics, which is easiest when both paths share the transport and
differ only in their MIME-decision function (`resolveMIME` vs
`reconcileReplaceMIME`, both operating on the sniff prefix).

**Alternatives**: separate streaming code per handler — rejected: two
implementations of staging/abort/hash invite drift exactly where atomicity
guarantees live.

## R2 — StoragePort: stage/commit contract, not writer-with-rename

**Decision**:

```go
OpenStage(ctx context.Context) (StageWriter, error)
type StageWriter interface {
    io.Writer
    Commit() (model.StoredFile, error) // finalize hash → dedup → publish
    Abort() error                      // destroy staging artifact (idempotent)
}
```

Hashing happens *inside* the stage (the adapter tees to a streaming sha3-256
— `NewHasher()` alongside the existing `ComputeHash` to guarantee identical
digests). `Commit` performs the backend's publish step; `Abort` is safe to
call after failed `Commit` and is deferred on every path.

**Rationale**: backend-neutral per the spec's no-atomic-rename assumption —
FS implements Commit as a no-overwrite hard link (with O_EXCL create+copy
fallback), so concurrent identical commits cannot overwrite each other; a
future S3 adapter implements it as complete-multipart + server-side copy,
with Abort = abort-multipart.
Putting the hash inside the stage keeps the identity computation in one
place per backend pipeline and lets Commit dedup without re-reading.

**Alternatives**: (a) `SaveStream(r io.Reader)` pull-shape — rejected: the
transcode path *pushes* (SaveToWriter); a pull port would force an io.Pipe
inside the service for every image; the push-shaped stage needs no pipe
anywhere. (b) Returning hash to caller for the dedup decision — kept:
`StoredFile.Created=false` signals dedup hits exactly as today.

## R3 — Existing `Save([]byte)` migration

**Decision**: Reimplement `Save` as `OpenStage`+write+`Commit` immediately
(one code path), keep it exported for the copy flow and the repair job's
small writes, and migrate ingest callers to the stage API. No deprecated
duplicate logic remains (constitution X).

## R4 — Multipart handling and field order

**Decision**: Replace `ParseMultipartForm` with `r.MultipartReader()` and
process parts sequentially. Verified fact: the server adapter appends the
**file part first**, then `displayName`, `storageBucketId`,
`authorizationId`, `tagsetId?`, `createdBy?`, `temporaryLocation?`,
`allowedMimeTypes?`, `maxFileSize?`, `skipDedup?`. Therefore:

- The stream guard during the file part is the service-level
  `MAX_UPLOAD_SIZE` (config, default 32 MiB — the clarified rollout-neutral
  default), enforced incrementally by the limit reader.
- Bucket-level `maxFileSize` and `allowedMimeTypes` are validated when their
  fields arrive (after the file part): violation ⇒ `Abort()` the stage and
  reject. Worst case a rejected upload transfers ≤ MAX_UPLOAD_SIZE bytes —
  bounded and safe, identical user outcome.
- Trailing-field validation errors must not be maskable by an
  already-committed stage ⇒ Commit happens only after *all* parts are
  consumed and validated.

**Alternatives**: requiring fields-before-file — rejected: needs a server
repo change (breaks single-repo scope); recorded as an optional follow-up
for early bucket-cap cutoff.

## R5 — Transcode composition (dims survive; one-shot helper not used)

**Decision**: Implement `ImageProcessor.TranscodeStream` in `imaging` by
composing fork primitives instead of calling the fork's one-shot
`TranscodeStream`: `LoadImageFromReader(r, AccessSequential)` → header dims
+ orientation (no pixels decoded) → **pixel-budget check (FR-010)**, with
width/height swapped for orientations 5–8 → orientation ≥ 3 ⇒ materialize
(fork handles memory-vs-scratch by `SetStreamDiscThreshold`) → `AutoRotate`
→ `SaveToWriter(stage)`. Return `TranscodeResult{MimeType, Width, Height}`.

**Rationale**: the one-shot helper doesn't expose dimensions, which the
document row records (`content_metadata`); the composition is the same code
path the helper uses internally, with dims read from the header between load
and save. Budget check from header metadata costs nothing and runs before
any pixel decode — including HEIC's whole-frame codec decode, which the disc
threshold cannot bound (the budget is the only RAM guard there; spec
clarification 2026-06-12).

**Implementation deltas (recorded at build time)**: (1) the fork's
disc-spill materialization is reachable through the *default* (non-
sequential) reader load, so the composition probes the buffered head
(≤64 KiB — added to the fixed budget) for dims/orientation, picks
sequential vs materialized access, and reads dims post-AutoRotate from the
ImageRef; (2) BMP and AVIF have no streaming saver in the library — they
join GIF/SVG as byte-identical pass-through, with dims measured by a
header-only read of the completed stage before commit (rotated BMP/AVIF lose
eager canonicalization; vanishingly rare); (3) HEIC→JPEG is now a single encode at Q82
baseline (the buffered path double-encoded Q100→Q82 — transcoded-class
output re-baselined per amended SC-002).

## R6 — Timeout architecture

**Decision**: Set `ReadHeaderTimeout: 10s` while retaining the server-wide
`ReadTimeout: 30s` for ordinary request bodies; introduce `progressReader` in
the HTTP adapter that wraps upload bodies and calls
`http.NewResponseController(w).SetReadDeadline(now+idle)` after every
successful `Read` (idle = `UPLOAD_IDLE_TIMEOUT_MS`, default 30 000), extending
the deadline while upload bytes keep arriving. A read that trips the deadline
surfaces as the `stalled` ingest outcome.

**Rationale**: progress-based semantics per the clarified FR-009; per-route
deadline control is exactly what `ResponseController` exists for (Go ≥1.20),
no middleware framework needed. The server keeps a 30 s `ReadTimeout` for
ordinary request bodies; upload reads continually extend it. Global
`WriteTimeout` is disabled because it is absolute from request headers and
would kill a healthy transfer based on total duration. The HTTP router instead
arms a 30 s write-idle deadline lazily when a response begins and extends it on
every write, bounding ordinary JSON responses without penalizing long uploads;
streamed blob responses additionally refresh it immediately before each chunk.

**Alternatives**: raising the global ReadTimeout — rejected in
clarification (slowloris exposure scales with the cap).

**h2c caveat (analyze finding I1)**: the server also runs h2c
(`cmd/server/app.go`) — `SetReadDeadline` support on the x/net HTTP/2 path
must be verified by test (T013 covers both transports). **Resolved at implementation time**: x/net's
h2c server DOES support `SetReadDeadline` (verified by
`TestProgressReader_StallAborts_H2C`), so the primary mechanism works on
both transports; the only h2c quirk is the deadline error shape (a bare
"i/o timeout" not satisfying `os.IsTimeout` — matched explicitly). The
watchdog fallback is implemented anyway and engages automatically if a
future transport returns `ErrNotSupported`.

**Transport modernization (deps rollup, post-merge of this spec)**: the
deprecated x/net h2c wrapper was replaced by Go's native
`http.Server.Protocols` unencrypted HTTP/2 (+ `http.HTTP2Config`). The
progressReader h2c stall test runs against the native transport and passes
— per-stream read deadlines hold there as well; the error-shape match and
watchdog fallback remain as written.

## R7 — JPEG interlace: streaming wins over byte-identity (SC-002 amended)

**Decision**: Switch JPEG export to `Interlace: false` (baseline) for all
transcode outputs. SC-002 is amended in the spec: byte-identity with the
pre-change implementation holds for **non-transcoded content**; transcoded
outputs are instead byte-identical to the corresponding buffered `Export*`
call with the *new* params (fork-guaranteed), with goldens re-baselined.

**Rationale (the conflict)**: verified — file-service uses
`NewJpegExportParams()` whose default is `Interlace: true` (progressive),
and progressive JPEG *encode buffers the entire compressed output* before
the first byte reaches the writer (fork README §16), defeating US2's "output
never held whole". Keeping interlace would silently void the feature's
guarantee for the most common transcode output. Baseline JPEG costs slightly
worse perceived progressive loading on slow connections; decoded pixels and
quality are unchanged (same Q82). Dedup impact: re-uploading a source image
after the change yields a different (baseline) blob than its pre-change
(progressive) copy — acceptable; content-addressing is exact-bytes identity
by definition.

**Alternatives**: keep `Interlace: true`, accept one whole compressed-output
copy in vips' target — rejected: contradicts US2/FR-004 as written, and the
buffered copy scales with output size precisely when large-image support is
the point.

## R8 — Fork consumption and pinning

**Decision**: `go.mod` gains
`replace github.com/davidbyttow/govips/v2 => github.com/antst/govips/v2 <pseudo-version of 10498ea>`.
The fork deliberately keeps the upstream module path (`module
github.com/davidbyttow/govips/v2` — verified), which is the standard
fork-replace workflow; `go mod tidy` resolves the pseudo-version. Wire the
three knobs at startup from config: `SetStreamDiscThreshold`,
`SetStreamScratchDir`, `SetPipeReadLimit`. The stub (`!vips`) build keeps
compiling: its `TranscodeStream` is a pass-through copy.

**Ops note (for the deploy/PR body)**: scratch dir needs an emptyDir mount
sized for `concurrent transcodes × disc-threshold-exceeding frames`;
defaults (os.TempDir, 100 MB threshold) are safe for current traffic.

## Post-review deltas (backfilled 2026-06-12 after PR #31 merged)

1. **R4 addendum — metadata field cap.** Each non-file multipart field is
   capped at 16 KiB and *rejected* (400, field named) when exceeded — read
   cap+1 then check, never truncate: a silently-truncated `displayName` or
   `allowedMimeTypes` would parse as a different request. The already-staged
   file part is aborted on this path (FR-006); the regression test also
   asserts the abort.
2. **FR-005 enforcement.** `MAX_UPLOAD_SIZE` is not merely validated to
   1 GiB — configuration above the ceiling is rejected at startup, so the
   knob cannot silently enter unproven territory.
3. **vips knob ordering.** `imaging.ConfigureStreaming` runs *after*
   `imaging.New()` (which performs `vips.Startup`); stream knobs set on an
   uninitialized libvips would be ignored.
