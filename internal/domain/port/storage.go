package port

import (
	"context"
	"errors"
	"io"

	"github.com/alkem-io/file-service/internal/domain/model"
)

// ErrInvalidKey is returned when an externalID fails the storage key rules
// (path-traversal guard / length bound). Distinct from a genuinely absent blob,
// so a handler can answer 400 rather than 404/500.
var ErrInvalidKey = errors.New("invalid storage key")

// StoragePort abstracts the file storage backend (local filesystem, S3, etc.).
// Storage is content-addressed: externalID is the hash of the blob's bytes.
type StoragePort interface {
	// Save stores a complete in-memory buffer and returns its content
	// identity. Saving bytes that already exist is not an error: the
	// existing blob is reused and StoredFile.Created reports false.
	Save(content []byte) (model.StoredFile, error)
	// Read returns the whole blob. Error contract: ErrInvalidKey (malformed id),
	// os.ErrNotExist (blob absent). A non-ENOENT backend failure is returned as-is
	// (the handler answers 500). Read does NOT distinguish an absent blob from a
	// store-wide outage — an empty store root is ambiguous; the caller that knows
	// what should exist makes that call (the backup worker cross-checks the corpus).
	Read(externalID string) ([]byte, error)
	// ReadStream opens the blob for streaming (constant memory — the caller does
	// not hold the whole blob resident), returning the content, its size, and a
	// closer the caller MUST close. Same error contract as Read. Prefer this for
	// bulk/high-concurrency readers so N concurrent fetches don't cost N×blobsize.
	ReadStream(externalID string) (io.ReadCloser, int64, error)
	// Delete removes the blob. Idempotent: deleting an id that does not
	// exist returns nil, so refcount-driven cleanup can race harmlessly.
	Delete(externalID string) error
	// Exists reports whether a blob is present without reading its bytes.
	// (false, nil) is a definitive "not there"; a non-nil error means the
	// backend could not answer.
	Exists(externalID string) (bool, error)
	// OpenStage begins a streaming ingestion into not-yet-published storage
	// (spec 020). Nothing is observable as a permanent object until Commit.
	OpenStage(ctx context.Context) (StageWriter, error)
}

// StageWriter is a per-upload staging artifact. It hashes internally while
// bytes are written; Commit finalizes the content identity and performs the
// backend-specific publish (FS: no-overwrite hard link, with O_EXCL-copy
// fallback; an object store would complete a multipart upload and copy to
// the content-addressed key — the contract assumes no rename primitive).
type StageWriter interface {
	io.Writer
	// Commit publishes the staged content under its content hash and
	// returns the stored identity. Created=false signals a dedup hit (the
	// blob already existed); the staging artifact is destroyed either way.
	Commit() (model.StoredFile, error)
	// Abort destroys the staging artifact. Idempotent; safe after a failed
	// Commit and after a successful one (no-op then).
	Abort() error
	// StagedReaderAt exposes the bytes written so far for random-access
	// inspection before Commit (e.g. reading a zip central directory). Valid
	// only after writes complete and before Commit/Abort. The local FS backend
	// returns the staging temp file; an object-store backend would satisfy this
	// with ranged reads of the staged object (zip.NewReader uses ReadAt on the
	// tail, so ranged GETs suffice).
	StagedReaderAt() (io.ReaderAt, int64, error)
}

// LegacyBlobStore is the one-off migration surface: everything `sweep-cids` needs and
// nothing else does.
//
// It is a SEPARATE port for the same reason ReportSink is. These seven methods have
// exactly one caller, and putting them on StoragePort made every serving-path consumer
// depend on them and every future backend implement them — for a migration that
// converges and goes away. SameFile is a POSIX inode concept with no object-store
// analogue and Park hardcodes a sidecar-directory model, so an S3 backend would be
// writing stubs to satisfy an interface it has no business satisfying.
//
// local.Adapter satisfies both, so the wiring costs nothing.
type LegacyBlobStore interface {
	// ListLegacyNamed walks the blob namespace ONCE and returns the names that are
	// not the current content-addressing scheme, up to limit. Reserved sidecar
	// directories are never included.
	//
	// One pass, not paged: legacy names are rare in a large store, so re-walking the
	// directory per page would cost a full enumeration to find a handful of files.
	// Implementations MUST stream the directory rather than materializing it, so
	// memory scales with the number of MATCHES, not with the corpus.
	//
	// The store, not the database, is the authority on what needs migrating: a
	// database scan sees only names some row references, so an UNREFERENCED legacy
	// blob would sit on the volume forever and "zero legacy-named blobs remain"
	// could never be satisfied.
	//
	// limit MUST be > 0. It is the only bound on what the implementation retains, so
	// a non-positive value would mean "keep everything" — the unbounded growth this
	// signature exists to prevent. Implementations MUST reject it rather than walk.
	//
	// Returning fewer than limit means the walk completed. Returning exactly limit
	// means it was truncated and a further run has more to do — callers MUST NOT
	// read that as a drained corpus.
	// Returns `unaddressable` alongside: names present on the store that the key
	// rules refuse, so they can never be fetched or migrated. They MUST NOT be
	// silently dropped — they are unbackable data-at-risk, exactly the population the
	// sweep exists for, and omitting them lets a pass report a converged corpus while
	// they sit on disk.
	ListLegacyNamed(limit int) (names, unaddressable []string, err error)
	// Link publishes existing bytes under a second name, without copying them.
	// Returns false if the target already exists — which is not an error on a
	// content-addressed store, just a dedup hit.
	Link(existing, newName string) (created bool, err error)
	// HasContent reports whether the store is usable and holds at least one blob.
	//
	// It is the sweep's pre-flight: an unmounted volume makes every read look like
	// "this content is permanently gone", so a pass would touch nothing, exit 0, and
	// write a report branding the whole corpus unrecoverable. The store answers it
	// because only the store knows what a blob looks like — asking in cmd meant a
	// second copy of that rule, which could drift into a pre-flight that passes on
	// entries the scan ignores.
	HasContent() (bool, error)
	// ParkingWouldOrphan reports whether removing `name` would leave `other`
	// unresolvable — the only question the sweep actually has before it parks a
	// legacy name, and one neither spelling nor inode identity answers alone.
	//
	// Three situations, all reachable, that must not be conflated:
	//
	//   - case-INSENSITIVE volume: the two spellings are ONE directory entry. Removing
	//     it removes the content. → true
	//   - this pass hard-linked them: same inode, TWO entries. → false
	//   - two genuinely distinct files with identical bytes: different inodes. → false
	//
	// `os.SameFile` alone says "same" to the first two; link count alone says "sole
	// entry" to the first and third. Only both together separate all three.
	ParkingWouldOrphan(name, other string) (bool, error)
	// Park moves a blob out of the content namespace into a reserved sidecar
	// directory, returning where it went. It is the non-destructive alternative to
	// Delete for a migration: the bytes remain on the volume, so a rename that
	// turns out to be wrong is recoverable by moving the file back, and an operator
	// clears the parked directory only once the result has been verified.
	//
	// Parking a name that is already absent returns ("", nil) — an EMPTY PATH with a
	// nil error, so a re-run after an interruption is safe. Callers MUST treat the
	// empty path as "nothing moved" rather than as a completed park, or they will
	// report bytes as recoverable that are not in the parked directory.
	Park(externalID string) (string, error)
}
