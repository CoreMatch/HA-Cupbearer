<div align="center">
  <h1>Armorial Cupbearer</h1>
  <p><strong>Controlled releases, from staging to health.</strong></p>
  <p><a href="./README.zh-CN.md">简体中文</a></p>
</div>

Armorial Cupbearer delivers and deploys Armorial services through a bare-metal-first control path. It combines artifact staging, symlink activation, health and smoke checks, rollback, and a supervised MaintenanceView for operators.

## Why Armorial Cupbearer

- **Stage with checksums.** A checksum-based staging path prepares artifact files before activation.
- **Activate and verify.** Symlink switching is followed by health and optional smoke checks, with rollback for failed bare-metal changes.
- **Gate risky work.** Graded preflight and credential references surface blockers without placing secret values in manifests.
- **Resume initialization.** Resumable bootstrap and supervised MaintenanceView reduce repeated first-run work.
- **Roll back containers by digest.** Managed-container rollback targets the recorded image digest rather than a movable tag.

## Learn more

- [Armorial Cupbearer on Docker Hub](https://hub.docker.com/r/limeight/ha-cupbearer) — published container image
- [Armorial Cupbearer on GitHub](https://github.com/CoreMatch/HA-Cupbearer) — canonical source repository
- [Armorial Cupbearer API contract](https://github.com/CoreMatch/HA-Contract/blob/main/docs/api/openapi/ha-cupbearer.yaml) — versioned delivery surface
- [MaintenanceView](https://github.com/CoreMatch/HA-Services-MaintenanceView) — operator console

## Boundary

Manifest-declared artifact bytes are not hash-verified, not every delivery path is signed, container triggers do not auto-roll back, and current production uses direct atomic swaps rather than Cupbearer orchestration.

## License

[AGPL-3.0](LICENSE)
