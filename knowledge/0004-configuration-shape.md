# 0004 — Configuration Shape

## Status

Accepted (records current state)

## Date

2026-11-04

## Context

The service reads `config.yaml` from the working directory
(`main.go:11`). A versioning/migration framework was added in commit
`9b937e1` (2026-10-17), and the previously shipped `config.example.yaml` was
removed in commit `a606997` (2026-11-04).

## Decision

Record the verified shape:

- **First start auto-generates the config.** When `config.yaml` does not
  exist, `Load` writes the defaults to disk **and continues running with
  them** (`internal/config/config.go:14-27`). This differs from
  HA-WebUI-SDKHandler, which creates the file and exits.
- **Defaults live in code** — `DefaultConfig()`
  (`internal/models/config.go:22-35`):
  - `version: "1"`
  - `server.listen: ":2770"`
  - `storage.artifacts_root: "./data/artifacts"`
  - `manifest.path: "./data/manifest.json"`
- **The example file is gone.** `a606997` deleted `config.example.yaml` and
  updated the README quick start to describe auto-generation instead
  (`README.md:21-28`). There is no shipped sample config for the service
  itself anymore; `examples/manifest.v1.json` is a manifest sample, not a
  service config.
- **Migration framework at version "1" with an empty chain.**
  `ConfigVersion = "1"` (`internal/config/migrations.go:11`);
  `configMigrations()` returns no migrations yet
  (`internal/config/migrations.go:23-27`). A config without `version` is
  stamped to `"1"` and rewritten (with a `.bak.<version>` backup)
  (`internal/config/migrations.go:39-47`, `internal/config/config.go:41-52`,
  `BackupConfigFile` at `migrations.go:115-130`). Newer-than-supported
  versions are tolerated with a warning (`migrations.go:53-57`).
- The artifacts root directory is created at router construction
  (`internal/server/router.go:33`).

## Consequences (including recorded drift)

- **Code default port vs deployment port.** Bare `go run .` listens on
  `:2770`. The platform compose deployment overrides this to `:18080` via the
  rendered config template (this repo's
  `deploy/config/ha-services-deployer.yaml.tmpl`, `listen: ":18080"`;
  `deploy/docker-compose.yml` maps `${DEPLOYER_PORT:-18080}`), and the
  Dockerfile `EXPOSE 18080`. HA-Contract's download-URL example also uses
  `18080` (version-manifest.md, Download URL Resolution). So `:2770` is only
  the local-dev default; operators should expect `18080` in composed
  deployments.
- Relative paths (`./data/...`) resolve against the process working
  directory; the compose deployment mounts a `deployer-data` volume at
  `/app/data` with `WORKDIR /app`, so artifacts live at
  `/app/data/artifacts` and the manifest at `/app/data/manifest.json` there.
- No config value is required: a brand-new instance starts with zero manual
  configuration but also with an empty manifest and empty artifacts root.

## Evidence

- `main.go:11` — config path.
- `internal/config/config.go:14-27` — auto-generate and continue.
- `internal/models/config.go:22-35` — `DefaultConfig` values.
- `internal/config/migrations.go:11`, `:23-27`, `:39-47`, `:53-57`, `:115-130`.
- Commits: `9b937e1` (versioning/migrations), `a606997` (remove config.example.yaml), `e4a2d12` (merge of that branch).
- Deployment: `deploy/config/ha-services-deployer.yaml.tmpl`, `deploy/docker-compose.yml` (deployer service block), `Dockerfile:21`.
