# OpenCode built-in OpenAI through Subswapper

Version **0.1.0**, targeting **OpenCode 2.2.6**, with the real
`@opencode/plugin@2.2.6` SDK pinned in `package-lock.json`. Requires Node 24+
for development and POSIX credential permissions. This is a local server
plugin, not a published npm package or a new model provider.

## Install and configure

First configure Subswapper's Codex accounts and run its proxy. On a hub client,
configure a persistent `proxy_listen` relay and run `subswapper proxy`; a
launcher's temporary relay does not serve an independently running OpenCode
server. The plugin does not start Subswapper or create credentials.

Copy `index.ts`, `private-group.ts`, `package.json` and `package-lock.json` into a private, immutable
versioned installation directory, for example
`~/.local/share/subswapper-opencode/0.1.0/`. Install its runtime dependencies:

```sh
npm ci --omit=dev --ignore-scripts --no-audit --no-fund
```

Run that command in the installed directory. Do not copy credential files.
From any directory, this **read-only** helper prints a standalone JSON config snippet:

```sh
subswapper setup opencode -plugin "$HOME/.local/share/subswapper-opencode/0.1.0" \
  -config "$HOME/.config/subswapper/config.json"
```

Append the resulting single entry to the server's existing canonical `plugins`
array, or use the snippet for a new configuration. Keep every existing entry
and setting; do not overwrite an existing array with the snippet. Installed
OpenCode 2.2.6 acceptance verified loading the canonical object shape below.
OpenCode 2.2.6 loads legacy `plugin` strings but skips object entries under that key.
Do not blindly merge this snippet into a configuration containing legacy
`plugin`; combined-key behavior is not verified here. A project-local
`.opencode/plugins/<directory>/` installation also loads automatically.
Do not install it both ways.

```json
{
  "$schema": "https://opencode.ai/config.json",
  "plugins": [{
    "package": "/absolute/path/to/subswapper-opencode/0.1.0",
    "options": { "config": "/absolute/path/to/subswapper/config.json", "service": "codex" }
  }]
}
```

Configure the **OpenCode server's** environment, not just a connecting CLI:

```sh
OPENAI_API_KEY=subswapper-dummy
```

The dummy key activates built-in `openai` without direct OAuth. It is not a
real API key and never reaches the ChatGPT backend. **Do not overwrite a real
`OPENAI_API_KEY` without accepting that direct API access in that server will
change.** Use an isolated server/profile when direct OpenAI access must remain.
Do not activate OpenCode's legacy ChatGPT or token-sharing OAuth connections
alongside this plugin. Their built-in routing may override the model URL; the
final HTTP guard then fails closed instead of silently sending a direct call.
Other providers are unchanged. This plugin does not copy, refresh, remove, or
inspect OpenCode OAuth credentials.

Discovery uses the plugin's `config` option, then `SUBSWAPPER_CONFIG`, then
Subswapper's platform/XDG config default. `service` defaults to `codex`.
The service's numeric-loopback `proxy_listen` supplies the origin. The default
setup uses `127.0.0.1:7879`. The placeholder lives beside the resolved
`state_path`, at `tokens/<sanitized-service>/proxy-placeholder.json`, matching
Subswapper's defaults and service-name hashing.

Explicit overrides, useful for isolated acceptance or alternate local relays:

```sh
SUBSWAPPER_CODEX_PROXY_URL=http://127.0.0.1:17879
SUBSWAPPER_CODEX_PLACEHOLDER=/absolute/private/path/proxy-placeholder.json
```

The URL is an **origin**, not a `/backend-api/codex` base URL. Only numeric
loopback HTTP origins are allowed. No DNS names, credentials, non-root paths,
queries, or fragments. When both overrides are set, no config is read. Supply
the placeholder **path only**, never its value. If your config is managed by a
dotfile tool, edit its source rather than creating a conflicting live file.
Server environment changes require that server's normal restart procedure;
loading a plugin into a connecting client does not change the server's route.

## Safety and behavior

- Only built-in `openai` is transformed: HTTP transport, no compression encoding,
  summary compaction. Native `/responses/compact`, WebSocket handshakes and
  destinations other than the exact configured JSON POST Responses URL fail.
- OpenCode's native conversion drops Web `Request.redirect`. Therefore a
  private per-plugin loopback bridge, on an ephemeral port, owns credential
  attachment. Native OpenCode requests carry a random bridge capability, **not
  the proxy placeholder**. The bridge streams only to the configured exact
  proxy endpoint using Node HTTP, which never follows redirects. It converts
  every 3xx into a secret-free 502 with no `Location` header. This guarantee is
  tested across the SDK's real Effect HTTP conversion and dispatch.
- The bridge accepts only its capability-authenticated JSON POST route. It
  forwards bodies and error/stream bytes without JSON adaptation, preserves
  cancellation/backpressure, strips hop-by-hop headers, and closes on plugin
  cleanup. It is not another persistent service and does not log credentials,
  bodies, headers, file paths or URLs.
- Each actual request reloads a bounded (16 KiB) regular `0600`, current-user
  placeholder file under a `0700` parent. All ancestors must be non-symlink
  directories owned by that user or root. World writes are rejected except on
  root-owned sticky ancestors. Group writes require the directory owner's
  exclusive primary group: its GID matches the owner's primary GID, its explicit
  member list is empty, and no other user has that primary GID. `O_NOFOLLOW` and descriptor
  identity/mode checks protect the leaf. Trusted owners and other loaded
  plugins remain part of the process trust boundary. Permission checks fail
  closed on Windows. The group-write exception requires Linux, `/usr/bin/getent`,
  and `passwd`/`group` NSS sources limited to `files` with optional `systemd`.
  An explicit `initgroups` entry must use the same supported sources.
  Identity queries have a two-second timeout and 1 MiB output limit. Missing,
  malformed, ambiguous or unsupported identity data fails closed; remote or
  `compat` NSS sources and other POSIX platforms cannot use this exception.
  No permission changes are made. Disable sensitive host/debug logs.
- The proxy replaces the placeholder with its selected account's real login.
  The placeholder still grants proxy access; treat it as a shared secret.
  OpenCode retries are disabled; the proxy owns bounded account failover.
  Codex quota-marker 429s and 401s may try another account before client bytes
  are sent. Transient 429s are relayed unchanged. Claude policy is unchanged.
- Titles, instructions, tools, token limits and model catalog costs are not
  rewritten. Automatic title/compaction/generation can consume subscription
  usage. Catalog dollar estimates are not proof of API billing. For bounded
  acceptance use a fixed CLI `--title`, disable warming and auto-compaction,
  and explicitly authorize every inference budget.

## Diagnostics and development

```sh
subswapper doctor -config /absolute/path/to/subswapper/config.json \
  -opencode-plugin /absolute/path/to/subswapper-opencode/0.1.0
```

This opt-in mode runs **only** read-only OpenCode checks: package/version and SDK
manifest pin, config route, safe existing placeholder, authenticated proxy health,
and selected account readiness. It does not create credentials, locks or state,
run usage probes, refresh accounts, or infer. Readiness uses cached usage and
stored-login shape, not a paid validation of upstream credentials. Hub clients
check through the fixed relay; the hub proxy must advertise `codex-readiness-v1`
in health before doctor probes the read-only readiness endpoint. Upgrade older
proxies or hubs first. Doctor does not verify installed SDK dependencies or
prove the plugin is loaded in a running
server or that an OAuth connection has not replaced its hooks. It checks the
config route, not temporary plugin environment overrides. Verify loading and
the server environment separately.

From the checkout's plugin directory:

```sh
npm ci --ignore-scripts --no-audit --no-fund
npm run typecheck
npm test
```

The shared full gate includes the typecheck and synthetic tests on Linux.
Install dependencies in both `paseo-plugin/` and `plugins/opencode/` first.
Tests use temporary synthetic credentials and loopback HTTP servers. They cover
native dispatch, redirects, streaming, cancellation, concurrency, unsafe files,
route guards, body fidelity and credential rotation, with no model prompts.
Go race tests cover Codex quota fallback, exhaustion, 401s and observations.
Live tools, continuations, summary compaction, account switches and quota
failover still need separate explicitly approved acceptance. Historical
single-prompt feasibility evidence is in [SPIKE.md](SPIKE.md).
