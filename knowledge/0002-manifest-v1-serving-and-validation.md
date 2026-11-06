# 0002 — Manifest v1 Serving and Validation

## Status

Accepted (records current state)

## Context

The manifest is the service's reason to exist. Its schema, field semantics,
comparison rules, and validation rules are contracted in HA-Contract
`docs/dev/HA-Services-Deployer/version-manifest.md`. This entry records how
the repo implements that contract and where the local docs drift.

## Decision

Verified serving behavior:

- `GET /api/v1/manifest` returns the manifest inside the standard envelope
  `{success, message, data}` (`internal/server/handlers.go::GetManifest`).
- `loadManifest` reads `manifest.path`, unmarshals, `Normalize()`s (fills
  defaults for missing `schema_version`/`generated_at`, nil slices), then
  `Validate()`s (`internal/server/handlers.go::loadManifest`).
- Missing manifest file → an **empty valid manifest**
  (`models.NewEmptyManifest()`, `internal/models/manifest.go::NewEmptyManifest`,
  called in `internal/server/handlers.go::loadManifest`), not an error.
- `Validate` implements the contract's validation rules 1–8
  (`internal/models/manifest.go::Validate` ↔
  `/home/lnb/Desktop/HA/HA-Services-Deployer/knowledge/0006-version-manifest.md (field: Validation Rules)`):
  schema_version==1, unique service names, non-empty version,
  ≥1 artifact per service, unique artifact ids, role in
  {binary, archive, asset}, non-empty file_name, lowercase 64-hex sha256,
  size_bytes ≥ 0, and download_path relative and non-escaping
  (`internal/models/manifest.go::validateDownloadPath`).
- `GET /api/v1/projects` projects the manifest to
  release-oriented entries keeping only `role == "binary"` artifacts, under a
  top-level `projects` key with `generated_at`
  (`internal/models/manifest.go::Projects`,
  `internal/server/handlers.go::GetProjects`). This matches the contract's
  description of `/api/v1/projects`
  (/home/lnb/Desktop/HA/HA-Services-Deployer/knowledge/0006-version-manifest.md::Transport).

## Consequences (including recorded drift)

- **README drift.** The `README.md` "API skeleton"
  (`README.md:L35-L40`) lists only `/healthz`, `/api/v1/healthz`,
  `/api/v1/manifest`, `/api/v1/artifacts/:service/*filepath` and omits
  `/api/v1/projects`. The contract lists it; the README does not. Recorded
  as-is.
- A malformed `manifest.json` surfaces as HTTP 500 with the validation error
  in the body (`internal/server/handlers.go::GetManifest`), which is the
  contract's "reject or fail to serve an invalid manifest" behavior.
- The local example `examples/manifest.v1.json` matches the contract's sample
  shape (three services, one binary artifact each).
- `ProjectRelease` duplicates `ServiceRelease`'s fields verbatim
  (`internal/models/manifest.go::ProjectRelease`); harmless but noted.

## Evidence

- `internal/server/handlers.go::GetManifest`, `internal/server/handlers.go::GetProjects`,
  `internal/server/handlers.go::GetArtifact` — manifest/projects/artifact handlers.
- `internal/models/manifest.go::Validate` — validation;
  `internal/models/manifest.go::Projects` — projects projection.
- `internal/server/router.go::NewRouter` — route table.
- `examples/manifest.v1.json` — local example.
- Contract:
  `/home/lnb/Desktop/HA/HA-Services-Deployer/knowledge/0006-version-manifest.md (field: Manifest Shape)`,
  `/home/lnb/Desktop/HA/HA-Services-Deployer/knowledge/0006-version-manifest.md (field: Field Reference)`,
  `/home/lnb/Desktop/HA/HA-Services-Deployer/knowledge/0006-version-manifest.md (field: Validation Rules)`,
  `/home/lnb/Desktop/HA/HA-Services-Deployer/knowledge/0006-version-manifest.md::Transport`.
