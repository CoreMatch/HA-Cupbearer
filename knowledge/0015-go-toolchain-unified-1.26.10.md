# 0015 — Go toolchain unified at 1.26.10: no per-service GO_VERSION overrides

## Status

Accepted (implemented + measured, 2026-11-12)

## Context

The 2026-11-06 dependency vuln patches raised `go.mod` to `go 1.26.10` in
seven repos while the Dockerfiles pinned older base images (1.24.0–1.26.4);
with the official golang images' `GOTOOLCHAIN=local`, builds failed at
`go mod download` unless each build passed `--build-arg GO_VERSION=…`
(recorded as a pitfall in 0013). The orchestration carried per-service
`build.args: GO_VERSION` exceptions in `deploy/docker-compose.build.yml`
and every multi-arch publish had to re-derive the override. Author
directive: unify the toolchain so no orchestration path needs a
per-service version override.

## Decision

- **Unified version: Go 1.26.10** — the lowest version satisfying every
  repo's `go.mod`. All ten Dockerfiles now default
  `ARG GO_VERSION=1.26.10`; `go.mod` directives are `go 1.26.10`
  everywhere except HA-Cupbearer, whose `go.mod` stays
  `go 1.26.0` + `toolchain go1.26.10` (file owned by an in-flight parallel
  track at the time; the 1.26.10 base image satisfies it as-is — align the
  directive when that track lands).
- **CI needed no changes**: every repo's `go.yml` already uses
  `go-version-file: go.mod`, so CI follows the same single source of truth.
- **Exceptions removed**: `deploy/docker-compose.build.yml` no longer
  carries per-service `build.args: GO_VERSION`; the Dockerfile `ARG`
  remains overridable for deliberate one-off toolchain experiments.
- **Republished images** (2026-11-12, rebuilt with zero build args):
  digests changed only where inputs actually changed —
  `ha-hrpauth` (Dockerfile + upstream code commits),
  `ha-cupbearer`, `ha-services-maintenance-view`, `ha-bs2ha`
  (Dockerfile/go.mod). The other six came out **byte-identical** to the
  previous publication (their earlier builds already ran the 1.26.10
  toolchain via build-arg; the Dockerfile text is not part of the final
  image layers) — direct measured proof of the reproducibility flags.
  Manifests and the compose template pin the new list digests where they
  changed; tags advanced to the unification commits everywhere.

## Measured evidence (2026-11-12)

- All ten multi-arch builds succeeded with **no `--build-arg`** under the
  unified Dockerfiles (`--platform linux/amd64,linux/arm64 --push`,
  `--provenance=false --sbom=false`, `SOURCE_DATE_EPOCH=0`).
- Six digests unchanged vs the 2026-11-11 publication
  (e.g. `ha-haskinlib` still `sha256:56350ff1…`); four changed and were
  re-pinned in both places (service `deploy-manifest.yaml` +
  `deploy/docker-compose.yml`).
- Default compose path (`docker compose up -d`, no build args) and the
  local-build path (`docker-compose.build.yml`, no GO_VERSION args) both
  verified end-to-end; see the verification report for per-round health.

## Boundaries / open items

- HA-Cupbearer `go.mod` directive unification is deferred to the
  parallel track owning that file (see Context); no build behavior depends
  on it.
- Future Go bumps: raise `go.mod` first (CI follows automatically), then
  the Dockerfile `ARG` in the same commit wave, then republish and re-pin
  digests in both places.
