# Quickstart — verifying image canonicalization + lazy backfill locally

Manual verification flows that mirror the automated tests. Run them after the implementation lands to confirm the feature works end-to-end.

## Prerequisites

- Local stack running with libvips support (`make run` or the Docker build with the `vips` build tag).
- `exiftool` for inspecting metadata: `brew install exiftool` / `apt install libimage-exiftool-perl`.
- `psql` access to the local Alkemio DB to inspect / seed `content_metadata`.
- A test directory with sample images (see "Test fixtures" below).

## Test fixtures

For each raster format, three flavors:
1. **Canonical** — orientation tag is `1` or absent.
2. **Rotated** — orientation tag is `6` (common phone-camera case).
3. **Wide-gamut** — embeds a Display P3 ICC profile.

Plus a known-dimension SVG and GIF for non-raster image coverage.

```bash
# Canonical baseline (1024×512 wide rectangle)
convert -size 1024x512 xc:white -fill black -draw "rectangle 0,0 32,512" canonical.png
cp canonical.png rotated.png && exiftool -Orientation=6 -n -overwrite_original rotated.png

# Wide-gamut variant
convert canonical.png -profile /System/Library/ColorSync/Profiles/Display\ P3.icc wide-gamut.png

# SVG with known viewBox
cat > test.svg <<'SVG'
<?xml version="1.0"?>
<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 200 100" width="200" height="100">
  <rect width="200" height="100" fill="red"/>
</svg>
SVG

# GIF with known canvas (use ImageMagick)
convert -size 300x200 canvas:blue test.gif

# Repeat the orientation pair for .jpg, .webp, .heic, .bmp, .avif as available
```

## Test 1 — Phone-photo regression (P1, SC-001)

Reproduce the production bug, then verify it's fixed.

```bash
# Upload an EXIF-Orientation=6 JPEG with raw bytes 1082×127.
curl -s -X POST http://localhost:4003/internal/file \
  -F "file=@phone-photo-1082x127-orient6.jpg" \
  -F "displayName=avatar.jpg" \
  -F "storageBucketId=$BUCKET" \
  -F "authorizationId=$AUTH" \
  | jq '{imageWidth, imageHeight, externalID}'
```

Expected output:

```json
{
  "imageWidth": 127,
  "imageHeight": 1082,
  "externalID": "<sha3-hash>"
}
```

Confirm the bytes are physically rotated:

```bash
DOC_ID=$(echo "$RESPONSE" | jq -r '.id')
curl -s "http://localhost:4003/rest/storage/document/$DOC_ID" -o canonical.jpg
exiftool canonical.jpg | grep -i orientation || echo "no orientation tag — canonical"
```

## Test 2 — Per-format orientation pairs (US2, SC-002, SC-004)

For each of {PNG, BMP, AVIF}, do both sides:

```bash
# Orientation=1 / absent — must come back byte-identical
sha256sum canonical.png
RESP=$(curl -s -X POST http://localhost:4003/internal/file \
  -F "file=@canonical.png" -F "displayName=x.png" \
  -F "storageBucketId=$BUCKET" -F "authorizationId=$AUTH")
ID=$(echo "$RESP" | jq -r '.id')
curl -s "http://localhost:4003/rest/storage/document/$ID" -o roundtrip.png
sha256sum roundtrip.png  # must equal canonical.png
echo "$RESP" | jq '{imageWidth, imageHeight}'  # dims still reported
```

```bash
# Orientation=6 — must be re-encoded with rotation; orientation tag absent
RESP=$(curl -s -X POST http://localhost:4003/internal/file \
  -F "file=@rotated.png" -F "displayName=x.png" \
  -F "storageBucketId=$BUCKET" -F "authorizationId=$AUTH")
ID=$(echo "$RESP" | jq -r '.id')
curl -s "http://localhost:4003/rest/storage/document/$ID" -o roundtrip-rotated.png
exiftool roundtrip-rotated.png | grep -i orientation || echo "no orientation — canonical"
echo "$RESP" | jq '{imageWidth, imageHeight}'  # post-rotation dims
```

Repeat for JPEG/WebP/HEIC: orientation=1 input → response carries dims (existing path is now also dim-aware); orientation=6 input → dims are post-rotation.

## Test 3 — SVG and GIF dim reporting (SC-014)

```bash
# SVG with viewBox="0 0 200 100"
curl -s -X POST http://localhost:4003/internal/file \
  -F "file=@test.svg" -F "displayName=test.svg" \
  -F "storageBucketId=$BUCKET" -F "authorizationId=$AUTH" \
  | jq '{imageWidth, imageHeight}'
# Expected: {"imageWidth": 200, "imageHeight": 100}

# GIF with 300×200 canvas
curl -s -X POST http://localhost:4003/internal/file \
  -F "file=@test.gif" -F "displayName=test.gif" \
  -F "storageBucketId=$BUCKET" -F "authorizationId=$AUTH" \
  | jq '{imageWidth, imageHeight}'
# Expected: {"imageWidth": 300, "imageHeight": 200}
```

## Test 4 — ICC profile preservation (US3, SC-003)

```bash
# Extract ICC from input
exiftool -ICC_Profile -b wide-gamut.png > input.icc

# Round-trip through file-service
RESP=$(curl -s -X POST http://localhost:4003/internal/file \
  -F "file=@wide-gamut.png" -F "displayName=p3.png" \
  -F "storageBucketId=$BUCKET" -F "authorizationId=$AUTH")
ID=$(echo "$RESP" | jq -r '.id')
curl -s "http://localhost:4003/rest/storage/document/$ID" -o output.png

# Extract ICC from output
exiftool -ICC_Profile -b output.png > output.icc

# Must be byte-identical
diff input.icc output.icc && echo "ICC preserved" || echo "ICC LOST"
```

Repeat with the rotated wide-gamut variant (forces the re-encode path to actually run).

## Test 5 — Deterministic dedup (FR-010, SC-005)

```bash
# Upload the same rotated PNG twice into different buckets
RESP1=$(curl -s -X POST http://localhost:4003/internal/file \
  -F "file=@rotated.png" -F "displayName=a.png" \
  -F "storageBucketId=$BUCKET1" -F "authorizationId=$AUTH1")
RESP2=$(curl -s -X POST http://localhost:4003/internal/file \
  -F "file=@rotated.png" -F "displayName=b.png" \
  -F "storageBucketId=$BUCKET1" -F "authorizationId=$AUTH2")

echo "$RESP1" | jq '{externalID, reused}'
echo "$RESP2" | jq '{externalID, reused}'
# Both responses must show the same externalID; second must show reused=true
```

## Test 6 — Lazy-backfill on legacy row (SC-009)

Seed a "legacy" row by clearing its `content_metadata` directly in the DB, then exercise a metadata-returning endpoint.

```bash
# Pick an existing image row
psql "$DB_URL" -c "SELECT id FROM file WHERE \"mimeType\" LIKE 'image/%' LIMIT 1"

# Force it to legacy state
psql "$DB_URL" -c "UPDATE file SET content_metadata = '{}'::jsonb WHERE id = '<id>'"

# Verify: row has empty metadata
psql "$DB_URL" -c "SELECT content_metadata FROM file WHERE id = '<id>'"
# Expected: {}

# Trigger lazy-backfill via Copy
curl -s -X POST http://localhost:4003/internal/file/copy \
  -H "Content-Type: application/json" \
  -d "{\"sourceId\":\"<id>\",\"destinationBucketId\":\"$BUCKET2\",\"authorizationId\":\"$AUTH3\"}" \
  | jq '{imageWidth, imageHeight}'
# Expected: dims populated

# Verify both source row and copy destination have populated metadata
psql "$DB_URL" -c "SELECT id, content_metadata FROM file WHERE id IN ('<source-id>', '<dest-id>')"
# Expected: both rows have { "imageWidth": ..., "imageHeight": ... }
```

## Test 7 — Lazy-backfill failure modes (SC-010, SC-012, SC-013)

These run as automated tests with fault injection (mock storage adapter, mock DB). Manual repro requires constructing the failure conditions.

### Decode failure (corrupt bytes → sentinel)

```bash
# Seed a row whose stored bytes are corrupt (truncate the file on storage)
truncate -s 100 "$LOCAL_STORAGE_PATH/<externalID>"

# Trigger lazy-backfill
curl -s -X PATCH http://localhost:4003/internal/file/<id> \
  -H "Content-Type: application/json" \
  -d '{"displayName":"foo"}' | jq '{imageWidth, imageHeight}'
# Expected: dims absent

psql "$DB_URL" -c "SELECT content_metadata FROM file WHERE id = '<id>'"
# Expected: {"_decodeFailed": true}

# Subsequent reads do NOT retry — the sentinel short-circuits
```

### Storage.Read transient failure

Use the integration test (`internal/adapter/inbound/http/document_handler_test.go`) with a mock storage adapter that returns an error. Manual repro: rename the file on local storage to simulate "missing":

```bash
mv "$LOCAL_STORAGE_PATH/<externalID>" "$LOCAL_STORAGE_PATH/<externalID>.bak"

curl -s -X PATCH http://localhost:4003/internal/file/<id> \
  -H "Content-Type: application/json" \
  -d '{"displayName":"foo"}' | jq '{imageWidth, imageHeight}'
# Expected: dims absent, request still 200, no sentinel persisted

psql "$DB_URL" -c "SELECT content_metadata FROM file WHERE id = '<id>'"
# Expected: {} (still empty — transient, retry on next read)

# Restore and try again
mv "$LOCAL_STORAGE_PATH/<externalID>.bak" "$LOCAL_STORAGE_PATH/<externalID>"

curl -s -X PATCH http://localhost:4003/internal/file/<id> \
  -H "Content-Type: application/json" \
  -d '{"displayName":"bar"}' | jq '{imageWidth, imageHeight}'
# Expected: dims populated this time
```

## Test 8 — Non-raster passthrough (FR-005)

```bash
# Upload a PDF — response must omit imageWidth/imageHeight
curl -s -X POST http://localhost:4003/internal/file \
  -F "file=@example.pdf" -F "displayName=doc.pdf" \
  -F "storageBucketId=$BUCKET" -F "authorizationId=$AUTH" \
  | jq '{imageWidth, imageHeight}'
# Expected: {"imageWidth": null, "imageHeight": null} (jq prints absent fields as null)

# DB row's content_metadata is empty {} — no decode attempt
```

## Test 9 — Race with PATCH (SC-011)

Concurrent PATCH (which bumps `version`) and lazy-backfill (which doesn't) must both succeed. Hard to reproduce by hand; covered by an integration test using two parallel goroutines + a synchronization point.

## Automated equivalents

The same checks live in:
- `internal/imaging/processor_test.go` — per-format orientation pairs, ICC round-trip, deterministic-bytes assertion, SVG/GIF dim measurement, decode-failure paths.
- `internal/domain/service/file_service_test.go` — `mockProcessor` returns dims; `backfillIfNeeded` helper tests covering all 3 failure modes; race-with-PATCH assertion.
- `internal/adapter/inbound/http/document_handler_test.go` — response-shape assertions for Create/Copy/Replace/PATCH; lazy-backfill end-to-end with mock storage.
- `internal/adapter/outbound/alkemiodb/adapter_test.go` — real-DB integration test for `BackfillContentMetadata` (no version bump, idempotent writes).

Run them with:

```bash
go test -tags vips ./internal/...
golangci-lint run
make openapi  # must produce no diff
```
