// Package main runs the removable legacy-CID migration executable.
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/signal"
	"syscall"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/alkem-io/file-service/internal/adapter/outbound/alkemiodb"
	"github.com/alkem-io/file-service/internal/adapter/outbound/storage/local"
	"github.com/alkem-io/file-service/internal/config"
	"github.com/alkem-io/file-service/internal/domain/port"
	"github.com/alkem-io/file-service/internal/domain/service"
)

type sweepExecutor func(context.Context) (port.CIDSweepResult, error)

func run(ctx context.Context, args []string) int {
	return runWith(ctx, args, os.Stdout, os.Stderr, executeSweep)
}

func runWith(ctx context.Context, args []string, stdout, stderr io.Writer, execute sweepExecutor) int {
	if len(args) != 0 {
		_, _ = fmt.Fprintf(stderr, "sweep-ipfs-cids: unexpected arguments: %v\n", args)
		return 2
	}
	result, runErr := execute(ctx)
	if runErr != nil {
		result.Aborted = true
		if err := writeJSON(stdout, struct {
			Event  string `json:"event"`
			Reason string `json:"reason"`
		}{Event: "cid_sweep_abort", Reason: runErr.Error()}); err != nil {
			_, _ = fmt.Fprintf(stderr, "sweep-ipfs-cids: write abort: %v\n", err)
			return 1
		}
	}
	for _, failure := range result.Failures {
		if err := writeJSON(stdout, struct {
			Event  string `json:"event"`
			CID    string `json:"cid"`
			Reason string `json:"reason"`
		}{Event: "cid_sweep_failure", CID: failure.CID, Reason: failure.Reason}); err != nil {
			_, _ = fmt.Fprintf(stderr, "sweep-ipfs-cids: write failure: %v\n", err)
			return 1
		}
	}
	summary := struct {
		Event                   string `json:"event"`
		Complete                bool   `json:"complete"`
		CIDReferencesFound      int64  `json:"cid_references_found"`
		CaseReferencesFound     int64  `json:"case_variant_references_found"`
		ReferencesUpdated       int64  `json:"references_updated"`
		DistinctCIDSources      int64  `json:"distinct_cid_sources"`
		MigratedSourceBlobs     int64  `json:"migrated_source_blobs"`
		ConsolidatedSourceBlobs int64  `json:"consolidated_source_blobs"`
		FailedSourceBlobs       int64  `json:"failed_source_blobs"`
		Aborted                 bool   `json:"aborted"`
	}{
		Event:                   "cid_sweep_summary",
		Complete:                result.Complete(),
		CIDReferencesFound:      result.CIDReferencesFound,
		CaseReferencesFound:     result.CaseVariantReferencesFound,
		ReferencesUpdated:       result.ReferencesUpdated,
		DistinctCIDSources:      result.DistinctCIDSources,
		MigratedSourceBlobs:     result.MigratedSourceBlobs,
		ConsolidatedSourceBlobs: result.ConsolidatedSourceBlobs,
		FailedSourceBlobs:       result.FailedSourceBlobs,
		Aborted:                 result.Aborted,
	}
	if err := writeJSON(stdout, summary); err != nil {
		_, _ = fmt.Fprintf(stderr, "sweep-ipfs-cids: write summary: %v\n", err)
		return 1
	}
	if runErr != nil || !result.Complete() {
		return 1
	}
	return 0
}

func writeJSON(writer io.Writer, value any) error {
	encoder := json.NewEncoder(writer)
	encoder.SetEscapeHTML(false)
	return encoder.Encode(value)
}

func executeSweep(ctx context.Context) (port.CIDSweepResult, error) {
	logger, err := config.NewLogger()
	if err != nil {
		return port.CIDSweepResult{Aborted: true}, fmt.Errorf("initialize logger: %w", err)
	}
	defer func() { _ = logger.Sync() }()

	cfg, err := config.LoadForSweep()
	if err != nil {
		return port.CIDSweepResult{Aborted: true}, fmt.Errorf("load config: %w", err)
	}
	if cfg.StorageType != "local" {
		return port.CIDSweepResult{Aborted: true}, fmt.Errorf("unsupported STORAGE_TYPE %q: CID sweep requires local storage", cfg.StorageType)
	}
	pool, err := pgxpool.New(ctx, cfg.AlkemioDB.ConnString())
	if err != nil {
		return port.CIDSweepResult{Aborted: true}, fmt.Errorf("connect database: %w", err)
	}
	defer pool.Close()
	if err := pool.Ping(ctx); err != nil {
		return port.CIDSweepResult{Aborted: true}, fmt.Errorf("ping database: %w", err)
	}

	sweeper := &service.CIDSweeper{
		Repo:    alkemiodb.NewWithLogger(pool, logger),
		Storage: local.NewCIDMigration(cfg.StoragePath),
	}
	return sweeper.Run(ctx)
}

func main() {
	os.Exit(mainExitCode())
}

func mainExitCode() int {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	return run(ctx, os.Args[1:])
}
