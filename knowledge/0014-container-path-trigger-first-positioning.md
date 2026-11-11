# 0014 — Container path positioning: trigger-first interaction, management kept as opt-in capability

## Status

Accepted (author directive 2026-11-11; trigger implemented + measured)

## Context

0011 opened the container runtime with a full per-container pipeline
(image-as-version, recreate, health, smoke, digest-pinned rollback); 0012
made `registry` sources really pull; 0013 published all ten images as
multi-arch manifest lists with the manifests pinning list digests. The
author then set the positioning for the container path (2026-11-11, two
directives, final wording recorded here):

1. Cupbearer's design intent is **orchestrating microservices without
   containers** — bare-metal orchestration (version dir + symlink,
   Tinyvisor restarts, health, rollback) is the body.
2. The container path's **default interaction is a one-click trigger**:
   hand the whole container orchestration out for execution and report the
   result truthfully.
3. The already-implemented **container-level management is kept** — not
   removed, not demoted in code. It is an opt-in capability
   (`runtime: container`), not the main line. An intermediate wording
   ("narrow the container path to trigger-only, remove management") was
   superseded by directive 3 before any code was removed; nothing was
   deleted, so there is nothing to restore.

## Decision

**One-click trigger** (`internal/orchestrator/container_trigger.go::TriggerContainerStack`,
endpoint `POST /api/v1/deploy/container/trigger`,
`internal/server/handlers_deploy.go::PostDeployContainerTrigger`):

- Token-gated fail-closed like run/rollback; shares the single-flight
  constraint; recorded as a run of type `container-trigger`, queryable via
  the existing runs endpoints.
- Collects each manifest's **published registry image reference**
  (`internal/orchestrator/container_trigger.go::publishedImageRefs`,
  digest-first via `internal/orchestrator/container.go::registryPullRef`);
  `build`/`local_image` sources are local, not published, and stay out of
  the trigger's scope.
- Generates a compose override
  (`internal/orchestrator/container_trigger.go::renderComposeOverride`,
  written under `<state_dir>/container-trigger/`) that pins
  template services sharing a project name to those references, then runs
  `docker compose -f <template> -f <override> up -d`
  (`internal/docker/docker.go::CLI.ComposeUp`) and snapshots
  `docker compose ps` (`internal/docker/docker.go::CLI.ComposePs`).
- The template (default `deploy/docker-compose.yml`, configurable via
  `orchestration.container.compose_file` in
  `internal/models/config.go::ContainerRuntimeConfig`) carries the whole
  env/config wiring; manifests contribute image references only. Mismatches
  are reported in run notes, never silent: published reference without a
  template service, or template service without a published reference
  (the latter runs on the template's own semantics, e.g. local build).
- The orchestrator does **not** do per-container creation, health polling,
  or rollback on this path — compose owns the stack lifecycle; the
  Cupbearer triggers and reports truthfully (compose output, exit status,
  per-service snapshot).

**Container-level management stays as implemented** (0011/0012/0013):
`internal/orchestrator/container.go::deployContainer`,
`internal/orchestrator/container.go::executeRollbackContainer`, orphan
reconciliation, digest-pinned rollback — all reachable via
`runtime: container` and unchanged by this entry.

**Manifest semantics**: the `container` section carries "what to trigger"
(published image reference, including the manifest-list digest) plus the
execution declarations consumed by the opt-in management path.
`container.enabled` remains retired/false everywhere; the `runtime`
selector keeps its meaning.

## Measured evidence (2026-11-11)

- Unit: override generation pins digest-first references and skips
  non-registry sources; refusal paths (no published references, missing
  template, docker unreachable) fail loud; compose failure lands as a
  failed run with the output recorded
  (`internal/orchestrator/container_trigger_test.go`).
- Real trigger run: full platform stack brought up via the endpoint — see
  the run record of type `container-trigger` in the verification report
  (compose up ok, digest-pinned images, per-service snapshot reported).

## Boundaries / open items

- The trigger does not manage teardown (`compose down`); stopping the
  stack is a compose-side operation until a teardown counterpart is asked
  for.
- Services whose manifests carry no published registry reference run on
  the template's own semantics (e.g. the Cupbearer's own `build` source);
  this is reported in run notes, not hidden.
