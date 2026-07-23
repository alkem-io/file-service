package service

import (
	"bytes"
	"context"
	"errors"
	"io"
	"runtime"
	"sync"
	"testing"

	"github.com/google/uuid"
	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"

	"github.com/alkem-io/file-service/internal/domain/model"
)

// Spec 020 T010 — streaming ingest pipeline.

// discardingStage extension: budget tests must not buffer the stream in the
// mock itself.
func discardingStorage() *mockStorage {
	return &mockStorage{stageDiscard: true}
}

// patternReader yields size deterministic bytes without allocating them.
type patternReader struct {
	size int64
	off  int64
}

func (p *patternReader) Read(b []byte) (int, error) {
	if p.off >= p.size {
		return 0, io.EOF
	}
	n := int64(len(b))
	if rem := p.size - p.off; rem < n {
		n = rem
	}
	for i := int64(0); i < n; i++ {
		b[i] = byte((p.off + i) & 0x7F) //nolint:gosec // bounded by mask
	}
	p.off += n
	return int(n), nil
}

// failAfterReader delivers n bytes then fails with err (transport faults).
type failAfterReader struct {
	n   int
	err error
}

func (f *failAfterReader) Read(b []byte) (int, error) {
	if f.n <= 0 {
		return 0, f.err
	}
	n := len(b)
	if n > f.n {
		n = f.n
	}
	for i := 0; i < n; i++ {
		b[i] = 'x'
	}
	f.n -= n
	return n, nil
}

func newIngestService(storage *mockStorage, repo *mockRepo) *FileService {
	return &FileService{Logger: nopLogger, Repo: repo, Storage: storage, Processor: &mockProcessor{}}
}

// (a) Pass-through equivalence: bytes, hash, size and dedup outcome match
// the buffered implementation.
func TestStageUpload_PassThroughEquivalence(t *testing.T) {
	content := bytes.Repeat([]byte("equivalence!"), 1<<18) // 3 MiB
	storage := &mockStorage{}
	repo := &mockRepo{}
	svc := newIngestService(storage, repo)

	su, err := svc.StageUpload(context.Background(), bytes.NewReader(content), "")
	if err != nil {
		t.Fatal(err)
	}
	input := model.CreateDocumentInput{DisplayName: "f.bin", StorageBucketID: uuid.New(), AuthorizationID: uuid.New()}
	doc, err := svc.CompleteUpload(context.Background(), su, input, nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(storage.saved, content) {
		t.Error("staged bytes differ from input")
	}
	if doc.ExternalID != ComputeHash(content) {
		t.Errorf("externalID = %s, want ComputeHash", doc.ExternalID)
	}
	if doc.Size != len(content) {
		t.Errorf("size = %d, want %d", doc.Size, len(content))
	}
	if su.MimeType != "application/octet-stream" {
		t.Errorf("mime = %s", su.MimeType)
	}
}

// (b) Over-limit mid-stream: transport sentinel propagates, stage aborted,
// never committed.
func TestStageUpload_OverLimitAborts(t *testing.T) {
	storage := &mockStorage{}
	svc := newIngestService(storage, &mockRepo{})

	_, err := svc.StageUpload(context.Background(), &failAfterReader{n: 8192, err: ErrOverLimit}, "")
	if !errors.Is(err, ErrOverLimit) {
		t.Fatalf("err = %v, want ErrOverLimit", err)
	}
	assertSingleAbortedStage(t, storage)
}

// (c) Client abort mid-stream: reader error propagates, stage aborted.
func TestStageUpload_ClientAbortAborts(t *testing.T) {
	storage := &mockStorage{}
	svc := newIngestService(storage, &mockRepo{})

	_, err := svc.StageUpload(context.Background(), &failAfterReader{n: 8192, err: io.ErrUnexpectedEOF}, "")
	if !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Fatalf("err = %v, want ErrUnexpectedEOF", err)
	}
	assertSingleAbortedStage(t, storage)
}

// (d) Trailing bucket-policy violation (fields after the file part,
// research R4): rejection + abort, no publish.
func TestCompleteUpload_TrailingPolicyViolationAborts(t *testing.T) {
	storage := &mockStorage{}
	svc := newIngestService(storage, &mockRepo{})
	input := model.CreateDocumentInput{DisplayName: "f.bin", StorageBucketID: uuid.New(), AuthorizationID: uuid.New()}

	su, err := svc.StageUpload(context.Background(), bytes.NewReader([]byte("plain text body")), "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := svc.CompleteUpload(context.Background(), su, input, []string{"image/png"}, 0); !errors.Is(err, ErrUnsupportedMediaType) {
		t.Fatalf("err = %v, want ErrUnsupportedMediaType", err)
	}
	assertSingleAbortedStage(t, storage)

	su2, err := svc.StageUpload(context.Background(), bytes.NewReader(bytes.Repeat([]byte("z"), 2048)), "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := svc.CompleteUpload(context.Background(), su2, input, nil, 1024); !errors.Is(err, ErrPayloadTooLarge) {
		t.Fatalf("err = %v, want ErrPayloadTooLarge", err)
	}
	if st := storage.stages[1]; !st.aborted || st.committed {
		t.Error("over-bucket-limit stage not aborted")
	}
}

// (e) Dedup at end-of-stream: Created=false → existing row reused.
func TestCompleteUpload_DedupAtEnd(t *testing.T) {
	existing := model.Document{ID: uuid.New(), ExternalID: "whatever"}
	storage := &mockStorage{stageDedupHit: true}
	repo := &mockRepo{findDoc: &existing}
	svc := newIngestService(storage, repo)
	input := model.CreateDocumentInput{DisplayName: "dup.bin", StorageBucketID: uuid.New(), AuthorizationID: uuid.New()}

	su, err := svc.StageUpload(context.Background(), bytes.NewReader([]byte("dup content")), "")
	if err != nil {
		t.Fatal(err)
	}
	doc, err := svc.CompleteUpload(context.Background(), su, input, nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	if !doc.Reused || doc.ID != existing.ID {
		t.Errorf("doc = %+v, want reuse of existing row", doc)
	}
}

// (f) FR-001/SC-001 allocation bound: a 64 MiB pass-through allocates
// O(budget), not O(size).
func TestStageUpload_AllocationBound(t *testing.T) {
	storage := discardingStorage()
	svc := newIngestService(storage, &mockRepo{})

	runtime.GC()
	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	su, err := svc.StageUpload(context.Background(), &patternReader{size: 64 << 20}, "")
	runtime.ReadMemStats(&after)
	if err != nil {
		t.Fatal(err)
	}
	if su.Size != 64<<20 {
		t.Fatalf("size = %d", su.Size)
	}
	allocated := after.TotalAlloc - before.TotalAlloc
	const bound = 4 << 20 // budget (256 KiB + 3 KiB) with generous slack
	if allocated > bound {
		t.Errorf("allocated %d bytes for a 64 MiB stream; budget bound %d (O(size) buffering suspected)", allocated, bound)
	}
	su.Discard()
}

// (g) SC-005: 8 concurrent ingests stay within 8 × budget aggregate.
func TestStageUpload_ConcurrentAllocationBound(t *testing.T) {
	const workers = 8
	storages := make([]*mockStorage, workers)
	services := make([]*FileService, workers)
	for i := range storages {
		storages[i] = discardingStorage()
		services[i] = newIngestService(storages[i], &mockRepo{})
	}

	runtime.GC()
	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	var wg sync.WaitGroup
	errs := make([]error, workers)
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			su, err := services[i].StageUpload(context.Background(), &patternReader{size: 32 << 20}, "")
			if err == nil {
				su.Discard()
			}
			errs[i] = err
		}(i)
	}
	wg.Wait()
	runtime.ReadMemStats(&after)
	for i, err := range errs {
		if err != nil {
			t.Fatalf("worker %d: %v", i, err)
		}
	}
	allocated := after.TotalAlloc - before.TotalAlloc
	const bound = workers * (4 << 20) // 8 × per-stream slack bound
	if allocated > bound {
		t.Errorf("allocated %d bytes for %d concurrent 32 MiB streams; bound %d", allocated, workers, bound)
	}
}

// (h) FR-008: exactly one structured log per terminal outcome; transport
// faults distinguishable from service faults by field.
func TestIngest_OutcomeLogging(t *testing.T) {
	cases := []struct {
		name          string
		run           func(svc *FileService)
		wantMsg       string
		wantTransport *bool // nil = field absent (accepted)
	}{
		{
			name: "accepted",
			run: func(svc *FileService) {
				su, _ := svc.StageUpload(context.Background(), bytes.NewReader([]byte("ok")), "")
				input := model.CreateDocumentInput{DisplayName: "a", StorageBucketID: uuid.New(), AuthorizationID: uuid.New()}
				_, _ = svc.CompleteUpload(context.Background(), su, input, nil, 0)
			},
			wantMsg: "ingest accepted",
		},
		{
			name: "over-limit is transport",
			run: func(svc *FileService) {
				_, _ = svc.StageUpload(context.Background(), &failAfterReader{n: 8192, err: ErrOverLimit}, "")
			},
			wantMsg:       "ingest: stream copy failed",
			wantTransport: boolp(true),
		},
		{
			name: "client abort is service-distinguishable",
			run: func(svc *FileService) {
				_, _ = svc.StageUpload(context.Background(), &failAfterReader{n: 8192, err: io.ErrUnexpectedEOF}, "")
			},
			wantMsg:       "ingest: stream copy failed",
			wantTransport: boolp(false),
		},
		{
			name: "stage write failure is service-side",
			run: func(svc *FileService) {
				svc.Storage.(*mockStorage).stageWriteErr = errors.New("disk gone")
				_, _ = svc.StageUpload(context.Background(), bytes.NewReader(bytes.Repeat([]byte("y"), 8192)), "")
			},
			wantMsg:       "ingest: stream copy failed",
			wantTransport: boolp(false),
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			core, logs := observer.New(zap.InfoLevel)
			svc := newIngestService(&mockStorage{}, &mockRepo{})
			svc.Logger = zap.New(core)
			c.run(svc)

			matched := logs.FilterMessage(c.wantMsg).All()
			if len(matched) != 1 {
				t.Fatalf("logs for %q = %d, want exactly 1 (all: %v)", c.wantMsg, len(matched), logs.All())
			}
			if c.wantTransport != nil {
				fields := matched[0].ContextMap()
				got, ok := fields["transport"].(bool)
				if !ok || got != *c.wantTransport {
					t.Errorf("transport field = %v (ok=%v), want %v", got, ok, *c.wantTransport)
				}
			}
		})
	}
}

func boolp(b bool) *bool { return &b }

func assertSingleAbortedStage(t *testing.T, storage *mockStorage) {
	t.Helper()
	if len(storage.stages) != 1 {
		t.Fatalf("stages = %d, want 1", len(storage.stages))
	}
	if st := storage.stages[0]; !st.aborted || st.committed {
		t.Errorf("stage state aborted=%v committed=%v, want aborted only", st.aborted, st.committed)
	}
}

// US3: replace-path transport guards — over-limit propagates, stage aborted,
// document row untouched.
func TestStoreAndLinkStream_OverLimitAborts(t *testing.T) {
	storage := &mockStorage{}
	repo := &mockRepo{doc: model.Document{ID: uuid.New(), MimeType: "application/pdf", ExternalID: "old"}}
	svc := newIngestService(storage, repo)

	_, err := svc.StoreAndLinkStream(context.Background(), uuid.New(), &failAfterReader{n: 8192, err: ErrOverLimit})
	if !errors.Is(err, ErrOverLimit) {
		t.Fatalf("err = %v, want ErrOverLimit", err)
	}
	assertSingleAbortedStage(t, storage)
	if repo.updateFileCalls != 0 {
		t.Error("document row mutated for an over-limit replace")
	}
}

// GIF/SVG/BMP/AVIF remain byte-identical pass-through, but their headers are measured from the
// completed stage so new rows do not depend on a later read or one-shot legacy sweep. Non-images
// skip both the transcoder and dimension measurement.
func TestStageUpload_PassthroughFormatsSkipTranscoder(t *testing.T) {
	w, h := 80, 60
	for _, tc := range []struct {
		mime          string
		wantMeasured  bool
		wantDimsCalls int
	}{
		{"image/gif", true, 1},
		{"image/svg+xml", true, 1},
		{"application/pdf", false, 0},
		{"image/bmp", true, 1},
		{"image/avif", true, 1},
	} {
		proc := &mockProcessor{detectMIME: tc.mime, measureDimsW: &w, measureDimsH: &h}
		svc := &FileService{Logger: nopLogger, Repo: &mockRepo{}, Storage: &mockStorage{}, Processor: proc}
		su, err := svc.StageUpload(context.Background(), bytes.NewReader([]byte("some bytes")), "")
		if err != nil {
			t.Fatalf("%s: %v", tc.mime, err)
		}
		su.Discard()
		if proc.transcodeCalls != 0 {
			t.Errorf("%s routed through the transcoder; want passthrough", tc.mime)
		}
		if su.Measured != tc.wantMeasured || proc.measureDimsCalls != tc.wantDimsCalls {
			t.Errorf("%s: measured=%v dimsCalls=%d, want measured=%v dimsCalls=%d",
				tc.mime, su.Measured, proc.measureDimsCalls, tc.wantMeasured, tc.wantDimsCalls)
		}
	}
	// control: a transcodable type does route
	proc := &mockProcessor{detectMIME: "image/jpeg"}
	svc := &FileService{Logger: nopLogger, Repo: &mockRepo{}, Storage: &mockStorage{}, Processor: proc}
	su, err := svc.StageUpload(context.Background(), bytes.NewReader([]byte("jpegish")), "")
	if err != nil {
		t.Fatal(err)
	}
	su.Discard()
	if proc.transcodeCalls != 1 {
		t.Errorf("jpeg transcodeCalls = %d, want 1", proc.transcodeCalls)
	}
}

func TestStageUpload_PassthroughMeasurementOutcomes(t *testing.T) {
	t.Run("decoder-failure-records-verdict", func(t *testing.T) {
		proc := &mockProcessor{detectMIME: "image/gif", measureDimsErr: errors.New("corrupt")}
		repo := &mockRepo{}
		svc := &FileService{Logger: nopLogger, Repo: repo, Storage: &mockStorage{}, Processor: proc}
		su, err := svc.StageUpload(context.Background(), bytes.NewReader([]byte("gif bytes")), "")
		if err != nil {
			t.Fatal(err)
		}
		input := model.CreateDocumentInput{
			DisplayName: "bad.gif", StorageBucketID: uuid.New(), AuthorizationID: uuid.New(),
		}
		if _, err := svc.CompleteUpload(context.Background(), su, input, nil, 0); err != nil {
			t.Fatal(err)
		}
		if !repo.lastCreateContentMetadata.Populated || !repo.lastCreateContentMetadata.DecodeFailed {
			t.Fatalf("metadata = %+v, want persisted decode-failed verdict", repo.lastCreateContentMetadata)
		}
	})

	t.Run("no-decoder-leaves-for-sweep", func(t *testing.T) {
		proc := &mockProcessor{detectMIME: "image/gif"}
		svc := &FileService{Logger: nopLogger, Repo: &mockRepo{}, Storage: &mockStorage{}, Processor: proc}
		su, err := svc.StageUpload(context.Background(), bytes.NewReader([]byte("gif bytes")), "")
		if err != nil {
			t.Fatal(err)
		}
		defer su.Discard()
		if su.Measured || proc.measureDimsCalls != 1 {
			t.Fatalf("measured=%v dimsCalls=%d, want unresolved after one no-decoder attempt",
				su.Measured, proc.measureDimsCalls)
		}
	})

	t.Run("stage-read-failure-does-not-fail-upload", func(t *testing.T) {
		proc := &mockProcessor{detectMIME: "image/gif"}
		storage := &mockStorage{stageReaderErr: errors.New("staging read unavailable")}
		svc := &FileService{Logger: nopLogger, Repo: &mockRepo{}, Storage: storage, Processor: proc}
		su, err := svc.StageUpload(context.Background(), bytes.NewReader([]byte("gif bytes")), "")
		if err != nil {
			t.Fatal(err)
		}
		defer su.Discard()
		if su.Measured || proc.measureDimsCalls != 0 {
			t.Fatalf("measured=%v dimsCalls=%d, want unresolved without decoder call",
				su.Measured, proc.measureDimsCalls)
		}
	})

	t.Run("stage-short-read-is-not-a-decode-verdict", func(t *testing.T) {
		proc := &mockProcessor{detectMIME: "image/gif", measureDimsErr: errors.New("unexpected EOF")}
		storage := &mockStorage{stageReaderSize: 999}
		svc := &FileService{Logger: nopLogger, Repo: &mockRepo{}, Storage: storage, Processor: proc}
		su, err := svc.StageUpload(context.Background(), bytes.NewReader([]byte("short gif")), "")
		if err != nil {
			t.Fatal(err)
		}
		defer su.Discard()
		if su.Measured || proc.measureDimsCalls != 1 {
			t.Fatalf("measured=%v dimsCalls=%d, want retryable short-read after one decoder call",
				su.Measured, proc.measureDimsCalls)
		}
	})
}
