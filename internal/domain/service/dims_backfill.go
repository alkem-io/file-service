package service

import (
	"context"

	"github.com/google/uuid"
	"go.uber.org/zap"

	"github.com/alkem-io/file-service/internal/domain/model"
)

// DimsBackfillSummary reports what the boot-time image-dimension backfill sweep did. The caller
// (cmd/server) feeds these counts into the dims_backfill_total metric.
type DimsBackfillSummary struct {
	Measured     int // dims computed and persisted
	DecodeFailed int // decoder ran and failed → {_decodeFailed:true} sentinel persisted
	Skipped      int // storage read / persist failed, or no decoder (stub) → left for a future run
}

// dimsBackfillPageSize bounds one keyset page so a large first-run legacy set never loads whole.
const dimsBackfillPageSize = 500

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
		page, err := s.Repo.ListImagesNeedingDims(ctx, cursor, dimsBackfillPageSize)
		if err != nil {
			s.Logger.Error("dims-backfill: page scan failed; ending sweep", zap.Error(err))
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
	s.Logger.Info("dims-backfill: completed",
		zap.Int("measured", sum.Measured),
		zap.Int("decodeFailed", sum.DecodeFailed),
		zap.Int("skipped", sum.Skipped))
	return sum
}

// backfillOneDims measures one legacy row and compare-and-sets its content_metadata. Mirrors the
// three outcomes of the retired lazy path: measured dims, a {_decodeFailed:true} sentinel (so it is
// never retried), or a skip (read/persist error, or the no-vips stub) that leaves the row for a
// future boot. The compare-and-set (on externalID) protects against overwriting content that was
// replaced between the scan and the persist.
func (s *FileService) backfillOneDims(ctx context.Context, doc model.Document, sum *DimsBackfillSummary) {
	rc, _, err := s.Storage.ReadStream(doc.ExternalID)
	if err != nil {
		s.Logger.Warn("dims-backfill: storage read failed; leaving content_metadata empty",
			zap.String("externalID", doc.ExternalID), zap.Error(err))
		sum.Skipped++
		return
	}
	// Header-only, streamed — never buffers the whole image.
	w, h, measureErr := s.Processor.MeasureDims(rc, doc.MimeType)
	_ = rc.Close()

	switch {
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
