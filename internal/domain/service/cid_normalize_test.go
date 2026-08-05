package service

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/google/uuid"
	"go.uber.org/zap"
)

// cidTestRate keeps the pacer out of the way — throughput bounding is exercised
// by its own test, not paid for by every other one.
const cidTestRate = 10_000

func newCIDSweep(repo *cidRepo, store *cidStore) *FileService {
	return &FileService{Repo: repo, Storage: store, Logger: zap.NewNop()}
}

func cidOpts(sink *cidSink) CIDNormalizeOptions {
	o := CIDNormalizeOptions{Rate: cidTestRate}
	if sink != nil {
		o.Report = sink
	}
	return o
}

// FR-004 is the requirement that makes this sweep safe to run against live
// traffic: the serve path resolves a record and then reads the blob it names, so
// if a record ever names a blob that is not there, a real user gets a 404 for
// content that exists. Publish→repoint→reclaim is what prevents that, and the
// ordering only matters when something goes wrong in between — so the invariant
// is checked at every seam, under every injected fault.
func TestCIDNormalize_NoRecordEverNamesAnAbsentBlob(t *testing.T) {
	faults := []struct {
		name string
		// minChecks is how many times the invariant must actually be evaluated,
		// so a case cannot pass by never reaching the code it is meant to probe.
		// "publish fails" reaches no seam at all — the pass is a no-op — so its
		// floor is just the post-pass check.
		minChecks int
		inject    func(r *cidRepo, s *cidStore)
	}{
		{"clean pass", 7, func(*cidRepo, *cidStore) {}},
		{"publish fails", 1, func(_ *cidRepo, s *cidStore) { s.commitErr = errors.New("stage commit failed") }},
		{"repoint fails after publish", 4, func(r *cidRepo, _ *cidStore) { r.normalizeErr = errors.New("db down") }},
		{"refcount fails after repoint", 7, func(r *cidRepo, _ *cidStore) { r.countErr = errors.New("db down") }},
		{"reclaim fails after repoint", 7, func(_ *cidRepo, s *cidStore) { s.deleteErr = errors.New("volume read-only") }},
	}

	for _, f := range faults {
		t.Run(f.name, func(t *testing.T) {
			repo := newCIDRepo(
				&cidRow{ID: cidUUID(1), ExternalID: "QmAlphaAlphaAlphaAlphaAlphaAlphaAlpha", Version: 1},
				&cidRow{ID: cidUUID(2), ExternalID: "QmBetaBetaBetaBetaBetaBetaBetaBetaBeta", Version: 3},
				&cidRow{ID: cidUUID(3), ExternalID: "QmGammaGammaGammaGammaGammaGammaGamma", Version: 7},
			)
			store := newCIDStore()
			for _, row := range repo.rows {
				store.put(row.ExternalID, []byte("bytes of "+row.ExternalID))
			}

			checks := 0
			verify := func() {
				checks++
				for _, row := range repo.rows {
					if !store.has(row.ExternalID) {
						t.Errorf("[%s] record %s names %q, which is NOT on the store — the serve path would 404 real content",
							f.name, row.ID, row.ExternalID)
					}
				}
			}
			store.afterCommit = verify
			store.afterDelete = verify
			repo.afterNormalize = func(*cidRepo, uuid.UUID) { verify() }

			f.inject(repo, store)
			newCIDSweep(repo, store).RunCIDNormalize(context.Background(), cidOpts(nil))

			verify() // and once more after the pass, whatever state it ended in
			if checks < f.minChecks {
				t.Fatalf("invariant was checked %d time(s), want >= %d — the seams were never reached, so this proves nothing",
					checks, f.minChecks)
			}
		})
	}
}

// One legacy blob may back several records (FR-005). Publishing must happen once
// (the store dedups), every referencing record must be repointed, and the legacy
// blob must survive until the LAST reference goes — deleting it earlier would
// break the records not yet repointed.
func TestCIDNormalize_SharedLegacyBlobReclaimedOnceAtRefcountZero(t *testing.T) {
	const legacy = "QmSharedSharedSharedSharedSharedShared"
	repo := newCIDRepo(
		&cidRow{ID: cidUUID(1), ExternalID: legacy, Version: 1},
		&cidRow{ID: cidUUID(2), ExternalID: legacy, Version: 1},
		&cidRow{ID: cidUUID(3), ExternalID: legacy, Version: 1},
	)
	store := newCIDStore()
	store.put(legacy, []byte("one blob, three records"))

	// Each time a record is repointed, any record still naming the legacy blob
	// must still be able to read it.
	repo.afterNormalize = func(r *cidRepo, _ uuid.UUID) {
		stillReferencing := 0
		for _, row := range r.rows {
			if row.ExternalID == legacy {
				stillReferencing++
			}
		}
		if stillReferencing > 0 && !store.has(legacy) {
			t.Fatalf("legacy blob reclaimed while %d record(s) still referenced it", stillReferencing)
		}
	}

	sum := newCIDSweep(repo, store).RunCIDNormalize(context.Background(), cidOpts(nil))

	if len(store.created) != 1 {
		t.Errorf("published %d blobs (%v), want exactly 1 — the content-addressed store must dedup", len(store.created), store.created)
	}
	if repo.normalizeApplied != 3 {
		t.Errorf("repointed %d records, want 3 — every referencing record must be fixed", repo.normalizeApplied)
	}
	if sum.Normalized != 3 || sum.Reclaimed != 1 {
		t.Errorf("normalized=%d reclaimed=%d, want 3 and 1", sum.Normalized, sum.Reclaimed)
	}
	if len(store.deletes) != 1 || store.deletes[0] != legacy {
		t.Errorf("deletes = %v, want exactly one delete of %q", store.deletes, legacy)
	}
	if store.has(legacy) {
		t.Error("legacy blob still present after its last reference went away")
	}

	// The report explains WHY reclamation waited: the earlier records were shared.
	wantShared := []int{2, 1, 0}
	for i, c := range sum.Changed {
		if c.SharedWith != wantShared[i] {
			t.Errorf("change[%d].SharedWith = %d, want %d", i, c.SharedWith, wantShared[i])
		}
	}
}

// The legacy blob is reclaimed in the same pass that renames it, so once the
// process exits the report is the ONLY record of the old name (SC-009). It has
// to satisfy the contract that future readers will parse it against.
func TestCIDNormalize_ReportReconstructsTheMapping(t *testing.T) {
	repo := newCIDRepo(
		&cidRow{ID: cidUUID(1), ExternalID: "QmHasContentHasContentHasContentHas", Version: 1},
		&cidRow{ID: cidUUID(2), ExternalID: "QmGoneGoneGoneGoneGoneGoneGoneGone", Version: 1},
		&cidRow{ID: cidUUID(3), ExternalID: "QmAlsoHereAlsoHereAlsoHereAlsoHere", Version: 2},
	)
	store := newCIDStore()
	store.put(repo.rows[0].ExternalID, []byte("first payload"))
	store.put(repo.rows[2].ExternalID, []byte("third payload"))
	// rows[1]'s blob is deliberately absent — the alkemio#1995 cohort.

	wantMapping := map[string]string{
		repo.rows[0].ExternalID: ComputeHash([]byte("first payload")),
		repo.rows[2].ExternalID: ComputeHash([]byte("third payload")),
	}

	sink := &cidSink{}
	sum := newCIDSweep(repo, store).RunCIDNormalize(context.Background(), cidOpts(sink))

	if sink.writes != 1 {
		t.Fatalf("report written %d times, want 1", sink.writes)
	}
	assertReportName(t, sink.name)

	var rep map[string]any
	if err := json.Unmarshal(sink.data, &rep); err != nil {
		t.Fatalf("report is not valid JSON: %v", err)
	}

	assertReportShape(t, rep)
	counts, _ := rep["counts"].(map[string]any)
	if counts["normalized"] != float64(2) || counts["skipped"] != float64(1) || counts["failed"] != float64(0) {
		t.Errorf("counts = %v, want normalized 2 / skipped 1 / failed 0", counts)
	}

	// The whole point: old→new must be recoverable for every changed record.
	got := readReportMapping(t, rep)
	if len(got) != 2 {
		t.Fatalf("changed has %d entries, want 2", len(got))
	}
	for prev, next := range wantMapping {
		if got[prev] != next {
			t.Errorf("report maps %q → %q, want %q", prev, got[prev], next)
		}
	}

	// The unfixable cohort is recorded with its reason, so a reader can tell it
	// apart from a record that merely lost a race.
	assertOnlyNotNormalized(t, rep, repo.rows[1].ExternalID, reasonContentAbsent)
	if sum.Failed != 0 {
		t.Errorf("Failed = %d, want 0 — absent content is a skip, not a failure", sum.Failed)
	}
}

// Settled decision #1: the legacy name is never decoded or checked against the
// bytes. These objects predate content addressing with unknown write history, so
// the bytes on the store are the only truth available; adopting their digest
// makes the new address correct by construction. A record whose bytes do not
// correspond to its legacy name must therefore be normalized like any other —
// not rejected, not quarantined (FR-006).
func TestCIDNormalize_DoesNotVerifyTheLegacyName(t *testing.T) {
	const legacy = "QmThisNameHasNothingToDoWithTheBytes"
	content := []byte("bytes that were overwritten years ago")

	repo := newCIDRepo(&cidRow{ID: cidUUID(1), ExternalID: legacy, Version: 4})
	store := newCIDStore()
	store.put(legacy, content)

	sum := newCIDSweep(repo, store).RunCIDNormalize(context.Background(), cidOpts(nil))

	if sum.Normalized != 1 || sum.Skipped != 0 || sum.Failed != 0 {
		t.Fatalf("normalized=%d skipped=%d failed=%d, want 1/0/0 — a bytes/name mismatch must not block the rename",
			sum.Normalized, sum.Skipped, sum.Failed)
	}
	want := ComputeHash(content)
	if repo.rows[0].ExternalID != want {
		t.Errorf("record now names %q, want the digest of the bytes as found (%q)", repo.rows[0].ExternalID, want)
	}
	if !store.has(want) {
		t.Error("the bytes were not published under their digest")
	}
}

// assertReportName checks the file name an operator globs for.
func assertReportName(t *testing.T, name string) {
	t.Helper()
	if !strings.HasPrefix(name, "sweep-cids-") || !strings.HasSuffix(name, ".json") {
		t.Errorf("report name = %q, want sweep-cids-<timestamp>.json", name)
	}
}

// assertOnlyNotNormalized checks the single expected untouched record and its
// reason — the field that tells a permanently-unfixable record apart from one
// that merely lost a race.
func assertOnlyNotNormalized(t *testing.T, rep map[string]any, wantExternalID, wantReason string) {
	t.Helper()
	entries, _ := rep["notNormalized"].([]any)
	if len(entries) != 1 {
		t.Fatalf("notNormalized has %d entries, want 1", len(entries))
	}
	e, ok := entries[0].(map[string]any)
	if !ok {
		t.Fatalf("notNormalized entry %v is not an object", entries[0])
	}
	assertExactKeys(t, "notNormalized entry", e, "fileId", "externalID", "reason", "detail")
	if e["reason"] != wantReason {
		t.Errorf("reason = %v, want %q", e["reason"], wantReason)
	}
	if e["externalID"] != wantExternalID {
		t.Errorf("notNormalized names %v, want %q", e["externalID"], wantExternalID)
	}
}

// assertReportShape pins the report to the contract every retained report will
// be parsed against: exactly the declared properties, and the run-level fields a
// reader keys off.
func assertReportShape(t *testing.T, rep map[string]any) {
	t.Helper()
	assertExactKeys(t, "report", rep,
		"schemaVersion", "startedAt", "finishedAt", "rate", "outcome", "counts", "changed", "notNormalized")
	if rep["schemaVersion"] != float64(cidReportSchemaVersion) {
		t.Errorf("schemaVersion = %v, want %d", rep["schemaVersion"], cidReportSchemaVersion)
	}
	if rep["outcome"] != cidOutcomeCompleted {
		t.Errorf("outcome = %v, want %q", rep["outcome"], cidOutcomeCompleted)
	}
	counts, ok := rep["counts"].(map[string]any)
	if !ok {
		t.Fatalf("counts = %v, want an object", rep["counts"])
	}
	assertExactKeys(t, "counts", counts, "normalized", "skipped", "failed", "reclaimed")
}

// readReportMapping reconstructs old→new from the report the way an operator
// would after the legacy blobs are gone — validating each entry on the way.
func readReportMapping(t *testing.T, rep map[string]any) map[string]string {
	t.Helper()
	changed, _ := rep["changed"].([]any)
	out := map[string]string{}
	for _, raw := range changed {
		e, ok := raw.(map[string]any)
		if !ok {
			t.Fatalf("changed entry %v is not an object", raw)
		}
		assertExactKeys(t, "changed entry", e, "fileId", "previousExternalID", "newExternalID", "sharedWith", "legacyBlob")
		prev, _ := e["previousExternalID"].(string)
		next, _ := e["newExternalID"].(string)
		if !sha3HexName.MatchString(next) {
			t.Errorf("newExternalID %q is not a SHA3-256 hex name", next)
		}
		id, _ := e["fileId"].(string)
		if _, err := uuid.Parse(id); err != nil {
			t.Errorf("fileId %q is not a uuid: %v", id, err)
		}
		out[prev] = next
	}
	return out
}

// assertExactKeys pins a JSON object to exactly the contract's property set —
// a missing required field and an invented one are both contract breaks.
func assertExactKeys(t *testing.T, what string, obj map[string]any, keys ...string) {
	t.Helper()
	want := map[string]bool{}
	for _, k := range keys {
		want[k] = true
		if _, ok := obj[k]; !ok {
			t.Errorf("%s is missing required key %q", what, k)
		}
	}
	for k := range obj {
		if !want[k] {
			t.Errorf("%s carries unknown key %q (the schema sets additionalProperties:false)", what, k)
		}
	}
}

// The migration is irreversible and the production count is unmeasured, so the
// preview is what makes the real run approvable. FR-007 requires it to make "no
// change of any kind" — which includes not writing a report, since the report
// lands on the same mounted volume.
func TestCIDNormalize_DryRunChangesNothingAndPredictsTheRealPass(t *testing.T) {
	newFixture := func() (*cidRepo, *cidStore) {
		repo := newCIDRepo(
			&cidRow{ID: cidUUID(1), ExternalID: "QmOneOneOneOneOneOneOneOneOneOneOne", Version: 1},
			&cidRow{ID: cidUUID(2), ExternalID: "QmTwoTwoTwoTwoTwoTwoTwoTwoTwoTwoTwo", Version: 1},
			// Already content-addressed, and a temporary record: both out of scope,
			// so the preview must not count them.
			&cidRow{ID: cidUUID(3), ExternalID: ComputeHash([]byte("already normalized")), Version: 1},
			&cidRow{ID: cidUUID(4), ExternalID: "QmTemporaryTemporaryTemporaryTempor", Version: 1, Temporary: true},
		)
		store := newCIDStore()
		store.put(repo.rows[0].ExternalID, []byte("one"))
		store.put(repo.rows[1].ExternalID, []byte("two"))
		store.put(repo.rows[2].ExternalID, []byte("already normalized"))
		store.put(repo.rows[3].ExternalID, []byte("temp"))
		return repo, store
	}

	repo, store := newFixture()
	sink := &cidSink{}
	opts := cidOpts(sink)
	opts.DryRun = true
	dry := newCIDSweep(repo, store).RunCIDNormalize(context.Background(), opts)

	if dry.WouldNormalize != 2 {
		t.Errorf("wouldNormalize = %d, want 2 (already-addressed and temporary records are out of scope)", dry.WouldNormalize)
	}
	if dry.Normalized != 0 || dry.Reclaimed != 0 {
		t.Errorf("a dry run reported normalized=%d reclaimed=%d, want 0/0", dry.Normalized, dry.Reclaimed)
	}
	if len(store.created) != 0 || len(store.deletes) != 0 {
		t.Errorf("dry run wrote to the store: created=%v deletes=%v", store.created, store.deletes)
	}
	if repo.normalizeCalls != 0 {
		t.Errorf("dry run made %d database writes, want 0", repo.normalizeCalls)
	}
	if sink.writes != 0 {
		t.Errorf("dry run wrote %d report(s); FR-007 allows no change of any kind, and the report lands on the mounted volume", sink.writes)
	}
	for _, row := range repo.rows {
		if row.Version != 1 {
			t.Errorf("dry run bumped version of %s to %d", row.ID, row.Version)
		}
	}

	// The preview is only useful if it predicts the real pass on the same corpus.
	realRepo, realStore := newFixture()
	actual := newCIDSweep(realRepo, realStore).RunCIDNormalize(context.Background(), cidOpts(nil))
	if actual.Normalized != dry.WouldNormalize {
		t.Errorf("real pass normalized %d, dry run predicted %d — a preview that misleads is worse than none",
			actual.Normalized, dry.WouldNormalize)
	}
}

// A multi-hour pass over shared production infrastructure will be interrupted,
// and an operator will re-run it. Resumability here is implicit rather than
// stateful: normalizing a record removes it from the predicate, so there is no
// cursor to reconcile — which is also what makes the pass repeatable.
func TestCIDNormalize_ResumableAndRepeatable(t *testing.T) {
	repo := newCIDRepo(
		&cidRow{ID: cidUUID(1), ExternalID: "QmFirstFirstFirstFirstFirstFirstFir", Version: 1},
		&cidRow{ID: cidUUID(2), ExternalID: "QmSecondSecondSecondSecondSecondSec", Version: 1},
		&cidRow{ID: cidUUID(3), ExternalID: "QmThirdThirdThirdThirdThirdThirdThi", Version: 1},
	)
	store := newCIDStore()
	for i, row := range repo.rows {
		store.put(row.ExternalID, []byte{byte('a' + i)})
	}
	sweep := newCIDSweep(repo, store)

	// Interrupt right after the first record is published, the way a SIGTERM
	// during a rolling node drain would.
	ctx, cancel := context.WithCancel(context.Background())
	store.afterCommit = func() { cancel() }
	first := sweep.RunCIDNormalize(ctx, cidOpts(nil))
	store.afterCommit = nil

	if !first.Aborted {
		t.Fatal("interrupted pass did not report Aborted — an operator would read a partial sweep as a finished one")
	}
	if first.Normalized != 1 {
		t.Fatalf("interrupted pass normalized %d records, want 1", first.Normalized)
	}

	// Re-run: it must finish the remainder without redoing the first record.
	second := sweep.RunCIDNormalize(context.Background(), cidOpts(nil))
	if second.Aborted {
		t.Error("the resumed pass reported Aborted")
	}
	if second.Normalized != 2 {
		t.Errorf("resumed pass normalized %d, want the 2 remaining records", second.Normalized)
	}
	if repo.normalizeApplied != 3 {
		t.Errorf("%d total repoints across both passes, want 3 — no record may be processed twice", repo.normalizeApplied)
	}
	for _, row := range repo.rows {
		if !sha3HexName.MatchString(row.ExternalID) {
			t.Errorf("record %s still names %q after the resumed pass", row.ID, row.ExternalID)
		}
		if row.Version != 2 {
			t.Errorf("record %s is at version %d, want exactly one bump", row.ID, row.Version)
		}
	}

	// Repeatable: a pass over a converged corpus is a no-op, which is also how an
	// operator confirms the migration is done.
	beforeCreated, beforeDeletes := len(store.created), len(store.deletes)
	third := sweep.RunCIDNormalize(context.Background(), cidOpts(nil))
	if third.Normalized != 0 || third.Skipped != 0 || third.Failed != 0 {
		t.Errorf("a converged corpus reported normalized=%d skipped=%d failed=%d, want all zero",
			third.Normalized, third.Skipped, third.Failed)
	}
	if len(store.created) != beforeCreated || len(store.deletes) != beforeDeletes {
		t.Error("a pass over a converged corpus still touched the store")
	}
}

// The sweep runs against a live platform, so a user's Replace or a
// temporary→permanent promote can land between the sweep's read and its write.
// The user's change must win: it already writes a proper digest, so retrying
// would fight a writer that has fixed the problem for us.
func TestCIDNormalize_ConcurrentWriterWinsAndIsCountedSkipped(t *testing.T) {
	const legacy = "QmRacedRacedRacedRacedRacedRacedRac"
	replacement := ComputeHash([]byte("what the user uploaded instead"))

	repo := newCIDRepo(&cidRow{ID: cidUUID(1), ExternalID: legacy, Version: 5})
	store := newCIDStore()
	store.put(legacy, []byte("the old bytes"))

	// A content Replace lands in the window between the sweep's page read and its
	// compare-and-set: new name, version bumped.
	repo.beforeNormalize = func(r *cidRepo, id uuid.UUID) {
		row := r.row(id)
		row.ExternalID = replacement
		row.Version++
	}

	sink := &cidSink{}
	sum := newCIDSweep(repo, store).RunCIDNormalize(context.Background(), cidOpts(sink))

	if sum.Skipped != 1 || sum.Normalized != 0 || sum.Failed != 0 {
		t.Fatalf("skipped=%d normalized=%d failed=%d, want 1/0/0 — losing a race is not a failure",
			sum.Skipped, sum.Normalized, sum.Failed)
	}
	if repo.rows[0].ExternalID != replacement || repo.rows[0].Version != 6 {
		t.Errorf("record is %q@v%d, want the concurrent writer's %q@v6 — the user's change was lost",
			repo.rows[0].ExternalID, repo.rows[0].Version, replacement)
	}
	if len(store.deletes) != 0 {
		t.Errorf("deleted %v after losing the race; the sweep must not reclaim a blob it did not repoint away from", store.deletes)
	}
	if len(sum.NotChanged) != 1 || sum.NotChanged[0].Reason != reasonConcurrentChange {
		t.Errorf("notChanged = %+v, want one entry with reason %q", sum.NotChanged, reasonConcurrentChange)
	}
}

// A page-scan error must end the pass rather than silently truncating it — an
// operator reading "completed" over a half-swept corpus would stop re-running.
func TestCIDNormalize_PageScanErrorEndsThePassEarly(t *testing.T) {
	repo := newCIDRepo(&cidRow{ID: cidUUID(1), ExternalID: "QmScanScanScanScanScanScanScanScanS", Version: 1})
	repo.listErrOn = 1
	store := newCIDStore()

	sum := newCIDSweep(repo, store).RunCIDNormalize(context.Background(), cidOpts(nil))

	if !sum.Aborted {
		t.Error("a failed page scan did not mark the pass Aborted")
	}
	if sum.Normalized != 0 {
		t.Errorf("normalized = %d, want 0", sum.Normalized)
	}
}
