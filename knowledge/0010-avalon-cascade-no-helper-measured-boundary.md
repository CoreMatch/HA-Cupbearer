# 0010 — Avalon + Cascade No-Helper Form: Measured Capability Boundary

## Status

Accepted (records measured state, 2026-11-08; all four deployment
pitfalls below FIXED and re-measured 2026-11-08)

## Context

Avalon (game server) and Cascade (edge proxy) joined the deploy aggregate
as `manager: tinyvisor` manifests in the author-approved **no-helper
form**: Avalon runs with `gameHelperPath` empty (no Java engine helper)
and `cascadeEnable: true` (Cascade backend contract + `/v1` control
plane). Both were then really deployed in the isolated environment
(`/tmp/ha-e2e`, Tinyvisor-supervised, `local_path` artifacts built from
source, no releases published) to turn the declared boundary into
measured conclusions. Isolated-context ports: Avalon 5124 (game, declared
value; code default 5123 is yielded to the Cascade edge) + 5125
(control); Cascade remapped via `overrides.ports` to 15123 (game) +
15153 (http) because 5123/8080 are occupied on the dev machine.

## What works (measured)

- **Tinyvisor supervision**: both processes registered, started, restarted
  and kept alive (`restart_policy: unless-stopped`). Deploy pipeline ran
  end-to-end for both: artifact+sha256 → version dir → symlink → script
  upsert → restart → health → smoke; run record `20261108-074432`
  (avalon up-to-date after `20261108-074034` deployed it, cascade
  deployed with smoke ok).
- **Avalon control plane**: `GET /v1/rooms` → 200 with the single-room
  snapshot (`backendId`, room `GW01`, `inGame:false`). This is the health
  endpoint of the no-helper form. Console commands work over the
  Tinyvisor terminal (stdin).
- **Game start is unavailable — measured, not assumed**: Avalon
  auto-runs `start` at boot (`internal/app/bootstrap.go` DefStartCommand)
  and the run was repeated explicitly via the console; both times it
  logged `start: game: no simulation backend available`
  (`internal/command/start.go` → `game.ErrNotImplemented`,
  `internal/game/seam.go`). The game listener (port 5124) is only opened
  by `startnetservice` after a successful start, so it never opens in
  this form.
- **Avalon `/v1/health` = 503 by design** in this form: its ready
  criterion is "game listener established"
  (`internal/net/gateway/control.go` handleHealth,
  `internal/net/service.go` SetReady). The manifest smoke asserts exactly
  this 503 + `BACKEND_NOT_READY` as the form boundary — if a helper is
  ever attached, that smoke fails and forces the manifest to be updated
  (no silent drift).
- **Edge→Avalon chain is live at the data level**: Cascade fetches room
  snapshots from Avalon's control plane every 2s; measured
  `GET :15153/api/rooms` → room `GW01 (Avalon-E2E)` and
  `/api/backends` → `roomCount:1` with a fresh `lastSnapshot`.
  Cascade `/healthz` → 200.

## What does not work in this form (measured)

- **Starting a game**: impossible without the simulation backend (above).
  Client game connections through the edge cannot enter a match.
- **Avalon `/v1/health`**: 503 (above) — anything that uses it as the
  health signal (including Cascade's backend health probe) sees the
  backend as down.
- **Cascade `/readyz`**: 503 `no healthy backend available` — by design:
  `Ready()` requires the default backend `Healthy`
  (`internal/backend/directory.go`), and backend health is Avalon's
  `/v1/health` (`internal/backend/client.go` Health → `/v1/health`,
  requires `status:"ready"`). The chain works for room routing data, but
  "ready" in Cascade's sense includes game readiness, which this form
  cannot provide. Hence the manifest uses `/api/rooms` (data flow) as
  smoke instead of `/readyz`.

## What is missing to unlock game start

1. **Avalon-Helper** jar (Java engine shell) built with a matching helper
   protocol version (version-gated; see Avalon knowledge/0011) + JDK/JRE
   17 `java` on PATH (or `javaPath` configured).
2. `gameHelperPath` (and optional `gameHelperArgs`) set in Avalon's
   `data/Config.json` — then `loadSimulation` attaches the external
   backend (`internal/app/simulation.go`), `start` can succeed,
   `startnetservice` opens the game listener, `/v1/health` goes ready,
   Cascade marks the backend healthy and `/readyz` passes.
3. Manifest updates in the same change: Avalon health/smoke flip to the
   with-helper semantics (the 503-boundary smoke must go), Cascade smoke
   may move to `/readyz`.

## Deployment pitfalls found by running it (first-mover notes)

All four were FIXED and re-measured in the isolated environment on
2026-11-08. Kept here as the record of what broke and how.

- **FIXED — Avalon config/data tied to the executable dir**: `BasePath()`
  fell back to `filepath.Dir(os.Executable())` (`internal/util/file`),
  so under the `releases/<v>/` layout every upgrade reset config, maps,
  mods and runtime data. Fix: `baremetal.install.shared_data` manifest
  field (Cupbearer `feat/shared-data-dir`). The orchestrator creates
  `<root>/shared/` and never touches it on version switch/prune; the
  rendered startup script injects the absolute path via
  `exec env "avalonhelper.basePath=<root>/shared" ./current/<binary>`
  (the dotted name cannot go through env_file/shell assignment, so it is
  written into the script directly). Avalon's manifest declares
  `shared_data: shared`. Measured: deploy v1.0.0 → write marker +
  record checksums in `shared/` → upgrade to v1.0.1 → marker present,
  `Config.json`/`Settings.bin` byte-identical, `releases/1.0.1/` holds
  only the binary (runs `20261108-084109`, `20261108-084434`).
- **FIXED — Avalon first-run prompt blocked on stdin**: the "WEB panel
  to help you initialize? Yes/No" prompt (`internal/app/initialization.go`
  firstRunPrompt) read stdin unbounded. Tinyvisor starts scripts under a
  PTY, so stdin *is* a terminal but nobody answers — unattended boot hung
  (measured: `[ -t 0 ]` → true under Tinyvisor; `SetReadDeadline` →
  "file type does not support deadline" on the PTY). Fix (Avalon
  `fix/unattended-firstrun`): non-terminal stdin skips the prompt;
  terminal stdin uses a bounded wait (`WaitLicenseBounded`, 30s); both
  paths take the default (no WEB-panel init), log one line and persist
  `initStart=false` so it never asks again for that data dir. Measured:
  prompt shown 08:41:11 → timed out 08:41:41 → boot completed; after the
  v1.0.1 upgrade no prompt at all.
- **FIXED — Cascade needed `internal/database/migrations/` on disk**:
  `RunMigrations` resolved the dir CWD-relative, so a bare binary failed
  with `migrations directory not found`. Chosen fix (Cascade
  `fix/embed-migrations`): embed the SQL into the binary
  (`go:embed migrations/{sqlite,mysql}/*.sql` + golang-migrate `iofs`)
  instead of bundling the dir in the artifact or copying at stage time —
  the single-binary artifact contract (sha256, atomic rename, staging,
  disk preflight) stays untouched and migrations can never be missing or
  out of sync with the binary. Measured: deleted the migrations dir and
  the DB from the install root, redeployed the bare binary →
  `[migrate] migrations completed successfully`, tables created, health +
  edge-rooms smoke ok (run `20261108-084629`).
- **FIXED — Tinyvisor restart-by-name after a crash**: a crashed process
  has its session cleaned up immediately while the app-level status flips
  only on the next 2s watch tick; in that window restart tried to stop a
  gone session and answered 400 `session not found` (id-based `/start`
  worked because it never stops). Fix (Minivisor
  `fix/restart-crashed-session`): `ErrSessionNotFound` sentinel; stop/kill
  treat a gone session as already stopped (idempotent); the auto-restart
  path skips when the script is already running again. Measured on the
  live E2E instance: kill → by-name restart → old binary 400, fixed
  binary 200 with a fresh PID. Regression tests in
  `backend/restart_test.go` (crashed-session window, stopped state,
  idempotent stop).

## Evidence

- Run records: `data/deploy/runs/20261108-072605.json` (first attempt:
  avalon health failed — config not read, cascade skipped by failure
  isolation), `20261108-074034.json` (avalon deployed + 2 smokes ok,
  cascade restart 400), `20261108-074432.json` (avalon up-to-date,
  cascade deployed + smoke ok), `20261108-084109.json` (avalon 1.0.0 on
  shared layout, ensure-shared step, prompt timeout measured),
  `20261108-084434.json` (avalon 1.0.1 upgrade, shared data intact),
  `20261108-084629.json` (cascade bare binary, embedded migrations) —
  isolated environment state dir.
- Measurements: `/v1/rooms` 200 snapshot; `/v1/health` 503
  `BACKEND_NOT_READY`; console `start` → `game: no simulation backend
  available`; Cascade `/healthz` 200, `/readyz` 503
  `cascade_backend_unavailable`, `/api/rooms` → GW01, `/api/backends` →
  `healthy:false, roomCount:1`.
- Code anchors: [xref] `internal/net/gateway/control.go::handleHealth`,
  [xref] `internal/command/start.go::registerStartGame`,
  [xref] `internal/game/seam.go::ErrNotImplemented` (Avalon);
  [xref] `internal/backend/directory.go::Ready`,
  [xref] `internal/backend/client.go::Health`,
  [xref] `internal/api/proxy_http.go` readyz/backends (Cascade).
