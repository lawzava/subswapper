# subswapper

[![CI](https://github.com/lawzava/subswapper/actions/workflows/ci.yml/badge.svg)](https://github.com/lawzava/subswapper/actions/workflows/ci.yml)
[![License: MIT](https://img.shields.io/badge/License-MIT-blue.svg)](LICENSE)

`subswapper` is a small Go CLI that manages isolated [Claude Code](https://claude.com/claude-code)
and [Codex](https://openai.com/codex/) account homes on one machine. Claude
launches use a separate long-lived setup token for each account. Subswapper
tracks trusted usage and chooses a home without changing credentials in an
existing process.

## Features

- **Permanent account homes**: creates private per-account directories for
  `CLAUDE_CONFIG_DIR` and `CODEX_HOME`, with commands to log in, print the
  environment, and launch a client in the selected home.
- **Live usage tracking**: uses fresh provider usage data. Claude setup-token
  accounts fall back to Claude's normal status-line response data when the
  OAuth usage endpoint rejects inference-only tokens.
- **Automatic account selection**: a monitor loop can change the preferred
  account when the selected one crosses a configurable usage threshold. It
  never changes a running provider's credentials.
- **Switching without a restart**: an optional loopback auth proxy keeps real
  credentials out of launched processes and sends each request with the
  selected account's token. Running Claude and Codex sessions pick up a switch
  on their next request. Rate-limit responses reach the client unchanged.
- **Safe by design**: cross-process locking, private `0700` homes, atomic
  credential persistence, and non-destructive migration from old snapshots.
- **Extensible**: any other service can be managed by listing its credential
  files in the config; plug in a custom `usage_command` for usage probing.

## Installation

Requires Go 1.26.6 or newer.

```sh
go install github.com/lawzava/subswapper/cmd/subswapper@latest
```

Or build from source:

```sh
git clone https://github.com/lawzava/subswapper.git
cd subswapper
go build ./cmd/subswapper
```

For Codex usage probing, the `codex` CLI must be on `PATH` (see
[Usage probes](#usage-probes)).

## Quick start

```sh
# Create the default config (Claude Code + Codex)
subswapper init

# Create permanent homes
subswapper home create -service claude -account personal
subswapper home create -service codex  -account personal
subswapper home login  -service codex  -account personal

# Store each pre-created Claude setup token through a hidden prompt
subswapper home token set -service claude -account personal
subswapper home create -service claude -account work
subswapper home token set -service claude -account work

# See every account's usage windows
subswapper status

# Select a preferred account, or let subswapper pick the best one
subswapper switch -service claude -account work
subswapper switch -service all -account auto

# Run a client with the selected account home
subswapper home run -service claude -- claude

# Add missing shared user-configuration links to an existing Claude home
subswapper home repair -service claude -account work

# Keep the preferred route current in the background
subswapper monitor
```

`status` prints one row per registered account:

```
subswapper status 2026-07-02T14:07:31Z

SERVICE    ACCOUNT                  SELECTED  5H                           WEEKLY                       FABLE5                       SCORE    STATE
-------    -------                  --------  --                           ------                       ------                       -----    -----
claude     personal                 yes     62% reset Jul02 15:00        31% reset Jul05 23:00        18% reset Jul05 23:00        62%      ready
claude     work                             12% reset Jul02 19:00        8% reset Jul07 11:00         4% reset Jul07 11:00         12%      ready
codex      personal                 yes     91% reset Jul02 16:30        44% reset Jul06 09:00        -                            91%      ready
```

`FABLE5` is the weekly window scoped to Claude's Fable models (`-` until a
Fable response has been seen through the proxy). `SCORE` is the worst of an account's windows; it breaks
ranking ties and paces switches away from an account at the threshold.

## Commands

| Command | Description |
| --- | --- |
| `init` | Write a starter config file. |
| `home create -service <name> -account <name> [-email <label>]` | Create and register a private account home. New Claude homes inherit allowlisted user configuration. |
| `home repair -service claude [-account <name>]` | Add missing allowlisted user-configuration links without replacing existing entries. |
| `home token set\|status\|remove -service claude [-account <name>]` | Manage a Claude setup token without printing its value. `set` accepts the token only through stdin or a hidden prompt. |
| `home login -service <name> [-account <name>]` | Run the provider's legacy login command in that account home. Do not use this for setup-token routing. |
| `home path\|env -service <name> [-account <name>]` | Print a home path or shell export for configuring other tools. |
| `home run -service <name> [-account <name>] [-- command...]` | Run a command with the selected account's home environment, or through the service's proxy when one is configured. |
| `home proxy-auth -service codex` | Move a real ChatGPT login out of the Codex runtime home and install the proxy placeholder login (backup kept). |
| `home migrate` | Copy legacy snapshots into native home filenames without deleting or overwriting files. |
| `capture -service <name> -account <name> [-email <label>]` | Import the current login into a home; retained for migration and bundle-mode services. |
| `switch -service <name> [-account <name>\|auto]` | Change the preferred route; `auto` picks the best healthy account (see [How auto-switching works](#how-auto-switching-works)). |
| `switch -service all -account auto` | Auto-pick the best account for every service at once. |
| `status` (alias `list`) | Show every captured account with usage windows, score, and state. |
| `monitor [-interval 5m] [-once] [-no-auto] [-verbose] [-proxy]` | Poll usage on a loop and auto-switch when thresholds are hit. Continuous mode logs events; `-verbose` prints every table; `-proxy` also serves every configured auth proxy. |
| `proxy [-service <name>] [-listen 127.0.0.1:7878]` | Serve the auth proxies configured by `proxy_listen`; `-listen` overrides one service's address. |
| `hub export [-service <name>] [-host <name>] -out <file\|->` | Write a bundle that lets another machine use this machine's proxies over Tailscale (see [Hub mode](#hub-mode-across-machines)). |
| `hub import -in <file\|->` | Make this machine a hub client from a bundle. |
| `remove -service <name> -account <name> [-force] [-delete-home]` (alias `rm`) | Unregister an account; preserve its home unless deletion is explicit. Remove a Claude setup token first. |
| `import-cswap [-root <dir>]` | Import accounts from an existing claude-swap (`cswap`) install. |
| `version` | Print the subswapper version. |

All commands accept `-config <path>` (default
`~/.config/subswapper/config.json` on Linux).

## Using homes with external launchers

Configure any process supervisor or agent launcher to start Claude through
Subswapper:

```sh
subswapper home run -service claude -- claude
```

Subswapper selects the routed account for each new process. It injects the
account's setup token, a controlled `CLAUDE_CONFIG_DIR`, and, for fixed-token
launches, `CLAUDE_CODE_SUBPROCESS_ENV_SCRUB=1`. Proxy launches skip the scrub
unless the service sets `proxy_env_scrub: true`: Claude Code treats the scrub
as a hard sandbox, runs every Bash command confined, ignores
`dangerouslyDisableSandbox`, and masks `~/.gnupg` and `~/.ssh`, which breaks
signed commits. It removes conflicting Anthropic,
Bedrock, Vertex, and Foundry variables described by Anthropic's
[environment guide](https://code.claude.com/docs/en/env-vars). Changing the
route does not change an existing agent.

For Codex launchers that support shadow homes, use a shared `CODEX_HOME`
(normally `~/.codex`) and each Subswapper Codex account path as its shadow
home. This keeps every `auth.json` private while the launcher shares sessions.

Without the proxy below, a process keeps the token it was launched with.
Start a new process through `home run` to use a newly selected account.

### Live account swapping through the local proxy

Set `proxy_listen` on the Claude service and run the proxy next to the
monitor:

```json
{
  "name": "claude",
  "kind": "claude",
  "account_mode": "home",
  "proxy_listen": "127.0.0.1:7878",
  "shared_runtime_home": "native"
}
```

```sh
subswapper monitor -interval 5m -proxy   # or: subswapper proxy
subswapper home run -service claude -- claude
```

While the proxy is reachable, `home run` launches Claude with
`ANTHROPIC_BASE_URL` pointing at the proxy and a per-install placeholder
secret in `CLAUDE_CODE_OAUTH_TOKEN`. Real setup tokens never enter the
process environment. For every request the proxy replaces the bearer token
with the selected account's token, so `switch` and the monitor take effect on
the next request of every running session. If the proxy is down, `home run`
warns and falls back to a fixed-token launch.

The proxy reads Anthropic's `anthropic-ratelimit-unified-*` response headers
and stores them as `proxy_usage` for the account that served the request.
This is the usage source for setup-token accounts, which the OAuth usage
endpoint rejects. The `5h` and `7d` windows arrive on every response. The
`7d_oi` window (Anthropic's "7-day overage-included" claim, shown by Claude
Code as the Fable limit) arrives only on responses served by a Fable model;
it fills the `FABLE5` column, counts toward the score, and is kept across
responses from other models because those do not consume it. An unused
account keeps its last sample; a window counts as 0% once its reset time
passes, and the proxy corrects the estimate on the first real response.

When Anthropic rejects a request for a usage limit
(`anthropic-ratelimit-unified-*-status: rejected`, or HTTP 429 with the
unified headers), the proxy returns that response to the client unchanged. It
does not resend the request with another account. The window named by
`representative-claim` is recorded as 100% even when its utilization header
still reads lower. Until that window resets, the proxy ranks the account
after accounts that have not reached a limit. A 429 without the unified
headers is also returned unchanged and records no usage. A 401 marks the
token rejected for 30 minutes, and the proxy sends that request with the next
usable account.

`shared_runtime_home: "native"` is only valid with `proxy_listen`. Proxy
launches then leave `CLAUDE_CONFIG_DIR` alone, so Claude uses its own
`~/.claude`: settings, hooks, plugins, skills, MCP servers and their OAuth
state, transcripts, `--resume`, and auto-memory are the same for every
account and for plain `claude`. The account homes are still used when the
proxy is down, because a fixed-token launch must not touch the native home.
Any other absolute `shared_runtime_home` keeps a separate managed directory.

The listen address must be a loopback address; the port is fixed so launched
processes can find it. Requests without the placeholder secret get 401.
Request bodies are buffered up to 64 MiB for replay. Each switch costs one
prompt-cache miss.

### Live Codex account swapping through the local proxy

The same proxy exists for Codex ChatGPT logins:

```json
{
  "name": "codex",
  "kind": "codex",
  "account_mode": "home",
  "proxy_listen": "127.0.0.1:7879",
  "shared_runtime_home": "native"
}
```

```sh
subswapper capture -service codex -account main2      # register the current ~/.codex login
subswapper home proxy-auth -service codex             # replace ~/.codex/auth.json with the placeholder
subswapper monitor -interval 5m -proxy
subswapper home run -service codex -- codex
```

Codex cannot be pointed at a proxy by environment alone. `home run` therefore
inserts global `-c` overrides before the Codex subcommand: `chatgpt_base_url`
and a custom model provider named `subswapper` with `requires_openai_auth`,
because Codex refuses overrides of its built-in `openai` provider. The
provider keeps `supports_websockets=false`, so every turn is a replayable
HTTP request; WebSocket upgrades get 501. Codex 0.153 was verified to send
its identity as `Authorization: Bearer` plus `chatgpt-account-id`; the proxy
replaces both with the selected account's login read from that account's
home. Requests Codex sends without credentials (plugin catalogs, MCP) are
relayed unchanged.

The runtime home holds a placeholder login: an unsigned JWT that Codex
parses as a logged-in `pro` account. It never reaches upstream and its
refresh token is a marker, so the process cannot refresh a real session and
invalidate the registered copy. `home run` installs it when `auth.json` is
missing or already a placeholder and refuses to overwrite a real login; run
`home proxy-auth` once after `capture` to move the real login aside. A plain
`codex` outside `home run` then fails to authenticate, so launch Codex
through `home run` (or add the same `-c` values to `config.toml`).

Every 429 is returned to the client unchanged. When its body names
`usage_limit_reached` or `rate_limit_reached`, the proxy also asks
`wham/usage` for that account's reset time and ranks the account last until
then. A 401 marks the login rejected for 30 minutes, and the proxy sends that
request with the next usable account. After successful
responses the proxy refreshes an account's usage from `wham/usage` at most
every five minutes; the monitor's app-server probe keeps refreshing the
stored tokens. Without the proxy, `home run` falls back to the selected
account home and its real login.

### Hub mode across machines

One machine, the hub, keeps every account and runs the proxies. Other
machines, the clients, launch Claude Code and Codex through the hub over
Tailscale. Real tokens never leave the hub, and only the hub refreshes them,
so a rotating ChatGPT refresh token is never used from two machines. Proxied
API traffic leaves from the hub's network, so a residential hub gives every
client the same egress.

On the hub, add `hub_listen` with the hub's Tailscale IP next to
`proxy_listen`:

```json
{
  "name": "claude",
  "kind": "claude",
  "proxy_listen": "127.0.0.1:7878",
  "hub_listen": "100.67.68.117:7878",
  "shared_runtime_home": "native"
}
```

`hub_listen` accepts only a Tailscale address (`100.64.0.0/10` or
`fd7a:115c:a1e0::/48`) or loopback, because the hub speaks plain HTTP and
relies on WireGuard for encryption. Clients authenticate with the same proxy
secret or Codex placeholder that local launches use. Start the proxies after
`tailscaled`, or binding the Tailscale IP fails.

```sh
# on the hub
subswapper monitor -interval 5m -proxy
subswapper hub export -host box-box -out hub.json   # 0600, never overwrites
# on each client, after copying hub.json there
subswapper hub import -in hub.json && rm hub.json
subswapper home run -service claude -- claude
subswapper home run -service codex -- codex
```

`hub import` stores the credential, sets `hub_url` on each service, and sets
`shared_runtime_home: "native"` when the service had none. It refuses a
service that has its own `proxy_listen` and no `hub_url`. A client has no
accounts. `home run` opens a loopback relay to the hub for the lifetime of the
process, so the CLI still sees a local base URL.

A client service may also keep a loopback `proxy_listen`. `subswapper proxy`
then serves a fixed relay to the hub there, and `home run` uses it while it
answers. This lets a former hub become a client without restarting its
sessions: they keep their base URL and secret, and the relay now carries
their requests to the new hub. If the hub is unreachable or rejects the
credential, `home run` fails instead of launching directly. On a client,
`home proxy-auth -service codex` also moves a real `~/.codex/auth.json` aside.
Account commands such as `login`, `token`, and `switch` run on the hub.

Only traffic sent to the proxy goes through the hub. Telemetry, MCP
connectors, and tool web fetches from a client CLI still use the client's own
network.

### Shared Claude user configuration

Claude treats `CLAUDE_CONFIG_DIR` as the location for every documented
`~/.claude` path. Subswapper keeps runtime state in each account home and links
only this explicit user-configuration allowlist from the native `~/.claude`:

| Shared from native `~/.claude` | Kept inside each account home |
| --- | --- |
| `CLAUDE.md`, `settings.json`, `keybindings.json` | `.credentials.json`, `.config.json`, `mcp-needs-auth-cache.json` |
| `plugins/`, `skills/`, `agents/`, `output-styles/` | `projects/`, including transcripts and auto-memory |
| `rules/`, `commands/`, `workflows/`, `themes/` | `sessions/`, `history.jsonl`, `agent-memory/`, and all other generated state |

This allowlist follows Claude's documented
[user directory layout](https://code.claude.com/docs/en/claude-directory).
Missing native sources are skipped and reported. Existing files, directories,
and links to other targets are conflicts and are never replaced. Run
`subswapper home repair -service claude -account <name>` after an upgrade to
repair an existing home. The command is idempotent and reports names only; it
does not read or print file contents.

Subswapper uses symbolic links so later user-configuration changes apply to
every account. On Windows, enable Developer Mode or grant symbolic-link
privilege. Creation or repair stops with an explicit error if Windows cannot
create a link.

### Optional shared Claude runtime

By default, each Claude account has a separate runtime home. User-scoped MCP
configuration and MCP OAuth state therefore follow the selected account home.
To keep one MCP state across setup-token account switches, configure a shared
runtime home:

```json
{
  "name": "claude",
  "kind": "claude",
  "account_mode": "home",
  "shared_runtime_home": "~/.local/share/subswapper/shared/claude"
}
```

Every setup-token launch then uses that path as `CLAUDE_CONFIG_DIR`.
Subswapper still selects and injects one account token per new process. Usage
captured through Claude's status line remains bound to that token and account.
Existing processes keep their launch token.

The shared directory keeps MCP configuration, MCP OAuth, settings, plugins,
credentials, history, sessions, and auto-memory together. This option disables
the account-state isolation described above. Subswapper removes only stale
top-level `oauthAccount` metadata before launch. It preserves `.credentials.json`
contents and secures the file to `0600`. The native user home and native
`~/.claude` directory remain prohibited as shared runtime paths.

An empty shared directory starts without user-scoped MCP registrations. Seed
it from one trusted Subswapper account home while no Claude process uses that
home, or configure MCP servers again in the shared runtime. The
`shared_runtime_home` path must resolve to an absolute path. Omitting it keeps
the original per-account behavior.

## How auto-switching works

Weekly quota left unused at a reset is lost, so subswapper drains the account
whose weekly window resets first. It ranks healthy accounts in this order:

1. accounts below the switch threshold (default **90%**) in every window,
   earliest running weekly reset first;
2. accounts below the threshold with no running weekly window (the reset time
   has passed or is unknown), since waiting costs them nothing;
3. accounts at or above the threshold, lowest worst-window score first.

Ties go to the lower worst-window score, then the lower average, then the name.
The proxies use the same order for fallback routes after the selected account.

`monitor` evaluates every service each cycle. With automatic switching
enabled, a service moves to the best-ranked account only when the cooldown
since the service last switched accounts, manually or automatically, has
passed (default **30 minutes**), and one of these holds:

- the active account is below the switch threshold, and the best account is
  also below it and has an earlier running weekly reset;
- the active account has reached the switch threshold in its 5-hour, weekly,
  or Fable weekly window, and the best account improves the worst-window score
  by at least the minimum improvement (default **10 percentage points**).

Proxy samples are trusted without an age limit, since an unused account's
windows only fall until their reset. Both pacing rules are skipped when the
active account is exhausted or its stored credentials stop working. The
monitor then moves to the best healthy account on the next cycle. Claude accounts with missing, expired, rejected, or
unsafe setup tokens are never selected. Accounts without fresh trusted usage
are also excluded. A manual
`switch -account auto` always forces the best account immediately.

In account-home mode, switching updates routing state only. Existing launcher
or CLI processes are not silently rebound; start the next command through
`home run`. Explicit custom file-bundle services retain the legacy
transactional switching behavior.

## Configuration

`subswapper init` writes a config like this:

```json
{
  "monitor": {
    "interval": "5m",
    "auto_switch": true
  },
  "services": [
    { "name": "claude", "kind": "claude", "display_name": "Claude Code", "account_mode": "home" },
    { "name": "codex", "kind": "codex", "display_name": "Codex", "account_mode": "home" }
  ]
}
```

The `monitor` block accepts these knobs (defaults shown):

```json
"monitor": {
  "interval": "5m",
  "auto_switch": true,
  "switch_threshold": 0.90,
  "min_improvement": 0.10,
  "cooldown": "30m"
}
```

Configs written by older versions may still contain `warmup`,
`warmup_model`, or `warmup_fable_model`. These keys are ignored.

Top-level `backup_root` and `state_path` override where account homes and state
are stored. (`backup_root` keeps its historical name for compatibility.)
Built-in services without explicit `files` default to `account_mode: "home"`.
Account-home files use these native names:

- Claude: `<account-home>/.credentials.json` and optional `.config.json`
- Codex: `<account-home>/auth.json`

Claude home-mode services may set `shared_runtime_home`. This changes only the
runtime home used by setup-token launches and status-line settings. Registered
account homes and setup-token storage remain separate. Codex home-mode services accept the same
keys. `proxy_listen` enables the local auth proxy; `proxy_upstream` overrides
the API origin for testing. `hub_listen` and `hub_url` configure
[hub mode](#hub-mode-across-machines).

An explicit `files` list defaults a service to `account_mode: "bundle"`. This
keeps custom-service support and legacy transactional switching available.

Codex can also store credentials in an OS keyring. `subswapper` manages
file-backed credentials only, so configure Codex with:

```toml
cli_auth_credentials_store = "file"
```

Claude setup tokens are not stored in account homes. Token files use `0600`
mode under separate `0700` directories. Token values never enter config or
state. Identity remains `unknown` when Anthropic does not expose a trusted
`accountUuid` for an inference-only token. Anthropic documents setup-token
creation and lifetime in its [authentication guide](https://code.claude.com/docs/en/authentication).
The isolation design was also compared with the MIT-licensed
[claude-code-account-switcher](https://github.com/claude-code-tools/claude-code-account-switcher);
Subswapper uses an independent implementation.

## Usage probes

**Claude** home-mode usage first probes Subswapper's existing OAuth usage
endpoint with the setup token. Subswapper never refreshes, exchanges, or
rotates a setup token. If the endpoint does not support that token, a launch
wrapper captures the documented five-hour and seven-day limits from Claude's
normal status-line response data. It runs any existing user or workspace
status-line command with the original input. Cached data expires after five
minutes and is bound to the random revision of the current token. Anthropic
documents the response fields in its [status-line guide](https://code.claude.com/docs/en/statusline).

Claude's status line exposes only the five-hour and seven-day windows.
`FABLE5` therefore shows `-` for status-line-only setup-token accounts; the
proxy fills it from the `7d_oi` response header. Subswapper
reports usage as unavailable when neither a direct response nor a fresh,
complete status-line sample exists.

**Codex** usage is read through the local `codex app-server` JSON-RPC
interface using the permanent account `CODEX_HOME`, so an official credential
refresh stays in that home. Subswapper does not create a disposable copy of
the refresh token. Current plans may expose only a weekly
window; any available provider window is displayed and included in scoring.
This requires ChatGPT auth in file storage; API-key mode has no subscription
limits to read.

**Custom services** (or overrides) can set `usage_command`. The command runs
once per captured account with these environment variables:

- `SUBSWAPPER_SERVICE`
- `SUBSWAPPER_ACCOUNT`
- `SUBSWAPPER_EMAIL`
- `SUBSWAPPER_ACCOUNT_DIR`
- `SUBSWAPPER_BACKUP_ROOT`

It must print JSON with both `five_hour` and `weekly` windows (output missing
either is rejected):

```json
{
  "five_hour": { "pct": 25, "resets_at": "2026-07-01T23:00:00Z" },
  "weekly": { "pct": 16, "resets_at": "2026-07-05T23:00:00Z" }
}
```

Windows may alternatively be given as `{ "used": 25, "limit": 100 }`. An
optional third window, `fable_weekly`, is also accepted and counts toward
exhaustion and autoswitch scoring.

## Importing from claude-swap

To replace an existing `cswap` install, import its stored Claude accounts
directly:

```sh
subswapper import-cswap
```

This reads the claude-swap data directory (default
`~/.local/share/claude-swap` on Linux), decodes its stored credential backups,
copies config snapshots, and imports the usage cache, naming each slot
`cswap-N`. cswap's active slot is adopted as the active account when the live
credential files still match it. After import, the `cswap` binary is no
longer needed.

## Migrating from Subswapper 0.1

After upgrading an existing installation, run:

```sh
subswapper home migrate
subswapper home repair -service claude -account <name>
```

Legacy Claude `credentials.json` and `claude.json` snapshots are copied to
their native home names, `.credentials.json` and `.config.json`. Existing
native files win; nothing is overwritten or deleted. Codex `auth.json` files
already have their native filename. `home repair` adds only missing allowlisted
Claude user-configuration links. Verify with `subswapper status`, then use
`home path` to configure external launcher instances.

### Safe Claude setup-token migration

Do not restart a launcher or Subswapper for this migration. Existing agents
keep their original process environment.

1. List active agents and record the current Subswapper route.
2. Obtain explicit approval before creating any setup token.
3. Open a private browser profile that is signed out of Claude.
4. Run `claude setup-token` and authenticate only the intended subscription.
5. Abort if the browser identity is absent or ambiguous.
6. Run `subswapper home token set -service claude -account <name>`.
7. Paste the token only into the hidden prompt.
8. Run `subswapper home token status -service claude -account <name>`.
9. Launch one explicit test process with `subswapper home run -service claude -account <name> -- claude`.
10. Wait for a normal response and fresh usage before enabling auto-selection.
11. Repeat for each account.
12. Obtain explicit approval before replacing or removing any live token.

Rollback affects only new launches. With approval, use `home token remove` for
the affected account. Existing processes retain the token supplied at launch.

## Data & security

Defaults on Linux (macOS and Windows use their native config/data folders):

- config: `~/.config/subswapper/config.json`
- state: `~/.local/share/subswapper/state.json`
- account homes: `~/.local/share/subswapper/accounts/`
- Claude setup tokens: `~/.local/share/subswapper/tokens/`
- Claude proxy secret: `~/.local/share/subswapper/tokens/claude/proxy-secret.json`

Linux and macOS are tested in CI; Windows builds are cross-compiled but
currently untested. Claude setup-token storage requires POSIX `0600` and
`0700` permission checks, so it fails closed on Windows. Other Windows support
remains experimental. Claude user-configuration inheritance also needs Windows
Developer Mode or symbolic-link privilege.

Credentials and state are written with `0600` permissions under `0700`
directories. Setup-token replacement uses atomic rename and directory sync.
Home removal preserves the directory by default;
`-delete-home` is required to erase it. Explicit bundle-mode changes still use
rollback snapshots and a recovery journal. **Treat the account root like a
password store** — it holds working OAuth tokens.

## Running as a service

To keep the monitor running, a systemd user unit works well:

```ini
# ~/.config/systemd/user/subswapper.service
[Unit]
Description=subswapper account-home usage monitor

[Service]
ExecStart=%h/go/bin/subswapper monitor
Restart=on-failure

[Install]
WantedBy=default.target
```

```sh
systemctl --user enable --now subswapper
```

The default five-minute monitor writes startup, switch, failure-transition,
and recovery events. Use `monitor -verbose` only for interactive diagnostics
when a complete status table every cycle is useful.

## Contributing

Contributions are welcome — see [CONTRIBUTING.md](CONTRIBUTING.md) for the
development workflow. Please report security issues privately (see
[SECURITY.md](SECURITY.md)).

## License

[MIT](LICENSE)

## Optional harness plugin

Claude Code and Codex can run bounded tasks and model probes through
`subswapper delegate`, including same-provider CLI processes. The plugin also
routes full-harness checks through `subswapper home run`.
See [installation, usage, and permission boundaries](docs/plugin.md).
