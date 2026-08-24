# subswapper

[![CI](https://github.com/lawzava/subswapper/actions/workflows/ci.yml/badge.svg)](https://github.com/lawzava/subswapper/actions/workflows/ci.yml)
[![License: MIT](https://img.shields.io/badge/License-MIT-blue.svg)](LICENSE)

`subswapper` is a small Go CLI that manages isolated [Claude Code](https://claude.com/claude-code)
and [Codex](https://openai.com/codex/) account homes on one machine. Claude
launches use a separate long-lived setup token for each account. Subswapper
tracks trusted usage and chooses a home without changing credentials in an
existing process.

## Features

- **Permanent account homes** — creates private per-account directories for
  `CLAUDE_CONFIG_DIR` and `CODEX_HOME`, with commands to log in, print the
  environment, and launch a client in the selected home.
- **Live usage tracking** — uses fresh provider usage data. Claude setup-token
  accounts fall back to Claude's normal status-line response data when the
  OAuth usage endpoint rejects inference-only tokens.
- **Quota-aware routing** — a monitor loop changes the preferred account when
  the selected one crosses a configurable threshold, without mutating a
  running provider's credentials.
- **Safe by design** — cross-process locking, private `0700` homes, atomic
  credential persistence, and non-destructive migration from old snapshots.
- **Extensible** — any other service can be managed by listing its credential
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

# Select a preferred account, or let subswapper pick the least-used one
subswapper switch -service claude -account work
subswapper switch -service all -account auto

# Run a client with the selected account home
subswapper home run -service claude -- claude

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

`FABLE5` is the weekly window scoped to Claude's Fable models (`-` for
accounts without one). `SCORE` is the worst of an account's windows — the
value auto-switching compares.

## Commands

| Command | Description |
| --- | --- |
| `init` | Write a starter config file. |
| `home create -service <name> -account <name> [-email <label>]` | Create and register an empty private account home. |
| `home token set\|status\|remove -service claude [-account <name>]` | Manage a Claude setup token without printing its value. `set` accepts the token only through stdin or a hidden prompt. |
| `home login -service <name> [-account <name>]` | Run the provider's legacy login command in that account home. Do not use this for setup-token routing. |
| `home path\|env -service <name> [-account <name>]` | Print a home path or shell export for configuring other tools. |
| `home run -service <name> [-account <name>] [-- command...]` | Run a command with the selected account's home environment. |
| `home migrate` | Copy legacy snapshots into native home filenames without deleting or overwriting files. |
| `capture -service <name> -account <name> [-email <label>]` | Import the current login into a home; retained for migration and bundle-mode services. |
| `switch -service <name> [-account <name>\|auto]` | Change the preferred route; `auto` picks the least-used healthy account. |
| `switch -service all -account auto` | Auto-pick the best account for every service at once. |
| `status` (alias `list`) | Show every captured account with usage windows, score, and state. |
| `monitor [-interval 5m] [-once] [-no-auto] [-verbose]` | Poll usage on a loop and auto-switch when thresholds are hit. Continuous mode logs events; `-verbose` prints every table. |
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
account's setup token, isolated `CLAUDE_CONFIG_DIR`, and
`CLAUDE_CODE_SUBPROCESS_ENV_SCRUB=1`. It removes conflicting Anthropic,
Bedrock, Vertex, and Foundry variables described by Anthropic's
[environment guide](https://code.claude.com/docs/en/env-vars). Changing the
route does not change an existing agent.

For Codex launchers that support shadow homes, use a shared `CODEX_HOME`
(normally `~/.codex`) and each Subswapper Codex account path as its shadow
home. This keeps every `auth.json` private while the launcher shares sessions.

Transparent mid-agent failover is not supported. Start a new process through
`home run` to use a newly selected account.

## How auto-switching works

`monitor` evaluates every service each cycle. With automatic switching
enabled, a service moves to the captured account with the lowest worst-window
usage only when all of these hold:

- the active account has reached the switch threshold (default **90%**) in its
  5-hour, weekly, or Fable weekly window;
- the best alternative improves the worst-window score by at least the minimum
  improvement (default **10 percentage points**);
- the cooldown since the service last switched accounts — manually or
  automatically — has passed (default **30 minutes**).

Both pacing rules are skipped when the active account is exhausted or its
stored credentials stop working. The monitor escapes to the best healthy
account on the next cycle. Claude accounts with missing, expired, rejected, or
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

Top-level `backup_root` and `state_path` override where account homes and state
are stored. (`backup_root` keeps its historical name for compatibility.)
Built-in services without explicit `files` default to `account_mode: "home"`.
Account-home files use these native names:

- Claude: `<account-home>/.credentials.json` and optional `.config.json`
- Codex: `<account-home>/auth.json`

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

Anthropic does not document a Fable-specific status-line window. `FABLE5`
therefore shows `-` for status-line-only setup-token accounts. Subswapper
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
```

Legacy Claude `credentials.json` and `claude.json` snapshots are copied to
their native home names, `.credentials.json` and `.config.json`. Existing
native files win; nothing is overwritten or deleted. Codex `auth.json` files
already have their native filename. Verify with `subswapper status`, then use
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

Linux and macOS are tested in CI; Windows builds are cross-compiled but
currently untested. Claude setup-token storage requires POSIX `0600` and
`0700` permission checks, so it fails closed on Windows. Other Windows support
remains experimental.

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
Description=subswapper account-home quota monitor

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
