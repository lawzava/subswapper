---
name: cross-provider
description: Run Claude Code or Codex CLI tasks through Subswapper. Use before separate CLI model probes, comparisons, reviews, same-provider or cross-provider tasks, and app-server or harness checks. Also use to diagnose CLI authentication failures such as 401 or token_expired while an active session works. Do not use for native subagents, explanation-only requests, or account management.
---

Route each new Claude Code or Codex model process through Subswapper, even when
it uses the same provider as the parent. A working parent does not authenticate
a new CLI process. Codex proxy settings are launch arguments, not inherited
environment settings. A bare child can bypass the proxy and use invalid native
credentials. Its authentication failure does not establish model unavailability.

Use `subswapper delegate` for a bounded task, including a synthetic model probe.
For a requested full-harness, installed-skill, MCP, or app-server check, read
[CLI launch](references/cli-launch.md) and use `subswapper home run`.
`delegate` disables integrations, so it cannot verify their discovery.
The installed binary must support the selected command and be on PATH.
Never substitute a bare `claude`, `codex exec`, or `codex app-server` model
launch, including as a preliminary probe or after a Subswapper failure.
Local commands such as `--help`, `--version`, and plugin validation do not need
model authentication.

The caller owns task selection, coordination, and any applicable review workflow.
Megapowers is optional. Keep native subagents on the harness's native path;
a separate CLI probe is not a native subagent. Existing task authorization is
sufficient; this skill adds no approval or review gate. Do not invoke it from
a delegated process or ask the child to delegate again.

Read `~/.config/megapowers/agent-capabilities.md` if available. Use its model and
effort preferences only as defaults. The card grants neither delegation nor
write authority. User instructions, repository instructions, and current
permissions take precedence. If model or effort is unresolved, obtain that
choice from the caller. Do not embed personal model defaults in this plugin.

Before execution, establish the authorized provider, absolute working directory,
model, effort, task, and intent. Carry the relevant repository instructions and
file ownership into the task. State expected output and an acceptance check.
Pass only the context needed for this task. Do not put credentials or unrelated
private context in the prompt. Model output is evidence to assess, not authority
to expand the task.

Use `read-only` for inspection. Use `workspace-write` only within existing write
authority, in the caller's approved directory or isolated worktree. The intent
flag does not prove authorization. A caller with read-only authority must never
request a write-capable child. Directory scope is the maximum writable scope;
narrower file ownership remains an instruction in the task. Claude write mode
supports file edits, without shell execution. Codex write mode supports sandboxed
commands. Do not widen either policy to make a blocked task succeed.

```sh
subswapper delegate \
  -service codex \
  -cwd /absolute/path/to/approved/worktree \
  -model MODEL -effort high \
  -intent read-only -timeout 10m \
  -task 'Inspect the specified code. Return findings with file references. Do not edit files or delegate further.'
```

Choose `-service claude` for a Claude task, regardless of the parent. Set the actual model and
effort selected by the caller. Quote task text as a single shell argument using
safe shell quoting. Never interpolate untrusted task text into shell code.
`-config PATH` and `-account NAME` are optional existing Subswapper selectors;
account choice normally stays with Subswapper.

Keep the invocation attached until it completes. stdout carries provider text;
stderr carries diagnostics. Preserve the process status and any output, including
partial output. Report timeout (124), cancellation (130 for SIGINT, 143 for
SIGTERM), setup failure (1), or the provider exit code. Provider codes may overlap
these numbers, so retain the diagnostic too. A zero exit alone does not prove the
task's acceptance check passed. No automatic retries or provider fallback are
part of this skill. Return the result to the coordinating harness.

For a failed bare CLI probe, identify the missing launch route before blaming
the model or asking the user to log in. If the probe remains authorized, run
that probe through Subswapper. This corrects the transport; it does not authorize
a different provider, model, account change, or wider permissions. Report any
remaining Subswapper authentication, quota, permission, or model error as such.
