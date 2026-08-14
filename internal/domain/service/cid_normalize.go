package service

import (
	"context"
	"errors"
	"fmt"
	"io"
	"math"
	"time"

	"go.uber.org/zap"
	"golang.org/x/time/rate"

	"github.com/alkem-io/file-service/internal/domain/model"
	"github.com/alkem-io/file-service/internal/domain/port"
)

// Reasons a legacy blob was not normalized. The vocabulary is fixed by
// contracts/run-report.schema.json. The first two are SKIPS — a re-run cannot
// change the outcome, so they must never fail the run or the operator has no
// terminating condition. The last two are FAILURES, where a re-run can help.
// Reasons a legacy blob was not normalized, fixed by
// contracts/run-report.schema.json. unaddressable_name and already_addressed are SKIPS — a re-run cannot change the outcome, so they must never
// fail the run or the operator has no terminating condition. read_failed and
// write_failed are FAILURES, where a re-run can help. The split, not the raw count,
// drives the exit code.
const (
	// reasonUnaddressableName: the store's key rules refuse the name, so its bytes
	// can never be fetched. Also unfixable by re-running (the name must be repaired
	// first), but logged at WARN because — unlike the absent cohort — it is not an
	// expected population: no known bucket-A name can produce it.
	reasonUnaddressableName = "unaddressable_name"
	// reasonReadFailed: the store could not deliver the bytes, or delivered fewer
	// than it promised. Transient, so a re-run is warranted.
	reasonReadFailed = "read_failed"
	// reasonWriteFailed: publishing the bytes or renaming the rows errored.
	reasonWriteFailed = "write_failed"
	// reasonAlreadyAddressed: the name differs from its digest only by case and no row
	// names it, so the content is already at its correct address. A skip, and not a
	// defect — but it must be COUNTED, or the buckets stop accounting for the scan.
	reasonAlreadyAddressed = "already_addressed"
)

// cidNormalizeMaxPerPass bounds how many legacy names one pass takes on. It is a
// backstop, not a page size: the store is walked once and only MATCHES are kept, so
// this only trips if production holds far more legacy content than expected. When it
// trips the pass is truncated, which the summary says explicitly — reading it as a
// drained corpus is how a migration gets declared done early.
const cidNormalizeMaxPerPass = 50_000

// maxPacerInterval caps how long one blob may wait. A rate low enough to exceed it
// is not a throughput choice an operator can have meant — it is a hang, and the Job
// carries no deadline and backoffLimit 0.
const maxPacerInterval = time.Hour

// CIDNormalizeChange records one legacy blob that was renamed. Because the blob is
// reclaimed in the same pass, this is the only surviving witness of the old name
// (FR-016, SC-009).
type CIDNormalizeChange struct {
	PreviousExternalID string
	NewExternalID      string
	// Records is how many rows the rename moved — all of them, in one statement.
	Records int64
	// Parked reports whether the legacy file was actually moved aside.
	Parked bool
}

// CIDNormalizeNotChanged records one legacy blob the sweep left alone.
type CIDNormalizeNotChanged struct {
	ExternalID string
	Reason     string
	Detail     string
}

// CIDNormalizeSummary is what one `sweep-cids` pass did. It is both the operator
// summary (FR-010) and the body of the run report (FR-016).
type CIDNormalizeSummary struct {
	Normalized int   // legacy blobs renamed
	Records    int64 // rows repointed across all of them
	Skipped    int   // deliberately left alone; never drives a failure verdict (FR-017)
	Failed     int   // genuinely could not be normalized — the only bucket that fails a run
	Parked     int   // legacy files moved aside; nothing is deleted
	Orphans    int   // legacy files no row referenced — parked, nothing renamed

	// WouldNormalize is the preview figure: legacy-named files found. Dry runs only.
	// There is deliberately no WouldSkip: the work-list comes from the store, so
	// every name in it is a file that exists, and a preview that reported a skip
	// count would be reporting a number it never measured.
	WouldNormalize int

	// Truncated means the pass hit its per-pass ceiling and legacy names remain.
	// Distinct from Aborted: nothing went wrong, there is simply more to do.
	Truncated bool
	// Aborted means the pass ENDED EARLY (page-scan error or shutdown) rather than
	// exhausting the corpus. Distinct from "completed with failures".
	Aborted bool
	// ReportFailed means the durable record could not be written. It gates the exit
	// code: a pass that destroyed blobs and then lost the mapping is not success.
	ReportFailed bool

	DryRun     bool
	Rate       float64
	StartedAt  time.Time
	FinishedAt time.Time

	Changed    []CIDNormalizeChange
	NotChanged []CIDNormalizeNotChanged
	// ParkedOrphans names the legacy files that no row referenced. They are parked
	// like any other, so `_parked/` holds them — and without this an operator
	// reconciling that directory finds files there with nothing saying why they
	// moved, on the one directory whose purpose is to be reversible.
	ParkedOrphans []string
}

// CIDNormalizeOptions configures one pass.
type CIDNormalizeOptions struct {
	// DryRun enumerates through the same predicate a real pass uses and returns
	// before any store or database write. FR-007 requires "no change of any kind",
	// so a dry run also writes no report and no journal.
	DryRun bool
	// Rate bounds blobs/second. Must be positive and finite — the caller rejects
	// anything else rather than reading it as "unlimited" (FR-015).
	Rate float64
	// Report receives the journal and the run report. Required for a real pass:
	// without it nothing could record what was destroyed.
	Report port.ReportSink
}

// RunCIDNormalize re-addresses legacy-named blobs under the digest of their bytes
// and repoints every row naming them (018-legacy-cid-normalization).
//
// The cohort is content written before content addressing: the name is an IPFS
// CID, so SHA3-256 of the bytes can never equal it and file-backup-service refuses
// them — unbackable, hence data-at-risk (file-service#63 bucket A).
//
// The unit of work is a BLOB, not a row:
//
//	digest := publish(bytes of AAA)                  // link; an existing blob dedups
//	n      := UPDATE file SET externalID=digest WHERE externalID=AAA
//	journal(AAA → digest)                            // durable BEFORE the unlink
//	unlink(AAA)                                      // nothing references it now
//
// One UPDATE moves every row sharing the blob, atomically, so afterwards nothing
// references the old name and reclamation is a plain unlink rather than a refcount
// protocol. `WHERE externalID = AAA` is also the entire concurrency guard: a row
// whose content was replaced concurrently already carries a different name and
// simply does not match, so the user's write survives untouched.
//
// Ordering makes it safe against live traffic with no coordination with the serving
// path: publish precedes the rename, so a row names the legacy blob (still present)
// before and the new blob (already created) after. At no instant does a row name a
// blob that is absent (FR-004). An interruption between any two steps is safe — a
// re-run re-publishes (idempotent on a content-addressed store), re-runs the
// UPDATE, and re-checks before unlinking.
//
// Resumable with no stored cursor: renaming a blob removes it from the predicate.
func (s *FileService) RunCIDNormalize(ctx context.Context, opts CIDNormalizeOptions) CIDNormalizeSummary {
	pace := newPacer(opts.Rate)
	// The rate ACTUALLY enforced, not the one requested: newPacer floors a rate low
	// enough to exceed the per-object cap, and recording the request would put a
	// throughput figure that was never in effect into the permanent audit trail.
	sum := CIDNormalizeSummary{DryRun: opts.DryRun, Rate: float64(pace.Limit()), StartedAt: time.Now().UTC()}
	run := &cidRun{opts: opts, journal: cidJournalName(sum.StartedAt)}

	// One walk of the store, up to a bounded number of matches. Legacy names are rare
	// in a large corpus, so paging would re-enumerate everything to find a handful.
	work, unaddressable, err := s.LegacyStore.ListLegacyNamed(cidNormalizeMaxPerPass)
	if err != nil { //nolint:nestif // the error path deliberately falls through to the report
		// A scan error is NOT an empty corpus: an unmounted volume must not read as a
		// fully-swept store.
		s.Logger.Error("sweep-cids: store scan failed; nothing was touched", zap.Error(err))
		sum.Aborted = true
		// Falls through to the report block rather than returning: README, CLAUDE.md
		// and the Job manifest all promise a real pass ALWAYS writes the .json, and an
		// aborted pass is the one an operator most needs an artifact for — the Job's
		// logs expire with its TTL, the report does not.
		work = nil
	}
	// Names the store itself cannot address. They are on disk, unbackable, and the
	// sweep can never fetch them — so they are counted and reported rather than
	// dropped, or a pass reports a converged corpus over content that is still there.
	for _, n := range unaddressable {
		s.skipCID(n, &sum, reasonUnaddressableName,
			"the store's key rules refuse this name, so its bytes cannot be fetched; it needs manual repair before any sweep can migrate it")
	}
	if len(work) == cidNormalizeMaxPerPass {
		// Truncated, not drained. Saying so matters: "no work left" is the operator's
		// terminating condition, and this is the opposite of that.
		sum.Truncated = true
		s.Logger.Warn("sweep-cids: hit the per-pass ceiling; MORE LEGACY NAMES REMAIN — re-run after this pass",
			zap.Int("ceiling", cidNormalizeMaxPerPass))
	}

	for _, legacy := range work {
		if ctx.Err() != nil {
			sum.Aborted = true
			break
		}
		if opts.DryRun {
			sum.WouldNormalize++
			continue
		}
		// Pacing applies only to real passes: a dry run does no per-blob store I/O
		// worth throttling, and pacing it would turn the preview an operator runs
		// before deciding into a long wait.
		if err := pace.Wait(ctx); err != nil {
			sum.Aborted = true
			break
		}
		s.normalizeOneCID(ctx, legacy, &sum, run)
	}

	sum.FinishedAt = time.Now().UTC()
	// Ordering, and all three matter:
	//   1. write the report — the only assigner of ReportFailed
	//   2. log the summary — so it can SEE ReportFailed
	//   3. log the report path last — the runbook tells the operator to look there
	if !opts.DryRun && opts.Report != nil {
		s.writeCIDNormalizeReport(&sum, run)
	} else if run.journalFailed {
		// writeCIDNormalizeReport is normally the sole assigner, but it is itself
		// sink-guarded — so with no sink a real pass would have exited 0 having
		// recorded nothing. The verdict must not depend on whether the thing that
		// reports it was configured.
		sum.ReportFailed = true
	}
	s.logCIDNormalizeSummary(sum)
	s.logCIDNormalizeReportPath(sum, run)
	return sum
}

// cidRun carries per-run state the journal needs, kept off the summary so the
// summary stays a pure result.
type cidRun struct {
	opts    CIDNormalizeOptions
	journal string
	// reportPath is where the summary report landed, logged as the run's last line.
	reportPath string
	// journalPath is where the journal landed, named in the error when the report
	// cannot be written — that is when an operator most needs to find it.
	journalPath string
	// journalFailed records that at least one mapping could not be made durable.
	journalFailed bool
}

// normalizeOneCID runs publish → rename → journal → reclaim for one legacy blob.
//
// A Go panic is contained per blob. Unlike sweep-dims — which contains panics
// because it decodes arbitrary bytes through a CGo image library — this pass
// decodes nothing, so a panic signals a defect and is counted a FAILURE. What the
// containment buys is that the pass keeps going: it unlinks blobs as it runs, and
// dying mid-corpus is how a half-migrated corpus is produced.
func (s *FileService) normalizeOneCID(ctx context.Context, legacy string, sum *CIDNormalizeSummary, run *cidRun) {
	defer func() {
		if rec := recover(); rec != nil {
			s.Logger.Error("sweep-cids: PANIC normalizing a blob; continuing with the rest of the pass",
				zap.String("externalID", legacy), zap.Any("panic", rec))
			s.failCID(legacy, sum, reasonWriteFailed, fmt.Sprintf("panic: %v", rec))
		}
	}()

	digest, ok := s.digestOf(legacy, sum)
	if !ok {
		return
	}

	// ALWAYS attempt the link, and let the FILESYSTEM answer whether these are one
	// file or two. A string compare cannot: `strings.EqualFold` says an uppercase-hex
	// legacy name and its lowercase digest are "the same file", which is true only on
	// a case-INSENSITIVE volume. On the case-sensitive volume this Job actually runs
	// on they are two distinct files, and skipping the link left every repointed row
	// naming a blob that was never created — a permanent 404 the pass reported as
	// clean, and which a second pass then turned into the only copy being parked.
	//
	// os.Link tells us the truth for free:
	//   created == true   → it made a second name, so they ARE two files
	//   created == false  → the target already resolved to existing content, either a
	//                       genuine dedup hit or the same file under a folded name
	created, err := s.LegacyStore.Link(legacy, digest)
	if err != nil {
		s.failCID(legacy, sum, reasonWriteFailed, "link: "+err.Error())
		return
	}
	// Ask the FILESYSTEM the question that actually matters: would parking this name
	// remove the content?
	//
	// It would, on a case-INSENSITIVE volume, where the legacy spelling and the digest
	// are one directory entry. It would NOT once this pass has hard-linked them, where
	// there are two. `os.SameFile` says "same file" to both, which is why the earlier
	// version left the legacy name unparked forever on any pass that was interrupted
	// after Link — the digest entry made every later run think it was an alias.
	//
	// Link count separates them, and it is only asked when Link found the target
	// already present: a link this pass just created always leaves two entries.
	sameFile := false
	if !created {
		var err error
		if sameFile, err = s.LegacyStore.ParkingWouldOrphan(legacy, digest); err != nil {
			s.failCID(legacy, sum, reasonReadFailed, "park-safety check: "+err.Error())
			return
		}
	}

	records, err := s.Repo.RenameExternalID(ctx, legacy, digest)
	if err != nil {
		if ctx.Err() != nil {
			// Shutdown is not a fault of this blob; booking it as a write failure
			// would write a false I/O error against healthy content into the only
			// durable audit trail of an irreversible migration.
			sum.Aborted = true
			s.Logger.Warn("sweep-cids: shutdown during a rename; the blob is untouched and remains in scope",
				zap.String("externalID", legacy))
			return
		}
		if errors.Is(err, model.ErrDuplicateKey) {
			s.failCID(legacy, sum, reasonWriteFailed,
				"rename: another record in this bucket already holds "+digest+"; needs manual reconciliation")
			return
		}
		// The link we made stays. It was unlinked here for one round, to avoid an
		// invisible orphan — that was the wrong trade. An errored UPDATE may have
		// COMMITTED server-side: pgx surfaces the error when the connection drops
		// after the commit, so the rows may already name this digest. Deleting it
		// then leaves every one of them pointing at nothing. Leaking a blob costs
		// disk; deleting a live one costs content.
		s.failCID(legacy, sum, reasonWriteFailed, "rename: "+err.Error())
		return
	}

	if records == 0 && sameFile {
		// Nothing names the legacy spelling, and on a case-insensitive volume this
		// file IS the digest — so rows naming the digest are pointing at THIS file.
		// Parking it would delete live content, which is the same data loss the
		// records > 0 path guards, reached by a different route: an uppercase-hex
		// name is in scope (the scan wants lowercase) while every row already carries
		// the lowercase form, so the rename matches nothing and control lands here.
		//
		// Nothing to do: the content is already at its correct address and no row
		// needs moving.
		s.Logger.Info("sweep-cids: name differs from its digest only by case and no row names it; the content is already correctly addressed",
			zap.String("externalID", legacy), zap.String("digest", digest))
		// Counted, so the buckets still account for every name the scan returned. A
		// branch that returns silently makes the report show all-zero counts for a
		// pass that enumerated work and left it in place.
		s.skipCID(legacy, sum, reasonAlreadyAddressed, "the file is its own digest modulo case; no row needs moving")
		return
	}
	if records == 0 {
		// Nothing referenced this blob. It is an orphan — from a deleted row, or from
		// a Replace that repointed its row and left this file behind — and parking it
		// is the whole job. But the link we just made must go: an orphan under a
		// DIGEST name is invisible to this sweep on any future run (it no longer
		// matches the scan) and indistinguishable from live content, so leaving one
		// converts a visible, sweepable problem into a permanent invisible one.
		s.unlinkUnreferencedPublish(ctx, digest, created)
		s.parkLegacy(legacy, "", 0, sum, run, true)
		return
	}

	sum.Normalized++
	sum.Records += records
	if sameFile {
		// The file is already at its content-addressed name; only the rows moved.
		sum.Changed = append(sum.Changed, CIDNormalizeChange{
			PreviousExternalID: legacy, NewExternalID: digest, Records: records,
		})
		return
	}
	s.parkLegacy(legacy, digest, records, sum, run, false)
}

// unlinkUnreferencedPublish removes a link this pass created for a blob that turned
// out to be referenced by nothing.
//
// Only a link WE created is removed: one that already existed may be the legitimate
// home of bytes other rows reference.
//
// There is deliberately NO re-check afterwards. A row can always appear after any
// check, so a second query would narrow the window without closing it, at the cost
// of code and a relink that can itself fail. What actually makes this safe is that
// the bytes are about to be parked, not destroyed: if an upload does dedup onto
// this digest in the window, restoring it is moving one file back out of _parked.
func (s *FileService) unlinkUnreferencedPublish(ctx context.Context, digest string, created bool) {
	if !created {
		return
	}
	// `records == 0` proves nothing names the LEGACY name. It says nothing about the
	// digest — and rows naming a digest whose blob was MISSING are the alkemio#1995
	// dangling cohort. Publishing the link healed them; unlinking it re-breaks them,
	// and their only remaining copy would be the parked legacy file.
	n, err := s.Repo.CountByExternalID(ctx, digest)
	if err != nil {
		// Cannot tell. Keeping it is the safe direction, but it leaves an orphan under
		// a digest name that no future scan can match — so it is a WARN naming the
		// blob, not an Info that reads like a routine decision.
		s.Logger.Warn("sweep-cids: could not count references for a published blob; keeping it, and it is now invisible to future passes",
			zap.String("externalID", digest), zap.Error(err))
		return
	}
	if n > 0 {
		s.Logger.Info("sweep-cids: keeping the published blob — rows name this digest",
			zap.String("externalID", digest), zap.Int("rows", n))
		return
	}
	if err := s.Storage.Delete(digest); err != nil {
		s.Logger.Warn("sweep-cids: could not remove a link published for an unreferenced blob",
			zap.String("externalID", digest), zap.Error(err))
	}
}

// parkLegacy records the mapping durably, then moves the legacy file aside.
//
// Parking rather than deleting is what makes this migration reversible: the bytes
// stay on the volume under a reserved directory, so a rename that turns out to be
// wrong is repaired by moving the file back, and an operator clears the directory
// only once the result is verified.
func (s *FileService) parkLegacy(legacy, digest string, records int64, sum *CIDNormalizeSummary, run *cidRun, orphan bool) {
	change := CIDNormalizeChange{PreviousExternalID: legacy, NewExternalID: digest, Records: records}
	if orphan {
		// An orphan has no old→new mapping to preserve — no row ever named it — but it
		// still must not move when nothing could record that the pass ran at all. The
		// no-sink guard lives in journalChange, so skipping the call entirely let a
		// real pass with no sink park the whole corpus and exit 0. Worse: point the Job
		// at the wrong database and EVERY blob takes this branch.
		if run.opts.Report == nil {
			run.journalFailed = true
			s.Logger.Error("sweep-cids: no report sink configured; refusing to park",
				zap.String("externalID", legacy))
			return
		}
	}
	if !orphan {
		// Durable BEFORE the file moves. An orphan needs no journal line: nothing
		// referenced it, so there is no mapping to reconstruct.
		if !s.journalChange(change, run) {
			s.Logger.Error("sweep-cids: keeping the legacy file in place because its mapping is not durable",
				zap.String("externalID", legacy))
			sum.Changed = append(sum.Changed, change)
			return
		}
	}
	path, err := s.LegacyStore.Park(legacy)
	if err != nil {
		s.Logger.Warn("sweep-cids: could not park the legacy file; it remains at its original name",
			zap.String("externalID", legacy), zap.Error(err))
	} else {
		change.Parked = true
		sum.Parked++
		s.Logger.Info("sweep-cids: parked", zap.String("externalID", legacy), zap.String("path", path))
	}
	if orphan {
		sum.Orphans++
		if change.Parked {
			// Only name it if it actually moved. Otherwise the report lists a file as
			// being in _parked/ that is not there — the inverse of the care taken for
			// change.Parked above, and it breaks the very reconciliation the parked
			// directory exists to support.
			sum.ParkedOrphans = append(sum.ParkedOrphans, legacy)
		}
		return
	}
	sum.Changed = append(sum.Changed, change)
}

// digestOf streams the legacy file and returns the SHA3-256 of its bytes.
//
// Read once, hash on the way through, never buffered and never written back: the
// target name is produced by a hard link, not a copy. The bytes are immutable — a
// Replace publishes a NEW digest and leaves this file untouched — so nothing can
// change under the read and no coordination is needed.
//
// The legacy name is deliberately NOT decoded or verified against the bytes: this
// content predates the current scheme with unknown write history, so the bytes are
// the only truth available and adopting their digest is correct by construction
// (FR-006, research R2).
func (s *FileService) digestOf(legacy string, sum *CIDNormalizeSummary) (digest string, ok bool) {
	rc, _, err := s.Storage.ReadStream(legacy)
	if err != nil {
		s.failCID(legacy, sum, reasonReadFailed, err.Error())
		return "", false
	}
	defer func() { _ = rc.Close() }()

	h := NewHasher()
	if _, err := io.Copy(h, rc); err != nil {
		s.failCID(legacy, sum, reasonReadFailed, "read: "+err.Error())
		return "", false
	}
	return h.Sum(), true
}

// journalChange makes one old→new mapping durable before the blob it describes is
// unlinked. Append-only NDJSON, fsynced per line.
func (s *FileService) journalChange(c CIDNormalizeChange, run *cidRun) bool {
	if run.opts.Report == nil {
		// A dry run never reaches here, so this is a real pass with no sink. Returning
		// true would ungate reclamation for every blob: the corpus is destroyed, no
		// journal and no report are written, and ReportFailed stays false, so the Job
		// exits 0 over a destroyed corpus with no audit trail.
		run.journalFailed = true
		s.Logger.Error("sweep-cids: no report sink configured for a real pass; refusing to reclaim, since no mapping can be made durable",
			zap.String("externalID", c.PreviousExternalID))
		return false
	}
	line, err := cidJournalLine(c)
	if err != nil {
		run.journalFailed = true
		s.Logger.Error("sweep-cids: could not encode a journal line; this mapping is NOT durable",
			zap.String("externalID", c.PreviousExternalID), zap.Error(err))
		return false
	}
	path, err := run.opts.Report.AppendJournal(run.journal, line)
	if err != nil {
		run.journalFailed = true
		s.Logger.Error("sweep-cids: could not append to the journal; this mapping is NOT durable and its blob was about to be reclaimed",
			zap.String("previousExternalID", c.PreviousExternalID),
			zap.String("newExternalID", c.NewExternalID),
			zap.Error(err))
		return false
	}
	// Kept, but in its OWN field: sharing reportPath made a failed report announce
	// "run report written" pointing at the journal, while discarding it left the
	// operator guessing a nanosecond-timestamped filename in a directory holding
	// every past run — for the one pass whose files are already parked.
	run.journalPath = path
	return true
}

func (s *FileService) skipCID(legacy string, sum *CIDNormalizeSummary, reason, detail string) {
	sum.Skipped++
	sum.NotChanged = append(sum.NotChanged, CIDNormalizeNotChanged{ExternalID: legacy, Reason: reason, Detail: detail})
	fields := []zap.Field{
		zap.String("externalID", legacy),
		zap.String("reason", reason),
		zap.String("detail", detail),
	}
	if reason == reasonUnaddressableName {
		s.Logger.Warn("sweep-cids: blob left unchanged — its name is unaddressable and needs manual repair", fields...)
		return
	}
	s.Logger.Info("sweep-cids: blob left unchanged", fields...)
}

func (s *FileService) failCID(legacy string, sum *CIDNormalizeSummary, reason, detail string) {
	sum.Failed++
	sum.NotChanged = append(sum.NotChanged, CIDNormalizeNotChanged{ExternalID: legacy, Reason: reason, Detail: detail})
	s.Logger.Error("sweep-cids: blob FAILED to normalize",
		zap.String("externalID", legacy),
		zap.String("reason", reason),
		zap.String("detail", detail))
}

// logCIDNormalizeSummary emits the counts an operator acts on (FR-010). A pass that
// ended early must never be logged as "completed", or a partial sweep reads as a
// drained corpus.
func (s *FileService) logCIDNormalizeSummary(sum CIDNormalizeSummary) {
	fields := []zap.Field{
		zap.Bool("dryRun", sum.DryRun),
		zap.Float64("rate", sum.Rate),
		zap.Int("normalized", sum.Normalized),
		zap.Int64("records", sum.Records),
		zap.Int("skipped", sum.Skipped),
		zap.Int("failed", sum.Failed),
		zap.Int("parked", sum.Parked),
		zap.Int("orphans", sum.Orphans),
		zap.Bool("reportFailed", sum.ReportFailed),
		zap.Bool("truncated", sum.Truncated),
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

// newPacer bounds throughput to a blobs/second ceiling. Each blob costs a read, a
// write and a database round-trip against SHARED PRODUCTION infrastructure, so an
// unbounded full-corpus pass would be a self-inflicted load test (FR-014, FR-015).
//
// Burst 1: a token bucket would permit a burst after any idle stretch, which is
// exactly the load shape this prevents.
func newPacer(blobsPerSecond float64) *rate.Limiter {
	// !(v > 0) rather than v <= 0 so NaN — for which every comparison is false —
	// lands here too; +Inf is excluded explicitly because it divides to a zero wait.
	if !(blobsPerSecond > 0) || math.IsInf(blobsPerSecond, 1) {
		blobsPerSecond = 1
	}
	// Floor the rate so no single blob can wait longer than maxPacerInterval. The
	// flag validation does not cover this: a perfectly valid `--rate 1e-5` means
	// 27h46m per blob and 1e-12 means 292 years, with no deadline on the Job's
	// context — the pod hangs having logged only "job starting".
	if floor := 1 / maxPacerInterval.Seconds(); blobsPerSecond < floor {
		blobsPerSecond = floor
	}
	return rate.NewLimiter(rate.Limit(blobsPerSecond), 1)
}
