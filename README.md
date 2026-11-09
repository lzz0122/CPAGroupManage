# CPA LZZ Groups Plugin

`cpa-plugin-lzz-groups` is an out-of-tree CLIProxyAPI plugin that keeps the LZZ
credential-group policy outside the frequently updated CPA source tree.

It provides:

- exclusive downstream API-key authentication;
- API-key-to-group policies;
- credential-to-group assignments for OAuth files, runtime credentials, and
  configuration-defined API keys;
- request-scoped credential filtering while preserving CPA priority,
  round-robin, fill-first, weighted selection, and session affinity;
- group-scoped model discovery, quota-health, and official Codex quota APIs for NewAPI;
- a management page registered as `分组管理` in the CPA navigation.

## Architecture

The customization is split into three independently upgradeable layers:

1. CPA contains only generic plugin-host extensions. It does not contain LZZ
   policy rules or UI code.
2. This repository owns group policy, management UI, and NewAPI-facing APIs.
3. NewAPI calls the plugin management APIs instead of depending on private CPA
   endpoints or the global `/v1/models` response.

The plugin refuses to load unless the host reports all required features:

- `scheduler.eligible-auth-filter.v1`
- `auth.runtime-inspection.v1`
- `auth.http-request.v1`

This fail-closed handshake prevents an older CPA host from silently ignoring a
group filter and routing a request to the wrong credential.

## Build

The current release targets CPA v8 (validated against v8.0.61). Build the host
and plugin with Go 1.26.10 or newer and the same Linux libc. The native C ABI
remains version 1; the v8 SDK module path is used for RPC types.

The host patch also exposes the generic `scheduler_candidate_filter` capability.
The plugin opts into it so group eligibility runs before cooldown and priority
selection, then delegates to the configured selector. An absent eligibility
decision fails closed. Ordinary upstream scheduler plugins retain their existing
available-candidate behavior. Preserve `EligibleAuthIDs` (including an explicit
empty list) in the v8 `SchedulerPickResponse.UnmarshalJSON` implementation.

Frontend authentication opts out of request-body delivery: it uses only headers
and query parameters, so long conversation payloads need not cross the plugin ABI.

Requirements:

- Linux;
- Go 1.26.10 or newer;
- a C toolchain for `-buildmode=c-shared`.

```bash
go test ./...
go build -o /tmp/cpa-plugin-lzz-groups-check .
rm /tmp/cpa-plugin-lzz-groups-check
go build -buildmode=c-shared -o bin/lzz-groups.so .
```

The generated `.so` and `.h` files are build artifacts and are intentionally
ignored by Git.

## CPA configuration

Copy `lzz-groups.so` into the configured CPA plugin directory, then add a
configuration like this:

```yaml
plugins:
  enabled: true
  dir: /CLIProxyAPI/plugins
  configs:
    lzz-groups:
      enabled: true
      priority: 100
      refresh-seconds: 60
      api-keys:
        - id: lzz_plus
          label: LZZ Plus
          key: "<DOWNSTREAM_API_KEY>"
          groups:
            - lzz_plus
      credential-groups:
        "<AUTH_INDEX>":
          - lzz_plus
```

`credential-groups` should normally use the stable `auth_index` shown by the
management page. Credential IDs are accepted as a migration fallback.

Policy behavior:

- an API-key policy with an empty `groups` list can use all credentials;
- `groups: ["*"]` can also use all credentials;
- a restricted API key can use only credentials with an intersecting group;
- an unknown or not-yet-refreshed credential is rejected for restricted keys;
- group names are matched case-insensitively.

The plugin is an exclusive frontend authentication provider. Every downstream
key that must remain usable has to be present under the plugin's `api-keys`
before cutover.

## Management UI

Open the CPA control panel and select `分组管理`. The page requires the CPA
management key and stores it only in the current browser tab's session storage.

The account table shows a stable auth index, source, provider, priority, masked
API-key preview where applicable, status, model count, and group source.

OAuth/auth-file credentials with legacy `group` or `groups` fields are imported
as a read-compatible migration fallback. Once a group is saved in plugin
configuration, plugin configuration takes precedence.

## Management API

All endpoints require normal CPA management authentication.

| Method | Path | Purpose |
| --- | --- | --- |
| `GET` | `/v0/management/plugins/lzz-groups/state` | Policies and credential state |
| `POST` | `/v0/management/plugins/lzz-groups/refresh` | Force a credential metadata refresh |
| `POST` | `/v0/management/plugins/lzz-groups/models` | Models visible to one downstream API key |
| `POST` | `/v0/management/plugins/lzz-groups/quota-health` | Group-scoped quota availability |
| `POST` | `/v0/management/plugins/lzz-groups/usage` | Official Codex 5h/7d quota windows |
| `GET` | `/v0/resource/plugins/lzz-groups/ui` | Management page resource |

Model request example:

```json
{"api_key":"<DOWNSTREAM_API_KEY>"}
```

Quota-health request example:

```json
{"api_key":"<DOWNSTREAM_API_KEY>","models":["gpt-5.6-sol"]}
```

Quota usage request example:

```json
{"api_key":"<DOWNSTREAM_API_KEY>","group":"lzz_plus","refresh":true}
```

Official quota responses are cached per credential for 60 seconds. Ordinary reads and explicit
refreshes both fetch a new official snapshot once that interval expires. Failed attempts also
respect the interval, so retries cannot query the upstream more than once per minute.
A failed refresh may return
the last cached snapshot with `stale=true` and a non-empty `refresh_error`; stale data is never
presented as a successful fresh refresh.

The raw CPA `/v1/models` endpoint remains CPA's global model catalog. NewAPI
must use the plugin `/models` endpoint as the policy-scoped source of truth.

## Security properties

- full upstream API keys are never returned by the runtime inspection API;
- OAuth tokens used by quota requests are substituted inside the CPA host and are never returned to the plugin or NewAPI;
- account API keys are exposed only as masked previews;
- physical credential JSON is read only for file-backed migration data;
- downstream API keys are returned only by the authenticated generic plugin
  configuration endpoint because the UI must preserve editable configuration;
- a missing cache entry fails closed for restricted policies;
- an incompatible host causes plugin registration to fail.

See [UPGRADE.md](UPGRADE.md) for migration, release, upgrade, and rollback steps.
