package service

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/google/uuid"
	"go.uber.org/zap"

	"github.com/alkem-io/file-service-go/internal/domain/model"
	"github.com/alkem-io/file-service-go/internal/domain/port"
)

// FileService orchestrates file and document operations.
type FileService struct {
	Repo      port.DocumentRepo
	Auth      port.AuthPort
	Storage   port.StoragePort
	Processor port.ImageProcessor
	Logger    *zap.Logger
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

// CreateDocument processes a file, stores it, and creates a document record.
func (s *FileService) CreateDocument(ctx context.Context, input model.CreateDocumentInput, content []byte, declaredMIME string, allowedMimeTypes []string, maxFileSize int) (*model.Document, error) {
	if maxFileSize > 0 && len(content) > maxFileSize {
		return nil, ErrPayloadTooLarge
	}

	mimeType, err := resolveMIME(s.Processor, content, declaredMIME, allowedMimeTypes)
	if err != nil {
		return nil, err
	}

	processed, finalMIME, err := s.Processor.Process(content, mimeType)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrImageProcessing, err)
	}

	stored, err := s.Storage.Save(processed)
	if err != nil {
		return nil, fmt.Errorf("store file: %w", err)
	}

	// Caller opt-out: skip the dedup lookup and force a fresh row insert
	// even when an existing row in this bucket has the same externalID.
	// Used by placeholder flows (e.g., Collabora documents) where two
	// logical documents must NOT share a backing row even if their content
	// hashes collide. The caller-supplied AuthorizationID / TagsetID are
	// honored on the new row.
	if input.SkipDedup {
		return s.insertDocument(ctx, input, stored, finalMIME)
	}

	// Row-level dedup: if a file row already exists for (externalID, storageBucketID),
	// return it as-is. The caller-supplied AuthorizationID / TagsetID are ignored —
	// the existing row's values are authoritative. Caller must use Reused=true to
	// clean up the auth/tagset rows it pre-created.
	existing, err := s.Repo.FindByExternalIDAndBucket(ctx, stored.ExternalID, input.StorageBucketID)
	if err != nil && !errors.Is(err, model.ErrDocumentNotFound) {
		// Lookup failed for a reason we can't diagnose (DB hiccup).
		// Another row may already reference this blob — deleting it would
		// orphan that row, which is worse than orphaning a blob (blobs are
		// GC-able by a periodic sweep). Leave the blob in place.
		return nil, fmt.Errorf("dedup lookup: %w", err)
	}
	if err == nil {
		existing.Reused = true
		return &existing, nil
	}

	return s.insertDocument(ctx, input, stored, finalMIME)
}

// resolveMIME determines the effective MIME type and validates it against the allow-list.
// Empty content: trust declaredMIME (no bytes to detect from).
// Non-empty: detect from content bytes.
func resolveMIME(p port.ImageProcessor, content []byte, declaredMIME string, allowedMimeTypes []string) (string, error) {
	var mimeType string
	if len(content) == 0 {
		mimeType = normalizeMIME(declaredMIME)
		if mimeType == "" {
			mimeType = "application/octet-stream"
		}
	} else {
		mimeType = normalizeMIME(p.DetectMIME(content))
	}

	if len(allowedMimeTypes) > 0 {
		normalized := make([]string, len(allowedMimeTypes))
		for i, m := range allowedMimeTypes {
			normalized[i] = normalizeMIME(m)
		}
		if !slices.Contains(normalized, mimeType) {
			return "", ErrUnsupportedMediaType
		}
	}
	return mimeType, nil
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
func (s *FileService) insertDocument(ctx context.Context, input model.CreateDocumentInput, stored model.StoredFile, finalMIME string) (*model.Document, error) {
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
	}

	_, err = s.Repo.Create(ctx, doc)
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
		if delErr := s.Storage.Delete(deleted.ExternalID); delErr != nil {
			s.Logger.Warn("cleanup: failed to delete orphaned file", zap.String("externalID", deleted.ExternalID), zap.Error(delErr))
		}
	}

	return &deleted, nil
}

// StoreAndLink replaces file content for an existing document atomically.
// Cleans up the old file if no other documents reference it.
//
// Conflict case: if the new content's hash matches another file row already
// in this document's bucket, the unique(externalID, storageBucketID) index
// is violated. This can't be auto-merged without losing distinct document
// identity, so we surface it as ErrConflict (HTTP 409). The caller should
// delete one of the conflicting rows or abort the operation.
func (s *FileService) StoreAndLink(ctx context.Context, documentID uuid.UUID, content []byte) (*model.StoredFile, error) {
	doc, err := s.Repo.GetByID(ctx, documentID)
	if err != nil {
		return nil, err
	}
	oldExternalID := doc.ExternalID

	mimeType := normalizeMIME(s.Processor.DetectMIME(content))
	processed, finalMIME, err := s.Processor.Process(content, mimeType)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrImageProcessing, err)
	}

	stored, err := s.Storage.Save(processed)
	if err != nil {
		return nil, fmt.Errorf("store file: %w", err)
	}

	err = s.Repo.UpdateFile(ctx, documentID, stored.ExternalID, finalMIME, stored.Size)
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
			if delErr := s.Storage.Delete(oldExternalID); delErr != nil {
				s.Logger.Warn("cleanup: failed to delete old file", zap.String("externalID", oldExternalID), zap.Error(delErr))
			}
		}
	}

	return &model.StoredFile{
		ExternalID: stored.ExternalID,
		MimeType:   finalMIME,
		Size:       stored.Size,
	}, nil
}

// UpdateDocumentLocation updates the storage bucket and temporary location flag.
// Uses optimistic locking via the version column to prevent concurrent overwrites.
func (s *FileService) UpdateDocumentLocation(ctx context.Context, documentID uuid.UUID, storageBucketID uuid.UUID, temporaryLocation bool, version int) (*model.Document, error) {
	err := s.Repo.UpdateLocation(ctx, documentID, storageBucketID, temporaryLocation, version)
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
	ErrForbidden            = errors.New("forbidden")
	ErrConflict             = errors.New("conflict: document was modified concurrently")
	ErrPayloadTooLarge      = errors.New("payload too large")
	ErrUnsupportedMediaType = errors.New("unsupported media type")
	ErrImageProcessing      = errors.New("image processing failed")
)

// normalizeMIME strips parameters and lowercases a MIME type.
func normalizeMIME(mimeType string) string {
	return strings.ToLower(strings.TrimSpace(strings.SplitN(mimeType, ";", 2)[0]))
}
