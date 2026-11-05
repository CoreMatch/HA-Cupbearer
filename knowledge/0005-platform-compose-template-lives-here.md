# 0005 — Platform Compose Template Lives Here

## Status

Accepted (records current state and boundary)

## Context

Besides the deployer service itself, this repository hosts the Compose
template that orchestrates the whole HA platform in containers, documented
in `README.md`.

## Decision

Record the arrangement and its boundary:

- Location: `deploy/` — `docker-compose.yml` (Compose project
  `ha-platform`), `.env.example`, `config/*.yaml.tmpl` per service,
  `nginx-sdkhandler-artifacts.conf` (SPA fallback for the optional `webui`
  profile), `secrets/` (signature keys), `sdkhandler-template/`.
- **Build contexts point at sibling repositories** next to this one
  (`deploy/docker-compose.yml`: `../HRPAuth`, `../HRPAuth-Yggdrasil-API`,
  `../HASkinLib`, `../HASkinProxy`, `../WinnerProxy`,
  `../MatchCoreArena-Server`, `../HA-WebUI-SDKHandler`,
  `../HA-Services-Deployer`, `../HA-Services-MaintenanceView`). That is why
  the orchestration lives here rather than in each service repo: a single
  checkout level where all siblings exist. A full platform build therefore
  requires all sibling repos checked out side by side.
- **MySQL and Redis are external.** The template does not start them;
  services read `DB_*` / `REDIS_*` from `.env`
  (`deploy/docker-compose.yml (field: x-common-env)`,
  `deploy/.env.example (field: DB_HOST)`,
  `deploy/.env.example (field: REDIS_HOST)`).
- **Config rendering.** Each service container receives
  `CONFIG_TEMPLATE`/`RENDERED_CONFIG` and its entrypoint renders the YAML
  template with `envsubst` before start (this repo's
  `docker-entrypoint.sh::envsubst`; the same script is duplicated in the
  sibling repos). All mutable values therefore flow from `.env`.
- **Boundary statement:** `deploy/` is deployment tooling. It is not part of
  the deployer service's runtime API surface (0001) and is not covered by the
  version-manifest contract. Changes to `deploy/` do not change service
  behavior; they change how the platform is assembled.

## Consequences

- This repo has a dual role (service + platform orchestration home). Anyone
  cloning only this repo cannot build the platform; the sibling repos are
  mandatory.
- Port defaults in compose: hrpauth 2778, yggdrasil-api 2770, haskinlib
  2701, haskinproxy 2702, winnerproxy 2777, mca-server 9178 listen on all
  interfaces; sdkhandler 2704/2705, deployer 18080, and maintenance-view
  18081 are loopback-bound by default (`SDKHANDLER_BIND` / `DEPLOYER_BIND` /
  `MAINTENANCEVIEW_BIND` override the bind address). The optional `webui`
  profile adds an nginx sidecar on 8080 serving the built frontend
  (`deploy/docker-compose.yml`, per-service `ports` blocks).
- Secrets (`private_key.pem`/`public_key.pem`) must be placed into
  `deploy/secrets/` manually; `.env.example` documents this
  (`deploy/.env.example (field: SIGNATURE_KEY_DIR)`).

## Evidence

- `deploy/docker-compose.yml` — full orchestration; service list and build
  contexts (`deploy/docker-compose.yml (field: services)`,
  `deploy/docker-compose.yml (field: context: ../)`); external DB/Redis note
  (`deploy/docker-compose.yml (field: MySQL / Redis)`).
- `deploy/.env.example` — deployment-scoped values.
- `deploy/config/*.yaml.tmpl` — per-service config templates.
- `deploy/nginx-sdkhandler-artifacts.conf` — SPA fallback for the `webui` profile.
- `docker-entrypoint.sh::envsubst` — envsubst rendering.
