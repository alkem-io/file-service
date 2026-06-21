package http

import (
	"encoding/base64"
	"errors"
	"net/http"

	"github.com/google/uuid"
	"go.uber.org/zap"

	"github.com/alkem-io/file-service/internal/domain/model"
	"github.com/alkem-io/file-service/internal/domain/service"
)

// maxContentBatchSize bounds how many ids one content-batch request may carry.
// Each id costs a document lookup plus a blob read, so an unbounded list would
// let a single request fan out into an arbitrarily large burst of DB + storage
// work. 256 comfortably covers the derived-text resolver's real fan-in (a page
// of memo previews) while capping the per-request blast radius; callers with
// more ids page the request.
const maxContentBatchSize = 256

// ContentBatch handles POST /internal/file/content-batch — the batched
// counterpart to GET /internal/file/{id}/content. It takes N document ids and
// returns N content blobs (base64) in request order, so the server's
// derived-text resolvers can read a list of snapshot pointers in one round trip
// instead of an N+1 of single reads.
//
// Per-id failures are non-fatal: a malformed id, a missing document, or a
// missing blob is reported as found=false on that item while the rest of the
// batch still resolves — the endpoint returns 200 as long as the request itself
// is well-formed. Wired with the same internal (no-auth) middleware as the
// other /internal/file routes; per-blob authorization does not apply (these are
// NULL-authz internal snapshot blobs governed by the owning document).
func (h *DocumentHandler) ContentBatch(w http.ResponseWriter, r *http.Request) {
	var body ContentBatchRequest
	if !decodeStrictJSON(w, r, &body) {
		return
	}

	if len(body.Ids) == 0 {
		writeJSONError(w, http.StatusBadRequest, "ids must not be empty")
		return
	}
	if len(body.Ids) > maxContentBatchSize {
		writeJSONError(w, http.StatusBadRequest, "too many ids in one batch")
		return
	}

	// Parse every requested id up front, preserving request order. Valid ids
	// are collected into validIDs (passed to the service); a nil parsed pointer
	// marks a position whose raw id was malformed — reported as a non-fatal
	// miss without a service lookup.
	type parsedID struct {
		raw    string
		parsed *uuid.UUID
	}
	parsed := make([]parsedID, len(body.Ids))
	validIDs := make([]uuid.UUID, 0, len(body.Ids))
	for i, raw := range body.Ids {
		id, err := uuid.Parse(raw)
		if err != nil {
			parsed[i] = parsedID{raw: raw}
			continue
		}
		parsed[i] = parsedID{raw: raw, parsed: &id}
		validIDs = append(validIDs, id)
	}

	results := h.Service.ReadContentBatch(r.Context(), validIDs)

	items := make([]ContentBatchItem, len(parsed))
	resultIdx := 0
	for i, p := range parsed {
		if p.parsed == nil {
			items[i] = ContentBatchItem{ID: p.raw, Found: false, Error: "invalid id"}
			continue
		}
		items[i] = h.batchItem(p.raw, results[resultIdx])
		resultIdx++
	}

	ContentBatchResponse{Items: items}.Render(w)
}

// batchItem maps one service-layer BatchContentResult to its wire item: a hit
// carries the base64 blob + MIME; a miss carries a stable, caller-safe reason
// (the raw error may name internal storage paths, so it is logged, not
// returned). A genuine storage failure is logged at Warn for observability; a
// not-found row is an expected, silent miss (the caller's pointer simply has no
// snapshot yet).
func (h *DocumentHandler) batchItem(rawID string, res service.BatchContentResult) ContentBatchItem {
	if res.Found {
		return ContentBatchItem{
			ID:            rawID,
			Found:         true,
			MimeType:      res.MimeType,
			ContentBase64: base64.StdEncoding.EncodeToString(res.Content),
		}
	}
	if res.Err != nil && !errors.Is(res.Err, model.ErrDocumentNotFound) {
		h.Logger.Warn("content-batch: content read failed for id",
			zap.String("id", rawID), zap.Error(res.Err))
	}
	return ContentBatchItem{ID: rawID, Found: false, Error: batchMissReason(res.Err)}
}

// batchMissReason renders a stable, caller-safe reason for a non-fatal miss.
// A not-found row is "document not found"; anything else (e.g. a storage read
// failure that may embed an internal path) collapses to the generic
// "content unavailable" so the body never leaks internals.
func batchMissReason(err error) string {
	if errors.Is(err, model.ErrDocumentNotFound) {
		return "document not found"
	}
	return "content unavailable"
}
