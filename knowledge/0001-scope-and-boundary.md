# 0001 — Scope and Boundary

## Status

Accepted (records current state)

## Date

2026-11-04

## Context

HA-Services-Deployer was initialized on 2026-10-17 (commits `1d34788`,
`6b2df78`) as "the backend service for HA service rollout and artifact
delivery" (`README.md:3`). Its external contract is HA-Contract
`docs/dev/HA-Services-Deployer/version-manifest.md`.

## Decision

Record the actual boundary of the service at HEAD:

**It is a read-only delivery service.** The entire HTTP surface is GET-only
(`internal/server/router.go:15-23`):

- `GET /healthz`, `GET /api/v1/healthz` — liveness.
- `GET /api/v1/manifest` — the version manifest.
- `GET /api/v1/projects` — binary-only projection (see 0002).
- `GET /api/v1/artifacts/:service/*filepath` — artifact file download with a
  path-traversal guard (`internal/server/handlers.go:74-106`).

There is **no upload, publish, or mutation API**. Artifacts become available
by an operator placing files under `storage.artifacts_root/<service>/...`,
and the manifest becomes available by writing the JSON file at
`manifest.path` (default `./data/manifest.json`). The manifest is re-read
from disk on every request (`internal/server/handlers.go:108-128`).

There is **no authentication or authorization** anywhere in the server
(`internal/server/router.go` uses plain `gin.Default()` plus handlers; no
middleware verifies identity).

## Consequences

- The "artifact delivery" half of the rollout story is manual/out-of-band:
  building releases, computing sha256, copying files to the artifacts root,
  and updating `manifest.json` are not implemented by any component in this
  repository. Gap recorded as-is; whether an automated publisher is planned
  is *pending author confirmation*.
- Anyone who can reach the port can list releases and download artifacts;
  self-hosted deployments must firewall the port (compose default `18080`).
- Because the manifest is read per request, updating releases is a simple
  file replacement with no restart needed; but an invalid manifest makes
  `GET /api/v1/manifest` and `GET /api/v1/projects` return 500 until fixed
  (`internal/server/handlers.go:35-51`, validation in
  `internal/models/manifest.go:80-134`).
- HA-Services-MaintenanceView is the known consumer of `/api/v1/projects`;
  the contract explicitly notes MaintenanceView is work-in-progress and not
  covered by the manifest contract (version-manifest.md, Purpose section).

## Evidence

- `internal/server/router.go:15-23` — GET-only route table.
- `internal/server/handlers.go:74-106` — artifact serving with traversal guard.
- `internal/server/handlers.go:108-128` — manifest loaded from disk per request; missing file → empty manifest.
- `README.md:5-9` — stated current scope.
- Commits: `6b2df78` (initial codebase, 2026-10-17), `c5da9d6` (manifest v1 schema and validation), `67b873d` (`/projects` endpoint).
- Contract: HA-Contract `docs/dev/HA-Services-Deployer/version-manifest.md` (Transport, Purpose).
