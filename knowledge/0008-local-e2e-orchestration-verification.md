# 0008 — Local End-to-End Orchestration Verification (2026-11-08)

## Status

Accepted (verification record; describes a past local run, not live state)

## Context

The bare-metal orchestration capability (0007) required a real closed-loop
verification before merge — not unit tests alone. This entry records what was
verified, how, and which parts were real versus local placeholders. The full
machine transcript was archived at `/tmp/ha-e2e/REPORT.md` (ephemeral; the
load-bearing evidence is reproduced below).

## What was verified

A full deploy → healthy → idempotent re-run → upgrade → rollback cycle against
a **real project** (HA-Services-MaintenanceView) with **real binaries**, driven
by the Deployer orchestrator through a real Tinyvisor instance:

1. **Deploy v1**: artifact staged (sha256 checked) → `releases/1.0.0/` →
   `current` symlink → Tinyvisor script upsert → restart → health probe green.
2. **Idempotent re-run**: outcome `up-to-date`, **no restart, PID unchanged**
   (645884 before and after).
3. **Upgrade to v2**: symlink `1.0.0 -> 1.1.0`, restart, health green.
4. **Rollback**: symlink `1.1.0 -> 1.0.0`, restart, health green; the running
   binary fingerprint switched back to v1.
5. **Auth fail-closed**: `POST /deploy/run` and `/deploy/rollback` without or
   with a wrong `X-Deploy-Token` → 401; orchestration-disabled instance → 503.
6. **Foreign semantics**: `manager: aapanel` projects reported `foreign`
   (read-only, passive probe only), never restarted or overwritten.
7. **Zero regression**: with `orchestration` empty the service auto-migrates
   config v1→v2 and the four pre-existing read-only endpoints behave
   identically; deploy endpoints answer 503.

## How it was verified

- Environment: single local host, isolated directories and ports — Tinyvisor
  on 18093 (fresh data dir), Deployer orchestrator on 18090, deployed service
  on 18091. No production host was touched.
- Tinyvisor script registered by name `ha-services-maintenance-view`,
  `workDir` = install root, content `#!/bin/sh\nset -e\nexec ./current/hasmv`.
- Run records were read back from `GET /api/v1/deploy/runs/:id`; persistent
  state from `data/deploy/state.json`.

Representative run records (abridged):

- Deploy v1 (`20261108-040502`): `deployed`, steps
  `artifact(sha256 7626101ecc64) → symlink((none) -> 1.0.0) →
  ensure-script(drift=true) → restart → health`.
- Idempotent re-run (`20261108-041030`): `up-to-date`, "version active, script
  aligned, health ok: no action".
- Upgrade v2 (`20261108-041122`): `deployed`, steps
  `artifact(sha256 deb5db64cbbd) → symlink(1.0.0 -> 1.1.0) →
  ensure-script(drift=false) → restart → health`.
- Rollback (`20261108-041229-rb`): `rolled-back`, steps
  `symlink(1.1.0 -> 1.0.0) → restart → health`, detail
  "rolled back 1.1.0 -> 1.0.0, healthy".

## Real binaries vs. local_path placeholders

**Real (verified by content fingerprint).** The deployed payloads were genuine
HA-Services-MaintenanceView binaries, built by `go build` from two distinct
source states of the real repository:

- v1 = commit `5007136` → sha256 `7626101ecc64…`
- v2 = working tree at build time → sha256 `deb5db64cbbd…`

Rollback was proven at the binary level, not just the symlink: the running
process image `/proc/<pid>/exe` hashed to `deb5db64cbbd…` (v2) before rollback
and to `7626101ecc64…` (v1) after, with a changed PID proving a real restart.

**Placeholder / not-yet-real.** These parts were deliberately local and must
not be read as production capability:

- `artifact.source` was `local_path` everywhere: the orchestrator copied a file
  already present on the local disk. The `ci_release` download path (GitHub
  release asset + sha256) is implemented but **not exercised** — no Release is
  published yet (author decision: no tags/releases this phase).
- The two "versions" were produced by rebuilding the same repository at two
  commits, not by a release pipeline. Version strings `1.0.0`/`1.1.0` were
  supplied via deployment-context overrides, not by repository tags.
- MySQL/Redis external dependencies were declared but not provisioned; the
  scoped runs did not require them.

## Consequences

- The closed loop works end-to-end with real binaries; the mechanism (version
  dirs + symlink + Tinyvisor restart + health gate + rollback) is proven.
- The `ci_release` source remains unverified against a real published release;
  when releases are enabled, the naming convention
  `<project>-linux-amd64` + `<project>-linux-amd64.sha256` must be confirmed
  against the actual publishing job.
- Health-probe cadence comes from each manifest (`interval_seconds`/`retries`);
  the MaintenanceView manifest inherits the compose-style 30s interval, so a
  single missed probe costs ~30s of wall time. Expected, not a defect.

## Evidence

- Run records and state under the ephemeral `/tmp/ha-e2e/deployer-run/data/deploy/`
  at verification time (transcript archived at `/tmp/ha-e2e/REPORT.md`).
- Binary fingerprints: `sha256sum` of the staged artifacts and of
  `/proc/<pid>/exe` before/after rollback (values above).
- `internal/orchestrator/orchestrator.go::StartRun` — the exercised entry point.
