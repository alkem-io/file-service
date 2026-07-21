// Package service is the domain core of the file-service: document upload
// (buffered and streaming), content replacement with MIME guarding, copy,
// metadata update, delete with blob refcounting, authorization checks, and
// the lazy content-metadata backfill. It orchestrates everything through the
// port interfaces and knows nothing about HTTP, Postgres, or libvips.
package service

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"expvar"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/google/uuid"
	"go.uber.org/zap"

	"github.com/alkem-io/file-service/internal/domain/model"
	"github.com/alkem-io/file-service/internal/domain/port"
)

// processResultToContentMetadata translates the processor's output into the
// typed ContentMetadata persisted on the row. Decision tree:
//
//   - non-image MIME → Populated=true, no dims (writes "{}" via the adapter,
//     but Populated=true so the lazy-backfill skips this row)
//   - image MIME, Measured=false → Populated=false (no decoder available;
//     lazy-backfill or a future vips run can retry)
//   - image MIME, dims present → Populated=true with dims
//   - image MIME, Measured=true, no dims → Populated=true with DecodeFailed
func processResultToContentMetadata(r port.ProcessResult, mimeType string) model.ContentMetadata {
	if !strings.HasPrefix(mimeType, "image/") {
		// Non-images: nothing to measure. We persist {} but the row is
		// considered "decision recorded" so backfill never touches it.
		return model.ContentMetadata{Populated: false}
	}
	if !r.Measured {
		return model.ContentMetadata{} // Populated=false; lazy-backfill may retry
	}
	if r.ImageWidth != nil && r.ImageHeight != nil {
		return model.ContentMetadata{
			Populated:   true,
			ImageWidth:  r.ImageWidth,
			ImageHeight: r.ImageHeight,
		}
	}
	// Measured ran but no dims → permanent decode failure.
	return model.ContentMetadata{Populated: true, DecodeFailed: true}
}

// FileService orchestrates file and document operations.
type FileService struct {
	Repo      port.DocumentRepo
	Auth      port.AuthPort
	Storage   port.StoragePort
	Processor port.ImageProcessor
	Logger    *zap.Logger
	// Outbox, when non-nil, makes create/replace of a NON-temporary object also commit a
	// backup-outbox row in the same transaction (008-continuous-file-backup FR-001). nil = the
	// producer is off (flag default) and the plain Repo path runs — no behaviour change.
	Outbox port.BackupOutboxRepo
	// HotMimePrefixes marks document/office/Yjs mime types as outbox priority=1 (hot).
	HotMimePrefixes []string
}

// priorityForMime returns the backup-outbox priority for a mime type: 1 (hot) when it carries
// any configured hot prefix — documents/office/Yjs, the low-RPO driver — else 0 (normal).
// MIME types are case-insensitive (RFC 2045), so both sides are lowercased — a mixed-case stored
// or configured value can't silently miss a hot prefix.
func (s *FileService) priorityForMime(mimeType string) int16 {
	m := strings.ToLower(mimeType)
	for _, p := range s.HotMimePrefixes {
		if strings.HasPrefix(m, strings.ToLower(p)) {
			return 1
		}
	}
	return 0
}

// Continuous-backup producer counters (008-continuous-file-backup T011), published on the
// expvar endpoint (/internal/debug/vars) alongside the other file-service metrics. They are declared
// here in the domain core — not in the inbound HTTP metrics file with the rest — because their
// increment sites are here, and the core must not import the inbound adapter (hexagonal
// boundary). expvar.NewInt registers globally, so they still surface on the same endpoint.
var (
	backupOutboxEnqueued = expvar.NewInt("file_backup_outbox_enqueued_total")
	backupOutboxPruned   = expvar.NewInt("file_backup_outbox_pruned_total")
	// Orphan-hygiene deletions are a third producer-side mutation of the outbox depth; count them
	// like the other two so an operator reconciling backlog from /internal/debug/vars (enqueued −
	// pruned − orphaned) stays accurate and a hygiene-firing spike is visible, not silent.
	backupOutboxOrphaned = expvar.NewInt("file_backup_outbox_orphaned_total")
)

// writeCreate persists a new document — via the transactional outbox path (a backup-outbox row
// in the same commit, FR-001) when the producer is on and the object is non-temporary, else the
// original non-transactional Repo.Create. Both return model.ErrDuplicateKey on the dedup race.
func (s *FileService) writeCreate(ctx context.Context, doc model.Document, meta model.ContentMetadata) error {
	if s.Outbox != nil && !doc.TemporaryLocation {
		_, err := s.Outbox.CreateWithOutbox(ctx, doc, meta, s.priorityForMime(doc.MimeType))
		if err == nil {
			backupOutboxEnqueued.Add(1)
		}
		return err
	}
	_, err := s.Repo.Create(ctx, doc, meta)
	return err
}

// writeReplace persists replaced content — via the transactional outbox path (enqueue the new
// content hash in the same commit) when the producer is on and the document is non-temporary,
// else the original Repo.UpdateFile. Same error semantics either way.
func (s *FileService) writeReplace(ctx context.Context, doc model.Document, documentID uuid.UUID, externalID, mimeType string, size int, meta model.ContentMetadata) error {
	if s.Outbox != nil && !doc.TemporaryLocation {
		err := s.Outbox.UpdateFileWithOutbox(ctx, documentID, externalID, mimeType, size, meta, s.priorityForMime(mimeType))
		if err == nil {
			backupOutboxEnqueued.Add(1)
		}
		return err
	}
	return s.Repo.UpdateFile(ctx, documentID, externalID, mimeType, size, meta)
}

// PruneBackupOutbox drops consumer-finished outbox rows older than `retention`, keeping the
// shared outbox bounded (SC-008). A no-op (0, nil) when the producer is off.
func (s *FileService) PruneBackupOutbox(ctx context.Context, retention time.Duration) (int64, error) {
	if s.Outbox == nil {
		return 0, nil
	}
	n, err := s.Outbox.PruneBackupOutbox(ctx, time.Now().Add(-retention))
	if err == nil && n > 0 {
		backupOutboxPruned.Add(n)
	}
	return n, err
}

// ServeResult contains file content and metadata for serving.
type ServeResult struct {
	Content  []byte
	MimeType string
	Document model.Document
}

// ServeFile looks up a document, checks authorization, and reads the file.
func (s *FileService) ServeFile(ctx context.Context, documentID uuid.UUID, actorID string) (*ServeResult, error) {
	doc, err := s.Repo.GetByID(ctx, documentID)
	if err != nil {
		return nil, err
	}

	result, err := s.Auth.CheckPrivilege(ctx, actorID, "read", doc.AuthorizationID.String())
	if err != nil {
		return nil, fmt.Errorf("authorization check: %w", err)
	}
	if !result.Allowed {
		return nil, ErrForbidden
	}

	content, err := s.Storage.Read(doc.ExternalID)
	if err != nil {
		return nil, fmt.Errorf("read file: %w", err)
	}

	return &ServeResult{
		Content:  content,
		MimeType: doc.MimeType,
		Document: doc,
	}, nil
}

// CreateDocument ingests a complete buffer through the streaming pipeline
// (spec 020): stage → validate → publish. Kept for buffer-shaped callers;
// the HTTP handler streams the request directly via StageUpload +
// CompleteUpload. One pipeline, no buffered twin (constitution X).
//
// Note: the buffered implementation rejected over-limit/disallowed uploads
// before any storage work; the pipeline rejects them before *publish*
// (validation order is a documented consequence of the multipart field
// order, research R4). Observable outcomes are identical.
func (s *FileService) CreateDocument(ctx context.Context, input model.CreateDocumentInput, content []byte, declaredMIME string, allowedMimeTypes []string, maxFileSize int) (*model.Document, error) {
	su, err := s.StageUpload(ctx, bytes.NewReader(content), declaredMIME)
	if err != nil {
		return nil, err
	}
	doc, err := s.CompleteUpload(ctx, su, input, allowedMimeTypes, maxFileSize)
	if err != nil {
		su.Discard()
		return nil, err
	}
	return doc, nil
}

// findDedupDocument looks up an existing file row for (externalID, bucketID).
// Returns (doc, true, nil) on hit, (nil, false, nil) when no row exists, and
// (nil, false, err) on lookup failure.
func (s *FileService) findDedupDocument(ctx context.Context, externalID string, bucketID uuid.UUID) (*model.Document, bool, error) {
	existing, err := s.Repo.FindByExternalIDAndBucket(ctx, externalID, bucketID)
	if err != nil {
		if errors.Is(err, model.ErrDocumentNotFound) {
			return nil, false, nil
		}
		return nil, false, fmt.Errorf("dedup lookup: %w", err)
	}
	return &existing, true, nil
}

// CopyDocument creates a new file row in another bucket that references the
// same content as an existing source row. No bytes are moved or re-uploaded:
// content is content-addressed by hash, so two rows in different buckets
// can share underlying storage. The source row is not modified.
//
// Per-bucket dedup applies by default (skip with SkipDedup): if the
// destination bucket already has a row with the same externalID, return
// it as-is with Reused=true, matching the CreateDocument contract.
func (s *FileService) CopyDocument(ctx context.Context, sourceID uuid.UUID, input model.CopyDocumentInput) (*model.Document, error) {
	source, err := s.Repo.GetByID(ctx, sourceID)
	if err != nil {
		// ErrDocumentNotFound surfaces as 404 in the handler.
		return nil, err
	}

	// Lazy-backfill the source row when it's a legacy image with empty
	// content_metadata so the copy carries dims to the destination row
	// (FR-014, FR-018, SC-007 + SC-009).
	s.backfillIfNeeded(ctx, &source)

	if !input.SkipDedup {
		existing, found, err := s.findDedupDocument(ctx, source.ExternalID, input.DestinationBucketID)
		if err != nil {
			return nil, err
		}
		if found {
			existing.Reused = true
			// The destination row may itself be legacy; backfill so the
			// response carries dims for it too.
			return s.backfillIfNeeded(ctx, existing), nil
		}
	}

	// Reuse insertDocument so race-handling, error mapping (ErrDuplicateKey →
	// ErrConflict on SkipDedup, race re-query otherwise), and audit fields
	// stay identical to CreateDocument.
	createInput := model.CreateDocumentInput{
		DisplayName:       source.DisplayName,
		CreatedBy:         input.CreatedBy,
		TemporaryLocation: false,
		StorageBucketID:   input.DestinationBucketID,
		AuthorizationID:   input.AuthorizationID,
		TagsetID:          input.TagsetID,
		SkipDedup:         input.SkipDedup,
	}
	stored := model.StoredFile{
		ExternalID: source.ExternalID,
		MimeType:   source.MimeType,
		Size:       source.Size,
	}
	// Propagate the source's content_metadata verbatim — copy doesn't
	// re-run Process, so dims, the {_decodeFailed:true} sentinel, and any
	// forward-fit per-content-type fields must ride along from the source
	// row. Source has been backfilled above when it was a legacy image row
	// (FR-014).
	contentMetadata := source.ContentMetadata
	return s.insertDocument(ctx, createInput, stored, source.MimeType, contentMetadata, source.ImageWidth, source.ImageHeight)
}

// MimeMismatchError reports a content replacement whose detected type is
// unambiguously different from the document's stored type (FR-004). It
// matches ErrMimeMismatch via errors.Is.
type MimeMismatchError struct {
	Known    string
	Detected string
}

func (e *MimeMismatchError) Error() string {
	return fmt.Sprintf("content type %q does not match the document's stored type %q", e.Detected, e.Known)
}

// Is makes every MimeMismatchError match the ErrMimeMismatch sentinel, so
// callers can branch with errors.Is and only reach for errors.As when they
// need the concrete MIME pair.
func (e *MimeMismatchError) Is(target error) bool { return target == ErrMimeMismatch }

// Replace outcomes, persisted on model.StoredFile.ReplaceOutcome so the HTTP
// adapter can count them (content_replace_outcomes_total) without the domain
// importing adapter metrics.
const (
	ReplaceOutcomeAccepted         = "accepted"
	ReplaceOutcomeFallback         = "fallback_generic_sniff"
	ReplaceOutcomeRejectedEmpty    = "rejected_empty"
	ReplaceOutcomeRejectedMismatch = "rejected_mismatch"
)

// reconcileReplaceMIME decides the MIME type to persist when replacing a
// document's content. Unlike resolveMIME (create path), the question here is
// not "what is this content?" but "is this content compatible with the type
// this document already has?" — the stored type is authoritative, content
// detection is only a guard (FR-001..004):
//
//	empty content              → ErrEmptyContent (a valid save is never 0 bytes)
//	known type empty/generic   → accept the sniff (legacy/corrupted rows may
//	                             self-heal to a concrete type, never downgrade
//	                             below what they already are)
//	sniff generic              → keep the known type (container formats and
//	                             degenerate bodies can't carry their identity)
//	sniff == known             → keep the known type
//	concrete sniff ≠ known     → MimeMismatchError (silent relabeling forbidden)
func (s *FileService) reconcileReplaceMIME(knownMIME string, content []byte) (mimeType, detected, outcome string, err error) {
	if len(content) == 0 {
		return "", "", ReplaceOutcomeRejectedEmpty, ErrEmptyContent
	}
	known := normalizeMIME(knownMIME)
	detected = normalizeMIME(s.Processor.DetectMIME(content))

	switch {
	case known == "" || model.IsGenericMIME(known):
		// No trustworthy stored type to defend; behave like before.
		return detected, detected, ReplaceOutcomeAccepted, nil
	case model.IsGenericMIME(detected):
		return known, detected, ReplaceOutcomeFallback, nil
	case detected == known:
		return known, detected, ReplaceOutcomeAccepted, nil
	default:
		return "", detected, ReplaceOutcomeRejectedMismatch, &MimeMismatchError{Known: known, Detected: detected}
	}
}

// insertDocument builds the Document, attempts a Create, and handles the
// unique-key race by re-querying and returning the concurrent winner.
//
// Blob cleanup policy: once Storage.Save has published a blob, we never
// delete it on subsequent failures. externalID is global content identity,
// but the dedup rule is per-(externalID, storageBucketID), so another
// concurrent request in a different bucket may have already linked this
// same blob to its own document row before our error path runs. Deleting
// here would orphan that row — a permanent 404 for its users. Orphan blobs
// left behind are recoverable via a periodic GC sweep.
func (s *FileService) insertDocument(ctx context.Context, input model.CreateDocumentInput, stored model.StoredFile, finalMIME string, contentMetadata model.ContentMetadata, imageWidth, imageHeight *int) (*model.Document, error) {
	now := time.Now()
	docID, err := uuid.NewV7()
	if err != nil {
		return nil, fmt.Errorf("generate UUIDv7: %w", err)
	}

	doc := model.Document{
		ID:                docID,
		ExternalID:        stored.ExternalID,
		MimeType:          finalMIME,
		Size:              stored.Size,
		DisplayName:       input.DisplayName,
		CreatedBy:         input.CreatedBy,
		TemporaryLocation: input.TemporaryLocation,
		StorageBucketID:   input.StorageBucketID,
		AuthorizationID:   input.AuthorizationID,
		TagsetID:          input.TagsetID,
		CreatedDate:       now,
		UpdatedDate:       now,
		ContentMetadata:   contentMetadata,
		ImageWidth:        imageWidth,
		ImageHeight:       imageHeight,
	}

	err = s.writeCreate(ctx, doc, contentMetadata)
	if err == nil {
		return &doc, nil
	}

	if errors.Is(err, model.ErrDuplicateKey) {
		// SkipDedup means the caller explicitly asked for a fresh row. If the
		// schema enforces unique(externalID, storageBucketID), we can't honor
		// that intent — surface as ErrConflict rather than masquerading as a
		// dedup hit (which would silently corrupt placeholder flows).
		if input.SkipDedup {
			return nil, ErrConflict
		}
		// Default path: another concurrent creator won the race on the
		// unique(externalID, storageBucketID) index. Re-query and return
		// the winner with Reused=true.
		raced, findErr := s.Repo.FindByExternalIDAndBucket(ctx, stored.ExternalID, input.StorageBucketID)
		if findErr == nil {
			raced.Reused = true
			return &raced, nil
		}
		s.Logger.Warn("dedup: unique violation but re-query failed",
			zap.String("externalID", stored.ExternalID), zap.Error(findErr))
		return nil, fmt.Errorf("create document record: duplicate key, winner lookup failed: %w", findErr)
	}
	return nil, fmt.Errorf("create document record: %w", err)
}

// DeleteDocument removes a document record and its file (if not shared).
// Counts remaining references AFTER delete to avoid TOCTOU race.
func (s *FileService) DeleteDocument(ctx context.Context, documentID uuid.UUID) (*model.DeletedDocument, error) {
	// Delete the row first — returns externalID for post-delete cleanup
	deleted, err := s.Repo.Delete(ctx, documentID)
	if err != nil {
		return nil, err
	}

	// Count remaining references AFTER delete (not before)
	// This eliminates the TOCTOU race where two concurrent deletes
	// both see count > 1 and skip file cleanup
	count, err := s.Repo.CountByExternalID(ctx, deleted.ExternalID)
	if err != nil {
		s.Logger.Warn("cleanup: failed to count remaining references after delete",
			zap.String("externalID", deleted.ExternalID), zap.Error(err))
		return &deleted, nil
	}

	// Only delete file if no remaining documents reference it
	if count == 0 {
		s.cleanupOrphanedBlob(ctx, documentID, deleted.ExternalID)
	}

	return &deleted, nil
}

// cleanupOrphanedBlob removes a blob whose last referencing file row is gone (refcount→0), and —
// when the backup producer is on — drops fileID's own still-pending backup-outbox hint for that
// hash: it points at bytes that no longer exist, so the consumer's fetch could only ever 404
// (file-service is the master of orphans; it deleted the blob, so it removes the dead hint too).
// The outbox delete is scoped and guarded so it can only ever remove a genuinely-dead hint —
// never another document's, nor a live re-enqueued one from a same-document A→B→A replace (the
// exact (fileID, externalID) + NOT EXISTS predicate and its reasoning live in the query,
// DeleteBackupOutboxPendingForFile). Two residuals remain, both benign: a sibling document that
// shared the hash and was deleted earlier leaves a stale pending row until the consumer's 404→skip
// backstop retires it; and the pre-existing count→Storage.Delete blob-GC race (a concurrent create
// re-referencing the blob) is unchanged by this PR — its proper fix is atomic GC. Both steps are
// best-effort warn-only cleanup: a leftover blob is GC-able, and a leftover pending row is caught
// by the consumer's own 404→skip backstop. The outbox cleanup runs only after a successful blob
// delete — while the blob exists, pending rows are still backable.
func (s *FileService) cleanupOrphanedBlob(ctx context.Context, fileID uuid.UUID, externalID string) {
	if err := s.Storage.Delete(externalID); err != nil {
		s.Logger.Warn("cleanup: failed to delete orphaned file", zap.String("externalID", externalID), zap.Error(err))
		return
	}
	if s.Outbox == nil {
		return
	}
	if n, err := s.Outbox.DeletePendingForFile(ctx, fileID, externalID); err != nil {
		s.Logger.Warn("cleanup: failed to drop pending outbox row for deleted blob",
			zap.String("fileID", fileID.String()), zap.String("externalID", externalID), zap.Error(err))
	} else if n > 0 {
		backupOutboxOrphaned.Add(n)
		s.Logger.Info("cleanup: dropped pending outbox row for deleted blob",
			zap.String("fileID", fileID.String()), zap.String("externalID", externalID), zap.Int64("rows", n))
	}
}

// StoreAndLink replaces file content for an existing document atomically.
// Cleans up the old file if no other documents reference it.
//
// Conflict case: if the new content's hash matches another file row already
// in this document's bucket, the unique(externalID, storageBucketID) index
// is violated. This can't be auto-merged without losing distinct document
// identity, so we surface it as ErrConflict (HTTP 409). The caller should
// delete one of the conflicting rows or abort the operation.
// StoreAndLink stores new content for an existing document from a complete
// buffer; delegates to the streaming pipeline (spec 020).
func (s *FileService) StoreAndLink(ctx context.Context, documentID uuid.UUID, content []byte) (*model.StoredFile, error) {
	return s.StoreAndLinkStream(ctx, documentID, bytes.NewReader(content))
}

// StoreAndLinkStream replaces a document's content from a one-pass stream
// (spec 020 US3) while preserving every 019 semantic: the stored type is
// authoritative (the sniff — now on the bounded prefix — is only a guard),
// rejections happen before any storage side effect, and the outcome matrix
// is unchanged.
func (s *FileService) StoreAndLinkStream(ctx context.Context, documentID uuid.UUID, r io.Reader) (*model.StoredFile, error) {
	doc, err := s.Repo.GetByID(ctx, documentID)
	if err != nil {
		return nil, err
	}
	oldExternalID := doc.ExternalID

	br := bufio.NewReaderSize(r, sniffPrefixSize)
	prefix, perr := br.Peek(sniffPrefixSize)
	if perr != nil && !errors.Is(perr, io.EOF) && !errors.Is(perr, bufio.ErrBufferFull) {
		s.logIngest("replace prefix read failed", normalizeMIME(doc.MimeType), int64(len(prefix)), perr)
		return nil, fmt.Errorf("read replacement prefix: %w", perr)
	}

	// The stored type is authoritative across content edits (FR-001); the
	// content sniff is a guard, never the source of truth. All validation
	// happens before the stage opens, so a rejection has zero side effects
	// (FR-007). Empty content = empty prefix (019 FR-003a unchanged).
	mimeType, detected, outcome, err := s.reconcileReplaceMIME(doc.MimeType, prefix)
	if err != nil {
		s.Logger.Warn("content replace rejected",
			zap.String("documentID", documentID.String()),
			zap.String("knownMime", normalizeMIME(doc.MimeType)),
			zap.String("detectedMime", detected),
			zap.String("outcome", outcome),
			zap.Error(err))
		return nil, err
	}
	if outcome == ReplaceOutcomeFallback {
		s.Logger.Info("content replace: generic sniff, keeping stored type",
			zap.String("documentID", documentID.String()),
			zap.String("knownMime", mimeType),
			zap.String("detectedMime", detected),
			zap.String("outcome", outcome))
	}

	su, err := s.stageContent(ctx, br, mimeType, len(prefix) > 0)
	if err != nil {
		return nil, err
	}

	stored, err := su.stage.Commit()
	if err != nil {
		su.Discard()
		s.logIngest("replace stage commit failed", su.MimeType, su.Size, err)
		return nil, fmt.Errorf("store file: %w", err)
	}
	su.done = true

	// Persist su.MimeType (the transcode output), not the reconciled
	// mimeType: the streaming transcode may canonicalize the encoding
	// (HEIC/WebP → JPEG) and the staged bytes ARE that new format. The two
	// values are identical on every other path (office and non-image types
	// pass through), and stored types are always post-canonicalization, so
	// the type-stability invariant (FR-001/FR-005) is preserved: a stored
	// type can never regress to a generic or mismatched value here.
	result := port.ProcessResult{
		MimeType:    su.MimeType,
		ImageWidth:  su.ImageWidth,
		ImageHeight: su.ImageHeight,
		Measured:    su.Measured,
	}
	contentMetadata := processResultToContentMetadata(result, su.MimeType)
	err = s.writeReplace(ctx, doc, documentID, stored.ExternalID, su.MimeType, stored.Size, contentMetadata)
	if err != nil {
		// Never delete the newly-stored blob on failure: another concurrent
		// request in any bucket may have already linked this externalID
		// (storage dedup is global, index is per-bucket). Orphan blobs are
		// GC'able; orphan rows are permanent 404s.
		if errors.Is(err, model.ErrDuplicateKey) {
			return nil, ErrConflict
		}
		return nil, fmt.Errorf("update document record: %w", err)
	}

	// Clean up old file if content changed and no other documents reference it
	if oldExternalID != stored.ExternalID {
		count, countErr := s.Repo.CountByExternalID(ctx, oldExternalID)
		if countErr != nil {
			s.Logger.Warn("cleanup: failed to count references for old file",
				zap.String("externalID", oldExternalID), zap.Error(countErr))
		} else if count == 0 {
			s.cleanupOrphanedBlob(ctx, documentID, oldExternalID)
		}
	}

	return &model.StoredFile{
		ExternalID:     stored.ExternalID,
		MimeType:       su.MimeType,
		Size:           stored.Size,
		ImageWidth:     su.ImageWidth,
		ImageHeight:    su.ImageHeight,
		ReplaceOutcome: outcome,
	}, nil
}

// UpdateDocumentMetadata updates the mutable metadata fields (storage bucket,
// temporary-location flag, display name) atomically. The handler reads the
// current row first and fills any fields the caller didn't supply, so this
// method always overwrites all three columns. Uses optimistic locking via
// the version column.
//
// mimeType, externalID, and size are not mutable through this method — they
// change only via StoreAndLink (replace content). Callers that rename a
// document are responsible for keeping displayName's extension consistent
// with the (immutable) mimeType; this service does not enforce extension
// matching.
func (s *FileService) UpdateDocumentMetadata(ctx context.Context, documentID uuid.UUID, storageBucketID uuid.UUID, temporaryLocation bool, displayName string, version int) (*model.Document, error) {
	err := s.Repo.UpdateMetadata(ctx, documentID, storageBucketID, temporaryLocation, displayName, version)
	if err != nil {
		// Version mismatch returns ErrDocumentNotFound (0 rows); translate to ErrConflict
		if errors.Is(err, model.ErrDocumentNotFound) {
			return nil, ErrConflict
		}
		return nil, err
	}
	doc, err := s.Repo.GetByID(ctx, documentID)
	if err != nil {
		return nil, err
	}
	return &doc, nil
}

var (
	// ErrForbidden is returned when the auth evaluation explicitly denies
	// the actor the required privilege. An evaluation that could not run at
	// all surfaces as a wrapped transport error instead.
	ErrForbidden = errors.New("forbidden")
	// ErrConflict covers both flavors of write conflict: an optimistic-lock
	// version mismatch on metadata updates, and a content-hash collision
	// when SkipDedup forbids reusing the existing row.
	ErrConflict = errors.New("conflict: document was modified concurrently")
	// ErrPayloadTooLarge rejects an upload exceeding the bucket's maxFileSize
	// policy (distinct from the transport-level ErrOverLimit cap).
	ErrPayloadTooLarge = errors.New("payload too large")
	// ErrUnsupportedMediaType rejects an upload whose detected MIME type is
	// not in the bucket's allowedMimeTypes policy.
	ErrUnsupportedMediaType = errors.New("unsupported media type")
	// ErrImageProcessing wraps a canonicalization failure for content that
	// claims to be an image but cannot be processed as one.
	ErrImageProcessing = errors.New("image processing failed")

	// ErrEmptyContent rejects 0-byte content replacement: a valid office file
	// is never empty, so an empty body always signals a failed save (FR-003a).
	ErrEmptyContent = errors.New("empty content: replacement bodies must not be 0 bytes")

	// ErrMimeMismatch is the sentinel for errors.Is matching; the concrete
	// error carrying the MIME pair is MimeMismatchError (errors.As).
	ErrMimeMismatch = errors.New("content type does not match the document's stored type")
)

// normalizeMIME strips parameters and lowercases a MIME type.
func normalizeMIME(mimeType string) string {
	return model.NormalizeMIME(mimeType)
}

// BackfillIfNeeded is the public entry point for handlers that read a
// document and want lazy-backfill applied before rendering. Wraps the
// internal helper so the PATCH handler doesn't have to duplicate the
// decision tree. Best-effort, never returns an error (FR-020).
func (s *FileService) BackfillIfNeeded(ctx context.Context, doc *model.Document) *model.Document {
	return s.backfillIfNeeded(ctx, doc)
}

// backfillIfNeeded is the lazy-backfill helper for legacy image rows whose
// content_metadata is empty. Called from the Create dedup-hit branch (and
// from Copy + PATCH paths). Best-effort: never returns an error — backfill
// failures don't fail the underlying request.
//
// Decision tree:
//   - non-image MIME → return doc unchanged (no metadata work for non-images)
//   - ContentMetadata.Populated=true → already decided (measured OR
//     {_decodeFailed:true} sentinel); return unchanged so persisted decode-
//     failure rows short-circuit instead of looping decode attempts
//   - Storage.Read fails → log warn, return unchanged (transient retry next read)
//   - MeasureDims returns dims → persist via BackfillContentMetadata
//     (compare-and-set on externalID), populate doc.ImageWidth/Height; on
//     persist error or race-lost log debug AND still populate doc so the
//     response carries the just-measured dims (FR-020 case c)
//   - MeasureDims returns err → persist {_decodeFailed:true} best-effort;
//     leave doc unchanged (response omits dims, FR-019)
//   - MeasureDims returns (nil, nil, nil) (no decoder available, !vips stub) →
//     skip persist, leave doc unchanged so a future vips run retries
func (s *FileService) backfillIfNeeded(ctx context.Context, doc *model.Document) *model.Document {
	if doc == nil {
		return doc
	}
	if !strings.HasPrefix(doc.MimeType, "image/") {
		return doc
	}
	if doc.ContentMetadata.Populated {
		// Either dims already measured or {_decodeFailed:true} persisted —
		// nothing to do. Persisted sentinels short-circuit here instead of
		// re-running the decoder on every metadata-returning request.
		return doc
	}

	content, err := s.Storage.Read(doc.ExternalID)
	if err != nil {
		s.Logger.Warn("backfill: storage read failed; leaving content_metadata empty",
			zap.String("externalID", doc.ExternalID), zap.Error(err))
		return doc
	}

	w, h, measureErr := s.Processor.MeasureDims(content, doc.MimeType)
	switch {
	case measureErr != nil:
		// Decoder ran and failed → write the {_decodeFailed: true} sentinel.
		// Persist best-effort. Doc stays without dims; the response simply
		// omits imageWidth/imageHeight.
		sentinel := model.ContentMetadata{Populated: true, DecodeFailed: true}
		if persistErr := s.Repo.BackfillContentMetadata(ctx, doc.ID, doc.ExternalID, sentinel); persistErr != nil {
			s.Logger.Warn("backfill: persist of _decodeFailed sentinel failed",
				zap.String("documentID", doc.ID.String()), zap.Error(persistErr))
		}
		return doc
	case w != nil && h != nil:
		measured := model.ContentMetadata{Populated: true, ImageWidth: w, ImageHeight: h}
		// Always populate the doc with the just-computed dims, even if
		// persistence fails or the compare-and-set lost a race (FR-020
		// case c). The dims are accurate for the bytes the request decoded.
		doc.ImageWidth = w
		doc.ImageHeight = h
		doc.ContentMetadata = measured
		if persistErr := s.Repo.BackfillContentMetadata(ctx, doc.ID, doc.ExternalID, measured); persistErr != nil {
			s.Logger.Warn("backfill: persist of measured dims failed; response still carries dims",
				zap.String("documentID", doc.ID.String()), zap.Error(persistErr))
		}
		return doc
	default:
		// (nil, nil, nil) — no decoder available (no-vips stub). Skip persist
		// so a future vips run retries. Doc stays without dims.
		return doc
	}
}
