package service

import (
	"context"
	"errors"
	"testing"
)

// cidTestRate keeps the pacer out of the way — throughput bounding has its own test.
const cidTestRate = 10_000

func newCIDSweep(repo *cidRepo, store *cidStore) *FileService {
	return &FileService{Repo: repo, Storage: store, Logger: testLogger()}
}

func cidOpts(sink *cidSink) CIDNormalizeOptions {
	if sink == nil {
		sink = &cidSink{}
	}
	return CIDNormalizeOptions{Rate: cidTestRate, Report: sink}
}

// The whole migration, on the shape it was designed for: one legacy blob, several
// rows naming it. One UPDATE moves all of them, so there is no refcount to track
// and no per-row bookkeeping.
func TestCIDNormalize_OneUpdateMovesEveryRowSharingABlob(t *testing.T) {
	const legacy = "QmSharedSharedSharedSharedSharedShared"
	content := []byte("one blob, three rows")
	digest := ComputeHash(content)

	repo := newCIDRepo(
		&cidRow{ExternalID: legacy},
		&cidRow{ExternalID: legacy},
		&cidRow{ExternalID: legacy},
	)
	store := newCIDStore()
	store.put(legacy, content)

	sum := newCIDSweep(repo, store).RunCIDNormalize(context.Background(), cidOpts(nil))

	if sum.Normalized != 1 || sum.Records != 3 {
		t.Errorf("normalized=%d records=%d, want 1 blob and 3 rows", sum.Normalized, sum.Records)
	}
	if repo.normalizeCalls != 1 {
		t.Errorf("%d rename statements for one blob, want 1 — the point of the blob-oriented design", repo.normalizeCalls)
	}
	for _, row := range repo.rows {
		if row.ExternalID != digest {
			t.Errorf("row still names %q, want %q", row.ExternalID, digest)
		}
	}
	if !store.has(digest) {
		t.Error("content is not available under its digest")
	}
	// Parked, not deleted — the property that makes this reversible.
	if store.has(legacy) {
		t.Error("the legacy name is still in the content namespace")
	}
	if _, ok := store.parked[legacy]; !ok {
		t.Error("the legacy file was destroyed rather than parked")
	}
}

// The sweep works from the STORE, so a legacy blob no row references is in scope.
// A database-driven scan could never see it, and "zero legacy-named blobs remain"
// would be unsatisfiable.
func TestCIDNormalize_UnreferencedLegacyBlobIsParkedAndLeavesNoNewOrphan(t *testing.T) {
	const orphan = "QmOrphanOrphanOrphanOrphanOrphanOr"
	content := []byte("referenced by nothing")
	digest := ComputeHash(content)

	repo := newCIDRepo() // no rows at all
	store := newCIDStore()
	store.put(orphan, content)

	sum := newCIDSweep(repo, store).RunCIDNormalize(context.Background(), cidOpts(nil))

	if sum.Orphans != 1 || sum.Normalized != 0 {
		t.Errorf("orphans=%d normalized=%d, want 1 and 0", sum.Orphans, sum.Normalized)
	}
	if _, ok := store.parked[orphan]; !ok {
		t.Error("the unreferenced legacy file was not parked")
	}
	// The link made before the UPDATE must be removed: an orphan under a DIGEST name
	// no longer matches the scan, so no future pass can ever see it — converting a
	// visible problem into a permanent invisible one.
	if store.has(digest) {
		t.Error("left a new orphan under a digest name, which no future sweep can find")
	}
}

// Uppercase-hex names are in scope (the scan wants LOWERCASE hex), and whether the
// legacy name and its digest are one file or two depends on the VOLUME, which no
// string comparison can tell you.
//
// The invariant is the same on both, and it is the one that matters: after the pass,
// every row resolves to content. The previous version of this test asserted the
// case-INSENSITIVE outcome against a map-backed fake — which is case-sensitive — so
// it passed while the code left every repointed row naming a blob that was never
// created.
func TestCIDNormalize_CaseFoldedNameResolvesOnBothVolumeKinds(t *testing.T) {
	for _, insensitive := range []bool{false, true} {
		name := "case-sensitive volume"
		if insensitive {
			name = "case-insensitive volume"
		}
		t.Run(name, func(t *testing.T) {
			content := []byte("case-folded")
			digest := ComputeHash(content)
			upper := upperHex(digest)

			repo := newCIDRepo(&cidRow{ExternalID: upper})
			store := newCIDStore()
			store.caseInsensitive = insensitive
			store.put(upper, content)

			sum := newCIDSweep(repo, store).RunCIDNormalize(context.Background(), cidOpts(nil))

			if sum.Records != 1 {
				t.Fatalf("records = %d, want 1 — the row still had to move", sum.Records)
			}
			// THE invariant: the row names something that exists.
			if got := repo.rows[0].ExternalID; !store.has(got) {
				t.Errorf("row names %q, which is NOT on the store — permanent 404 on live content", got)
			}
			if insensitive && len(store.parked) != 0 {
				t.Error("parked a file that IS the digest — that removes the content the row now names")
			}
			if !insensitive && len(store.parked) != 1 {
				t.Error("on a case-sensitive volume the old name is a distinct file and should be parked")
			}
		})
	}
}

// io.Copy returns (n, nil) on a clean EOF short of the promised length. Addressing
// that would name a fragment as if it were the file.
func TestCIDNormalize_ShortReadNeverNamesATruncatedFile(t *testing.T) {
	const legacy = "QmTruncatedTruncatedTruncatedTrunca"
	repo := newCIDRepo(&cidRow{ExternalID: legacy})
	store := newCIDStore()
	store.put(legacy, []byte("the complete payload, honest"))
	store.shortRead[legacy] = 5

	sum := newCIDSweep(repo, store).RunCIDNormalize(context.Background(), cidOpts(nil))

	if sum.Failed != 1 || sum.Normalized != 0 {
		t.Fatalf("failed=%d normalized=%d, want 1/0", sum.Failed, sum.Normalized)
	}
	if repo.rows[0].ExternalID != legacy || len(store.parked) != 0 {
		t.Error("acted on a truncated read")
	}
}

// Nothing is reclaimed before its mapping is on disk.
func TestCIDNormalize_MappingIsDurableBeforeTheFileMoves(t *testing.T) {
	const legacy = "QmJournalJournalJournalJournalJour"
	repo := newCIDRepo(&cidRow{ExternalID: legacy})
	store := newCIDStore()
	store.put(legacy, []byte("payload"))

	sink := &cidSink{}
	sink.onAppend = func([]byte) {
		if !store.has(legacy) {
			t.Error("the mapping was journalled only after the file moved — a crash between the two loses it")
		}
	}
	sum := newCIDSweep(repo, store).RunCIDNormalize(context.Background(), cidOpts(sink))

	if len(sink.journalLines) != 1 {
		t.Fatalf("journal has %d lines, want 1", len(sink.journalLines))
	}
	if sum.ReportFailed {
		t.Error("ReportFailed on a clean run")
	}
}

// A journal that cannot be written must stop the file moving, and must fail the run.
func TestCIDNormalize_UnrecordableMappingKeepsTheFileAndFailsTheRun(t *testing.T) {
	const legacy = "QmNoJournalNoJournalNoJournalNoJo"
	repo := newCIDRepo(&cidRow{ExternalID: legacy})
	store := newCIDStore()
	store.put(legacy, []byte("payload"))

	sink := &cidSink{journalErr: errors.New("volume read-only")}
	sum := newCIDSweep(repo, store).RunCIDNormalize(context.Background(), cidOpts(sink))

	if len(store.parked) != 0 {
		t.Error("moved a file whose mapping never reached disk")
	}
	if !sum.ReportFailed {
		t.Error("the run did not fail despite losing a mapping")
	}
}

// The preview is the approval gate; it must change nothing at all.
func TestCIDNormalize_DryRunChangesNothing(t *testing.T) {
	repo := newCIDRepo(&cidRow{ExternalID: "QmOneOneOneOneOneOneOneOneOneOneOn"})
	store := newCIDStore()
	store.put(repo.rows[0].ExternalID, []byte("one"))
	store.put(ComputeHash([]byte("already")), []byte("already")) // out of scope

	sink := &cidSink{}
	opts := cidOpts(sink)
	opts.DryRun = true
	sum := newCIDSweep(repo, store).RunCIDNormalize(context.Background(), opts)

	if sum.WouldNormalize != 1 {
		t.Errorf("wouldNormalize = %d, want 1", sum.WouldNormalize)
	}
	if repo.normalizeCalls != 0 || len(store.parked) != 0 || len(store.created) != 0 || sink.writes != 0 {
		t.Error("a dry run wrote something")
	}
}

// Re-running a converged corpus is a no-op, which is how an operator confirms done.
func TestCIDNormalize_ConvergedCorpusIsANoOp(t *testing.T) {
	content := []byte("already normalized")
	repo := newCIDRepo(&cidRow{ExternalID: ComputeHash(content)})
	store := newCIDStore()
	store.put(ComputeHash(content), content)

	sum := newCIDSweep(repo, store).RunCIDNormalize(context.Background(), cidOpts(nil))

	if sum.Normalized != 0 || sum.Orphans != 0 || len(store.parked) != 0 {
		t.Errorf("a converged corpus was not a no-op: %+v", sum)
	}
}

// A rate low enough to exceed the cap is a hang, not a throughput choice.
func TestNewPacer_CapsThePerObjectWait(t *testing.T) {
	for _, r := range []float64{1e-5, 1e-9, 1e-12, 0, -1} {
		lim := newPacer(r)
		lim.Reserve()
		if d := lim.Reserve().Delay(); d > maxPacerInterval {
			t.Errorf("rate %v → delay %v, want at most %v", r, d, maxPacerInterval)
		}
	}
}
