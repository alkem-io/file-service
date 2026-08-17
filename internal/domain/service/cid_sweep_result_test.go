package service

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/alkem-io/file-service/internal/domain/port"
)

func TestCIDSweeper_ResultEquationAndFailureReason(t *testing.T) {
	cidA, cidB := testCID("f"), testCID("g")
	contentA, contentB := []byte("good"), []byte("bad")
	repo := &sweepRepoFake{
		candidates: []port.CIDCandidate{{ExternalID: cidA}, {ExternalID: cidB}},
		aliases:    map[string][]port.CIDAlias{},
		refs:       map[string]int64{cidA: 2, cidB: 3},
		updateErr:  map[string]error{},
	}
	events := []string{}
	storage := &sweepStorageFake{
		content: map[string][]byte{cidA: contentA, cidB: contentB},
		preparations: map[string]port.CIDTargetPreparation{
			cidA: {Created: true, ObsoleteAliases: []port.CIDStorageAlias{{Name: cidA}}},
		},
		prepareErr: map[string]error{cidB: errors.New("readable target unavailable")},
		events:     &events,
	}

	result, err := (&CIDSweeper{Repo: repo, Storage: storage}).Run(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if result.DistinctCIDSources != result.MigratedSourceBlobs+result.ConsolidatedSourceBlobs+result.FailedSourceBlobs {
		t.Fatalf("result equation failed: %+v", result)
	}
	if result.CIDReferencesFound != 5 || result.ReferencesUpdated != 2 || result.FailedSourceBlobs != 1 {
		t.Fatalf("unexpected counters: %+v", result)
	}
	if len(result.Failures) != 1 || result.Failures[0].CID != cidB || !strings.Contains(result.Failures[0].Reason, "readable target unavailable") {
		t.Fatalf("unexpected failure detail: %+v", result.Failures)
	}
}
