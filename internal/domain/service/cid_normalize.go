package service

import (
	"context"
	"errors"
	"io"
	"os"
	"time"

	"github.com/google/uuid"
	"go.uber.org/zap"

	"github.com/alkem-io/file-service/internal/domain/model"
	"github.com/alkem-io/file-service/internal/domain/port"
)

// Reasons a record in scope was not normalized. The vocabulary is fixed by
// contracts/run-report.schema.json — the first two are SKIPS (nothing the sweep
// can do differently on a re-run), the last two are FAILURES (a re-run may well
// succeed). That split, not the raw count, is what the exit code is derived from.
const (
	// reasonContentAbsent: the record names a blob that is not on the store.
	// Owned by alkemio#1995, not fixable here — a skip, forever (FR-011, FR-017).
	reasonContentAbsent = "content_absent"
	// reasonConcurrentChange: a Replace or a temporary→permanent promote landed
	// between our read and our write, so the compare-and-set lost. The other
	// writer stores a proper digest anyway, so the record is already fixed.
	reasonConcurrentChange = "concurrent_change"
	// reasonReadFailed: the store could not deliver the bytes (an I/O fault, or
	// a name the store's key rules refuse). Transient faults clear on a re-run;
	// an unaddressable name is a genuine anomaly an operator should see. Both
	// warrant a non-zero exit, unlike an absent blob.
	reasonReadFailed = "read_failed"
	// reasonWriteFailed: publishing the bytes or repointing the record errored.
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
	// after this one was repointed — i.e. why reclamation was deferred.
	SharedWith int
}

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
	// Aborted means the pass ENDED EARLY (page-scan error or shutdown signal)
	// rather than exhausting the corpus. Distinct from "completed with failures".
	Aborted bool

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
	// kind", so a dry run also writes no report.
	DryRun bool
	// Rate bounds objects/second. Must be > 0 — the caller rejects a
	// non-positive value rather than reading it as "unlimited" (FR-015).
	Rate float64
	// Report receives the run report. Nil disables it (dry runs, and tests that
	// assert on the summary directly).
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
// Per object the order is PUBLISH → REPOINT → RECLAIM, and that order is what
// makes the pass safe against live traffic without any coordination with the
// serving path: before the repoint the record names the legacy blob, which
// still exists; after it, the record names the new blob, which the publish
// already created. At no instant does a record name a blob that is absent
// (FR-004). An interruption between any two steps is safe — a re-run
// re-publishes (idempotent on a content-addressed store), re-attempts the
// compare-and-set, and re-checks the reference count.
//
// The pass is resumable WITHOUT any stored cursor: normalizing a record removes
// it from the predicate, so a re-run simply re-derives what is left (FR-008).
func (s *FileService) RunCIDNormalize(ctx context.Context, opts CIDNormalizeOptions) CIDNormalizeSummary {
	sum := CIDNormalizeSummary{DryRun: opts.DryRun, Rate: opts.Rate, StartedAt: time.Now().UTC()}
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
				sum.WouldNormalize++
				continue
			}
			// Pacing is per object and applies only to real passes: a dry run does
			// no per-object I/O, so throttling it would turn a preview an operator
			// runs before deciding into a multi-minute wait for no reason.
			if err := pace.wait(ctx); err != nil {
				sum.Aborted = true
				break
			}
			s.normalizeOneCID(ctx, doc, &sum)
		}
		if sum.Aborted {
			break
		}
	}

	sum.FinishedAt = time.Now().UTC()
	s.logCIDNormalizeSummary(sum)
	// A dry run writes nothing at all (FR-007); a real pass's report is the only
	// durable record of the old→new mapping, so write it even for an aborted pass.
	if !opts.DryRun && opts.Report != nil {
		s.writeCIDNormalizeReport(sum, opts.Report)
	}
	return sum
}

// normalizeOneCID runs publish → repoint → reclaim for a single record.
func (s *FileService) normalizeOneCID(ctx context.Context, doc model.Document, sum *CIDNormalizeSummary) {
	newExternalID, ok := s.publishUnderDigest(doc, sum)
	if !ok {
		return
	}

	applied, err := s.Repo.NormalizeExternalID(ctx, doc.ID, doc.ExternalID, doc.Version, newExternalID)
	if err != nil {
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
	remaining := s.reclaimLegacyBlob(ctx, doc, sum)
	sum.Changed = append(sum.Changed, CIDNormalizeChange{
		FileID:             doc.ID,
		PreviousExternalID: doc.ExternalID,
		NewExternalID:      newExternalID,
		SharedWith:         remaining,
	})
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
	rc, _, err := s.Storage.ReadStream(doc.ExternalID)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			// The 2026-06-30 NFS data-loss cohort (alkemio#1995). Nothing this
			// system can do restores it, so it is a skip — counting it as a
			// failure would leave the migration permanently red with no
			// terminating condition (FR-011, FR-017).
			s.skipCID(doc, sum, reasonContentAbsent, "no blob on the store under this name")
			return "", false
		}
		s.failCID(doc, sum, reasonReadFailed, err.Error())
		return "", false
	}
	defer func() { _ = rc.Close() }()

	stage, err := s.Storage.OpenStage(context.Background())
	if err != nil {
		s.failCID(doc, sum, reasonWriteFailed, "open stage: "+err.Error())
		return "", false
	}
	if _, err := io.Copy(stage, rc); err != nil {
		_ = stage.Abort()
		s.failCID(doc, sum, reasonReadFailed, "copy to stage: "+err.Error())
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
// returns how many records still do (FR-005).
//
// Several records may share one legacy blob, so the count is re-read live after
// each repoint rather than tallied up front — that is what makes reclamation
// correct across a resumed pass. A reclamation failure is logged but NOT counted
// as a failure: the record is already correctly addressed and therefore no
// longer at risk; what is left behind is an unreferenced blob, which costs disk
// and nothing else. Failing the run for it would make a rerun the operator's
// only option, and a rerun cannot fix it — the record no longer matches the
// predicate.
func (s *FileService) reclaimLegacyBlob(ctx context.Context, doc model.Document, sum *CIDNormalizeSummary) int {
	remaining, err := s.Repo.CountByExternalID(ctx, doc.ExternalID)
	if err != nil {
		s.Logger.Warn("sweep-cids: reference count failed; leaving the legacy blob in place",
			zap.String("documentID", doc.ID.String()),
			zap.String("legacyExternalID", doc.ExternalID),
			zap.Error(err))
		return 0
	}
	if remaining > 0 {
		return remaining
	}
	if err := s.Storage.Delete(doc.ExternalID); err != nil {
		s.Logger.Warn("sweep-cids: legacy blob delete failed; it is now unreferenced and can be removed by hand",
			zap.String("legacyExternalID", doc.ExternalID),
			zap.Error(err))
		return 0
	}
	sum.Reclaimed++
	return 0
}

func (s *FileService) skipCID(doc model.Document, sum *CIDNormalizeSummary, reason, detail string) {
	sum.Skipped++
	sum.NotChanged = append(sum.NotChanged, CIDNormalizeNotChanged{
		FileID: doc.ID, ExternalID: doc.ExternalID, Reason: reason, Detail: detail,
	})
	s.Logger.Info("sweep-cids: record left unchanged",
		zap.String("documentID", doc.ID.String()),
		zap.String("externalID", doc.ExternalID),
		zap.String("reason", reason),
		zap.String("detail", detail))
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
func (s *FileService) logCIDNormalizeSummary(sum CIDNormalizeSummary) {
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
		fields = append(fields, zap.Int("wouldNormalize", sum.WouldNormalize))
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

func newPacer(rate float64) *pacer {
	if rate <= 0 { // the caller rejects this; belt-and-braces so a zero can never mean "unlimited"
		rate = 1
	}
	return &pacer{interval: time.Duration(float64(time.Second) / rate)}
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
