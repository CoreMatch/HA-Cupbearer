# 0007 — Bare-Metal Orchestration Scope and Boundary

## Status

Accepted (records current state)

## Context

Deployer was a read-only delivery service (0001). Since config v2 it also
carries an opt-in bare-metal orchestration capability that drives Tinyvisor
on the target machine: HA-Services-Deployer is the orchestrator, Tinyvisor
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
(schema, duplicate project, dependency existence, dependency cycle,
cross-project port conflict, enum checks). Foreign projects
(`manager != tinyvisor`) may record `"unknown"` for informational fields.

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
  adjust manager / install_root / health_port / service_version /
  artifact_local_path. Production contexts apply no overrides.

**Auth and credentials.** Mutating endpoints (`POST /deploy/run`,
`POST /deploy/rollback`) require `X-Deploy-Token`; missing/unconfigured
token ⇒ 503 (fail-closed), wrong token ⇒ 401. Read-only deploy endpoints
follow the existing unauthenticated read surface (loopback-bound by
default). All secrets (`tinyvisor_api_key`, `deploy_token`, `github_token`)
live only in the local credentials file referenced by
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

- Deployer now has a dual runtime role (delivery + orchestration). The
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
- `internal/orchestrator/pipeline.go::switchSymlink` — symlink/script/probe mechanics.
- `internal/server/handlers_deploy.go::deployAuth` — fail-closed token auth.
- E2E record 2026-11-08 (local, unprivileged, isolated ports): deploy v1 →
  up-to-date re-run (PID unchanged) → upgrade v2 → rollback to v1 with
  binary fingerprint verified (`/proc/<pid>/exe` sha256). Full transcript:
  `knowledge/0008`.
