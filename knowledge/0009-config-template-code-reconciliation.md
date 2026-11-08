# 0009. Config Template ↔ Code Reconciliation (2026-11-08)

## Status
Accepted

## Context
`deploy/config/*.yaml.tmpl` are the real deployment inputs: the container
entrypoint renders them with `envsubst` on every start. A template field the
code never reads, a missing required field, or a stale version marker all
surface only at runtime (silently or as a crash loop). This entry records the
first full field-by-field reconciliation of every template against the
config-loading code of its service repository, plus the defects fixed in the
same batch (merge `d294fdc`) and the observations deliberately left open.

Method: per template, every leaf field was matched to a read site in the
service repo; version markers were compared against each code base's
`ConfigVersion` / migration-chain endpoint; every `${VAR}` placeholder was
checked against the compose `environment` (and `.env.example` defaults);
every template comment was checked against current code facts.

## Decision

### Per-service results (field counts = template leaves matched to code reads)

| Template | Version vs code | Fields | Dead/unused in code | Missing from template | Verdict |
|---|---|---|---|---|---|
| hrpauth.yaml.tmpl | fixed 14→15 (code v15) | 64/64 read | none | webauthn section (derived defaults are correct for this deployment; acceptable), haskinlib seed (fixed in this batch) | aligned after fixes |
| hrpauth-yggdrasil-api.yaml.tmpl | "4" = code v4 | 52/52 read | none | none (signature keys are file-path driven, covered) | aligned |
| haskinlib.yaml.tmpl | "7" = code v7 | 19/19 read | none | none | aligned after comment fix |
| haskinproxy.yaml.tmpl | no versioning either side | 13/13 read | none | none | aligned; compose mount was missing (fixed) |
| winnerproxy.yaml.tmpl | "2" = code v2 (unchecked metadata) | 26/26 parsed | 7 never referenced (version, server.read/write_timeout_sec, log.level, log.format, site.name, site.version) + timeout_sec logged-but-not-applied | none (Load defaults everything; creds guarded by :?) | aligned; stale comment fixed; compose mount was missing (fixed) |
| mca-server.yaml.tmpl | "1.9.0" = code 1.9.0 (historical drift already fixed) | 34/34 parsed | server.read/write_timeout_seconds (gin runs without timeouts) | auth.tokens_path (compose injects MCA_TOKENS_PATH, always overrides; absent file only warns) | aligned |
| sdkhandler.yaml.tmpl | "4" = code v4 | 20/20 read | current_link is semi-degenerate (existence check only; publish no longer swaps the symlink) | none | aligned |
| ha-services-deployer.yaml.tmpl | fixed "1"→"2" (code v2) | 4/4 read | none | orchestration.* (zero-value = orchestrator disabled by design) | aligned after fix |
| ha-services-maintenance-view.yaml.tmpl | "1" = code v1 | 6/6 parsed | server.public_base_url reserved (echoed by /api/config only; knowledge 0005/0006) | none | aligned |

### Defects fixed in this batch (merge `d294fdc`, plus service-repo commits)

1. **Missing template volume mounts** (compose): `haskinproxy` and
   `winnerproxy` services had no `./config/*.yaml.tmpl:/etc/ha/config.yaml.tmpl:ro`
   mount. The entrypoint `[ -f ]` guard then silently skipped rendering:
   haskinproxy ran on code defaults (upstream `http://localhost:2778`
   unreachable, empty credentials) while `/health` still returned 200;
   winnerproxy crash-looped on its empty-credentials fatal check. Mounts
   added mirroring the other seven services.
2. **haskinlib credential chain broken under rendered deployment**: seeding
   is config-driven (`oauth2_service.go` iterates `oauth2.service_clients`);
   the default-config generator (which carries the haskinlib seed) never
   runs when a rendered config.yaml exists. `hrpauth.yaml.tmpl` gained the
   5th seed entry and the hrpauth compose service injects
   `HASKINLIB_CLIENT_ID/SECRET` with `:?` guards — the same .env pair the
   haskinlib template consumes, so seed secret and service credential are
   inherently equal. Related code fix: HASkinLib `EnsureMigrations` DSN used
   `cfg.Host` instead of `cfg.Addr()`, ignoring `database.port` (merge
   `e43a58b`); the same one-line defect was fixed in HRPAuth (`f633104`,
   silent migration failure) and HRPAuth-Yggdrasil-API (`481e972`, fatal at
   startup).
3. **Stale version markers**: hrpauth template 14→15, deployer template 1→2.
   Both eliminated a migrate+backup+rewrite cycle on every container start.
4. **Stale comments**: winnerproxy credential comment ("compose injects
   super creds by default" — actually :?-required fill-what-you-give);
   hrpauth encryption-removal wording, sdk_packages fallback wording,
   v14→v15 reference; haskinlib core_api provenance repointed to the
   rendered seed.

### Follow-ups settled by the author (2026-11-08, second pass)

- **`public_redirect_uris`** — fixed to `${HRPAUTH_FRONTEND_URL}/oauth/callback`:
  the author confirmed the WebUI callback lives on the frontend domain and
  that the template must follow the code convention (redirect URIs derived
  from `frontend.url`).
- **Fail-fast guards added** (author decision: "宁可让 compose 起不来也不能静默
  放过"; container deployments only, bare-metal production unaffected):
  `:?` with actionable messages on `DB_HOST/DB_USER/DB_PASSWORD` (common-env),
  `HRPAUTH_PUBLIC_CLIENT_ID`, `HRPAUTH_SUPER_CLIENT_ID/SECRET` (empty super
  secret authenticates empty-secret clients — security gap),
  `HASKINPROXY_PUBLIC_URL`, `WINNERPROXY_PUBLIC_URL` (empty silently skips
  relay registration), `MCA_MAINTENANCE_TOKEN` (empty silently disables OPS
  auth), `MAINTENANCEVIEW_PUBLIC_URL`.
- **Live `deploy/.env` aligned** (backup outside the repo at
  `/home/lnb/Desktop/HA/.env-backups/deployer-20261108.env`; `deploy/.env` is
  gitignored): satellite client ids corrected to presence names
  (WinnerProxy / HRPAuth-Yggdrasil-API / HASkinProxy) with fresh secrets
  (old winnerproxy secret equalled the super secret, tripping the seed-skip
  guard); `HASKINLIB_CLIENT_ID/SECRET` and `MCA_MAINTENANCE_TOKEN` added.
  All 20 `:?`-guarded variables verified present and non-empty afterwards.
- **Code-side dead fields stay** (author decision): MCA
  `server.read_timeout_seconds/write_timeout_seconds`; WinnerProxy
  `log.*`, `site.*`, `server.read/write_timeout_sec`,
  `upstreams.hrpauth.timeout_sec`; SDKHandler `current_link` degenerate use;
  MV `public_base_url` reserved. Templates keep them (harmless, documented).

## Consequences
- Every template field now maps to a code read site or is documented as
  reserved/dead; version markers match code; all `${VAR}` placeholders are
  compose-injected.
- The two silent-failure orchestration gaps (missing mounts, haskinlib seed
  chain) are closed; rendering is verified by envsubst + YAML parse in the
  batch commit message.
- Open items above are tracked here, not in PENDING-DECISIONS, because they
  need author confirmation or deployment-window action rather than code.

## Evidence
- Reconciliation batch: merge `d294fdc` (this repository), HASkinLib `e43a58b`,
  HRPAuth `f633104`, HRPAuth-Yggdrasil-API `481e972`.
- Read-site citations per service are recorded in the batch work papers
  (agent reconciliation reports, 2026-11-08); key anchors: [xref] HRPAuth
  [xref] config/config.go:177 (ConfigVersion 15), MatchCoreArena-Server
  [xref] config/config.go:30 (1.9.0), WinnerProxy config/config.go:188 (v2),
  [xref] HASkinLib config/config.go:69 (v7), HA-WebUI-SDKHandler
  [xref] internal/config/config.go:16 (v4), HA-Services-MaintenanceView
  [xref] internal/config/migrations.go:11 (v1).
- This repository's own config-version anchor (locally verified):
  internal/config/migrations.go:11 (v2).
