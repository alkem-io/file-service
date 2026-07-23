package service

import (
	"context"
	"errors"
	"io"

	"github.com/google/uuid"
	"go.uber.org/zap"

	"github.com/alkem-io/file-service/internal/domain/model"
)

// DimsBackfillSummary reports what one image-dimension backfill pass did. The `sweep-dims`
// subcommand logs these counts and maps them to the Job exit code (dimsJobExitCode).
type DimsBackfillSummary struct {
	Measured     int  // dims computed and persisted
	DecodeFailed int  // decoder ran and failed → {_decodeFailed:true} sentinel persisted
	Skipped      int  // no decoder, read/persist failure, or CAS not applied → not claimed successful
	Aborted      bool // the sweep ended early (ctx cancelled or a page scan failed) — NOT a drained corpus
}

// dimsBackfillPageSize bounds one keyset page so a large first-run legacy set never loads whole.
const dimsBackfillPageSize = 500

// readFaultCapture records transport errors plus byte/EOF state from the wrapped reader, so a
// caller can tell a storage fault or short read from a decode failure after the decoder returns one
// opaque error.
type readFaultCapture struct {
	r      io.Reader
	err    error
	read   int64
	sawEOF bool
}

func (c *readFaultCapture) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	c.read += int64(n)
	if errors.Is(err, io.EOF) {
		c.sawEOF = true
	} else if err != nil {
		c.err = err
	}
	return n, err
}

// RunDimsBackfill populates image dimensions for LEGACY image rows whose content_metadata is still
// empty (spec 019/020 FR-018/FR-020). New writes already measure dims in the write pipeline
// (transcoded images inline; byte-identical images from their completed stage); this heals the
// finite pre-existing set the same way RunMimeRepair heals legacy MIME labels — off any request
// read path, so a libvips decode NEVER runs on a read / PATCH / copy. It converges: each pass
// stamps every row it visits (dims, or a decode-failed sentinel), so later passes scan a near-empty
// tail. Best-effort: never returns an error — a failed pass leaves its rows for the next run of
// the sweep-dims job (they stay unpopulated and keep matching the scan).
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
			if ctx.Err() != nil {
				sum.Aborted = true
				break
			}
			doc := page[i]
			cursor = doc.ID // keyset advances even for rows we skip, so a poison row can't wedge the sweep
			s.backfillOneDims(ctx, doc, &sum)
		}
		if sum.Aborted {
			break
		}
	}
	// Report convergence honestly: "completed" must not be logged for a pass that gave up early,
	// or an operator reads a partial sweep as a drained legacy set.
	msg := "dims-backfill: completed"
	if sum.Aborted {
		msg = "dims-backfill: ENDED EARLY (corpus not fully swept — rerun the sweep-dims job to finish)"
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
	if _, err := s.Repo.BackfillContentMetadata(ctx, doc.ID, doc.ExternalID, meta); err != nil {
		s.Logger.Warn("dims heal: persist failed; the sweep will retry (response still carries dims)",
			zap.String("documentID", doc.ID.String()), zap.Error(err))
	}
}

// backfillOneDims measures one legacy row and compare-and-sets its content_metadata. Mirrors the
// three outcomes of the retired lazy path: measured dims, a {_decodeFailed:true} sentinel (so it is
// never retried), or a skip (read/persist error, or the no-vips stub) that leaves the row for a
// future run — a transient read/persist failure is therefore re-attempted the NEXT time the
// sweep-dims job runs, since the row stays unpopulated and keeps matching the scan. The
// compare-and-set (on externalID) protects against overwriting content that was replaced between
// the scan and the persist.
//
// A GO-level panic is contained PER ROW: the sweep decodes arbitrary legacy bytes through a CGo
// image library, so without this one malformed blob would take down the whole sweep process (the
// one-shot sweep-dims job) and the pass would make no progress until an operator noticed. Contained,
// it is logged, counted skipped, and the sweep moves on. NOTE the limit: recover() catches Go panics
// only — a fault inside the C image library (SIGSEGV/abort) kills the process regardless, and no
// Go-side guard can change that.
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

	rc, size, err := s.Storage.ReadStream(doc.ExternalID)
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
	// an I/O fault is a skip, retried on the next sweep-dims run.
	src := &readFaultCapture{r: rc}
	w, h, measureErr := s.Processor.MeasureDims(src, doc.MimeType)

	switch {
	case measureErr != nil && src.err != nil:
		s.Logger.Warn("dims-backfill: storage read faulted mid-header; leaving the row for a future run",
			zap.String("externalID", doc.ExternalID), zap.Error(src.err))
		sum.Skipped++
	case measureErr != nil && src.sawEOF && src.read < size:
		// The opened handle reported `size`, but delivered fewer bytes before a clean EOF. This is a
		// storage race/fault, not proof that the immutable blob itself is undecodable. Only use this
		// signal when EOF was actually observed: a successful header-only decoder intentionally
		// stops well before size and must not be considered short.
		s.Logger.Warn("dims-backfill: storage short-read mid-header; leaving the row for a future run",
			zap.String("externalID", doc.ExternalID),
			zap.Int64("read", src.read),
			zap.Int64("expectedSize", size))
		sum.Skipped++
	case measureErr != nil:
		sentinel := model.ContentMetadata{Populated: true, DecodeFailed: true}
		applied, persistErr := s.Repo.BackfillContentMetadata(ctx, doc.ID, doc.ExternalID, sentinel)
		if persistErr != nil {
			s.Logger.Warn("dims-backfill: persist of _decodeFailed sentinel failed",
				zap.String("documentID", doc.ID.String()), zap.Error(persistErr))
			sum.Skipped++
			return
		}
		if !applied {
			sum.Skipped++
			return
		}
		sum.DecodeFailed++
	case w != nil && h != nil:
		measured := model.ContentMetadata{Populated: true, ImageWidth: w, ImageHeight: h}
		applied, persistErr := s.Repo.BackfillContentMetadata(ctx, doc.ID, doc.ExternalID, measured)
		if persistErr != nil {
			s.Logger.Warn("dims-backfill: persist of measured dims failed",
				zap.String("documentID", doc.ID.String()), zap.Error(persistErr))
			sum.Skipped++
			return
		}
		if !applied {
			sum.Skipped++
			return
		}
		sum.Measured++
	default:
		// (nil, nil, nil) — no decoder available (no-vips stub). Skip persist so a future vips run retries.
		sum.Skipped++
	}
}
