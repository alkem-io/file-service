package service

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/alkem-io/file-service/internal/domain/model"
	"github.com/alkem-io/file-service/internal/domain/port"
)

// recordingOutbox is a fake BackupOutboxRepo that records which transactional path ran and the
// priority it was handed — enough to assert the flag/temporary-location routing (FR-001).
type recordingOutbox struct {
	createCalls      int
	updateCalls      int
	lastPriority     int16
	deletePendingFor []string // "<fileID>|<hash>" passed to DeletePendingForFile (orphan hygiene)
	deletePendingN   int64    // rows DeletePendingForFile reports removed (drives the counter assertion)
}

var _ port.BackupOutboxRepo = (*recordingOutbox)(nil)

func (o *recordingOutbox) CreateWithOutbox(_ context.Context, _ model.Document, _ model.ContentMetadata, priority int16) (uuid.UUID, error) {
	o.createCalls++
	o.lastPriority = priority
	return uuid.New(), nil
}

func (o *recordingOutbox) UpdateFileWithOutbox(_ context.Context, _ uuid.UUID, _, _ string, _ int, _ model.ContentMetadata, priority int16) error {
	o.updateCalls++
	o.lastPriority = priority
	return nil
}

func (o *recordingOutbox) PruneBackupOutbox(_ context.Context, _ time.Time) (int64, error) {
	return 0, nil
}

func (o *recordingOutbox) DeletePendingForFile(_ context.Context, fileID uuid.UUID, externalID string) (int64, error) {
	o.deletePendingFor = append(o.deletePendingFor, fileID.String()+"|"+externalID)
	return o.deletePendingN, nil
}

// When the last reference to a blob is deleted and the producer is ON, THIS file's pending outbox
// hint is dropped too (orphan hygiene: the bytes are gone, a fetch could only 404) — scoped to
// (fileID, externalID), never the hash alone. With the producer OFF (Outbox nil) only the outbox
// step is skipped — the blob delete itself is unchanged.
func TestCleanupOrphanedBlobDropsPendingOutboxRows(t *testing.T) {
	fileID := uuid.New()

	// Producer ON: the blob is deleted AND this file's pending hint is dropped, scoped to
	// (fileID, hash). The counter advances by the exact number of rows removed (n=2, not a
	// hardcoded 1) so an Add(n)→Add(1) regression is caught.
	outbox := &recordingOutbox{deletePendingN: 2}
	storage := &mockStorage{data: []byte("x")}
	s := &FileService{Storage: storage, Outbox: outbox, Logger: nopLogger}
	before := backupOutboxOrphaned.Value()
	s.cleanupOrphanedBlob(context.Background(), fileID, "somehash")
	if !storage.deleted {
		t.Fatal("the orphaned blob must be deleted")
	}
	want := fileID.String() + "|somehash"
	if len(outbox.deletePendingFor) != 1 || outbox.deletePendingFor[0] != want {
		t.Fatalf("pending outbox row not dropped for (fileID, hash): got %v, want [%q]", outbox.deletePendingFor, want)
	}
	if got := backupOutboxOrphaned.Value() - before; got != 2 {
		t.Fatalf("orphan-hygiene counter must advance by the rows removed (2), got +%d", got)
	}

	// n==0 (nothing was pending for this file) must NOT move the counter (the n>0 guard).
	zeroOutbox := &recordingOutbox{deletePendingN: 0}
	sZero := &FileService{Storage: &mockStorage{data: []byte("x")}, Outbox: zeroOutbox, Logger: nopLogger}
	before0 := backupOutboxOrphaned.Value()
	sZero.cleanupOrphanedBlob(context.Background(), fileID, "somehash")
	if got := backupOutboxOrphaned.Value() - before0; got != 0 {
		t.Fatalf("no rows removed must not move the counter, got +%d", got)
	}

	// Producer OFF (nil Outbox): only the outbox step is skipped — the blob delete path is
	// unchanged, so the blob must STILL be deleted (and no panic on the nil Outbox).
	offStorage := &mockStorage{data: []byte("x")}
	sOff := &FileService{Storage: offStorage, Logger: nopLogger}
	sOff.cleanupOrphanedBlob(context.Background(), fileID, "somehash")
	if !offStorage.deleted {
		t.Fatal("producer off must not change the blob-delete path — the blob must still be deleted")
	}

	// Blob delete FAILING must keep the row: while the blob exists it is still backable.
	failing := &recordingOutbox{deletePendingN: 1}
	sFail := &FileService{
		Storage: &mockStorage{data: []byte("x"), deleteErr: errors.New("fs busy")},
		Outbox:  failing,
		Logger:  nopLogger,
	}
	sFail.cleanupOrphanedBlob(context.Background(), fileID, "somehash")
	if len(failing.deletePendingFor) != 0 {
		t.Fatal("outbox row must be kept when the blob delete failed (blob still backable)")
	}
}

func TestPriorityForMime(t *testing.T) {
	s := &FileService{HotMimePrefixes: []string{"application/vnd.openxmlformats-officedocument", "application/x-yjs"}}
	hot := []string{"application/x-yjs", "application/vnd.openxmlformats-officedocument.wordprocessingml.document"}
	for _, m := range hot {
		if s.priorityForMime(m) != 1 {
			t.Fatalf("%q must be hot (1)", m)
		}
	}
	for _, m := range []string{"image/png", "text/plain", ""} {
		if s.priorityForMime(m) != 0 {
			t.Fatalf("%q must be normal (0)", m)
		}
	}
	// Case-insensitive (RFC 2045): a mixed-case MIME must still match a lowercase hot prefix.
	if s.priorityForMime("Application/X-Yjs") != 1 {
		t.Fatal("mixed-case yjs must still be hot (1)")
	}
}

// TestWriteCreateRouting: the outbox path runs only when the producer is on AND the object is
// non-temporary; otherwise the plain Repo.Create path runs (FR-001 excludes temporary objects,
// and the flag-off default is unchanged behaviour).
func TestWriteCreateRouting(t *testing.T) {
	nonTemp := model.Document{MimeType: "application/x-yjs"}
	temp := model.Document{MimeType: "application/x-yjs", TemporaryLocation: true}

	t.Run("producer-off-uses-repo", func(t *testing.T) {
		repo, ob := &mockRepo{}, &recordingOutbox{}
		s := &FileService{Repo: repo, Logger: nopLogger} // Outbox nil
		_ = s.writeCreate(context.Background(), nonTemp, model.ContentMetadata{})
		if repo.createCalls != 1 || ob.createCalls != 0 {
			t.Fatalf("off: repo=%d outbox=%d (want repo=1 outbox=0)", repo.createCalls, ob.createCalls)
		}
	})
	t.Run("producer-on-nontemporary-uses-outbox-hot", func(t *testing.T) {
		repo, ob := &mockRepo{}, &recordingOutbox{}
		s := &FileService{Repo: repo, Outbox: ob, HotMimePrefixes: []string{"application/x-yjs"}, Logger: nopLogger}
		before := backupOutboxEnqueued.Value()
		_ = s.writeCreate(context.Background(), nonTemp, model.ContentMetadata{})
		if ob.createCalls != 1 || repo.createCalls != 0 {
			t.Fatalf("on: outbox=%d repo=%d (want outbox=1 repo=0)", ob.createCalls, repo.createCalls)
		}
		if ob.lastPriority != 1 {
			t.Fatalf("a yjs object must enqueue hot priority=1, got %d", ob.lastPriority)
		}
		if got := backupOutboxEnqueued.Value() - before; got != 1 {
			t.Fatalf("a committed enqueue must count once (T011): got +%d", got)
		}
	})
	t.Run("producer-on-temporary-uses-repo", func(t *testing.T) {
		repo, ob := &mockRepo{}, &recordingOutbox{}
		s := &FileService{Repo: repo, Outbox: ob, Logger: nopLogger}
		before := backupOutboxEnqueued.Value()
		_ = s.writeCreate(context.Background(), temp, model.ContentMetadata{})
		if repo.createCalls != 1 || ob.createCalls != 0 {
			t.Fatalf("temporary must NOT enqueue: repo=%d outbox=%d", repo.createCalls, ob.createCalls)
		}
		if got := backupOutboxEnqueued.Value() - before; got != 0 {
			t.Fatalf("temporary must NOT count an enqueue (T011): got +%d", got)
		}
	})
}

// TestWriteReplaceRouting mirrors the create routing for the content-replace path.
func TestWriteReplaceRouting(t *testing.T) {
	t.Run("producer-off-uses-repo", func(t *testing.T) {
		repo, ob := &mockRepo{}, &recordingOutbox{}
		s := &FileService{Repo: repo, Logger: nopLogger} // Outbox nil
		_ = s.writeReplace(context.Background(), model.Document{}, uuid.New(), "h", "image/jpeg", 5, model.ContentMetadata{})
		if repo.updateFileCalls != 1 || ob.updateCalls != 0 {
			t.Fatalf("off: repo=%d outbox=%d (want repo=1 outbox=0)", repo.updateFileCalls, ob.updateCalls)
		}
	})
	t.Run("producer-on-nontemporary-uses-outbox", func(t *testing.T) {
		repo, ob := &mockRepo{}, &recordingOutbox{}
		s := &FileService{Repo: repo, Outbox: ob, Logger: nopLogger}
		before := backupOutboxEnqueued.Value()
		_ = s.writeReplace(context.Background(), model.Document{}, uuid.New(), "h", "image/jpeg", 5, model.ContentMetadata{})
		if ob.updateCalls != 1 || repo.updateFileCalls != 0 {
			t.Fatalf("outbox=%d repo=%d (want outbox=1 repo=0)", ob.updateCalls, repo.updateFileCalls)
		}
		if got := backupOutboxEnqueued.Value() - before; got != 1 {
			t.Fatalf("a committed replace-enqueue must count once (T011): got +%d", got)
		}
	})
	t.Run("producer-on-temporary-uses-repo", func(t *testing.T) {
		repo, ob := &mockRepo{}, &recordingOutbox{}
		s := &FileService{Repo: repo, Outbox: ob, Logger: nopLogger}
		_ = s.writeReplace(context.Background(), model.Document{TemporaryLocation: true}, uuid.New(), "h", "image/jpeg", 5, model.ContentMetadata{})
		if repo.updateFileCalls != 1 || ob.updateCalls != 0 {
			t.Fatalf("temporary must NOT enqueue: repo=%d outbox=%d", repo.updateFileCalls, ob.updateCalls)
		}
	})
}
