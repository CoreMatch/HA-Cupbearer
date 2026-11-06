# 0001 — Scope and Boundary

## Status

Accepted (records current state)

## Context

HA-Services-Deployer is "the backend service for HA service rollout and
artifact delivery" (`README.md:L3`). Its external contract is HA-Contract
`/home/lnb/Desktop/HA/HA-Services-Deployer/knowledge/0006-version-manifest.md`.

## Decision

Record the actual boundary of the service:

**It is a read-only delivery service.** The entire HTTP surface is GET-only
(`internal/server/router.go::NewRouter`):

- `GET /healthz`, `GET /api/v1/healthz` — liveness.
- `GET /api/v1/manifest` — the version manifest.
- `GET /api/v1/projects` — binary-only projection (see 0002).
- `GET /api/v1/artifacts/:service/*filepath` — artifact file download with a
  path-traversal guard (`internal/server/handlers.go::GetArtifact`).

There is **no upload, publish, or mutation API**. Artifacts become available
by an operator placing files under `storage.artifacts_root/<service>/...`,
and the manifest becomes available by writing the JSON file at
`manifest.path` (default `./data/manifest.json`). The manifest is re-read
from disk on every request (`internal/server/handlers.go::loadManifest`).

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
  (`internal/server/handlers.go::GetManifest`, validation in
  `internal/models/manifest.go::Validate`).
- HA-Services-MaintenanceView is the known consumer of `/api/v1/projects`;
  the contract explicitly notes MaintenanceView is work-in-progress and not
  covered by the manifest contract
  (/home/lnb/Desktop/HA/HA-Services-Deployer/knowledge/0006-version-manifest.md::Purpose).

## Evidence

- `internal/server/router.go::NewRouter` — GET-only route table.
- `internal/server/handlers.go::GetArtifact` — artifact serving with traversal guard.
- `internal/server/handlers.go::loadManifest` — manifest loaded from disk per request; missing file → empty manifest.
- `README.md:L5-L9` — stated current scope.
- Contract:
  `/home/lnb/Desktop/HA/HA-Services-Deployer/knowledge/0006-version-manifest.md::Transport`,
  `/home/lnb/Desktop/HA/HA-Services-Deployer/knowledge/0006-version-manifest.md::Purpose`.
