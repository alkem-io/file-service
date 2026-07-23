package service

import (
	"archive/zip"
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"slices"
	"strings"

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
	// they pass through byte-identical. Their headers are measured from the
	// completed stage before commit, without routing bytes through an encoder.
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
	if hasContent && strings.HasPrefix(mimeType, "image/") && !transcodableMIME(mimeType) {
		s.measureStagedImageDims(su)
	}
	return su, nil
}

// measureStagedImageDims fills metadata for byte-identical pass-through image formats
// (GIF/SVG/BMP/AVIF). The upload has already streamed into its bounded storage stage, so a
// SectionReader gives the header-only decoder a constant-memory read without buffering the object
// or changing its bytes. This keeps new writes complete after retiring lazy read-path backfill.
//
// Best-effort by design: an infrastructure read failure or no-vips build leaves Measured=false so
// the explicit sweep can retry. A decoder verdict is recorded as Measured=true even on error, which
// makes processResultToContentMetadata persist the same _decodeFailed sentinel as the sweep.
func (s *FileService) measureStagedImageDims(su *StagedUpload) {
	ra, size, err := su.stage.StagedReaderAt()
	if err != nil {
		s.logIngest("pass-through image header unavailable; leaving dims for sweep", su.MimeType, su.Size, err)
		return
	}
	src := &readFaultCapture{r: io.NewSectionReader(ra, 0, size)}
	w, h, measureErr := s.Processor.MeasureDims(src, su.MimeType)
	switch {
	case measureErr != nil && src.err != nil:
		s.logIngest("pass-through image stage read faulted; leaving dims for sweep",
			su.MimeType, su.Size, src.err)
	case measureErr != nil && src.sawEOF && src.read < size:
		s.logIngest("pass-through image stage short-read; leaving dims for sweep",
			su.MimeType, su.Size, measureErr)
	case measureErr != nil:
		su.Measured = true
	case w != nil && h != nil:
		su.ImageWidth = w
		su.ImageHeight = h
		su.Measured = true
	case w != nil || h != nil:
		// Contract violation from a processor implementation: do not bake a permanent sentinel into
		// the row. Leave it eligible for a later sweep and make the inconsistency diagnosable.
		s.logIngest("pass-through image measurement returned incomplete dimensions; leaving dims for sweep",
			su.MimeType, su.Size, nil)
	}
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

	// A front-window sniff (3072 bytes) degrades to application/zip when an
	// OOXML package's [Content_Types].xml lands past that window — the cause
	// of spurious 415s on reordered office zips (cf. spec 019 / PR #13). When
	// the sniff is the generic application/zip (and was not overridden by a
	// transcode, where MimeType is the encoder output), recover the real
	// OOXML type from the staged file's central directory and validate THAT
	// against the allow-list below. The su.MimeType == su.DetectedMIME guard
	// keeps the image-transcode path untouched (images never sniff as zip).
	if normalizeMIME(su.DetectedMIME) == "application/zip" && su.MimeType == su.DetectedMIME {
		ra, size, err := su.stage.StagedReaderAt()
		switch {
		case err != nil:
			// An infrastructure read failure (FS sync, or a future object-store
			// ranged read) is not a verdict on the content: don't treat it as
			// "not an office package". Recovery is skipped and the upload falls
			// through to plain application/zip allow-list validation, but log it
			// so a legitimate office file rejected only because its staged bytes
			// were unreadable is diagnosable rather than silent.
			s.logIngest("zip central-directory reader unavailable; skipping OOXML recovery", su.DetectedMIME, su.Size, err)
		default:
			if officeMIME, ok := detectZipOfficeMIME(ra, size); ok {
				su.DetectedMIME = officeMIME // validated against the allow-list below
				su.MimeType = officeMIME     // persisted as the real office type, not application/zip
				s.logIngest("recovered office MIME from zip central directory", officeMIME, su.Size, nil)
			}
		}
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
		// The dedup hit is over the SAME bytes we just transcoded, so the dims are already in hand.
		// Heal a legacy row here rather than waiting for the sweep: this is a plain compare-and-set
		// with values we already computed — NOT a decode on the request path, which is what belongs
		// exclusively to the write path / the sweep.
		s.healKnownDims(ctx, existing, contentMetadata)
		return existing, nil
	}
	return s.insertDocument(ctx, input, stored, su.MimeType, contentMetadata, su.ImageWidth, su.ImageHeight)
}

// detectZipOfficeMIME recovers the canonical OOXML MIME from a staged zip's
// central directory when the 3072-byte front-window sniff degraded to
// application/zip (the mechanism behind spurious 415s, cf. spec 019 / PR #13:
// an office package whose [Content_Types].xml lands past the sniff window).
// Reads only the central directory (cheap; no full decompression). Returns
// the office MIME + true for an OOXML package, or "" / false for a plain
// (non-office) or unreadable zip.
func detectZipOfficeMIME(ra io.ReaderAt, size int64) (string, bool) {
	zr, err := zip.NewReader(ra, size)
	if err != nil {
		return "", false
	}

	// An OOXML package is identified by [Content_Types].xml at the root plus
	// a top-level marker directory naming the family. This mirrors mimetype's
	// own msoxml heuristic — reliable and parse-free.
	hasContentTypes := false
	var ext string
	for _, f := range zr.File {
		switch {
		case f.Name == "[Content_Types].xml":
			hasContentTypes = true
		case strings.HasPrefix(f.Name, "ppt/"):
			ext = ".pptx"
		case strings.HasPrefix(f.Name, "word/"):
			ext = ".docx"
		case strings.HasPrefix(f.Name, "xl/"):
			ext = ".xlsx"
		}
	}
	if !hasContentTypes || ext == "" {
		return "", false
	}
	mime, ok := model.OfficeExtToMIME[ext]
	return mime, ok
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
