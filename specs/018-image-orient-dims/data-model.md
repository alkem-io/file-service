# Phase 1 Data Model

## Database

### `file.content_metadata` (new column)

```sql
ALTER TABLE file
  ADD COLUMN content_metadata JSONB NOT NULL DEFAULT '{}'::jsonb;
```

- **Type**: `JSONB`. Forward-fit for additional per-content-type fields without further schema migrations.
- **Default**: `'{}'::jsonb`. Existing rows pick up the default during the `ALTER TABLE`. Backfill of legacy image rows is amortized via lazy-backfill (FR-018).
- **Nullable**: NOT NULL with default — every row always has a JSON object, even if empty. Simplifies queries (no `COALESCE` needed; predicate `content_metadata = '{}'::jsonb` matches both legacy and never-touched rows).
- **Index**: None initially. The lazy-backfill predicate (`mimeType LIKE 'image/%' AND content_metadata = '{}'::jsonb`) runs only inside the metadata-returning handlers' row read; we don't query the column at scale. If operator queries for `_decodeFailed` rows become frequent, a partial GIN index can be added later.

**Schema migration responsibility**: The actual `ALTER TABLE` runs in the paired alkem-io/server PR (server owns the DB lifecycle). This PR updates `db/schema/document.sql` (sqlc's source-of-truth) so generated Go bindings match.

### Logical states of `content_metadata`

| State | JSON shape | Meaning |
|-------|-----------|---------|
| Empty | `{}` | Non-image row, OR legacy row not yet read via metadata endpoint, OR newly-uploaded `image/*` whose Process hasn't completed yet (transient). |
| Measured | `{ "imageWidth": <int>, "imageHeight": <int> }` | Image dimensions known. Populated by `Process` at create/replace, or by lazy-backfill (FR-018). |
| Decode-failed | `{ "_decodeFailed": true }` | Bytes were present but libvips couldn't parse them. Permanent until manually cleared via SQL (out of scope). The pending-work predicate (`content_metadata = '{}'`) does not match this state, preventing repeated decode attempts. |

State transitions:
- Empty → Measured: at create time (Process succeeds), or lazy-backfill (header decode succeeds + persist succeeds).
- Empty → Decode-failed: at create time (Process tries to measure but fails), or lazy-backfill (header decode fails per FR-019).
- Empty → Empty (no transition): lazy-backfill encounters Storage.Read or DB persist failure (FR-020); no sentinel persisted, next read retries.
- Measured → other: never (within this feature's scope).
- Decode-failed → other: never (within this feature's scope; operator can clear via SQL if needed).

### New sqlc query

```sql
-- name: BackfillContentMetadata :exec
-- Updates only the content_metadata JSONB column. Does NOT bump version
-- (avoids race with optimistic-locked PATCH per FR-018).
UPDATE file
SET content_metadata = $2
WHERE id = $1;
```

### Existing queries — additions

`CreateDocument` / `UpdateDocumentFile` / `FindDocumentByExternalIDAndBucket` / `GetDocumentByID` all gain `content_metadata` in their `SELECT` / `INSERT` / `UPDATE` column lists so the generated `Document` row struct includes the column. `CreateDocument` and `UpdateDocumentFile` accept it as a parameter (populated from `Process`). `Copy` does its insert from the source row's `content_metadata` value (propagated verbatim per FR-014).

## Domain types

### `model.Document` (modified)

```go
type Document struct {
    // ...existing fields unchanged...

    Reused bool

    // ImageWidth and ImageHeight are post-rotation pixel dimensions for
    // image rows whose dimensions could be measured. Both nil when the
    // upload is not an image, or when the row's content_metadata is
    // empty/sentinel. Populated from content_metadata on read; from
    // ProcessResult at create/replace.
    ImageWidth  *int
    ImageHeight *int
}
```

`ImageWidth` and `ImageHeight` are populated by:
1. The alkemiodb adapter, when reading a row's `content_metadata` and the JSON shape matches `{ "imageWidth": N, "imageHeight": N }`.
2. The lazy-backfill helper, when it just measured dims and is about to persist them.
3. The service's create/replace path, propagating from `ProcessResult`.

### `port.ImageProcessor` (modified)

```go
// ProcessResult captures everything Process decided about an upload.
// ImageWidth/ImageHeight are nil when the input is not an image-MIME,
// when the dims couldn't be measured (decode failure), or when the
// processor doesn't decode at all (no-vips stub).
//
// Measured distinguishes "decoder ran" from "decoder unavailable":
//   - Measured=true  → an image decoder ran on these bytes; nil dims
//                       mean the decoder confirmed the bytes are
//                       unreadable (write _decodeFailed sentinel).
//   - Measured=false → no decoder attempted (no-vips stub for the !vips
//                       build); nil dims mean nothing about the bytes
//                       (leave content_metadata empty for retry later).
type ProcessResult struct {
    Content     []byte
    MimeType    string
    ImageWidth  *int
    ImageHeight *int
    Measured    bool
}

type ImageProcessor interface {
    DetectMIME(content []byte) string
    Process(content []byte, mimeType string) (ProcessResult, error)

    // MeasureDims performs a header-only decode (no pixel decode, no re-encode)
    // and returns post-rotation pixel dimensions. Used by the lazy-backfill
    // path in service.backfillIfNeeded; separate from Process because Process
    // re-encodes for raster orient!=1 inputs which would violate FR-018's
    // "no pixel decode, microseconds even for large files" guarantee.
    // Implementation wraps vips.NewImageFromBuffer + extractDims + img.Close.
    // Returns (nil, nil, err) on vips-load failure so the caller can persist
    // the {"_decodeFailed": true} sentinel per FR-019.
    MeasureDims(content []byte, mimeType string) (*int, *int, error)
}
```

### `model.StoredFile` (modified)

```go
// StoredFile is the result of a content-store operation (Save / StoreAndLink).
// Already carries ExternalID, MimeType, Size, Created. This feature adds
// post-rotation pixel dimensions so the Replace handler can surface them
// on ReplaceContentResponse without an extra row read.
type StoredFile struct {
    ExternalID  string
    MimeType    string
    Size        int
    Created     bool
    ImageWidth  *int  // populated from ProcessResult inside StoreAndLink; nil for non-image
    ImageHeight *int
}
```

`StoredFile` is the dim transport for the Replace path specifically. Create / Copy / PATCH carry dims via `model.Document` (see above). Both transports are populated from the same source (`ProcessResult` for Process-driven paths; `content_metadata` JSON for read-only paths).

### `port.DocumentRepo` (modified)

```go
type DocumentRepo interface {
    // ...existing methods unchanged...

    // BackfillContentMetadata persists computed content_metadata for a row.
    // Updates only the content_metadata column; does NOT bump version
    // (FR-018). The metadata argument is the raw JSON to write; callers
    // produce it via json.Marshal of the appropriate struct.
    BackfillContentMetadata(ctx context.Context, id uuid.UUID, metadata []byte) error
}
```

`Create` and `UpdateFile` (existing) gain a `contentMetadata []byte` parameter so the create-time persistence is one query, not two.

## HTTP DTOs

### Modified response payloads

All four metadata-returning responses gain two optional integer fields:

```go
type CreateDocumentResponse struct {
    ID          string `json:"id"`
    ExternalID  string `json:"externalID"`
    MimeType    string `json:"mimeType"`
    Size        int    `json:"size"`
    Reused      bool   `json:"reused"`
    ImageWidth  *int   `json:"imageWidth,omitempty"`
    ImageHeight *int   `json:"imageHeight,omitempty"`
}

type ReplaceContentResponse struct {
    ExternalID  string `json:"externalID"`
    MimeType    string `json:"mimeType"`
    Size        int    `json:"size"`
    ImageWidth  *int   `json:"imageWidth,omitempty"`
    ImageHeight *int   `json:"imageHeight,omitempty"`
}

type UpdateDocumentResponse struct {
    ID                string `json:"id"`
    StorageBucketID   string `json:"storageBucketId"`
    TemporaryLocation bool   `json:"temporaryLocation"`
    DisplayName       string `json:"displayName"`
    ImageWidth        *int   `json:"imageWidth,omitempty"`
    ImageHeight       *int   `json:"imageHeight,omitempty"`
}

// Copy already reuses CreateDocumentResponse for its response — no
// separate DTO needed; the new fields ride along.
```

`omitempty` on the pointer-typed fields ensures absent (`null`/missing on the wire) when not measured. Generated TypeScript clients see `imageWidth?: number | undefined`.

## Validation rules

- `ImageWidth`/`ImageHeight`, when present, are positive integers (post-rotation pixel count). Process and lazy-backfill never set `0` — if measurement succeeds, both are ≥ 1; if it fails, both are nil.
- Either both are present or both nil — never one without the other.
- The handler does not validate dimensions against any policy. Pixel-range constraints live on the upstream alkem-io/server.

## Persistence flow

| Path | Reads `content_metadata` from | Writes `content_metadata` via |
|------|------------------------------|------------------------------|
| `POST /internal/file` (create, fresh) | n/a | Existing `CreateDocument` query (gains `contentMetadata` param), populated from `ProcessResult` |
| `POST /internal/file` (create, dedup hit) | Existing row's column | Lazy-backfill if existing row's value is `{}` and mimeType is `image/*` |
| `POST /internal/file/copy` | Source row's column | Propagates verbatim into new row's `CreateDocument` insert. If source is `{}`, lazy-backfill on the source first, then propagate populated value. |
| `PUT /internal/file/{id}/content` (replace) | n/a | Existing `UpdateDocumentFile` query (gains `contentMetadata` param), populated from `ProcessResult`. Existing row's previous value is overwritten. |
| `PATCH /internal/file/{id}` | Current row's column | Lazy-backfill if current row's value is `{}` and mimeType is `image/*`. PATCH itself does not write `content_metadata` — only `BackfillContentMetadata` does. |

## Test data fixtures

For `internal/imaging/processor_test.go`:
- `testdata/landscape-orient1.jpg` — already canonical (existing fixture if any)
- `testdata/landscape-orient6.jpg` — wide bytes + EXIF Orientation=6 (renderer-display dims = swapped)
- Same pair for `.png`, `.bmp`, `.avif`, `.webp`, `.heic`
- `testdata/wide-gamut-displayp3.jpg` — image with embedded Display P3 ICC profile
- `testdata/test.svg` — known viewBox dimensions
- `testdata/test.gif` — known canvas dimensions (single frame; multi-frame is functionally equivalent)
- `testdata/corrupt.png` — bytes that fail libvips load (truncated header)

Generation: a `make testdata-images` target that uses ImageMagick / `cwebp` / `heif-enc` to produce the fixtures from a single source PNG. Or hand-crafted with `exiftool -Orientation=6` for the orientation pairs.
