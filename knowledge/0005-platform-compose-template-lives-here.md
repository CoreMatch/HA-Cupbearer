# 0005 — Platform Compose Template Lives Here

## Status

Accepted (records current state and boundary)

## Date

2026-11-04

## Context

Besides the deployer service itself, this repository hosts the Compose
template that orchestrates the whole HA platform in containers, added in
commit `e1a42cd` (2026-11-03) and documented in `README.md` in commit
`657bcb8` (2026-11-04).

## Decision

Record the arrangement and its boundary:

- Location: `deploy/` — `docker-compose.yml` (Compose project
  `ha-platform`), `.env.example`, `config/*.yaml.tmpl` per service,
  `secrets/` (signature keys), `sdkhandler-template/`.
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
  2701, haskinproxy 2702, winnerproxy 2777, mca-server 9178, sdkhandler
  2704/2705, deployer 18080, maintenance-view 18081
  (`deploy/docker-compose.yml`, per-service `ports` blocks).
- Secrets (`private_key.pem`/`public_key.pem`) must be placed into
  `deploy/secrets/` manually; `.env.example` documents this
  (`deploy/.env.example (field: SIGNATURE_KEY_DIR)`).
- Working-tree note at time of writing: the uncommitted local `README.md`
  edit removes the compose documentation section; the committed README at
  HEAD (`657bcb8`) documents `deploy/`. Recorded, no action taken.

## Evidence

- `deploy/docker-compose.yml` — full orchestration; service list and build
  contexts (`deploy/docker-compose.yml (field: services)`,
  `deploy/docker-compose.yml (field: context: ../)`); external DB/Redis note
  (`deploy/docker-compose.yml (field: MySQL / Redis)`).
- `deploy/.env.example` — deployment-scoped values.
- `deploy/config/*.yaml.tmpl` — per-service config templates.
- `docker-entrypoint.sh::envsubst` — envsubst rendering.
- Commits: `e1a42cd` (container image + compose template), `657bcb8` (README documentation of the compose template).
