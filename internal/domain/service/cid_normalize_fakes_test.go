package service

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"regexp"
	"sort"

	"github.com/google/uuid"

	"github.com/alkem-io/file-service/internal/domain/model"
	"github.com/alkem-io/file-service/internal/domain/port"
)

// The sweep's whole correctness argument is about the ORDER of a store write, a
// database write and a store delete, and about what a concurrent writer sees in
// between. Scripted mocks cannot express that, so these fakes model the two
// stores faithfully: a `file` table that really compare-and-sets, and a
// content-addressed blob store that really dedups.

var sha3HexName = regexp.MustCompile(`^[0-9a-f]{64}$`)

// cidRow is one row of the `file` table, restricted to the columns the sweep
// reads or writes.
type cidRow struct {
	ID         uuid.UUID
	ExternalID string
	Version    int
	Temporary  bool
}

// cidUUID makes ascending, comparable ids so keyset paging is deterministic.
// Numbering starts at 1: id 0 is uuid.Nil, the "start of corpus" cursor, and a
// row carrying it could never satisfy `id > $1`.
func cidUUID(n byte) uuid.UUID {
	var u uuid.UUID
	u[15] = n
	return u
}

// cidRepo is an in-memory `file` table implementing the four operations the
// sweep uses; everything else on the port comes from the embedded mockRepo.
type cidRepo struct {
	mockRepo
	rows []*cidRow

	listCalls int
	// listErrOn fails the Nth (1-based) page scan, to exercise the ended-early path.
	listErrOn int
	// beforeNormalize runs immediately before each compare-and-set — the seam a
	// concurrent Replace or promote would land in.
	beforeNormalize func(r *cidRepo, id uuid.UUID)
	// afterNormalize runs immediately after a compare-and-set applied.
	afterNormalize func(r *cidRepo, id uuid.UUID)

	normalizeApplied int
	countCalls       int
}

var _ port.DocumentRepo = (*cidRepo)(nil)

func newCIDRepo(rows ...*cidRow) *cidRepo {
	r := &cidRepo{rows: rows}
	sort.Slice(r.rows, func(i, j int) bool {
		return bytes.Compare(r.rows[i].ID[:], r.rows[j].ID[:]) < 0
	})
	return r
}

func (r *cidRepo) row(id uuid.UUID) *cidRow {
	for _, row := range r.rows {
		if row.ID == id {
			return row
		}
	}
	return nil
}

// ListLegacyNamed applies the same predicate as the SQL: permanent rows whose
// externalID is not a SHA3-256 hex name, keyset-paged by id.
func (r *cidRepo) ListLegacyNamed(_ context.Context, after uuid.UUID, limit int32) ([]model.Document, error) {
	r.listCalls++
	if r.listErrOn != 0 && r.listCalls == r.listErrOn {
		return nil, errors.New("page scan failed")
	}
	var page []model.Document
	for _, row := range r.rows {
		if bytes.Compare(row.ID[:], after[:]) <= 0 {
			continue
		}
		if row.Temporary || sha3HexName.MatchString(row.ExternalID) {
			continue
		}
		page = append(page, model.Document{ID: row.ID, ExternalID: row.ExternalID, Version: row.Version})
		if len(page) == int(limit) {
			break
		}
	}
	return page, nil
}

// NormalizeExternalID is a real compare-and-set: it applies only while the row
// still carries the externalID and version the sweep read.
func (r *cidRepo) NormalizeExternalID(_ context.Context, id uuid.UUID, expectedExternalID string, expectedVersion int, newExternalID string) (bool, error) {
	r.normalizeCalls++
	r.lastNormalize = normalizeCall{ID: id, Expected: expectedExternalID, Version: expectedVersion, New: newExternalID}
	if r.beforeNormalize != nil {
		r.beforeNormalize(r, id)
	}
	if r.normalizeErr != nil {
		return false, r.normalizeErr
	}
	row := r.row(id)
	if row == nil || row.ExternalID != expectedExternalID || row.Version != expectedVersion {
		return false, nil
	}
	row.ExternalID = newExternalID
	row.Version++
	r.normalizeApplied++
	if r.afterNormalize != nil {
		r.afterNormalize(r, id)
	}
	return true, nil
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

	created []string // names published for the first time (dedup hits excluded)
	deletes []string

	// afterCommit / afterDelete observe the instants between the sweep's three
	// steps, which is where the "no record ever names an absent blob" invariant
	// has to be checked.
	afterCommit func()
	afterDelete func()
}

var _ port.StoragePort = (*cidStore)(nil)

func newCIDStore() *cidStore {
	return &cidStore{blobs: map[string][]byte{}, readErr: map[string]error{}, shortRead: map[string]int{}}
}

func (s *cidStore) put(name string, content []byte) { s.blobs[name] = content }

func (s *cidStore) has(name string) bool {
	_, ok := s.blobs[name]
	return ok
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

func (s *cidStore) OpenStage(_ context.Context) (port.StageWriter, error) {
	if s.openStageErr != nil {
		return nil, s.openStageErr
	}
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
