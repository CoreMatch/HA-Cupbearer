# 0013 — Multi-arch publication: ten images as manifest lists, pinned by list digest

## Status

Accepted (published + measured end-to-end, 2026-11-11)

## Context

0012 closed the registry-pull loop while the ten platform images were still
single-arch `linux/amd64`, and left an explicit open item: a multi-arch
manifest list carries a list digest differing from per-arch image digests;
re-verify the reconciliation rule before publishing multi-arch. This entry
records that publication and the verification.

## Decision

1. **All ten images are multi-arch.** Each is an OCI image index (manifest
   list) with `linux/amd64` + `linux/arm64` members, built with the
   `ha-multiarch` docker-container-driver builder via
   `--platform linux/amd64,linux/arm64 --push` and the established
   reproducibility flags (`--provenance=false --sbom=false`,
   `SOURCE_DATE_EPOCH=0`). Tags keep the commit-tracking convention
   (`<service_version>-<short SHA>`, bare short SHA where no version
   exists) plus `latest`.
2. **Manifests pin the manifest-list digest.** Every
   `container.image.digest` in the eight registry-source manifests holds the
   digest of the image index, not a per-platform child digest. A child
   digest would resolve to one fixed platform only; the list digest lets
   each host resolve its own platform from one pinned reference.
3. **Reconciliation is unchanged and verified.** Pulling
   `repo@sha256:<list digest>` stores one image record whose `RepoDigests`
   contains exactly that list digest (the platform variants nest under the
   record), so `internal/orchestrator/container.go::containsRepoDigest`
   passes without modification; digest-first reference assembly
   (`internal/orchestrator/container.go::registryPullRef`) is
   digest-format-only and architecture-agnostic. Measured 2026-11-11 with
   `limeight/ha-bs2ha@sha256:4f8dcf41…` pulled for both platforms.

## Build facts (measured 2026-11-11)

- arm64 members are built under QEMU user-mode emulation on the amd64
  build host; per-image wall time ran ~4–7 min, dominated by the emulated
  arm64 leg (the native amd64 leg finishes within the first ~1–2 min) —
  within the expected 5–15x emulation envelope. This is expected and is
  not a reason to drop the arm64 leg.
- Individual builds can fail under emulation; failures are triaged
  individually — no silent skip, no single-platform fallback.
- One failure observed this round was emulation-unrelated: dependency vuln
  patches (2026-11-06) raised `go.mod` to `go 1.26.10` in seven repos while
  the Dockerfiles pin older `GO_VERSION` base images; with the official
  golang images' `GOTOOLCHAIN=local`, `go mod download` fails with
  `go.mod requires go >= 1.26.10`. Fix without touching the repos:
  `--build-arg GO_VERSION=<go.mod value>` (the Dockerfiles declare
  `ARG GO_VERSION`, overridable by design).
- Build contexts were clean `git archive` exports of each repo HEAD, so a
  dirty worktree (parallel knowledge-anchor edits present at the time)
  could not leak into the images.

## Smoke evidence (2026-11-11)

Both rounds pulled every image **by manifest-list digest** and ran the full
ha-platform compose stack (9 services; external `mysql-smoke` /
`redis-smoke` on a dedicated network), then re-verified per container:

- amd64 round: 9/9 healthy; every container `uname -m` = `x86_64`;
  spot checks: hrpauth `/health` ok, mca-server `/api/status` reporting
  1.11.0, Cupbearer `/healthz` ok.
- arm64 round: 9/9 healthy under QEMU; every container `uname -m` =
  `aarch64`; same spot checks pass.
- `limeight/ha-bs2ha` (not in the compose stack): run on both platforms;
  the binary executes (expected TUI error without a TTY), `uname -m`
  `x86_64` / `aarch64` respectively.

## Boundaries / open items

- `container.enabled` stays `false` in every manifest; nothing changes for
  bare-metal orchestration.
- The arm64 smoke ran on an amd64 host under QEMU (`--platform
  linux/arm64`); a first deploy on real arm64 hardware should still watch
  startup timing, but no extra code path is involved — the pull is the same
  digest-first pull.
