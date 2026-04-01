package service

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/alkem-io/file-service-go/internal/domain/model"
	"github.com/alkem-io/file-service-go/internal/domain/port"
)

// FileService orchestrates file and document operations.
type FileService struct {
	Repo      port.DocumentRepo
	Auth      port.AuthPort
	Storage   port.StoragePort
	Processor port.ImageProcessor
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
func (s *FileService) CreateDocument(ctx context.Context, input model.CreateDocumentInput, content []byte, allowedMimeTypes []string, maxFileSize int) (*model.Document, error) {
	if maxFileSize > 0 && len(content) > maxFileSize {
		return nil, ErrPayloadTooLarge
	}

	mimeType := strings.ToLower(strings.SplitN(s.Processor.DetectMIME(content), ";", 2)[0])

	if len(allowedMimeTypes) > 0 {
		normalized := make([]string, len(allowedMimeTypes))
		for i, m := range allowedMimeTypes {
			normalized[i] = strings.ToLower(strings.SplitN(m, ";", 2)[0])
		}
		if !contains(normalized, mimeType) {
			return nil, ErrUnsupportedMediaType
		}
	}

	processed, finalMIME, err := s.Processor.Process(content, mimeType)
	if err != nil {
		return nil, fmt.Errorf("image processing: %w", err)
	}

	stored, err := s.Storage.Save(processed)
	if err != nil {
		return nil, fmt.Errorf("store file: %w", err)
	}

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
	if err != nil {
		// Cleanup: remove stored file if DB insert fails
		_ = s.Storage.Delete(stored.ExternalID)
		return nil, fmt.Errorf("create document record: %w", err)
	}

	return &doc, nil
}

// DeleteDocument removes a document record and its file (if not shared).
func (s *FileService) DeleteDocument(ctx context.Context, documentID uuid.UUID) (*model.DeletedDocument, error) {
	doc, err := s.Repo.GetByID(ctx, documentID)
	if err != nil {
		return nil, err
	}

	count, err := s.Repo.CountByExternalID(ctx, doc.ExternalID)
	if err != nil {
		return nil, fmt.Errorf("count references: %w", err)
	}

	deleted, err := s.Repo.Delete(ctx, documentID)
	if err != nil {
		return nil, err
	}

	// Only delete file if this was the last document referencing it
	if count <= 1 {
		_ = s.Storage.Delete(doc.ExternalID) // best-effort; log warning on error
	}

	return &deleted, nil
}

// StoreAndLink replaces file content for an existing document atomically.
func (s *FileService) StoreAndLink(ctx context.Context, documentID uuid.UUID, content []byte) (*model.StoredFile, error) {
	_, err := s.Repo.GetByID(ctx, documentID)
	if err != nil {
		return nil, err
	}

	mimeType := s.Processor.DetectMIME(content)
	processed, finalMIME, err := s.Processor.Process(content, mimeType)
	if err != nil {
		return nil, fmt.Errorf("image processing: %w", err)
	}

	stored, err := s.Storage.Save(processed)
	if err != nil {
		return nil, fmt.Errorf("store file: %w", err)
	}

	err = s.Repo.UpdateFile(ctx, documentID, stored.ExternalID, finalMIME, stored.Size)
	if err != nil {
		// Cleanup: delete the newly stored file
		_ = s.Storage.Delete(stored.ExternalID)
		return nil, fmt.Errorf("update document record: %w", err)
	}

	result := model.StoredFile{
		ExternalID: stored.ExternalID,
		MimeType:   finalMIME,
		Size:       stored.Size,
	}
	return &result, nil
}

// UpdateDocumentLocation updates the storage bucket and temporary location flag.
func (s *FileService) UpdateDocumentLocation(ctx context.Context, documentID uuid.UUID, storageBucketID uuid.UUID, temporaryLocation bool) (*model.Document, error) {
	err := s.Repo.UpdateLocation(ctx, documentID, storageBucketID, temporaryLocation)
	if err != nil {
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
	ErrPayloadTooLarge      = errors.New("payload too large")
	ErrUnsupportedMediaType = errors.New("unsupported media type")
)

func contains(slice []string, item string) bool {
	for _, s := range slice {
		if s == item {
			return true
		}
	}
	return false
}
