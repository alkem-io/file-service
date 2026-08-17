package main

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/alkem-io/file-service/internal/domain/port"
)

func TestRunWith_ExitCodesAndTerminalSummary(t *testing.T) {
	tests := []struct {
		name       string
		args       []string
		result     port.CIDSweepResult
		runErr     error
		wantCode   int
		wantCalled bool
	}{
		{name: "complete", result: port.CIDSweepResult{DistinctCIDSources: 1, MigratedSourceBlobs: 1}, wantCode: 0, wantCalled: true},
		{name: "empty", result: port.CIDSweepResult{}, wantCode: 0, wantCalled: true},
		{name: "partial failure", result: port.CIDSweepResult{DistinctCIDSources: 1, FailedSourceBlobs: 1, Failures: []port.CIDSweepFailure{{CID: "Qmfailed", Reason: "forced"}}}, wantCode: 1, wantCalled: true},
		{name: "aborted", result: port.CIDSweepResult{Aborted: true}, runErr: errors.New("scan failed"), wantCode: 1, wantCalled: true},
		{name: "unexpected argument", args: []string{"extra"}, wantCode: 2, wantCalled: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			called := false
			code := runWith(t.Context(), tt.args, &stdout, &stderr, func(context.Context) (port.CIDSweepResult, error) {
				called = true
				return tt.result, tt.runErr
			})
			if code != tt.wantCode || called != tt.wantCalled {
				t.Fatalf("runWith code/called = %d/%v, want %d/%v", code, called, tt.wantCode, tt.wantCalled)
			}
			if tt.wantCalled && strings.Count(stdout.String(), `"event":"cid_sweep_summary"`) != 1 {
				t.Fatalf("stdout must contain one summary: %q", stdout.String())
			}
			if tt.result.FailedSourceBlobs == 1 && !strings.Contains(stdout.String(), `"event":"cid_sweep_failure"`) {
				t.Fatalf("stdout lacks failure event: %q", stdout.String())
			}
			if tt.runErr != nil && !strings.Contains(stdout.String(), `"event":"cid_sweep_abort"`) {
				t.Fatalf("stdout lacks abort event: %q", stdout.String())
			}
			if !tt.wantCalled && stderr.Len() == 0 {
				t.Fatal("invalid invocation must explain the error")
			}
		})
	}
}
