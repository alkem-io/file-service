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

	su, err := svc.StageUpload(context.Background(), bytes.NewReader(content), "", false)
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

	_, err := svc.StageUpload(context.Background(), &failAfterReader{n: 8192, err: ErrOverLimit}, "", false)
	if !errors.Is(err, ErrOverLimit) {
		t.Fatalf("err = %v, want ErrOverLimit", err)
	}
	assertSingleAbortedStage(t, storage)
}

// (c) Client abort mid-stream: reader error propagates, stage aborted.
func TestStageUpload_ClientAbortAborts(t *testing.T) {
	storage := &mockStorage{}
	svc := newIngestService(storage, &mockRepo{})

	_, err := svc.StageUpload(context.Background(), &failAfterReader{n: 8192, err: io.ErrUnexpectedEOF}, "", false)
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

	su, err := svc.StageUpload(context.Background(), bytes.NewReader([]byte("plain text body")), "", false)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := svc.CompleteUpload(context.Background(), su, input, []string{"image/png"}, 0); !errors.Is(err, ErrUnsupportedMediaType) {
		t.Fatalf("err = %v, want ErrUnsupportedMediaType", err)
	}
	assertSingleAbortedStage(t, storage)

	su2, err := svc.StageUpload(context.Background(), bytes.NewReader(bytes.Repeat([]byte("z"), 2048)), "", false)
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

	su, err := svc.StageUpload(context.Background(), bytes.NewReader([]byte("dup content")), "", false)
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
	su, err := svc.StageUpload(context.Background(), &patternReader{size: 64 << 20}, "", false)
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
			su, err := services[i].StageUpload(context.Background(), &patternReader{size: 32 << 20}, "", false)
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
				su, _ := svc.StageUpload(context.Background(), bytes.NewReader([]byte("ok")), "", false)
				input := model.CreateDocumentInput{DisplayName: "a", StorageBucketID: uuid.New(), AuthorizationID: uuid.New()}
				_, _ = svc.CompleteUpload(context.Background(), su, input, nil, 0)
			},
			wantMsg: "ingest accepted",
		},
		{
			name: "over-limit is transport",
			run: func(svc *FileService) {
				_, _ = svc.StageUpload(context.Background(), &failAfterReader{n: 8192, err: ErrOverLimit}, "", false)
			},
			wantMsg:       "ingest: stream copy failed",
			wantTransport: boolp(true),
		},
		{
			name: "client abort is service-distinguishable",
			run: func(svc *FileService) {
				_, _ = svc.StageUpload(context.Background(), &failAfterReader{n: 8192, err: io.ErrUnexpectedEOF}, "", false)
			},
			wantMsg:       "ingest: stream copy failed",
			wantTransport: boolp(false),
		},
		{
			name: "stage write failure is service-side",
			run: func(svc *FileService) {
				svc.Storage.(*mockStorage).stageWriteErr = errors.New("disk gone")
				_, _ = svc.StageUpload(context.Background(), bytes.NewReader(bytes.Repeat([]byte("y"), 8192)), "", false)
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

// GIF/SVG (and non-images) never route through the transcoder — passthrough
// with lazily backfilled dims (US2 scenario 2 as clarified).
func TestStageUpload_PassthroughFormatsSkipTranscoder(t *testing.T) {
	for _, mime := range []string{"image/gif", "image/svg+xml", "application/pdf", "image/bmp", "image/avif"} {
		proc := &mockProcessor{detectMIME: mime}
		svc := &FileService{Logger: nopLogger, Repo: &mockRepo{}, Storage: &mockStorage{}, Processor: proc}
		su, err := svc.StageUpload(context.Background(), bytes.NewReader([]byte("some bytes")), "", false)
		if err != nil {
			t.Fatalf("%s: %v", mime, err)
		}
		su.Discard()
		if proc.transcodeCalls != 0 {
			t.Errorf("%s routed through the transcoder; want passthrough", mime)
		}
	}
	// control: a transcodable type does route
	proc := &mockProcessor{detectMIME: "image/jpeg"}
	svc := &FileService{Logger: nopLogger, Repo: &mockRepo{}, Storage: &mockStorage{}, Processor: proc}
	su, _ := svc.StageUpload(context.Background(), bytes.NewReader([]byte("jpegish")), "", false)
	su.Discard()
	if proc.transcodeCalls != 1 {
		t.Errorf("jpeg transcodeCalls = %d, want 1", proc.transcodeCalls)
	}
}

// T007 (013): a skipImageProcessing=true upload of a transcodable image type
// (HEIC/PNG/JPEG) is stored byte-identical — no transcode, no MIME
// canonicalization, no dimension measure. Synapse's read-back must be exact.
func TestStageUpload_SkipImageProcessing_StoresVerbatim(t *testing.T) {
	// Bytes a transcode WOULD rewrite. transcodeMIME proves the transcoder, if
	// invoked, would canonicalize image/png → image/jpeg; the verbatim arm must
	// keep both the bytes and the MIME type unchanged.
	raw := append([]byte("\x89PNG\r\n\x1a\n"), bytes.Repeat([]byte("verbatim-heic-or-png-payload"), 256)...)

	t.Run("verbatim when skip=true", func(t *testing.T) {
		storage := &mockStorage{}
		proc := &mockProcessor{detectMIME: "image/png", transcodeMIME: "image/jpeg", processDimsW: intpService(640), processDimsH: intpService(480)}
		svc := &FileService{Logger: nopLogger, Repo: &mockRepo{}, Storage: storage, Processor: proc}

		su, err := svc.StageUpload(context.Background(), bytes.NewReader(raw), "image/png", true)
		if err != nil {
			t.Fatalf("StageUpload: %v", err)
		}
		if _, err := su.stage.Commit(); err != nil {
			t.Fatalf("commit: %v", err)
		}
		if proc.transcodeCalls != 0 {
			t.Errorf("transcodeCalls = %d, want 0 (verbatim must not transcode)", proc.transcodeCalls)
		}
		if !bytes.Equal(storage.saved, raw) {
			t.Errorf("stored bytes are not byte-identical to the upload")
		}
		if su.MimeType != "image/png" {
			t.Errorf("MimeType = %q, want image/png (no canonicalization on verbatim)", su.MimeType)
		}
		if su.Measured {
			t.Error("Measured = true, want false (no dimension measure on verbatim)")
		}
		if su.ImageWidth != nil || su.ImageHeight != nil {
			t.Error("dims populated on verbatim store; want nil")
		}
	})

	t.Run("transcodes when skip=false", func(t *testing.T) {
		storage := &mockStorage{}
		proc := &mockProcessor{detectMIME: "image/png", transcodeMIME: "image/jpeg", processDimsW: intpService(640), processDimsH: intpService(480)}
		svc := &FileService{Logger: nopLogger, Repo: &mockRepo{}, Storage: storage, Processor: proc}

		su, err := svc.StageUpload(context.Background(), bytes.NewReader(raw), "image/png", false)
		if err != nil {
			t.Fatalf("StageUpload: %v", err)
		}
		su.Discard()
		if proc.transcodeCalls != 1 {
			t.Errorf("transcodeCalls = %d, want 1 (normal path transcodes)", proc.transcodeCalls)
		}
		if su.MimeType != "image/jpeg" {
			t.Errorf("MimeType = %q, want image/jpeg (canonicalized)", su.MimeType)
		}
	})
}
