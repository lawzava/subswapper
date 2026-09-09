# Optional Claude Code and Codex CLI plugin

The Subswapper plugin routes separate Claude Code and Codex processes, including
same-provider probes, through account-aware launchers. Its shared skill retains
the name `cross-provider` for compatibility. Use `subswapper delegate` for bounded
tasks and `subswapper home run` for full-harness checks. The caller remains
responsible for task selection and coordination; Megapowers is optional.
Personal model defaults remain in
`~/.config/megapowers/agent-capabilities.md`.

## Install the public plugin

Install CLI v0.3.0 or newer, which includes the delegation command. The older
v0.2.0 CLI release does not include it.

```sh
go install github.com/lawzava/subswapper/cmd/subswapper@latest
export PATH="$(go env GOPATH)/bin:$PATH"

# Claude Code
claude plugin marketplace add lawzava/subswapper
claude plugin install subswapper@subswapper --scope user

# Codex
codex plugin marketplace add lawzava/subswapper
codex plugin add subswapper@subswapper
```

This installs plugin version 0.1.1 through the repository's public marketplace.
It is not a listing in either provider's curated plugin directory. Configure
Subswapper accounts as described in the repository README before delegation.

## Install from this checkout

Requirements: Go from `go.mod`, an existing Subswapper configuration with selected
accounts, and provider binaries on PATH. Delegation requires Unix process groups.
The inspected CLI versions were Claude Code 2.1.258 and Codex 0.153.3.
Older versions must support the flags below; unsupported flags fail without retry.
Windows builds, but the delegation command refuses execution there.

The following commands install into the current user profile. For development,
use isolated harness configuration directories to avoid changing active profiles.

```sh
# From the repository root, install the updated CLI into your Go bin directory.
go install ./cmd/subswapper
export PATH="$(go env GOPATH)/bin:$PATH"

# Claude Code, persistent installation from the local marketplace.
claude plugin marketplace add "$PWD"
claude plugin install subswapper@subswapper --scope user

# Codex, persistent installation from the same local marketplace.
codex plugin marketplace add "$PWD"
codex plugin add subswapper@subswapper
```

The marketplace uses `.claude-plugin/marketplace.json`, which Codex also supports.
Its `policy` field is Codex metadata; Claude's validator reports that it ignores
this field. Each plugin has both harness manifests and the same skill directory.
Start a new harness session after installation. Keep using
`subswapper home run -service claude` or `subswapper home run -service codex`.
No other plugin needs to be enabled for this integration.

For a temporary Claude session without installing the plugin:

```sh
subswapper home run -service claude -- claude --plugin-dir "$PWD/plugins/subswapper"
```

In Claude, invoke `/subswapper:cross-provider`. In Codex, select `cross-provider`
from the skills picker. Supply the target provider, task, authorized directory,
model, effort, and intent. Native subagent use in the parent remains unchanged.

The skill also activates for CLI comparisons, same-provider model probes,
app-server checks, and CLI authentication failures while an active session works.
A new CLI process does not inherit Codex's proxy launch arguments from its parent.
Bare probes can therefore report `401` or `token_expired` while routed calls work.
Check the launch route before treating that error as a model or account failure.

For full-harness discovery, follow the skill's
[CLI launch reference](../plugins/subswapper/skills/cross-provider/references/cli-launch.md).
The restricted `delegate` command disables integrations and cannot verify installed
skills or MCP discovery. `home run` preserves the normal harness configuration;
the caller must supply task permissions and a bounded process lifecycle.

## Direct execution

```sh
subswapper delegate \
  -service codex -cwd /absolute/path/to/approved/worktree \
  -model YOUR_CODEX_MODEL -effort high \
  -intent read-only -timeout 10m \
  -task 'Inspect the specified code. Report findings with file references. Do not edit or delegate.'

subswapper delegate \
  -service claude -cwd /absolute/path/to/approved/worktree \
  -model YOUR_CLAUDE_MODEL -effort high \
  -intent workspace-write -timeout 10m \
  -task 'Make only the authorized file edits described in this task. Report changed files and unresolved checks. Do not delegate.'
```

Model names are examples to replace, not shipped preferences. Effort is validated
as a provider CLI value; model-specific availability is determined by the provider.
The launcher requires every task field and refuses extra provider arguments.
Quote the task as one shell argument. Task text is visible in the caller's process
arguments, so do not include credentials. The provider receives task text on stdin.

`-config PATH` defaults to the parent's `SUBSWAPPER_CONFIG_PATH`, then the normal
Subswapper default. `-account NAME` is optional. Account selection, authentication,
proxy configuration, and proxy-down fallback all remain in `home run`. A configured
but unavailable proxy retains the existing account-login fallback and warning.
The plugin does not guarantee proxy availability or create another account manager.

## Permissions and scope

The coordinating harness must already have authority for delegation, disclosure
of task context, and requested writes. Preferences and the `-intent` flag do not
create that authority. The command cannot infer the parent's sandbox policy.
Use an approved worktree when isolation from other edits is needed.

| Provider | `read-only` | `workspace-write` |
| --- | --- | --- |
| Claude | Restricted mode; Read, Glob, Grep; `dontAsk` | Restricted mode; adds Edit and Write; `acceptEdits` |
| Codex | Native `read-only` sandbox; approvals `never` | Native `workspace-write` sandbox; approvals `never`; no extra writable roots or command network access |

Claude's `--restricted` confines file tools to working directories and ignores
user/project settings. The launcher supplies no Bash, Agent, or other execution
tools. It disables hooks and uses an empty strict MCP configuration. Claude write
tasks can edit files, but cannot run tests through shell tools. The caller runs
those checks within its existing authority.

Codex ignores user configuration and exec-policy rules, marks the task directory
untrusted to exclude project configuration, disables plugin, hook, and app
features, and supplies an empty MCP table. The Git trust check is skipped for this
explicitly scoped execution; the sandbox and approval policy still apply.
Workspace-write excludes
`/tmp` and `$TMPDIR` as additional writable roots. Put required build artifacts
inside the approved workspace. Administrator configuration and provider binaries
are trusted inputs; the launcher is not an OS sandbox around the provider itself.
In particular, configuration layering and managed integrations must not grant
capabilities outside the intended task. Read-only intent controls model tools;
provider authentication and runtime bookkeeping may still write their own state.
Narrow file ownership within the working directory remains part of the task brief.

A marker rejects another `subswapper delegate` call from a delegated process.
This prevents accidental recursive use, not deliberate environment manipulation.
No retry, provider switch, extra review gate, or background orchestration is added.

## Output and lifecycle

stdout forwards provider text. stderr forwards diagnostics, plus a generic launcher
failure when needed. Claude retains the existing launch-token output redaction.
Launcher errors do not echo task, configuration contents, or command arguments.
Provider output can contain task data; this is not a general secret scanner.

| Result | Exit status |
| --- | --- |
| Provider success | 0 |
| Provider failure | Provider exit code |
| Launcher validation/setup failure | 1 |
| Deadline, default 10 minutes | 124 |
| SIGINT cancellation | 130 |
| SIGTERM cancellation | 143 |
| Provider terminated by signal | 128 + signal number |

Provider codes can overlap launcher codes. Preserve diagnostics with the status.
A zero exit does not establish that the acceptance check passed.
Timeout and cancellation forcibly terminate the `home run` process group,
including its provider and descendants. Partial output stays available. Writes
already made are not rolled back. Processes that detach into another process group,
or a launcher killed with SIGKILL, are outside this cancellation guarantee.

## Verification

Local verification for plugin 0.1.0 used Claude Code 2.1.258 and Codex 0.153.3
on Linux. Both harnesses installed the plugin into isolated test profiles.
Authenticated synthetic read tests passed through the launcher for both providers.
The real Codex test reported `provider: subswapper` and `sandbox: read-only`.
For both providers, synthetic read-only tests preserved the fixture, while
workspace-write tests changed the in-scope fixture and preserved an outside
sibling. Claude enforced read-only intent by removing write tools; Codex returned
an OS read-only filesystem error. Claude restricted file access and Codex's
sandbox blocked the respective outside-workspace attempts.
All Codex configuration overrides stay before `exec`: mixing root-level and
exec-level `-c` options discarded proxy settings in the inspected CLI version.
A regression test covers that ordering.


Process tests compile the real Subswapper CLI and execute fake `claude` and `codex`
binaries against local HTTP proxies. They check task/argument forwarding, working
directory, proxy settings, exit propagation, input rejection, generic errors,
write-intent flags, recursion rejection, timeout, SIGINT, SIGTERM, and descendant
termination. These tests do not authenticate or prove model tool enforcement.

Run repository checks from `CONTRIBUTING.md` and `.github/workflows/ci.yml`.
Validate the Claude artifacts with:

```sh
claude plugin validate plugins/subswapper
claude plugin validate .claude-plugin/marketplace.json
```

The Codex plugin-creator manifest validator and skill-creator format validator
can also validate their respective artifacts when those developer tools are
available. Model-specific effort compatibility beyond the tested models remains
separate qualification work.
Use only synthetic fixtures and prompts for those tests. For example, read a
fixture containing `SYNTHETIC_OK`, then separately authorize editing that fixture.
Verify that read-only runs preserve it and write-capable runs cannot edit a sibling
outside the approved directory. Include direct invocation, native-subagent near
misses, and prompts that pressure the skill to exceed authority.

### Routing regression

The opt-in Go test evaluates catalog selection for nine synthetic requests:
same-provider probes, cross-provider tasks, explicit Subswapper requests,
app-server discovery, authentication failures, urgency, native agents,
explanation-only requests, and account management. It sends only the description
and synthetic requests to the selected model. Ordinary `go test ./...` skips
authenticated evaluation.

```sh
SUBSWAPPER_TEST_SERVICE=codex SUBSWAPPER_TEST_MODEL=YOUR_CODEX_MODEL \
  go test ./plugins/subswapper -run TestSkillRouting -count=2 -v

# Compare the identical cases against a saved previous skill.
SUBSWAPPER_TEST_SERVICE=codex SUBSWAPPER_TEST_MODEL=YOUR_CODEX_MODEL \
  SUBSWAPPER_TEST_SKILL=/absolute/path/to/previous/SKILL.md \
  go test ./plugins/subswapper -run TestSkillRouting -count=2 -v
```

On 2026-09-10, `gpt-6-astra` selected the expected route in all nine cases in two
runs. The previous description missed the same four cases in both baseline runs:
same-provider probes, app-server discovery, authentication errors, and urgency.
This test measures selection, not model correctness or actual child execution.
A fresh Codex app-server separately discovered the updated installed skill.
A fresh Codex model session loaded it and invoked `subswapper delegate` with
the requested model, effort, directory, and read-only intent against a CLI test
double. The test double returned the expected result and exit status.
Claude's installed files matched the source; its live Fable evaluation was
blocked by the provider's usage limit.

## Upstream references

- [Claude plugin layout and validation](https://code.claude.com/docs/en/plugins-reference)
- [Claude CLI flags](https://code.claude.com/docs/en/cli-reference)
- [Codex plugin manifests and compatible marketplaces](https://developers.openai.com/plugins/build/plugins)
