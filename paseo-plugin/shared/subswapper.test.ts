import assert from "node:assert/strict";
import { test } from "node:test";
import { reportSchema } from "./subswapper.ts";

// Shape of `subswapper status -json` on a hub client.
const sample = {
  generated_at: "2026-10-03T11:13:53.812180366Z",
  services: [
    {
      name: "claude",
      kind: "claude",
      hub: "http://100.67.68.117:7878",
      selected: "h2",
      accounts: [
        {
          name: "foxy2",
          email: "work@example.com",
          selected: false,
          ready: true,
          score: 90,
          state: "ready",
          five_hour: { used_percent: 38, resets_at: "2026-10-03T14:30:00Z" },
          weekly: { used_percent: 90, resets_at: "2026-10-04T04:00:00Z" },
          fable_weekly: { used_percent: 3, resets_at: "2026-10-04T04:00:00Z" },
          updated_at: "2026-10-03T10:51:59.756019418Z",
        },
        { name: "main", selected: false, ready: false, state: "usage unavailable" },
      ],
    },
    { name: "codex", kind: "codex", note: "hub at http://100.67.68.117:7879 unavailable", accounts: [] },
  ],
};

test("the plugin accepts subswapper status -json", () => {
  const report = reportSchema.parse(sample);
  assert.equal(report.services[0].accounts[0].weekly?.used_percent, 90);
  assert.equal(report.services[1].note, "hub at http://100.67.68.117:7879 unavailable");
});

test("the plugin rejects a report without accounts", () => {
  assert.throws(() => reportSchema.parse({ generated_at: "x", services: [{ name: "claude", kind: "claude" }] }));
});
