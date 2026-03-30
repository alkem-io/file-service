package service

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"

	"github.com/alkem-io/file-service-go/internal/domain/model"
	"github.com/alkem-io/file-service-go/internal/domain/port"
)

// --- Mocks ---

type mockRepo struct {
	doc          model.Document
	getErr       error
	createErr    error
	updateErr    error
	deleteResult model.DeletedDocument
	deleteErr    error
	count        int
	countErr     error
}

// Ensure mockRepo implements the full interface at compile time.
var _ port.DocumentRepo = (*mockRepo)(nil)

func (m *mockRepo) GetByID(_ context.Context, _ uuid.UUID) (model.Document, error) {
	return m.doc, m.getErr
}
func (m *mockRepo) Create(_ context.Context, doc model.Document) (uuid.UUID, error) {
	return doc.ID, m.createErr
}
func (m *mockRepo) UpdateFile(_ context.Context, _ uuid.UUID, _, _ string, _ int) error {
	return m.updateErr
}
func (m *mockRepo) UpdateLocation(_ context.Context, _ uuid.UUID, _ uuid.UUID, _ bool) error {
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
	return model.StoredFile{ExternalID: hash, Size: len(content)}, nil
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
	svc := &FileService{
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
	svc := &FileService{
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
	svc := &FileService{
		Repo:      &mockRepo{},
		Storage:   &mockStorage{},
		Processor: &mockProcessor{},
	}

	input := model.CreateDocumentInput{
		DisplayName:     "test.txt",
		StorageBucketID: uuid.New(),
		AuthorizationID: uuid.New(),
	}

	doc, err := svc.CreateDocument(context.Background(), input, []byte("content"), nil, 0)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if doc.ExternalID == "" {
		t.Error("expected non-empty externalID")
	}
	if doc.ID == uuid.Nil {
		t.Error("expected non-nil document ID")
	}
}

func TestCreateDocument_TooLarge(t *testing.T) {
	svc := &FileService{
		Repo:      &mockRepo{},
		Storage:   &mockStorage{},
		Processor: &mockProcessor{},
	}

	input := model.CreateDocumentInput{DisplayName: "big.bin", StorageBucketID: uuid.New(), AuthorizationID: uuid.New()}
	_, err := svc.CreateDocument(context.Background(), input, []byte("content"), nil, 3)
	if !errors.Is(err, ErrPayloadTooLarge) {
		t.Errorf("expected ErrPayloadTooLarge, got %v", err)
	}
}

func TestCreateDocument_UnsupportedMIME(t *testing.T) {
	svc := &FileService{
		Repo:      &mockRepo{},
		Storage:   &mockStorage{},
		Processor: &mockProcessor{},
	}

	input := model.CreateDocumentInput{DisplayName: "test.bin", StorageBucketID: uuid.New(), AuthorizationID: uuid.New()}
	_, err := svc.CreateDocument(context.Background(), input, []byte("content"), []string{"image/jpeg"}, 0)
	if !errors.Is(err, ErrUnsupportedMediaType) {
		t.Errorf("expected ErrUnsupportedMediaType, got %v", err)
	}
}

func TestCreateDocument_DBFails_CleansUpFile(t *testing.T) {
	storage := &mockStorage{}
	svc := &FileService{
		Repo:      &mockRepo{createErr: errors.New("db error")},
		Storage:   storage,
		Processor: &mockProcessor{},
	}

	input := model.CreateDocumentInput{DisplayName: "test.txt", StorageBucketID: uuid.New(), AuthorizationID: uuid.New()}
	_, err := svc.CreateDocument(context.Background(), input, []byte("content"), nil, 0)
	if err == nil {
		t.Fatal("expected error")
	}
	if !storage.deleted {
		t.Error("expected file cleanup after DB failure")
	}
}

func TestDeleteDocument_UniqueFile_DeletesFromStorage(t *testing.T) {
	authID := uuid.New()
	storage := &mockStorage{}
	svc := &FileService{
		Repo: &mockRepo{
			doc:          model.Document{ExternalID: "abc"},
			count:        1,
			deleteResult: model.DeletedDocument{AuthorizationID: authID},
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
	svc := &FileService{
		Repo: &mockRepo{
			doc:          model.Document{ExternalID: "shared"},
			count:        2,
			deleteResult: model.DeletedDocument{AuthorizationID: uuid.New()},
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
	svc := &FileService{
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

func TestStoreAndLink_DBFails_CleansUpFile(t *testing.T) {
	storage := &mockStorage{}
	svc := &FileService{
		Repo:      &mockRepo{doc: model.Document{ID: uuid.New()}, updateErr: errors.New("db error")},
		Storage:   storage,
		Processor: &mockProcessor{},
	}

	_, err := svc.StoreAndLink(context.Background(), uuid.New(), []byte("content"))
	if err == nil {
		t.Fatal("expected error")
	}
	if !storage.deleted {
		t.Error("expected file cleanup after DB failure")
	}
}

func TestUpdateDocumentLocation_Happy(t *testing.T) {
	docID := uuid.New()
	newBucket := uuid.New()
	svc := &FileService{
		Repo: &mockRepo{doc: model.Document{
			ID:                docID,
			StorageBucketID:   uuid.New(),
			TemporaryLocation: true,
		}},
	}

	updated, err := svc.UpdateDocumentLocation(context.Background(), docID, newBucket, false)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if updated == nil {
		t.Fatal("expected non-nil result")
	}
}

func TestUpdateDocumentLocation_NotFound(t *testing.T) {
	svc := &FileService{
		Repo: &mockRepo{getErr: errors.New("not found")},
	}

	_, err := svc.UpdateDocumentLocation(context.Background(), uuid.New(), uuid.New(), false)
	if err == nil {
		t.Fatal("expected error for not found")
	}
}

func TestUpdateDocumentLocation_UpdateFails(t *testing.T) {
	svc := &FileService{
		Repo: &mockRepo{
			doc:       model.Document{ID: uuid.New()},
			updateErr: errors.New("db error"),
		},
	}

	_, err := svc.UpdateDocumentLocation(context.Background(), uuid.New(), uuid.New(), false)
	if err == nil {
		t.Fatal("expected error")
	}
}

func TestServeFile_AuthError(t *testing.T) {
	svc := &FileService{
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
	svc := &FileService{
		Repo: &mockRepo{getErr: errors.New("not found")},
	}

	_, err := svc.ServeFile(context.Background(), uuid.New(), "actor-1")
	if err == nil {
		t.Fatal("expected error for missing doc")
	}
}

func TestServeFile_StorageReadFails(t *testing.T) {
	svc := &FileService{
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
	svc := &FileService{
		Repo: &mockRepo{getErr: errors.New("not found")},
	}

	_, err := svc.DeleteDocument(context.Background(), uuid.New())
	if err == nil {
		t.Fatal("expected error for missing doc")
	}
}

func TestContains(t *testing.T) {
	if !contains([]string{"a", "b", "c"}, "b") {
		t.Error("expected true")
	}
	if contains([]string{"a", "b"}, "z") {
		t.Error("expected false")
	}
	if contains(nil, "a") {
		t.Error("expected false for nil slice")
	}
}

func TestCreateDocument_StorageFails(t *testing.T) {
	svc := &FileService{
		Repo:      &mockRepo{},
		Storage:   &mockStorage{saveErr: errors.New("disk full")},
		Processor: &mockProcessor{},
	}

	input := model.CreateDocumentInput{DisplayName: "test.txt", StorageBucketID: uuid.New(), AuthorizationID: uuid.New()}
	_, err := svc.CreateDocument(context.Background(), input, []byte("content"), nil, 0)
	if err == nil {
		t.Fatal("expected error for storage failure")
	}
}

func TestStoreAndLink_DocNotFound(t *testing.T) {
	svc := &FileService{
		Repo: &mockRepo{getErr: errors.New("not found")},
	}

	_, err := svc.StoreAndLink(context.Background(), uuid.New(), []byte("content"))
	if err == nil {
		t.Fatal("expected error for missing doc")
	}
}

func TestStoreAndLink_StorageFails(t *testing.T) {
	svc := &FileService{
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
	svc := &FileService{
		Repo:      &mockRepo{},
		Storage:   &mockStorage{},
		Processor: &mockProcessor{processErr: errors.New("corrupt image")},
	}

	input := model.CreateDocumentInput{DisplayName: "bad.jpg", StorageBucketID: uuid.New(), AuthorizationID: uuid.New()}
	_, err := svc.CreateDocument(context.Background(), input, []byte("content"), nil, 0)
	if err == nil {
		t.Fatal("expected error for processor failure")
	}
}

func TestStoreAndLink_ProcessorFails(t *testing.T) {
	svc := &FileService{
		Repo:      &mockRepo{doc: model.Document{ID: uuid.New()}},
		Storage:   &mockStorage{},
		Processor: &mockProcessor{processErr: errors.New("corrupt image")},
	}

	_, err := svc.StoreAndLink(context.Background(), uuid.New(), []byte("content"))
	if err == nil {
		t.Fatal("expected error for processor failure")
	}
}

func TestDeleteDocument_CountFails(t *testing.T) {
	svc := &FileService{
		Repo: &mockRepo{
			doc:      model.Document{ExternalID: "abc"},
			countErr: errors.New("db error"),
		},
		Storage: &mockStorage{},
	}

	_, err := svc.DeleteDocument(context.Background(), uuid.New())
	if err == nil {
		t.Fatal("expected error for count failure")
	}
}

func TestDeleteDocument_DeleteRepoFails(t *testing.T) {
	svc := &FileService{
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
