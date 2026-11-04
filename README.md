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

1. Run the service; on first start it auto-generates `config.yaml` with the
   default values from `internal/models/config.go` (`DefaultConfig`):
   listen `:2770`, artifacts root `./data/artifacts`, manifest path
   `./data/manifest.json`. Edit the generated file to override them.
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

## Platform container orchestration

This repository also hosts the Compose template that runs the whole HA platform
in containers. It lives in `deploy/` because the build contexts point at the
sibling service repositories next to it.

```
deploy/
├── docker-compose.yml          # platform orchestration template
├── .env.example                # deployment-scoped values (copy to .env)
├── config/*.yaml.tmpl          # per-service config templates
├── secrets/                    # signature keys (private_key.pem, public_key.pem)
└── sdkhandler-template/        # optional frontend template for HA-WebUI-SDKHandler
```

Quick start:

```bash
cd deploy
cp .env.example .env   # fill in MySQL/Redis addresses, upstream URLs, secrets
docker compose up -d --build
```

Design notes:

- MySQL and Redis are **not** started by the template. Services read them from
  the `DB_*` / `REDIS_*` variables, which point at externally managed instances.
- Each service reads a YAML config file. The templates in `config/` use
  `${VAR}` placeholders; the container entrypoint renders them with `envsubst`
  into the path the service expects (`config.yaml`, `config.yml`, or
  `config/config.yaml` for MatchCoreArena-Server).
- Images are built locally and tagged `ha/<service>:<VERSION>`.
- Service-to-service traffic uses Compose service names, e.g.
  `http://hrpauth:2778` and `http://ha-services-deployer:18080`.
