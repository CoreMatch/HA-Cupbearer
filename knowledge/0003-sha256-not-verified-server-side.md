# 0003 — sha256 Is Not Verified Server-Side (Unimplemented)

## Status

Accepted (records a known unimplemented item)

## Date

2026-11-04

## Context

Each manifest artifact carries `sha256` and `size_bytes`. The contract makes
sha256 the **authoritative content identity** for update detection: consumers
compare sha256 to decide whether a binary really changed, even when `version`
stays the same (HA-Contract `version-manifest.md`, "Comparison Semantics",
steps 4–5).

## Decision

Record what is and is not implemented:

**Implemented (format only):**

- `sha256` must be a lowercase 64-char hex string and `size_bytes` must be
  ≥ 0, enforced during manifest validation
  (`internal/models/manifest.go:17`, `:121-126`).

**Not implemented:**

- The server never hashes any served file. There is no sha256 computation
  anywhere under `internal/server/` (verified by search). `GetArtifact`
  stats the file and streams it (`internal/server/handlers.go:74-106`); it
  does not compare bytes against the manifest's `sha256`, nor check
  `size_bytes`, nor expose the digest in response headers.
- There is no ingestion path that could compute digests when artifacts are
  added (see 0001), so nothing ever reconciles the manifest against the
  files on disk.

## Consequences (known unimplemented items)

- **Manifest and files can silently diverge.** If the on-disk artifact
  differs from what `sha256` describes, the server serves it anyway. The
  manifest's digest is only as trustworthy as whoever wrote the file and the
  JSON.
- **Update detection is entirely a consumer-side duty.** The contract's
  comparison semantics assume some consumer downloads the artifact and
  verifies sha256. At HEAD, the only known consumer (HA-Services-MaintenanceView)
  does **not** download or verify artifacts at all (it only renders the
  download URL), so today **no component in the ecosystem verifies artifact
  content against the manifest**. Recorded honestly as an open gap.
- Candidate future work (not decided): verify bytes at serve time, or provide
  an ingestion/publish tool that computes digests. *Pending author
  confirmation* on whether either is planned.

## Evidence

- `internal/models/manifest.go:17` — `sha256Pattern`; `:121-126` — format checks only.
- `internal/server/handlers.go:74-106` — artifact serving without hashing.
- Search verification: no `sha256`/`SHA256` usage under `internal/server/`.
- Contract: HA-Contract `docs/dev/HA-Services-Deployer/version-manifest.md`, Field Reference (`sha256`, `size_bytes`) and Comparison Semantics.
