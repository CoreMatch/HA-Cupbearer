# 0007 — Bare-Metal Orchestration Scope and Boundary

## Status

Accepted (records current state)

## Context

Cupbearer was a read-only delivery service (0001). Since config v2 it also
carries an opt-in bare-metal orchestration capability that drives Tinyvisor
on the target machine: HA-Cupbearer is the orchestrator, Tinyvisor
is the process manager. No standalone orchestration CLI exists.

## Decision

Record the boundary of the orchestration capability.

**Activation.** Orchestration is off unless `orchestration.enabled: true`
(config v2). When off, the service is byte-for-byte the 0001 read-only
delivery service; all `/api/v1/deploy/*` routes answer 503. Config v1 files
auto-migrate to v2 without behavior change (`internal/config/migrations.go`).

**Manifest model.** Each project declares `deploy-manifest.yaml` at its repo
root; external dependencies (mysql/redis) live in `deploy/manifests/`.
Schema v1 splits runtime-agnostic `facts` (ports, health, depends_on, group,
data dirs, migration mode) from runtime-specific `baremetal` /
`container` execution blocks; `container.enabled` must be false (frozen for
the later Docker phase). Loader + validator: `internal/manifest/`
(schema, dependency existence, dependency cycle, cross-project port
conflict, enum checks). Foreign projects (`manager != tinyvisor`) may
record `"unknown"` for informational fields.

**Duplicate project names are resolved, not fatal.** Parallel worktrees and
extra checkouts carry manifest copies into the scan roots; refusing to run
on a duplicate would take the whole deploy chain down whenever someone
opens a branch (measured 2026-11-08). Rules (`internal/manifest/manifest.go::LoadAggregate`):
hidden dirs (`.trae`, `.git`, …) and obvious artifact/temp dirs
(`build`, `dist`, `out`, `target`, `tmp`, `temp`, `node_modules`,
`vendor`) are never scanned; among the rest, the first manifest in load
order wins — `scan_roots` configuration order, then directory-name order
within a root, then the external manifest dir — and every ignored copy is
reported with its full path as a **warning** (runs proceed). Strict mode
(`orchestration.strict_project_names: true`, default off) escalates
duplicates to errors for contexts that want fail-fast.

**Execution semantics** (`internal/orchestrator/`):

- Deploy pipeline per managed project: artifact (sha256 verified, idempotent
  staging) → `releases/<version>/` → atomic `current` symlink switch →
  idempotent Tinyvisor script upsert by name → restart via Tinyvisor →
  HTTP health probe per `facts.health`. A symlink switch without restart is
  never treated as a deployment.
- Idempotence: version unchanged + script aligned + healthy ⇒ `up-to-date`,
  zero actions, process untouched. Re-running a run is the resume mechanism.
- Ordering: group order (infra→core→satellite→build→ops→game→edge→web) with
  dependency topological precedence. Failure isolation: a failed project
  only skips its `required` dependents; unrelated branches continue.
- Rollback: `current` back to the previous release + restart + health probe;
  `keep_releases` pruning never deletes the active release.
- `manager: aapanel | manual` projects are **foreign**: status visibility
  (passive probe) only — never restarted, never overwritten.
- Deployment-context overrides (`orchestration.overrides.<project>`) may
  adjust manager / install_root / health_port / ports (per-name remap used
  by the preflight port check, for isolated environments where a declared
  port is already taken) / service_version / artifact_local_path.
  Production contexts apply no overrides.

**Preflight gate.** Deploy and rollback runs are gated by a one-pass
preflight (`internal/orchestrator/preflight.go::Preflight`): everything
checkable up front is checked in a single pass and reported graded —
**blockers** refuse the run (HTTP 400 carries the full report),
**warnings** pass with risk. Every finding carries the concrete
path/port/address. Checks (deploy mode): install-root / version-dir
writability (missing paths probe the nearest existing ancestor),
`current` must be a symlink, workdir, env-file existence/readability
(missing = warning — the startup script tolerates it; unreadable =
blocker — `set -e` would fail), Tinyvisor reachability + apiKey validity
(`/ping` unauthenticated, script list 401 = invalid key), declared-port
occupancy via bind test (a port held by the project's own running process
— Tinyvisor state `running` — is skipped, the pipeline restarts it), disk
space against the artifact size (`internal/artifact/artifact.go::EnsureDiskSpaceFor`;
unknown size degrades to warning), and declared dependency reachability
(required + out of scope + unreachable = blocker; required but started by
this run = warning; optional = warning). Rollback mode skips
artifact/disk checks. `dry_run` passes the same gate (blockers refused)
and records the report plus planned steps. The passing report is stored
on the run record (`Run.Preflight`). Read-only surface:
`GET /api/v1/deploy/preflight` (query `projects`/`group`), not subject to
single-flight. Refused runs leave no run record.

**Post-deploy smoke verification.** Health probes only prove the process is
alive and routes are mounted — not that the service can issue tokens, read
config, or reach its database. Manifests may declare `facts.smoke`
(`internal/manifest/manifest.go::SmokeCheck`): a list of real requests
(method, path, expected status, optional body assertions
`contains` / `json_field` / `json_not_empty`, optional fixed headers and
request body). Managed projects with `facts.smoke` run it right after the
health probe (`internal/orchestrator/smoke.go::runSmoke`); a failed smoke
is a failed deploy and takes the same auto-rollback path. Rollback and
cascade restarts re-verify smoke too. Projects without `facts.smoke` keep
health-only verification (not enforced). Secrets never live in manifests:
headers/body reference them as `${secret:<name>}`, resolved from the
credentials file's `smoke_secrets` map; missing secrets fail the smoke
with the name stated, and the preflight reports them as blockers
(`smoke-secrets` check). Smoke declarations (all endpoint facts verified
against code): batch 1 (2026-11-08) — hrpauth `POST /oauth/token`
(client_credentials, asserts `access_token`), yggdrasil profile query miss
(deterministic 404 `ProfileNotFoundException`), mca `/api/status` (asserts
`data.version` non-empty), Cupbearer `/api/v1/projects` (asserts
`data.projects` non-empty); batch 2 (2026-11-08) — haskinlib
`GET /texture/listpreview` (asserts `items` field), haskinproxy CSL
`GET /:username` miss (404 + `user not found`), winnerproxy `hasJoined`
probe (deterministic 204), sdkhandler `GET /api/build/status` (asserts
`state`; `X-Internal-Key` via `${secret:sdkhandler_internal_key}`),
maintenance-view `GET /api/session` (asserts `data.auth_required`; deeper
endpoints are session-cookie gated, which static smoke cannot hold).

**Game-side inclusion (2026-11-08).** Avalon and Cascade joined the
aggregate as `manager: tinyvisor` manifests. Avalon is declared in the
author-approved **no-helper form**: `gameHelperPath` empty with
`cascadeEnable: true`. Measured in the isolated environment
(`knowledge/0010`): the control plane comes up and serves `/v1/rooms`
(health endpoint of this form), but `/v1/health` is 503 by design — its
ready criterion is the game listener, which only opens after a successful
game start, which requires the helper. Starting a game was measured to
return `game: no simulation backend available`. Avalon smoke therefore
asserts the boundary itself (`/v1/health` must be 503 BACKEND_NOT_READY)
plus `/v1/rooms` functionality. Composed ports: Cascade edge 5123
(public) + management 8080 (loopback); Avalon game 5124 (loopback; code
default 5123 is yielded to the Cascade edge, per Cascade
`config.example.yaml` backend example) + control 5125. Cascade smoke is
`/api/rooms` non-empty (room snapshots flowing from Avalon prove the
chain; `/readyz` is 503 by design in this form and is documented as the
boundary, not used as smoke). Isolated-context port remapping uses
`overrides.ports` (preflight checks the remapped ports). Artifacts are
not built yet in the repositories (`build/` pending); preflight reports
that honestly at deploy time. Install roots are planned values
(`/www/wwwroot/avl`, `/www/wwwroot/casc`), first deployment decides.
Deployment pitfalls found by actually running the form (config/data tied
to the executable dir, first-run stdin prompt, Cascade migrations dir,
Tinyvisor restart-after-crash) are recorded in `knowledge/0010`.

**Auth and credentials.** Mutating endpoints (`POST /deploy/run`,
`POST /deploy/rollback`) require `X-Deploy-Token`; missing/unconfigured
token ⇒ 503 (fail-closed), wrong token ⇒ 401. Read-only deploy endpoints
follow the existing unauthenticated read surface (loopback-bound by
default). All secrets (`tinyvisor_api_key`, `deploy_token`, `github_token`,
`smoke_secrets`) live only in the local credentials file referenced by
`orchestration.credentials_path` — never in manifests, config, or the repo.
The ops panel (HA-Services-MaintenanceView) acts as the operation surface:
it holds the deploy token server-side and forwards it; the browser never
sees credentials.

**Artifacts.** This phase uses `artifact.source: local_path` exclusively;
`ci_release` is implemented (GitHub release download + sha256) but not
activated — no releases are published yet. When publishing is enabled,
assets must follow the hard naming convention
`<project>-linux-amd64` + `<project>-linux-amd64.sha256`
(`internal/manifest/validate.go::EffectiveAsset`).

## Consequences

- Cupbearer now has a dual runtime role (delivery + orchestration). The
  orchestration surface is additive; the 0001 GET surface is unchanged.
- Process lifecycle belongs to Tinyvisor exclusively (name-uniqueness,
  by-name idempotent upsert, atomic restart were added there for this).
  The orchestrator never spawns processes, never installs systemd units,
  and does not run as a daemon.
- Health probing is the orchestrator's job (Manifest-driven HTTP probes);
  Tinyvisor remains a generic liveness/restart supervisor.
- The aggregate must validate before any run; a single invalid manifest
  blocks runs (fail early, never partially).

## Evidence

- `internal/models/config.go::OrchestrationConfig` — config shape.
- `internal/config/migrations.go::configMigrations` — v1→v2 zero-regression migration.
- `internal/manifest/manifest.go::LoadAggregate` — manifest loading.
- `internal/manifest/validate.go::ValidateAggregate` — aggregate validation.
- `internal/orchestrator/orchestrator.go::StartRun` — run/rollback/status semantics.
- `internal/orchestrator/preflight.go::Preflight` — one-pass graded preflight gate.
- `internal/orchestrator/smoke.go::runSmoke` — post-deploy smoke verification.
- `internal/orchestrator/pipeline.go::switchSymlink` — symlink/script/probe mechanics.
- `internal/server/handlers_deploy.go::deployAuth` — fail-closed token auth.
- E2E record 2026-11-08 (local, unprivileged, isolated ports): deploy v1 →
  up-to-date re-run (PID unchanged) → upgrade v2 → rollback to v1 with
  binary fingerprint verified (`/proc/<pid>/exe` sha256). Full transcript:
  `knowledge/0008`.
