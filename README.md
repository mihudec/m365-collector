# m365-collector

A Go collector for the Microsoft 365 Management Activity API. Emits one original audit event per line (NDJSON) on stdout for Vector's exec source. Diagnostics go to stderr. Supports commercial Microsoft 365 tenants, client credentials, Cobra flags, strict YAML configuration, and durable SQLite checkpoints. No CGO required.

## Setup

1. Register an Entra application and grant **Office 365 Management APIs → Application permissions → ActivityFeed.Read**, with tenant admin consent. For `DLP.All`, add the applicable `ActivityFeed.ReadDlp` permission; sensitive DLP details require additional authorization. Enable unified audit logging in the tenant.
2. Create a client secret and supply it through `M365_CLIENT_SECRET`, or an age-encrypted file as described below.
3. Build with Go 1.25 or later: `make build`. Binaries are under `dist/{linux,macos}/{amd64,arm64}/`.
4. Copy `examples/config.yaml` to your deployment configuration, set tenant/application IDs, and choose a persistent state directory writable by the service account.
5. Enable subscriptions once, then start collecting:

```sh
export M365_TENANT_ID='your-tenant-uuid'
export M365_CLIENT_ID='your-application-uuid'
# Inject M365_CLIENT_SECRET through your service manager or secret manager.
./dist/macos/arm64/m365-collector --config config.yaml subscribe
./dist/macos/arm64/m365-collector --config config.yaml collect
```

`subscribe` lists subscriptions and starts only missing/disabled types. It is deliberately separate from polling to avoid repeatedly starting subscriptions on restarts. Allow up to 12 hours for initial content after subscription creation. Do not repeatedly invoke subscription setup within 15 minutes after a start request.

## Vector

Use [examples/vector.yaml](examples/vector.yaml) for streaming exec. The `run` command polls every five minutes and exits on errors so Vector can restart it. This avoids scheduled exec's hard runtime limit when catching up on a backlog. Supply credentials in Vector's service environment, or configure age files. Use an absolute, persistent `state_path`, including across container replacement. Each concurrently running collector needs its own state path unless it is meant to be rejected by the lock.

For scheduled execution, replace `run` with `collect`, set `mode: scheduled`, remove `streaming`, and add:

```yaml
scheduled:
  exec_interval_secs: 900
```

Choose an interval long enough for a complete poll: Vector kills scheduled commands exceeding this time. Keep `include_stderr: false`, JSON decoding, and newline framing. The example raises both input buffer and frame limits to accommodate large events. Monitor process failures through Vector/service logs. Attach your normal transforms and sinks to the `m365` source.

## Configuration

Precedence is flags > environment > YAML > defaults. `--config` selects a YAML file; without it, defaults and environment are used. Unknown YAML keys and multiple documents are rejected. Relative paths resolve from the working directory. Secrets are deliberately unavailable as CLI flags or plaintext YAML keys.

| YAML | Environment | Flag/default |
| --- | --- | --- |
| `tenant_id` | `M365_TENANT_ID` | `--tenant-id` (required UUID) |
| `client_id` | `M365_CLIENT_ID` | `--client-id` (required UUID) |
| — | `M365_CLIENT_SECRET` | Environment secret takes precedence over age file |
| `secret_file` | `M365_SECRET_FILE` | Binary age-encrypted client secret |
| `identity_file` | `M365_AGE_IDENTITY_FILE` | age identity file |
| `state_path` | `M365_STATE_PATH` | `--state-path`, `state/collector.db` |
| `content_types` | — | `--content-types`, four Audit types |
| `lookback` | — | `--lookback`, `24h`, max `144h` |
| `overlap` | — | `--overlap`, `1h`, max `24h` |
| `lag` | — | `--lag`, `5m`, max `24h` |
| `interval` | — | `--interval`, `5m`, run mode only |
| `timeout` | — | `--timeout`, `60s` per HTTP request |

Durations use Go syntax such as `30s`, `5m`, or `24h`. Supported content types: `Audit.AzureActiveDirectory`, `Audit.Exchange`, `Audit.SharePoint`, `Audit.General`, and optional `DLP.All`. `--help` and `--version` describe the CLI/build.

## Checkpoints and delivery

SQLite stores a cursor per hashed tenant/content-type scope and SHA-256 hashes of completed content IDs. It never stores tokens, secrets, content URLs, or audit event bodies. The new state directory is mode 0700 and database mode 0600. SQLite uses full synchronous commits; an OS file lock prevents concurrent processes using the same state path. Use a local filesystem, not a network share. Completion hashes are retained for eight days.

The initial range starts `lookback` before `now - lag`; its lower bound is persisted before network access. Polls scan at most 24 hours per request, follow every `NextPageUri`, and rescan `overlap` before the saved cursor to tolerate delayed listing visibility. Already completed blobs are skipped. The window cursor advances only after all its pages succeed; completed blobs are saved independently so a later page failure does not repeat them. Events are selected by **content availability time**, not their audit timestamps.

Each blob is marked complete only after every event was successfully written to stdout. If output fails, or the process crashes between output and commit, that blob is replayed. Duplicate event IDs across different blobs are also possible: deduplicate downstream using tenant/organization plus event `Id` where needed. An overlap is a bounded rescan, not a guarantee against arbitrarily late API visibility.

**A successful pipe write is not a Vector or sink acknowledgment.** This tool cannot guarantee end-to-end at-least-once or exactly-once delivery through exec. A Vector crash after accepting bytes but before persisting them can lose events; use appropriate Vector buffering and sink delivery policies, understanding that buffering does not close this acknowledgment gap. No plaintext spool is written.

A checkpoint outside the API's seven-day availability window fails explicitly. Investigate the outage, then select a new state path with an appropriate `lookback` to deliberately resume within retention; keep the old database for inspection. This accepts an unrecoverable API gap and may replay recent events. To replay intentionally, stop the collector and use a new state path. Never delete a database while a collector uses it.

HTTP 429 and 5xx responses retry up to five attempts, honoring `Retry-After` up to five minutes; a 401 refreshes the token once. Other failures exit nonzero. Responses larger than 64 MiB fail without advancing that blob. Transport failures are retried by the next invocation. Content/pagination URLs must remain under the configured tenant feed on the API origin, and redirects are rejected. HTTP response bodies are not included in errors to avoid leaking sensitive data.

## age-encrypted secrets

For persistence, encrypt the client secret with [age](https://github.com/FiloSottile/age). The collector accepts binary age files and native identity files; passphrase/plugin/SSH/armored workflows are not supported here. For example, provision an identity with `age-keygen -o identity.key`, then encrypt a secret supplied on stdin:

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

- [Microsoft Activity API reference](https://learn.microsoft.com/en-us/office/office-365-management-api/office-365-management-activity-api-reference)
- [Microsoft application registration and permissions](https://learn.microsoft.com/en-us/office/office-365-management-api/get-started-with-office-365-management-apis)
- [Vector exec source](https://vector.dev/docs/reference/configuration/sources/exec/)
- [Original office365-audit-log-collector inspiration](https://github.com/ddbnl/office365-audit-log-collector)
