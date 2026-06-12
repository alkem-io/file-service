# Data Model: Preserve Document MIME Type Across Content Edits

**No schema changes.** The feature operates entirely on the existing `file` table
(see `db/schema/document.sql`) and content-addressed blob storage.

> **Terminology**: the PostgreSQL table is named `file`; the domain model and HTTP
> surface call the same record a **Document** (`model.Document`,
> `/internal/file/{id}`). Pre-existing repo convention — both names refer to one
> entity throughout these artifacts.

## Entities

### Document (existing — `file` table)

| Field | Type | Role in this feature |
|---|---|---|
| `id` | UUID (v7) | identity |
| `displayName` | VARCHAR(512) | carries the office extension; repair-job evidence and WOPI `BaseFileName` |
| `mimeType` | VARCHAR(128) | **the protected field** — authoritative type, set at creation, stable across edits |
| `externalID` | VARCHAR(128) | content hash (blob key); changes on successful replace |
| `size` | INTEGER | 0 ⇒ empty content; repair-job recoverability signal |
| `content_metadata` | JSONB | refreshed on successful replace (unchanged behavior) |
| `storageBucketId` | UUID | bucket linkage; policy itself is server-owned and not locally readable (research R3) |

### MIME vocabulary (new — `internal/domain/model/mime.go`)

- **GenericMIMEs** (set): `application/zip`, `application/octet-stream`, `text/plain`
  — sniff results that carry no trustworthy type information.
- **OfficeExtToMIME** (map): `.docx/.xlsx/.pptx/.odt/.ods/.odp` → canonical MIME.
  Single source of truth (constitution VIII); used by the repair job.

## State transitions

### `mimeType` on content replace (the invariant)

```
                       ┌──────────────────────────────────────────────┐
   replace(content) →  │ content empty?          → REJECT (422)       │  mimeType unchanged
                       │ sniff ∈ GenericMIMEs    → ACCEPT, keep known │  mimeType unchanged
                       │ sniff == known          → ACCEPT             │  mimeType unchanged
                       │ sniff concrete ≠ known  → REJECT (422)       │  mimeType unchanged
                       └──────────────────────────────────────────────┘
```

**Invariant**: after this feature, no code path mutates `mimeType` post-creation
except the repair job's corrective relabel. (Today `StoreAndLink` violates this.)

### Repair-job row lifecycle

```
suspect (mimeType ∈ GenericMIMEs ∧ displayName has office ext)
   ├─ blob non-empty ∧ zip magic  → relabeled (mimeType := OfficeExtToMIME[ext])
   ├─ blob empty                  → unrecoverable (logged + counted; row untouched)
   └─ blob fails zip check        → skipped (logged; row untouched — genuinely not
                                    an office file despite its name)
```

Relabeled rows leave the suspect set ⇒ the job is idempotent across boots.

## Repository port additions

| Method | Query | Notes |
|---|---|---|
| `ListByMimeTypes(ctx, mimeTypes)` | `SELECT id, "externalID", "mimeType", size, "displayName" FROM file WHERE "mimeType" = ANY($1::text[])` | sqlc-generated; office-extension filtering happens in the domain via `OfficeMIMEForName` so the office vocabulary stays single-sourced (shipped delta vs the original SQL-regex design) |
| `UpdateMimeType(ctx, id, expectedExternalID, mimeType) (bool, error)` | compare-and-set: `UPDATE … SET "mimeType" = $2, "updatedDate" = now() WHERE id = $1 AND "externalID" = $4` | CAS guard added in review (PR #29): a Replace landing between the repair scan and the relabel already wrote the correct type for the *new* content — the stale-blob relabel must lose that race silently (false return = skip, not error) |

## Validation rules (from FRs)

- FR-002/003: generic sniff never overwrites a concrete stored type.
- FR-003a: `len(content) == 0` on replace → reject before any side effect.
- FR-004: concrete sniff ≠ stored type → reject before any side effect.
- FR-007: rejection occurs before `Storage.Save`; the existing
  Save → UpdateFile → cleanup ordering preserves the old row on mid-flight failure.
- FR-006: repair relabels only content-verified office zips; empty rows are reported,
  never relabeled (relabeling an empty blob would create an openable-but-blank lie).
