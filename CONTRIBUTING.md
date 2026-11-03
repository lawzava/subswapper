# Contributing to subswapper

Thanks for your interest in contributing! Bug reports, fixes, and focused
improvements are all welcome.

## Getting started

You need Go 1.26.10 or newer. Then:

```sh
git clone https://github.com/lawzava/subswapper.git
cd subswapper
go run ./internal/tools/check -tier=fast
```

Keep dependencies minimal and avoid adding new ones. The Go module currently
uses `github.com/creack/pty`, `golang.org/x/term`, and indirectly `golang.org/x/sys`.

## Development workflow

1. Fork the repository and create a branch from `main`.
2. Make your change, keeping it as small and focused as possible.
3. Make sure the checks that CI runs pass locally:

   ```sh
   go run ./internal/tools/check -tier=fast  # while iterating
   go run ./internal/tools/check -tier=full  # before completion
   go run ./internal/tools/check -list       # tiers, steps and prerequisites
   ```

   The full tier matches CI's OS split: coverage and race tests run on Linux
   and macOS; lint, vulnerability checks, Windows cross-compilation and the
   Paseo plugin checks run on Linux. Install the plugin dependencies first
   with `npm ci --no-audit --no-fund` in `paseo-plugin/`. Missing
   `golangci-lint`, `govulncheck` or `npm` prints `SKIPPED` locally; CI uses
   `-ci` to make missing tools fatal. CI pins golangci-lint v2.12.2 and
   govulncheck v1.5.0. The gate runs all selected steps, then fails if any
   failed. Fix failures and report skips before calling a change verified.

4. Open a pull request with a clear description of the problem and the fix.

## Guidelines

- **Preserve behavior guarantees.** This tool moves credential files around.
  Atomic writes, `0600`/`0700` permissions, cross-process locking, and the
  never-overwrite-a-good-backup rules are load-bearing — changes that weaken
  them will not be merged.
- **Add tests.** Every behavior change or bug fix needs a test that fails
  without it. The existing tests in `internal/subswapper` show the style:
  plain table-style tests against temp directories, no test frameworks.
- **Keep changes surgical.** No drive-by refactors or reformatting in the
  same PR as a functional change.
- **Commit messages** follow [Conventional Commits](https://www.conventionalcommits.org/)
  (`feat:`, `fix:`, `refactor:`, `test:`, `chore:`, …).

## Reporting bugs

Open an issue with the command you ran, what you expected, and what happened
instead. Include your config (with any emails or account names redacted) when
it is relevant.

**Never include credential file contents, tokens, or backup directory
listings in an issue.** For anything security-sensitive, see
[SECURITY.md](SECURITY.md).
