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

### Left open deliberately (report-only)

- **`public_redirect_uris` uses `${HRPAUTH_PUBLIC_URL}/oauth/callback`** while
  the code convention derives redirect URIs from `frontend.url`
  (config.go migrateV3ToV4 + generator). Correct value depends on where the
  WebUI actually serves the callback — needs author confirmation; if the
  WebUI lives on the frontend domain, switch the template line to
  `${HRPAUTH_FRONTEND_URL}/oauth/callback`.
- **Unguarded compose variables** (no `:?`, no default): `MCA_MAINTENANCE_TOKEN`
  (empty silently disables ops-token auth), `HRPAUTH_SUPER_CLIENT_SECRET`
  (empty secret validates against empty secret — a security gap if .env
  omits it), `HRPAUTH_PUBLIC_CLIENT_ID`, `WINNERPROXY_PUBLIC_URL`,
  `HASKINPROXY_PUBLIC_URL`, `MAINTENANCEVIEW_PUBLIC_URL`, `DB_*`. Adding `:?`
  changes start-failure behavior of existing deployments; proposed as
  hardening, not applied.
- **Code-side dead fields** (not template errors): MCA
  `server.read_timeout_seconds/write_timeout_seconds`; WinnerProxy
  `log.*`, `site.*`, `server.read/write_timeout_sec`,
  `upstreams.hrpauth.timeout_sec`; SDKHandler `current_link` degenerate use;
  MV `public_base_url` reserved. Templates keep them (harmless, documented);
  removal is a code-side decision.
- **Actual `deploy/.env` drift** (deployment input, not repo content):
  missing `MCA_MAINTENANCE_TOKEN`; `WINNERPROXY_CLIENT_ID=winnerproxy`
  lowercase legacy name with secret equal to the super secret (the
  seed-skip guard then never creates the row; startup can fatal). Align the
  live `.env` with `.env.example` at the next maintenance window. Same for
  lowercase legacy `YGG_CLIENT_ID` / `HASKINPROXY_CLIENT_ID`.

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
  (agent reconciliation reports, 2026-11-08); key anchors: HRPAuth
  config/config.go:177 (ConfigVersion 15), MatchCoreArena-Server
  config/config.go:30 (1.9.0), WinnerProxy config/config.go:188 (v2),
  HASkinLib config/config.go:69 (v7), HA-WebUI-SDKHandler
  internal/config/config.go:16 (v4), HA-Services-Deployer
  internal/config/migrations.go:11 (v2), HA-Services-MaintenanceView
  internal/config/migrations.go:11 (v1).
