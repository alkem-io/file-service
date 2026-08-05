package service

import (
	"context"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"time"

	"github.com/google/uuid"
	"go.uber.org/zap"

	"github.com/alkem-io/file-service/internal/domain/model"
	"github.com/alkem-io/file-service/internal/domain/port"
)

// Reasons a record in scope was not normalized. The vocabulary is fixed by
// contracts/run-report.schema.json. The first three are SKIPS — nothing the
// sweep can do differently on a re-run, so they must never fail the run or the
// operator has no terminating condition. The last two are FAILURES, where a
// re-run can make progress. That split, not the raw count, drives the exit code.
const (
	// reasonContentAbsent: the record names a blob that is not on the store.
	// Owned by alkemio#1995, not fixable here — a skip, forever (FR-011, FR-017).
	reasonContentAbsent = "content_absent"
	// reasonConcurrentChange: a Replace or a temporary→permanent promote landed
	// between our read and our write, so the compare-and-set lost. The other
	// writer stores a proper digest anyway, so the record is already fixed.
	reasonConcurrentChange = "concurrent_change"
	// reasonUnaddressableName: the record's name is one the store's key rules
	// refuse, so its bytes can never be fetched through the storage port. Like
	// absent content this is unfixable by re-running — the name would have to be
	// repaired by hand first — so it is a skip. It is logged at WARN rather than
	// INFO because, unlike the alkemio#1995 cohort, it is not an expected
	// population: none of the known bucket-A names (base58 CIDs) can produce it.
	reasonUnaddressableName = "unaddressable_name"
	// reasonReadFailed: the store could not deliver the bytes, or delivered
	// fewer than it promised. Transient, so a re-run is warranted.
	reasonReadFailed = "read_failed"
	// reasonWriteFailed: publishing the bytes or repointing the record errored.
	// Includes a dedup collision with a row that already holds the target digest
	// in the same bucket — rare, and it needs a human, so it must be loud.
	reasonWriteFailed = "write_failed"
)

// cidNormalizePageSize bounds one keyset page, so a full-corpus pass never loads
// the work-list whole and never pins a connection across the whole run.
const cidNormalizePageSize = 500

// CIDNormalizeChange records one record whose name the sweep changed. Because
// the legacy blob is reclaimed in the same pass, this is the only surviving
// witness of the old name (FR-016, SC-009).
type CIDNormalizeChange struct {
	FileID             uuid.UUID
	PreviousExternalID string
	NewExternalID      string
	// SharedWith is how many OTHER records still referenced the legacy blob
	// after this one was repointed.
	SharedWith int
	// LegacyBlob is what became of the legacy blob once this record stopped
	// naming it. Explicit rather than inferred from SharedWith == 0, which
	// cannot tell a reclaimed blob from one a failed refcount query or a failed
	// delete left on disk — a distinction that exists nowhere else once the pass
	// exits, since the row no longer matches the scan predicate.
	LegacyBlob string
}

// What became of a legacy blob after a record was repointed away from it.
const (
	legacyBlobReclaimed  = "reclaimed"    // deleted; no record referenced it
	legacyBlobStillInUse = "still_in_use" // other records still name it
	legacyBlobRetained   = "retained"     // could not be reclaimed; still on disk
)

// CIDNormalizeNotChanged records one in-scope record the sweep left alone, with
// the reason from the vocabulary above.
type CIDNormalizeNotChanged struct {
	FileID     uuid.UUID
	ExternalID string
	Reason     string
	Detail     string
}

// CIDNormalizeSummary is what one `sweep-cids` pass did. It is both the operator
// summary (FR-010) and the body of the run report (FR-016).
type CIDNormalizeSummary struct {
	Normalized int // records repointed to a content-derived name
	Skipped    int // deliberately left alone; never drives a failure verdict (FR-017)
	Failed     int // genuinely could not be normalized — the only bucket that fails a run
	Reclaimed  int // legacy blobs deleted once their last reference went away
	// WouldNormalize is the preview figure: how many records a real pass would
	// normalize. Dry runs only.
	WouldNormalize int
	// WouldSkip is how many in-scope records a real pass would leave alone
	// because their content is already gone. Dry runs only.
	WouldSkip int
	// Aborted means the pass ENDED EARLY (page-scan error or shutdown signal)
	// rather than exhausting the corpus. Distinct from "completed with failures".
	Aborted bool
	// ReportFailed means the durable record of this pass could not be written.
	// It gates the exit code: a pass that destroyed legacy blobs and then lost
	// the mapping must not read as clean success.
	ReportFailed bool

	DryRun     bool
	Rate       float64
	StartedAt  time.Time
	FinishedAt time.Time

	Changed    []CIDNormalizeChange
	NotChanged []CIDNormalizeNotChanged
}

// CIDNormalizeOptions configures one pass.
type CIDNormalizeOptions struct {
	// DryRun enumerates through the same predicate a real pass uses and returns
	// before any store or database write. FR-007 requires "no change of any
	// kind", so a dry run also writes no report and no journal.
	DryRun bool
	// Rate bounds objects/second. Must be a positive, finite number — the caller
	// rejects anything else rather than reading it as "unlimited" (FR-015).
	Rate float64
	// Report receives the journal and the run report. Nil disables both (dry
	// runs, and tests that assert on the summary directly).
	Report port.ReportSink
}

// RunCIDNormalize re-addresses legacy-named objects under the digest of their
// bytes and repoints their records (018-legacy-cid-normalization).
//
// The cohort is objects written before content addressing: their name is an
// IPFS CID, so SHA3-256 of their bytes can never equal their name and
// file-backup-service refuses them — they are unbackable, hence data-at-risk
// (file-service#63 bucket A).
//
// Per object the order is PUBLISH → REPOINT → RECORD → RECLAIM, and that order
// is what makes the pass safe against live traffic without any coordination
// with the serving path: before the repoint the record names the legacy blob,
// which still exists; after it, the record names the new blob, which the publish
// already created. At no instant does a record name a blob that is absent
// (FR-004). An interruption between any two steps is safe — a re-run
// re-publishes (idempotent on a content-addressed store), re-attempts the
// compare-and-set, and re-checks the reference count.
//
// RECORD sits before RECLAIM deliberately: reclamation destroys the last copy of
// the old name, so the mapping is made durable first and a pass killed mid-way
// loses nothing it already destroyed.
//
// The pass is resumable WITHOUT any stored cursor: normalizing a record removes
// it from the predicate, so a re-run simply re-derives what is left (FR-008).
func (s *FileService) RunCIDNormalize(ctx context.Context, opts CIDNormalizeOptions) CIDNormalizeSummary {
	sum := CIDNormalizeSummary{DryRun: opts.DryRun, Rate: opts.Rate, StartedAt: time.Now().UTC()}
	run := &cidRun{opts: opts, journal: cidJournalName(sum.StartedAt)}
	pace := newPacer(opts.Rate)

	var cursor uuid.UUID // uuid.Nil → first page
	for {
		if ctx.Err() != nil { // shutdown: stop promptly rather than scanning on through a drain
			sum.Aborted = true
			break
		}
		page, err := s.Repo.ListLegacyNamed(ctx, cursor, cidNormalizePageSize)
		if err != nil {
			s.Logger.Error("sweep-cids: page scan failed; ending pass early", zap.Error(err))
			sum.Aborted = true
			break
		}
		if len(page) == 0 {
			break
		}
		for i := range page {
			if ctx.Err() != nil {
				sum.Aborted = true
				break
			}
			doc := page[i]
			// The cursor advances even for records we skip. A record whose blob is
			// gone stays in the predicate forever; not advancing past it would wedge
			// the pass on the first one — a lesson already paid for in sweep-dims.
			cursor = doc.ID

			if opts.DryRun {
				s.previewOneCID(doc, &sum)
				continue
			}
			// Pacing is per object and applies only to real passes: a dry run does
			// no per-object store I/O worth throttling, so pacing it would turn a
			// preview an operator runs before deciding into a long wait.
			if err := pace.wait(ctx); err != nil {
				sum.Aborted = true
				break
			}
			s.normalizeOneCID(ctx, doc, &sum, run)
		}
		if sum.Aborted {
			break
		}
	}

	sum.FinishedAt = time.Now().UTC()
	// A dry run writes nothing at all (FR-007); a real pass's report is the only
	// durable record of the old→new mapping, so write it even for an aborted pass.
	if !opts.DryRun && opts.Report != nil {
		s.writeCIDNormalizeReport(&sum, run)
	}
	s.logCIDNormalizeSummary(sum, run)
	return sum
}

// cidRun carries the per-run state the journal needs, kept off the summary so
// the summary stays a pure result.
type cidRun struct {
	opts    CIDNormalizeOptions
	journal string
	// journalPath is where the journal actually landed, for the closing log line.
	journalPath string
	// journalFailed records that at least one mapping could not be made durable
	// before its blob was reclaimed.
	journalFailed bool
}

// previewOneCID is the dry-run per-record step. It probes for the content
// because the preview is the approval gate for an irreversible migration, and a
// count that ignores the permanently-absent cohort over-reports the real pass by
// the size of that cohort. Exists() is a read: FR-007's "no change of any kind"
// is respected.
func (s *FileService) previewOneCID(doc model.Document, sum *CIDNormalizeSummary) {
	present, err := s.Storage.Exists(doc.ExternalID)
	if err != nil {
		// The preview cannot tell what a real pass would do with this record, and
		// saying nothing would quietly inflate the figure the operator approves.
		s.Logger.Warn("sweep-cids: dry run could not probe content; counting it as skipped",
			zap.String("documentID", doc.ID.String()),
			zap.String("externalID", doc.ExternalID),
			zap.Error(err))
		sum.WouldSkip++
		return
	}
	if !present {
		sum.WouldSkip++
		return
	}
	sum.WouldNormalize++
}

// normalizeOneCID runs publish → repoint → record → reclaim for a single record.
//
// A Go panic is contained PER RECORD. Unlike sweep-dims — which contains panics
// because it decodes arbitrary legacy bytes through a CGo image library — this
// pass decodes nothing, so a panic here signals a defect rather than a poison
// input, and is counted a FAILURE so the run exits non-zero. What the
// containment buys is that the pass keeps going: this one deletes blobs as it
// runs, and dying mid-corpus is how a partially-migrated corpus is produced.
func (s *FileService) normalizeOneCID(ctx context.Context, doc model.Document, sum *CIDNormalizeSummary, run *cidRun) {
	defer func() {
		if rec := recover(); rec != nil {
			s.Logger.Error("sweep-cids: PANIC normalizing a record; continuing with the rest of the pass",
				zap.String("documentID", doc.ID.String()),
				zap.String("externalID", doc.ExternalID),
				zap.Any("panic", rec))
			sum.Failed++
			sum.NotChanged = append(sum.NotChanged, CIDNormalizeNotChanged{
				FileID: doc.ID, ExternalID: doc.ExternalID, Reason: reasonWriteFailed,
				Detail: fmt.Sprintf("panic: %v", rec),
			})
		}
	}()

	newExternalID, ok := s.publishUnderDigest(doc, sum)
	if !ok {
		return
	}

	applied, err := s.Repo.NormalizeExternalID(ctx, doc.ID, doc.ExternalID, doc.Version, newExternalID)
	if err != nil {
		if errors.Is(err, model.ErrDuplicateKey) {
			// Another row in this bucket already holds the target digest — the same
			// asset uploaded before and after the addressing migration. The bytes
			// are safe (that row names them), but this row cannot take the name and
			// no re-run changes that, so it needs a person rather than a retry.
			s.failCID(doc, sum, reasonWriteFailed,
				"repoint: another record in this bucket already holds "+newExternalID+"; needs manual reconciliation")
			return
		}
		s.failCID(doc, sum, reasonWriteFailed, "repoint: "+err.Error())
		return
	}
	if !applied {
		// A concurrent Replace or promote moved externalID or version between our
		// read and our write. It wrote a proper digest, so the record is already
		// correct — retrying would fight a writer that fixed the problem for us.
		//
		// The blob we just published is deliberately NOT deleted: the store is
		// content-addressed, so that digest may be the legitimate home of bytes
		// other records reference. Deleting it would destroy live content.
		s.skipCID(doc, sum, reasonConcurrentChange, "compare-and-set lost to a concurrent writer")
		return
	}

	sum.Normalized++
	change := CIDNormalizeChange{
		FileID:             doc.ID,
		PreviousExternalID: doc.ExternalID,
		NewExternalID:      newExternalID,
	}
	// Durable BEFORE the bytes are destroyed. Everything after this point can be
	// reconstructed from the journal if the process dies.
	s.journalChange(change, run)
	change.SharedWith, change.LegacyBlob = s.reclaimLegacyBlob(ctx, doc, sum)
	sum.Changed = append(sum.Changed, change)
}

// publishUnderDigest streams the legacy blob into a staging write and commits
// it, returning the content-derived name. The StageWriter hashes internally, so
// the bytes are never held whole in memory and are never hashed twice.
//
// The legacy name is deliberately NOT decoded or verified against the bytes:
// these objects predate the current scheme with unknown write history, so the
// bytes on the store are the only truth available, and adopting their digest
// makes the new address correct by construction (FR-006, research R2).
//
// Publishing is idempotent — an existing blob under the same digest is
// authoritative and Commit reports a dedup hit rather than an error — which is
// what makes an interrupted pass safe to re-run.
func (s *FileService) publishUnderDigest(doc model.Document, sum *CIDNormalizeSummary) (string, bool) {
	rc, size, err := s.Storage.ReadStream(doc.ExternalID)
	if err != nil {
		switch {
		case errors.Is(err, os.ErrNotExist):
			// The 2026-06-30 NFS data-loss cohort (alkemio#1995). Nothing this
			// system can do restores it, so it is a skip — counting it as a
			// failure would leave the migration permanently red with no
			// terminating condition (FR-011, FR-017).
			s.skipCID(doc, sum, reasonContentAbsent, "no blob on the store under this name")
		case errors.Is(err, port.ErrInvalidKey):
			// The scan predicate is "not the current scheme", so it can select a
			// name the store itself refuses to address. Unfixable by re-running —
			// the name has to be repaired first — so it is a skip, not a failure
			// that would keep the Job red forever. Logged loudly: no known
			// bucket-A name can produce this.
			s.skipCID(doc, sum, reasonUnaddressableName,
				"the store's key rules refuse this name, so its bytes cannot be fetched: "+err.Error())
		default:
			s.failCID(doc, sum, reasonReadFailed, err.Error())
		}
		return "", false
	}
	defer func() { _ = rc.Close() }()

	stage, err := s.Storage.OpenStage(context.Background())
	if err != nil {
		s.failCID(doc, sum, reasonWriteFailed, "open stage: "+err.Error())
		return "", false
	}
	copied, err := io.Copy(stage, rc)
	if err != nil {
		_ = stage.Abort()
		s.failCID(doc, sum, reasonReadFailed, "copy to stage: "+err.Error())
		return "", false
	}
	// io.Copy returns (n, nil) on a SHORT read that ended in a clean EOF — a
	// blipping NFS volume, a truncated blob. Publishing that would address a
	// fragment as if it were the file, repoint the record to it, and then delete
	// the intact original: silent, irreversible data loss. The open handle's own
	// Stat is the authority on how many bytes it promised.
	if copied != size {
		_ = stage.Abort()
		s.failCID(doc, sum, reasonReadFailed,
			fmt.Sprintf("short read: got %d of %d bytes with no error; refusing to publish a truncated blob", copied, size))
		return "", false
	}
	stored, err := stage.Commit()
	if err != nil {
		_ = stage.Abort()
		s.failCID(doc, sum, reasonWriteFailed, "commit: "+err.Error())
		return "", false
	}
	return stored.ExternalID, true
}

// reclaimLegacyBlob deletes the legacy blob once no record references it, and
// reports how many records still do plus what became of the blob (FR-005).
//
// Several records may share one legacy blob, so the count is re-read live after
// each repoint rather than tallied up front — that is what makes reclamation
// correct across a resumed pass. It goes through cleanupOrphanedBlob, the
// service's single refcount-zero deletion path, so the pending backup hints for
// a hash are dropped with its bytes exactly as they are on every other delete.
//
// A reclamation failure is logged but NOT counted as a failure: the record is
// already correctly addressed and therefore no longer at risk; what is left
// behind is an unreferenced blob, which costs disk and nothing else. Failing the
// run for it would make a rerun the operator's only option, and a rerun cannot
// fix it — the record no longer matches the predicate.
func (s *FileService) reclaimLegacyBlob(ctx context.Context, doc model.Document, sum *CIDNormalizeSummary) (int, string) {
	remaining, err := s.Repo.CountByExternalID(ctx, doc.ExternalID)
	if err != nil {
		s.Logger.Warn("sweep-cids: reference count failed; leaving the legacy blob in place",
			zap.String("documentID", doc.ID.String()),
			zap.String("legacyExternalID", doc.ExternalID),
			zap.Error(err))
		return 0, legacyBlobRetained
	}
	if remaining > 0 {
		return remaining, legacyBlobStillInUse
	}
	if !s.cleanupOrphanedBlob(ctx, doc.ExternalID) {
		return 0, legacyBlobRetained
	}
	sum.Reclaimed++
	s.verifyNoLateReference(ctx, doc)
	return 0, legacyBlobReclaimed
}

// verifyNoLateReference closes the loop on the count→delete window. A copy that
// read its source before the repoint inserts a row carrying the legacy name
// AFTER the count and BEFORE the delete (CopyDocument copies externalID verbatim
// and does not re-upload), leaving a live row pointing at bytes just deleted.
//
// The count is therefore re-read afterwards. This does not close the window —
// the codebase's standing note is that the proper fix is atomic GC — but it
// turns silent, permanent 404s on real user content into a loud failure, and the
// bytes are still recoverable: they live under the new digest recorded in the
// journal on the line immediately above.
func (s *FileService) verifyNoLateReference(ctx context.Context, doc model.Document) {
	resurrected, err := s.Repo.CountByExternalID(ctx, doc.ExternalID)
	if err != nil || resurrected == 0 {
		return
	}
	s.Logger.Error("sweep-cids: a record began referencing the legacy name AFTER its blob was reclaimed — those rows now 404",
		zap.String("legacyExternalID", doc.ExternalID),
		zap.Int("rows", resurrected),
		zap.String("recoverFrom", "the bytes are intact under the new digest in this run's journal"))
}

// journalChange makes one old→new mapping durable before the bytes it describes
// are destroyed. Append-only NDJSON, fsynced per line: a pass killed at record
// 900 of 1053 loses no mapping for a blob it already reclaimed.
func (s *FileService) journalChange(c CIDNormalizeChange, run *cidRun) {
	if run.opts.Report == nil {
		return
	}
	line, err := cidJournalLine(c)
	if err != nil {
		run.journalFailed = true
		s.Logger.Error("sweep-cids: could not encode a journal line; this mapping is NOT durable",
			zap.String("documentID", c.FileID.String()), zap.Error(err))
		return
	}
	path, err := run.opts.Report.AppendJournal(run.journal, line)
	if err != nil {
		run.journalFailed = true
		s.Logger.Error("sweep-cids: could not append to the journal; this mapping is NOT durable and its blob is about to be reclaimed",
			zap.String("documentID", c.FileID.String()),
			zap.String("previousExternalID", c.PreviousExternalID),
			zap.String("newExternalID", c.NewExternalID),
			zap.Error(err))
		return
	}
	run.journalPath = path
}

func (s *FileService) skipCID(doc model.Document, sum *CIDNormalizeSummary, reason, detail string) {
	sum.Skipped++
	sum.NotChanged = append(sum.NotChanged, CIDNormalizeNotChanged{
		FileID: doc.ID, ExternalID: doc.ExternalID, Reason: reason, Detail: detail,
	})
	fields := []zap.Field{
		zap.String("documentID", doc.ID.String()),
		zap.String("externalID", doc.ExternalID),
		zap.String("reason", reason),
		zap.String("detail", detail),
	}
	if reason == reasonUnaddressableName {
		s.Logger.Warn("sweep-cids: record left unchanged — its name is unaddressable and needs manual repair", fields...)
		return
	}
	s.Logger.Info("sweep-cids: record left unchanged", fields...)
}

func (s *FileService) failCID(doc model.Document, sum *CIDNormalizeSummary, reason, detail string) {
	sum.Failed++
	sum.NotChanged = append(sum.NotChanged, CIDNormalizeNotChanged{
		FileID: doc.ID, ExternalID: doc.ExternalID, Reason: reason, Detail: detail,
	})
	s.Logger.Error("sweep-cids: record FAILED to normalize",
		zap.String("documentID", doc.ID.String()),
		zap.String("externalID", doc.ExternalID),
		zap.String("reason", reason),
		zap.String("detail", detail))
}

// logCIDNormalizeSummary emits the counts an operator acts on (FR-010). A pass
// that ended early must never be logged as "completed", or a partial sweep reads
// as a drained corpus.
func (s *FileService) logCIDNormalizeSummary(sum CIDNormalizeSummary, run *cidRun) {
	fields := []zap.Field{
		zap.Bool("dryRun", sum.DryRun),
		zap.Float64("rate", sum.Rate),
		zap.Int("normalized", sum.Normalized),
		zap.Int("skipped", sum.Skipped),
		zap.Int("failed", sum.Failed),
		zap.Int("reclaimed", sum.Reclaimed),
		zap.Duration("took", sum.FinishedAt.Sub(sum.StartedAt)),
	}
	if sum.DryRun {
		fields = append(fields,
			zap.Int("wouldNormalize", sum.WouldNormalize),
			zap.Int("wouldSkip", sum.WouldSkip))
	}
	if run.journalPath != "" {
		fields = append(fields, zap.String("journal", run.journalPath))
	}
	switch {
	case sum.Aborted:
		s.Logger.Warn("sweep-cids: ENDED EARLY (corpus not fully swept — rerun to finish)", fields...)
	case sum.DryRun:
		s.Logger.Info("sweep-cids: dry run complete (nothing was changed)", fields...)
	default:
		s.Logger.Info("sweep-cids: completed", fields...)
	}
}

// pacer bounds throughput to a fixed objects/second ceiling. Each object costs a
// blob read, a blob write and a database round-trip against SHARED PRODUCTION
// infrastructure, so an unbounded full-corpus pass would be a self-inflicted
// load test — the ceiling is what makes the sweep safe to run without a
// maintenance window (FR-014, FR-015).
//
// Deliberately not a token bucket: a bucket permits a burst after any idle
// stretch, which is precisely the shape of load this is meant to prevent.
type pacer struct {
	interval time.Duration
	next     time.Time
}

// maxPacerInterval caps the wait one object can impose. float64(time.Second)/rate
// exceeds int64 for a small enough rate, and converting an out-of-range float to
// a Duration yields an implementation-defined value rather than an error — so
// the bound is applied to the float, before the conversion.
const maxPacerInterval = time.Hour

func newPacer(rate float64) *pacer {
	// Written as !(rate > 0) rather than rate <= 0 so NaN — for which every
	// comparison is false — is caught here too, and +Inf is excluded explicitly
	// because it divides down to a zero interval. The caller rejects both as
	// well; this layer is what makes a malformed value impossible to read as
	// "unlimited" even if a future caller forgets.
	if !(rate > 0) || math.IsInf(rate, 1) {
		rate = 1
	}
	nanos := float64(time.Second) / rate
	if !(nanos < float64(maxPacerInterval)) { // also true for NaN
		return &pacer{interval: maxPacerInterval}
	}
	if interval := time.Duration(nanos); interval > 0 {
		return &pacer{interval: interval}
	}
	// A rate high enough to round the interval down to zero still gets a floor:
	// a zero interval is an unbounded pass, whatever produced it. An operator who
	// genuinely asks for a rate this high has effectively asked for no ceiling,
	// which is their call to make explicitly — it just cannot happen by accident.
	return &pacer{interval: time.Nanosecond}
}

// wait blocks until this object's slot, or until ctx is done.
func (p *pacer) wait(ctx context.Context) error {
	now := time.Now()
	if p.next.IsZero() { // first object goes immediately
		p.next = now.Add(p.interval)
		return nil
	}
	if d := time.Until(p.next); d > 0 {
		t := time.NewTimer(d)
		defer t.Stop()
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-t.C:
		}
		now = time.Now()
	}
	// Anchor the next slot on the clock, not on when this call returned, so slow
	// objects are not double-charged; re-anchor if we have fallen behind, so the
	// sweep never tries to "catch up" with a burst.
	p.next = p.next.Add(p.interval)
	if p.next.Before(now) {
		p.next = now.Add(p.interval)
	}
	return nil
}
