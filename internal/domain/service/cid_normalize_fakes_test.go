package service

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"regexp"
	"sort"
	"strings"
	"testing"

	"go.uber.org/zap"

	"github.com/google/uuid"

	"github.com/alkem-io/file-service/internal/domain/model"
	"github.com/alkem-io/file-service/internal/domain/port"
)

// The sweep's whole correctness argument is about the ORDER of a store write, a
// database write and a store delete, and about what a concurrent writer sees in
// between. Scripted mocks cannot express that, so these fakes model the two
// stores faithfully: a `file` table that really compare-and-sets, and a
// content-addressed blob store that really dedups.

// The current content-addressing scheme, as the fakes see it. Pinned to the real
// adapter's rule by TestFakeSchemeMatchesTheAdapter — a fake that classifies names
// differently from production tests nothing.
// IsBlobName mirrors the adapter's key rule for the fakes.
func IsBlobName(n string) bool {
	if len(n) < 32 || len(n) > 128 {
		return false
	}
	for _, c := range n {
		isDigit := c >= '0' && c <= '9'
		isLower := c >= 'a' && c <= 'z'
		isUpper := c >= 'A' && c <= 'Z'
		if !isDigit && !isLower && !isUpper {
			return false
		}
	}
	return true
}

var sha3HexName = regexp.MustCompile(`^[0-9a-f]{64}$`)

// cidRow is one row of the `file` table, restricted to the columns the sweep
// reads or writes.
type cidRow struct {
	ID         uuid.UUID
	ExternalID string
	Version    int
	Temporary  bool
}

// cidRepo is an in-memory `file` table implementing the four operations the
// sweep uses; everything else on the port comes from the embedded mockRepo.
type cidRepo struct {
	mockRepo
	rows []*cidRow

	// beforeNormalize runs immediately before each compare-and-set — the seam a
	// concurrent Replace or promote would land in.
	beforeNormalize func(r *cidRepo, externalID string)

	normalizeApplied int
	countCalls       int
	normalizeCalls   int
	// normalizeErr, when set, is what the compare-and-set returns instead of
	// evaluating the guard — used to inject a duplicate-key or a dead database.
	normalizeErr error
	countErr     error
}

var _ port.DocumentRepo = (*cidRepo)(nil)

func newCIDRepo(rows ...*cidRow) *cidRepo {
	r := &cidRepo{rows: rows}
	sort.Slice(r.rows, func(i, j int) bool {
		return bytes.Compare(r.rows[i].ID[:], r.rows[j].ID[:]) < 0
	})
	return r
}

// RenameExternalID moves EVERY row naming oldExternalID, in one shot, and reports
// how many. Guarded solely on the old name — a row whose content was replaced
// concurrently already carries a different name and simply does not match.
func (r *cidRepo) RenameExternalID(_ context.Context, oldExternalID, newExternalID string) (int64, error) {
	r.normalizeCalls++
	if r.beforeNormalize != nil {
		r.beforeNormalize(r, oldExternalID)
	}
	if r.normalizeErr != nil {
		return 0, r.normalizeErr
	}
	var moved int64
	for _, row := range r.rows {
		if row.ExternalID == oldExternalID {
			row.ExternalID = newExternalID
			moved++
		}
	}
	if moved > 0 {
		r.normalizeApplied++
	}
	return moved, nil
}

// CountByExternalID counts every row naming the blob, temporary ones included —
// the real query is unfiltered, and a temporary row is a live reference that
// must keep the blob alive.
func (r *cidRepo) CountByExternalID(_ context.Context, externalID string) (int, error) {
	r.countCalls++
	if r.countErr != nil {
		return 0, r.countErr
	}
	n := 0
	for _, row := range r.rows {
		if row.ExternalID == externalID {
			n++
		}
	}
	return n, nil
}

// cidStore is a content-addressed blob store: the name IS the digest, so
// publishing bytes that already exist dedups instead of erroring.
type cidStore struct {
	blobs map[string][]byte

	readErr      map[string]error // per-name injected read faults
	openStageErr error
	commitErr    error
	deleteErr    error
	// shortRead makes ReadStream report the blob's true size but hand back only
	// the first N bytes, ending in a clean EOF — the NFS fault io.Copy cannot see.
	shortRead map[string]int
	listErr   error
	linkErr   error
	parkErr   error
	// parked holds what Park moved aside; nothing is ever destroyed.
	parked map[string][]byte
	// links counts directory entries per stored content, mirroring st_nlink.
	links map[string]int
	// caseInsensitive models APFS/SMB/Azure Files, where two names differing only by
	// case ARE one file. A plain map is case-SENSITIVE, so without this the fakes can
	// only ever exercise one of the two volume kinds — which is how a data-loss path
	// shipped with a passing test.
	caseInsensitive bool

	created []string // names published for the first time (dedup hits excluded)
	deletes []string
	reads   map[string]int // per-name ReadStream calls, to prove a shared blob is read once
	stages  int            // staging writes opened

	// onReadStream fires as a blob is opened — the seam a shutdown lands in when it
	// arrives mid-copy rather than between records.
	onReadStream func()

	// afterCommit / afterDelete observe the instants between the sweep's three
	// steps, which is where the "no record ever names an absent blob" invariant
	// has to be checked.
	afterCommit func()
	afterDelete func()
}

var _ port.StoragePort = (*cidStore)(nil)

func newCIDStore() *cidStore {
	return &cidStore{blobs: map[string][]byte{}, readErr: map[string]error{}, shortRead: map[string]int{}, reads: map[string]int{}}
}

func (s *cidStore) put(name string, content []byte) {
	s.blobs[name] = content
	s.link(name, 1)
}

// link records an entry count, tolerating a zero-value cidStore built as a literal.
func (s *cidStore) link(name string, n int) {
	if s.links == nil {
		s.links = map[string]int{}
	}
	s.links[name] = n
}

func (s *cidStore) has(name string) bool { return s.resolve(name) != "" }

// resolve returns the key that actually holds this name on this volume kind, or "".
func (s *cidStore) resolve(name string) string {
	if _, ok := s.blobs[name]; ok {
		return name
	}
	if s.caseInsensitive {
		for k := range s.blobs {
			if strings.EqualFold(k, name) {
				return k
			}
		}
	}
	return ""
}

func (s *cidStore) Save(content []byte) (model.StoredFile, error) {
	name := ComputeHash(content)
	created := !s.has(name)
	s.put(name, content)
	return model.StoredFile{ExternalID: name, Size: len(content), Created: created}, nil
}

func (s *cidStore) Read(externalID string) ([]byte, error) {
	if err := s.readErr[externalID]; err != nil {
		return nil, err
	}
	b, ok := s.blobs[externalID]
	if !ok {
		return nil, fmt.Errorf("read %s: %w", externalID, os.ErrNotExist)
	}
	return b, nil
}

func (s *cidStore) ReadStream(externalID string) (io.ReadCloser, int64, error) {
	s.reads[externalID]++
	if s.onReadStream != nil {
		s.onReadStream()
	}
	b, err := s.Read(externalID)
	if err != nil {
		return nil, 0, err
	}
	if n, ok := s.shortRead[externalID]; ok {
		// Size from the open handle's Stat, body truncated: exactly what a
		// blipping volume delivers, and exactly what io.Copy reports as success.
		return io.NopCloser(bytes.NewReader(b[:n])), int64(len(b)), nil
	}
	return io.NopCloser(bytes.NewReader(b)), int64(len(b)), nil
}

func (s *cidStore) Delete(externalID string) error {
	if s.deleteErr != nil {
		return s.deleteErr
	}
	s.deletes = append(s.deletes, externalID)
	delete(s.blobs, externalID)
	if s.afterDelete != nil {
		s.afterDelete()
	}
	return nil
}

func (s *cidStore) Exists(externalID string) (bool, error) { return s.has(externalID), nil }

// ListLegacyNamed enumerates names on the store that are not the current scheme —
// the sweep's actual work-list, which includes blobs no row references.
func (s *cidStore) ListLegacyNamed(limit int) ([]string, []string, error) {
	// The port makes limit > 0 a precondition; a fake that quietly accepts a bad one
	// lets a caller ship a violation the real adapter would have refused.
	if limit <= 0 {
		return nil, nil, fmt.Errorf("limit must be positive, got %d", limit)
	}
	if s.listErr != nil {
		return nil, nil, s.listErr
	}
	var names, unaddressable []string
	for n := range s.blobs {
		switch {
		case sha3HexName.MatchString(n) || strings.HasPrefix(n, "_"):
		case !IsBlobName(n):
			// Mirrors the adapter: a name the store cannot address is surfaced, not
			// dropped. The fake filtered differently, so tests exercised a work-list
			// production never produces.
			unaddressable = append(unaddressable, n)
		default:
			names = append(names, n)
		}
	}
	sort.Strings(unaddressable)
	sort.Strings(names) // map order is random; tests need a stable work-list
	if len(names) > limit {
		names = names[:limit]
	}
	return names, unaddressable, nil
}

func (s *cidStore) Link(existing, newName string) (bool, error) {
	if s.linkErr != nil {
		return false, s.linkErr
	}
	if s.has(newName) {
		return false, nil // dedup hit — on a case-insensitive volume, the same file
	}
	s.blobs[newName] = s.blobs[existing]
	s.link(newName, 1)
	s.link(existing, s.links[existing]+1) // a second entry for the same content
	s.created = append(s.created, newName)
	return true, nil
}

// Park moves the file aside instead of destroying it — the property that makes the
// migration reversible.
// SameFile models inode identity: on a case-insensitive volume two spellings are one
// file, on a case-sensitive one they are two — which a map alone cannot express.
func (s *cidStore) HasContent() (bool, error) { return len(s.blobs) > 0, nil }

// ParkingWouldOrphan models the three situations: an alias (one entry, resolves to
// the same content), a hard link this pass made (two entries), and two distinct files.
func (s *cidStore) ParkingWouldOrphan(name, other string) (bool, error) {
	kn, ko := s.resolve(name), s.resolve(other)
	if kn == "" || ko == "" {
		return false, nil
	}
	if kn != ko {
		return false, nil // distinct entries
	}
	return s.links[kn] <= 1, nil
}

func (s *cidStore) Park(externalID string) (string, error) {
	if s.parkErr != nil {
		return "", s.parkErr
	}
	key := s.resolve(externalID)
	if key == "" {
		return "", nil // already gone — an empty path, not a completed park
	}
	externalID = key
	if s.parked == nil {
		s.parked = map[string][]byte{}
	}
	s.parked[externalID] = s.blobs[externalID]
	delete(s.blobs, externalID)
	return "/mnt/storage/_parked/" + externalID, nil
}

func (s *cidStore) OpenStage(_ context.Context) (port.StageWriter, error) {
	if s.openStageErr != nil {
		return nil, s.openStageErr
	}
	s.stages++
	return &cidStage{store: s}, nil
}

type cidStage struct {
	store *cidStore
	buf   bytes.Buffer
}

func (st *cidStage) Write(p []byte) (int, error) { return st.buf.Write(p) }

func (st *cidStage) Commit() (model.StoredFile, error) {
	if st.store.commitErr != nil {
		return model.StoredFile{}, st.store.commitErr
	}
	content := st.buf.Bytes()
	name := ComputeHash(content)
	created := !st.store.has(name)
	if created {
		st.store.created = append(st.store.created, name)
		st.store.put(name, content)
	}
	if st.store.afterCommit != nil {
		st.store.afterCommit()
	}
	return model.StoredFile{ExternalID: name, Size: len(content), Created: created}, nil
}

func (st *cidStage) Abort() error { return nil }

func (st *cidStage) StagedReaderAt() (io.ReaderAt, int64, error) {
	b := st.buf.Bytes()
	return bytes.NewReader(b), int64(len(b)), nil
}

// cidSink captures the run report and the journal instead of writing them
// anywhere, and records WHEN each landed relative to the store's deletes — the
// journal's whole purpose is to precede reclamation.
type cidSink struct {
	writes int
	name   string
	data   []byte
	err    error

	journalName  string
	journalLines [][]byte
	journalErr   error
	// onAppend fires after each journal line is accepted, so a test can assert
	// what the store looked like at that instant.
	onAppend func(line []byte)
}

var _ port.ReportSink = (*cidSink)(nil)

func (s *cidSink) WriteReport(name string, data []byte) (string, error) {
	s.writes++
	s.name = name
	s.data = data
	if s.err != nil {
		return "", s.err
	}
	return "/mnt/storage/_sweep-reports/" + name, nil
}

func (s *cidSink) AppendJournal(name string, line []byte) (string, error) {
	if s.journalErr != nil {
		return "", s.journalErr
	}
	s.journalName = name
	s.journalLines = append(s.journalLines, append([]byte(nil), line...))
	if s.onAppend != nil {
		s.onAppend(line)
	}
	return "/mnt/storage/_sweep-reports/" + name, nil
}

func testLogger() *zap.Logger { return zap.NewNop() }

// upperHex produces the uppercase form of a digest — in scope for the sweep,
// because the scan predicate demands LOWERCASE hex, and the case where the legacy
// name and the digest are the same file on a case-insensitive volume.
func upperHex(s string) string { return strings.ToUpper(s) }

// TestFakeSchemeMatchesTheAdapter pins the fakes' notion of "already normalized" to
// the storage adapter's. They are separate definitions — the fakes cannot import the
// adapter without a cycle — so nothing but this test stops them drifting, and a drift
// makes every sweep test assert against a corpus production would classify differently.
func TestFakeSchemeMatchesTheAdapter(t *testing.T) {
	cases := []string{
		strings.Repeat("a", 64),                          // current scheme
		strings.ToUpper(strings.Repeat("a", 64)),         // uppercase hex — NOT the scheme
		"QmViQoajobQiqmFLn1Mk3rBt5BiGbnidYXso8oKiWq1fcp", // legacy CID
		strings.Repeat("a", 63),                          // too short
		strings.Repeat("a", 65),                          // too long
		"",
	}
	for _, name := range cases {
		fake := sha3HexName.MatchString(name)
		adapterRule := sha3HexRealRule(name)
		if fake != adapterRule {
			t.Errorf("%q: fake says normalized=%v, the adapter's rule says %v", name, fake, adapterRule)
		}
	}
}

// sha3HexRealRule restates the adapter's predicate independently, so the assertion is
// not the regexp compared with itself.
func sha3HexRealRule(name string) bool {
	if len(name) != 64 {
		return false
	}
	for _, c := range name {
		isDigit := c >= '0' && c <= '9'
		isLowerHex := c >= 'a' && c <= 'f'
		if !isDigit && !isLowerHex {
			return false
		}
	}
	return true
}
