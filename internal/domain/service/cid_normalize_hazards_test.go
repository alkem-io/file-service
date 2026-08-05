package service

import (
	"context"
	"encoding/json"
	"errors"
	"math"
	"strings"
	"testing"

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
// If one reached the pacer it would collapse the interval to zero — an unbounded
// pass against shared production storage.
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
		{"vanishingly small (would overflow the interval)", 1e-12},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			p := newPacer(c.rate)
			if p.interval <= 0 {
				t.Errorf("interval = %v for rate %v — a non-positive interval is an unbounded pass", p.interval, c.rate)
			}
			if p.interval > maxPacerInterval {
				t.Errorf("interval = %v for rate %v, want at most %v", p.interval, c.rate, maxPacerInterval)
			}
		})
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
