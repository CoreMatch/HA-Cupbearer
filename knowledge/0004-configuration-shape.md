# 0004 — Configuration Shape

## Status

Accepted (records current state)

## Context

The service reads `config.yaml` from the working directory
(`main.go::main`).

## Decision

Record the verified shape:

- **First start auto-generates the config.** When `config.yaml` does not
  exist, `Load` writes the defaults to disk **and continues running with
  them** (`internal/config/config.go::Load`). This differs from
  HA-WebUI-SDKHandler, which creates the file and exits.
- **Defaults live in code** — `DefaultConfig()`
  (`internal/models/config.go::DefaultConfig`):
  - `version: "1"`
  - `server.listen: ":2770"`
  - `storage.artifacts_root: "./data/artifacts"`
  - `manifest.path: "./data/manifest.json"`
- **No sample service config is shipped.** The README quick start describes
  the auto-generated defaults instead (`README.md:L51-L63 fp=cac45034 ~ Quick start`);
  `examples/manifest.v1.json` is a manifest sample, not a service config.
- **Migration framework at version "1" with an empty chain.**
  `ConfigVersion = "1"` (`internal/config/migrations.go::ConfigVersion`);
  `configMigrations()` returns no migrations yet
  (`internal/config/migrations.go::configMigrations`). A config without
  `version` is stamped to `"1"` and rewritten (with a `.bak.<version>`
  backup) (`internal/config/migrations.go::MigrateConfig`,
  `internal/config/config.go::Load`,
  `internal/config/migrations.go::BackupConfigFile`). Newer-than-supported
  versions are tolerated with a warning
  (`internal/config/migrations.go::MigrateConfig`).
- The artifacts root directory is created at router construction
  (`internal/server/router.go::NewRouter`).

## Consequences (including recorded drift)

- **Code default port vs deployment port.** Bare `go run .` listens on
  `:2770`. The platform compose deployment overrides this to `:18080` via the
  rendered config template (this repo's
  `deploy/config/ha-cupbearer.yaml.tmpl (field: listen)`,
  `listen: ":18080"`; `deploy/docker-compose.yml (field: CUPBEARER_PORT)` maps
  `${CUPBEARER_PORT:-18080}`), and the Dockerfile `EXPOSE 18080`. HA-Contract's
  download-URL example also uses `18080`
  (/home/lnb/Desktop/HA/maintanance/HA-Cupbearer/knowledge/0006-version-manifest.md (field: Download URL Resolution)).
  So `:2770` is only the local-dev default; operators should expect `18080`
  in composed deployments.
- Relative paths (`./data/...`) resolve against the process working
  directory; the compose deployment mounts a `cupbearer-data` volume at
  `/app/data` with `WORKDIR /app`, so artifacts live at
  `/app/data/artifacts` and the manifest at `/app/data/manifest.json` there.
- No config value is required: a brand-new instance starts with zero manual
  configuration but also with an empty manifest and empty artifacts root.

## Evidence

- `main.go::main` — config path.
- `internal/config/config.go::Load` — auto-generate and continue.
- `internal/models/config.go::DefaultConfig` — `DefaultConfig` values.
- `internal/config/migrations.go::ConfigVersion`,
  `internal/config/migrations.go::configMigrations`,
  `internal/config/migrations.go::MigrateConfig`,
  `internal/config/migrations.go::BackupConfigFile`.
- Deployment: `deploy/config/ha-cupbearer.yaml.tmpl (field: listen)`,
  `deploy/docker-compose.yml (field: CUPBEARER_PORT)` (Cupbearer service
  block), `Dockerfile:L21 fp=40012d36`.
