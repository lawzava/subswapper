# subswapper

Subswapper lets Claude Code and Codex share several Claude and ChatGPT
subscriptions. A local proxy attaches the selected account's token to each
request, so sessions can switch accounts without restarting. A Tailscale hub
can hold accounts and serve other machines. Start with README.md for behavior
and CONTRIBUTING.md for contribution policy.

## Map

- `cmd/`: the subswapper CLI, setup, account commands and bounded delegation.
- `internal/subswapper/`: config, credential storage, locking, proxies, hub,
  launchers, usage monitoring and account selection.
- `internal/repohygiene/`: repository-wide tests for machine-specific paths.
- `internal/tools/check/`: development/CI gate, outside the shipped CLI.
- `paseo-plugin/`: TypeScript Paseo integration, usage UI and session hooks.
- `plugins/`: Claude Code/Codex plugin manifests, routing skill and its tests.
- `docs/`: CLI plugin installation, delegation and harness integration guide.

## Verify

Run from the repository root; prefer the shared gate to hand-built checks:

```sh
go run ./internal/tools/check -tier=fast  # during iteration
go run ./internal/tools/check -tier=full  # before completion
go run ./internal/tools/check -list       # menu and prerequisites
```

The full tier follows CI's OS split; Linux runs the extra lint, vulnerability,
Windows build and plugin checks. Install plugin dependencies with
`npm ci --no-audit --no-fund` in `paseo-plugin/` first. Missing optional tools
print `SKIPPED`; `-ci` makes skips fail. Report skips, not a blanket pass.
For focused checks that do not fit a tier, keep the gate's flags, for example
`go test -race -count=1 ./internal/subswapper -run TestName`.
Add regression tests for behavior changes. Keep dependencies minimal; avoid new ones.

## Do not weaken

Preserve atomic writes, credential file/directory modes `0600`/`0700`,
cross-process locking, and the rule never to overwrite a good backup.
These are load-bearing guarantees in CONTRIBUTING.md.

## Boundaries

Never read or print credential/auth files or `.env*` contents. Use synthetic
fixtures and temporary directories for tests. Do not run account add, remove,
login, or hub-affecting commands without the maintainer's approval; accounts
may live on a separate hub. Never include secrets or backup listings in reports.

## Release

No documented release flow; ask before tagging.
