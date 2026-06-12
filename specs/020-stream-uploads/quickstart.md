# Quickstart: 020-stream-uploads

## Build & test

```bash
go mod tidy                  # after the fork replace directive lands
make test                    # unit (no vips)
make test-vips               # full suite incl. streaming transcode
make lint
make openapi                 # after handler/DTO changes
```

## Verify the memory budget (SC-001)

```bash
# 300 MiB random file (raise MAX_UPLOAD_SIZE for the dev run)
head -c 314572800 /dev/urandom > big.bin
MAX_UPLOAD_SIZE=1073741824 ./bin/server &
curl -sf -X POST localhost:4003/internal/file \
  -F 'file=@big.bin' -F 'displayName=big.bin' -F 'storageBucketId=<b>' \
  -F 'authorizationId=<a>' -F 'allowedMimeTypes=application/octet-stream'
# watch RSS while uploading; assert ≤ baseline + fixed budget
ps -o rss= -p $(pgrep -f bin/server)
```

Equivalence: hash of stored blob == `sha3-256` of `big.bin`; `size` matches;
re-upload dedups (`reused=true`).

## Verify streaming transcode (SC-002 amended)

```bash
# HEIC → JPEG, dims recorded, output == buffered ExportJpeg(Q82, baseline)
curl -sf -X POST ... -F 'file=@photo.heic;type=image/heic' ...
# pixel bomb: header says 30000x30000 → 422 PIXEL_BUDGET_EXCEEDED, no decode
# rotated JPEG (EXIF 6): materializes (scratch if > threshold), dims swapped
```

## Verify guards

```bash
# over-limit: cut off promptly, no artifact
head -c 40000000 /dev/urandom | curl -sf -X POST ... # default 32 MiB cap → 413
ls storage/ | grep tmp   # nothing staged left behind
# stall: open a connection, send headers + 1 KiB, sleep 40s → connection
# aborted, ingest_outcomes_total.stalled++ in /debug/vars
# client abort: kill curl mid-upload → client_abort++, no staging artifact
```

## Verify 019 preservation (SC-006)

```bash
go test ./internal/domain/service/ -run 'StoreAndLink|Reconcile|MimeRepair'
go test ./internal/adapter/inbound/http/ -run 'ReplaceContent'
```

## Stacked-branch workflow reminder

Branch sits on `019-preserve-mime-on-edit`. After PR #29 merges:
`git rebase --onto origin/develop 019-preserve-mime-on-edit 020-stream-uploads`,
re-run gates, retarget the PR to `develop`.
