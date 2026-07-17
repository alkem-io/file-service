package service

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/alkem-io/file-service/internal/domain/model"
	"github.com/alkem-io/file-service/internal/domain/port"
)

// recordingOutbox is a fake BackupOutboxRepo that records which transactional path ran and the
// priority it was handed — enough to assert the flag/temporary-location routing (FR-001).
type recordingOutbox struct {
	createCalls         int
	updateCalls         int
	updateMetadataCalls int
	lastPriority        int16
	lastMetaExternalID  string
	lastMetaSize        int
	updateMetadataErr   error
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

func (o *recordingOutbox) UpdateMetadataWithOutbox(_ context.Context, _ uuid.UUID, _ model.DocumentMetadataUpdate, _ int, externalID string, size int, priority int16) error {
	o.updateMetadataCalls++
	o.lastPriority = priority
	o.lastMetaExternalID = externalID
	o.lastMetaSize = size
	return o.updateMetadataErr
}

func (o *recordingOutbox) PruneBackupOutbox(_ context.Context, _ time.Time) (int64, error) {
	return 0, nil
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

// TestUpdateMetadataOutboxRouting: 013 conversation media reaches durability via a metadata PATCH
// flipping temporaryLocation true→false (re-home MOVE / re-share pin / outbound flip). Only that
// transition — outbox on AND the row WAS temporary AND the update targets durable — enqueues a
// backup-outbox row; every other PATCH takes the plain Repo.UpdateMetadata path with no enqueue.
func TestUpdateMetadataOutboxRouting(t *testing.T) {
	const officeMime = "application/vnd.openxmlformats-officedocument.wordprocessingml.document"
	// The threaded `current` document (what the handler already loaded) and what a re-read of the
	// repo would return are given DIVERGENT content fields on purpose: the outbox row must carry the
	// THREADED values, so if anyone reintroduces the removed s.Repo.GetByID pre-read (reading
	// repo.doc) the enqueued externalID/size/priority would flip to repo.doc's and these assertions
	// would fail — genuinely guarding the no-pre-read invariant this commit establishes.
	const threadedExternalID, threadedSize = "hashThreaded", 99
	cases := []struct {
		name         string
		wasTemporary bool   // the threaded current row's temporaryLocation before the PATCH
		currentMime  string // the threaded current row's mime — drives the enqueued priority
		repoMime     string // what a (forbidden) re-read would see — OPPOSITE priority class, to catch it
		outboxOn     bool
		updateTemp   bool  // meta.TemporaryLocation (the PATCH target)
		outboxErr    error // when set, UpdateMetadataWithOutbox fails — the metric-guard error path
		wantOutbox   int   // expected UpdateMetadataWithOutbox calls
		wantRepo     int   // expected plain Repo.UpdateMetadata calls
		wantMetric   int64 // expected backupOutboxEnqueued delta (increments only on a SUCCESSFUL enqueue)
		wantErr      bool
		wantPriority int16 // priority derived from currentMime (NOT repoMime)
	}{
		{name: "transition-image-priority-0", wasTemporary: true, currentMime: "image/png", repoMime: officeMime, outboxOn: true, wantOutbox: 1, wantMetric: 1, wantPriority: 0},
		{name: "transition-office-hot-priority-1", wasTemporary: true, currentMime: officeMime, repoMime: "image/png", outboxOn: true, wantOutbox: 1, wantMetric: 1, wantPriority: 1},
		{name: "outbox-off-uses-repo", wasTemporary: true, currentMime: "image/png", repoMime: "image/png", outboxOn: false, wantRepo: 1},
		{name: "already-durable-no-transition", wasTemporary: false, currentMime: "image/png", repoMime: "image/png", outboxOn: true, wantRepo: 1},
		{name: "keeps-temporary-no-enqueue", wasTemporary: true, currentMime: "image/png", repoMime: "image/png", outboxOn: true, updateTemp: true, wantRepo: 1},
		// Metric-guard error path: a transition whose outbox write fails must propagate the error
		// AND leave the counter untouched (the `if err == nil { Add(1) }` guard).
		{name: "transition-outbox-error-no-metric", wasTemporary: true, currentMime: "image/png", repoMime: officeMime, outboxOn: true, outboxErr: ErrConflict, wantOutbox: 1, wantMetric: 0, wantErr: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// repo.doc is the post-update read source AND what a forbidden pre-read would return —
			// deliberately divergent from `current` in every content field.
			repo := &mockRepo{doc: model.Document{ExternalID: "hashRepoRead", Size: 7, MimeType: tc.repoMime, TemporaryLocation: tc.wasTemporary}}
			ob := &recordingOutbox{updateMetadataErr: tc.outboxErr}
			s := &FileService{Repo: repo, HotMimePrefixes: []string{"application/vnd.openxmlformats-officedocument"}, Logger: nopLogger}
			if tc.outboxOn {
				s.Outbox = ob
			}
			// The handler loads the current document and threads it in; content fields (immutable on
			// a metadata PATCH) come from here, NOT from a re-read.
			current := model.Document{ExternalID: threadedExternalID, Size: threadedSize, MimeType: tc.currentMime, TemporaryLocation: tc.wasTemporary}
			meta := model.DocumentMetadataUpdate{TemporaryLocation: tc.updateTemp}
			before := backupOutboxEnqueued.Value()

			_, err := s.UpdateDocumentMetadata(context.Background(), uuid.New(), meta, 1, current)
			if tc.wantErr && err == nil {
				t.Fatal("expected an error to propagate from the failed outbox enqueue")
			}
			if !tc.wantErr && err != nil {
				t.Fatalf("UpdateDocumentMetadata: %v", err)
			}
			if ob.updateMetadataCalls != tc.wantOutbox {
				t.Fatalf("outbox calls = %d, want %d", ob.updateMetadataCalls, tc.wantOutbox)
			}
			if repo.updateMetadataCalls != tc.wantRepo {
				t.Fatalf("repo calls = %d, want %d", repo.updateMetadataCalls, tc.wantRepo)
			}
			if got := backupOutboxEnqueued.Value() - before; got != tc.wantMetric {
				t.Fatalf("enqueue counter delta = %d, want %d", got, tc.wantMetric)
			}
			if tc.wantOutbox == 1 {
				// The row MUST carry the threaded current values, never repo.doc's — a reintroduced
				// GetByID pre-read would surface repo.doc's ("hashRepoRead"/7/opposite priority) here.
				if ob.lastMetaExternalID != threadedExternalID || ob.lastMetaSize != threadedSize {
					t.Fatalf("outbox row must carry the THREADED externalID/size %q/%d, got %q/%d (a re-read would give repo.doc's)",
						threadedExternalID, threadedSize, ob.lastMetaExternalID, ob.lastMetaSize)
				}
				if ob.lastPriority != tc.wantPriority {
					t.Fatalf("priority = %d, want %d (derived from the THREADED mime, not repo.doc's)", ob.lastPriority, tc.wantPriority)
				}
			}
		})
	}
}
