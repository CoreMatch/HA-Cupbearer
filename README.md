# HA-Cupbearer

`HA-Cupbearer` is the backend service for HA service rollout and artifact delivery.

## Current scope

- expose the Cupbearer HTTP API
- provide a version manifest for downstream maintenance views
- serve release artifacts from a local storage directory

## Version Manifest Protocol

- Contract document: `knowledge/0006-version-manifest.md`
- Local example: `examples/manifest.v1.json`

Manifest v1 is organized by service release. It is used for release display,
version comparison, and artifact download selection. First-deployment
configuration initialization is **no longer out of scope**: it is provided by
the bootstrap layer below (outside the version-manifest schema — manifests
stay release-oriented). See `knowledge/0016-first-run-bootstrap-mv-autostart-infra.md`.

## First-run bootstrap (one-click initialization)

Turns a fresh machine into a runnable platform without manual secret
generation, database creation, or `.env` assembly. Independent of
`orchestration.enabled`; the wizard UI lives in HA-Services-MaintenanceView,
which the Cupbearer **auto-launches and supervises** (reuses an already-
running instance, never starts a second).

- `GET /api/v1/deploy/bootstrap/status` — current state + wizard metadata
- `GET /api/v1/deploy/bootstrap/infra` — MySQL/Redis infrastructure status
  (connection forms per deployment path: bare-metal `127.0.0.1:<port>`,
  container compose `mysql:3306`)
- `POST /api/v1/deploy/bootstrap/infra/mysql|redis` — one-click ensure
  (detect → adopt existing → launch managed container, wait for real
  readiness; **X-Deploy-Token**)
- `POST /api/v1/deploy/bootstrap/plan` — dry-run preview (**X-Deploy-Token**)
- `POST /api/v1/deploy/bootstrap/run` — one-click initialization
  (**X-Deploy-Token**): infra → secrets → databases → signing keys →
  `.env` merge → bare-metal config rendering
- `GET /api/v1/deploy/bootstrap/runs[/:id]` — run records (resume basis)
- `GET /api/v1/deploy/maintenance-view` — supervised MV status

Guarantees: idempotent (existing values/files/databases preserved, drift
warns, never overwrites); resumable (re-run continues from the breakpoint);
no plaintext secrets in any response/log (fingerprints only); MySQL/Redis
infrastructure stands per selected deployment path (`deploy/infra.yml`; wipe
procedure in its header comment). Deploy token is auto-generated into the
credentials file on first start when absent.

The databases step probes before it acts. When the app account already
exists and this run's credential does not verify against it (re-initializing
a machine, or redoing an interrupted init), the step refuses without an
explicit `database.policy`: `takeover` (alter the account password to this
run's credential), `reuse` (keep it; the operator-provided
`database.existing_password` is verified, then adopted into state), or
`skip` (leave the whole step untouched, complete the remaining steps).
Matching credentials stay a no-op; with no conflict the policy is ignored.
Plans report the probe so the wizard shows what already exists, what was
chosen, and what happened.

## Quick start

1. Run the service; on first start it auto-generates `config.yaml` with the
   default values from `internal/models/config.go` (`DefaultConfig`):
   listen `:18080`, artifacts root `./data/artifacts`, manifest path
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
- `POST /api/v1/deploy/container/trigger` — one-click container stack trigger
  (**requires `X-Deploy-Token`**)
- bootstrap + maintenance-view endpoints — see "First-run bootstrap" above
  (always available, independent of orchestration)

## Bare-metal orchestration (opt-in)

Cupbearer can act as a bare-metal orchestrator that drives
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
  `facts`（端口/健康/依赖/分组/数据目录）与运行时无关，两种运行时复用；
  `baremetal`/`container` 为运行时执行方式，`runtime` 选择器按项目二选一
  （默认 `baremetal`，可用 `overrides.<p>.runtime` 按部署上下文覆盖）。
- **进程归 Tinyvisor**：编排器不常驻、不写 systemd、不自己拉进程。
- **生效 = 软链切换 + 重启 + 健康**；只搬文件不算部署。幂等重跑即续跑。
- **前置检查门（preflight）**：run/rollback 开局一次查全——目录/软链可写、
  env 文件、smoke 密钥可解析、Tinyvisor 可达且密钥有效、端口占用（跳过项目
  自身进程）、磁盘空间、依赖可达；阻断项直接拒绝（400 附完整分级报告），
  警告项放行；每条都带具体路径/端口。`dry_run` 同样过门，并把报告与计划
  步骤一起落记录。
- **部署后冒烟（facts.smoke，可选）**：健康探测只证明进程活着；声明了
  `facts.smoke` 的项目在健康判定后还要跑真实请求（方法/路径/期望状态码/
  响应体断言），冒烟失败 = 部署失败，走同样的自动回滚。密钥值只以
  `${secret:<name>}` 引用，从凭据文件 `smoke_secrets` 解析，绝不进 Manifest。
  托管 SPA 的服务注意：静态回退会对未知路径一律回 200 + text/html，
  伪造"服务活着"的假象——冒烟断言用 `expect_content_type`
  （如 `application/json`）先照出回退，再核对响应体字段。
- **`manager: aapanel|manual` 的项目一律 foreign**：只读、绝不触碰。
- **鉴权 fail-closed**：变更端点必须携带 `X-Deploy-Token`；令牌未配置时
  返回 503，任何情况下不放开为无鉴权。运维面板经服务端转发令牌，
  浏览器端不接触凭据。
- **凭据边界**：`tinyvisor_api_key` / `deploy_token` / `github_token` /
  `smoke_secrets`（冒烟请求引用的密钥）只存在于 `credentials_path` 指向的
  本地文件（0600），不进仓库、不进清单。
- **产物**：本阶段统一 `artifact.source: local_path`（sha256 强制校验）；
  `ci_release` 已实现但未启用。将来启用发布时资产按硬约定命名：
  `<项目名>-linux-amd64` + `<项目名>-linux-amd64.sha256`。

## Container path (default interaction: one-click trigger)

**定位**：Cupbearer 的设计意图是"在不使用容器的情况下编排微服务"——裸金属
编排（版本目录+软链、Tinyvisor 重启、健康判定、回滚）是本体。容器路径在
此之上提供两种形态，职责不同：

1. **一键触发（容器路径的默认交互）**：
   `POST /api/v1/deploy/container/trigger`（X-Deploy-Token，fail-closed，
   单飞约束与部署/回滚共享）。编排器按各项目清单 `container` 段已发布的
   镜像引用（digest 优先：`repo@sha256:<manifest list digest>`；仅声明 tag
   时 `repo:tag`）生成 compose override，叠加在平台 compose 模板上执行
   `docker compose up -d`，然后把结果**如实回报**（运行类型
   `container-trigger`，经 `GET /api/v1/deploy/runs/:id` 查询）。整套容器
   生命周期（创建、网络、健康检查、重启策略）由 compose 承担，编排器在这
   条路径上不做逐容器的创建编排、健康轮询或回滚。模板位置由
   `orchestration.container.compose_file` 指定（默认
   `deploy/docker-compose.yml`）：模板承载 env/配置模板等整套接线，清单只
   贡献镜像引用；清单声明了已发布引用但模板无同名服务的、以及模板有服务
   但清单无已发布引用的，都记入运行备注，不静默。
2. **容器级管理（已实现的可选能力，保留）**：`runtime: container`
   （或 `overrides.<p>.runtime: container`）把单项目切到容器流水线：
   镜像即版本（标签 `<project>:<service_version>`），版本切换 = 同名容器
   重建，监督由 docker `--restart unless-stopped` 负责（容器项目不触碰
   Tinyvisor）。健康探测与冒烟断言打在发布的宿主口上，语义不变。带托管
   标签但项目已不在清单中的孤儿容器**默认只报不删**
   （`orchestration.container.orphan_policy: report | remove`）。
   设计与边界：`knowledge/0011-container-orchestration-minimal-loop.md`；
   定位决策记录：`knowledge/0014-container-path-trigger-first-positioning.md`。

以下为容器级管理（可选能力）的细节。

Runtime 选择与 docker 端点：

- **runtime 选择器**：清单 `runtime:` 字段，`baremetal`（默认）| `container`
  二选一（`internal/manifest/manifest.go::Manifest`，其它值校验即拒）；
  配置 `orchestration.overrides.<项目>.runtime` 按部署上下文覆盖清单选择
  （如生产裸金属、隔离环境容器），覆盖优先。
- **`docker_host`**：`orchestration.container.docker_host` 显式指定 docker
  端点（如 `unix:///run/user/1000/docker.sock`）；留空时按序探测
  （`internal/docker/docker.go::DetectHost`）：
  1. 环境变量 `DOCKER_HOST`（CLI 本身也尊重它与当前 context）；
  2. `$XDG_RUNTIME_DIR/docker.sock` —— rootless 用户单元；
  3. `/var/run/docker.sock` —— 系统 dockerd。
- **rootless 前置**（无特权验证的默认形态）：
  `dockerd-rootless-setuptool.sh install` → `systemctl --user start docker`
  （systemd **用户**单元，不碰系统 dockerd）。前置条件：subuid/subgid、
  rootlesskit、slirp4netns、内核非特权 overlayfs（无需 fuse-overlayfs）；
  端点即 `$XDG_RUNTIME_DIR/docker.sock`。验证过程见
  `knowledge/0011-container-orchestration-minimal-loop.md`。

回滚语义（重要，与裸金属的"切软链"对应）：

- **回滚按镜像 digest 钉死，不按标签。** 标签是可移动的引用——同版本重打
  一次标签，按标签回滚就会静默落到另一份内容上，且不报任何错。因此运行
  记录存镜像内容摘要（`active_image_id`）；回滚（自动与一键）先校验该摘要的
  镜像仍在、再按摘要定位重建；幂等判定也比较摘要而非标签。目标镜像已不
  存在时**明确报"目标镜像已不存在"并拒绝**，绝不退回按标签定位、绝不落到
  别的东西上。标签在记录里只作为人类可读的名字出现。
- **摘要丢了怎么办**：两种形态——① 记录的摘要对应的镜像本机已不在：
  回滚明确拒绝（自动与一键皆然），恢复手段是**重新部署该版本**（重建或
  重新装载镜像），运行记录记下新摘要后回滚能力恢复；② 该版本从未在本机
  部署过、没有任何已记录摘要：回滚同样拒绝。不存在"找个近似镜像顶上"的
  路径。
- **`cascade: true` 跨运行时真实现**：容器依赖方按其自身当前运行的摘要
  重建，裸金属依赖方经 Tinyvisor 重启，两侧都必须回到健康（含冒烟）。
  不存在"接受 cascade 参数却只回滚一个"的降级形态。执行顺序上游先行、
  依赖方随后，逐个项目回到健康（含冒烟），任一失败整体报失败。
- **镜像构建可复现**（回滚钉死 digest 的前提）：构建固定
  `--provenance=false --sbom=false` + `SOURCE_DATE_EPOCH=0`——同输入
  连续两次构建摘要一致（非确定性来源是 BuildKit 默认的
  provenance/attestation 清单，携带构建时间戳）。仍不可复现的边界：
  基础镜像标签被上游重推、上下文内容真实变化——那属于输入变了。

## Manifest 凭据写法：只允许 `${secret:<name>}`

凭据值绝不进 `deploy-manifest.yaml`。装载层
（`internal/manifest/manifest.go::scanCredentials`）对清单原始结构全文扫描，
命中以下任一形态即**整体拒绝装载**（报出位置与原因）：

1. **凭据类键名带字面值**：键名含 password / passwd / secret / token /
   api_key / api-key / credential / private_key / access_key（大小写不
   敏感）时，值必须是整值的 `${secret:<name>}` 引用；
2. **任意位置的值形似凭据**（与键名无关）：PEM 私钥块
   （`-----BEGIN … PRIVATE KEY-----`）、GitHub 令牌族
   （`ghp_`/`gho_`/`ghs_`/`ghu_`/`github_pat_` + ≥16 字符）、AWS 访问密钥
   （`AKIA` + 16 字符）、JWT（`eyJ` 三段式）、≥48 位十六进制串、
   ≥48 字符 base64 串；
3. **非法占位符**：任何不是完整 `${secret:<name>}` 的 `${...}`。冒烟请求
   （`facts.smoke`）的 headers 只接受字面量或整值引用、body 允许内联
   `${secret:<name>}` 替换，其余占位形态一律拒绝。

键值在运行时从 `credentials_path` 所指文件（0600，不进仓库）的
`smoke_secrets` 解析。

## 运维注意事项（docker CLI 踩坑）

写容器相关的脚本/驱动前必读——这两条都在实测中踩过并修掉：

1. **一律 `docker container inspect`，不要用裸 `docker inspect`。**
   本编排器的项目名同时是镜像仓库名；同名镜像存在时，裸
   `docker inspect <名字>` 会解析成**镜像**的 JSON（根本没有 `.State`），
   静默返回错误对象。见 `internal/docker/docker.go::CLI.Inspect`。
2. **inspect JSON 的运行状态字段是 `.State.Status`，不是 `.State.State`。**
   `{{.State.State}}` 渲染为空字符串且不报错——静默错误，最坏的一类。

## Deployment facts (this service)

Authoritative deployment facts for the Cupbearer itself live in
[`deploy-manifest.yaml`](deploy-manifest.yaml) at the repo root
(`manager: aapanel` = panel-managed, orchestrator read-only):

- Deployment port `:18080`, loopback-only (per
  `deploy/config/ha-cupbearer.yaml.tmpl`; identical to the bare-local
  first-run default); health probe `GET /healthz`.
- No service dependencies (local-file delivery service); no database, no Redis.
- Config file `config.yaml` (template version 1; the code auto-migrates the
  file to the current version on load).
- Data dir: `./data` (artifacts root, version manifest, orchestration state).
- External dependency declarations (reachability-only) for MySQL/Redis live in
  [`deploy/manifests/`](deploy/manifests/).

## Platform container orchestration

This repository hosts the Compose template for the whole HA platform under
`deploy/` (build contexts point at the sibling service repositories). This
section covers deployment usage only; API/manifest contracts live in
HA-Contract and are not repeated here. 一键触发
（`POST /api/v1/deploy/container/trigger`）消费的就是这份模板：按清单已
发布镜像引用生成 override 叠加其上，模板自身的 env/配置接线原样生效。

```
deploy/
├── docker-compose.yml                  # platform orchestration template (published images, digest-pinned)
├── docker-compose.build.yml            # explicit opt-in override: local builds instead of published images
├── .env.example                        # deployment-scoped values (copy to .env)
├── config/*.yaml.tmpl                  # per-service config templates (envsubst)
├── nginx-sdkhandler-artifacts.conf     # SPA-fallback config for the webui profile
├── secrets/                            # signature keys (private_key.pem, public_key.pem)
└── sdkhandler-template/                # optional pre-placed frontend template (read-only)
```

Quick start (default: pull the published images — no build):

```bash
cd deploy
cp .env.example .env   # fill in MySQL/Redis addresses, upstream URLs, secrets
docker compose up -d
```

`docker-compose.yml` pins every service to its published multi-arch image
(`limeight/ha-*`) by **manifest-list digest** (`repo@sha256:...`), the same
digests the service repos' `deploy-manifest.yaml` pin; when an image is
republished, update both places. Local building is an explicit opt-in path
(no network, or verifying local code changes), never the default:

```bash
docker compose -f docker-compose.yml -f docker-compose.build.yml up -d --build
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
| ha-cupbearer | 18080 | **127.0.0.1** | internal delivery service |
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
