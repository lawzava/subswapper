import assert from "node:assert/strict";
import { test } from "node:test";
import { accountLoad, headerLabel, percent, relativeTime, windowText } from "./format.ts";

const now = Date.parse("2026-10-03T10:00:00Z");

test("headerLabel summarizes each service's selected account", () => {
  const label = headerLabel({
    generated_at: "2026-10-03T10:00:00Z",
    services: [
      { name: "claude", kind: "claude", accounts: [
        { name: "a", selected: false, ready: true, score: 10, state: "ready" },
        { name: "b", selected: true, ready: true, score: 86, state: "ready" },
      ] },
      { name: "codex", kind: "codex", accounts: [
        { name: "c", selected: true, ready: false, state: "limit reached", weekly: { used_percent: 100 } },
      ] },
      { name: "empty", kind: "claude", note: "hub at x unavailable", accounts: [] },
    ],
  });
  assert.equal(label, "claude 86% · codex 100%");
  assert.equal(headerLabel(null), "subswapper");
});

test("accountLoad prefers the score and falls back to the fullest window", () => {
  assert.equal(accountLoad({ name: "a", selected: false, ready: true, score: 42, state: "ready" }), 42);
  assert.equal(
    accountLoad({ name: "a", selected: false, ready: false, state: "x", five_hour: { used_percent: 20 }, weekly: { used_percent: 55 } }),
    55,
  );
  assert.equal(accountLoad({ name: "a", selected: false, ready: false, state: "x" }), null);
  assert.equal(percent(null), "-");
});

test("relativeTime and windowText render compact distances", () => {
  assert.equal(relativeTime("2026-10-03T10:00:20Z", now), "now");
  assert.equal(relativeTime("2026-10-03T10:03:00Z", now), "3m");
  assert.equal(relativeTime("2026-10-03T15:00:00Z", now), "5h");
  assert.equal(relativeTime("2026-10-06T10:00:00Z", now), "3d");
  assert.equal(relativeTime(undefined, now), null);
  assert.equal(windowText("week", { used_percent: 86.4, resets_at: "2026-10-04T07:00:00Z" }, now), "week 86% · resets in 21h");
  assert.equal(windowText("5h", undefined, now), null);
});
