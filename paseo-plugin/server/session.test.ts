import assert from "node:assert/strict";
import { test } from "node:test";
import { sessionEnvironment } from "./session.ts";

const wrapped = { extends: "codex", command: ["/go/bin/subswapper", "home", "run", "-service", "codex", "--", "codex"] };

test("a Codex session through subswapper names its service and hides the placeholder login from Paseo's Codex usage", () => {
  assert.deepEqual(sessionEnvironment("codex-subswapper", wrapped, { KEEP: "1" }), {
    KEEP: "1",
    SUBSWAPPER_SERVICE: "codex",
    OPENAI_BASE_URL: "https://api.openai.com/v1",
  });
  // A provider that runs Codex directly with the proxy flags marks itself with SUBSWAPPER_SERVICE.
  const direct = { extends: "codex", command: ["/usr/bin/codex", "-c", "x=y"], env: { SUBSWAPPER_SERVICE: "work", OPENAI_BASE_URL: "" } };
  assert.deepEqual(sessionEnvironment("codex-subswapper", direct, {}), { SUBSWAPPER_SERVICE: "work", OPENAI_BASE_URL: "https://api.openai.com/v1" });
  // The built-in codex provider can be pointed at subswapper too.
  assert.equal(sessionEnvironment("codex", { command: ["subswapper", "codex"] }, {})?.OPENAI_BASE_URL, "https://api.openai.com/v1");
});

test("an OPENAI_BASE_URL someone already chose is kept", () => {
  assert.equal(sessionEnvironment("codex-subswapper", wrapped, { OPENAI_BASE_URL: "http://gateway/v1" })?.OPENAI_BASE_URL, "http://gateway/v1");
  assert.equal(sessionEnvironment("codex-subswapper", { ...wrapped, env: { OPENAI_BASE_URL: "http://gateway/v1" } }, {})?.OPENAI_BASE_URL, undefined);
});

test("a Claude session through subswapper only names its service", () => {
  const claude = { extends: "claude", command: ["/go/bin/subswapper", "home", "run", "-service", "claude", "--", "claude"] };
  assert.deepEqual(sessionEnvironment("claude-subswapper", claude, {}), { SUBSWAPPER_SERVICE: "claude" });
});

test("sessions that do not launch subswapper are left alone", () => {
  assert.equal(sessionEnvironment("codex", undefined, {}), null);
  assert.equal(sessionEnvironment("codex-native", { extends: "codex", command: ["/usr/bin/codex"] }, {}), null);
  assert.equal(sessionEnvironment("grok", { extends: "acp", command: ["subswapper", "codex"] }, {}), null);
  assert.equal(sessionEnvironment("claude-subswapper", { extends: "claude", command: ["subswapper", "claude"] }, { SUBSWAPPER_SERVICE: "claude" }), null);
});
