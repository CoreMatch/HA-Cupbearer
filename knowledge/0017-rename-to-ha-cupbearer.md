# 0017 — Rename to HA-Cupbearer (2026-11-12)

## Status

Accepted (author directive; executed across the ecosystem on 2026-11-12)

## Why "Cupbearer"

The service pours what the Grail holds: artifacts, rendered configuration,
and first-run initialization flow out of it into every other service. The
Grail's water is not fetched by the cup itself — it is poured by the
cupbearer. The author named the service **HA-Cupbearer** on 2026-11-12 to
replace the functional-but-flat old name; the imagery is the pour, not the
vessel.

## The old name

The service was originally named **HA-Services-Deployer**. Per the author's
directive, the old name survives only in this record; usage documentation,
code, templates, and contracts refer to HA-Cupbearer without historical
footnotes.

## What was renamed (2026-11-12)

| Surface | Old | New |
|---|---|---|
| GitHub repository | `CoreMatch/HA-Services-Deployer` | `CoreMatch/HA-Cupbearer` (rename redirect kept as fallback only; CI checkouts use the new name explicitly) |
| Local workspace dir | `maintanance/HA-Services-Deployer` | `maintanance/HA-Cupbearer` |
| Go module | `HA-Services-Deployer` | `HA-Cupbearer` |
| Binary (bare metal + container) | `hasd` | `hacb` |
| Bare-metal install plan | `/www/wwwroot/hasd` | `/www/wwwroot/hacb` (plan only — the service has no production deployment yet) |
| Manifest project / display name | `ha-services-deployer` | `ha-cupbearer` |
| Published image | `limeight/ha-services-deployer` | `limeight/ha-cupbearer` (multi-arch rebuilt; the old repository remains on Docker Hub so digest-pinned old deployments keep pulling) |
| Config template | `deploy/config/ha-services-deployer.yaml.tmpl` | `deploy/config/ha-cupbearer.yaml.tmpl` |
| Compose service / volume | `ha-services-deployer` / `deployer-data` | `ha-cupbearer` / `cupbearer-data` |
| Compose env knobs | `DEPLOYER_BIND` / `DEPLOYER_PORT` / `DEPLOYER_URL` | `CUPBEARER_BIND` / `CUPBEARER_PORT` / `CUPBEARER_URL` |
| Docker labels | `ha-services-deployer.project` / `ha-services-deployer.managed-by` | `ha-cupbearer.project` / `ha-cupbearer.managed-by` |
| Healthz service id | `ha-services-deployer` | `ha-cupbearer` |
| MaintenanceView config section | `deployer:` | `cupbearer:` |
| MaintenanceView passthrough routes | `/api/deployer/healthz`, `/api/deployer/manifest` | `/api/cupbearer/healthz`, `/api/cupbearer/manifest` |
| MaintenanceView client package | `internal/deployer` | `internal/cupbearer` |
| Contract file (HA-Contract) | `docs/api/openapi/ha-services-deployer.yaml` | `docs/api/openapi/ha-cupbearer.yaml` |

## Compatibility decisions

- **MaintenanceView accepts the legacy section name.** `config.Load` maps a
  legacy `deployer:` section onto `cupbearer:` in memory when the new
  section is absent (file is not rewritten; when both are present,
  `cupbearer:` wins). Existing standalone MV configs therefore keep working.
- **State is not migrated.** Orchestration state and docker labels are keyed
  by the new names; containers/state created under the old keys are not
  adopted. This is acceptable: production runs bare metal and has no
  deployment of this service yet, so no live state exists under the old
  keys.
- **`deploy` as a verb is untouched.** Endpoint paths (`/api/v1/deploy/*`),
  `deploy_token`, and the `deploy`/`rollback` run types are orchestration
  semantics, not the product name, and stay as they are.
