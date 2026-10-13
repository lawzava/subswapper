import assert from "node:assert/strict";
import { test } from "node:test";
import type { StatusReport } from "../shared/subswapper.ts";
import { cachedReader, usageAccounts, usageReport } from "./usage.ts";

const now = Date.parse("2026-10-13T10:00:00Z");

const report: StatusReport = {
  generated_at: "2026-10-13T10:00:00Z",
  services: [
    {
      name: "claude",
      kind: "claude",
      hub: "http://100.64.0.1:7878",
      selected: "foxy2",
      accounts: [
        {
          name: "foxy2",
          email: "work@example.com",
          selected: true,
          ready: true,
          score: 72,
          state: "ready",
          five_hour: { used_percent: 72, resets_at: "2026-10-13T11:00:00Z" },
          weekly: { used_percent: 6, resets_at: "2026-10-18T04:00:00Z" },
          fable_weekly: { used_percent: 0, resets_at: "2026-10-04T04:00:00Z" },
          updated_at: "2026-10-13T09:57:00Z",
        },
        { name: "main", selected: false, ready: false, state: "usage unavailable" },
      ],
    },
    {
      name: "codex",
      kind: "codex",
      selected: "main",
      accounts: [
        {
          name: "main",
          selected: true,
          ready: true,
          score: 95,
          state: "ready",
          weekly: { used_percent: 95, resets_at: "2026-10-20T06:57:11Z" },
          updated_at: "2026-10-13T09:59:50Z",
        },
      ],
    },
    { name: "work", kind: "codex", note: "hub at http://100.64.0.1:7880 unavailable", accounts: [] },
  ],
};

test("global discovery lists each service's selected account and every account", () => {
  assert.deepEqual(usageAccounts(report, "claude", { kind: "global" }), [
    { key: "claude", label: "Selected account", harness: "Subswapper", input: { service: "claude" } },
    { key: "claude.foxy2", label: "foxy2 · work@example.com", harness: "Subswapper", input: { service: "claude", account: "foxy2" } },
    { key: "claude.main", label: "main", harness: "Subswapper", input: { service: "claude", account: "main" } },
  ]);
  assert.deepEqual(usageAccounts(report, "codex", { kind: "global" }).map((account) => [account.key, account.label]), [
    ["codex", "Selected account"],
    ["codex.main", "main"],
    ["work", "work selected account"],
  ]);
});

test("session discovery follows the service the session launches through", () => {
  const session = (provider: string, env: Record<string, string>) => usageAccounts(report, "codex", { kind: "session", provider, env });
  assert.deepEqual(session("codex", { SUBSWAPPER_SERVICE: "codex" }).map((account) => account.key), ["codex"]);
  assert.deepEqual(session("codex", { SUBSWAPPER_SERVICE: "work" }).map((account) => account.key), ["work"]);
  // A session without subswapper, or one whose service is another kind, gets no card.
  assert.deepEqual(session("codex", {}), []);
  assert.deepEqual(session("codex", { SUBSWAPPER_SERVICE: "claude" }), []);
  assert.deepEqual(session("claude", { SUBSWAPPER_SERVICE: "codex" }), []);
});

test("the selected-account card reports the account subswapper routes to now", () => {
  assert.deepEqual(usageReport(report, { service: "claude" }, now), {
    status: "available",
    planLabel: "foxy2",
    windows: [
      { id: "five_hour", label: "5-hour", shortLabel: "5h", summary: true, usedPct: 72, remainingPct: 28, resetsAt: "2026-10-13T11:00:00Z", tone: "warning" },
      { id: "weekly", label: "Weekly", shortLabel: "wk", summary: true, usedPct: 6, remainingPct: 94, resetsAt: "2026-10-18T04:00:00Z", tone: "ok" },
      // A reset in the past means the window restarted; subswapper already reports it as 0%.
      { id: "fable_weekly", label: "Fable weekly", shortLabel: "Fable", usedPct: 0, remainingPct: 100, resetsAt: null, tone: "ok" },
    ],
    details: [
      { id: "account", label: "Account", value: "foxy2 · work@example.com" },
      { id: "state", label: "State", value: "ready", tone: "ok" },
      { id: "updated", label: "Updated", value: "3m ago" },
    ],
  });
  const codex = usageReport(report, { service: "codex" }, now);
  assert.equal(codex.status, "available");
  if (codex.status === "available") {
    assert.deepEqual(codex.windows.map((window) => [window.id, window.tone]), [["weekly", "danger"]]);
    assert.deepEqual(codex.details?.at(-1), { id: "updated", label: "Updated", value: "just now" });
  }
});

test("an account card shows that account without claiming the summary", () => {
  assert.deepEqual(usageReport(report, { service: "claude", account: "main" }, now), {
    status: "available",
    windows: [],
    details: [{ id: "state", label: "State", value: "usage unavailable", tone: "warning" }],
  });
  const foxy = usageReport(report, { service: "claude", account: "foxy2" }, now);
  assert.ok(foxy.status === "available" && foxy.windows.every((window) => window.summary === undefined));
});

test("missing services and accounts are errors, with the service note when there is one", () => {
  assert.deepEqual(usageReport(report, { service: "work" }, now), { status: "error", error: "hub at http://100.64.0.1:7880 unavailable" });
  assert.deepEqual(usageReport(report, { service: "codex", account: "gone" }, now), { status: "error", error: "codex has no account gone" });
  assert.deepEqual(usageReport(report, { service: "gone" }, now), { status: "error", error: "subswapper has no gone service" });
});

test("cachedReader shares one read until it expires and never caches a failure", async () => {
  let clock = 0;
  let reads = 0;
  let fail = true;
  const read = cachedReader(async () => {
    reads += 1;
    if (fail) {
      throw new Error("hub down");
    }
    return report;
  }, 10_000, () => clock);
  await assert.rejects(read(), /hub down/);
  fail = false;
  const [first, second] = await Promise.all([read(), read()]);
  assert.equal(first, second);
  assert.equal(reads, 2);
  clock = 9_999;
  await read();
  assert.equal(reads, 2);
  clock = 10_000;
  await read();
  assert.equal(reads, 3);
});
