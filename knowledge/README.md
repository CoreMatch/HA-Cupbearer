# HA-Services-Deployer Knowledge Base

## Positioning

This directory is **the project's own notebook** — ADR-style records kept for
self-hosting and maintenance of this service.

- It does **not** define external behavior. The version-manifest contract is
  owned by [HA-Contract](https://github.com/CoreMatch/HA-Contract) at
  `/home/lnb/Desktop/HA/HA-Services-Deployer/knowledge/0006-version-manifest.md`.
  API shapes, field semantics, and validation rules are normative there, not
  here.
- When an entry here conflicts with HA-Contract, **fix the entry, not the
  contract**.
- Entries describe only the project's verified current state. Every claim
  carries evidence (`file:line`). Claims that could not be verified are
  marked `not verified`. Gaps and unimplemented items are recorded as-is,
  without sugar-coating.

## Entries

| ID | Title | One-line claim |
|---|---|---|
| [0001](0001-scope-and-boundary.md) | Scope and boundary | Read-only delivery service: it serves the manifest and artifact files placed on disk; it has no upload/ingest API and no authentication. |
| [0002](0002-manifest-v1-serving-and-validation.md) | Manifest v1 serving and validation | The manifest file is loaded, normalized, and validated on every request; a missing file yields an empty manifest; `/api/v1/projects` is the binary-only projection consumed by MaintenanceView. |
| [0003](0003-sha256-not-verified-server-side.md) | sha256 is not verified server-side (unimplemented) | Manifest `sha256`/`size_bytes` are format-checked only; the server never hashes served files, so content identity is currently a consumer-side duty. |
| [0004](0004-configuration-shape.md) | Configuration shape | `config.yaml` is auto-generated on first start from `DefaultConfig` (`:2770`, `./data/artifacts`, `./data/manifest.json`); no sample service config is shipped; compose deployments override the port to `:18080`. |
| [0005](0005-platform-compose-template-lives-here.md) | Platform compose template lives here | `deploy/` hosts the whole-platform Compose orchestration whose build contexts point at sibling repos; it is deployment tooling, not part of the service's API surface. |
| [0006](0006-version-manifest.md) | HA Services Deployer version manifest | Migrated back from HA-Contract `docs/dev/HA-Services-Deployer/version-manifest.md` on 2026-11-07 (source: HA-Contract@bab2a05). |
| [0007](0007-bare-metal-orchestration-scope.md) | Bare-metal orchestration scope and boundary | Config v2 adds an opt-in orchestrator that drives Tinyvisor (artifact → version dir + symlink → script upsert → restart → health → optional post-deploy smoke → rollback), gated by a one-pass graded preflight (blockers refuse with the full report); `manager != tinyvisor` projects are foreign/read-only; mutating endpoints are token-gated fail-closed. |
| [0008](0008-local-e2e-orchestration-verification.md) | Local end-to-end orchestration verification | 2026-11-08 local closed loop on real MaintenanceView binaries: deploy → up-to-date re-run → upgrade → rollback with binary-fingerprint proof; records what was real vs. local_path placeholder. |
| [0009](0009-config-template-code-reconciliation.md) | Config template ↔ code reconciliation | 2026-11-08 full field-by-field audit of deploy/config/*.yaml.tmpl vs each service's config loader: versions, dead/missing fields, ${VAR} injection, stale comments; fixes (missing template mounts, haskinlib seed chain, version markers) and the deliberately-open items. |
| [0010](0010-avalon-cascade-no-helper-measured-boundary.md) | Avalon + Cascade no-helper form: measured boundary | 2026-11-08 isolated deployment of the no-helper form: what works (supervision, control plane, edge room-snapshot flow), what cannot work by design (game start, `/v1/health`, Cascade `/readyz`), what unlocks game start, and the first-mover deployment pitfalls. |
