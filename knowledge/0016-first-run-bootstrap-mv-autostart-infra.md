# 0016 — First-Run Bootstrap Layer, MV Autostart, and Per-Path Infra (2026-11-12)

## Status

Accepted (implemented + measured, E2E on a clean environment)

## Context

Until this entry, "first-deployment configuration initialization" was
explicitly out of scope (README, version-manifest schema note): a fresh
machine needed manual `openssl` secret generation, manual database/account
creation, manual `.env` assembly, manual signing keys, and per-service config
authoring before anything could run. The author ruled this gap closed:
one-click initialization, with a wizard in MaintenanceView collecting only
what no backend can derive, and MySQL/Redis infrastructure standing per
selected deployment path.

## Decision

### Bootstrap layer (`internal/bootstrap/`)

Independent of `orchestration.enabled`; available whenever the Cupbearer runs.
Contract: HA-Contract `docs/api/openapi/ha-cupbearer.yaml` v1.3.0
(`/api/v1/deploy/bootstrap/*`). Steps, in order:

1. **infra** — MySQL/Redis, standing per selected deployment path
   (`internal/bootstrap/infra.go`):
   - detect → adopt → launch. Managed containers are `ha-infra-mysql-1` /
     `ha-infra-redis-1` from `deploy/infra.yml` (compose project `ha-infra`,
     named volumes `ha-infra-mysql-data` / `ha-infra-redis-data`, loopback
     port publication, network `ha-infra`);
   - existing reachable instances are adopted (`mode=external`), never
     duplicated; adopting a MySQL without operator credentials fails loud;
   - launch generates root/redis passwords into state (fingerprint-only in
     every response), waits for real readiness (MySQL `SELECT 1`, Redis
     `AUTH+PING`), records the published host port in state — reusing a
     managed container keeps the recorded port (changing ports would require
     recreating the container, and this layer never removes containers);
   - port conflicts are bind-tested up front and reported in plain language.
   - Connection forms are reported per selected path (no cross-path "shared"
     promise): bare-metal orchestration connects `127.0.0.1:<host port>`;
     container orchestration connects `mysql:3306` / `redis:6379` via the
     `ha-infra` network (the container trigger attaches the main stack when
     that network exists —
     `internal/orchestrator/container_trigger.go::renderComposeOverride`).
     Both forms address the same managed instance; a deployment picks one
     path, and only that path's form is rendered into its configs.
2. **secrets** — get-or-create all platform secrets
   (`internal/bootstrap/state.go::EnsureSecrets`): super client secret, five
   service client pairs' secrets, MANAGE_TOKEN, MCA_MAINTENANCE_TOKEN,
   SDKHANDLER_INTERNAL_KEY, app DB password. State file
   `data/bootstrap/state.json` (0600, gitignored). Existing `.env` values are
   adopted first (`AdoptSecrets`): the live target wins, drift rewrites the
   state to match and warns — never the other way around.
3. **databases** — `CREATE DATABASE IF NOT EXISTS` ×4 (utf8mb4; hrpa / ygg /
   haskinlib / matchcorearena), `CREATE USER IF NOT EXISTS` + idempotent
   GRANT for the app user, then per-database connectivity verification as the
   app user. Migrations stay where they belong: each service migrates its own
   schema at startup (`facts.migration.mode=auto_on_startup`).
4. **signing-keys** — RSA-4096 pair byte-compatible with YGG's
   `generateKeyPair` (PKCS1 private 0600 / PKIX public 0644); existing pair
   kept; missing public derived from private; public-only fails loud.
5. **env-file** (container target) — merge `deploy/.env`: existing keys
   preserved verbatim (drift warns), missing keys appended with a provenance
   comment (`internal/bootstrap/render.go::MergeEnvFile`). When infra is
   container-managed, `DB_HOST=mysql` / `REDIS_HOST=redis` (compose form).
6. **render-configs** (bare-metal target) — renders `deploy/config/*.tmpl`
   (previously referenced by zero Go code) to per-service output paths with
   the same variable dictionary; `DB_HOST=127.0.0.1:<host port>` form;
   existing files never overwritten; missing template variables fail loud.
   Same-pair guarantee: the HRPAuth `oauth2.service_clients` seeds and each
   consumer's credentials render from one dictionary, so both sides carry the
   identical pair by construction (verified in E2E).

Semantics: every step idempotent; run records persist per step
(`data/bootstrap/runs/`); re-running is the resume mechanism (a killed run
stays `running` on disk; the next run skips/reuses completed work). All
generated values appear only masked (`sha256:<16>` fingerprint) in plans,
run records, and logs. `plan` = dry-run (writes nothing); `run` = async apply
(single-flight). Auth: plan/run fail-closed on `X-Deploy-Token`; the token
itself is auto-generated into the credentials file on first start when absent
(fingerprint logged, plaintext never).

### MaintenanceView autostart (`internal/mvmanager/`)

The Cupbearer supervises exactly one MV instance so operators never deploy it
separately: a healthy MV at the configured address is reused (never a second
copy); otherwise the binary is located (config path or candidates next to the
Cupbearer binary), its config is generated (listen address, cupbearer URL,
deploy token held server-side, `loopback_session: true`) and it runs as a
managed child (bounded restarts, graceful stop with the Cupbearer, log at
`<work_dir>/mv.log`). Missing binary = `unavailable` with a plain-language
reason, never fatal. MV side: `server.loopback_session` (MV config) trusts
loopback clients without login for this managed form only; the deploy token
still guards mutating proxy calls server-side and never reaches the browser.

### Wizard (MV frontend, `WizardView.tsx`)

Replaces the legacy per-project bootstrap page. Shows infra cards (both
address forms, one-click launch when absent), asks only the six public URLs
(client-side blocking with plain-language messages), advanced overrides
collapsed; preview (plan) and one-click run with per-step progress, masked
fingerprints, and a retry button (resume).

## Consequences

- Fresh machine: run the Cupbearer binary → open the MV URL it reports →
  one-click infra + fill URLs + run → platform config exists and services can
  start. Measured E2E (2026-11-12, clean environment): infra launched and
  health-waited; four DBs + app user created; keys generated; `.env` merged;
  nine bare-metal configs rendered with identical credential pairs both sides;
  composed stack (published images) started healthy against the `ha-infra`
  MySQL/Redis (container-path connection form); super client issued a real token; all seven
  `oauth2_clients` rows seeded; winnerproxy `hasJoined` probe 204. Idempotent
  re-run: all steps skip/reuse; edited `.env` ports preserved with drift
  warnings. Mid-run kill: record stays `running`, re-run resumes. Port
  conflict and wipe (`docker compose -f deploy/infra.yml down -v`) paths
  verified with plain-language outcomes.
- `deploy/config/hrpauth.yaml.tmpl` gained one variable: `yggdrasil_api.base_url`
  is now `${YGG_INTERNAL_URL}` (compose injects the previous hardcoded value
  as the default — container behavior unchanged; bare-metal renders the
  loopback form). `.env.example` documents the optional variable.
- Out of scope (recorded, not built): HTTP-triggered bootstrap from
  MaintenanceView was implemented, but bare-metal process start of the
  services themselves remains orchestration's job (deploy runs / panels);
  bootstrap stops at "config + infra ready".

## Evidence

- `internal/bootstrap/engine.go::execute` — step pipeline and resume records.
- `internal/bootstrap/infra.go::EnsureInfra` — per-path infra semantics.
- `internal/bootstrap/render.go::MergeEnvFile`, `::EnvForBaremetal` — merge
  and dual-form address derivation.
- `internal/mvmanager/manager.go::Start` — reuse-or-manage decision.
- `internal/server/handlers_bootstrap.go` — endpoint surface.
- `deploy/infra.yml` — infra compose (wipe procedure in header comment).
- HA-Contract `docs/api/openapi/ha-cupbearer.yaml` v1.3.0.
- E2E transcript: this session's run records under `data/bootstrap/runs/`
  (ids 20261112-0124xx … 20261112-0134xx, including the killed `running`
  record and its successful resume).
