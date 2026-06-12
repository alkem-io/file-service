package service

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"slices"

	"go.uber.org/zap"

	"github.com/alkem-io/file-service/internal/domain/model"
	"github.com/alkem-io/file-service/internal/domain/port"
)

// Streaming ingest pipeline (spec 020). One pipeline serves both the create
// path (CreateDocument) and the replace path (StoreAndLink): request bytes
// flow prefix-sniff → optional streaming transcode → staged storage, and
// are published only after all validations pass. Every error path aborts
// the stage, so no partial permanent object is ever observable (FR-006).

// sniffPrefixSize matches the MIME detector's read window: type decisions
// never require buffering more than this (FR-003).
const sniffPrefixSize = 3072

// copyBufferSize is the fixed per-request budget for pass-through streaming
// (FR-001/SC-001: 256 KiB copy buffer + the sniff prefix).
const copyBufferSize = 256 << 10

var (
	// ErrOverLimit reports an upload that crossed the configured size cap
	// mid-stream (FR-005). Produced by the transport reader; recognized
	// here so the pipeline can label the outcome.
	ErrOverLimit = errors.New("upload exceeds the configured size limit")

	// ErrStalled reports an upload aborted by the progress-based idle
	// timeout (FR-009).
	ErrStalled = errors.New("upload stalled: no bytes received within the idle timeout")
)

// transcodableMIME reports whether content of this (normalized, detected)
// type streams through the image encoder. Per the 2026-06-12 clarification
// every JPEG recompresses (the buffered size guard is gone); GIF/SVG and
// non-images pass through untouched.
func transcodableMIME(mime string) bool {
	switch mime {
	case "image/heic", "image/heif", "image/webp",
		"image/png", "image/jpeg", "image/jpg":
		return true
	}
	// BMP/AVIF have no streaming saver in the image library; like GIF/SVG
	// they pass through byte-identical, dims arriving via the existing
	// lazy backfill on first access (research R5 addendum).
	return false
}

// StagedUpload is content that has been fully received into a storage stage
// but not yet validated or published.
type StagedUpload struct {
	stage port.StageWriter

	// MimeType is the type to persist (post-transcode for images).
	MimeType string
	// DetectedMIME is the effective input type (declared for empty
	// content, sniffed otherwise) — the value bucket allow-lists validate.
	DetectedMIME string
	Size         int64
	ImageWidth   *int
	ImageHeight  *int
	Measured     bool

	done bool
}

// Discard aborts the underlying stage. Idempotent; safe after Complete.
func (su *StagedUpload) Discard() {
	if su == nil || su.done {
		return
	}
	su.done = true
	_ = su.stage.Abort()
}

// countingWriter tracks bytes and remembers whether a failure came from the
// stage (write side) — so the pipeline can tell storage failures apart from
// transport failures on the shared io.Copy error.
type countingWriter struct {
	w        io.Writer
	n        int64
	writeErr error
}

func (cw *countingWriter) Write(p []byte) (int, error) {
	n, err := cw.w.Write(p)
	cw.n += int64(n)
	if err != nil {
		cw.writeErr = err
	}
	return n, err
}

// StageUpload streams r into a fresh storage stage: peek the sniff prefix,
// decide the type, route through the streaming transcoder when the content
// is a transcodable image, otherwise copy with a fixed buffer. The returned
// upload is NOT yet published — callers validate and then CompleteUpload,
// or Discard on any failure.
func (s *FileService) StageUpload(ctx context.Context, r io.Reader, declaredMIME string) (*StagedUpload, error) {
	br := bufio.NewReaderSize(r, sniffPrefixSize)
	prefix, err := br.Peek(sniffPrefixSize)
	if err != nil && !errors.Is(err, io.EOF) && !errors.Is(err, bufio.ErrBufferFull) {
		s.logIngest("prefix read failed", "", int64(len(prefix)), err)
		return nil, fmt.Errorf("read upload prefix: %w", err)
	}

	// Effective input type mirrors resolveMIME: declared for empty
	// content (no bytes to detect from), sniffed otherwise.
	var detected string
	if len(prefix) == 0 {
		detected = normalizeMIME(declaredMIME)
		if detected == "" {
			detected = "application/octet-stream"
		}
	} else {
		detected = normalizeMIME(s.Processor.DetectMIME(prefix))
	}

	su, err := s.stageContent(ctx, br, detected, len(prefix) > 0)
	if err != nil {
		return nil, err
	}
	su.DetectedMIME = detected
	return su, nil
}

// stageContent opens a stage and streams br into it under the already-
// decided MIME type — the shared back half of the create (StageUpload) and
// replace (StoreAndLinkStream) pipelines.
func (s *FileService) stageContent(ctx context.Context, br *bufio.Reader, mimeType string, hasContent bool) (*StagedUpload, error) {
	stage, err := s.Storage.OpenStage(ctx)
	if err != nil {
		return nil, fmt.Errorf("open storage stage: %w", err)
	}
	su := &StagedUpload{stage: stage, MimeType: mimeType, DetectedMIME: mimeType}
	cw := &countingWriter{w: stage}

	if hasContent && transcodableMIME(mimeType) {
		result, terr := s.Processor.TranscodeStream(br, cw, mimeType)
		if terr != nil {
			su.Discard()
			if isTransportError(terr, cw) {
				s.logIngest("transcode transport failure", mimeType, cw.n, terr)
				return nil, terr
			}
			if errors.Is(terr, port.ErrPixelBudgetExceeded) {
				s.logIngest("rejected: pixel budget", mimeType, cw.n, terr)
				return nil, terr
			}
			s.logIngest("transcode failed", mimeType, cw.n, terr)
			return nil, fmt.Errorf("%w: %w", ErrImageProcessing, terr)
		}
		su.MimeType = normalizeMIME(result.MimeType)
		su.ImageWidth = result.ImageWidth
		su.ImageHeight = result.ImageHeight
		su.Measured = result.Measured
	} else {
		buf := make([]byte, copyBufferSize)
		if _, cerr := io.CopyBuffer(cw, br, buf); cerr != nil {
			su.Discard()
			s.logIngest("stream copy failed", mimeType, cw.n, cerr)
			if cw.writeErr != nil {
				return nil, fmt.Errorf("stage write: %w", cerr)
			}
			return nil, cerr // transport-side: over-limit, stall, client abort
		}
	}

	su.Size = cw.n
	return su, nil
}

// isTransportError reports whether err originates from the request stream
// (reader side) rather than from processing or storage.
func isTransportError(err error, cw *countingWriter) bool {
	if cw.writeErr != nil {
		return false
	}
	return errors.Is(err, ErrOverLimit) || errors.Is(err, ErrStalled)
}

// CompleteUpload validates the staged content against the bucket policy and
// publishes it, then creates/dedups the document row exactly like the
// buffered implementation did.
func (s *FileService) CompleteUpload(ctx context.Context, su *StagedUpload, input model.CreateDocumentInput, allowedMimeTypes []string, maxFileSize int) (*model.Document, error) {
	if su.done {
		return nil, fmt.Errorf("complete upload: stage already discarded")
	}

	// Bucket policy validates after the stream because the metadata fields
	// trail the file part in the multipart body (research R4). The global
	// cap bounded the transfer; this is the finer per-bucket limit.
	if maxFileSize > 0 && su.Size > int64(maxFileSize) {
		su.Discard()
		return nil, ErrPayloadTooLarge
	}
	if len(allowedMimeTypes) > 0 {
		normalized := make([]string, len(allowedMimeTypes))
		for i, m := range allowedMimeTypes {
			normalized[i] = normalizeMIME(m)
		}
		if !slices.Contains(normalized, su.DetectedMIME) {
			su.Discard()
			return nil, ErrUnsupportedMediaType
		}
	}

	stored, err := su.stage.Commit()
	if err != nil {
		su.Discard()
		s.logIngest("stage commit failed", su.MimeType, su.Size, err)
		return nil, fmt.Errorf("store file: %w", err)
	}
	su.done = true

	result := port.ProcessResult{
		MimeType:    su.MimeType,
		ImageWidth:  su.ImageWidth,
		ImageHeight: su.ImageHeight,
		Measured:    su.Measured,
	}
	contentMetadata := processResultToContentMetadata(result, su.MimeType)

	s.Logger.Info("ingest accepted",
		zap.String("mimeType", su.MimeType),
		zap.Int64("bytes", su.Size),
		zap.String("outcome", "accepted"))

	if input.SkipDedup {
		return s.insertDocument(ctx, input, stored, su.MimeType, contentMetadata, su.ImageWidth, su.ImageHeight)
	}
	existing, found, err := s.findDedupDocument(ctx, stored.ExternalID, input.StorageBucketID)
	if err != nil {
		return nil, err
	}
	if found {
		existing.Reused = true
		return s.backfillIfNeeded(ctx, existing), nil
	}
	return s.insertDocument(ctx, input, stored, su.MimeType, contentMetadata, su.ImageWidth, su.ImageHeight)
}

// logIngest emits the FR-008 structured failure log: outcome derivable from
// the error class, transport vs service distinguishable by field.
func (s *FileService) logIngest(msg, mimeType string, bytes int64, err error) {
	s.Logger.Warn("ingest: "+msg,
		zap.String("mimeType", mimeType),
		zap.Int64("bytes", bytes),
		zap.Bool("transport", errors.Is(err, ErrOverLimit) || errors.Is(err, ErrStalled)),
		zap.Error(err))
}
