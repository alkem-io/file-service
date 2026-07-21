package service

import (
	"context"
	"errors"
	"io"

	"github.com/google/uuid"
	"go.uber.org/zap"

	"github.com/alkem-io/file-service/internal/domain/model"
)

// DimsBackfillSummary reports what the boot-time image-dimension backfill sweep did. The caller
// (cmd/server) feeds these counts into the dims_backfill_total metric.
type DimsBackfillSummary struct {
	Measured     int  // dims computed and persisted
	DecodeFailed int  // decoder ran and failed → {_decodeFailed:true} sentinel persisted
	Skipped      int  // storage read / persist failed, or no decoder (stub) → left for a future run
	Aborted      bool // the sweep ended early (ctx cancelled or a page scan failed) — NOT a drained corpus
}

// dimsBackfillPageSize bounds one keyset page so a large first-run legacy set never loads whole.
const dimsBackfillPageSize = 500

// readFaultCapture records a non-EOF read error from the wrapped reader, so a caller can tell a
// storage TRANSPORT fault from a decode failure after the decoder returns one opaque error.
type readFaultCapture struct {
	r   io.Reader
	err error
}

func (c *readFaultCapture) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	if err != nil && !errors.Is(err, io.EOF) {
		c.err = err
	}
	return n, err
}

// RunDimsBackfill populates image dimensions for LEGACY image rows whose content_metadata is still
// empty (spec 019/020 FR-018/FR-020). New writes already measure dims at write time (Process); this
// heals the finite pre-existing set the same way RunMimeRepair heals legacy MIME labels — off any
// request path, so a libvips decode NEVER runs on a read / PATCH / copy. It converges: each pass
// stamps every row it visits (dims, or a decode-failed sentinel), so later passes scan a near-empty
// tail. Best-effort: never returns an error — a failed pass just leaves rows for the next boot.
func (s *FileService) RunDimsBackfill(ctx context.Context) DimsBackfillSummary {
	var sum DimsBackfillSummary
	var cursor uuid.UUID // uuid.Nil → first page
	for {
		if ctx.Err() != nil { // shutdown: stop promptly rather than scanning on through a drain
			sum.Aborted = true
			break
		}
		page, err := s.Repo.ListImagesNeedingDims(ctx, cursor, dimsBackfillPageSize)
		if err != nil {
			s.Logger.Error("dims-backfill: page scan failed; ending sweep early", zap.Error(err))
			sum.Aborted = true
			break
		}
		if len(page) == 0 {
			break
		}
		for i := range page {
			doc := page[i]
			cursor = doc.ID // keyset advances even for rows we skip, so a poison row can't wedge the sweep
			s.backfillOneDims(ctx, doc, &sum)
		}
	}
	// Report convergence honestly: "completed" must not be logged for a pass that gave up early,
	// or an operator reads a partial sweep as a drained legacy set.
	msg := "dims-backfill: completed"
	if sum.Aborted {
		msg = "dims-backfill: ENDED EARLY (corpus not fully swept; the next boot resumes)"
	}
	s.Logger.Info(msg,
		zap.Bool("aborted", sum.Aborted),
		zap.Int("measured", sum.Measured),
		zap.Int("decodeFailed", sum.DecodeFailed),
		zap.Int("skipped", sum.Skipped))
	return sum
}

// healKnownDims persists dims the caller ALREADY computed onto a legacy row whose
// content_metadata is still empty — the dedup-hit case, where the matched row is over the very
// bytes we just processed. This is deliberately allowed on a request path: it is a compare-and-set
// with values already in hand, not a decode (the thing that belongs only to the write path or the
// sweep). Best-effort: a persist failure leaves the row for the sweep, and the caller's response
// still carries the dims. No-op when there is nothing to add or the row is already decided.
func (s *FileService) healKnownDims(ctx context.Context, doc *model.Document, meta model.ContentMetadata) {
	if doc == nil || doc.ContentMetadata.Populated || !meta.Populated || meta.ImageWidth == nil || meta.ImageHeight == nil {
		return
	}
	doc.ImageWidth = meta.ImageWidth
	doc.ImageHeight = meta.ImageHeight
	doc.ContentMetadata = meta
	if err := s.Repo.BackfillContentMetadata(ctx, doc.ID, doc.ExternalID, meta); err != nil {
		s.Logger.Warn("dims heal: persist failed; the sweep will retry (response still carries dims)",
			zap.String("documentID", doc.ID.String()), zap.Error(err))
	}
}

// backfillOneDims measures one legacy row and compare-and-sets its content_metadata. Mirrors the
// three outcomes of the retired lazy path: measured dims, a {_decodeFailed:true} sentinel (so it is
// never retried), or a skip (read/persist error, or the no-vips stub) that leaves the row for a
// future boot — a transient read/persist failure is therefore re-attempted on the NEXT boot, since
// the row stays unpopulated and keeps matching the scan. The compare-and-set (on externalID)
// protects against overwriting content that was replaced between the scan and the persist.
//
// A GO-level panic is contained PER ROW: the sweep runs in a bare background goroutine, so without
// this one malformed blob would take down the whole process at boot (a crash-loop on a poison row);
// the retired lazy path was implicitly shielded by net/http's per-connection recover. NOTE the limit:
// recover() catches Go panics only — a fault inside the C image library (SIGSEGV/abort) kills the
// process regardless, and no Go-side guard can change that.
func (s *FileService) backfillOneDims(ctx context.Context, doc model.Document, sum *DimsBackfillSummary) {
	defer func() {
		if rec := recover(); rec != nil {
			s.Logger.Error("dims-backfill: panic measuring a row; skipping it",
				zap.String("documentID", doc.ID.String()),
				zap.String("externalID", doc.ExternalID),
				zap.Any("panic", rec))
			sum.Skipped++
		}
	}()

	rc, _, err := s.Storage.ReadStream(doc.ExternalID)
	if err != nil {
		s.Logger.Warn("dims-backfill: storage read failed; leaving content_metadata empty",
			zap.String("externalID", doc.ExternalID), zap.Error(err))
		sum.Skipped++
		return
	}
	// Deferred (not closed inline) so a panic inside MeasureDims can't leak the handle.
	defer func() { _ = rc.Close() }()

	// Header-only, streamed — never buffers the whole image. The reader is wrapped so a TRANSPORT
	// failure (the volume blipped mid-header: EIO, an NFS timeout, a truncated blob) can be told
	// apart from a genuine decode failure: the decoder surfaces both as one opaque load error, and
	// treating a transient read fault as "undecodable" would persist the PERMANENT
	// {_decodeFailed:true} sentinel — excluding a perfectly good image from this and every future
	// sweep, with nothing that would ever retry it. Only a real decode failure earns the sentinel;
	// an I/O fault is a skip, retried on the next boot.
	src := &readFaultCapture{r: rc}
	w, h, measureErr := s.Processor.MeasureDims(src, doc.MimeType)

	switch {
	case measureErr != nil && src.err != nil:
		s.Logger.Warn("dims-backfill: storage read faulted mid-header; leaving the row for a future run",
			zap.String("externalID", doc.ExternalID), zap.Error(src.err))
		sum.Skipped++
	case measureErr != nil:
		sentinel := model.ContentMetadata{Populated: true, DecodeFailed: true}
		if persistErr := s.Repo.BackfillContentMetadata(ctx, doc.ID, doc.ExternalID, sentinel); persistErr != nil {
			s.Logger.Warn("dims-backfill: persist of _decodeFailed sentinel failed",
				zap.String("documentID", doc.ID.String()), zap.Error(persistErr))
			sum.Skipped++
			return
		}
		sum.DecodeFailed++
	case w != nil && h != nil:
		measured := model.ContentMetadata{Populated: true, ImageWidth: w, ImageHeight: h}
		if persistErr := s.Repo.BackfillContentMetadata(ctx, doc.ID, doc.ExternalID, measured); persistErr != nil {
			s.Logger.Warn("dims-backfill: persist of measured dims failed",
				zap.String("documentID", doc.ID.String()), zap.Error(persistErr))
			sum.Skipped++
			return
		}
		sum.Measured++
	default:
		// (nil, nil, nil) — no decoder available (no-vips stub). Skip persist so a future vips run retries.
		sum.Skipped++
	}
}
