# 0002 — Manifest v1 Serving and Validation

## Status

Accepted (records current state)

## Date

2026-11-04

## Context

The manifest is the service's reason to exist. Its schema, field semantics,
comparison rules, and validation rules are contracted in HA-Contract
`docs/dev/HA-Services-Deployer/version-manifest.md`. This entry records how
the repo implements that contract and where the local docs drift.

## Decision

Verified serving behavior:

- `GET /api/v1/manifest` returns the manifest inside the standard envelope
  `{success, message, data}` (`internal/server/handlers.go:35-51`).
- `loadManifest` reads `manifest.path`, unmarshals, `Normalize()`s (fills
  defaults for missing `schema_version`/`generated_at`, nil slices), then
  `Validate()`s (`internal/server/handlers.go:108-128`).
- Missing manifest file → an **empty valid manifest**
  (`models.NewEmptyManifest()`, `internal/models/manifest.go:55-61`,
  `handlers.go:111-113`), not an error.
- `Validate` implements the contract's validation rules 1–8
  (`internal/models/manifest.go:80-134` ↔ version-manifest.md "Validation
  Rules"): schema_version==1, unique service names, non-empty version,
  ≥1 artifact per service, unique artifact ids, role in
  {binary, archive, asset}, non-empty file_name, lowercase 64-hex sha256,
  size_bytes ≥ 0, and download_path relative and non-escaping
  (`internal/models/manifest.go:136-151`).
- `GET /api/v1/projects` (commit `67b873d`) projects the manifest to
  release-oriented entries keeping only `role == "binary"` artifacts, under a
  top-level `projects` key with `generated_at`
  (`internal/models/manifest.go:153-175`,
  `internal/server/handlers.go:53-72`). This matches the contract's
  description of `/api/v1/projects` (version-manifest.md, Transport).

## Consequences (including recorded drift)

- **README drift.** The committed `README.md` "API skeleton"
  (`README.md:35-40`) lists only `/healthz`, `/api/v1/healthz`,
  `/api/v1/manifest`, `/api/v1/artifacts/:service/*filepath` and omits
  `/api/v1/projects`. The contract lists it; the README does not. Recorded
  as-is; the README is not modified by this knowledge-base change.
- A malformed `manifest.json` surfaces as HTTP 500 with the validation error
  in the body (`handlers.go:36-44`), which is the contract's "reject or fail
  to serve an invalid manifest" behavior.
- The local example `examples/manifest.v1.json` matches the contract's sample
  shape (three services, one binary artifact each).
- `ProjectRelease` duplicates `ServiceRelease`'s fields verbatim
  (`internal/models/manifest.go:46-53`); harmless but noted.

## Evidence

- `internal/server/handlers.go:35-128` — manifest/projects/artifact handlers.
- `internal/models/manifest.go:80-151` — validation; `:153-175` — projects projection.
- `internal/server/router.go:17-23` — route table.
- `examples/manifest.v1.json` — local example.
- Commits: `c5da9d6` (schema + validation), `67b873d` (`/projects`).
- Contract: HA-Contract `docs/dev/HA-Services-Deployer/version-manifest.md` (Manifest Shape, Field Reference, Validation Rules, Transport).
