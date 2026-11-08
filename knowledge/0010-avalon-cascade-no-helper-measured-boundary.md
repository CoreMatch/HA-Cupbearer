# 0010 — Avalon + Cascade No-Helper Form: Measured Capability Boundary

## Status

Accepted (records measured state, 2026-11-08)

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

- **Avalon config/data is tied to the executable dir**: `BasePath()` =
  `filepath.Dir(os.Executable())` (`internal/util/file`), so
  `data/Config.json` resolves inside the release dir
  (`releases/<v>/data/`), not the install root. The pipeline stages only
  the binary — Avalon auto-initializes default config on first boot
  (`cascadeEnable:false`), silently ignoring any config seeded at the
  install root. The E2E seeded `releases/1.0.0/data/Config.json`
  manually. An env pin exists (`avalonhelper.basePath`,
  `GetOrEnv` in `internal/util/systemprops`) but the dotted name cannot
  be set by the shell-sourced env-file mechanism; a script/render change
  would be needed to use it. **Open item**: config seeding for Avalon in
  the pipeline (or artifact = tarball of the dist layout).
- **Avalon first-run prompt blocks on stdin** ("WEB panel to help you
  initialize? Yes/No", `internal/app/initialization.go` firstRunPrompt,
  gated by the `initStart` setting persisted in `data/Settings.bin`).
  Unattended deploys must pre-seed `Settings.bin` (initStart=false) or
  answer once via console; otherwise boot blocks before the auto-start.
- **Cascade needs `internal/database/migrations/` relative to CWD**
  (`internal/database/migrate.go`); a bare binary fails with
  `migrations directory not found`. The artifact must bundle the
  migrations dir (tarball or post-stage copy). **Open item** likewise.
- **Tinyvisor restart-by-name after a crash**: when the supervised
  process exited (crash loop), `POST /api/scripts/by-name/<n>/restart`
  answered 400 `session not found` while the id-based `/start` worked.
  Deploy pipeline restarts can hit this for crash-looping services;
  reported for the Tinyvisor side to address.

## Evidence

- Run records: `data/deploy/runs/20261108-072605.json` (first attempt:
  avalon health failed — config not read, cascade skipped by failure
  isolation), `20261108-074034.json` (avalon deployed + 2 smokes ok,
  cascade restart 400), `20261108-074432.json` (avalon up-to-date,
  cascade deployed + smoke ok) — isolated environment state dir.
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
