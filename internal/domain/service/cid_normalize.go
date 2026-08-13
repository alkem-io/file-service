package service

import (
	"context"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"strings"
	"time"

	"github.com/google/uuid"
	"go.uber.org/zap"
	"golang.org/x/time/rate"

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

// sharedUnknown marks a reference count that was never taken — the equal-fold
// bail-out and a failed count query both reach the report without one. It is NOT
// zero: the report's `sharedWith` is what the runbook's post-pass reconciliation
// reads, and a fabricated 0 tells an operator "nothing else pointed at this blob,
// safe to GC" about a blob that may still back many rows. Deleting on that advice
// would undo the equal-fold guard by hand. Serialized as an omitted field, so a
// reader can tell "unknown" from "counted zero".
const sharedUnknown = -1

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
	// DetailTruncated means the per-record arrays hit their retention ceiling, so
	// the report lists fewer entries than the counts. The journal is complete
	// regardless; the report says so explicitly rather than looking short.
	DetailTruncated bool
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

// cidRetainedDetailLimit bounds how many per-record entries the summary holds.
// The corpus size in production is unmeasured, and these arrays are copied again
// by the report encoder at the very end of the pass — the worst moment to
// discover the ceiling. Counts stay exact past the limit; only the per-record
// detail stops accumulating, and it is not the durable record anyway: the
// fsynced journal carries every mapping, line by line, as the pass runs.
const cidRetainedDetailLimit = 20_000

// recordChanged / recordNotChanged keep the counters authoritative and the
// slices bounded, which is why the counters are not derived from len().
// recordChanged returns the index of the retained entry, or -1 when the ceiling
// was hit and only the counts will reflect this record.
func (sum *CIDNormalizeSummary) recordChanged(c CIDNormalizeChange) int {
	if len(sum.Changed) < cidRetainedDetailLimit {
		sum.Changed = append(sum.Changed, c)
		return len(sum.Changed) - 1
	}
	sum.DetailTruncated = true
	return -1
}

// updateChanged fills in what became of the legacy blob, once that is known.
func (sum *CIDNormalizeSummary) updateChanged(idx, sharedWith int, legacyBlob string) {
	if idx < 0 || idx >= len(sum.Changed) {
		return
	}
	sum.Changed[idx].SharedWith = sharedWith
	sum.Changed[idx].LegacyBlob = legacyBlob
}

func (sum *CIDNormalizeSummary) recordNotChanged(n CIDNormalizeNotChanged) {
	if len(sum.NotChanged) < cidRetainedDetailLimit {
		sum.NotChanged = append(sum.NotChanged, n)
		return
	}
	sum.DetailTruncated = true
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
	// The rate is normalized HERE, not only at the CLI. The report declares `rate`
	// as a positive finite number, and a NaN reaching json.Marshal takes the whole
	// report down — the one artifact this pass cannot afford to lose. Recording
	// what the limiter actually enforces also stops the report claiming a ceiling
	// that was never in force.
	pace := newPacer(opts.Rate)
	sum := CIDNormalizeSummary{DryRun: opts.DryRun, Rate: float64(pace.Limit()), StartedAt: time.Now().UTC()}
	run := &cidRun{opts: opts, journal: cidJournalName(sum.StartedAt)}

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
		// A page shorter than the limit is the last one. Looping for an empty page
		// would cost one more scan of a predicate Postgres cannot index — the
		// regex is evaluated per row — purely to be told there is nothing left.
		lastPage := len(page) < cidNormalizePageSize
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

			// Paced in BOTH modes. The preview probes the store for each record, so
			// it is no longer the metadata-only scan that once justified exempting
			// it — an unpaced preview would be the one mode hammering the shared
			// volume the ceiling exists to protect.
			if err := pace.Wait(ctx); err != nil {
				sum.Aborted = true
				break
			}
			if opts.DryRun {
				s.previewOneCID(doc, &sum)
				continue
			}
			s.normalizeOneCID(ctx, doc, &sum, run)
		}
		if sum.Aborted || lastPage {
			break
		}
	}

	sum.FinishedAt = time.Now().UTC()
	// A dry run writes nothing at all (FR-007); a real pass's report is the only
	// durable record of the old→new mapping, so write it even for an aborted pass.
	// The summary is logged AFTER the report write, not before it. Taken by value
	// beforehand, it could not see ReportFailed — which writeCIDNormalizeReport is
	// the sole assigner of — so a pass that irreversibly reclaimed blobs and then
	// lost its report logged `completed … failed=0` on the very line the runbook
	// tells the operator to act on, while the process exited 1 and the report's own
	// mappingIncomplete said otherwise.
	// Ordering, and both halves matter:
	//   1. write the report — the only assigner of ReportFailed
	//   2. log the summary — so it can SEE ReportFailed
	//   3. log the report path last — the runbook tells the operator to look there
	if !opts.DryRun && opts.Report != nil {
		s.writeCIDNormalizeReport(&sum, run)
	} else if run.journalFailed {
		// writeCIDNormalizeReport is the usual assigner of ReportFailed, but it is
		// itself sink-guarded — so with no sink at all, a real pass that could record
		// nothing would have exited 0. The verdict must not depend on whether the
		// thing that reports the verdict was configured.
		sum.ReportFailed = true
	}
	s.logCIDNormalizeSummary(sum, run)
	s.logCIDNormalizeReportPath(sum, run)
	return sum
}

// cidRun carries the per-run state the journal needs, kept off the summary so
// the summary stays a pure result.
type cidRun struct {
	opts    CIDNormalizeOptions
	journal string
	// journalPath is where the journal actually landed, for the closing log line.
	journalPath string
	// reportPath is where the summary report landed, logged as the run's last line.
	reportPath string
	// journalFailed records that at least one mapping could not be made durable
	// before its blob was reclaimed.
	journalFailed bool
	// published memoizes legacy name → digest within this pass. One legacy blob
	// commonly backs several records (FR-005), and without this each of them
	// re-reads the whole blob and re-writes it into a staging file only for
	// Commit to discard the copy as a dedup hit — K full read+write cycles for
	// one blob, against the shared production volume this sweep is rate-limited
	// to protect. A legacy blob cannot change under us mid-pass: a Replace writes
	// a NEW digest name and leaves the legacy name alone.
	published map[string]string
}

// cidDigestMemoLimit bounds the memo so a corpus of unknown size cannot turn a
// throughput optimization into the memory problem it was meant to avoid. Past the
// limit the sweep simply republishes, which is correct, just slower.
const cidDigestMemoLimit = 10_000

func (r *cidRun) digestFor(legacy string) (string, bool) {
	d, ok := r.published[legacy]
	return d, ok
}

// forgetDigest drops a memoized mapping whose published blob is about to be
// deleted, so no later record is handed a name that no longer resolves.
//
// It drops the entry for this legacy name AND every other entry pointing at the
// same digest. Dropping only the key was the bug: byte-identical content can sit
// under two legacy names (a CIDv0 `Qm…` and a CIDv1 `bafy…`, or an uppercase-hex
// name), so a second key can still map to a digest that is about to be destroyed
// — and the record that takes that surviving hit is repointed to nothing.
func (r *cidRun) forgetDigest(legacy string) {
	digest, ok := r.published[legacy]
	delete(r.published, legacy)
	if !ok {
		return
	}
	for k, v := range r.published {
		if v == digest {
			delete(r.published, k)
		}
	}
}

func (r *cidRun) rememberDigest(legacy, digest string) {
	if len(r.published) >= cidDigestMemoLimit {
		return
	}
	if r.published == nil {
		r.published = make(map[string]string)
	}
	r.published[legacy] = digest
}

// cidCancellableReader makes a long blob copy responsive to shutdown. io.Copy
// over a plain file reader cannot be interrupted, so without this a SIGTERM
// arriving mid-blob is absorbed until the copy finishes — and if that outlasts
// terminationGracePeriodSeconds, kubernetes SIGKILLs the pod, which is the one
// exit path that skips the end-of-pass report.
type cidCancellableReader struct {
	ctx context.Context
	r   io.Reader
}

func (c *cidCancellableReader) Read(p []byte) (int, error) {
	if err := c.ctx.Err(); err != nil {
		return 0, err
	}
	return c.r.Read(p)
}

// sha3ExternalID reports whether a name is already in the current addressing
// scheme — the same test the scan predicate applies, in Go.
func sha3ExternalID(externalID string) bool {
	if len(externalID) != 64 {
		return false
	}
	for _, c := range externalID {
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}

// reclaimOrphanPublish drops a blob this pass published for a record that then
// lost its compare-and-set, when nothing references it.
//
// Round one left these leaked, justified by "that digest may be the legitimate
// home of bytes other records reference". That justification is exactly right in
// general and exactly wrong in the case it was written for: when the CAS lost to
// a Replace, the Replace stored DIFFERENT bytes, so the blob we published is
// referenced by nobody and never will be. The refcount decides which case this
// is — the same rule that gates every other deletion in the service.
// It carries the SAME guards as reclaimLegacyBlob, because it deletes from a more
// dangerous namespace: a legacy CID can never be produced by a new upload, whereas
// the digest this path targets is exactly what every matching upload dedups into.
func (s *FileService) reclaimOrphanPublish(ctx context.Context, published string, doc model.Document, createdHere bool) {
	if published == "" {
		return
	}
	// EqualFold, not ==. Uppercase-hex names are in scope (the scan predicate wants
	// LOWERCASE hex) and Commit returns the lowercase digest, so on a
	// case-insensitive volume the two names are the SAME file — and a byte
	// comparison passes the guard that was written to stop exactly this. Its
	// sibling at reclaimLegacyBlob already folds; this one did not, which made the
	// `==` test dead code: the one case it existed to catch is the one it missed.
	if strings.EqualFold(published, doc.ExternalID) {
		return
	}
	// Only ever delete a blob THIS pass created. A dedup hit means the bytes were
	// already on the store under that digest before we touched it — someone else's
	// content, possibly an upload whose row has not committed yet — and unlinking
	// it makes their record 404 permanently.
	if !createdHere {
		return
	}
	remaining, err := s.Repo.CountByExternalID(ctx, published)
	if err != nil {
		// Silently folding this into the referenced case leaked a blob per record
		// with no diagnostics at all.
		s.Logger.Warn("sweep-cids: could not count references for an orphaned publish; leaving it in place",
			zap.String("externalID", published), zap.Error(err))
		return
	}
	if remaining > 0 {
		return
	}
	if !s.cleanupOrphanedBlob(ctx, published) {
		return
	}
	s.Logger.Info("sweep-cids: dropped a blob published for a record that then lost its compare-and-set",
		zap.String("documentID", doc.ID.String()),
		zap.String("externalID", published))
	// Same late-reference re-check the legacy path does: an upload that dedups onto
	// this digest can commit its row between the count and the unlink.
	if resurrected, err := s.Repo.CountByExternalID(ctx, published); err == nil && resurrected > 0 {
		s.Logger.Error("sweep-cids: a record began referencing a published digest AFTER it was reclaimed — those rows now 404",
			zap.String("externalID", published),
			zap.Int("rows", resurrected))
	}
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
			sum.recordNotChanged(CIDNormalizeNotChanged{
				FileID: doc.ID, ExternalID: doc.ExternalID, Reason: reasonWriteFailed,
				Detail: fmt.Sprintf("panic: %v", rec),
			})
		}
	}()

	// A memo hit skips the republish, so it must first prove the memoized blob is
	// still there. It may not be: an ordinary DELETE of any record naming that
	// digest drops its refcount to zero and the service's own cleanup unlinks it,
	// and a memo entry keyed on a DIFFERENT legacy name can outlive
	// reclaimOrphanPublish destroying the digest it points at. Either way the
	// record would be repointed to nothing and then have its own legacy blob
	// reclaimed — a live row naming an absent blob, permanently, in direct
	// violation of FR-004. A stat is cheap next to the full read+write the memo
	// exists to avoid; an unreadable probe is treated as a miss, which merely
	// republishes.
	// createdHere stays false on a memo hit: the blob may have been created by an
	// earlier record in this pass, but it may equally be a dedup hit onto content
	// that was already there. Only a publish that reports Created can be safely
	// reclaimed, so an unprovable case leaks a blob rather than risking one.
	createdHere := false
	newExternalID, ok := run.digestFor(doc.ExternalID)
	if ok {
		present, err := s.Storage.Exists(newExternalID)
		if err != nil || !present {
			run.forgetDigest(doc.ExternalID)
			ok = false
		}
	}
	if !ok {
		newExternalID, createdHere, ok = s.publishUnderDigest(ctx, doc, sum)
		if !ok {
			return
		}
		run.rememberDigest(doc.ExternalID, newExternalID)
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
		if ctx.Err() != nil {
			// Shutdown is not a fault of this record. publishUnderDigest already makes
			// this distinction; without it here, a SIGTERM lands as a genuine
			// `write_failed` against a healthy object and is written into the only
			// durable audit trail of an irreversible migration. It is reachable on
			// every 2nd+ record sharing a legacy blob, where the memo skips the
			// publish and the CAS is the first ctx-touching call.
			sum.Aborted = true
			s.Logger.Warn("sweep-cids: shutdown during a repoint; the record is untouched and remains in scope",
				zap.String("documentID", doc.ID.String()),
				zap.String("externalID", doc.ExternalID))
			return
		}
		s.failCID(doc, sum, reasonWriteFailed, "repoint: "+err.Error())
		return
	}
	if !applied {
		// A concurrent writer moved externalID or version between our read and our
		// write. Retrying would fight it, so this record is skipped either way — but
		// the two cases are NOT the same, and claiming "the record is already
		// correct" is only true for one of them:
		//
		//   - a Replace wrote a proper digest, so the row is fixed and leaves the
		//     work-list;
		//   - a metadata PATCH bumps `version` WITHOUT touching externalID, so the
		//     row is still legacy-named and still in scope. It is not lost — it
		//     matches the predicate, so the next pass picks it up — but a summary
		//     that called it "already correct" would misreport convergence.
		//
		// The distinction costs one read, and it is what makes a re-run's shrinking
		// count trustworthy.
		detail := "compare-and-set lost to a concurrent writer"
		if current, err := s.Repo.GetByID(ctx, doc.ID); err == nil && !sha3ExternalID(current.ExternalID) {
			detail = "compare-and-set lost to a concurrent metadata write; the record is STILL legacy-named and remains in scope for the next pass"
		}
		// Order matters: forget the memo BEFORE dropping the blob. Otherwise the next
		// record sharing this legacy name gets a memo hit for a digest that no longer
		// exists, is repointed to it, and then has its own legacy blob reclaimed —
		// leaving a live record pointing at nothing, permanently.
		run.forgetDigest(doc.ExternalID)
		s.reclaimOrphanPublish(ctx, newExternalID, doc, createdHere)
		s.skipCID(doc, sum, reasonConcurrentChange, detail)
		return
	}

	sum.Normalized++
	change := CIDNormalizeChange{
		FileID:             doc.ID,
		PreviousExternalID: doc.ExternalID,
		NewExternalID:      newExternalID,
	}
	// Recorded before reclamation is attempted, so a panic inside reclaim cannot
	// drop a record that `normalized` has already counted — the recover handler
	// would then count the same record failed as well.
	// legacyBlob is seeded with the honest value for "reclamation was never
	// attempted". The field has no omitempty and the cross-repo schema requires it
	// with an enum, so a panic between here and updateChanged would otherwise emit
	// `"legacyBlob": ""` and make the whole retained report fail validation.
	change.LegacyBlob = legacyBlobRetained
	idx := sum.recordChanged(change)

	// Durable BEFORE the bytes are destroyed — that ordering IS the guarantee that
	// a pass killed mid-corpus loses no mapping for a blob it already destroyed. If
	// the line did not land, the guarantee does not hold for this record, so the
	// blob is kept rather than destroyed with nothing recording its old name. That
	// costs disk; the alternative costs the only copy of the mapping.
	if !s.journalChange(change, run) {
		sum.updateChanged(idx, 0, legacyBlobRetained)
		s.Logger.Error("sweep-cids: keeping the legacy blob because its mapping is not durable",
			zap.String("documentID", doc.ID.String()),
			zap.String("legacyExternalID", doc.ExternalID))
		return
	}
	shared, fate := s.reclaimLegacyBlob(ctx, doc, newExternalID, sum)
	sum.updateChanged(idx, shared, fate)
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
func (s *FileService) publishUnderDigest(ctx context.Context, doc model.Document, sum *CIDNormalizeSummary) (name string, created, ok bool) {
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
		return "", false, false
	}
	defer func() { _ = rc.Close() }()

	stage, err := s.Storage.OpenStage(ctx)
	if err != nil {
		s.failCID(doc, sum, reasonWriteFailed, "open stage: "+err.Error())
		return "", false, false
	}
	copied, err := io.Copy(stage, &cidCancellableReader{ctx: ctx, r: rc})
	if err != nil {
		_ = stage.Abort()
		// Shutdown is not a fault of this record. Recording it as one would put a
		// false I/O failure against a healthy object in the durable report, and —
		// if the signal arrived while processing the last record — would let the
		// pass report "completed" over a corpus it never finished.
		if ctx.Err() != nil {
			sum.Aborted = true
			s.Logger.Info("sweep-cids: shutdown during a blob copy; ending the pass without blaming the record",
				zap.String("documentID", doc.ID.String()),
				zap.String("externalID", doc.ExternalID))
			return "", false, false
		}
		s.failCID(doc, sum, reasonReadFailed, "copy to stage: "+err.Error())
		return "", false, false
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
		return "", false, false
	}
	stored, err := stage.Commit()
	if err != nil {
		_ = stage.Abort()
		s.failCID(doc, sum, reasonWriteFailed, "commit: "+err.Error())
		return "", false, false
	}
	return stored.ExternalID, stored.Created, true
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
func (s *FileService) reclaimLegacyBlob(ctx context.Context, doc model.Document, newExternalID string, sum *CIDNormalizeSummary) (int, string) {
	// Uppercase hex is one of the legacy forms known to exist on disk (see the
	// storage adapter's note on the key allow-list), and it matches the scan
	// predicate because the predicate demands LOWERCASE hex. Its bytes therefore
	// hash to the same 64 characters in the other case. On a case-sensitive volume
	// those are two distinct files and reclaiming the old one is right; on a
	// case-insensitive one they are the SAME file, and deleting the legacy name
	// would unlink the blob the record was just repointed to — destroying live
	// content in the name of tidying up. The store gives us no way to ask which
	// kind of volume it is, so the equal-fold case is simply never reclaimed. The
	// cost is one retained blob per uppercase-hex record; the alternative cost is
	// data loss.
	if strings.EqualFold(doc.ExternalID, newExternalID) {
		s.Logger.Warn("sweep-cids: legacy name differs from its digest only by case; not reclaiming, since on a case-insensitive volume they are the same file",
			zap.String("documentID", doc.ID.String()),
			zap.String("legacyExternalID", doc.ExternalID),
			zap.String("newExternalID", newExternalID))
		return sharedUnknown, legacyBlobRetained
	}
	remaining, err := s.Repo.CountByExternalID(ctx, doc.ExternalID)
	if err != nil {
		s.Logger.Warn("sweep-cids: reference count failed; leaving the legacy blob in place",
			zap.String("documentID", doc.ID.String()),
			zap.String("legacyExternalID", doc.ExternalID),
			zap.Error(err))
		return sharedUnknown, legacyBlobRetained
	}
	if remaining > 0 {
		return remaining, legacyBlobStillInUse
	}
	if !s.cleanupOrphanedBlob(ctx, doc.ExternalID) {
		return remaining, legacyBlobRetained // a genuinely counted zero
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
func (s *FileService) journalChange(c CIDNormalizeChange, run *cidRun) bool {
	if run.opts.Report == nil {
		// A DRY RUN never reaches here, so this is a real pass with no sink. Returning
		// true would ungate reclamation for every record: the corpus is destroyed, no
		// journal and no report are written, and ReportFailed — only ever assigned by
		// writeCIDNormalizeReport, which is itself guarded on a non-nil sink — stays
		// false, so the Job exits 0 over a destroyed corpus with no audit trail. The
		// service enforces the sibling FR-007 rule itself rather than trusting its
		// caller; this one is enforced the same way.
		run.journalFailed = true
		s.Logger.Error("sweep-cids: no report sink configured for a real pass; refusing to reclaim, since no mapping can be made durable",
			zap.String("documentID", c.FileID.String()))
		return false
	}
	line, err := cidJournalLine(c)
	if err != nil {
		run.journalFailed = true
		s.Logger.Error("sweep-cids: could not encode a journal line; this mapping is NOT durable",
			zap.String("documentID", c.FileID.String()), zap.Error(err))
		return false
	}
	path, err := run.opts.Report.AppendJournal(run.journal, line)
	if err != nil {
		run.journalFailed = true
		s.Logger.Error("sweep-cids: could not append to the journal; this mapping is NOT durable and its blob is about to be reclaimed",
			zap.String("documentID", c.FileID.String()),
			zap.String("previousExternalID", c.PreviousExternalID),
			zap.String("newExternalID", c.NewExternalID),
			zap.Error(err))
		return false
	}
	run.journalPath = path
	return true
}

func (s *FileService) skipCID(doc model.Document, sum *CIDNormalizeSummary, reason, detail string) {
	sum.Skipped++
	sum.recordNotChanged(CIDNormalizeNotChanged{
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
	sum.recordNotChanged(CIDNormalizeNotChanged{
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

// newPacer bounds throughput to a fixed objects/second ceiling. Each object costs
// a blob read, a blob write and a database round-trip against SHARED PRODUCTION
// infrastructure, so an unbounded full-corpus pass would be a self-inflicted load
// test — the ceiling is what makes the sweep safe to run without a maintenance
// window (FR-014, FR-015).
//
// burst=1 is deliberate: a larger burst would let the pass bank idle time and then
// spend it all at once, which is precisely the shape of load this prevents.
//
// The rate is normalized before it reaches the limiter. The caller rejects a
// non-positive or non-finite value, but this is the layer that makes a malformed
// one impossible to read as "unlimited" even if a future caller forgets — round
// one of review found exactly that hole in the hand-rolled version this replaces
// (+Inf divided down to a zero interval).
func newPacer(objectsPerSecond float64) *rate.Limiter {
	// !(v > 0) rather than v <= 0 so NaN — for which every comparison is false —
	// lands here too.
	if !(objectsPerSecond > 0) || math.IsInf(objectsPerSecond, 1) {
		objectsPerSecond = 1
	}
	// Floor the rate so no single object can wait longer than maxPacerInterval.
	// The hand-rolled pacer this replaced clamped the computed interval; swapping
	// in rate.Limiter dropped that, and the flag validation does not cover it —
	// it rejects only non-positive and infinite values. A perfectly valid
	// `--rate 1e-5` then means 27h46m PER OBJECT, and 1e-12 means 292 years, with
	// the Job's context carrying no deadline: the pod hangs indefinitely having
	// logged only "sweep-cids job starting", with backoffLimit 0 and no
	// activeDeadlineSeconds to end it, and must be killed by hand.
	if floor := 1 / maxPacerInterval.Seconds(); objectsPerSecond < floor {
		objectsPerSecond = floor
	}
	return rate.NewLimiter(rate.Limit(objectsPerSecond), 1)
}

// maxPacerInterval caps how long one object may wait. A rate low enough to exceed
// it is not a throughput choice an operator can have meant — it is a hang.
const maxPacerInterval = time.Hour
