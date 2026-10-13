import assert from "node:assert/strict";
import { test } from "node:test";
import type { PluginHandlerContext } from "@getpaseo/plugin/server";
import { configureCodexProvider, launchedService } from "./configure.ts";
import { launchConfigurationSchema, type LaunchConfiguration } from "../shared/subswapper.ts";

type ConfigApi = PluginHandlerContext["paseo"]["config"];

const shim = "/data/subswapper/bin/codex";
const launch: LaunchConfiguration = {
  command: [shim],
  env: { SUBSWAPPER_CONFIG_PATH: "/data/subswapper/config.json", SUBSWAPPER_SERVICE: "codex" },
};

function fakeConfig(providers: Record<string, unknown>) {
  const patches: unknown[] = [];
  const config = {
    get: async () => ({ config: { providers } }),
    patch: async (patch: unknown) => { patches.push(patch); },
  } as unknown as ConfigApi;
  return { config, patches };
}

test("launchedService reads shims, wrapper flags, and provider shortcuts", () => {
  assert.equal(launchedService({ command: [shim], env: { SUBSWAPPER_SERVICE: "work" } }), "work");
  assert.equal(launchedService({ command: [shim] }), "codex");
  assert.equal(launchedService({ command: ["/usr/bin/subswapper", "home", "run", "-service", "work", "--", "codex"] }), "work");
  assert.equal(launchedService({ command: ["/usr/bin/subswapper", "home", "run", "--service=work", "--", "codex"] }), "work");
  assert.equal(launchedService({ command: ["subswapper", "-config", "/c.json", "codex"] }), "codex");
  assert.equal(launchedService({ command: ["C:\\Tools\\subswapper.exe", "claude"] }), "claude");
  assert.equal(launchedService({ command: ["/usr/bin/codex"] }), null);
  assert.equal(launchedService({}), null);
});

test("configure Codex repoints every provider that launches the service and nothing else", async () => {
  const providers = {
    "codex-subswapper": { extends: "codex", label: "Mine", command: ["/go/bin/subswapper", "home", "run", "-service", "codex", "--", "codex"], env: { KEEP: "1" } },
    "codex-short": { extends: "codex", label: "Short", command: ["subswapper", "codex"] },
    "codex-native": { extends: "codex", label: "Exported", command: ["/usr/bin/codex", "-c", "x=y"], env: { SUBSWAPPER_SERVICE: "codex" } },
    "codex-work": { extends: "codex", label: "Work", command: ["subswapper", "home", "run", "-service", "work"] },
    "claude-subswapper": { extends: "claude", label: "Claude", command: ["subswapper", "home", "run", "-service", "codex"] },
    codex: { enabled: false },
  };
  const before = structuredClone(providers);
  const { config, patches } = fakeConfig(providers);
  const result = await configureCodexProvider({ config }, "codex", launch);
  const update = { command: launch.command, env: launch.env };
  assert.deepEqual(patches, [{ providers: { "codex-subswapper": update, "codex-short": update, "codex-native": update } }]);
  assert.match(result.message, /codex-subswapper, codex-short, codex-native/);
  assert.deepEqual(providers, before);
});

test("configure Codex adds a provider when none launches the service", async () => {
  const { config, patches } = fakeConfig({ codex: { enabled: true } });
  await configureCodexProvider({ config }, "codex", launch);
  assert.deepEqual(patches, [{ providers: { "codex-subswapper": { extends: "codex", label: "Codex (Subswapper)", command: launch.command, env: launch.env } } }]);

  const other = fakeConfig({});
  const work = { ...launch, env: { ...launch.env, SUBSWAPPER_SERVICE: "Work.1" } };
  await configureCodexProvider({ config: other.config }, "Work.1", work);
  assert.deepEqual(Object.keys((other.patches[0] as { providers: object }).providers), ["codex-subswapper-work-1"]);
});

test("configure Codex refuses to take over an unrelated provider ID", async () => {
  const { config, patches } = fakeConfig({ "codex-subswapper": { extends: "codex", label: "Plain Codex" } });
  await assert.rejects(configureCodexProvider({ config }, "codex", launch), /does not launch Subswapper/);
  assert.deepEqual(patches, []);
});

test("launch configuration refuses credentials and wrapper commands", () => {
  assert.deepEqual(launchConfigurationSchema.parse(launch), launch);
  assert.throws(() => launchConfigurationSchema.parse({ ...launch, env: { ...launch.env, OPENAI_API_KEY: "private-token" } }));
  assert.throws(() => launchConfigurationSchema.parse({ ...launch, command: [shim, "--dangerously-bypass-approvals-and-sandbox"] }));
});
