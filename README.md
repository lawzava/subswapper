# subswapper

[![CI](https://github.com/lawzava/subswapper/actions/workflows/ci.yml/badge.svg)](https://github.com/lawzava/subswapper/actions/workflows/ci.yml)
[![License: MIT](https://img.shields.io/badge/License-MIT-blue.svg)](LICENSE)

`subswapper` is a small Go CLI that manages multiple [Claude Code](https://claude.com/claude-code)
and [Codex](https://openai.com/codex/) subscription accounts on one machine.
Capture each account's credential files once, then switch between them without
logging in again — manually, or automatically when the active account
approaches its usage limits.

## Features

- **Capture once, switch instantly** — snapshots the credential files of the
  logged-in account into a private backup, then swaps bundles atomically on
  demand.
- **Live usage tracking** — reads real usage windows (5-hour, weekly, and
  Claude's Fable-scoped weekly) straight from each provider using the stored
  credentials; no scraping, no extra logins.
- **Automatic switching** — a monitor loop moves each service to the
  least-used healthy account when the active one crosses a configurable
  threshold, with cooldown and minimum-improvement pacing to avoid churn.
- **Safe by design** — cross-process locking, atomic file replacement, `0600`
  files under `0700` directories, and corruption guards that never overwrite a
  good credential backup with a broken live file.
- **Extensible** — any other service can be managed by listing its credential
  files in the config; plug in a custom `usage_command` for usage probing.

## Installation

Requires Go 1.26+.

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

# Log in to an account with the normal client, then snapshot it
subswapper capture -service claude -account personal
subswapper capture -service codex -account personal

# Log in to the next account and snapshot that too
subswapper capture -service claude -account work

# See every account's usage windows
subswapper status

# Switch to a specific account, or let subswapper pick the least-used one
subswapper switch -service claude -account work
subswapper switch -service all -account auto

# Keep switching automatically in the background
subswapper monitor
```

`status` prints one row per captured account:

```
subswapper status 2026-07-02T14:07:31Z

SERVICE    ACCOUNT                  ACTIVE  5H                           WEEKLY                       FABLE5                       SCORE    STATE
-------    -------                  ------  --                           ------                       ------                       -----    -----
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
| `capture -service <name> -account <name> [-email <label>]` | Snapshot the currently logged-in account's credential files. |
| `switch -service <name> [-account <name>\|auto]` | Restore an account's bundle; `auto` picks the least-used healthy account. |
| `switch -service all -account auto` | Auto-pick the best account for every service at once. |
| `status` (alias `list`) | Show every captured account with usage windows, score, and state. |
| `monitor [-interval 30s] [-once] [-no-auto]` | Poll usage on a loop and auto-switch when thresholds are hit. |
| `remove -service <name> -account <name> [-force]` (alias `rm`) | Delete a captured account and its backup. |
| `import-cswap [-root <dir>]` | Import accounts from an existing claude-swap (`cswap`) install. |
| `version` | Print the subswapper version. |

All commands accept `-config <path>` (default
`~/.config/subswapper/config.json` on Linux).

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
stored credentials stop working — the monitor escapes to the best healthy
account on the next cycle. Accounts whose credentials are missing or rejected
are never selected, whatever their cached usage says. A manual
`switch -account auto` always forces the best account immediately.

Before every switch, the outgoing account's live files are synced back into
its backup so credentials rotated while it was active are never lost.

## Configuration

`subswapper init` writes a config like this:

```json
{
  "monitor": {
    "interval": "30s",
    "auto_switch": true
  },
  "services": [
    { "name": "claude", "kind": "claude", "display_name": "Claude Code" },
    { "name": "codex", "kind": "codex", "display_name": "Codex" }
  ]
}
```

The `monitor` block accepts these knobs (defaults shown):

```json
"monitor": {
  "interval": "1m",
  "auto_switch": true,
  "switch_threshold": 0.90,
  "min_improvement": 0.10,
  "cooldown": "30m"
}
```

Top-level `backup_root` and `state_path` override where backups and state are
stored. Each service may list explicit `files` (path + `backup_name` +
optional `required: false`) to manage any credential layout; services of kind
`claude`/`claude-code` and `codex` get sensible defaults:

Claude Code:

- credentials: `${CLAUDE_CONFIG_DIR:-~/.claude}/.credentials.json`
- config: `${CLAUDE_CONFIG_DIR:-~}/.claude.json`, or legacy
  `${CLAUDE_CONFIG_DIR:-~/.claude}/.config.json` when present

Codex:

- auth: `${CODEX_HOME:-~/.codex}/auth.json`

Codex can also store credentials in an OS keyring. `subswapper` manages
file-backed credentials only, so configure Codex with:

```toml
cli_auth_credentials_store = "file"
```

Then log in before capturing.

## Usage probes

**Claude** usage is fetched from Anthropic's OAuth usage endpoint using the
stored credentials, refreshing expired tokens automatically. The weekly limit
scoped to Anthropic's Fable models is tracked as its own window — the `FABLE5`
column in `status` output, JSON key `fable_weekly` — shown alongside the
standard windows and included in autoswitch scoring. Other model-scoped
limits are ignored.

**Codex** usage is read through the local `codex app-server` JSON-RPC
interface. For each captured account, `subswapper` starts the app-server with
a temporary `CODEX_HOME` containing that account's stored `auth.json` and maps
the primary window to 5 hours and the secondary window to 7 days. This
requires ChatGPT auth in file storage; API-key mode has no subscription limits
to read.

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

## Data & security

Defaults on Linux (macOS and Windows use their native config/data folders):

- config: `~/.config/subswapper/config.json`
- state: `~/.local/share/subswapper/state.json`
- backups: `~/.local/share/subswapper/accounts/`

Linux and macOS are tested in CI; Windows builds are cross-compiled but
currently untested — treat Windows support as experimental.

Credential backups and state are written with `0600` permissions under `0700`
directories, and every write is atomic (staged to a temp file, then renamed).
Still: **treat the backup directory like a password store** — it holds working
OAuth tokens for every captured account.

## Running as a service

To keep the monitor running, a systemd user unit works well:

```ini
# ~/.config/systemd/user/subswapper.service
[Unit]
Description=subswapper account monitor

[Service]
ExecStart=%h/go/bin/subswapper monitor
Restart=on-failure

[Install]
WantedBy=default.target
```

```sh
systemctl --user enable --now subswapper
```

## Contributing

Contributions are welcome — see [CONTRIBUTING.md](CONTRIBUTING.md) for the
development workflow. Please report security issues privately (see
[SECURITY.md](SECURITY.md)).

## License

[MIT](LICENSE)
