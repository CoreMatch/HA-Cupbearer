# HA-Services-Deployer

`HA-Services-Deployer` is the backend service for HA service rollout and artifact delivery.

## Current scope

- expose a deployer HTTP API
- provide a version manifest for downstream maintenance views
- serve release artifacts from a local storage directory

## Version Manifest Protocol

- Contract document: `HA-Contract/docs/dev/HA-Services-Deployer/version-manifest.md`
- Local example: `examples/manifest.v1.json`

Manifest v1 is organized by service release. It is used for release display,
version comparison, and artifact download selection. First-deployment
configuration initialization is intentionally out of scope for this schema
revision.

## Quick start

1. Copy `config.example.yaml` to `config.yaml`.
2. Create `data/manifest.json` if you want to provide version data immediately.
   You can start from `examples/manifest.v1.json`.
3. Run:

```bash
go run .
```

## API skeleton

- `GET /healthz`
- `GET /api/v1/healthz`
- `GET /api/v1/manifest`
- `GET /api/v1/artifacts/:service/*filepath`
