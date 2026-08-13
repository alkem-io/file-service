package service

import (
	"context"
	"encoding/json"
	"errors"
	"math"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/alkem-io/file-service/internal/domain/model"
	"github.com/alkem-io/file-service/internal/domain/port"
)

// Every test here defends against a way this pass could destroy data or lie
// about what it did. The pass deletes the only copy of the legacy blob, so each
// of these is a one-shot: nothing downstream re-checks its work.

// io.Copy returns (n, nil) when a read ends in a clean EOF short of the
// promised length — a blipping NFS volume, a truncated blob. Publishing that
// would address a FRAGMENT as if it were the file, repoint the record to it,
// and then delete the intact original.
func TestCIDNormalize_ShortReadNeverPublishesATruncatedBlob(t *testing.T) {
	const legacy = "QmTruncatedTruncatedTruncatedTrunca"
	full := []byte("the complete forty-two byte payload, honest")

	repo := newCIDRepo(&cidRow{ID: cidUUID(1), ExternalID: legacy, Version: 1})
	store := newCIDStore()
	store.put(legacy, full)
	store.shortRead[legacy] = 9 // handle promises len(full), delivers 9 bytes, no error

	sum := newCIDSweep(repo, store).RunCIDNormalize(context.Background(), cidOpts(nil))

	if sum.Normalized != 0 || sum.Failed != 1 {
		t.Fatalf("normalized=%d failed=%d, want 0/1 — a short read must not be mistaken for the file",
			sum.Normalized, sum.Failed)
	}
	if repo.rows[0].ExternalID != legacy {
		t.Errorf("record was repointed to %q on a short read", repo.rows[0].ExternalID)
	}
	if !store.has(legacy) {
		t.Error("the intact legacy blob was deleted after a short read — that is unrecoverable data loss")
	}
	if len(store.created) != 0 {
		t.Errorf("published %v from a truncated read", store.created)
	}
	if got := sum.NotChanged[0].Reason; got != reasonReadFailed {
		t.Errorf("reason = %q, want %q (transient — a re-run should retry)", got, reasonReadFailed)
	}
}

// The scan predicate is "not the current scheme", so it can select a name the
// store itself refuses to address. That is as unfixable by re-running as absent
// content, so it must be a skip — a failure would keep the Job red forever and
// the operator would never reach a terminating condition.
func TestCIDNormalize_UnaddressableNameIsASkipNotAFailure(t *testing.T) {
	const bad = "not-a-valid-key"
	repo := newCIDRepo(&cidRow{ID: cidUUID(1), ExternalID: bad, Version: 1})
	store := newCIDStore()
	store.readErr[bad] = port.ErrInvalidKey

	sum := newCIDSweep(repo, store).RunCIDNormalize(context.Background(), cidOpts(nil))

	if sum.Failed != 0 {
		t.Errorf("failed = %d, want 0 — no re-run can repair the name, so failing is a non-terminating verdict", sum.Failed)
	}
	if sum.Skipped != 1 {
		t.Fatalf("skipped = %d, want 1", sum.Skipped)
	}
	if got := sum.NotChanged[0].Reason; got != reasonUnaddressableName {
		t.Errorf("reason = %q, want %q — it must be distinguishable from the alkemio#1995 absent-content cohort",
			got, reasonUnaddressableName)
	}
}

// A genuine I/O fault is the opposite call: a re-run can fix it, so it fails.
func TestCIDNormalize_StoreIOFaultIsAFailure(t *testing.T) {
	const legacy = "QmEioEioEioEioEioEioEioEioEioEioEio"
	repo := newCIDRepo(&cidRow{ID: cidUUID(1), ExternalID: legacy, Version: 1})
	store := newCIDStore()
	store.readErr[legacy] = errors.New("input/output error")

	sum := newCIDSweep(repo, store).RunCIDNormalize(context.Background(), cidOpts(nil))

	if sum.Failed != 1 || sum.Skipped != 0 {
		t.Errorf("failed=%d skipped=%d, want 1/0 — a transient I/O fault must make the Job rerunnable",
			sum.Failed, sum.Skipped)
	}
}

// The report is written once, at the end of a pass that may run for hours while
// deleting blobs as it goes. The journal is what bounds the loss: each mapping
// is durable BEFORE the bytes it describes are destroyed.
func TestCIDNormalize_MappingIsDurableBeforeItsBlobIsReclaimed(t *testing.T) {
	repo := newCIDRepo(
		&cidRow{ID: cidUUID(1), ExternalID: "QmFirstFirstFirstFirstFirstFirstFir", Version: 1},
		&cidRow{ID: cidUUID(2), ExternalID: "QmSecondSecondSecondSecondSecondSec", Version: 1},
	)
	store := newCIDStore()
	for i, row := range repo.rows {
		store.put(row.ExternalID, []byte{byte('a' + i)})
	}

	sink := &cidSink{}
	sink.onAppend = func(line []byte) {
		var entry map[string]any
		if err := json.Unmarshal(line, &entry); err != nil {
			t.Fatalf("journal line is not JSON: %v (%q)", err, line)
		}
		prev, _ := entry["previousExternalID"].(string)
		if !store.has(prev) {
			t.Errorf("journal recorded %q only AFTER its blob was reclaimed — a crash between the two loses the mapping forever", prev)
		}
	}

	sum := newCIDSweep(repo, store).RunCIDNormalize(context.Background(), cidOpts(sink))

	if len(sink.journalLines) != 2 {
		t.Fatalf("journal has %d lines, want one per changed record (2)", len(sink.journalLines))
	}
	if !strings.HasSuffix(sink.journalName, ".ndjson") {
		t.Errorf("journal name = %q, want an .ndjson companion to the report", sink.journalName)
	}
	// The journal alone must reconstruct old→new; that is its entire job.
	for i, line := range sink.journalLines {
		var e map[string]any
		if err := json.Unmarshal(line, &e); err != nil {
			t.Fatalf("line %d: %v", i, err)
		}
		for _, k := range []string{"fileId", "previousExternalID", "newExternalID"} {
			if v, _ := e[k].(string); v == "" {
				t.Errorf("journal line %d is missing %s", i, k)
			}
		}
	}
	if sum.ReportFailed {
		t.Error("ReportFailed set on a clean run")
	}
}

// Losing the mapping is not a cosmetic failure: the change it describes is
// irreversible and already done.
func TestCIDNormalize_LostMappingFailsTheRun(t *testing.T) {
	fixture := func() (*cidRepo, *cidStore) {
		repo := newCIDRepo(&cidRow{ID: cidUUID(1), ExternalID: "QmLostLostLostLostLostLostLostLostL", Version: 1})
		store := newCIDStore()
		store.put(repo.rows[0].ExternalID, []byte("payload"))
		return repo, store
	}

	t.Run("the final report could not be written", func(t *testing.T) {
		repo, store := fixture()
		sink := &cidSink{err: errors.New("ENOSPC")}
		sum := newCIDSweep(repo, store).RunCIDNormalize(context.Background(), cidOpts(sink))
		if !sum.ReportFailed {
			t.Error("a pass that migrated and then lost its report reported clean success")
		}
	})

	t.Run("a journal line could not be appended", func(t *testing.T) {
		repo, store := fixture()
		sink := &cidSink{journalErr: errors.New("volume read-only")}
		sum := newCIDSweep(repo, store).RunCIDNormalize(context.Background(), cidOpts(sink))
		if !sum.ReportFailed {
			t.Error("a mapping that never reached the disk did not fail the run")
		}
	})
}

// Reclamation must go through the service's single refcount-zero deletion path,
// so the pending backup hints for a hash are dropped with its bytes exactly as
// they are on every other delete. Duplicating the delete would leave hints
// pointing at content that can only 404.
func TestCIDNormalize_ReclamationDropsPendingBackupHints(t *testing.T) {
	const legacy = "QmOutboxOutboxOutboxOutboxOutboxOu"
	repo := newCIDRepo(&cidRow{ID: cidUUID(1), ExternalID: legacy, Version: 1})
	store := newCIDStore()
	store.put(legacy, []byte("payload"))

	outbox := &recordingOutbox{deletePendingN: 2}
	svc := newCIDSweep(repo, store)
	svc.Outbox = outbox

	sum := svc.RunCIDNormalize(context.Background(), cidOpts(nil))

	if sum.Reclaimed != 1 {
		t.Fatalf("reclaimed = %d, want 1", sum.Reclaimed)
	}
	if len(outbox.deletePendingFor) != 1 || outbox.deletePendingFor[0] != legacy {
		t.Errorf("DeletePendingByHash calls = %v, want exactly [%q] — a promoted legacy-named row can leave a pending hint naming the CID",
			outbox.deletePendingFor, legacy)
	}
}

// The bytes may already live under the target digest in the same bucket — the
// same asset uploaded before and after the addressing migration. No re-run
// changes that, so the operator needs a message that says what to do.
func TestCIDNormalize_DedupCollisionIsReportedAsNeedingAHuman(t *testing.T) {
	repo := newCIDRepo(&cidRow{ID: cidUUID(1), ExternalID: "QmCollideCollideCollideCollideColl", Version: 1})
	repo.normalizeErr = model.ErrDuplicateKey
	store := newCIDStore()
	store.put(repo.rows[0].ExternalID, []byte("payload"))

	sum := newCIDSweep(repo, store).RunCIDNormalize(context.Background(), cidOpts(nil))

	if sum.Failed != 1 {
		t.Fatalf("failed = %d, want 1", sum.Failed)
	}
	if got := sum.NotChanged[0].Detail; !strings.Contains(got, "already holds") || !strings.Contains(got, "manual") {
		t.Errorf("detail = %q, want it to name the collision and say a human is needed", got)
	}
	if !store.has(repo.rows[0].ExternalID) {
		t.Error("the legacy blob was reclaimed even though the repoint failed")
	}
}

// This pass deletes blobs as it runs, so dying mid-corpus is how a partially
// migrated corpus is produced. One bad record must not take the process down.
func TestCIDNormalize_PanicOnOneRecordDoesNotKillThePass(t *testing.T) {
	repo := newCIDRepo(
		&cidRow{ID: cidUUID(1), ExternalID: "QmPanicPanicPanicPanicPanicPanicPa", Version: 1},
		&cidRow{ID: cidUUID(2), ExternalID: "QmFineFineFineFineFineFineFineFine", Version: 1},
	)
	store := newCIDStore()
	for _, row := range repo.rows {
		store.put(row.ExternalID, []byte("payload for "+row.ExternalID))
	}
	repo.beforeNormalize = func(_ *cidRepo, id uuid.UUID) {
		if id == cidUUID(1) {
			panic("simulated defect in the write path")
		}
	}

	sum := newCIDSweep(repo, store).RunCIDNormalize(context.Background(), cidOpts(nil))

	if sum.Normalized != 1 {
		t.Errorf("normalized = %d, want 1 — the pass must continue past a panicking record", sum.Normalized)
	}
	if sum.Failed != 1 {
		t.Errorf("failed = %d, want 1 — this pass decodes nothing, so a panic is a defect and must fail the run", sum.Failed)
	}
	if !sha3HexName.MatchString(repo.rows[1].ExternalID) {
		t.Error("the healthy record after the panicking one was never processed")
	}
}

// The count→delete window: a copy that read its source before the repoint can
// insert a row naming the legacy blob after the count and before the delete.
// The window is not closed here (the standing fix is atomic GC), but it must not
// pass silently — the bytes are still recoverable under the new digest.
func TestCIDNormalize_ReclamationRechecksForALateReference(t *testing.T) {
	const legacy = "QmRaceRaceRaceRaceRaceRaceRaceRace"
	repo := newCIDRepo(&cidRow{ID: cidUUID(1), ExternalID: legacy, Version: 1})
	store := newCIDStore()
	store.put(legacy, []byte("payload"))

	// A copy lands in the window: the row appears after the refcount says zero.
	store.afterDelete = func() {
		repo.rows = append(repo.rows, &cidRow{ID: cidUUID(9), ExternalID: legacy, Version: 1})
	}

	before := repo.countCalls
	newCIDSweep(repo, store).RunCIDNormalize(context.Background(), cidOpts(nil))

	if repo.countCalls-before < 2 {
		t.Errorf("refcount was queried %d time(s) around one reclamation, want a re-check after the delete — otherwise a late copy 404s silently and permanently",
			repo.countCalls-before)
	}
}

// A dry run that ignores whether the content is even there over-reports the real
// pass by the size of the permanently-absent cohort — and it is the approval gate
// for an irreversible migration.
func TestCIDNormalize_DryRunCountsOnlyRecordsARealPassWouldNormalize(t *testing.T) {
	repo := newCIDRepo(
		&cidRow{ID: cidUUID(1), ExternalID: "QmPresentPresentPresentPresentPres", Version: 1},
		&cidRow{ID: cidUUID(2), ExternalID: "QmMissingMissingMissingMissingMiss", Version: 1},
		&cidRow{ID: cidUUID(3), ExternalID: "QmAlsoPresentAlsoPresentAlsoPresen", Version: 1},
	)
	store := newCIDStore()
	store.put(repo.rows[0].ExternalID, []byte("one"))
	store.put(repo.rows[2].ExternalID, []byte("three"))
	// rows[1] is the alkemio#1995 cohort: in scope, but nothing to normalize.

	opts := cidOpts(nil)
	opts.DryRun = true
	dry := newCIDSweep(repo, store).RunCIDNormalize(context.Background(), opts)

	if dry.WouldNormalize != 2 {
		t.Errorf("wouldNormalize = %d, want 2 — a record whose content is gone is not one a real pass normalizes", dry.WouldNormalize)
	}
	if dry.WouldSkip != 1 {
		t.Errorf("wouldSkip = %d, want 1", dry.WouldSkip)
	}
	if len(store.created) != 0 || len(store.deletes) != 0 || repo.normalizeCalls != 0 {
		t.Error("the existence probe was not read-only")
	}
}

// sharedWith == 0 cannot distinguish "reclaimed" from "the refcount query failed"
// from "the delete failed" — and the run report is the only place that
// distinction survives, since the row no longer matches the scan predicate.
func TestCIDNormalize_ReportRecordsWhatBecameOfEachLegacyBlob(t *testing.T) {
	cases := []struct {
		name   string
		inject func(r *cidRepo, s *cidStore)
		want   string
	}{
		{"reclaimed", func(*cidRepo, *cidStore) {}, legacyBlobReclaimed},
		{"refcount query failed", func(r *cidRepo, _ *cidStore) { r.countErr = errors.New("db down") }, legacyBlobRetained},
		{"delete failed", func(_ *cidRepo, s *cidStore) { s.deleteErr = errors.New("read-only") }, legacyBlobRetained},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			repo := newCIDRepo(&cidRow{ID: cidUUID(1), ExternalID: "QmOutcomeOutcomeOutcomeOutcomeOut", Version: 1})
			store := newCIDStore()
			store.put(repo.rows[0].ExternalID, []byte("payload"))
			c.inject(repo, store)

			sum := newCIDSweep(repo, store).RunCIDNormalize(context.Background(), cidOpts(nil))

			if len(sum.Changed) != 1 {
				t.Fatalf("changed has %d entries, want 1", len(sum.Changed))
			}
			if got := sum.Changed[0].LegacyBlob; got != c.want {
				t.Errorf("legacyBlob = %q, want %q — an operator cannot otherwise tell a reclaimed blob from one still occupying disk",
					got, c.want)
			}
		})
	}

	t.Run("still in use", func(t *testing.T) {
		const legacy = "QmSharedOutcomeSharedOutcomeShared"
		repo := newCIDRepo(
			&cidRow{ID: cidUUID(1), ExternalID: legacy, Version: 1},
			&cidRow{ID: cidUUID(2), ExternalID: legacy, Version: 1},
		)
		store := newCIDStore()
		store.put(legacy, []byte("payload"))

		sum := newCIDSweep(repo, store).RunCIDNormalize(context.Background(), cidOpts(nil))

		if sum.Changed[0].LegacyBlob != legacyBlobStillInUse {
			t.Errorf("first record's legacyBlob = %q, want %q", sum.Changed[0].LegacyBlob, legacyBlobStillInUse)
		}
		if sum.Changed[1].LegacyBlob != legacyBlobReclaimed {
			t.Errorf("last record's legacyBlob = %q, want %q", sum.Changed[1].LegacyBlob, legacyBlobReclaimed)
		}
	})
}

// NaN defeats every ordinary bound because all comparisons against it are false.
// If one reached the limiter it would stop bounding anything — round one of review
// found exactly that hole in the hand-rolled pacer this replaces, where +Inf
// divided down to a zero interval.
func TestNewPacer_BoundsPathologicalRates(t *testing.T) {
	cases := []struct {
		name string
		rate float64
	}{
		{"zero", 0},
		{"negative", -5},
		{"NaN", math.NaN()},
		{"positive infinity", math.Inf(1)},
		{"negative infinity", math.Inf(-1)},
		{"vanishingly small", 1e-12},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			p := newPacer(c.rate)
			lim := float64(p.Limit())
			if !(lim > 0) || math.IsInf(lim, 0) {
				t.Errorf("limit = %v for rate %v — that is not a bound", lim, c.rate)
			}
			if p.Burst() != 1 {
				t.Errorf("burst = %d, want 1 — a larger burst lets the pass bank idle time and spend it at once",
					p.Burst())
			}
		})
	}
}

// The ceiling has to actually throttle, not merely exist. Asserted as a lower
// bound on elapsed time, so a slow machine can never make it flaky.
func TestNewPacer_ActuallyThrottles(t *testing.T) {
	const perSecond = 50.0
	const objects = 5

	p := newPacer(perSecond)
	start := time.Now()
	for i := 0; i < objects; i++ {
		if err := p.Wait(context.Background()); err != nil {
			t.Fatalf("Wait: %v", err)
		}
	}
	// burst=1 lets the first object through immediately; the remaining four are paced.
	want := time.Duration(float64(objects-1) / perSecond * float64(time.Second))
	if elapsed := time.Since(start); elapsed < want*8/10 {
		t.Errorf("%d objects at %v/s took %v, want at least ~%v — the rate ceiling is not throttling",
			objects, perSecond, elapsed, want)
	}
}

// Shutdown must be honoured while waiting for a slot, not only between records.
func TestNewPacer_WaitHonoursCancellation(t *testing.T) {
	p := newPacer(0.01) // one object per 100s: the wait will not complete on its own
	if err := p.Wait(context.Background()); err != nil {
		t.Fatalf("first Wait should pass immediately (burst=1): %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := p.Wait(ctx); err == nil {
		t.Error("Wait ignored a cancelled context — SIGTERM would not be honoured mid-pace")
	}
}

// A store that is not there makes every read ENOENT, which this sweep correctly
// reads as "content permanently absent" — so an unmounted volume would otherwise
// produce a clean exit 0 and a report branding the whole corpus unrecoverable.
// The guard lives in the command layer; this pins the behaviour it protects.
func TestCIDNormalize_AbsentStoreLooksExactlyLikeAConvergedCorpus(t *testing.T) {
	repo := newCIDRepo(
		&cidRow{ID: cidUUID(1), ExternalID: "QmOneOneOneOneOneOneOneOneOneOneOn", Version: 1},
		&cidRow{ID: cidUUID(2), ExternalID: "QmTwoTwoTwoTwoTwoTwoTwoTwoTwoTwoTw", Version: 1},
	)
	store := newCIDStore() // nothing in it at all

	sum := newCIDSweep(repo, store).RunCIDNormalize(context.Background(), cidOpts(nil))

	if sum.Failed != 0 || sum.Aborted {
		t.Fatalf("failed=%d aborted=%v — this is the indistinguishable case the command-layer preflight exists for",
			sum.Failed, sum.Aborted)
	}
	if sum.Skipped != 2 {
		t.Fatalf("skipped = %d, want 2", sum.Skipped)
	}
	for _, n := range sum.NotChanged {
		if n.Reason != reasonContentAbsent {
			t.Errorf("reason = %q, want %q", n.Reason, reasonContentAbsent)
		}
	}
}

// Uppercase hex is one of the legacy forms known to exist on disk, and it matches
// the scan predicate precisely because the predicate demands LOWERCASE hex. Its
// bytes therefore hash to the same 64 characters in the other case — the same file
// on a case-insensitive volume, so reclaiming the legacy name would unlink the blob
// the record was just repointed to.
func TestCIDNormalize_NeverReclaimsANameThatDiffersOnlyByCase(t *testing.T) {
	content := []byte("bytes whose digest differs from the legacy name only in case")
	digest := ComputeHash(content)
	legacy := strings.ToUpper(digest)

	repo := newCIDRepo(&cidRow{ID: cidUUID(1), ExternalID: legacy, Version: 1})
	store := newCIDStore()
	store.put(legacy, content)

	sum := newCIDSweep(repo, store).RunCIDNormalize(context.Background(), cidOpts(nil))

	if sum.Normalized != 1 {
		t.Fatalf("normalized = %d, want 1 — an uppercase-hex name is still a legacy name", sum.Normalized)
	}
	if repo.rows[0].ExternalID != digest {
		t.Errorf("record names %q, want the lowercase digest %q", repo.rows[0].ExternalID, digest)
	}
	if len(store.deletes) != 0 {
		t.Errorf("deleted %v — on a case-insensitive volume that is the very blob the record now names", store.deletes)
	}
	if sum.Changed[0].LegacyBlob != legacyBlobRetained {
		t.Errorf("legacyBlob = %q, want %q so the report says the blob is still on disk",
			sum.Changed[0].LegacyBlob, legacyBlobRetained)
	}
}

// The CAS also loses to writers that bump `version` without touching `externalID` —
// a metadata PATCH. Calling that "already correct" would misreport convergence: the
// row is still legacy-named and still in scope.
func TestCIDNormalize_DistinguishesAMetadataRaceFromAContentRace(t *testing.T) {
	t.Run("content replace leaves the row correctly addressed", func(t *testing.T) {
		repo := newCIDRepo(&cidRow{ID: cidUUID(1), ExternalID: "QmContentRaceContentRaceContentRa", Version: 1})
		store := newCIDStore()
		store.put(repo.rows[0].ExternalID, []byte("payload"))
		repo.beforeNormalize = func(r *cidRepo, id uuid.UUID) {
			row := r.row(id)
			row.ExternalID = ComputeHash([]byte("what the user uploaded instead"))
			row.Version++
		}

		sum := newCIDSweep(repo, store).RunCIDNormalize(context.Background(), cidOpts(nil))

		if got := sum.NotChanged[0].Detail; strings.Contains(got, "STILL legacy-named") {
			t.Errorf("detail = %q, but the concurrent write left a proper digest", got)
		}
	})

	t.Run("metadata patch leaves the row still legacy-named", func(t *testing.T) {
		repo := newCIDRepo(&cidRow{ID: cidUUID(1), ExternalID: "QmMetadataRaceMetadataRaceMetada", Version: 1})
		store := newCIDStore()
		store.put(repo.rows[0].ExternalID, []byte("payload"))
		// A PATCH bumps version and touches nothing else.
		repo.beforeNormalize = func(r *cidRepo, id uuid.UUID) { r.row(id).Version++ }

		sum := newCIDSweep(repo, store).RunCIDNormalize(context.Background(), cidOpts(nil))

		if sum.Skipped != 1 || sum.Failed != 0 {
			t.Fatalf("skipped=%d failed=%d, want 1/0", sum.Skipped, sum.Failed)
		}
		if got := sum.NotChanged[0].Detail; !strings.Contains(got, "STILL legacy-named") {
			t.Errorf("detail = %q, want it to say the record remains in scope — a summary claiming it is already correct misreports convergence", got)
		}
		// And it must genuinely still be in scope for the next pass.
		page, _ := repo.ListLegacyNamed(context.Background(), uuid.Nil, 10)
		if len(page) != 1 {
			t.Errorf("the record left the work-list despite never being renamed")
		}
	})
}

// A legacy blob backing K records must be read and re-written ONCE, not K times —
// every extra cycle is load on the shared volume the rate ceiling exists to protect.
func TestCIDNormalize_SharedBlobIsReadAndRepublishedOnce(t *testing.T) {
	const legacy = "QmMemoMemoMemoMemoMemoMemoMemoMem"
	repo := newCIDRepo(
		&cidRow{ID: cidUUID(1), ExternalID: legacy, Version: 1},
		&cidRow{ID: cidUUID(2), ExternalID: legacy, Version: 1},
		&cidRow{ID: cidUUID(3), ExternalID: legacy, Version: 1},
	)
	store := newCIDStore()
	store.put(legacy, []byte("one blob, three records"))

	sum := newCIDSweep(repo, store).RunCIDNormalize(context.Background(), cidOpts(nil))

	if sum.Normalized != 3 {
		t.Fatalf("normalized = %d, want 3", sum.Normalized)
	}
	if store.reads[legacy] != 1 {
		t.Errorf("read the shared legacy blob %d times, want 1 — the other reads are pure waste against a shared production volume",
			store.reads[legacy])
	}
	if store.stages != 1 {
		t.Errorf("opened %d staging writes, want 1 — the rest would be written only for Commit to discard them as dedup hits",
			store.stages)
	}
}

// The loop must not pay for one more scan of a predicate Postgres cannot index just
// to be told the corpus is exhausted.
func TestCIDNormalize_DoesNotScanPastTheLastShortPage(t *testing.T) {
	repo := newCIDRepo(&cidRow{ID: cidUUID(1), ExternalID: "QmOnePageOnePageOnePageOnePageOn", Version: 1})
	store := newCIDStore()
	store.put(repo.rows[0].ExternalID, []byte("payload"))

	newCIDSweep(repo, store).RunCIDNormalize(context.Background(), cidOpts(nil))

	if repo.listCalls != 1 {
		t.Errorf("issued %d page scans for a single short page, want 1", repo.listCalls)
	}
}

// A blob copy that ignores cancellation absorbs SIGTERM until it finishes; if that
// outlasts the grace period kubernetes SIGKILLs the pod, which is the one exit path
// that skips the end-of-pass report.
//
// The cancellation must land MID-COPY. Cancelling before the pass starts proves
// nothing: the loop checks ctx at the top of every page and every record, so such a
// test never reaches the copy at all.
func TestCIDNormalize_CopyHonoursCancellationMidBlob(t *testing.T) {
	repo := newCIDRepo(&cidRow{ID: cidUUID(1), ExternalID: "QmCancelCancelCancelCancelCancel", Version: 1})
	store := newCIDStore()
	store.put(repo.rows[0].ExternalID, []byte("payload"))

	ctx, cancel := context.WithCancel(context.Background())
	// Fires once the record has been selected and its blob opened — past every
	// loop-level ctx check, so only the copy can observe it.
	store.onReadStream = func() { cancel() }

	sum := newCIDSweep(repo, store).RunCIDNormalize(ctx, cidOpts(nil))

	if store.reads[repo.rows[0].ExternalID] == 0 {
		t.Fatal("the blob was never opened — the test did not reach the copy path it claims to exercise")
	}
	if sum.Normalized != 0 {
		t.Errorf("normalized %d records after the copy was cancelled", sum.Normalized)
	}
	if !sum.Aborted {
		t.Error("a shutdown during the copy did not mark the pass Aborted — the pass would report 'completed' over a corpus it never finished")
	}
	// A shutdown is not the record's fault, and the durable report must not say it was.
	if sum.Failed != 0 {
		t.Errorf("failed = %d, want 0 — a signal is not an I/O fault of the record being processed", sum.Failed)
	}
	if len(store.deletes) != 0 {
		t.Errorf("reclaimed %v during a cancelled pass", store.deletes)
	}
}

// The per-record arrays are a convenience copy; the journal is the durable record.
// A report that lists fewer entries than its counts must say so rather than look
// like a shorter pass than actually ran.
func TestCIDNormalize_TruncatedDetailIsDeclared(t *testing.T) {
	sum := CIDNormalizeSummary{}
	for i := 0; i < cidRetainedDetailLimit+5; i++ {
		sum.recordChanged(CIDNormalizeChange{PreviousExternalID: "Qm", NewExternalID: "ab"})
	}
	if len(sum.Changed) != cidRetainedDetailLimit {
		t.Errorf("retained %d entries, want the ceiling of %d", len(sum.Changed), cidRetainedDetailLimit)
	}
	if !sum.DetailTruncated {
		t.Error("hit the ceiling without declaring truncation — the report would read as a shorter pass")
	}
}

// The memo makes a shared legacy blob publish once. If a record's compare-and-set
// then fails and the orphan reclaim deletes that published blob, the memo must not
// keep handing the dead name to the next record sharing the same legacy blob —
// which would repoint a live record at nothing and then reclaim its legacy copy too.
func TestCIDNormalize_OrphanReclaimDoesNotPoisonTheMemo(t *testing.T) {
	const legacy = "QmPoisonPoisonPoisonPoisonPoison"
	content := []byte("shared by two records")
	digest := ComputeHash(content)

	repo := newCIDRepo(
		&cidRow{ID: cidUUID(1), ExternalID: legacy, Version: 1},
		&cidRow{ID: cidUUID(2), ExternalID: legacy, Version: 1},
	)
	store := newCIDStore()
	store.put(legacy, content)

	// The FIRST record loses its compare-and-set; the second proceeds normally.
	repo.beforeNormalize = func(r *cidRepo, id uuid.UUID) {
		if id == cidUUID(1) {
			r.row(id).Version++
		}
	}

	sum := newCIDSweep(repo, store).RunCIDNormalize(context.Background(), cidOpts(nil))

	if sum.Normalized != 1 || sum.Skipped != 1 {
		t.Fatalf("normalized=%d skipped=%d, want 1/1", sum.Normalized, sum.Skipped)
	}
	second := repo.row(cidUUID(2))
	if second.ExternalID != digest {
		t.Fatalf("second record names %q, want %q", second.ExternalID, digest)
	}
	// The whole point: whatever the second record names must actually be on the store.
	if !store.has(second.ExternalID) {
		t.Error("a live record names a blob that is not on the store — the orphan reclaim deleted a digest the memo kept serving")
	}
}

// The journal ordering is the guarantee. If a line cannot be written, the blob it
// describes must survive — destroying bytes whose old name was never recorded is
// exactly the loss the journal exists to prevent.
func TestCIDNormalize_UnjournalledRecordKeepsItsLegacyBlob(t *testing.T) {
	const legacy = "QmNoJournalNoJournalNoJournalNoJ"
	repo := newCIDRepo(&cidRow{ID: cidUUID(1), ExternalID: legacy, Version: 1})
	store := newCIDStore()
	store.put(legacy, []byte("payload"))

	sink := &cidSink{journalErr: errors.New("volume read-only")}
	sum := newCIDSweep(repo, store).RunCIDNormalize(context.Background(), cidOpts(sink))

	if sum.Normalized != 1 {
		t.Fatalf("normalized = %d, want 1 — the repoint itself succeeded", sum.Normalized)
	}
	if !store.has(legacy) {
		t.Error("the legacy blob was reclaimed even though its mapping never reached the disk")
	}
	if sum.Reclaimed != 0 {
		t.Errorf("reclaimed = %d, want 0", sum.Reclaimed)
	}
	if sum.Changed[0].LegacyBlob != legacyBlobRetained {
		t.Errorf("legacyBlob = %q, want %q", sum.Changed[0].LegacyBlob, legacyBlobRetained)
	}
	if !sum.ReportFailed {
		t.Error("a run that could not record a mapping reported clean success")
	}
}

// A journal line is written before the reference count is taken, so it cannot know
// sharedWith or the blob's fate. Emitting the report struct would stamp a confident
// "sharedWith": 0 on every line and a reader recovering from the journal would
// conclude every shared legacy blob was unshared.
func TestCIDNormalize_JournalLinesClaimOnlyWhatIsKnown(t *testing.T) {
	const legacy = "QmJournalShapeJournalShapeJourn"
	repo := newCIDRepo(
		&cidRow{ID: cidUUID(1), ExternalID: legacy, Version: 1},
		&cidRow{ID: cidUUID(2), ExternalID: legacy, Version: 1},
	)
	store := newCIDStore()
	store.put(legacy, []byte("shared"))

	sink := &cidSink{}
	newCIDSweep(repo, store).RunCIDNormalize(context.Background(), cidOpts(sink))

	if len(sink.journalLines) != 2 {
		t.Fatalf("journal has %d lines, want 2", len(sink.journalLines))
	}
	for i, line := range sink.journalLines {
		var e map[string]any
		if err := json.Unmarshal(line, &e); err != nil {
			t.Fatalf("line %d: %v", i, err)
		}
		assertExactKeys(t, "journal line", e, "fileId", "previousExternalID", "newExternalID")
	}
}
