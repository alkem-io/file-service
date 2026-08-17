package service

import (
	"context"
	"errors"
	"io"
	"io/fs"
	"strings"
	"testing"
	"time"

	"github.com/alkem-io/file-service/internal/domain/port"
)

func testCID(ch string) string { return "Qm" + strings.Repeat(ch, 44) }

type sweepRepoFake struct {
	candidates []port.CIDCandidate
	aliases    map[string][]port.CIDAlias
	refs       map[string]int64
	updateErr  map[string]error
	events     []string
}

func (f *sweepRepoFake) ListCIDCandidates(context.Context) ([]port.CIDCandidate, error) {
	f.events = append(f.events, "list")
	result := make([]port.CIDCandidate, 0, len(f.candidates))
	for _, candidate := range f.candidates {
		if f.refs[candidate.ExternalID] > 0 {
			candidate.ReferenceCount = f.refs[candidate.ExternalID]
			result = append(result, candidate)
		}
	}
	return result, nil
}

func (f *sweepRepoFake) ListCIDCaseAliases(context.Context) ([]port.CIDAlias, error) {
	f.events = append(f.events, "aliases")
	var result []port.CIDAlias
	for _, aliases := range f.aliases {
		result = append(result, aliases...)
	}
	return result, nil
}

func (f *sweepRepoFake) UpdateCIDGroup(_ context.Context, target string, aliases []string) (int64, error) {
	f.events = append(f.events, "update:"+target)
	if err := f.updateErr[target]; err != nil {
		return 0, err
	}
	var changed int64
	for _, alias := range aliases {
		if alias == target {
			continue
		}
		changed += f.refs[alias]
		f.refs[alias] = 0
	}
	f.refs[target] += changed
	return changed, nil
}

func (f *sweepRepoFake) CountCIDAliasReferences(_ context.Context, alias string) (int64, error) {
	f.events = append(f.events, "count:"+alias)
	return f.refs[alias], nil
}

type sweepStorageFake struct {
	content      map[string][]byte
	readers      map[string]io.ReadCloser
	preparations map[string]port.CIDTargetPreparation
	prepareErr   map[string]error
	prepareHook  func(string)
	events       *[]string
	beforeFinish func(port.CIDStorageAlias) error
}

func (f *sweepStorageFake) OpenCIDSource(_ context.Context, externalID string) (io.ReadCloser, error) {
	*f.events = append(*f.events, "open:"+externalID)
	if reader, ok := f.readers[externalID]; ok {
		return reader, nil
	}
	content, ok := f.content[externalID]
	if !ok {
		return nil, fs.ErrNotExist
	}
	return io.NopCloser(strings.NewReader(string(content))), nil
}

func (f *sweepStorageFake) PrepareCIDTarget(_ context.Context, source, _ string) (port.CIDTargetPreparation, error) {
	*f.events = append(*f.events, "prepare:"+source)
	if f.prepareHook != nil {
		f.prepareHook(source)
	}
	if err := f.prepareErr[source]; err != nil {
		return port.CIDTargetPreparation{}, err
	}
	return f.preparations[source], nil
}

func (f *sweepStorageFake) FinalizeCIDAlias(_ context.Context, alias port.CIDStorageAlias, _ string) error {
	*f.events = append(*f.events, "finalize:"+alias.Name)
	if f.beforeFinish != nil {
		return f.beforeFinish(alias)
	}
	return nil
}

func TestCIDSweeper_MigratesManyReferencesAndRelatedCaseAlias(t *testing.T) {
	cid := testCID("a")
	content := []byte("shared immutable bytes")
	target := ComputeHash(content)
	upper := strings.ToUpper(target)
	repo := &sweepRepoFake{
		candidates: []port.CIDCandidate{{ExternalID: cid}},
		aliases: map[string][]port.CIDAlias{target: {
			{ExternalID: target, ReferenceCount: 1},
			{ExternalID: upper, ReferenceCount: 2},
		}},
		refs:      map[string]int64{cid: 4, target: 1, upper: 2},
		updateErr: map[string]error{},
	}
	storageEvents := []string{}
	storage := &sweepStorageFake{
		content: map[string][]byte{cid: content},
		preparations: map[string]port.CIDTargetPreparation{cid: {
			Created: false,
			ObsoleteAliases: []port.CIDStorageAlias{
				{Name: cid},
				{Name: upper},
			},
		}},
		prepareErr: map[string]error{},
		events:     &storageEvents,
	}

	result, err := (&CIDSweeper{Repo: repo, Storage: storage}).Run(context.Background())
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !result.Complete() {
		t.Fatalf("result not complete: %+v", result)
	}
	if result.CIDReferencesFound != 4 || result.CaseVariantReferencesFound != 2 || result.ReferencesUpdated != 6 {
		t.Fatalf("unexpected reference counters: %+v", result)
	}
	if result.DistinctCIDSources != 1 || result.ConsolidatedSourceBlobs != 1 || result.MigratedSourceBlobs != 0 {
		t.Fatalf("unexpected source counters: %+v", result)
	}
	if repo.refs[cid] != 0 || repo.refs[upper] != 0 || repo.refs[target] != 7 {
		t.Fatalf("references did not converge: %#v", repo.refs)
	}
}

func TestCIDSweeper_CaseInsensitiveFinalizationHappensAfterZeroRefcount(t *testing.T) {
	cid := testCID("b")
	content := []byte("case folded content")
	target := ComputeHash(content)
	upper := strings.ToUpper(target)
	repo := &sweepRepoFake{
		candidates: []port.CIDCandidate{{ExternalID: cid}},
		aliases:    map[string][]port.CIDAlias{target: {{ExternalID: upper, ReferenceCount: 1}}},
		refs:       map[string]int64{cid: 1, upper: 1},
		updateErr:  map[string]error{},
	}
	events := []string{}
	storage := &sweepStorageFake{
		content: map[string][]byte{cid: content},
		preparations: map[string]port.CIDTargetPreparation{cid: {
			ObsoleteAliases: []port.CIDStorageAlias{{Name: upper, CanonicalizeCase: true}},
		}},
		prepareErr: map[string]error{},
		events:     &events,
		beforeFinish: func(alias port.CIDStorageAlias) error {
			if repo.refs[alias.Name] != 0 {
				return errors.New("finalized referenced alias")
			}
			return nil
		},
	}

	result, err := (&CIDSweeper{Repo: repo, Storage: storage}).Run(context.Background())
	if err != nil || !result.Complete() {
		t.Fatalf("Run = (%+v, %v)", result, err)
	}
	wantOrder := []string{"open:" + cid, "prepare:" + cid, "finalize:" + upper}
	if strings.Join(events, "|") != strings.Join(wantOrder, "|") {
		t.Fatalf("storage order = %v, want %v", events, wantOrder)
	}
}

func TestCIDSweeper_ContinuesAndExplicitRerunConverges(t *testing.T) {
	cidA, cidB := testCID("c"), testCID("d")
	contentA, contentB := []byte("content A"), []byte("content B")
	targetA, targetB := ComputeHash(contentA), ComputeHash(contentB)
	repo := &sweepRepoFake{
		candidates: []port.CIDCandidate{{ExternalID: cidA}, {ExternalID: cidB}},
		aliases:    map[string][]port.CIDAlias{},
		refs:       map[string]int64{cidA: 1, cidB: 1},
		updateErr:  map[string]error{},
	}
	events := []string{}
	storage := &sweepStorageFake{
		content: map[string][]byte{cidA: contentA, cidB: contentB},
		preparations: map[string]port.CIDTargetPreparation{
			cidA: {Created: true, ObsoleteAliases: []port.CIDStorageAlias{{Name: cidA}}},
			cidB: {Created: true, ObsoleteAliases: []port.CIDStorageAlias{{Name: cidB}}},
		},
		prepareErr: map[string]error{cidB: errors.New("forced prepare failure")},
		events:     &events,
	}
	sweeper := &CIDSweeper{Repo: repo, Storage: storage}

	first, err := sweeper.Run(context.Background())
	if err != nil {
		t.Fatalf("first Run: %v", err)
	}
	if first.FailedSourceBlobs != 1 || first.MigratedSourceBlobs != 1 || repo.refs[cidA] != 0 || repo.refs[cidB] != 1 {
		t.Fatalf("unexpected first result: %+v refs=%v", first, repo.refs)
	}
	delete(storage.prepareErr, cidB)
	second, err := sweeper.Run(context.Background())
	if err != nil || !second.Complete() {
		t.Fatalf("second Run = (%+v, %v)", second, err)
	}
	if second.DistinctCIDSources != 1 || repo.refs[cidB] != 0 || repo.refs[targetB] != 1 {
		t.Fatalf("rerun did not converge only remaining group: %+v refs=%v", second, repo.refs)
	}
	if repo.refs[targetA] != 1 {
		t.Fatalf("completed target changed on rerun: refs=%v", repo.refs)
	}
}

func TestCIDSweeper_MissingSourceIsOutsideEligiblePresentSet(t *testing.T) {
	cid := testCID("e")
	repo := &sweepRepoFake{
		candidates: []port.CIDCandidate{{ExternalID: cid}},
		aliases:    map[string][]port.CIDAlias{},
		refs:       map[string]int64{cid: 3},
		updateErr:  map[string]error{},
	}
	events := []string{}
	storage := &sweepStorageFake{content: map[string][]byte{}, preparations: map[string]port.CIDTargetPreparation{}, prepareErr: map[string]error{}, events: &events}

	result, err := (&CIDSweeper{Repo: repo, Storage: storage}).Run(context.Background())
	if err != nil || !result.Complete() {
		t.Fatalf("Run = (%+v, %v)", result, err)
	}
	if result.DistinctCIDSources != 0 || result.CIDReferencesFound != 0 || result.ReferencesUpdated != 0 {
		t.Fatalf("missing source counted as eligible: %+v", result)
	}
}

func TestCIDSweeper_IdenticalContentCIDsConvergeSequentially(t *testing.T) {
	cidA, cidB := testCID("h"), testCID("i")
	content := []byte("same bytes under two legacy names")
	target := ComputeHash(content)
	repo := &sweepRepoFake{
		candidates: []port.CIDCandidate{{ExternalID: cidA}, {ExternalID: cidB}},
		aliases:    map[string][]port.CIDAlias{},
		refs:       map[string]int64{cidA: 2, cidB: 3},
		updateErr:  map[string]error{},
	}
	events := []string{}
	storage := &sweepStorageFake{
		content: map[string][]byte{cidA: content, cidB: content},
		preparations: map[string]port.CIDTargetPreparation{
			cidA: {Created: true, ObsoleteAliases: []port.CIDStorageAlias{{Name: cidA}}},
			cidB: {Created: false, ObsoleteAliases: []port.CIDStorageAlias{{Name: cidB}}},
		},
		prepareErr: map[string]error{},
		events:     &events,
	}

	result, err := (&CIDSweeper{Repo: repo, Storage: storage}).Run(context.Background())
	if err != nil || !result.Complete() {
		t.Fatalf("Run = (%+v, %v)", result, err)
	}
	if result.MigratedSourceBlobs != 1 || result.ConsolidatedSourceBlobs != 1 || result.ReferencesUpdated != 5 {
		t.Fatalf("unexpected result: %+v", result)
	}
	if repo.refs[target] != 5 || repo.refs[cidA] != 0 || repo.refs[cidB] != 0 {
		t.Fatalf("references did not converge: %v", repo.refs)
	}
}

func TestCIDSweeper_CountsSharedCaseAliasesOnceWhenFirstCIDFails(t *testing.T) {
	cidA, cidB := testCID("m"), testCID("n")
	content := []byte("same bytes with one shared case alias")
	target := ComputeHash(content)
	upper := strings.ToUpper(target)
	repo := &sweepRepoFake{
		candidates: []port.CIDCandidate{{ExternalID: cidA}, {ExternalID: cidB}},
		aliases: map[string][]port.CIDAlias{target: {
			{ExternalID: upper, ReferenceCount: 3},
		}},
		refs:      map[string]int64{cidA: 1, cidB: 1, upper: 3},
		updateErr: map[string]error{},
	}
	events := []string{}
	storage := &sweepStorageFake{
		content: map[string][]byte{cidA: content, cidB: content},
		preparations: map[string]port.CIDTargetPreparation{
			cidB: {Created: false, ObsoleteAliases: []port.CIDStorageAlias{{Name: cidB}, {Name: upper}}},
		},
		prepareErr: map[string]error{cidA: errors.New("forced first-source failure")},
		events:     &events,
	}

	result, err := (&CIDSweeper{Repo: repo, Storage: storage}).Run(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if result.CaseVariantReferencesFound != 3 {
		t.Fatalf("case aliases counted more than once: %+v", result)
	}
	if result.FailedSourceBlobs != 1 || result.ConsolidatedSourceBlobs != 1 || result.ReferencesUpdated != 4 {
		t.Fatalf("unexpected result: %+v", result)
	}
}

func TestCIDSweeper_DatabaseGroupFailureDoesNotStopLaterCID(t *testing.T) {
	cidA, cidB := testCID("j"), testCID("k")
	contentA, contentB := []byte("first"), []byte("second")
	targetA, targetB := ComputeHash(contentA), ComputeHash(contentB)
	repo := &sweepRepoFake{
		candidates: []port.CIDCandidate{{ExternalID: cidA}, {ExternalID: cidB}},
		aliases:    map[string][]port.CIDAlias{},
		refs:       map[string]int64{cidA: 1, cidB: 1},
		updateErr:  map[string]error{targetA: errors.New("forced database failure")},
	}
	events := []string{}
	storage := &sweepStorageFake{
		content: map[string][]byte{cidA: contentA, cidB: contentB},
		preparations: map[string]port.CIDTargetPreparation{
			cidA: {Created: true, ObsoleteAliases: []port.CIDStorageAlias{{Name: cidA}}},
			cidB: {Created: true, ObsoleteAliases: []port.CIDStorageAlias{{Name: cidB}}},
		},
		prepareErr: map[string]error{},
		events:     &events,
	}

	result, err := (&CIDSweeper{Repo: repo, Storage: storage}).Run(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if result.FailedSourceBlobs != 1 || result.MigratedSourceBlobs != 1 {
		t.Fatalf("unexpected result: %+v", result)
	}
	if repo.refs[cidA] != 1 || repo.refs[cidB] != 0 || repo.refs[targetB] != 1 {
		t.Fatalf("unexpected references: %v", repo.refs)
	}
}

func TestCIDSweeper_CancellationClassifiesOpenedSourceAsFailed(t *testing.T) {
	cid := testCID("p")
	content := []byte("cancel after opening source")
	repo := &sweepRepoFake{
		candidates: []port.CIDCandidate{{ExternalID: cid}},
		aliases:    map[string][]port.CIDAlias{},
		refs:       map[string]int64{cid: 2},
		updateErr:  map[string]error{},
	}
	events := []string{}
	ctx, cancel := context.WithCancel(context.Background())
	storage := &sweepStorageFake{
		content:      map[string][]byte{cid: content},
		preparations: map[string]port.CIDTargetPreparation{},
		prepareErr:   map[string]error{cid: context.Canceled},
		prepareHook:  func(string) { cancel() },
		events:       &events,
	}

	result, err := (&CIDSweeper{Repo: repo, Storage: storage}).Run(ctx)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Run error = %v, want context.Canceled", err)
	}
	if !result.Aborted || result.DistinctCIDSources != 1 || result.FailedSourceBlobs != 1 {
		t.Fatalf("interrupted source not classified: %+v", result)
	}
	if result.DistinctCIDSources != result.MigratedSourceBlobs+result.ConsolidatedSourceBlobs+result.FailedSourceBlobs {
		t.Fatalf("terminal accounting equation failed: %+v", result)
	}
	if len(result.Failures) != 1 || result.Failures[0].CID != cid {
		t.Fatalf("missing interrupted-source failure: %+v", result.Failures)
	}
}

func TestCIDSweeper_CancellationInterruptsSourceHashAndReturnsSummary(t *testing.T) {
	cid := testCID("r")
	repo := &sweepRepoFake{
		candidates: []port.CIDCandidate{{ExternalID: cid}},
		aliases:    map[string][]port.CIDAlias{},
		refs:       map[string]int64{cid: 1},
		updateErr:  map[string]error{},
	}
	events := []string{}
	reader := newCancellationBlockingReader()
	storage := &sweepStorageFake{
		content:      map[string][]byte{},
		readers:      map[string]io.ReadCloser{cid: reader},
		preparations: map[string]port.CIDTargetPreparation{},
		prepareErr:   map[string]error{},
		events:       &events,
	}
	ctx, cancel := context.WithCancel(context.Background())
	type runResult struct {
		result port.CIDSweepResult
		err    error
	}
	done := make(chan runResult, 1)
	go func() {
		result, err := (&CIDSweeper{Repo: repo, Storage: storage}).Run(ctx)
		done <- runResult{result: result, err: err}
	}()

	<-reader.started
	cancel()
	select {
	case run := <-done:
		if !errors.Is(run.err, context.Canceled) {
			t.Fatalf("Run error = %v, want context.Canceled", run.err)
		}
		if !run.result.Aborted || run.result.DistinctCIDSources != 1 || run.result.FailedSourceBlobs != 1 {
			t.Fatalf("interrupted hash result = %+v", run.result)
		}
		if run.result.DistinctCIDSources != run.result.MigratedSourceBlobs+run.result.ConsolidatedSourceBlobs+run.result.FailedSourceBlobs {
			t.Fatalf("terminal accounting equation failed: %+v", run.result)
		}
	case <-time.After(time.Second):
		t.Fatal("Run did not return after source-hash cancellation")
	}
}
