# subswapper

[![CI](https://github.com/lawzava/subswapper/actions/workflows/ci.yml/badge.svg)](https://github.com/lawzava/subswapper/actions/workflows/ci.yml)
[![License: MIT](https://img.shields.io/badge/License-MIT-blue.svg)](LICENSE)

`subswapper` lets [Claude Code](https://claude.com/claude-code) and
[Codex](https://openai.com/codex/) share several Claude and ChatGPT
subscriptions. A local proxy sends each request with the selected account's
token, so running sessions move to another account on their next request.
One machine can hold every account and serve them to your other machines over
Tailscale.

## Features

- **One-command setup**: `setup local` for one machine, `setup hub` and
  `setup client` for several, `doctor` to check everything and print fixes.
- **One-command accounts**: `add claude <name>` and `add codex <name>` run the
  provider's sign-in and register the account, from any machine.
- **Switching without a restart**: running sessions pick up a switch on their
  next request. Real tokens never enter the provider process.
- **Automatic selection**: a monitor drains the account whose weekly quota
  resets first and moves away from an account near its limit.
- **Hub across machines**: accounts live on one machine, only that machine
  refreshes tokens, and every client's API traffic leaves from its network.
- **Paseo plugin**: usage in every workspace header and one-click switching.
- **Safe by design**: cross-process locking, `0600` credentials under `0700`
  directories, atomic writes, and launches that fail closed when a hub is down.

## Installation

Requires Go 1.26.6 or newer.

```sh
go install github.com/lawzava/subswapper/cmd/subswapper@latest
```

Codex accounts need the `codex` CLI on `PATH` of the machine that holds them;
the monitor uses it to read usage and refresh logins.

## Quick start

### One machine

```sh
subswapper setup local          # config, auth proxies, systemd user services
subswapper add claude work      # runs `claude setup-token`, then asks for the token
subswapper add codex personal   # runs `codex login` in a throwaway home
subswapper claude               # Claude Code through subswapper
subswapper codex                # Codex through subswapper
subswapper status
```

### Several machines

Pick the machine that should hold the accounts, the hub. Its network becomes
the egress for every client, so a residential machine gives all of them a
residential IP.

```sh
# on the hub
subswapper setup hub -enroll    # -enroll lets tailnet members join with one command
subswapper add claude work
subswapper add codex personal

# on every other machine
subswapper setup client 100.67.68.117   # the hub's Tailscale IP
subswapper claude
```

`setup client` connects to the hub, installs the Codex placeholder login
(moving a real `~/.codex/auth.json` aside with a backup), and runs `doctor`.
Accounts can be added, switched, and removed from any client; the commands run
on the hub.

`status` prints one row per account:

```
subswapper status 2026-10-03T13:36:58+03:00

SERVICE    ACCOUNT    SELECTED 5H                     WEEKLY                  FABLE5                  SCORE    UPDATED  STATE
-------    -------    -------- --                     ------                  ------                  -----    -------  -----
claude     foxy2               38% reset Oct03 17:30  90% reset Oct04 07:00   3% reset Oct04 07:00    90%      2m       ready
claude     h2         yes      41% reset Oct03 17:50  12% reset Oct08 15:00   0% reset Oct08 15:00    41%      <1m      ready
codex      f2         yes      -                      1% reset Oct10 00:21    -                       1%       4m       ready
claude: accounts on the hub at http://100.67.68.117:7878
codex: accounts on the hub at http://100.67.68.117:7879
```

`FABLE5` is the weekly window for Claude's Fable models. `SCORE` is an
account's worst window. `UPDATED` is the age of the usage sample.

## Commands

All commands accept `-config <path>` (default `~/.config/subswapper/config.json`
on Linux). Flags may follow positional arguments.

| Command | Description |
| --- | --- |
| `setup local` | Configure the Claude and Codex proxies and install the systemd user services for one machine. `-no-service` writes the config only. |
| `setup hub [-enroll] [-tailscale-ip IP]` | Like `setup local`, and also serve the proxies on this machine's Tailscale IP. `-enroll` lets any tailnet member join with `setup client`. |
| `setup client <hub>` | Use a hub's accounts from this machine. Needs `-enroll` on the hub; otherwise use `hub export` and `hub import`. |
| `doctor` | Check the config, proxies, hub connection, Codex placeholder, accounts, monitor, and Paseo providers. Prints a fix for each problem. |
| `add claude\|codex <name> [-email label] [-device] [-paste]` | Sign in and register an account, or replace an existing account's login. `-device` uses Codex device-code sign-in for machines without a browser. `-paste` skips `claude setup-token` and asks for a token you already have. |
| `remove claude\|codex <name> [-force] [-delete-home]` | Unregister an account and delete its Claude setup token. `-force` removes the selected account. |
| `switch claude\|codex\|all [name\|auto]` | Select an account; `auto` (the default) picks the best one. |
| `status [-json]` (alias `list`) | Show every account with usage windows, score, and state. `-json` prints the report the Paseo plugin reads. |
| `claude [args...]`, `codex [args...]` | Launch the provider through subswapper. Same as `home run -service <name> -- <name> [args...]`. |
| `home run -service <name> [-account <name>] [-- command...]` | Launch any command the way `claude` and `codex` are launched. |
| `home path -service <name> [-account <name>]` | Print an account's home directory. |
| `home proxy-auth -service codex` | Move a real login out of the Codex runtime home and install the proxy placeholder (backup kept). |
| `monitor [-interval 5m] [-once] [-no-auto] [-verbose] [-proxy]` | Probe usage on a loop and switch automatically. `-proxy` also serves the proxies. |
| `proxy [-service <name>] [-listen 127.0.0.1:7878]` | Serve the proxies, or a client's fixed relays to its hub. |
| `hub connect <hub>`, `hub export -out <file\|->`, `hub import -in <file\|->` | Lower-level hub enrollment that `setup client` builds on. |
| `delegate ...` | Run a bounded task through a provider; see [the harness plugin](docs/plugin.md). |
| `init` | Write a minimal config without proxies. |
| `version` | Print the subswapper version. |

## Adding accounts

`subswapper add claude <name>` runs `claude setup-token`. Sign in to the
subscription you want in the browser, then paste the printed token into the
hidden prompt. Setup tokens last a year and are only used for inference, so
one token can serve every machine. A token can also be piped in:
`subswapper add claude work < token.txt`.

`subswapper add codex <name>` runs `codex login` in a throwaway Codex home
and registers the resulting login. The throwaway home is deleted afterwards,
so the registered copy is the only one and only subswapper's monitor
refreshes it. Your own `~/.codex` is never touched. Add `-device` on a machine
without a browser.

On a hub client, both commands sign in locally and upload the credential to
the hub over Tailscale. Adding an account that exists replaces its login, which
is how you renew an expired one.

## Paseo

The [Paseo plugin](paseo-plugin/README.md) shows every account's usage in the
workspace header and on a **Subscriptions** screen, switches accounts from
the header menu or the Command Center, and adds accounts: **Add account** opens
a terminal in the workspace that runs `subswapper add`, or takes a Claude setup
token directly. Turn on **Settings → Plugins → Enable
plugins** on the daemon, then:

```sh
paseo plugin install lawzava/subswapper:paseo-plugin
```

Point Paseo's providers at `subswapper claude` and `subswapper codex` (or
`subswapper home run -service <name> -- <cli>`). `doctor` checks that every
Paseo provider runs an existing subswapper binary of the current version.

## How it works

### Claude

`claude` and `home run` launch Claude Code with `ANTHROPIC_BASE_URL` pointing
at the local proxy and a per-install placeholder secret in
`CLAUDE_CODE_OAUTH_TOKEN`. For every request the proxy swaps in the selected
account's setup token, so `switch` and the monitor take effect on the next
request of every running session. If the proxy is down on a machine that
holds accounts, `home run` warns and launches with a fixed token instead.

The proxy reads Anthropic's `anthropic-ratelimit-unified-*` response headers
and records them as the account's usage. The `5h` and `7d` windows arrive on
every response. The `7d_oi` window, which Claude Code shows as the Fable
limit, arrives only on responses served by a Fable model and is kept across
other responses because they do not consume it. A window counts as 0% once
its reset time passes.

A rate-limit response reaches the client unchanged; the proxy never resends a
request with another account to get past a limit. The rejected window is
recorded as full until it resets, so the account ranks last. A 401 marks the
token rejected for 30 minutes, and that one request is sent with the next
usable account.

With `shared_runtime_home: "native"` (what `setup` writes) Claude keeps its
own `~/.claude`: settings, plugins, MCP servers, transcripts, `--resume`, and
memory are the same for every account and for plain `claude`.

Proxy launches skip `CLAUDE_CODE_SUBPROCESS_ENV_SCRUB` unless the service sets
`proxy_env_scrub: true`, because the scrub sandboxes every Bash command and
masks `~/.gnupg` and `~/.ssh`.

### Codex

Codex cannot be pointed at a proxy by environment alone, so launches add `-c`
overrides before the subcommand: `chatgpt_base_url` and a model provider named
`subswapper` with `requires_openai_auth` and `supports_websockets=false`, which
keeps every turn a replayable HTTP request. The proxy replaces the
`Authorization` and `chatgpt-account-id` headers with the selected account's
login and passes Codex's workspace-routing discovery through for the
placeholder account.

The runtime home holds a placeholder login: an unsigned JWT that never
reaches upstream and has no usable refresh token, so a Codex process cannot
refresh, and invalidate, a real login. A plain `codex login` in `~/.codex`
replaces the placeholder and breaks proxied launches; `doctor` detects that
and `home proxy-auth -service codex` repairs it.

When a 429 names `usage_limit_reached` or `rate_limit_reached`, the proxy reads
that account's reset time from `wham/usage` and ranks it last until then.

### Hub

The hub serves its proxies on loopback for its own launches and on its
Tailscale address (`hub_listen`) for clients. `hub_listen` accepts only a
Tailscale or loopback address, because the hub speaks plain HTTP and relies on
WireGuard for encryption. Clients authenticate with the same proxy secret or
Codex placeholder that local launches use; `hub_enroll` hands that credential
to any caller who asks, so turn it on only when every tailnet member may use
the accounts.

A client has no accounts. `claude`, `codex`, and `home run` open a loopback
relay to the hub for the lifetime of the process, so the CLI still sees a local
base URL. When the hub is unreachable or rejects the credential, the launch
fails instead of going around it. A client service may keep a loopback
`proxy_listen`; `subswapper proxy` then serves a fixed relay there, which lets
a former hub become a client without restarting its running sessions.

Only traffic sent to the proxy goes through the hub. Telemetry, MCP
connectors, and web fetches from a client CLI use the client's own network.

## How auto-switching works

Weekly quota left unused at a reset is lost, so subswapper drains the account
whose weekly window resets first. It ranks healthy accounts in this order:

1. accounts below the switch threshold (default **90%**) in every window,
   earliest running weekly reset first;
2. accounts below the threshold with no running weekly window (the reset time
   has passed or is unknown), since waiting costs them nothing;
3. accounts at or above the threshold, lowest worst-window score first.

Ties go to the lower worst-window score, then the lower average, then the name.
The proxies use the same order for fallback routes.

With automatic switching on, `monitor` moves a service to the best-ranked
account when the cooldown since its last switch has passed (default
**30 minutes**) and one of these holds:

- the active account is below the threshold, and the best account is too and
  has an earlier running weekly reset;
- the active account has reached the threshold in any window, and the best
  account improves the worst-window score by at least the minimum improvement
  (default **10 percentage points**).

An exhausted active account, or one whose credentials stop working, is left on
the next cycle regardless of cooldown. Accounts with missing, expired, or
rejected credentials, or without trusted usage, are never selected.
`switch <service> auto` picks the best account immediately.

## Configuration

`setup local` writes a config like this; `setup hub` adds `hub_listen` and
`hub_enroll`. `setup client` writes `hub_url` instead of `proxy_listen`, and
refuses a service that already serves its own proxy, so a machine with local
accounts does not silently stop using them:

```json
{
  "monitor": { "interval": "5m0s" },
  "services": [
    { "name": "claude", "kind": "claude", "proxy_listen": "127.0.0.1:7878", "shared_runtime_home": "native" },
    { "name": "codex", "kind": "codex", "proxy_listen": "127.0.0.1:7879", "shared_runtime_home": "native" }
  ]
}
```

Monitor settings and their defaults:

```json
"monitor": {
  "interval": "5m",
  "auto_switch": true,
  "switch_threshold": 0.90,
  "min_improvement": 0.10,
  "cooldown": "30m"
}
```

Service keys: `proxy_listen` (loopback address of the auth proxy),
`proxy_upstream` (API origin, for testing), `shared_runtime_home` (`native`,
or an absolute path for a separate Claude or Codex home), `proxy_env_scrub`,
`hub_listen`, `hub_enroll`, `hub_url`, `usage_command`, and `disabled`.
Top-level `backup_root` and `state_path` move the account homes and state.
Keys from older versions (`warmup`, `warmup_model`, `warmup_fable_model`)
are ignored.

Codex can store logins in an OS keyring; subswapper manages file logins only.
`add codex` forces file storage for its login; if you run Codex yourself, set
`cli_auth_credentials_store = "file"` in `~/.codex/config.toml`.

## Usage probes

**Claude** setup-token accounts get their usage from the proxy's rate-limit
headers. Without the proxy, subswapper probes the OAuth usage endpoint and
falls back to Claude's status-line data captured at launch; neither refreshes
or exchanges a setup token.

**Codex** usage is read through `codex app-server` in the account's own home,
so the official credential refresh stays in that home. Plans that expose only
a weekly window show `-` for five hours.

A Claude service may set `usage_command`. It runs once per account with
`SUBSWAPPER_SERVICE`, `SUBSWAPPER_ACCOUNT`, `SUBSWAPPER_EMAIL`,
`SUBSWAPPER_ACCOUNT_DIR`, and `SUBSWAPPER_BACKUP_ROOT` set, and must print JSON
with `five_hour` and `weekly` windows (`fable_weekly` is optional):

```json
{
  "five_hour": { "pct": 25, "resets_at": "2026-07-01T23:00:00Z" },
  "weekly": { "pct": 16, "resets_at": "2026-07-05T23:00:00Z" }
}
```

## Troubleshooting

Run `subswapper doctor`. It covers the common failures:

- **Codex launches fail after `codex login`.** The login replaced the
  placeholder in `~/.codex/auth.json`. Run
  `subswapper home proxy-auth -service codex`, or add that login as an account
  with `subswapper add codex <name>`.
- **A client cannot reach the hub.** Check Tailscale on both machines and that
  the hub's proxies run (`systemctl --user status subswapper-hub`).
- **The monitor is not probing.** Start it with
  `systemctl --user start subswapper` or `subswapper monitor`.
- **A Paseo provider runs an old binary.** Reinstall subswapper at the path the
  provider uses.

## Data & security

Defaults on Linux (macOS and Windows use their native config and data folders):

- config: `~/.config/subswapper/config.json`
- state: `~/.local/share/subswapper/state.json`
- account homes: `~/.local/share/subswapper/accounts/`
- Claude setup tokens, proxy secret, and Codex placeholder:
  `~/.local/share/subswapper/tokens/`

Credentials and state are written with `0600` permissions under `0700`
directories, with atomic renames. Token values never enter config, state, or
the provider process. Removing an account keeps its home unless `-delete-home`
is given. Treat the data directory like a password store.

Linux and macOS are tested in CI. Windows builds are cross-compiled but
untested, and setup-token storage fails closed there because it relies on
POSIX permission checks.

## Running as a service

`setup local` and `setup hub` install two systemd user services:
`subswapper-hub.service` runs `subswapper proxy` and `subswapper.service` runs
`subswapper monitor -interval 5m`. Enable lingering
(`sudo loginctl enable-linger $USER`) so they run without a login session. On
other systems, run `subswapper monitor -proxy` at login.

## Upgrading from v0.6

v0.7 removed commands that `add`, `remove`, and `setup` replace: `capture`,
`home create`, `home login`, `home token`, `home env`, `home repair`,
`home migrate`, and `import-cswap`. Registered accounts, tokens, and homes are
unchanged.

Bundle mode (`account_mode: "bundle"`, custom `files`, and services other than
Claude and Codex) is gone too; a config that uses it fails to load with a
message naming the service.

## Contributing

Contributions are welcome; see [CONTRIBUTING.md](CONTRIBUTING.md) for the
development workflow. Please report security issues privately (see
[SECURITY.md](SECURITY.md)).

## License

[MIT](LICENSE)

## Optional harness plugin

Claude Code and Codex can run bounded tasks and model probes through
`subswapper delegate`, including same-provider CLI processes. The plugin also
routes full-harness checks through `subswapper home run`.
See [installation, usage, and permission boundaries](docs/plugin.md).
