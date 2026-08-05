package service

import (
	"encoding/json"
	"fmt"
	"time"

	"go.uber.org/zap"
)

// cidReportSchemaVersion pins the shape defined by
// specs/018-legacy-cid-normalization/contracts/run-report.schema.json. Reports
// are retained indefinitely, so a reader may be much newer than the writer —
// bump this only on a breaking change.
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
}

type cidReportCounts struct {
	Normalized int `json:"normalized"`
	Skipped    int `json:"skipped"`
	Failed     int `json:"failed"`
	Reclaimed  int `json:"reclaimed"`
}

type cidReportChange struct {
	FileID             string `json:"fileId"`
	PreviousExternalID string `json:"previousExternalID"`
	NewExternalID      string `json:"newExternalID"`
	SharedWith         int    `json:"sharedWith"`
	// LegacyBlob is omitted from a journal line, which is written before
	// reclamation is attempted and therefore cannot know the answer.
	LegacyBlob string `json:"legacyBlob,omitempty"`
}

type cidReportSkipped struct {
	FileID     string `json:"fileId"`
	ExternalID string `json:"externalID"`
	Reason     string `json:"reason"`
	Detail     string `json:"detail,omitempty"`
}

// cidJournalLine encodes one mapping as a single NDJSON line, written and
// fsynced before the legacy blob it names is reclaimed.
func cidJournalLine(c CIDNormalizeChange) ([]byte, error) {
	data, err := json.Marshal(cidReportChange{
		FileID:             c.FileID.String(),
		PreviousExternalID: c.PreviousExternalID,
		NewExternalID:      c.NewExternalID,
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
			Skipped:    sum.Skipped,
			Failed:     sum.Failed,
			Reclaimed:  sum.Reclaimed,
		},
		// Never nil: the schema requires `changed` to be an array, and a JSON
		// null would fail a reader validating a retained report.
		Changed:       make([]cidReportChange, 0, len(sum.Changed)),
		NotNormalized: make([]cidReportSkipped, 0, len(sum.NotChanged)),
	}
	if sum.Aborted {
		rep.Outcome = cidOutcomeEndedEarly
	}
	for _, c := range sum.Changed {
		rep.Changed = append(rep.Changed, cidReportChange{
			FileID:             c.FileID.String(),
			PreviousExternalID: c.PreviousExternalID,
			NewExternalID:      c.NewExternalID,
			SharedWith:         c.SharedWith,
			LegacyBlob:         c.LegacyBlob,
		})
	}
	for _, n := range sum.NotChanged {
		rep.NotNormalized = append(rep.NotNormalized, cidReportSkipped{
			FileID:     n.FileID.String(),
			ExternalID: n.ExternalID,
			Reason:     n.Reason,
			Detail:     n.Detail,
		})
	}

	// A journal line that never reached the disk is already a lost mapping, even
	// if the report below writes cleanly.
	if run.journalFailed {
		sum.ReportFailed = true
	}

	data, err := json.MarshalIndent(rep, "", "  ")
	if err != nil {
		sum.ReportFailed = true
		s.Logger.Error("sweep-cids: could not encode the run report",
			zap.String("journal", run.journalPath),
			zap.String("recoverFrom", "the per-record journal, if it was written"),
			zap.Error(err))
		return
	}
	name := cidReportName(sum.StartedAt)
	path, err := run.opts.Report.WriteReport(name, data)
	if err != nil {
		sum.ReportFailed = true
		s.Logger.Error("sweep-cids: could not write the run report",
			zap.String("report", name),
			zap.String("journal", run.journalPath),
			zap.String("recoverFrom", "the per-record journal, if it was written"),
			zap.Error(err))
		return
	}
	// Deliberately the full path: an operator reading only the Job logs must be
	// able to find the file without knowing the storage layout (FR-019).
	s.Logger.Info("sweep-cids: run report written", zap.String("path", path))
}
