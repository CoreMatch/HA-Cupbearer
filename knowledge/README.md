# HA-Services-Deployer Knowledge Base

## Positioning

This directory is **the project's own notebook** — ADR-style records kept for
self-hosting and maintenance of this service.

- It does **not** define external behavior. The version-manifest contract is
  owned by [HA-Contract](https://github.com/CoreMatch/HA-Contract) at
  `/home/lnb/Desktop/HA/HA-Contract/docs/dev/HA-Services-Deployer/version-manifest.md`.
  API shapes, field semantics, and validation rules are normative there, not
  here.
- When an entry here conflicts with HA-Contract, **fix the entry, not the
  contract**.
- Entries describe the verified current state at the time of writing. Every
  claim carries evidence (`file:line` and/or commit hash). Claims that could
  not be verified are marked `not verified`. Gaps and unimplemented items are
  recorded as-is, without sugar-coating.

## Entries

| ID | Title | One-line claim |
|---|---|---|
| [0001](0001-scope-and-boundary.md) | Scope and boundary | Read-only delivery service: it serves the manifest and artifact files placed on disk; it has no upload/ingest API and no authentication. |
| [0002](0002-manifest-v1-serving-and-validation.md) | Manifest v1 serving and validation | The manifest file is loaded, normalized, and validated on every request; a missing file yields an empty manifest; `/api/v1/projects` is the binary-only projection consumed by MaintenanceView. |
| [0003](0003-sha256-not-verified-server-side.md) | sha256 is not verified server-side (unimplemented) | Manifest `sha256`/`size_bytes` are format-checked only; the server never hashes served files, so content identity is currently a consumer-side duty. |
| [0004](0004-configuration-shape.md) | Configuration shape | `config.yaml` is auto-generated on first start from `DefaultConfig` (`:2770`, `./data/artifacts`, `./data/manifest.json`); the example file was removed; compose deployments override the port to `:18080`. |
| [0005](0005-platform-compose-template-lives-here.md) | Platform compose template lives here | `deploy/` hosts the whole-platform Compose orchestration whose build contexts point at sibling repos; it is deployment tooling, not part of the service's API surface. |

## Maintenance notes

- Records are written against `main` at commit `657bcb8` (2026-11-04).
- At the time of writing, the working tree contains an **uncommitted** local
  edit to `README.md` (removing the compose section). It is not part of these
  records and was left untouched.
