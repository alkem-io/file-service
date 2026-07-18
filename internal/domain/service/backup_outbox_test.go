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
// UpdateMetadataWithOutbox returns the AUTHORITATIVE post-update document the real adapter would
// map in-tx from the locked row's full RETURNING; returnDoc seeds it.
type recordingOutbox struct {
	createCalls         int
	updateCalls         int
	updateMetadataCalls int
	lastPriority        int16
	returnDoc           model.Document // simulated post-update full-row RETURNING (authoritative)
	updateMetadataErr   error
	// updateFileEnqueued is what UpdateFileWithOutbox reports back — the in-tx durability decision
	// the real adapter derives from the UPDATE's RETURNING "temporaryLocation" (true = a durable
	// replace enqueued a backup row; false = still-temporary, enqueued nothing).
	updateFileEnqueued bool
}

var _ port.BackupOutboxRepo = (*recordingOutbox)(nil)

func (o *recordingOutbox) CreateWithOutbox(_ context.Context, _ model.Document, _ model.ContentMetadata, priority int16) (uuid.UUID, error) {
	o.createCalls++
	o.lastPriority = priority
	return uuid.New(), nil
}

func (o *recordingOutbox) UpdateFileWithOutbox(_ context.Context, _ uuid.UUID, _, _ string, _ int, _ model.ContentMetadata, priority int16) (bool, error) {
	o.updateCalls++
	o.lastPriority = priority
	return o.updateFileEnqueued, nil
}

func (o *recordingOutbox) UpdateMetadataWithOutbox(_ context.Context, _ uuid.UUID, _ model.DocumentMetadataUpdate, _ int, priorityFor func(mimeType string) int16) (model.Document, error) {
	o.updateMetadataCalls++
	// Mirror the real adapter: priority is computed in-tx from the AUTHORITATIVE post-update mime
	// (returnDoc = the locked row's full RETURNING), NOT from any pre-update caller snapshot.
	o.lastPriority = priorityFor(o.returnDoc.MimeType)
	return o.returnDoc, o.updateMetadataErr
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

// TestWriteReplaceRouting covers the content-replace path. With the producer on, the replace ALWAYS
// routes through the outbox-aware path (the enqueue-or-not decision is made in-tx from the row's
// authoritative "temporaryLocation", never the pre-loaded snapshot); the metric counts only when a
// backup row was actually enqueued (a durable replace). Producer off → the plain Repo.UpdateFile.
func TestWriteReplaceRouting(t *testing.T) {
	t.Run("producer-off-uses-repo", func(t *testing.T) {
		repo, ob := &mockRepo{}, &recordingOutbox{}
		s := &FileService{Repo: repo, Logger: nopLogger} // Outbox nil
		_ = s.writeReplace(context.Background(), uuid.New(), "h", "image/jpeg", 5, model.ContentMetadata{})
		if repo.updateFileCalls != 1 || ob.updateCalls != 0 {
			t.Fatalf("off: repo=%d outbox=%d (want repo=1 outbox=0)", repo.updateFileCalls, ob.updateCalls)
		}
	})
	t.Run("producer-on-durable-enqueues-and-counts", func(t *testing.T) {
		// In-tx RETURNING temporaryLocation=false (durable): the outbox row is enqueued and the
		// metric moves once.
		repo, ob := &mockRepo{}, &recordingOutbox{updateFileEnqueued: true}
		s := &FileService{Repo: repo, Outbox: ob, Logger: nopLogger}
		before := backupOutboxEnqueued.Value()
		_ = s.writeReplace(context.Background(), uuid.New(), "h", "image/jpeg", 5, model.ContentMetadata{})
		if ob.updateCalls != 1 || repo.updateFileCalls != 0 {
			t.Fatalf("outbox=%d repo=%d (want outbox=1 repo=0)", ob.updateCalls, repo.updateFileCalls)
		}
		if got := backupOutboxEnqueued.Value() - before; got != 1 {
			t.Fatalf("a durable replace-enqueue must count once (T011): got +%d", got)
		}
	})
	t.Run("producer-on-still-temporary-skips-enqueue", func(t *testing.T) {
		// In-tx RETURNING temporaryLocation=true (still staging): the replace still routes through
		// the outbox-aware path (the authoritative decision is made in-tx), but enqueues nothing and
		// the metric stays put. It must NOT fall back to the plain Repo path — that pre-loaded-snapshot
		// branch was the stranded-blob race.
		repo, ob := &mockRepo{}, &recordingOutbox{updateFileEnqueued: false}
		s := &FileService{Repo: repo, Outbox: ob, Logger: nopLogger}
		before := backupOutboxEnqueued.Value()
		_ = s.writeReplace(context.Background(), uuid.New(), "h", "image/jpeg", 5, model.ContentMetadata{})
		if ob.updateCalls != 1 || repo.updateFileCalls != 0 {
			t.Fatalf("still-temporary must route through the outbox path, not Repo: outbox=%d repo=%d", ob.updateCalls, repo.updateFileCalls)
		}
		if got := backupOutboxEnqueued.Value() - before; got != 0 {
			t.Fatalf("a still-temporary replace must NOT count an enqueue (T011): got +%d", got)
		}
	})
}

// Authoritative post-update content identity + dims, read in-tx from the locked row's full
// RETURNING on both paths. Deliberately DIVERGENT from the threaded `current` snapshot
// (metaRouteThreaded*), so any regression that rebuilt the response from `current` — or
// reintroduced a stale-snapshot enqueue — flips the assertions in assertMetadataRouting.
const (
	metaRouteThreadedExternalID, metaRouteThreadedSize = "hashThreaded", 99
	metaRouteAuthExternalID, metaRouteAuthSize         = "hashAuthoritative", 200
	metaRouteOfficeMime                                = "application/vnd.openxmlformats-officedocument.wordprocessingml.document"
)

// Authoritative (post-replace) vs threaded (stale) image dims — both non-nil and divergent, so a
// response that sources dims from `current` instead of the authoritative RETURNING row is caught.
var (
	metaRouteAuthW, metaRouteAuthH         = 200, 200
	metaRouteThreadedW, metaRouteThreadedH = 100, 100
)

type metadataRoutingCase struct {
	name         string
	wasTemporary bool   // the threaded current row's temporaryLocation before the PATCH
	currentMime  string // the threaded current (pre-update) row's mime — the STALE snapshot; must NOT drive priority
	repoMime     string // the AUTHORITATIVE post-update (RETURNING) mime — drives the enqueued priority; OPPOSITE class from currentMime to catch a stale-snapshot priority
	outboxOn     bool
	updateTemp   bool  // meta.TemporaryLocation (the PATCH target)
	outboxErr    error // when set, UpdateMetadataWithOutbox fails — the metric-guard error path
	wantOutbox   int   // expected UpdateMetadataWithOutbox calls
	wantRepo     int   // expected plain Repo.UpdateMetadata calls
	wantMetric   int64 // expected backupOutboxEnqueued delta (increments only on a SUCCESSFUL enqueue)
	wantErr      bool
	wantPriority int16 // priority derived from the AUTHORITATIVE post-update mime (repoMime / returnDoc), NOT the stale currentMime
}

// assertMetadataRouting checks the outbox-vs-repo routing, the enqueue metric, the mime-derived
// priority, and — the invariant this commit establishes — that a successful PATCH response carries
// the AUTHORITATIVE post-update externalID/size (never the stale threaded `current`), proving the
// response is built from the in-tx authoritative source AND is reload-free (no post-update
// s.Repo.GetByID).
func assertMetadataRouting(t *testing.T, tc metadataRoutingCase, updated *model.Document, err error, ob *recordingOutbox, repo *mockRepo, metricDelta int64) {
	t.Helper()
	if tc.wantErr != (err != nil) {
		t.Fatalf("error = %v, wantErr = %v", err, tc.wantErr)
	}
	if ob.updateMetadataCalls != tc.wantOutbox {
		t.Fatalf("outbox calls = %d, want %d", ob.updateMetadataCalls, tc.wantOutbox)
	}
	if repo.updateMetadataCalls != tc.wantRepo {
		t.Fatalf("repo calls = %d, want %d", repo.updateMetadataCalls, tc.wantRepo)
	}
	if metricDelta != tc.wantMetric {
		t.Fatalf("enqueue counter delta = %d, want %d", metricDelta, tc.wantMetric)
	}
	if tc.wantOutbox == 1 && ob.lastPriority != tc.wantPriority {
		t.Fatalf("priority = %d, want %d (must derive from the AUTHORITATIVE post-update mime, not the stale threaded current mime)", ob.lastPriority, tc.wantPriority)
	}
	if tc.wantErr {
		return
	}
	if updated == nil {
		t.Fatal("expected a non-nil updated document")
	}
	if updated.ExternalID != metaRouteAuthExternalID || updated.Size != metaRouteAuthSize {
		t.Fatalf("response carries %q/%d, want AUTHORITATIVE %q/%d (never the stale threaded %q/%d)",
			updated.ExternalID, updated.Size, metaRouteAuthExternalID, metaRouteAuthSize, metaRouteThreadedExternalID, metaRouteThreadedSize)
	}
	// Dims must ride along from the SAME authoritative row — not the threaded `current` snapshot.
	// This is the torn-response guard: externalID/size (new) can no longer disagree with dims (old).
	if updated.ImageWidth == nil || updated.ImageHeight == nil ||
		*updated.ImageWidth != metaRouteAuthW || *updated.ImageHeight != metaRouteAuthH {
		t.Fatalf("response dims = %v×%v, want AUTHORITATIVE %d×%d (never the stale threaded %d×%d)",
			updated.ImageWidth, updated.ImageHeight, metaRouteAuthW, metaRouteAuthH, metaRouteThreadedW, metaRouteThreadedH)
	}
}

// TestUpdateMetadataOutboxRouting: 013 conversation media reaches durability via a metadata PATCH
// flipping temporaryLocation true→false (re-home MOVE / re-share pin / outbound flip). Only that
// transition — outbox on AND the row WAS temporary AND the update targets durable — enqueues a
// backup-outbox row; every other PATCH takes the plain Repo.UpdateMetadata path with no enqueue.
// It also pins the authoritative-source / reload-free response invariant (see assertMetadataRouting)
// AND that the enqueued priority derives from the AUTHORITATIVE post-update mime (the RETURNING row),
// not the stale threaded current.MimeType — so a concurrent replace that upgrades a still-temporary
// row non-hot→hot without bumping version can't back the now-hot object up at normal priority.
func TestUpdateMetadataOutboxRouting(t *testing.T) {
	cases := []metadataRoutingCase{
		// current mime is non-hot but the authoritative RETURNING mime is HOT (a concurrent replace
		// upgraded the still-temporary row generic→concrete without bumping version) → HOT priority(1),
		// proving priority tracks the authoritative post-update mime, not the stale snapshot.
		{name: "transition-current-nonhot-authoritative-hot", wasTemporary: true, currentMime: "image/png", repoMime: metaRouteOfficeMime, outboxOn: true, wantOutbox: 1, wantMetric: 1, wantPriority: 1},
		// Mirror image: current mime is HOT but the authoritative RETURNING mime is non-hot → NORMAL priority(0).
		{name: "transition-current-hot-authoritative-nonhot", wasTemporary: true, currentMime: metaRouteOfficeMime, repoMime: "image/png", outboxOn: true, wantOutbox: 1, wantMetric: 1, wantPriority: 0},
		{name: "outbox-off-uses-repo", wasTemporary: true, currentMime: "image/png", repoMime: "image/png", outboxOn: false, wantRepo: 1},
		{name: "already-durable-no-transition", wasTemporary: false, currentMime: "image/png", repoMime: "image/png", outboxOn: true, wantRepo: 1},
		{name: "keeps-temporary-no-enqueue", wasTemporary: true, currentMime: "image/png", repoMime: "image/png", outboxOn: true, updateTemp: true, wantRepo: 1},
		// Metric-guard error path: a transition whose outbox write fails must propagate the error
		// AND leave the counter untouched (the `if err == nil { Add(1) }` guard). Priority is still
		// computed from the authoritative RETURNING mime (office → hot=1) before the failure.
		{name: "transition-outbox-error-no-metric", wasTemporary: true, currentMime: "image/png", repoMime: metaRouteOfficeMime, outboxOn: true, outboxErr: ErrConflict, wantOutbox: 1, wantMetric: 0, wantErr: true, wantPriority: 1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// repo.doc supplies the AUTHORITATIVE post-update row on BOTH paths — the adapter maps it
			// from the UPDATE's full RETURNING (externalID/size + dims + mime). repoMime is the
			// authoritative post-update mime that drives the enqueued priority; it is the opposite
			// priority class from currentMime so a regression that reverts to the stale-snapshot mime flips
			// the priority assertion.
			authoritative := model.Document{
				ExternalID: metaRouteAuthExternalID, Size: metaRouteAuthSize, MimeType: tc.repoMime,
				TemporaryLocation: tc.wasTemporary,
				ImageWidth:        &metaRouteAuthW, ImageHeight: &metaRouteAuthH,
				ContentMetadata: model.ContentMetadata{Populated: true, ImageWidth: &metaRouteAuthW, ImageHeight: &metaRouteAuthH},
			}
			repo := &mockRepo{doc: authoritative}
			// The outbox returns the same authoritative row it would map in-tx (full RETURNING).
			ob := &recordingOutbox{returnDoc: authoritative, updateMetadataErr: tc.outboxErr}
			s := &FileService{Repo: repo, HotMimePrefixes: []string{"application/vnd.openxmlformats-officedocument"}, Logger: nopLogger}
			if tc.outboxOn {
				s.Outbox = ob
			}
			// The handler loads the current document and threads it in. Its content fields (incl.
			// dims) are the STALE snapshot — divergent from the authoritative post-update row.
			current := model.Document{
				ExternalID: metaRouteThreadedExternalID, Size: metaRouteThreadedSize, MimeType: tc.currentMime,
				TemporaryLocation: tc.wasTemporary,
				ImageWidth:        &metaRouteThreadedW, ImageHeight: &metaRouteThreadedH,
			}
			meta := model.DocumentMetadataUpdate{TemporaryLocation: tc.updateTemp}
			before := backupOutboxEnqueued.Value()

			updated, err := s.UpdateDocumentMetadata(context.Background(), uuid.New(), meta, 1, current)
			assertMetadataRouting(t, tc, updated, err, ob, repo, backupOutboxEnqueued.Value()-before)
		})
	}
}
