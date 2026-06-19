# Quickstart: 019-preserve-mime-on-edit

## Build & test

```bash
make test          # unit tests (95% coverage target on touched packages)
make lint          # golangci-lint — must be clean (constitution IX)
sqlc generate -f db/sqlc.yaml   # after editing db/queries/document.sql
```

## Reproduce the bug (pre-fix) / verify the fix

```bash
# 1. Create an office document (declared MIME wins for empty content — correct today)
curl -s -X POST localhost:4003/internal/file \
  -F 'displayName=Deck.pptx' \
  -F 'mimeType=application/vnd.openxmlformats-officedocument.presentationml.presentation' \
  -F 'storageBucketId=<bucket>' -F 'allowedMimeTypes=...' -F 'content=@deck.pptx'

# 2. Simulate a Collabora save-back whose zip is reordered:
#    pre-fix: stored mimeType flips to application/zip (the bug)
#    post-fix: 200, mimeType unchanged, metric fallback_generic_sniff++
python3 - <<'EOF'
import zipfile, shutil
shutil.copy('deck.pptx', 'reordered.pptx')
# rewrite zip with [Content_Types].xml NOT first
src = zipfile.ZipFile('deck.pptx'); names = sorted(src.namelist(), reverse=True)
out = zipfile.ZipFile('reordered.pptx', 'w')
for n in names: out.writestr(n, src.read(n))
out.close()
EOF
curl -s -X PUT localhost:4003/internal/file/<id>/content --data-binary @reordered.pptx

# 3. Empty save-back: post-fix → 422 {"code":"EMPTY_CONTENT"}, row untouched
curl -s -X PUT localhost:4003/internal/file/<id>/content --data-binary ''

# 4. Type smuggle: push a real .docx into the pptx doc
#    post-fix → 422 {"code":"MIME_MISMATCH"}, row untouched
curl -s -X PUT localhost:4003/internal/file/<id>/content --data-binary @real.docx
```

## Verify the repair job

```bash
# Seed a corrupted row (dev DB), then restart the service and watch:
psql "$DB_URL" -c "UPDATE file SET \"mimeType\"='application/zip' WHERE id='<id>';"
# on boot: log line mime-repair relabeled … and
curl -s localhost:4003/debug/vars | jq '.mime_repair_total, .content_replace_outcomes_total'
```

## End-to-end (acceptance criteria SC-001)

Edit a .pptx via Collabora in the dev stack (see `docker-compose.yml` +
wopi-service), save, close, reopen — repeat ≥ 3 cycles; the document must reopen
every time and `SELECT "mimeType"` must stay the presentation type throughout.
