package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/alkem-io/file-service/internal/adapter/outbound/storage/local"

	"github.com/jackc/pgx/v5/pgxpool"
	natsserver "github.com/nats-io/nats-server/v2/server"
	"go.uber.org/zap"

	"github.com/alkem-io/file-service/internal/config"
	"github.com/alkem-io/file-service/internal/domain/service"
)

func TestConnectDatabase_Success(t *testing.T) {
	cfg := config.DatabaseConfig{
		Host:     "localhost",
		Port:     5432,
		Username: "synapse",
		Password: "synapse", //nolint:gosec // test credentials
		Name:     "alkemio",
	}

	pool, err := connectDatabase(context.Background(), cfg)
	if err != nil {
		t.Skipf("DB not available: %v", err)
	}
	defer pool.Close()

	if pool == nil {
		t.Fatal("nil pool")
	}
}

func TestConnectDatabase_BadHost(t *testing.T) {
	cfg := config.DatabaseConfig{
		Host:     "127.0.0.1",
		Port:     1, // nothing here
		Username: "x",
		Password: "x",
		Name:     "x",
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	_, err := connectDatabase(ctx, cfg)
	if err == nil {
		t.Fatal("expected error for bad host")
	}
}

func TestConnectNATS_Success(t *testing.T) {
	srv := startTestNATSServer(t)
	defer srv.Shutdown()

	logger, _ := zap.NewDevelopment()
	cfg := config.NATSConfig{
		URL:                srv.ClientURL(),
		ReconnectWaitMS:    100,
		ReconnectMaxWaitMS: 1000,
	}

	nc, err := connectNATS(cfg, logger)
	if err != nil {
		t.Fatalf("connectNATS: %v", err)
	}
	defer nc.Close()

	if !nc.IsConnected() {
		t.Error("not connected")
	}
}

func TestConnectNATS_BadURL(t *testing.T) {
	logger, _ := zap.NewDevelopment()
	cfg := config.NATSConfig{
		URL:                "nats://127.0.0.1:1",
		ReconnectWaitMS:    100,
		ReconnectMaxWaitMS: 200,
	}

	_, err := connectNATS(cfg, logger)
	if err == nil {
		t.Fatal("expected error")
	}
}

func TestBuildFileService(t *testing.T) {
	// Just verify it doesn't panic with nil-safe paths
	// We can't pass real connections but can verify the function signature works
	cfg := &config.Config{
		StoragePath: t.TempDir(),
		NATS:        config.NATSConfig{Subject: "auth.evaluate"},
	}

	// Use a real pool just to construct (won't be called)
	pool, _ := pgxpool.New(context.Background(), "postgres://invalid:5432/x")
	if pool == nil {
		t.Skip("cannot create pool stub")
	}
	defer pool.Close()

	logger, _ := zap.NewDevelopment()
	svc := buildFileService(pool, nil, cfg, logger)
	if svc.Repo == nil {
		t.Error("nil repo")
	}
	if svc.Storage == nil {
		t.Error("nil storage")
	}
	if svc.Processor == nil {
		t.Error("nil processor")
	}
}

func TestNewHTTPServer(t *testing.T) {
	handler := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})

	srv := newHTTPServer(0, handler)
	// Ordinary bodies keep the fixed cap; upload handlers replace it with
	// a rolling per-read deadline.
	if srv.ReadTimeout != 30*time.Second {
		t.Errorf("ReadTimeout = %v, want 30s", srv.ReadTimeout)
	}
	if srv.ReadHeaderTimeout != 10*time.Second {
		t.Errorf("ReadHeaderTimeout = %v, want 10s", srv.ReadHeaderTimeout)
	}
	// A global WriteTimeout is absolute from request headers and would
	// kill healthy long transfers. The router applies a lazy rolling idle cap.
	if srv.WriteTimeout != 0 {
		t.Errorf("WriteTimeout = %v, want 0", srv.WriteTimeout)
	}
	if srv.IdleTimeout != 120*time.Second {
		t.Errorf("IdleTimeout = %v", srv.IdleTimeout)
	}
}

func TestShutdownServer(_ *testing.T) {
	handler := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})

	srv := httptest.NewServer(handler)
	logger, _ := zap.NewDevelopment()

	// Shutdown should not panic
	shutdownServer(srv.Config, logger)
}

func startTestNATSServer(t *testing.T) *natsserver.Server {
	t.Helper()
	opts := &natsserver.Options{
		Host:   "127.0.0.1",
		Port:   -1,
		NoLog:  true,
		NoSigs: true,
	}
	srv, err := natsserver.NewServer(opts)
	if err != nil {
		t.Fatal(err)
	}
	srv.Start()
	if !srv.ReadyForConnections(2 * time.Second) {
		t.Fatal("NATS not ready")
	}
	return srv
}

// TestRun_UnknownCommand: a typo'd subcommand must fail loudly (exit 2), never fall through to
// serve or silently succeed.
func TestRun_UnknownCommand(t *testing.T) {
	if code := run([]string{"bogus-cmd"}); code != 2 {
		t.Fatalf("run(unknown) = %d, want 2", code)
	}
}

// TestDimsJobExitCode: the k8s Job retry contract — exit 1 ONLY on an ENDED-EARLY (Aborted) pass.
// Skips never gate the exit code (an all-skipped pass is ambiguous — a permanent orphan tail vs a
// transient outage — and a heuristic would either loop forever on the tail or miss a mid-run outage;
// the operator reads the logged counts). Matches file-backup-service's backfillVerdict all-skipped ruling.
func TestDimsJobExitCode(t *testing.T) {
	cases := []struct {
		name string
		sum  service.DimsBackfillSummary
		want int
	}{
		{"empty-drained-corpus", service.DimsBackfillSummary{}, 0},
		{"progress-with-some-skips", service.DimsBackfillSummary{Measured: 5, Skipped: 3, DecodeFailed: 1}, 0},
		{"all-skipped-ambiguous-not-a-failure", service.DimsBackfillSummary{Skipped: 100}, 0},
		{"convergent-orphan-tail", service.DimsBackfillSummary{Skipped: 5}, 0},
		{"aborted-after-progress", service.DimsBackfillSummary{Measured: 2, Aborted: true}, 1},
		// The load-bearing case: a total outage that fails the FIRST page scan yields Aborted with
		// zero of everything — Aborted alone (NOT coupled with progress) must gate exit 1, or a
		// mid/total outage on page one would silently exit 0.
		{"aborted-total-first-page-outage", service.DimsBackfillSummary{Aborted: true}, 1},
	}
	for _, c := range cases {
		if got := dimsJobExitCode(c.sum); got != c.want {
			t.Errorf("%s: exit = %d, want %d", c.name, got, c.want)
		}
	}
}

// The verdict rule the sweep-cids Job's retry behaviour hangs on (FR-017,
// SC-010). The load-bearing case is a pass whose only untouched records are the
// permanently-absent cohort: it MUST exit 0, or the operator's rerun loop has no
// terminating condition — nothing this system can do will ever restore those
// bytes, so they will skip on every future run.
func TestCIDsJobExitCode(t *testing.T) {
	cases := []struct {
		name string
		sum  service.CIDNormalizeSummary
		want int
	}{
		{"clean converged pass", service.CIDNormalizeSummary{Normalized: 1053, Parked: 1053}, 0},
		{"nothing left to do", service.CIDNormalizeSummary{}, 0},
		{"normalized plus permanently-absent content", service.CIDNormalizeSummary{Normalized: 900, Skipped: 153}, 0},
		{"only absent content left", service.CIDNormalizeSummary{Skipped: 153}, 0},
		{"lost races only", service.CIDNormalizeSummary{Normalized: 10, Skipped: 3}, 0},
		{"a genuine failure", service.CIDNormalizeSummary{Normalized: 900, Failed: 1}, 1},
		{"ended early after progress", service.CIDNormalizeSummary{Normalized: 500, Aborted: true}, 1},
		// A total outage fails the FIRST page scan: Aborted with zero of everything.
		// Aborted alone must gate exit 1, or that silently exits 0.
		{"ended early on page one", service.CIDNormalizeSummary{Aborted: true}, 1},
		{"dry run is subject to the same rule", service.CIDNormalizeSummary{DryRun: true, WouldNormalize: 1053}, 0},
		// A pass that migrated cleanly and then lost its audit trail is NOT success:
		// re-running cannot recover the mapping, and the Job would otherwise be marked
		// Complete with no alert.
		{"report could not be written", service.CIDNormalizeSummary{Normalized: 1053, ReportFailed: true}, 1},
		{"report lost with nothing else wrong", service.CIDNormalizeSummary{ReportFailed: true}, 1},
	}
	for _, c := range cases {
		if got := cidsJobExitCode(c.sum); got != c.want {
			t.Errorf("%s: exit = %d, want %d", c.name, got, c.want)
		}
	}
}

// `--rate` bounds load on shared production infrastructure, so a non-positive
// value is rejected rather than read as "unlimited". This is checked before any
// database connection, which is what makes it testable here.
func TestSweepCIDsRejectsNonPositiveRate(t *testing.T) {
	for _, arg := range []string{"0", "-1", "-0.5"} {
		if got := runSweepCIDs([]string{"--rate", arg}); got != 2 {
			t.Errorf("--rate %s: exit = %d, want 2", arg, got)
		}
	}
}

// Go's flag parser STOPS at the first non-flag operand, so a stray word makes
// every flag after it a positional argument that is silently ignored — turning a
// requested preview into the real, irreversible pass.
func TestSweepCIDsRejectsPositionalArguments(t *testing.T) {
	cases := [][]string{
		{"prod", "--dry-run"},  // a stray word in a Job's args:
		{"--", "--dry-run"},    // one separator too many in `kubectl create job ... --`
		{"--dry-run", "extra"}, // trailing junk
	}
	for _, args := range cases {
		if got := runSweepCIDs(args); got != 2 {
			t.Errorf("runSweepCIDs(%q) = %d, want 2 — these parse cleanly with --dry-run DROPPED", args, got)
		}
	}
}

// strconv.ParseFloat accepts "NaN" and "Inf". Both slip past a `<= 0` test — NaN
// because every comparison against it is false — and both collapse the pacing
// interval to zero AND make the run report unencodable, so the load ceiling is
// defeated and the only durable record of an irreversible pass is lost.
func TestSweepCIDsRejectsNonFiniteRate(t *testing.T) {
	for _, arg := range []string{"NaN", "Inf", "+Inf", "-Inf", "inf"} {
		if got := runSweepCIDs([]string{"--rate", arg}); got != 2 {
			t.Errorf("--rate %s: exit = %d, want 2", arg, got)
		}
	}
}

// An unmounted PVC or a misconfigured LOCAL_STORAGE_PATH makes every read ENOENT,
// which the sweep correctly classifies as "content permanently absent". Without
// this check the pass would exit 0 and write a report branding the entire corpus
// unrecoverable — the operator's documented terminating condition, reached by a
// pass that touched nothing.
func TestVerifySweepStorage(t *testing.T) {
	// A REAL blob name. The earlier fixture used a file literally called "blob",
	// which asserted the looseness this guard exists to remove.
	populated := t.TempDir()
	if err := os.WriteFile(filepath.Join(populated, strings.Repeat("a", 64)), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	legacyNamed := t.TempDir() // the cohort being migrated is not 64-hex
	if err := os.WriteFile(filepath.Join(legacyNamed, "QmViQoajobQiqmFLn1Mk3rBt5BiGbnidYXso8oKiWq1fcp"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	// Litter, not content: infra-ops' storage-checkup job writes exactly these into
	// the volume root and only removes them in its last step, so an evicted pod
	// leaves them behind. They must not make an empty store look populated.
	litter := t.TempDir()
	for _, n := range []string{"files_list.txt", "table_entries.txt", "removed_files.log"} {
		if err := os.WriteFile(filepath.Join(litter, n), []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	notADir := filepath.Join(t.TempDir(), "file")
	if err := os.WriteFile(notADir, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}

	cases := []struct {
		name    string
		path    string
		wantErr bool
	}{
		{"a populated content store", populated, false},
		{"a store holding only legacy-named blobs", legacyNamed, false},
		{"a store holding only another job's leftover temp files", litter, true},
		{"unset", "", true},
		{"missing (unmounted volume, or the relative dev default)", filepath.Join(t.TempDir(), "nope"), true},
		{"empty (indistinguishable from a corpus whose content is all gone)", t.TempDir(), true},
		{"not a directory", notADir, true},
	}
	for _, c := range cases {
		err := verifySweepStorage(local.New(c.path), c.path)
		if (err != nil) != c.wantErr {
			t.Errorf("%s: err = %v, wantErr = %v", c.name, err, c.wantErr)
		}
	}
}

// Asking a command for its usage is not a failure. flag.ContinueOnError returns
// ErrHelp for -h/--help, which is trivially mapped to the same exit code as a
// malformed invocation unless it is handled.
func TestSweepCIDsHelpExitsZero(t *testing.T) {
	for _, arg := range []string{"-h", "--help"} {
		if got := runSweepCIDs([]string{arg}); got != 0 {
			t.Errorf("runSweepCIDs(%q) = %d, want 0", arg, got)
		}
	}
}

// The rate override is parsed before anything connects, which is what makes the
// whole validation table testable without a database.
func TestParseSweepRate(t *testing.T) {
	cases := []struct {
		raw       string
		wantValue *float64
		wantErr   bool
	}{
		{raw: "", wantValue: nil},              // unset → the built-in default applies
		{raw: "5", wantValue: floatPtr(5)},     //
		{raw: "0.5", wantValue: floatPtr(0.5)}, //
		{raw: "0", wantErr: true},              // never "unlimited"
		{raw: "-1", wantErr: true},             //
		{raw: "NaN", wantErr: true},            // every comparison against NaN is false
		{raw: "Inf", wantErr: true},            //
		{raw: "-Inf", wantErr: true},           //
		{raw: "abc", wantErr: true},            // says so precisely rather than silently defaulting
		{raw: " 5", wantErr: true},             //
	}
	for _, c := range cases {
		got, err := parseSweepRate(c.raw)
		if (err != nil) != c.wantErr {
			t.Errorf("parseSweepRate(%q) err = %v, wantErr %v", c.raw, err, c.wantErr)
			continue
		}
		switch {
		case c.wantValue == nil && got != nil:
			t.Errorf("parseSweepRate(%q) = %v, want nil (unset)", c.raw, *got)
		case c.wantValue != nil && got == nil:
			t.Errorf("parseSweepRate(%q) = nil, want %v", c.raw, *c.wantValue)
		case c.wantValue != nil && got != nil && *got != *c.wantValue:
			t.Errorf("parseSweepRate(%q) = %v, want %v", c.raw, *got, *c.wantValue)
		}
	}
}

func floatPtr(v float64) *float64 { return &v }

// The pre-flight answers "is the volume mounted and holding content". It must not
// be satisfied by the sink's own reserved directory or a staging temp file — a
// store that has lost every blob would otherwise pass on the strength of a
// directory a previous run created, and every read would then be classified as
// permanently-absent content on a pass that exits 0.
func TestVerifySweepStorage_IgnoresNonBlobEntries(t *testing.T) {
	onlyReports := t.TempDir()
	if err := os.MkdirAll(filepath.Join(onlyReports, local.ReservedReportDir), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(onlyReports, ".stage-123"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := verifySweepStorage(local.New(onlyReports), onlyReports); err == nil {
		t.Error("a store holding only a reports directory and a staging temp passed the pre-flight — every read would then look like permanently-absent content")
	}

	withBlob := t.TempDir()
	if err := os.MkdirAll(filepath.Join(withBlob, local.ReservedReportDir), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(withBlob, strings.Repeat("a", 64)), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := verifySweepStorage(local.New(withBlob), withBlob); err != nil {
		t.Errorf("a store holding a real blob failed the pre-flight: %v", err)
	}
}

// Silently swallowing arguments is how a flag meant to change behaviour — a
// --dry-run typed against the wrong subcommand — goes unnoticed until the change
// is irreversible.
func TestTopLevelDispatch(t *testing.T) {
	cases := []struct {
		args []string
		want int
	}{
		{[]string{"-h"}, 0},
		{[]string{"--help"}, 0},
		{[]string{"help"}, 0},
		{[]string{"bogus"}, 2},
		{[]string{"serve", "--dry-run"}, 2}, // must not be swallowed
		{[]string{"sweep-dims", "--dry-run"}, 2},
	}
	for _, c := range cases {
		if got := run(c.args); got != c.want {
			t.Errorf("run(%q) = %d, want %d", c.args, got, c.want)
		}
	}
}

// The bare invocation — no arguments at all — is the documented default AND what
// the Dockerfile's argument-less ENTRYPOINT produces on every serving pod. It was
// missing from the table above, and `args[1:]` on the empty slice panicked with
// "slice bounds out of range [1:0]" before any config was loaded: the whole fleet
// would have CrashLoopBackOff'd on deploy while the test suite stayed green.
//
// This asserts only that dispatch REACHES serve — the config load then fails in a
// unit-test environment, which is fine. Any panic fails the test.
func TestTopLevelDispatchBareInvocationReachesServe(t *testing.T) {
	// Guarantee the config load fails, so dispatch is all this exercises. Without
	// this, a machine with a complete environment reaches runServe — which starts a
	// DB-WRITING MIME repair and then blocks on :4003 until the test times out.
	t.Setenv("AUTH_SERVICE_URL", "")
	t.Setenv("NATS_URL", "")

	for _, args := range [][]string{nil, {}, os.Args[1:][:0]} {
		func() {
			defer func() {
				if rec := recover(); rec != nil {
					t.Fatalf("run(%#v) panicked: %v", args, rec)
				}
			}()
			if got := run(args); got == 2 {
				t.Errorf("run(%#v) = 2 — a bare invocation must dispatch to serve, not be rejected as a usage error", args)
			}
		}()
	}
}
