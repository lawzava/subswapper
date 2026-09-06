---
name: cross-provider
description: Execute an already-authorized, bounded task with the other provider, Claude Code or Codex, through Subswapper. Use after the user or Megapowers selects cross-provider execution. Do not use for native subagents, routing decisions, or account management.
---

Use `subswapper delegate` for one cross-provider task. The installed Subswapper
binary must include this command and be on PATH. Never substitute a plain
`claude`, `codex exec`, or `codex app-server` launch if it fails.

Megapowers owns delegation decisions, coordination, and any applicable review
workflow. Keep same-provider work on the harness's native subagent path. This
skill adds no review gate. Do not invoke it from a delegated process or ask the
child to delegate again.

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

Choose `-service claude` for the reverse direction. Set the actual model and
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
