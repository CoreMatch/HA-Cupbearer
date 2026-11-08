# 0011 — Container Orchestration: Design and First Closed Loop

## Status

Accepted (design + measured minimal closed loop, 2026-11-08)

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
  rollback → recreate on previous image tag; `keep_releases` → keep 5 image
  tags + active; supervision moves from Tinyvisor to docker
  `--restart unless-stopped` (container projects never touch Tinyvisor).
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

## Caveats carried forward

- Rollback targets an image **tag**; if a same-version rebuild re-points the
  tag, rollback restores the re-pointed image (run records carry image IDs
  in step details for audit). Image-ID pinning is a later hardening.
- `registry` source: schema/validation only; pull + auth later.
- Rollback `cascade: true` for container projects degrades to single-project
  rollback (dependent restart across runtimes is a later design item).
- Two CLI bugs were found and fixed during the loop: bare `docker inspect`
  resolves the same-named image (must use `container inspect`), and the
  state field is `.State.Status`, not `.State.State`.

## Evidence anchors

- Code: [xref] `internal/manifest/manifest.go::LoadAggregate` (duplicate
  resolution unchanged), `internal/manifest/manifest.go::scanCredentials`,
  `internal/manifest/validate.go::validateContainer`,
  `internal/orchestrator/container.go::deployContainer`,
  `internal/docker/docker.go::DetectHost`.
- Runs: `data/deploy/runs/20261108-101730.json`,
  `20261108-102329.json`, `20261108-102506.json` (isolated state dir).
