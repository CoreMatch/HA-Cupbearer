# HA-Services-Deployer

`HA-Services-Deployer` is the backend service for HA service rollout and artifact delivery.

## Current scope

- expose a deployer HTTP API
- provide a version manifest for downstream maintenance views
- serve release artifacts from a local storage directory

## Version Manifest Protocol

- Contract document: `knowledge/0006-version-manifest.md`
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

When orchestration is enabled (see below), the following are added:

- `GET /api/v1/deploy/manifests` — aggregated project manifests
- `GET /api/v1/deploy/validate` — aggregate validation report
- `GET /api/v1/deploy/status` — declared vs. live status (read-only)
- `GET /api/v1/deploy/preflight` — one-pass preflight report, graded
  blocker/warning (read-only; query `projects`/`group` to scope)
- `GET /api/v1/deploy/runs`, `GET /api/v1/deploy/runs/:id` — run records
- `POST /api/v1/deploy/run` — start a deploy run (**requires `X-Deploy-Token`**)
- `POST /api/v1/deploy/rollback` — one-click rollback (**requires `X-Deploy-Token`**)

## Bare-metal orchestration (opt-in)

Deployer can act as a bare-metal orchestrator that drives
[Tinyvisor](https://github.com/SamuelCheston/Tinyvisor) on the target machine:
artifact staging → version dir + `current` symlink → script registration →
restart → health probe → rollback. Full boundary and semantics:
`knowledge/0007-bare-metal-orchestration-scope.md`.

Enable it via config v2:

```yaml
orchestration:
  enabled: true
  scan_roots: ["../"]                    # 扫描 <root>/<repo>/deploy-manifest.yaml
  external_manifests: ./deploy/manifests # mysql/redis 等外部依赖声明
  state_dir: ./data/deploy
  tinyvisor: { url: "http://127.0.0.1:7891" }
  credentials_path: ./data/deploy/credentials.json
```

Key rules:

- **Manifest 模型**：每个项目仓库根目录一份 `deploy-manifest.yaml`；
  `facts`（端口/健康/依赖/分组/数据目录）与运行时无关，容器阶段复用；
  `baremetal`/`container` 为运行时执行方式，`container` 当前冻结。
- **进程归 Tinyvisor**：编排器不常驻、不写 systemd、不自己拉进程。
- **生效 = 软链切换 + 重启 + 健康**；只搬文件不算部署。幂等重跑即续跑。
- **前置检查门（preflight）**：run/rollback 开局一次查全——目录/软链可写、
  env 文件、Tinyvisor 可达且密钥有效、端口占用（跳过项目自身进程）、
  磁盘空间、依赖可达；阻断项直接拒绝（400 附完整分级报告），警告项放行；
  每条都带具体路径/端口。`dry_run` 同样过门，并把报告与计划步骤一起落记录。
- **`manager: aapanel|manual` 的项目一律 foreign**：只读、绝不触碰。
- **鉴权 fail-closed**：变更端点必须携带 `X-Deploy-Token`；令牌未配置时
  返回 503，任何情况下不放开为无鉴权。运维面板经服务端转发令牌，
  浏览器端不接触凭据。
- **凭据边界**：`tinyvisor_api_key` / `deploy_token` / `github_token`
  只存在于 `credentials_path` 指向的本地文件（0600），不进仓库、不进清单。
- **产物**：本阶段统一 `artifact.source: local_path`（sha256 强制校验）；
  `ci_release` 已实现但未启用。将来启用发布时资产按硬约定命名：
  `<项目名>-linux-amd64` + `<项目名>-linux-amd64.sha256`。

## Deployment facts (this service)

Authoritative deployment facts for the Deployer itself live in
[`deploy-manifest.yaml`](deploy-manifest.yaml) at the repo root
(`manager: aapanel` = panel-managed, orchestrator read-only):

- Deployment port `:18080`, loopback-only (per
  `deploy/config/ha-services-deployer.yaml.tmpl`; the bare-local first-run
  default above is `:2770`); health probe `GET /healthz`.
- No service dependencies (local-file delivery service); no database, no Redis.
- Config file `config.yaml` (template version 1; the code auto-migrates the
  file to v2 on load).
- Data dir: `./data` (artifacts root, version manifest, orchestration state).
- External dependency declarations (reachability-only) for MySQL/Redis live in
  [`deploy/manifests/`](deploy/manifests/).

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

MySQL and Redis are external (not started by the template). Since HRPAuth
config v12 and Yggdrasil-API config v4 both have a dedicated `database.port`
field, the templates render `host: "${DB_HOST}"` plus `port: ${DB_PORT}`.
Keep `DB_HOST` a bare hostname/IP (no port). Older code without the port
field still works because its DSN uses the host value verbatim.

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
