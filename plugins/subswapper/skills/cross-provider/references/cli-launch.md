# Full-harness checks

Use this route only when the authorized check needs the provider's normal
configuration, installed skills, MCP tools, or app-server protocol. For a bounded
model task, use `subswapper delegate` instead. Its restricted child intentionally
disables integrations and cannot measure full-harness discovery.

Wrap each provider process with the matching service:

```sh
subswapper home run -service codex -- codex app-server
subswapper home run -service claude -- claude --print --model MODEL --effort high --permission-mode dontAsk --tools 'Read,Glob,Grep' 'TASK'
```

These are command shapes, not permission grants. Set the authorized working
directory, actual model, effort, task, and provider permissions. For app-server,
the client must set model, effort, sandbox, and approval policy in its session
requests. Preserve the parent's authority and avoid enabling integrations that
exceed the requested check. If the harness cannot enforce that boundary, report
the check as blocked. Do not use unrestricted permission flags to make it pass.

`home run` supplies account selection and proxy routing. It does not add
`delegate`'s timeout, tool restrictions, or recursion guard. The caller owns the
process lifecycle and must keep a bounded deadline and collect its exit status.
Do not use this route to bypass a blocked delegated task.

For Codex exec checks, put every global `-c` setting before `exec`. Mixing root
and exec-level overrides can discard the injected proxy settings. Launching
Claude through Subswapper does not wrap a Codex process started by a Claude
plugin. Verify that helper's actual child command before relying on its route.

Never copy authentication files, tokens, or proxy arguments by hand. Do not
change accounts or log the user out to repair a bare CLI probe. Preserve the
launcher's diagnostics and distinguish authentication, quota, sandbox, and
unsupported-model failures.
