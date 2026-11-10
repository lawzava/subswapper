# OpenCode built-in OpenAI through Subswapper

Version **0.2.0**, targeting **OpenCode 2.2.6**, with the real
`@opencode/plugin@2.2.6` SDK pinned in `package-lock.json`. Requires Node 24+
for development and POSIX credential permissions. This is a local server
plugin, not a published npm package or a new model provider.

## Install and configure

First configure Subswapper's Codex accounts and run its proxy. On a hub client,
configure a persistent `proxy_listen` relay and run `subswapper proxy`; a
launcher's temporary relay does not serve an independently running OpenCode
server. The plugin does not start Subswapper or create credentials.

Copy `index.ts`, `catalog.ts`, `private-group.ts`, `package.json` and `package-lock.json` into a private, immutable
versioned installation directory, for example
`~/.local/share/subswapper-opencode/0.2.0/`. Install its runtime dependencies:

```sh
npm ci --omit=dev --ignore-scripts --no-audit --no-fund
```

Run that command in the installed directory. Do not copy credential files.
From any directory, this **read-only** helper prints a standalone JSON config snippet:

```sh
subswapper setup opencode -plugin "$HOME/.local/share/subswapper-opencode/0.2.0" \
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
    "package": "/absolute/path/to/subswapper-opencode/0.2.0",
    "options": { "config": "/absolute/path/to/subswapper/config.json", "service": "codex" }
  }]
}
```

The plugin enables built-in `openai` directly. No `OPENAI_API_KEY` or dummy key
is required. For the config-only live installation, remove an existing
`OPENAI_API_KEY=subswapper-dummy` from the **OpenCode server's** managed
environment when its profile has no stored real OpenAI credential. An existing
dummy key remains compatible but is unnecessary. Do not remove or overwrite a
real key without accepting that direct API access in that server will change.
Use an isolated server/profile when direct OpenAI access must remain.
Do not activate OpenCode's legacy ChatGPT or token-sharing OAuth connections
alongside this plugin. Their built-in routing may override the model URL; the
final HTTP guard then fails closed instead of silently sending a direct call.
Providers other than `openai` and `claude-agent` are unchanged. This plugin does not copy, refresh, remove, or
inspect OpenCode OAuth credentials.

Discovery uses the plugin's `config` option, then `SUBSWAPPER_CONFIG`, then
Subswapper's platform/XDG config default. `service` defaults to `codex`.
The service's numeric-loopback `proxy_listen` supplies the origin. The default
setup uses `127.0.0.1:7879`. The placeholder lives beside the resolved
`state_path`, at `tokens/<sanitized-service>/proxy-placeholder.json`, matching
Subswapper's defaults and service-name hashing.

The plugin `config` option alone derives both the proxy origin and placeholder
path. Normal live installations need neither override below. Remove both
`SUBSWAPPER_CODEX_PROXY_URL` and `SUBSWAPPER_CODEX_PLACEHOLDER` from the server's
managed environment so stale overrides cannot bypass the configured route.
Do not inspect or copy the placeholder value.

Keep explicit overrides only for separately isolated, approved live acceptance:

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

- Built-in `openai` uses the private bridge as its provider-level base URL and
  a private `x-subswapper-bridge` capability header on providers and models.
  This covers native calls that have no session hooks,
  including stateless image and decision generation. Session hooks also guard
  HTTP and WebSocket destinations. Model overrides cannot restore a public API
  URL, unavailable text model, paid cost estimate, or `pro` reasoning variant.
  Compression is disabled. Native compaction stays enabled; the configured
  HTTP/WebSocket transport is retained.
- OpenCode's native conversion drops Web `Request.redirect`. Therefore a
  private per-plugin loopback bridge, on an ephemeral port, owns credential
  attachment. Native OpenCode requests carry a random bridge capability, **not
  the proxy placeholder**. Authentication uses the private header, not
  `Authorization`, which native stateless calls can replace with the dummy
  integration key. Neither that key nor a Bearer header alone grants bridge
  access. The bridge strips its capability header before forwarding and streams
  only to the configured proxy's `/backend-api/codex` tree using Node HTTP,
  which never follows redirects. It converts
  every 3xx into a secret-free 502 with no `Location` header. This guarantee is
  tested across the SDK's real Effect HTTP conversion and dispatch.
- The bridge accepts only capability-authenticated canonical ASCII paths,
  with GET, HEAD, POST, PUT, PATCH, DELETE or OPTIONS. It forwards queries,
  content types, bodies and error/stream bytes, preserves
  cancellation/backpressure, strips hop-by-hop headers, and closes on plugin
  cleanup. It is not another persistent service and does not log credentials,
  bodies, headers, file paths or URLs. One adaptation is required: native image
  edits with owned image bytes become the SDK's ChatGPT JSON-edits form, with
  inline data URLs. Multipart edits are bounded to the proxy's 64 MiB replay
  limit. Images are not downloaded during conversion.
- Authenticated WebSocket upgrades stream in both directions through the bridge
  and Codex proxy. Each socket stays bound to the account selected at handshake;
  an account switch affects the next connection, not frames already in flight.
  Failed handshakes and redirects never open another destination. Upgrade
  support requires the updated Go proxy on the account-holding machine.
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
  Session retry hooks disable retries. Bridge responses also set
  `x-should-retry: false`, overriding backend hints, so native stateless image
  and decision calls do not retry independently. Statuses and bodies remain
  unchanged; the proxy owns bounded account failover.
  Codex quota-marker 429s and 401s may try another account before client bytes
  are sent. Transient 429s are relayed unchanged. Claude policy is unchanged.
- Titles, instructions and tools are not rewritten. Session output-token
  overrides are removed, matching OpenCode's ChatGPT route contract. Automatic
  title/compaction/generation can consume subscription usage. For bounded
  acceptance use a fixed CLI `--title`, disable warming and auto-compaction,
  and explicitly authorize every inference budget.

## Catalog and request coverage

At plugin load, `GET /models?client_version=2.2.6` goes through the bridge and
selected-account proxy. Only returned text model IDs are exposed. OpenCode's
catalog supplies exact-ID dates and limits; backend context/output limits and
reasoning efforts take precedence when provided. Unknown dates stay `0`, not a
guessed release date. Dollar costs are zero and `pro` reasoning modes are removed.
If discovery fails, text models fail closed instead of falling back to API-key
inventory. Reload the plugin after changing backend availability.

All existing OpenAI image catalog entries remain listed, plus `gpt-image-2`,
`gpt-image-2.5-flare` and `gpt-image-2.5-sunburst`. The text inventory does not
decide image availability. Listing is not proof of subscription entitlement.
Backend errors reach the caller with no direct-API retry or model substitution.

Claude discovery uses `OPENCODE_CLAUDE_PATH`, or `claude` on PATH. Point that
variable at the existing Subswapper Claude shim to use the selected subscription.
The plugin sends only the CLI's SDK `initialize` control request, never a user
prompt. Hooks, tools, MCP configuration and session persistence are disabled for
this bounded discovery process. It retains only model metadata, not account or
login fields. Discovery times out after 15 seconds and limits stdout to 1 MiB.
The last successful metadata snapshot is the fallback when discovery fails, for
at most 24 hours. Older or future-dated snapshots are ignored.

Aliases keep their IDs and request values, but show the resolved version in
their visible names. Exact version IDs are also exposed and passed unchanged to
the Claude harness. Dates and output limits come only from matching OpenCode
catalog entries; unknown values stay `0`. Runtime-supported efforts become
variants, including `xhigh` and `max` when advertised. No family-version guessing
or fast-mode surcharge variant is added. This does not create or change an
OpenCode OAuth/external credential.

Secret-free plugin storage records provide discovery provenance:

- `catalog-discovery-v1`: `checkedAt` (epoch milliseconds), `openai.source`
  (`backend` or `unavailable`), `openai.wireModelIDs`, `openai.textFallback`
  (always false); `claude.source` (`runtime`, `cache`, or `unavailable`),
  `claude.fetchedAt`, `claude.ageMs`, `claude.maxCacheAgeMs`, and resolved model
  identity/name records. An unavailable Claude source leaves its native catalog
  unchanged, rather than inventing alias mappings.
- `claude-models-v1`: `{ fetchedAt, models }`, containing only sanitized CLI
  model metadata. It contains no login, account, token, URL or bridge capability.

Read these through OpenCode's existing plugin-storage interface in the isolated
profile being checked. Backend source means a successful local-proxy inventory
response, not proof a later inference or image generation will succeed.

| Request family | Route and boundary |
| --- | --- |
| Chat, tools, continuations, titles | HTTP or WS `/responses`; session guards plus provider bridge |
| Native compact | HTTP `/responses/compact`; no forced summary substitute |
| Image generation / editing | `/images/generations`, `/images/edits`; provider bridge even without session hooks |
| Hosted image tool | `/responses`, same bridge |
| Native decision / speech / transcription | `/decisions`, `/audio/speech`, `/audio/transcriptions`; provider bridge, backend support not assumed |
| Discovery / upload / file content | Canonical `/models`, `/files`, `/uploads` paths are bridged; no automatic upload claim |
| Other origins / encoded or malformed paths | Rejected before attaching the placeholder |
| Experimental Voice / Realtime | **Unsupported and outside the provider bridge.** OpenCode 2.2.6 reads its API-key integration directly and calls public OpenAI Live/Realtime endpoints. ChatGPT sign-in is explicitly unsupported by its source. A dummy API key fails OpenAI authentication. Do not configure a real OpenAI key in this server if direct Voice traffic must be impossible. |

These are routing guarantees, not proof every ChatGPT endpoint accepts every
model or payload. OpenCode's artifact/media layer can download remote image URLs
outside provider hooks. For successful nonstreaming image responses, the bridge
rejects any non-null `data[].url` before it reaches that layer, even alongside
inline data. These JSON replies are bounded to 64 MiB.

Image SSE bytes pass through unchanged; the bridge does not inspect stream
frames. Safety here depends on the pinned native SDK's `openai-images` protocol:
generation and edit `completed` and `partial_image` events require `b64_json`.
URL-only events fail schema decoding before publishing an image asset. Events
with both fields decode the base64 into owned bytes and ignore `url`. Synthetic
real-SDK tests consume `ai.image.stream` and materialize its assets against a
loopback download sink, verifying zero downloads for both cases. Re-run this
coverage when upgrading the SDK. This is not a general SSE URL filter. There is
no global fetch patch; unrelated user web fetches and artifact URLs are not
intercepted.

## Diagnostics and development

```sh
subswapper doctor -config /absolute/path/to/subswapper/config.json \
  -opencode-plugin /absolute/path/to/subswapper-opencode/0.2.0
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
Tests use temporary synthetic credentials and loopback HTTP servers. The
plugin tests and shared runner clear every inherited `SUBSWAPPER_*` variable,
including live-test flags. Explicit synthetic fixtures remain local to their
tests. The plugin tests never launch a real Claude process or print credential
values in assertion diagnostics. Subprocess regressions verify isolation without
account or installed-proxy calls. Tests cover
native dispatch, redirects, streaming, cancellation, concurrency, unsafe files,
route guards, body fidelity and credential rotation, with no live model prompts.
Image SSE tests cover URL-only and inline-plus-URL frames for completed results
and partial previews, including artifact materialization. Claude fallback tests
accept fresh and exactly 24-hour cache snapshots, reject stale and future-dated
snapshots, and verify native catalog preservation when the cache is rejected.
Go race tests cover duplex upgrades, Codex quota fallback, exhaustion, 401s and observations.
Live tools, continuations, native compaction, account switches and quota
failover still need separate explicitly approved acceptance. Historical
single-prompt feasibility evidence is in [SPIKE.md](SPIKE.md).
