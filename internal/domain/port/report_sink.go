package port

// ReportSink persists an operational run report somewhere that OUTLIVES the
// process that produced it (018-legacy-cid-normalization FR-018, FR-019).
//
// This is deliberately NOT part of StoragePort. A report is not content: the
// blob namespace is flat and content-addressed, so a report written among the
// blobs would be counted as stored content by any on-disk audit — exactly the
// confusion that made file-service#63's evidence-gathering awkward. Keeping it
// a separate port also keeps the blob port free of a concern an object-store
// backend would have to implement for no reason.
//
// It exists because the `sweep-cids` pass reclaims the legacy blob in the same
// run that renames it: once the pass ends, the report is the ONLY record of the
// old→new mapping. A one-shot Job's container filesystem dies with the pod, so
// the report has to land on a mounted, durable surface.
type ReportSink interface {
	// WriteReport durably stores data under name and returns the location an
	// operator can read it from — logged at the end of the run, so someone with
	// only the Job logs can find the file without knowing the storage layout.
	//
	// name is a bare file name: implementations MUST reject anything carrying a
	// path separator rather than writing outside their reserved directory.
	// The write must be durable before returning — a report that is still only
	// in the page cache when a one-shot Job's node reboots records nothing.
	//
	// It MUST NOT overwrite an existing report: reports accumulate across runs,
	// and clobbering one destroys the only record of an earlier pass.
	WriteReport(name string, data []byte) (string, error)

	// AppendJournal durably appends one line to name, creating it if needed, and
	// returns the location.
	//
	// This exists because WriteReport happens once, at the end of a pass that may
	// run for hours while irreversibly deleting blobs as it goes. A pass killed
	// at record 900 of 1053 — OOM, node eviction, SIGKILL past the grace period —
	// would take 900 old→new mappings with it, for blobs that are already gone
	// and rows that no longer match the scan predicate, so no re-run could
	// reconstruct them. The caller appends each mapping BEFORE destroying the
	// bytes it describes, which bounds the loss at zero rather than at the whole
	// pass. Each call must be durable on return, for the same reason.
	AppendJournal(name string, line []byte) (string, error)
}
