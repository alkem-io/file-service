package service

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"
	"go.uber.org/zap"

	"github.com/alkem-io/file-service-go/internal/domain/model"
	"github.com/alkem-io/file-service-go/internal/domain/port"
)

var nopLogger = zap.NewNop()

// --- Mocks ---

type mockRepo struct {
	doc           model.Document
	getErr        error
	findDoc       *model.Document // nil means "not found"
	findErr       error           // if set, overrides findDoc
	findCalls     int
	lastFindExt   string
	lastFindBkt   uuid.UUID
	createErr     error
	createErrOnce error // returned only on the first Create call
	createCalls   int
	updateErr     error
	deleteResult  model.DeletedDocument
	deleteErr     error
	count         int
	countErr      error
}

// Ensure mockRepo implements the full interface at compile time.
var _ port.DocumentRepo = (*mockRepo)(nil)

func (m *mockRepo) GetByID(_ context.Context, _ uuid.UUID) (model.Document, error) {
	return m.doc, m.getErr
}
func (m *mockRepo) FindByExternalIDAndBucket(_ context.Context, externalID string, storageBucketID uuid.UUID) (model.Document, error) {
	m.findCalls++
	m.lastFindExt = externalID
	m.lastFindBkt = storageBucketID
	if m.findErr != nil {
		return model.Document{}, m.findErr
	}
	if m.findDoc == nil {
		return model.Document{}, model.ErrDocumentNotFound
	}
	return *m.findDoc, nil
}
func (m *mockRepo) Create(_ context.Context, doc model.Document) (uuid.UUID, error) {
	m.createCalls++
	if m.createErrOnce != nil && m.createCalls == 1 {
		return uuid.Nil, m.createErrOnce
	}
	return doc.ID, m.createErr
}
func (m *mockRepo) UpdateFile(_ context.Context, _ uuid.UUID, _, _ string, _ int) error {
	return m.updateErr
}
func (m *mockRepo) UpdateLocation(_ context.Context, _ uuid.UUID, _ uuid.UUID, _ bool, _ int) error {
	return m.updateErr
}
func (m *mockRepo) Delete(_ context.Context, _ uuid.UUID) (model.DeletedDocument, error) {
	return m.deleteResult, m.deleteErr
}
func (m *mockRepo) CountByExternalID(_ context.Context, _ string) (int, error) {
	return m.count, m.countErr
}

type mockAuth struct {
	allowed bool
	err     error
}

func (m *mockAuth) CheckPrivilege(_ context.Context, _, _, _ string) (model.AuthResult, error) {
	return model.AuthResult{Allowed: m.allowed, Reason: "test"}, m.err
}

type mockStorage struct {
	data      []byte
	saved     []byte
	deleted   bool
	saveErr   error
	readErr   error
	deleteErr error
}

func (m *mockStorage) Save(content []byte) (model.StoredFile, error) {
	m.saved = content
	if m.saveErr != nil {
		return model.StoredFile{}, m.saveErr
	}
	hash := ComputeHash(content)
	return model.StoredFile{ExternalID: hash, Size: len(content), Created: true}, nil
}
func (m *mockStorage) Read(_ string) ([]byte, error) { return m.data, m.readErr }
func (m *mockStorage) Delete(_ string) error         { m.deleted = true; return m.deleteErr }
func (m *mockStorage) Exists(_ string) (bool, error) { return m.data != nil, nil }

type mockProcessor struct {
	processErr error
}

func (m *mockProcessor) DetectMIME(_ []byte) string { return "application/octet-stream" }
func (m *mockProcessor) Process(content []byte, mimeType string) ([]byte, string, error) {
	if m.processErr != nil {
		return nil, "", m.processErr
	}
	return content, mimeType, nil
}

// --- Tests ---

func TestServeFile_Allowed(t *testing.T) {
	svc := &FileService{Logger: nopLogger,
		Repo:    &mockRepo{doc: model.Document{ExternalID: "abc", MimeType: "text/plain", AuthorizationID: uuid.New()}},
		Auth:    &mockAuth{allowed: true},
		Storage: &mockStorage{data: []byte("content")},
	}

	result, err := svc.ServeFile(context.Background(), uuid.New(), "actor-1")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if string(result.Content) != "content" {
		t.Errorf("content = %q, want %q", result.Content, "content")
	}
}

func TestServeFile_Denied(t *testing.T) {
	svc := &FileService{Logger: nopLogger,
		Repo:    &mockRepo{doc: model.Document{AuthorizationID: uuid.New()}},
		Auth:    &mockAuth{allowed: false},
		Storage: &mockStorage{},
	}

	_, err := svc.ServeFile(context.Background(), uuid.New(), "actor-1")
	if !errors.Is(err, ErrForbidden) {
		t.Errorf("expected ErrForbidden, got %v", err)
	}
}

func TestCreateDocument_Happy(t *testing.T) {
	svc := &FileService{Logger: nopLogger,
		Repo:      &mockRepo{},
		Storage:   &mockStorage{},
		Processor: &mockProcessor{},
	}

	input := model.CreateDocumentInput{
		DisplayName:     "test.txt",
		StorageBucketID: uuid.New(),
		AuthorizationID: uuid.New(),
	}

	doc, err := svc.CreateDocument(context.Background(), input, []byte("content"), "", nil, 0)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if doc.ExternalID == "" {
		t.Error("expected non-empty externalID")
	}
	if doc.ID == uuid.Nil {
		t.Error("expected non-nil document ID")
	}
	if doc.Reused {
		t.Error("expected Reused=false for new content")
	}
}

// Dedup: same content in same bucket → existing row returned, Reused=true,
// caller-supplied auth/tagset ignored.
func TestCreateDocument_Dedup_SameContentSameBucket_ReturnsExisting(t *testing.T) {
	existingID := uuid.New()
	existingAuth := uuid.New()
	existingTagset := uuid.New()
	bucket := uuid.New()

	repo := &mockRepo{
		findDoc: &model.Document{
			ID:              existingID,
			ExternalID:      "sha3-of-content",
			MimeType:        "text/plain",
			Size:            7,
			StorageBucketID: bucket,
			AuthorizationID: existingAuth,
			TagsetID:        &existingTagset,
		},
	}
	storage := &mockStorage{}
	svc := &FileService{Logger: nopLogger, Repo: repo, Storage: storage, Processor: &mockProcessor{}}

	callerAuth := uuid.New()
	callerTagset := uuid.New()
	input := model.CreateDocumentInput{
		DisplayName:     "test.txt",
		StorageBucketID: bucket,
		AuthorizationID: callerAuth,
		TagsetID:        &callerTagset,
	}

	doc, err := svc.CreateDocument(context.Background(), input, []byte("content"), "", nil, 0)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !doc.Reused {
		t.Error("expected Reused=true for dedup match")
	}
	if doc.ID != existingID {
		t.Errorf("ID = %v, want existing %v", doc.ID, existingID)
	}
	if doc.AuthorizationID != existingAuth {
		t.Errorf("AuthorizationID = %v, want existing %v (caller-supplied must be ignored)", doc.AuthorizationID, existingAuth)
	}
	if doc.TagsetID == nil || *doc.TagsetID != existingTagset {
		t.Errorf("TagsetID = %v, want existing %v", doc.TagsetID, existingTagset)
	}
	if repo.createCalls != 0 {
		t.Errorf("expected 0 Create calls on dedup hit, got %d", repo.createCalls)
	}
	if repo.lastFindBkt != bucket {
		t.Errorf("lookup bucket = %v, want %v", repo.lastFindBkt, bucket)
	}
}

// Dedup is per-bucket: same content, different bucket → new row inserted.
func TestCreateDocument_Dedup_SameContentDifferentBucket_InsertsNew(t *testing.T) {
	// Default mockRepo findDoc=nil → returns ErrDocumentNotFound (bucket mismatch).
	repo := &mockRepo{}
	svc := &FileService{Logger: nopLogger, Repo: repo, Storage: &mockStorage{}, Processor: &mockProcessor{}}

	input := model.CreateDocumentInput{
		DisplayName:     "test.txt",
		StorageBucketID: uuid.New(),
		AuthorizationID: uuid.New(),
	}
	doc, err := svc.CreateDocument(context.Background(), input, []byte("content"), "", nil, 0)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if doc.Reused {
		t.Error("expected Reused=false for different-bucket content")
	}
	if repo.createCalls != 1 {
		t.Errorf("expected 1 Create call, got %d", repo.createCalls)
	}
}

// Concurrent race: first Create fails with ErrDuplicateKey (other writer
// won the race on unique(externalID, storageBucketID)). Service re-queries
// and returns the winner with Reused=true.
func TestCreateDocument_Dedup_CreateRace_ReturnsWinnerAsReused(t *testing.T) {
	winnerID := uuid.New()
	winnerAuth := uuid.New()
	bucket := uuid.New()

	callCount := 0
	repo := &mockRepoRace{
		find: func() (model.Document, error) {
			callCount++
			if callCount == 1 {
				// First lookup: no existing row → fall through to Create.
				return model.Document{}, model.ErrDocumentNotFound
			}
			// After Create race: the winner is now visible.
			return model.Document{
				ID:              winnerID,
				ExternalID:      "sha3-of-content",
				StorageBucketID: bucket,
				AuthorizationID: winnerAuth,
			}, nil
		},
		createErr: model.ErrDuplicateKey,
	}
	storage := &mockStorage{}
	svc := &FileService{Logger: nopLogger, Repo: repo, Storage: storage, Processor: &mockProcessor{}}

	input := model.CreateDocumentInput{
		DisplayName:     "test.txt",
		StorageBucketID: bucket,
		AuthorizationID: uuid.New(),
	}
	doc, err := svc.CreateDocument(context.Background(), input, []byte("content"), "", nil, 0)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !doc.Reused {
		t.Error("expected Reused=true after Create race")
	}
	if doc.ID != winnerID {
		t.Errorf("ID = %v, want winner %v", doc.ID, winnerID)
	}
	if doc.AuthorizationID != winnerAuth {
		t.Errorf("AuthorizationID = %v, want winner %v", doc.AuthorizationID, winnerAuth)
	}
}

// SkipDedup: true → fresh row inserted even when an existing row matches.
// Lookup must NOT be called; existing row must NOT be returned/mutated.
func TestCreateDocument_SkipDedup_InsertsFreshRowWhenContentExists(t *testing.T) {
	bucket := uuid.New()
	repo := &mockRepo{
		// findDoc set deliberately — proves we never even call FindByExternalIDAndBucket.
		findDoc: &model.Document{
			ID:              uuid.New(),
			ExternalID:      "sha3-of-content",
			StorageBucketID: bucket,
			AuthorizationID: uuid.New(),
		},
	}
	svc := &FileService{Logger: nopLogger, Repo: repo, Storage: &mockStorage{}, Processor: &mockProcessor{}}

	callerAuth := uuid.New()
	callerTagset := uuid.New()
	input := model.CreateDocumentInput{
		DisplayName:     "placeholder.docx",
		StorageBucketID: bucket,
		AuthorizationID: callerAuth,
		TagsetID:        &callerTagset,
		SkipDedup:       true,
	}

	doc, err := svc.CreateDocument(context.Background(), input, []byte("content"), "", nil, 0)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if doc.Reused {
		t.Error("expected Reused=false when SkipDedup=true")
	}
	if repo.findCalls != 0 {
		t.Errorf("expected 0 FindByExternalIDAndBucket calls when SkipDedup=true, got %d", repo.findCalls)
	}
	if repo.createCalls != 1 {
		t.Errorf("expected 1 Create call, got %d", repo.createCalls)
	}
	if doc.AuthorizationID != callerAuth {
		t.Errorf("AuthorizationID = %v, want caller-supplied %v (existing row's auth must not leak in)", doc.AuthorizationID, callerAuth)
	}
	if doc.TagsetID == nil || *doc.TagsetID != callerTagset {
		t.Errorf("TagsetID = %v, want caller-supplied %v", doc.TagsetID, callerTagset)
	}
}

// SkipDedup: true with empty content → still gets a fresh row.
// This is the Collabora placeholder path: 0-byte create that must not
// merge with another empty-content row in the same bucket.
func TestCreateDocument_SkipDedup_EmptyContent_GetsFreshRow(t *testing.T) {
	bucket := uuid.New()
	repo := &mockRepo{
		findDoc: &model.Document{
			ID:              uuid.New(),
			ExternalID:      "sha3-of-empty",
			StorageBucketID: bucket,
			AuthorizationID: uuid.New(),
		},
	}
	svc := &FileService{Logger: nopLogger, Repo: repo, Storage: &mockStorage{}, Processor: &mockProcessor{}}

	input := model.CreateDocumentInput{
		DisplayName:     "doc.docx",
		StorageBucketID: bucket,
		AuthorizationID: uuid.New(),
		SkipDedup:       true,
	}
	doc, err := svc.CreateDocument(context.Background(), input, []byte{}, "application/vnd.openxmlformats-officedocument.wordprocessingml.document", []string{"application/vnd.openxmlformats-officedocument.wordprocessingml.document"}, 0)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if doc.Reused {
		t.Error("expected Reused=false for skipDedup placeholder")
	}
	if repo.findCalls != 0 {
		t.Errorf("expected 0 FindByExternalIDAndBucket calls, got %d", repo.findCalls)
	}
}

// SkipDedup default false → backward-compatible: existing dedup behavior preserved.
// This is the regression guard against accidentally turning dedup off for everyone.
func TestCreateDocument_SkipDedupOmitted_DedupStillApplies(t *testing.T) {
	existingID := uuid.New()
	bucket := uuid.New()
	repo := &mockRepo{
		findDoc: &model.Document{
			ID:              existingID,
			ExternalID:      "sha3-of-content",
			StorageBucketID: bucket,
			AuthorizationID: uuid.New(),
		},
	}
	svc := &FileService{Logger: nopLogger, Repo: repo, Storage: &mockStorage{}, Processor: &mockProcessor{}}

	input := model.CreateDocumentInput{
		DisplayName:     "test.txt",
		StorageBucketID: bucket,
		AuthorizationID: uuid.New(),
		// SkipDedup omitted → defaults to false
	}
	doc, err := svc.CreateDocument(context.Background(), input, []byte("content"), "", nil, 0)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !doc.Reused {
		t.Error("expected Reused=true (default dedup path)")
	}
	if doc.ID != existingID {
		t.Errorf("expected existing row ID %v, got %v", existingID, doc.ID)
	}
}

// SkipDedup hits the unique index: surface as ErrConflict, not Reused=true.
// Server-side may add unique(externalID, storageBucketID); if present, a
// SkipDedup placeholder collision must error rather than silently coalesce.
func TestCreateDocument_SkipDedup_DuplicateKey_ReturnsConflict(t *testing.T) {
	repo := &mockRepoRace{
		find: func() (model.Document, error) {
			t.Fatal("FindByExternalIDAndBucket must not be called when SkipDedup=true")
			return model.Document{}, nil
		},
		createErr: model.ErrDuplicateKey,
	}
	svc := &FileService{Logger: nopLogger, Repo: repo, Storage: &mockStorage{}, Processor: &mockProcessor{}}

	input := model.CreateDocumentInput{
		DisplayName:     "test.docx",
		StorageBucketID: uuid.New(),
		AuthorizationID: uuid.New(),
		SkipDedup:       true,
	}
	_, err := svc.CreateDocument(context.Background(), input, []byte("content"), "", nil, 0)
	if !errors.Is(err, ErrConflict) {
		t.Errorf("expected ErrConflict on SkipDedup duplicate-key, got %v", err)
	}
}

// mockRepoRace is a minimal repo mock supporting a scripted FindByExternalIDAndBucket
// and a fixed Create error — used for the concurrent race test.
type mockRepoRace struct {
	find      func() (model.Document, error)
	createErr error
}

var _ port.DocumentRepo = (*mockRepoRace)(nil)

func (m *mockRepoRace) GetByID(_ context.Context, _ uuid.UUID) (model.Document, error) {
	return model.Document{}, nil
}
func (m *mockRepoRace) FindByExternalIDAndBucket(_ context.Context, _ string, _ uuid.UUID) (model.Document, error) {
	return m.find()
}
func (m *mockRepoRace) Create(_ context.Context, _ model.Document) (uuid.UUID, error) {
	return uuid.Nil, m.createErr
}
func (m *mockRepoRace) UpdateFile(_ context.Context, _ uuid.UUID, _, _ string, _ int) error {
	return nil
}
func (m *mockRepoRace) UpdateLocation(_ context.Context, _ uuid.UUID, _ uuid.UUID, _ bool, _ int) error {
	return nil
}
func (m *mockRepoRace) Delete(_ context.Context, _ uuid.UUID) (model.DeletedDocument, error) {
	return model.DeletedDocument{}, nil
}
func (m *mockRepoRace) CountByExternalID(_ context.Context, _ string) (int, error) {
	return 0, nil
}

func TestCreateDocument_TooLarge(t *testing.T) {
	svc := &FileService{Logger: nopLogger,
		Repo:      &mockRepo{},
		Storage:   &mockStorage{},
		Processor: &mockProcessor{},
	}

	input := model.CreateDocumentInput{DisplayName: "big.bin", StorageBucketID: uuid.New(), AuthorizationID: uuid.New()}
	_, err := svc.CreateDocument(context.Background(), input, []byte("content"), "", nil, 3)
	if !errors.Is(err, ErrPayloadTooLarge) {
		t.Errorf("expected ErrPayloadTooLarge, got %v", err)
	}
}

func TestCreateDocument_UnsupportedMIME(t *testing.T) {
	svc := &FileService{Logger: nopLogger,
		Repo:      &mockRepo{},
		Storage:   &mockStorage{},
		Processor: &mockProcessor{},
	}

	input := model.CreateDocumentInput{DisplayName: "test.bin", StorageBucketID: uuid.New(), AuthorizationID: uuid.New()}
	_, err := svc.CreateDocument(context.Background(), input, []byte("content"), "", []string{"image/jpeg"}, 0)
	if !errors.Is(err, ErrUnsupportedMediaType) {
		t.Errorf("expected ErrUnsupportedMediaType, got %v", err)
	}
}

func TestCreateDocument_EmptyContent_UsesDeclaredMIME(t *testing.T) {
	svc := &FileService{Logger: nopLogger,
		Repo:      &mockRepo{},
		Storage:   &mockStorage{},
		Processor: &mockProcessor{},
	}

	input := model.CreateDocumentInput{DisplayName: "new.docx", StorageBucketID: uuid.New(), AuthorizationID: uuid.New()}
	doc, err := svc.CreateDocument(context.Background(), input, []byte{}, "application/vnd.openxmlformats-officedocument.wordprocessingml.document", []string{"application/vnd.openxmlformats-officedocument.wordprocessingml.document"}, 0)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if doc.MimeType != "application/vnd.openxmlformats-officedocument.wordprocessingml.document" {
		t.Errorf("MimeType = %q, want docx MIME", doc.MimeType)
	}
}

func TestCreateDocument_EmptyContent_RejectsDisallowedMIME(t *testing.T) {
	svc := &FileService{Logger: nopLogger,
		Repo:      &mockRepo{},
		Storage:   &mockStorage{},
		Processor: &mockProcessor{},
	}

	input := model.CreateDocumentInput{DisplayName: "bad.exe", StorageBucketID: uuid.New(), AuthorizationID: uuid.New()}
	_, err := svc.CreateDocument(context.Background(), input, []byte{}, "application/x-executable", []string{"image/png", "image/jpeg"}, 0)
	if !errors.Is(err, ErrUnsupportedMediaType) {
		t.Errorf("expected ErrUnsupportedMediaType, got %v", err)
	}
}

// Post-Save cleanup policy: once a blob is stored, we do NOT delete it on
// subsequent DB failures. A concurrent cross-bucket writer may have already
// linked the blob to its own row (storage dedup is global, table dedup is
// per-bucket). Orphaning a row is worse than orphaning a blob; orphan blobs
// are handled by a periodic GC sweep.
func TestCreateDocument_DBFails_DoesNotDeleteBlob(t *testing.T) {
	storage := &mockStorage{}
	svc := &FileService{Logger: nopLogger,
		Repo:      &mockRepo{createErr: errors.New("db error")},
		Storage:   storage,
		Processor: &mockProcessor{},
	}

	input := model.CreateDocumentInput{DisplayName: "test.txt", StorageBucketID: uuid.New(), AuthorizationID: uuid.New()}
	_, err := svc.CreateDocument(context.Background(), input, []byte("content"), "", nil, 0)
	if err == nil {
		t.Fatal("expected error")
	}
	if storage.deleted {
		t.Error("blob was deleted on DB failure; cross-bucket linker could be orphaned")
	}
}

func TestDeleteDocument_UniqueFile_DeletesFromStorage(t *testing.T) {
	authID := uuid.New()
	storage := &mockStorage{}
	svc := &FileService{Logger: nopLogger,
		Repo: &mockRepo{
			doc:          model.Document{ExternalID: "abc"},
			count:        0, // 0 remaining AFTER delete = last reference
			deleteResult: model.DeletedDocument{ExternalID: "abc", AuthorizationID: authID},
		},
		Storage: storage,
	}

	deleted, err := svc.DeleteDocument(context.Background(), uuid.New())
	if err != nil {
		t.Fatal(err)
	}
	if !storage.deleted {
		t.Error("expected file deletion for unique externalID")
	}
	if deleted.AuthorizationID != authID {
		t.Error("wrong authorizationID returned")
	}
}

func TestDeleteDocument_SharedFile_KeepsFile(t *testing.T) {
	storage := &mockStorage{}
	svc := &FileService{Logger: nopLogger,
		Repo: &mockRepo{
			doc:          model.Document{ExternalID: "shared"},
			count:        1, // 1 remaining AFTER delete = other doc still references it
			deleteResult: model.DeletedDocument{ExternalID: "shared", AuthorizationID: uuid.New()},
		},
		Storage: storage,
	}

	_, err := svc.DeleteDocument(context.Background(), uuid.New())
	if err != nil {
		t.Fatal(err)
	}
	if storage.deleted {
		t.Error("should not delete shared file")
	}
}

func TestStoreAndLink_Happy(t *testing.T) {
	svc := &FileService{Logger: nopLogger,
		Repo:      &mockRepo{doc: model.Document{ID: uuid.New()}},
		Storage:   &mockStorage{},
		Processor: &mockProcessor{},
	}

	result, err := svc.StoreAndLink(context.Background(), uuid.New(), []byte("new content"))
	if err != nil {
		t.Fatal(err)
	}
	if result.ExternalID == "" {
		t.Error("expected non-empty externalID")
	}
}

// Same post-Save cleanup policy: UpdateFile failures don't delete the new blob,
// because another cross-bucket writer may have linked it in the meantime.
func TestStoreAndLink_DBFails_DoesNotDeleteBlob(t *testing.T) {
	storage := &mockStorage{}
	svc := &FileService{Logger: nopLogger,
		Repo:      &mockRepo{doc: model.Document{ID: uuid.New()}, updateErr: errors.New("db error")},
		Storage:   storage,
		Processor: &mockProcessor{},
	}

	_, err := svc.StoreAndLink(context.Background(), uuid.New(), []byte("content"))
	if err == nil {
		t.Fatal("expected error")
	}
	if storage.deleted {
		t.Error("blob was deleted on UpdateFile failure; cross-bucket linker could be orphaned")
	}
}

func TestUpdateDocumentLocation_Happy(t *testing.T) {
	docID := uuid.New()
	newBucket := uuid.New()
	svc := &FileService{Logger: nopLogger,
		Repo: &mockRepo{doc: model.Document{
			ID:                docID,
			StorageBucketID:   uuid.New(),
			TemporaryLocation: true,
		}},
	}

	updated, err := svc.UpdateDocumentLocation(context.Background(), docID, newBucket, false, 1)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if updated == nil {
		t.Fatal("expected non-nil result")
	}
}

func TestUpdateDocumentLocation_NotFound(t *testing.T) {
	svc := &FileService{Logger: nopLogger,
		Repo: &mockRepo{updateErr: errors.New("not found")},
	}

	_, err := svc.UpdateDocumentLocation(context.Background(), uuid.New(), uuid.New(), false, 1)
	if err == nil {
		t.Fatal("expected error for not found")
	}
}

func TestUpdateDocumentLocation_UpdateFails(t *testing.T) {
	svc := &FileService{Logger: nopLogger,
		Repo: &mockRepo{
			doc:       model.Document{ID: uuid.New()},
			updateErr: errors.New("db error"),
		},
	}

	_, err := svc.UpdateDocumentLocation(context.Background(), uuid.New(), uuid.New(), false, 1)
	if err == nil {
		t.Fatal("expected error")
	}
}

func TestServeFile_AuthError(t *testing.T) {
	svc := &FileService{Logger: nopLogger,
		Repo:    &mockRepo{doc: model.Document{AuthorizationID: uuid.New()}},
		Auth:    &mockAuth{err: errors.New("nats timeout")},
		Storage: &mockStorage{},
	}

	_, err := svc.ServeFile(context.Background(), uuid.New(), "actor-1")
	if err == nil {
		t.Fatal("expected error for auth failure")
	}
}

func TestServeFile_DocNotFound(t *testing.T) {
	svc := &FileService{Logger: nopLogger,
		Repo: &mockRepo{getErr: errors.New("not found")},
	}

	_, err := svc.ServeFile(context.Background(), uuid.New(), "actor-1")
	if err == nil {
		t.Fatal("expected error for missing doc")
	}
}

func TestServeFile_StorageReadFails(t *testing.T) {
	svc := &FileService{Logger: nopLogger,
		Repo:    &mockRepo{doc: model.Document{ExternalID: "abc", AuthorizationID: uuid.New()}},
		Auth:    &mockAuth{allowed: true},
		Storage: &mockStorage{readErr: errors.New("disk error")},
	}

	_, err := svc.ServeFile(context.Background(), uuid.New(), "actor-1")
	if err == nil {
		t.Fatal("expected error for storage read failure")
	}
}

func TestDeleteDocument_NotFound(t *testing.T) {
	svc := &FileService{Logger: nopLogger,
		Repo: &mockRepo{deleteErr: errors.New("not found")},
	}

	_, err := svc.DeleteDocument(context.Background(), uuid.New())
	if err == nil {
		t.Fatal("expected error for missing doc")
	}
}

func TestCreateDocument_StorageFails(t *testing.T) {
	svc := &FileService{Logger: nopLogger,
		Repo:      &mockRepo{},
		Storage:   &mockStorage{saveErr: errors.New("disk full")},
		Processor: &mockProcessor{},
	}

	input := model.CreateDocumentInput{DisplayName: "test.txt", StorageBucketID: uuid.New(), AuthorizationID: uuid.New()}
	_, err := svc.CreateDocument(context.Background(), input, []byte("content"), "", nil, 0)
	if err == nil {
		t.Fatal("expected error for storage failure")
	}
}

func TestStoreAndLink_DocNotFound(t *testing.T) {
	svc := &FileService{Logger: nopLogger,
		Repo: &mockRepo{getErr: errors.New("not found")},
	}

	_, err := svc.StoreAndLink(context.Background(), uuid.New(), []byte("content"))
	if err == nil {
		t.Fatal("expected error for missing doc")
	}
}

func TestStoreAndLink_StorageFails(t *testing.T) {
	svc := &FileService{Logger: nopLogger,
		Repo:      &mockRepo{doc: model.Document{ID: uuid.New()}},
		Storage:   &mockStorage{saveErr: errors.New("disk full")},
		Processor: &mockProcessor{},
	}

	_, err := svc.StoreAndLink(context.Background(), uuid.New(), []byte("content"))
	if err == nil {
		t.Fatal("expected error for storage failure")
	}
}

func TestCreateDocument_ProcessorFails(t *testing.T) {
	svc := &FileService{Logger: nopLogger,
		Repo:      &mockRepo{},
		Storage:   &mockStorage{},
		Processor: &mockProcessor{processErr: errors.New("corrupt image")},
	}

	input := model.CreateDocumentInput{DisplayName: "bad.jpg", StorageBucketID: uuid.New(), AuthorizationID: uuid.New()}
	_, err := svc.CreateDocument(context.Background(), input, []byte("content"), "", nil, 0)
	if err == nil {
		t.Fatal("expected error for processor failure")
	}
}

func TestStoreAndLink_ProcessorFails(t *testing.T) {
	svc := &FileService{Logger: nopLogger,
		Repo:      &mockRepo{doc: model.Document{ID: uuid.New()}},
		Storage:   &mockStorage{},
		Processor: &mockProcessor{processErr: errors.New("corrupt image")},
	}

	_, err := svc.StoreAndLink(context.Background(), uuid.New(), []byte("content"))
	if err == nil {
		t.Fatal("expected error for processor failure")
	}
}

func TestDeleteDocument_CountFails_StillSucceeds(t *testing.T) {
	// Post-delete count failure is non-fatal — row is already deleted
	storage := &mockStorage{}
	svc := &FileService{Logger: nopLogger,
		Repo: &mockRepo{
			deleteResult: model.DeletedDocument{ExternalID: "abc", AuthorizationID: uuid.New()},
			countErr:     errors.New("db error"),
		},
		Storage: storage,
	}

	_, err := svc.DeleteDocument(context.Background(), uuid.New())
	if err != nil {
		t.Fatalf("count failure should not fail the delete: %v", err)
	}
	// File should NOT be deleted since count failed (we don't know if it's safe)
	if storage.deleted {
		t.Error("should not delete file when count fails")
	}
}

func TestDeleteDocument_DeleteRepoFails(t *testing.T) {
	svc := &FileService{Logger: nopLogger,
		Repo: &mockRepo{
			doc:       model.Document{ExternalID: "abc"},
			count:     1,
			deleteErr: errors.New("db error"),
		},
		Storage: &mockStorage{},
	}

	_, err := svc.DeleteDocument(context.Background(), uuid.New())
	if err == nil {
		t.Fatal("expected error for delete failure")
	}
}

func TestStoreAndLink_CleansUpOldBlob(t *testing.T) {
	storage := &mockStorage{}
	svc := &FileService{Logger: nopLogger,
		Repo: &mockRepo{
			doc:   model.Document{ID: uuid.New(), ExternalID: "old-hash"},
			count: 0, // old hash has no remaining references
		},
		Storage:   storage,
		Processor: &mockProcessor{},
	}

	result, err := svc.StoreAndLink(context.Background(), uuid.New(), []byte("new content"))
	if err != nil {
		t.Fatal(err)
	}
	// New content produces different hash than "old-hash"
	if result.ExternalID == "old-hash" {
		t.Skip("same hash — no cleanup needed")
	}
	if !storage.deleted {
		t.Error("expected old blob cleanup when no remaining references")
	}
}

func TestStoreAndLink_KeepsOldBlobWhenShared(t *testing.T) {
	storage := &mockStorage{}
	svc := &FileService{Logger: nopLogger,
		Repo: &mockRepo{
			doc:   model.Document{ID: uuid.New(), ExternalID: "shared-hash"},
			count: 2, // other docs still reference old hash
		},
		Storage:   storage,
		Processor: &mockProcessor{},
	}

	_, err := svc.StoreAndLink(context.Background(), uuid.New(), []byte("new content"))
	if err != nil {
		t.Fatal(err)
	}
	if storage.deleted {
		t.Error("should NOT delete old blob when other docs reference it")
	}
}

func TestUpdateDocumentLocation_VersionConflict(t *testing.T) {
	svc := &FileService{Logger: nopLogger,
		Repo: &mockRepo{
			doc:       model.Document{ID: uuid.New(), Version: 5},
			updateErr: model.ErrDocumentNotFound, // 0 rows = version mismatch
		},
	}

	_, err := svc.UpdateDocumentLocation(context.Background(), uuid.New(), uuid.New(), false, 3)
	if err == nil {
		t.Fatal("expected error for version conflict")
	}
}

func TestCreateDocument_DBFails_DedupDoesNotDeleteSharedBlob(t *testing.T) {
	storage := &mockStorage{}
	// Override Save to return Created=false (dedup matched existing file)
	dedupStorage := &dedupMockStorage{inner: storage}
	svc := &FileService{Logger: nopLogger,
		Repo:      &mockRepo{createErr: errors.New("db error")},
		Storage:   dedupStorage,
		Processor: &mockProcessor{},
	}

	input := model.CreateDocumentInput{DisplayName: "test.txt", StorageBucketID: uuid.New(), AuthorizationID: uuid.New()}
	_, err := svc.CreateDocument(context.Background(), input, []byte("content"), "", nil, 0)
	if err == nil {
		t.Fatal("expected error")
	}
	if dedupStorage.deleted {
		t.Error("should NOT delete file on rollback when dedup matched (Created=false)")
	}
}

// dedupMockStorage simulates dedup: Save returns Created=false
type dedupMockStorage struct {
	inner   *mockStorage
	deleted bool
}

func (m *dedupMockStorage) Save(content []byte) (model.StoredFile, error) {
	hash := ComputeHash(content)
	return model.StoredFile{ExternalID: hash, Size: len(content), Created: false}, nil
}
func (m *dedupMockStorage) Read(id string) ([]byte, error) { return m.inner.Read(id) }
func (m *dedupMockStorage) Delete(_ string) error          { m.deleted = true; return nil }
func (m *dedupMockStorage) Exists(id string) (bool, error) { return m.inner.Exists(id) }

func TestStoreAndLink_DBFails_DedupDoesNotDeleteSharedBlob(t *testing.T) {
	dedupStorage := &dedupMockStorage{}
	svc := &FileService{Logger: nopLogger,
		Repo:      &mockRepo{doc: model.Document{ID: uuid.New(), ExternalID: "old"}, updateErr: errors.New("db error")},
		Storage:   dedupStorage,
		Processor: &mockProcessor{},
	}

	_, err := svc.StoreAndLink(context.Background(), uuid.New(), []byte("content"))
	if err == nil {
		t.Fatal("expected error")
	}
	if dedupStorage.deleted {
		t.Error("should NOT delete file on rollback when dedup matched (Created=false)")
	}
}
