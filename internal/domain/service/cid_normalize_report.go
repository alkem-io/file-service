package service

import (
	"encoding/json"
	"fmt"
	"time"

	"go.uber.org/zap"
)

// cidReportSchemaVersion pins the report shape. The schema itself is a
// cross-repo artifact and lives in the workspace spec (018-legacy-cid-normalization,
// contracts/run-report.schema.json in alkem-io/agents-hq), not in this repo — so
// this is a version, not a path to follow. Reports are retained indefinitely and a
// reader may be much newer than the writer; bump only on a breaking change.
const cidReportSchemaVersion = 1

// Report outcomes. "endedEarly" is not a synonym for failure: it means the pass
// stopped before exhausting the corpus and is resumable.
const (
	cidOutcomeCompleted  = "completed"
	cidOutcomeEndedEarly = "endedEarly"
)

// cidReportTimeLayout names reports by UTC start instant. Nanosecond precision
// is what keeps two passes started in the same second from colliding — the sink
// creates reports exclusively and would otherwise fail the second one.
const cidReportTimeLayout = "20060102T150405.000000000Z"

func cidReportName(startedAt time.Time) string {
	return fmt.Sprintf("sweep-cids-%s.json", startedAt.UTC().Format(cidReportTimeLayout))
}

// The journal shares the report's stem so the two halves of one pass sort
// together, and carries .ndjson because it is written one fsynced line at a time.
func cidJournalName(startedAt time.Time) string {
	return fmt.Sprintf("sweep-cids-%s.ndjson", startedAt.UTC().Format(cidReportTimeLayout))
}

type cidRunReport struct {
	SchemaVersion int                `json:"schemaVersion"`
	StartedAt     time.Time          `json:"startedAt"`
	FinishedAt    time.Time          `json:"finishedAt"`
	Rate          float64            `json:"rate"`
	Outcome       string             `json:"outcome"`
	Counts        cidReportCounts    `json:"counts"`
	Changed       []cidReportChange  `json:"changed"`
	NotNormalized []cidReportSkipped `json:"notNormalized"`
	// MappingIncomplete says at least one old→new mapping could not be made
	// durable. Without it a pass that lost mappings still reads
	// `"outcome": "completed", "failed": 0` — the opposite of its exit code.
	MappingIncomplete bool `json:"mappingIncomplete,omitempty"`
}

type cidReportCounts struct {
	Normalized int   `json:"normalized"`
	Records    int64 `json:"records"`
	Skipped    int   `json:"skipped"`
	Failed     int   `json:"failed"`
	Parked     int   `json:"parked"`
	Orphans    int   `json:"orphans"`
}

type cidReportChange struct {
	PreviousExternalID string `json:"previousExternalID"`
	NewExternalID      string `json:"newExternalID"`
	// Records is how many rows the single UPDATE moved.
	Records int64 `json:"records"`
	// Parked reports whether the legacy file was moved aside. False means it is
	// still at its original name — the equal-fold case, a late reference, or a
	// failed move.
	Parked bool `json:"parked"`
}

// cidJournalEntry is deliberately NARROWER than cidReportChange: a journal line is
// written before the file is parked, so the outcome is not yet known and emitting
// the report struct would stamp a confident `"parked": false` on every line.
type cidJournalEntry struct {
	PreviousExternalID string `json:"previousExternalID"`
	NewExternalID      string `json:"newExternalID"`
	Records            int64  `json:"records"`
}

type cidReportSkipped struct {
	ExternalID string `json:"externalID"`
	Reason     string `json:"reason"`
	Detail     string `json:"detail,omitempty"`
}

// cidJournalLine encodes one mapping as a single NDJSON line, written and fsynced
// before the legacy file it names is parked.
func cidJournalLine(c CIDNormalizeChange) ([]byte, error) {
	data, err := json.Marshal(cidJournalEntry{
		PreviousExternalID: c.PreviousExternalID,
		NewExternalID:      c.NewExternalID,
		Records:            c.Records,
	})
	if err != nil {
		return nil, err
	}
	return append(data, '\n'), nil
}

// writeCIDNormalizeReport persists the durable record of what the pass did
// (FR-016, FR-018) and logs where it landed (FR-019).
//
// This is load-bearing rather than nice-to-have: the pass reclaims each legacy
// blob in the same run that renames it, so once the process exits this file is
// the only thing that still knows the old name. A failure here therefore sets
// ReportFailed, which gates the exit code — a pass that destroyed blobs and then
// lost the mapping must not read as clean success. The per-record journal is the
// backstop: it is already on disk, line by line, before each reclamation.
func (s *FileService) writeCIDNormalizeReport(sum *CIDNormalizeSummary, run *cidRun) {
	rep := cidRunReport{
		SchemaVersion: cidReportSchemaVersion,
		StartedAt:     sum.StartedAt,
		FinishedAt:    sum.FinishedAt,
		Rate:          sum.Rate,
		Outcome:       cidOutcomeCompleted,
		Counts: cidReportCounts{
			Normalized: sum.Normalized,
			Records:    sum.Records,
			Skipped:    sum.Skipped,
			Failed:     sum.Failed,
			Parked:     sum.Parked,
			Orphans:    sum.Orphans,
		},
		// Never nil: the schema requires arrays, and a JSON null would fail a reader
		// validating a retained report.
		Changed:           make([]cidReportChange, 0, len(sum.Changed)),
		NotNormalized:     make([]cidReportSkipped, 0, len(sum.NotChanged)),
		MappingIncomplete: run.journalFailed,
	}
	if sum.Aborted {
		rep.Outcome = cidOutcomeEndedEarly
	}
	for _, c := range sum.Changed {
		rep.Changed = append(rep.Changed, cidReportChange(c))
	}
	for _, n := range sum.NotChanged {
		rep.NotNormalized = append(rep.NotNormalized, cidReportSkipped(n))
	}

	// A journal line that never reached disk is already a lost mapping, even if the
	// report below writes cleanly.
	if run.journalFailed {
		sum.ReportFailed = true
	}

	data, err := json.MarshalIndent(rep, "", "  ")
	if err != nil {
		sum.ReportFailed = true
		s.Logger.Error("sweep-cids: could not encode the run report", zap.Error(err))
		return
	}
	path, err := run.opts.Report.WriteReport(cidReportName(sum.StartedAt), data)
	if err != nil {
		sum.ReportFailed = true
		s.Logger.Error("sweep-cids: could not write the run report",
			zap.String("recoverFrom", "the per-blob journal, if it was written"), zap.Error(err))
		return
	}
	run.reportPath = path
}

// logCIDNormalizeReportPath emits the run's final line. An operator reading only
// the Job logs must be able to find the report without knowing the storage layout
// (FR-019), so nothing is logged after it.
func (s *FileService) logCIDNormalizeReportPath(sum CIDNormalizeSummary, run *cidRun) {
	if sum.DryRun || run.reportPath == "" {
		return
	}
	s.Logger.Info("sweep-cids: run report written", zap.String("path", run.reportPath))
}
