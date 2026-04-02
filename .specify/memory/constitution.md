<!--
Sync Impact Report
- Version change: 1.2.0 → 1.3.0
- Changed principles:
  - Anti-Pattern #11 added — no map[string]any for HTTP responses; use named structs with Render()
- Previous version change: 1.1.0 → 1.2.0
- Changed principles:
  - III. Alkemio Integration First — file-service now owns document table (full CRUD)
  - Integration Requirements — Alkemio DB updated from read-only to full CRUD on document table
- Previous version change: 1.0.0 → 1.1.0
- Changed principles:
  - XII. Meaningful Tests Only — added 95% unit test coverage target
- Previous version change: N/A → 1.0.0 (initial ratification)
- Added principles:
  - I. Hexagonal Architecture
  - II. Storage Abstraction
  - III. Alkemio Integration First
  - IV. Type-Safe Database Access
  - V. Security by Design
  - VI. Test-First Development
  - VII. Root Cause Analysis (NON-NEGOTIABLE)
  - VIII. DRY — Single Source of Truth
  - IX. Lint on Completion
  - X. No Legacy Code
  - XI. No Busywork
  - XII. Meaningful Tests Only
  - XIII. Meaningful Success Criteria
  - XIV. Latest Dependencies Always
  - XV. No Assumptions
- Added sections:
  - Technology Stack Constraints
  - Integration Requirements
  - Anti-Patterns — Quick Reference
- Follow-up TODOs: none
-->

# Alkemio File Service (Go) Constitution

## Core Principles

### I. Hexagonal Architecture

All code MUST follow the hexagonal (ports and adapters) architecture
pattern. Business logic lives in the domain core and MUST NOT depend
on external infrastructure. External systems (database, HTTP,
storage backends) are accessed exclusively through well-defined
ports (interfaces) with concrete adapters.

- Domain types and interfaces MUST reside in dedicated domain packages
  with zero infrastructure imports.
- Each external dependency MUST have its own adapter implementing a
  domain-defined port.
- No adapter MAY import another adapter directly; cross-cutting
  concerns flow through the domain or application layer.

### II. Storage Abstraction

The file service MUST abstract the underlying storage backend behind
a clean port interface. The service MUST support pluggable storage
backends (local filesystem, S3, Azure Blob, etc.) without changes
to business logic or API contracts.

- All file I/O MUST go through a StoragePort interface (save, read,
  delete, exists).
- Storage backend selection MUST be configuration-driven.
- File content MUST be addressed by a content-hash (SHA3-256)
  serving as the externalID, consistent with the existing Alkemio
  storage convention.
- The service MUST NOT leak storage backend details through its API.

### III. Alkemio Integration First

This service exists to serve the Alkemio platform as the universal
file I/O gateway. It replaces the existing TypeScript file-service
and serves all Alkemio services that need file read/write access.

- Public endpoints MUST validate authorization via the
  authorization-evaluation-service (h2c HTTP/2 preferred, NATS
  `auth.evaluate` as fallback). At least one auth transport
  must be configured.
- Private (cluster-internal) endpoints MUST NOT require
  authorization — callers are trusted services within the K8s
  cluster.
- Actor identity on public endpoints MUST be resolved from
  Oathkeeper-injected JWT (`alkemio_actor_id` claim).
- The file-service owns the `document` table (full CRUD). Document
  metadata (externalID, authorizationPolicyId) is read for public
  serving; document records are created, updated, and deleted via
  private endpoints. Authorization policies and tagsets remain
  owned by the server.

### IV. Type-Safe Database Access

All database interactions MUST use sqlc for query generation and pgx
as the PostgreSQL driver. Hand-written SQL queries outside of sqlc
are prohibited except for migration files.

- SQL queries MUST be defined in `.sql` files and compiled via sqlc.
- Database schema changes MUST use versioned migrations.
- The pgx connection pool MUST be configured at the application layer
  and injected into adapters via the hexagonal architecture.

### V. Security by Design

The file service handles document access, making security a
non-negotiable concern at every layer.

- Public endpoints MUST validate authorization on every request via
  the auth-evaluation-service (h2c or NATS).
- Private endpoints MUST be accessible only within the K8s cluster
  (enforced by network policy, not application code).
- Secrets, tokens, and credentials MUST NOT be logged or included in
  error responses.
- All inter-service communication MUST use TLS in production.

### VI. Test-First Development

Tests MUST be written before implementation for all new features.
The red-green-refactor cycle is the standard workflow.

- Unit tests MUST cover domain logic with no infrastructure
  dependencies (use in-memory adapters or mocks for ports).
- Integration tests MUST verify adapter behavior against real
  dependencies (database, storage) where feasible.
- Storage backend tests MUST verify read/write/delete/exists
  operations.

### VII. Root Cause Analysis (NON-NEGOTIABLE)

All debugging and bug fixing MUST be driven by root cause analysis.
Opportunistic or speculative code changes hoping they might resolve
an issue are strictly forbidden.

- Before any fix is applied, the actual root cause MUST be
  identified and documented with evidence.
- If the root cause is unclear, invest time in debugging first —
  guessing wastes more time than investigating.
- Fixes MUST directly address the identified root cause, not
  symptoms.
- Every bug fix commit MUST be traceable to a specific diagnosed
  cause.

### VIII. DRY — Single Source of Truth

Code duplication is treated as a defect. When two or more methods
share substantially the same logic, that logic MUST be extracted
into a shared helper or refactored to eliminate the duplication.

- No two methods MAY implement the same logic in different modules.
- When methods share partial logic, the common part MUST be
  extracted to a shared helper.
- Before implementing new logic, search for existing
  implementations — extend rather than duplicate.
- Configuration, constants, and type definitions MUST live in one
  canonical location.
- Duplicated code paths MUST be identified during review and
  refactored before merge.
- Three similar lines of inline code are acceptable; duplicated
  multi-line blocks are not.

### IX. Lint on Completion

Every piece of code MUST pass linting before it is considered
ready. Linting is not a CI-only gate — it MUST be run locally
when a unit of work (function, file, feature slice) is complete.

- Code MUST pass `golangci-lint run` (or the project-configured
  linter) with zero violations before committing.
- Linter configuration is part of the project and MUST NOT be
  bypassed with `nolint` directives unless justified in a comment.

### X. No Legacy Code

We control the full stack and all consumers. Never silently assume
backward compatibility is required.

- Dead, deprecated, or unused code MUST be removed — not left
  "just in case."
- Backward-compatibility hacks, unused exports, commented-out code,
  and defensive code for scenarios that no longer apply MUST be
  deleted.
- When a feature requires changes across multiple services,
  coordinate those changes rather than maintaining compatibility
  shims.
- Every line of code MUST justify its existence.

### XI. No Busywork

Every task, test, and artifact MUST deliver demonstrable value.

- Reject make-work activities that exist only to satisfy process
  checkboxes.
- Do not create documentation, comments, or abstractions "just in
  case."
- Specifications MUST be lean: only what is necessary to
  communicate intent.

### XII. Meaningful Tests Only

Tests MUST defend real invariants or catch real regressions. Unit test
coverage MUST be at least 95%.

- The 95% coverage target is a minimum bar, not an excuse for
  padding — every test MUST still defend a real invariant.
- Never write tests for the sake of coverage metrics.
- Do not test implementation details, trivial getters/setters, or
  scenarios that cannot fail.
- If a test does not help catch bugs or document critical behavior,
  do not write it.

### XIII. Meaningful Success Criteria

Success criteria MUST be directly testable within this service.

- Never invent arbitrary metrics without baseline measurements or
  explicit stakeholder requirements.
- Avoid vanity metrics or external business outcomes that cannot be
  validated during development.

### XIV. Latest Dependencies Always

When adding or updating any dependency, the latest stable version
MUST be verified online (pkg.go.dev, GitHub releases, etc.).

- Never rely on AI training data for version numbers — it is likely
  outdated.
- Dependencies MUST be pinned to specific versions, but those
  versions MUST be current at time of addition.

### XV. No Assumptions

Never assume requirements, behavior, or implementation details that
are not explicitly defined.

- If something is unclear or unknown, ask the user for
  clarification before proceeding.
- If factual information is needed (versions, API specs, library
  behavior), search online to verify.
- Do not guess — guessing leads to rework; asking or searching
  takes less time than fixing wrong assumptions.

## Anti-Patterns — Quick Reference

The following are **strictly prohibited** (derived from principles
VII–XV):

1. Do not apply speculative fixes — find root cause first
2. Do not keep code "just in case" or for backward compatibility
   unless explicitly requested
3. Do not duplicate logic — find or create a single shared
   implementation
4. Do not add superficial tests for coverage padding
5. Do not invent performance SLAs without evidence
6. Do not create abstractions for hypothetical future needs
7. Do not add comments explaining obvious code
8. Do not rely on training data for dependency versions — check
   online
9. Do not create documentation files unless explicitly requested
10. Do not assume — ask or search when something is unclear
11. Do not use `map[string]any` for HTTP response bodies — use
    named structs with JSON tags. This enables OpenAPI spec
    generation and provides compile-time type safety. Each
    response type MUST have a `Render(w http.ResponseWriter)`
    method.

## Technology Stack Constraints

The following technology choices are fixed and MUST NOT be replaced
without a constitution amendment:

| Component        | Technology               |
|------------------|--------------------------|
| Language         | Go 1.26                  |
| Database driver  | pgx v5                   |
| Query generation | sqlc                     |
| Database         | PostgreSQL               |
| Architecture     | Hexagonal (ports/adapters)|
| Logging          | Zap (structured)         |
| Authorization    | h2c HTTP/2 (preferred) or NATS via auth-evaluation-service |
| Circuit breaker  | sony/gobreaker v2              |
| HTTP router      | chi v5                   |
| Storage backends | Local filesystem (primary), S3 (future) |

Additional dependencies SHOULD be minimized. The Go standard library
MUST be preferred over third-party packages when functionality is
equivalent.

## Integration Requirements

The file service integrates with the following systems:

**Alkemio Platform** (consumers):
- Alkemio Server (Node/TS) — uses private endpoints for file
  read/write/delete operations (replaces direct storageService
  calls).
- WOPI Service (Go) — uses private endpoints for GetFile/PutFile
  operations on behalf of Collabora.
- Frontend — uses public endpoint for file downloads (with
  Oathkeeper auth).

**Authorization Evaluation Service** (Go, h2c HTTP/2 or NATS):
- h2c transport (preferred): POST to `{AUTH_SERVICE_URL}/internal/auth/evaluate`
- NATS transport (fallback): Subject `auth.evaluate`
- Input: `{agentId, privilege, authorizationPolicyId}`
- Output: `{allowed, reason}`
- Circuit breaker: sony/gobreaker v2 (shared `AUTH_BREAKER_*` config)
- Used on public endpoints to check READ privilege before serving
  files.

**Alkemio PostgreSQL Database** (full CRUD on document table):
- Document table: full CRUD — create, read, update, delete document
  records. File-service is the single owner of this table.
- All other tables: read-only.

**Oathkeeper** (reverse proxy):
- Sits in front of public endpoints.
- Injects JWT with `alkemio_actor_id` claim into Authorization
  header.
- Private endpoints are NOT routed through Oathkeeper.

**Existing File Service** (TypeScript, being replaced):
- Located in the `file-service` repository (TypeScript/NestJS)
- This Go service is a drop-in replacement with additional
  write/delete capabilities.
- Must maintain the same public API contract:
  `GET /rest/storage/document/:id`

## Governance

This constitution is the authoritative guide for all development
decisions in the Alkemio File Service (Go). It supersedes informal
conventions and ad-hoc decisions.

- **Amendments**: Any change to this constitution MUST be documented
  with a version bump, rationale, and migration plan for affected
  code.
- **Versioning**: The constitution follows semantic versioning.
  MAJOR for principle removals/redefinitions, MINOR for additions
  or material expansions, PATCH for clarifications.
- **Compliance**: All pull requests MUST be reviewed for compliance
  with these principles. Violations MUST be justified in the PR
  description and tracked as tech debt if accepted.
- **Review cadence**: The constitution SHOULD be reviewed quarterly
  or when significant architectural decisions arise.

**Version**: 1.3.0 | **Ratified**: 2026-03-30 | **Last Amended**: 2026-03-31
