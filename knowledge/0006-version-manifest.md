> 来源：HA-Contract@bab2a05（2026-11-07），原路径 docs/dev/HA-Cupbearer/version-manifest.md（契约仓知识库回迁）。

# HA Cupbearer Version Manifest

## Purpose

`HA-Cupbearer` publishes a machine-readable manifest describing which
service releases are currently available for download.

Version manifest v1 supports:

- showing release status
- comparing the deployed service state with the available upstream release
- downloading the exact binary or package selected by the operator

Version manifest v1 is **not** responsible for:

- first-deployment configuration initialization
- config template rendering
- local deployment path discovery
- process stop/start policies

Those concerns stay outside the manifest and are handled by consumers or a
future deployment-init protocol.

> `HA-Services-MaintenanceView` exists in the ecosystem but is a
> work-in-progress project and is not covered by this contract.

## Transport

`HA-Cupbearer` exposes:

- `GET /healthz` — liveness probe
- `GET /api/v1/healthz` — liveness probe (API-prefixed)
- `GET /api/v1/manifest` — full manifest (structure defined below)
- `GET /api/v1/projects` — release-oriented projection of the manifest: only artifacts with `role: "binary"`, top-level key `projects` (no `schema_version`). Envelope: `{"success": true, "message": "projects loaded", "data": {"generated_at": "...", "projects": [...]}}`.
- `GET /api/v1/artifacts/:service/*filepath` — artifact download (path traversal guarded)

The HA Cupbearer service returns the manifest inside its standard HTTP envelope:

```json
{
  "success": true,
  "message": "manifest loaded",
  "data": {
    "schema_version": 1,
    "generated_at": "2026-10-17T06:00:00Z",
    "services": []
  }
}
```

This document defines the structure of the `data` object.

## Manifest Shape

```json
{
  "schema_version": 1,
  "generated_at": "2026-10-17T06:00:00Z",
  "services": [
    {
      "name": "hrpauth",
      "display_name": "HRPAuth",
      "channel": "stable",
      "version": "1.0.0",
      "published_at": "2026-10-17T05:45:00Z",
      "artifacts": [
        {
          "id": "linux-amd64-binary",
          "role": "binary",
          "os": "linux",
          "arch": "amd64",
          "file_name": "HRPAuth",
          "download_path": "release/linux-amd64/HRPAuth",
          "sha256": "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef",
          "size_bytes": 28753920
        }
      ]
    }
  ]
}
```

## Field Reference

### Top Level

| Field | Type | Required | Description |
|---|---|---|---|
| `schema_version` | int | yes | Manifest schema version. Current value is `1`. |
| `generated_at` | string | yes | RFC3339 UTC timestamp showing when the manifest document was produced. |
| `services` | array | yes | List of independently published service releases. Empty array is allowed. |

### Service Entry

| Field | Type | Required | Description |
|---|---|---|---|
| `name` | string | yes | Stable service identifier. MUST match `^[a-z0-9]([a-z0-9-]*[a-z0-9])?$`. |
| `display_name` | string | no | Human-facing label for dashboards or tables. |
| `channel` | string | no | Release channel label such as `stable`, `beta`, or `canary`. |
| `version` | string | yes | Human-facing release version string. v1 treats it as an opaque display value, not as an ordering guarantee. |
| `published_at` | string | no | RFC3339 UTC timestamp for the specific service release. |
| `artifacts` | array | yes | At least one downloadable artifact for this service release. |

### Artifact Entry

| Field | Type | Required | Description |
|---|---|---|---|
| `id` | string | yes | Unique artifact identifier within the service entry. |
| `role` | string | yes | Artifact purpose. Supported values in v1: `binary`, `archive`, `asset`. |
| `os` | string | no | Target operating system hint, for example `linux`. |
| `arch` | string | no | Target CPU architecture hint, for example `amd64`. |
| `file_name` | string | yes | File name presented to the operator after download. |
| `download_path` | string | yes | Relative path under `/api/v1/artifacts/{service-name}/`. MUST be relative and MUST NOT escape the service artifact root. |
| `sha256` | string | yes | Lowercase 64-character SHA-256 hex digest of the downloadable file bytes. |
| `size_bytes` | int64 | yes | Artifact size in bytes. |

## Comparison Semantics

The manifest fields are intended to be compared as follows:

1. Match a remote service by `services[].name`.
2. Select candidate artifacts by `role` plus the local runtime's supported
   `os`/`arch` policy.
3. Use `version` for display purposes.
4. Use `sha256` as the authoritative content identity for update detection.
5. Treat a changed `sha256` as a real binary/package change even if `version`
   stays the same.

This avoids coupling the deployment workflow to semantic-version parsing and
still preserves correct binary replacement behavior.

## Download URL Resolution

A concrete download URL resolves as:

`{cupbearer_base_url}/api/v1/artifacts/{service.name}/{artifact.download_path}`

Example:

```text
http://127.0.0.1:18080/api/v1/artifacts/hrpauth/release/linux-amd64/HRPAuth
```

## Validation Rules

The Cupbearer should reject or fail to serve an invalid manifest when:

1. `schema_version` is not `1`
2. two service entries share the same `name`
3. a service has an empty `version`
4. a service has no artifacts
5. two artifacts under the same service share the same `id`
6. an artifact uses an unsupported `role`
7. `download_path` is absolute or escapes the service root
8. `sha256` is not a lowercase 64-character hex string

## Future Evolution

If the deployment workflow later needs first-install bootstrap metadata,
configuration templates, or pre/post deployment hooks, those additions should be
introduced in a new schema revision instead of overloading manifest v1.
