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

This repository hosts the Compose template for the whole HA platform under
`deploy/` (build contexts point at the sibling service repositories). This
section covers deployment usage only; API/manifest contracts live in
HA-Contract and are not repeated here.

```
deploy/
├── docker-compose.yml                  # platform orchestration template
├── .env.example                        # deployment-scoped values (copy to .env)
├── config/*.yaml.tmpl                  # per-service config templates (envsubst)
├── nginx-sdkhandler-artifacts.conf     # SPA-fallback config for the webui profile
├── secrets/                            # signature keys (private_key.pem, public_key.pem)
└── sdkhandler-template/                # optional pre-placed frontend template (read-only)
```

Quick start:

```bash
cd deploy
cp .env.example .env   # fill in MySQL/Redis addresses, upstream URLs, secrets
docker compose up -d --build
```

### Services, ports, and exposure

| Service | Port | Default bind | Notes |
|---|---|---|---|
| hrpauth | 2778 | all interfaces | user/game-facing |
| hrpauth-yggdrasil-api | 2770 | all interfaces | game-facing |
| haskinlib | 2701 | all interfaces | texture uploads/serving |
| haskinproxy | 2702 | all interfaces | game-facing |
| winnerproxy | 2777 | all interfaces | game-facing |
| mca-server | 9178 | all interfaces | game-facing |
| ha-webui-sdkhandler | 2704, 2705 | **127.0.0.1** | internal build service |
| ha-services-deployer | 18080 | **127.0.0.1** | internal delivery service |
| ha-services-maintenance-view | 18081 | **127.0.0.1** | ops panel, unauthenticated |
| ha-webui-nginx (optional) | 8080 | all interfaces | profile `webui` |

Internal services are loopback-bound by default; inter-service calls use the
Compose network. To expose one deliberately, override its bind variable in
`.env` (e.g. `SDKHANDLER_BIND=0.0.0.0`) and firewall the host accordingly.
`ha-services-maintenance-view` has no authentication — prefer SSH tunneling
over exposing it.

### Optional WebUI hosting (`--profile webui`)

The built frontend (published by HA-WebUI-SDKHandler into the
`sdkhandler-artifacts` volume) is served by an optional nginx sidecar, off by
default:

```bash
docker compose --profile webui up -d
```

It mounts the artifacts volume read-only and applies an SPA fallback
(`deploy/nginx-sdkhandler-artifacts.conf`). `WEBUI_PORT` (default 8080) and
`WEBUI_BIND` (default 0.0.0.0) control the listener.

### Database connection convention

MySQL and Redis are external (not started by the template). HRPAuth and
Yggdrasil-API have **no separate DB port config field** — their DSN uses the
host value verbatim — so their templates render `host: "${DB_HOST}:${DB_PORT}"`.
Keep `DB_HOST` a bare hostname/IP (no port); services with a real port field
(e.g. MatchCoreArena-Server) use `DB_PORT` directly.

### Secrets to generate and distribute

- `SDKHANDLER_INTERNAL_KEY` — shared `X-Internal-Key` guarding SDKHandler's
  `/api/*` routes (fail-closed when empty). Generate e.g.
  `openssl rand -hex 32`; put the same value in `.env` and in any operator
  tooling that calls `/api/build/trigger`. No service calls these endpoints
  automatically.
- `MCA_MAINTENANCE_TOKEN` — MatchCoreArena-Server ops credential
  (`auth.maintenance_token`). Generate the same way; without it the container
  would generate a fresh random token on every restart.
- `MANAGE_TOKEN`, OAuth2 client secrets — see `.env.example`.

### Upgrade notes

- **`hrpauth-storage` volume is deprecated.** HRPAuth config v8 removed the
  `storage` section and v11 does not read or write `/app/storage` at all.
  The volume and mount are kept for existing deployments (removing them
  could discard data placed there by older versions). Plan: remove both in a
  future cleanup once operators have checked the volume contents.
- Since 2026-11-05 the internal-service ports are loopback-bound by default
  (see table above); adjust `*_BIND` if your automation reached them from
  another host.
