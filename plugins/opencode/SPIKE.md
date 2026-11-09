# OpenCode v2.2.6 feasibility result, 2026-11-09

Historical spike record. Current implementation and supported setup are in
[README.md](README.md); the limitations below describe the original spike.

## Verdict: works with caveats

The built-in `openai/gpt-6.1-sol-fast` returned `OK` through the existing
Subswapper Codex proxy. One live prompt was sent. This is an uncommitted local
prototype, not a supported integration or a release.

The test activated the built-in provider with a dummy `OPENAI_API_KEY`, then
changed its native Responses HTTP route using public plugin hooks. Neither
ChatGPT OAuth method was configured or copied. This avoids OpenCode's login
and token-refresh paths, but does not prove compatibility with an existing
legacy or token-sharing connection in OpenCode-web.

## Live evidence

The run used a fresh private HOME and XDG config/data/state/cache directories,
the installed standalone binary, HTTP transport, warming disabled, a fixed
title, and denied tool permissions. The plugin read the existing placeholder
only at runtime from its explicit path. Its value was not copied or logged.

The following is the executed invocation with machine-specific paths expressed
as variables. `spike` was a private directory under `$TMPDIR/opencode`, `root`
was this worktree, `bin` was `$HOME/.opencode/bin/opencode`, and `placeholder`
was `$HOME/.local/share/subswapper/tokens/codex/proxy-placeholder.json`.

```sh
env -i PATH="$PATH" HOME="$spike/attempt2/home" \
  XDG_CONFIG_HOME="$spike/attempt2/config" \
  XDG_DATA_HOME="$spike/attempt2/data" \
  XDG_STATE_HOME="$spike/attempt2/state" \
  XDG_CACHE_HOME="$spike/attempt2/cache" \
  OPENAI_API_KEY=placeholder \
  SUBSWAPPER_CODEX_PLACEHOLDER="$placeholder" SUBSWAPPER_CODEX_AUDIT=1 \
  timeout 120 strace -f -e trace=connect -o "$spike/connect-2.log" \
  "$bin" --standalone run -m openai/gpt-6.1-sol-fast \
    --title 'Proxy feasibility acceptance' --format json \
    'Do not call tools. Reply with exactly: OK'
```

Exit: **0**. Output excerpts:

```text
{"type":"text",...,"part":{...,"type":"text","text":"OK",...}}
{"type":"step_finish",...,"reason":"stop","cost":0.003316,
 "tokens":{"input":804,"output":5,"reasoning":0,"cache":{"read":0,"write":0}}}
```

Connect-only trace, line 28, with PID and descriptor redacted:

```text
<pid> connect(<fd>, {sa_family=AF_INET, sin_port=htons(7879),
 sin_addr=inet_addr("127.0.0.1")}, 16) = -1 EINPROGRESS
```

The full trace also contained external TCP/443, DNS, and other local
connections. It does not prove that all OpenCode traffic used Subswapper.

`subswapper status` immediately afterward, 2026-11-09T23:05:25+02:00:

```text
SERVICE ACCOUNT   SELECTED STATE
codex   account-a          ready
codex   account-b yes      ready
codex: accounts on the hub at http://<hub>:7879
```

Account labels and hub address are redacted; live usage figures are omitted.
An authenticated, runtime-only health probe to the same loopback proxy returned
`HTTP 200 service="codex" proxy="subswapper"`. No credential value was logged.

Together, the loopback connection, successful reply with a dummy upstream key,
and status establish proxy use. Status shows selection, not per-request account
attribution; the same account was selected before and after, but the proxy can fall back
on a rejected credential. No per-request success log was available.

An earlier launch failed before HTTP dispatch with `Expected JSON value at
["encoding"]`. The fix deletes the encoding key rather than assigning
`undefined`. A regression test failed before that fix and passed afterward.

## Local checks

- `TMPDIR=/var/tmp/agent-scratch/opencode node --test plugins/opencode/index.test.mjs`:
  10 tests passed after the final code changes.
- `node --check plugins/opencode/index.ts`: exit 0.
- A separate isolated `"$bin" --standalone models` with the README's
  `.opencode/plugins/subswapper` layout: exit 0, listed
  `openai/gpt-6.1-sol-fast`; its log confirmed loading the copied plugin.
  This also validated `warming`, `compaction.auto` and `permissions` in that
  sample configuration without a model call.
- `go test -race -count=1 -v ./internal/subswapper -run
  'TestCodexProxy(RelaysUsageLimitWithoutRetrying|RelaysThrottleAndFailsOverOnRejectedToken|SwapsIdentityAndPassesAnonymousCallsThrough)$'`:
  all three tests passed.
- `npm ci --no-audit --no-fund` in `paseo-plugin`: exit 0.
- `go run ./internal/tools/check -tier=full`: **exit 1**, 7 checks passed,
  2 failed, 0 skipped. Gofmt, vet, coverage, race, Windows build, existing
  Paseo plugin typecheck and its 24 tests passed. Unchanged files failed
  golangci-lint 2.15.0 errcheck at `cmd/subswapper/delegate_unix.go:46`,
  `internal/subswapper/fixes_test.go:119`, `internal/subswapper/hub.go:117`.
  Govulncheck was built with Go 1.26.4 and failed to parse the installed
  Go 1.27.3 standard library. No unrelated fixes were made.

The shared gate does not include the new OpenCode plugin tests.

## What this does and does not prove

- **Request body:** a synthetic capture before integration showed native
  Responses keys `include input instructions model prompt_cache_key
  service_tier store stream text tools`, `stream=true`, `store=false`, and
  no `max_output_tokens`. No body adaptation was required for the successful
  live prompt. Hook-level byte preservation is tested; post-conversion wire
  byte identity was not measured.
- **Streaming:** one short native Responses stream decoded successfully.
  Long streams, tool calls, continuations and cancellation remain untested.
- **Transport:** HTTP worked. WebSockets are blocked. The proxy itself returns
  501 for WebSocket upgrades (`codex_proxy.go:414-417`).
- **Compaction:** summary mode is selected; compaction was not exercised.
  `/responses/compact` is intentionally rejected by the endpoint guard.
- **Usage:** the proxy uses the selected subscription and its existing usage
  monitoring. The CLI's nonzero `cost` is model-catalog accounting, not evidence
  of an API charge. No exact subscription usage increment was measured.
- **Failover:** not tested live. The current proxy relays 429 unchanged and
  updates quota observations. It retries another account only on a rejected
  credential, not a quota rejection (`codex_proxy.go:439-480`).
- **Safety limits:** placeholder parent directories must be trusted. The
  placeholder is a proxy shared secret. Redirect suppression across OpenCode's
  native Request conversion is unverified. The console audit did not appear
  in standalone CLI output and is not a reliable audit sink.

## Production work belongs to a separate decision

1. Choose supported ChatGPT access policy and coexistence with direct
   token-sharing/legacy credentials. Do not silently replace a real login.
2. Package and version the plugin against the real SDK. Add SDK typechecks,
   native dispatch tests, configurable validated endpoint/service discovery,
   trusted placeholder-parent validation and verified redirect handling.
3. Load it through the OpenCode-web server's `plugins` configuration, in that
   server's environment. Provide the placeholder **path**, not its value.
   Verify provider-transform and hook order with the active connection.
   Nothing was installed into OpenCode-web during this spike.
4. Add a doctor check for plugin/version loading, safe placeholder access,
   authenticated proxy health, selected-account readiness, HTTP transport,
   route resolution, and unsupported compaction paths. Keep paid inference
   probes opt-in.
5. Add real streaming/tool/compaction/account-switch acceptance, bounded retry
   policy consistent with Subswapper, accurate subscription usage display,
   and secret-free operational audit records.

Open questions: should this replace or coexist with direct token-sharing;
what policy applies to the legacy Codex endpoint; should estimated API cost be
hidden for subscription-backed traffic? HQ owns these production decisions.
