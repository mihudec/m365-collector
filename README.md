# m365-collector

A Go collector for Microsoft Graph Entra sign-in logs and the Microsoft 365 Management Activity API. Emits one original audit event per line (NDJSON) on stdout for Vector's exec source. Diagnostics go to stderr. Supports commercial Microsoft 365 tenants, client credentials, Cobra flags, strict YAML configuration, and durable SQLite checkpoints. No CGO required.

## Entra sign-in logs (Microsoft Graph)

The primary example uses `source: graph` and the stable `v1.0/auditLogs/signIns` endpoint. It emits original Graph sign-in objects, including `id`, `createdDateTime`, `userPrincipalName`, `ipAddress`, and `status` when present. `status.errorCode == 0` indicates success; nonzero codes describe authentication failures. Optional fields depend on licensing and permissions.

1. Your tenant needs **Microsoft Entra ID P1 or P2** for Graph sign-in downloads (P1 is included in Microsoft 365 Business Premium and Microsoft 365 E3).
2. Register an Entra application and grant **Microsoft Graph → Application permissions → AuditLog.Read.All**, with admin consent. To include applied Conditional Access policies, also grant `Policy.Read.ConditionalAccess`. These are separate from Office 365 Management API permissions.
3. Inject `M365_CLIENT_SECRET` into the service environment, or configure the age-encrypted secret described below.
4. Build with Go 1.25 or later: `make build`. Copy `examples/config.yaml` to your deployment configuration, set your tenant/application UUIDs, and choose a persistent state directory.
5. Run collection directly—Graph does **not** require `subscribe`:

```sh
export M365_TENANT_ID='your-tenant-uuid'
export M365_CLIENT_ID='your-application-uuid'
./dist/macos/arm64/m365-collector --config config.yaml collect --source graph
# Continuous polling for Vector:
./dist/macos/arm64/m365-collector --config config.yaml run --source graph
```

The stable API mode supports **interactive user sign-ins**. It must not be interpreted as a complete export of all Entra sign-in categories. To collect non-interactive user or workload sign-ins, explicitly opt into Microsoft's **beta API**, which can change and is not supported by Microsoft for production applications:

```yaml
source: graph
graph_version: beta
signin_types:
  - interactiveUser
  - nonInteractiveUser
  - servicePrincipal
  - managedIdentity
```

Each category is queried explicitly using `signInEventTypes`; Graph otherwise defaults to interactive sign-ins. Stable mode rejects additional categories rather than silently omitting them. Separate cursors are used per tenant, API version and category; changing version intentionally recollects the initial lookback. Graph pagination follows `@odata.nextLink` unchanged and rejects links outside the sign-in endpoint on the same origin. Graph tokens use the Graph audience and are not reused for the Activity API.

Graph polls in one-hour windows on **event `createdDateTime`**, ending at `now - lag`. It rescans `overlap` before the cursor to capture late arrivals. A cursor advances only after all pages succeed, regardless of Graph's descending response order. Each event ID is hashed and saved after a successful stdout write, so restarts and overlap scans skip completed events. Initial lower bounds are persisted before requests. Only hashes and timestamps are stored, not user details or event bodies. Completion hashes are kept for 31 days for both sources.

With P1/P2, normal Entra sign-in retention is 30 days. Graph allows up to `696h` (29 days) initial lookback; a cursor/overlap outside the 30-day window fails explicitly. Exporting regularly permits longer downstream retention. Overlap is bounded: records appearing later than that overlap can be missed. Updates to an already emitted sign-in ID are not re-emitted. Use a fresh state path to replay records or inspect updated data. The stdout delivery limitations below apply equally to Graph.

## Existing Microsoft 365 Activity API collection

Existing configurations without `source` continue to use `activity` for backward compatibility; the shipped example explicitly selects Graph. To collect audit activity instead:

```sh
m365-collector --config config.yaml subscribe --source activity
m365-collector --config config.yaml collect --source activity
```

Enable unified audit logging, grant **Office 365 Management APIs → Application permissions → ActivityFeed.Read** with admin consent, and enable the configured feeds using `subscribe`. `DLP.All` requires applicable DLP permissions. `subscribe` checks existing feeds and starts only missing/disabled ones; it does not modify local checkpoints. Initial content can take up to 12 hours. Avoid repeatedly starting a subscription within 15 minutes. Subscriptions are not used for Graph; `subscribe --source graph` exits with an explanatory error without making a request.

Run separate Vector sources/processes for Graph and Activity if both are needed, with distinct state paths to avoid the process lock. Raw event schemas differ (`id` vs `Id`), so route them using the Vector source identity. The same SQLite schema remains compatible with existing checkpoints, and the Graph scopes cannot collide with Activity scopes.

## Vector

Use [examples/vector.yaml](examples/vector.yaml) for streaming exec. The `run` command polls every five minutes and exits on errors so Vector can restart it. This avoids scheduled exec's hard runtime limit when catching up on a backlog. Supply credentials in Vector's service environment, or configure age files. Use an absolute, persistent `state_path`, including across container replacement. Each concurrently running collector needs its own state path unless it is meant to be rejected by the lock.

For scheduled execution, replace `run` with `collect`, set `mode: scheduled`, remove `streaming`, and add:

```yaml
scheduled:
  exec_interval_secs: 900
```

Choose an interval long enough for a complete poll: Vector kills scheduled commands exceeding this time. Keep `include_stderr: false`, JSON decoding, and newline framing. The example raises both input buffer and frame limits to accommodate large events. Monitor process failures through Vector/service logs. Attach your normal transforms and sinks to the `m365` source.

## Separate source events from Vector metadata

Set `wrap: entra` in the collector YAML, or pass `--wrap entra` to `collect` or `run`:

```sh
m365-collector --config config.yaml run --wrap entra
```

An original event `{"id":"example","status":{"errorCode":0}}` is emitted as:

```json
{"entra":{"id":"example","status":{"errorCode":0}}}
```

Vector can then add its own root metadata while the original payload stays under `.entra`. Update `entra.vrl` references accordingly, for example `.entra.userPrincipalName` and `.entra.status.errorCode`. The same option works for Activity API events.

Wrapping is disabled by default. The CLI overrides YAML; `--wrap=""` explicitly disables a configured wrapper. The key is literal (`entra.raw` is one key, not a nested path). Choose a key that does not conflict with Vector metadata keys. Changing wrapping does not reset checkpoints or replay previously emitted events. Restart the collector after changing its YAML configuration.

## Configuration

Precedence is flags > environment > YAML > defaults. `--config` selects a YAML file; without it, defaults and environment are used. Unknown YAML keys and multiple documents are rejected. Relative paths resolve from the working directory. Secrets are deliberately unavailable as CLI flags or plaintext YAML keys.

| YAML | Environment | Flag/default |
| --- | --- | --- |
| `wrap` | — | `--wrap`, empty (disabled) |
| `source` | `M365_SOURCE` | `--source`, `activity` (example selects `graph`) |
| `graph_version` | — | `--graph-version`, `v1.0`; opt-in `beta` |
| `signin_types` | — | `--signin-types`, `interactiveUser` |
| `tenant_id` | `M365_TENANT_ID` | `--tenant-id` (required UUID) |
| `client_id` | `M365_CLIENT_ID` | `--client-id` (required UUID) |
| — | `M365_CLIENT_SECRET` | Environment secret takes precedence over age file |
| `secret_file` | `M365_SECRET_FILE` | Binary age-encrypted client secret |
| `identity_file` | `M365_AGE_IDENTITY_FILE` | age identity file |
| `state_path` | `M365_STATE_PATH` | `--state-path`, `state/collector.db` |
| `content_types` | — | `--content-types`, four Audit types |
| `lookback` | — | `--lookback`, `24h`, max `144h` for Activity / `696h` for Graph |
| `overlap` | — | `--overlap`, `1h`, max `24h` |
| `lag` | — | `--lag`, `5m`, max `24h` |
| `interval` | — | `--interval`, `5m`, run mode only |
| `timeout` | — | `--timeout`, `60s` per HTTP request |

Durations use Go syntax such as `30s`, `5m`, or `24h`. Supported content types: `Audit.AzureActiveDirectory`, `Audit.Exchange`, `Audit.SharePoint`, `Audit.General`, and optional `DLP.All`. `--help` and `--version` describe the CLI/build.

## Activity checkpoints and shared delivery limitations

SQLite stores a cursor per hashed tenant/content-type scope and SHA-256 hashes of completed content IDs. It never stores tokens, secrets, content URLs, or audit event bodies. The new state directory is mode 0700 and database mode 0600. SQLite uses full synchronous commits; an OS file lock prevents concurrent processes using the same state path. Use a local filesystem, not a network share. Completion hashes are retained for 31 days.

The initial range starts `lookback` before `now - lag`; its lower bound is persisted before network access. Polls scan at most 24 hours per request, follow every `NextPageUri`, and rescan `overlap` before the saved cursor to tolerate delayed listing visibility. Already completed blobs are skipped. The window cursor advances only after all its pages succeed; completed blobs are saved independently so a later page failure does not repeat them. Events are selected by **content availability time**, not their audit timestamps.

Each blob is marked complete only after every event was successfully written to stdout. If output fails, or the process crashes between output and commit, that blob is replayed. Duplicate event IDs across different blobs are also possible: deduplicate downstream using tenant/organization plus event `Id` where needed. An overlap is a bounded rescan, not a guarantee against arbitrarily late API visibility.

**A successful pipe write is not a Vector or sink acknowledgment.** This tool cannot guarantee end-to-end at-least-once or exactly-once delivery through exec. A Vector crash after accepting bytes but before persisting them can lose events; use appropriate Vector buffering and sink delivery policies, understanding that buffering does not close this acknowledgment gap. No plaintext spool is written.

A checkpoint outside the API's seven-day availability window fails explicitly. Investigate the outage, then select a new state path with an appropriate `lookback` to deliberately resume within retention; keep the old database for inspection. This accepts an unrecoverable API gap and may replay recent events. To replay intentionally, stop the collector and use a new state path. Never delete a database while a collector uses it.

HTTP 429 and 5xx responses retry up to five attempts, honoring `Retry-After` up to five minutes; a 401 refreshes the token once. Other failures exit nonzero. Responses larger than 64 MiB fail without advancing that blob. Transport failures are retried by the next invocation. Content/pagination URLs must remain under the configured tenant feed on the API origin, and redirects are rejected. HTTP response bodies are not included in errors to avoid leaking sensitive data.

## age-encrypted secrets

For persistence, encrypt the client secret with [age](https://github.com/FiloSottile/age). The collector accepts binary age files and native X25519 or hybrid post-quantum identity files (age v1.3+); passphrase/plugin/SSH/armored workflows are not supported here. For example, provision an identity with `age-keygen -o identity.key`, then encrypt a secret supplied on stdin:

```sh
# The secret manager writes only the secret to stdout.
secret-manager-command | age -r age1YOUR_RECIPIENT -o client-secret.age
chmod 600 identity.key client-secret.age
```

Set `secret_file` and `identity_file`, or their environment variables. Keep the private identity outside the repository, accessible only to the service account; storing it beside ciphertext under the same access controls provides no protection against compromise of that account. The secret and OAuth token remain in process memory. The tool does not persist event payloads; storage/encryption of downstream audit data is Vector's responsibility.

## Development and releases

```sh
make test       # unit and HTTP/SQLite integration tests; no tenant credentials
make check      # go vet and race-enabled tests
make build      # static Linux/macOS amd64/arm64 binaries
make fmt
```

The Makefile follows `faz-archive` conventions, including `VERSION`, `COMMIT`, and `BUILD_DATE` metadata. GitHub Actions tests both Linux and macOS. Pushing a `v*` tag runs checks and creates a GitHub release containing four tar.gz archives and SHA-256 checksums. No release is created by local builds.

Tests cover restart deduplication, write failure recovery, pagination failure, state locking, stale checkpoints, token refresh, retries, URL validation, environment precedence, YAML validation, and age decryption. Live Microsoft access requires your tenant credentials and has not been exercised by these tests.

## References

- [Graph sign-in API](https://learn.microsoft.com/en-us/graph/api/signin-list?view=graph-rest-1.0)
- [Graph beta sign-in categories](https://learn.microsoft.com/en-us/graph/api/signin-list?view=graph-rest-beta)
- [Entra sign-in licensing](https://learn.microsoft.com/en-us/graph/api/resources/signin?view=graph-rest-1.0)
- [Microsoft Activity API reference](https://learn.microsoft.com/en-us/office/office-365-management-api/office-365-management-activity-api-reference)
- [Microsoft application registration and permissions](https://learn.microsoft.com/en-us/office/office-365-management-api/get-started-with-office-365-management-apis)
- [Vector exec source](https://vector.dev/docs/reference/configuration/sources/exec/)
- [Original office365-audit-log-collector inspiration](https://github.com/ddbnl/office365-audit-log-collector)
