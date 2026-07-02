# Contributing to subswapper

Thanks for your interest in contributing! Bug reports, fixes, and focused
improvements are all welcome.

## Getting started

You need Go 1.26 or newer. Then:

```sh
git clone https://github.com/lawzava/subswapper.git
cd subswapper
go test ./...
```

There are no dependencies beyond the Go standard library, and the goal is to
keep it that way.

## Development workflow

1. Fork the repository and create a branch from `main`.
2. Make your change, keeping it as small and focused as possible.
3. Make sure the checks that CI runs pass locally:

   ```sh
   gofmt -l .        # must print nothing
   go vet ./...
   go test ./...
   GOOS=windows GOARCH=amd64 go build ./...   # CI also cross-compiles
   ```

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
