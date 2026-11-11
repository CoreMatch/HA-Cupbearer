# 0012 — Registry Image Source: Live Pull, Digest-First Pinning, Host-Login Auth

## Status

Accepted (implemented + measured end-to-end, 2026-11-11)

## Context

0011 opened the container runtime with `build` and `local_image` sources;
`registry` was schema-and-validation only — a deploy attempt failed with
"not implemented this phase". After the ten platform images were really
published to Docker Hub (`limeight/ha-*`, each manifest pinned to a digest),
that stub meant the published images had no consumer: "published but
unusable". This entry closes the loop.

## Decision

`container.image.source: registry` now performs a real pull at deploy time,
with three hard rules:

1. **Digest-first pull.** When the manifest declares `container.image.digest`,
   the pull reference is `repo@sha256:...` — content is pinned at fetch time.
   The "pull by tag, compare afterwards" pattern is rejected by design: tags
   are movable, and comparing after the content already landed is a post-hoc
   salvage, not a pin. Tag-only pull is the fallback when only a tag is
   declared (schema allows, not recommended).
   (`internal/orchestrator/container.go::registryPullRef`)
2. **Post-pull digest reconciliation, loud failure.** After the pull, the
   image's actual `RepoDigests` must contain the declared digest; mismatch
   fails the project immediately — no container is created, no fallback to
   any other content. (`internal/orchestrator/container.go::containsRepoDigest`,
   registry case of `::deployContainer`)
3. **Authentication follows the host docker login.** The pull shells out to
   the docker CLI, which reads the host's `~/.docker/config.json` (or its
   configured credential helper); under rootless that is the orchestrator
   user's own login. The Cupbearer holds no registry credentials, has no
   config key for them, and hardcodes no token. Pull failures are classified
   into readable causes — 401 authentication, 403 access denied, 404 not
   found, 429 rate limit — never swallowed into one "pull failed".
   (`internal/docker/docker.go::Pull`, `::ClassifyPullError`)

After a successful pull the image is tagged `<project>:<service_version>`
(image-as-version, same semantics as `build`/`local_image`); idempotency,
rollback, and prune keep operating on that tag plus the content digest,
unchanged from 0011.

Preflight for `registry` sources reports the resolved reference and notes
that the pull happens at deploy time (preflight does not mutate the image
store); the docker-daemon reachability check still gates any run containing
container projects. (`internal/orchestrator/preflight.go::checkContainerImage`)

## Measured evidence (2026-11-11)

- Unit: digest-first reference assembly, mismatch refusal (no container
  created), tag-only fallback, pull-error propagation
  (`internal/orchestrator/container_test.go` registry tests); error
  classification table (`internal/docker/docker_test.go`).
- End-to-end: `ha-services-maintenance-view` temporarily switched to the
  container runtime **via config override only** (manifest untouched,
  `container.enabled: false` preserved); all local copies of the image
  deleted first; the orchestrator then pulled
  `limeight/ha-services-maintenance-view@sha256:c350e9b0…` from Docker Hub,
  passed digest reconciliation, created and started container `0e693c0f0650`,
  health probe and the `/api/session` smoke assertion passed; the running
  container's image ID (`sha256:c350e9b0…`) equals the manifest-declared
  digest — pinning proven. The container was removed and the override
  discarded afterwards; nothing test-specific remains in any commit.

## Boundaries / open items

- The declared digest must be the registry **manifest digest** as pushed.
  Since 2026-11-11 the ten images are multi-arch manifest lists and the
  manifests pin the **list** digest; pulling `repo@sha256:<list digest>`
  yields an image record whose `RepoDigests` contains that list digest, so
  the reconciliation rule above passes unchanged — measured, see 0013.
- Private-registry support inherits whatever the host docker login supports;
  there is deliberately no per-registry credential plumbing in this service.
