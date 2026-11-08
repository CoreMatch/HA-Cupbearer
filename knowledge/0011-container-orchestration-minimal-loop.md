# 0011 — Container Orchestration: Design and First Closed Loop

## Status

Accepted (design + measured minimal closed loop + same-day digest-pinning /
cascade hardening, 2026-11-08)

## Context

Bare-metal orchestration (0007) is proven. This phase opens the container
runtime: the manifest `container` section un-freezes, a `runtime` selector
chooses the execution form per project, and the Deployer itself becomes the
first containerized service in the isolated environment (author-approved
minimal loop).

## Design decisions

- **`runtime: baremetal | container` selector** (default `baremetal`).
  Both sections coexist in the manifest (each declares how to run under its
  runtime); execution is mutually exclusive per project. Deployment contexts
  may override via `overrides.<p>.runtime` — production stays bare-metal
  while isolated environments run containers without touching repo manifests.
  The frozen `container.enabled` field is retired: a full-workspace scan
  (14 manifests) found every copy `false` before retirement; setting it
  `true` now fails validation pointing at the selector.
- **Image = version**: tag `<project>:<service_version>`, the analog of
  `releases/<v>/`. Image sources: `build` (local Dockerfile context,
  relative to the repo root), `local_image` (docker-save tarball with the
  same `.sha256` sidecar convention as binaries), `registry` (schema +
  validation only this phase; runtime pull/auth not implemented — preflight
  and deploy report it honestly as a blocker/failure).
- **Ports reuse `facts.ports`**: host port = declared port (remappable via
  `overrides.ports`), container port = same unless `container_ports`
  overrides by port name; `exposure: loopback` publishes on 127.0.0.1.
  Health probes and smoke assertions hit the published host port unchanged.
- **Volumes = the shared_data analog**: `container.volumes[].host_relative`
  under the project root (baremetal.install.root, overridable) survives
  image switches; the orchestrator creates host dirs, never touches content.
- **Reuse as-is**: run state machine / resume, Plan ordering + failure
  isolation (facts-based, mixed runtimes order together), health probing,
  smoke verification incl. `${secret:}` handling, preflight framework,
  single-flight, dry-run, duplicate resolution (0010-era rules).
- **Changed semantics**: symlink switch → container recreate (same name);
  rollback → recreate on the **recorded image digest** (see below);
  `keep_releases` → keep 5 image tags + active; supervision moves from
  Tinyvisor to docker `--restart unless-stopped` (container projects never
  touch Tinyvisor).
- **Rollback is pinned by image digest, never by tag** (hardened 2026-11-08,
  author-mandated): tags are movable references — re-tagging a version would
  silently land a tag-based rollback on different content, with no error, the
  worst kind of silent failure. Run records store the content digest
  (`ProjectResult.ActiveImageID` / `ProjectState.ImageID`); auto-rollback
  recreates from the pre-switch container's digest; one-click rollback
  resolves the target version's digest from run records
  (`Store.ImageIDForVersion`) and re-verifies existence at execution time.
  `docker create` accepts a sha256 ID as the image reference, so the
  recreated container's reference *is* the digest. Missing digest → explicit
  "目标镜像已不存在" refusal; the orchestrator never falls back to tag
  addressing and never rolls onto something else. Idempotency also compares
  digests (not reference strings), so digest-created containers are correctly
  judged up-to-date. Version display for digest-created containers resolves
  by matching tags to the digest (tags remain the human-readable names).
- **Cascade rollback is real for container projects** (hardened 2026-11-08,
  author-mandated; the earlier "accept the parameter but only roll back one"
  degradation is gone): dependents join the rollback run regardless of
  runtime — container dependents are recreated on their own current digest
  (they keep their version, only re-establish the connection to the rolled-back
  upstream), bare-metal dependents are restarted via Tinyvisor
  (`restartDependentAny` dispatches by runtime). Both sides must return to
  healthy including smoke; a dependent failure is reported as such.
- **Orphan handling (new)**: every managed container carries labels
  `ha-services-deployer.project` / `.managed-by`; a run-start reconcile
  reports labeled containers whose project left the manifests —
  **report-only by default** (`orchestration.container.orphan_policy:
  report | remove`), author-approved.
- **Credentials never in manifests — code-enforced**: `LoadFile` scans the
  raw YAML tree and rejects credential-like keys with literal values,
  known token shapes (GitHub/AWS/JWT/private-key blocks), long hex/base64
  blobs, and any `${...}` that is not `${secret:<name>}`. Comments do not
  stop people; the loader does.
- **Driver**: shells out to the docker CLI (`internal/docker`), no SDK
  dependency; respects `DOCKER_HOST` / CLI context. Socket discovery for
  `orchestration.container.docker_host` (empty default): `DOCKER_HOST` env
  → `$XDG_RUNTIME_DIR/docker.sock` (rootless user unit) →
  `/var/run/docker.sock`. Nothing hardcoded.

## Unprivileged verification (the default means)

Rootless Docker via systemd **user** unit: `dockerd-rootless-setuptool.sh
install` → `systemctl --user start docker` (prerequisites measured present:
subuid/subgid, rootlesskit, slirp4netns, kernel unprivileged overlayfs —
no fuse-overlayfs needed). System dockerd untouched; other sessions' docker
usage unaffected. Storage `overlayfs`, security `rootless`. Verified with a
scratch-image build + loopback publish before the loop, and by the loop
itself running entirely as uid 1000.

## Minimal closed loop (measured, isolated environment)

Service: **ha-services-deployer itself** (repo exclusively owned; manifest
facts pre-existing; `FROM scratch` + static binary, zero registry pulls).
Form: `runtime: container` via override, port 18080→18096 remap, volume
`/tmp/ha-e2e/hasdc/data → /data` seeded with config + manifest.json.

1. **Deploy v1.0.0** (run `20261108-101730`): image build → volume dir →
   create `740a3b6e8535` → start → health `/healthz` 200 → smoke
   `/api/v1/projects` non-empty. All steps OK.
2. **Idempotent re-run** (run `20261108-102329`): outcome `up-to-date`,
   step `activate: skipped (up-to-date, container 740a3b6e8535 not
   recreated)`; container ID and image digest byte-identical before/after —
   the container was provably not recreated.
3. **Failure + auto-rollback** (run `20261108-102506`): v1.0.1 image built
   from an exit-at-start binary → container `8378dce2422d` fails health
   (connection refused ×3) → auto-rollback recreated on
   `ha-services-deployer:1.0.0` (container `61c3e4755fb6`, image id
   `eb7c0873853b`); run record carries the full before/after evidence
   (`inspect-before` → failed `create` → `auto-rollback`), and `/healthz`
   answered 200 afterwards.

## Hardening loop — digest pinning + cascade (measured, 2026-11-08)

Both boundaries flagged above were closed the same day with measured drills
in the same isolated environment (rootless docker, Deployer as its own
deploy target, plus a throwaway dependent `drill-mirror`).

1. **Re-tag drill (tags lie, rollback must not care)**: with the container
   healthy on digest `eb7c0873853b`, tag `1.0.0` was re-pointed at the bad
   image (`7b1845b396fa`), the build context was swapped to an
   exit-at-start binary, and a same-version deploy was triggered (run
   `20261108-110259`): build produced bad content `fb7e8c48a875` under tag
   `1.0.0` → container `71361305a463` failed health (connection refused ×3)
   → auto-rollback recreated **on digest `eb7c0873853b`** (container
   `cc238fc6b0f8`; `docker container inspect` shows `Image` and
   `Config.Image` both equal to the digest), while `docker images` still
   showed `1.0.0 → fb7e` (bad). Running content ≠ tag target proves the
   rollback never consulted the tag; `/healthz` answered 200 afterwards.
   (The pre-hardening code would have skipped this rollback entirely —
   same-version guard — or, cross-version, recreated from the lying tag.)
2. **Cascade drill (dependents really follow)**: `drill-mirror` (container,
   `depends_on: ha-services-deployer required`, host port 18097) deployed;
   upstream upgraded `1.0.0` (digest `3eba28542729`) → `1.0.1` (digest
   `8c65cebe4058`); then `POST /api/v1/deploy/rollback
   {"project":"ha-services-deployer","cascade":true}` (run
   `20261108-111230-rb`, order `[ha-services-deployer, drill-mirror,
   ha-services-maintenance-view]`): upstream recreated on digest
   `3eba28542729` resolved from run records (`c3710ed8624e` →
   `0ce695502c4f`, healthy + smoke); container dependent recreated on its
   **own** current digest `0635e4d08d03` (`f34f5d780cf5` → `539821018bf6`,
   healthy); bare-metal dependent (`ha-services-maintenance-view`)
   restarted via Tinyvisor and healthy — its smoke then failed on a
   pre-existing endpoint mismatch (running binary serves the SPA fallback
   for `/api/session`), honestly reported; unrelated to the rollback path.
   Both container endpoints answered 200 after the run.
3. **Refusal paths (unit-measured)**: digest gone → explicit "目标镜像已
   不存在" with no container created (auto and one-click); version without
   a recorded digest → rollback refused. Also landed along the way: run-ID
   same-second collision avoidance (run records are the digest evidence
   chain and must never silently overwrite each other).

Unit coverage in `internal/orchestrator/container_test.go`: retag
unaffected, digest-gone refusals (auto + one-click), unrecorded-version
refusal, two-container cascade recreate.

## Caveats carried forward

- `registry` source: schema/validation only; pull + auth later.
- Image builds on the drill host are not byte-reproducible (same inputs →
  new digest per build), so an idempotent re-deploy after an out-of-band
  rebuild recreates the container once even when content is functionally
  identical. Digest comparison is the honest response; reproducibility is a
  build-pipeline concern, not an orchestrator one.
- Two CLI bugs were found and fixed during the loop: bare `docker inspect`
  resolves the same-named image (must use `container inspect`), and the
  state field is `.State.Status`, not `.State.State`. Both are documented
  in the repository README ("运维注意事项（docker CLI 踩坑）") — script
  writers do not read knowledge files.

## Evidence anchors

- Code: [xref] `internal/manifest/manifest.go::LoadAggregate` (duplicate
  resolution unchanged), `internal/manifest/manifest.go::scanCredentials`,
  `internal/manifest/validate.go::validateContainer`,
  `internal/orchestrator/container.go::deployContainer`,
  `internal/orchestrator/container.go::autoRollbackContainer` (digest-pinned),
  `internal/orchestrator/container.go::executeRollbackContainer` (digest +
  cascade), `internal/orchestrator/container.go::restartDependentAny`,
  `internal/orchestrator/state.go::Store.ImageIDForVersion`,
  `internal/orchestrator/orchestrator.go::Orchestrator.Rollback` (digest
  resolution + refusal), `internal/docker/docker.go::DetectHost`.
- Runs (isolated state dir `/tmp/ha-e2e/deployer-run/data/deploy/runs/`):
  first loop `20261108-101730.json`, `20261108-102329.json`,
  `20261108-102506.json`; hardening loop `20261108-110259.json` (re-tag
  drill), `20261108-110901.json` (drill-mirror first deploy),
  `20261108-111045.json` (upstream upgrade), `20261108-111230-rb.json`
  (cascade rollback).
