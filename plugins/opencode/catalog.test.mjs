import assert from "node:assert/strict";
import test from "node:test";
import childProcess from "node:child_process";
import { EventEmitter } from "node:events";
import { PassThrough } from "node:stream";
import { Model } from "@opencode/plugin";
import { claudeModels, discoverClaude, subscriptionModels } from "./catalog.ts";

// Synthetic discovery must not inherit routes, credentials or live-test flags.
for (const key of Object.keys(process.env)) {
  if (key.startsWith("SUBSWAPPER_")) delete process.env[key];
}

function model(providerID, id, extra = {}) {
  return { ...Model.Info.default(providerID, id), type: "chat", ...extra };
}

test("Claude discovery preserves aliases and exposes resolved versions with authoritative metadata", () => {
  const source = model("anthropic", "claude-opus-5-5", {
    name: "Claude Opus 5.5", time: { released: 1780272000000 },
    limit: { context: 1000000, input: 1000000, output: 128000 },
  });
  const models = claudeModels([{ value: "opus", displayName: "Opus", resolvedModel: source.id, contextWindow: 1000000, supportedEffortLevels: ["low", "medium", "high", "xhigh", "max"] }], [source]);
  const alias = models.find(m => m.id === "opus");
  assert.equal(alias.modelID, "opus");
  assert.equal(alias.name, "Claude Opus 5.5 (opus)");
  assert.deepEqual(alias.time, source.time);
  assert.deepEqual(alias.limit, source.limit);
  assert.deepEqual(alias.variants.map(v => v.id), ["low", "medium", "high", "xhigh", "max"]);
  assert.deepEqual(alias.variants[3].settings, { effort: "xhigh" });
  const version = models.find(m => m.id === source.id);
  assert.equal(version.modelID, source.id);
  assert.equal(version.name, source.name);
});

test("unknown Claude versions never borrow dates or limits from a different generation", () => {
  const models = claudeModels([{ value: "fable", displayName: "Fable", resolvedModel: "claude-fable-5-2", supportedEffortLevels: ["high", "pro", "malformed"] }], [model("anthropic", "claude-fable-5-1", { time: { released: 123 }, limit: { context: 200000, output: 64000 } })]);
  assert.match(models[0].name, /claude-fable-5-2/);
  assert.equal(models[0].time.released, 0);
  assert.deepEqual(models[0].limit, { context: 0, output: 0 });
  assert.deepEqual(models[0].variants.map(v => v.id), ["high"]);
});

test("ChatGPT inventory is backend-authoritative, zero cost, and has no pro reasoning modes", () => {
  const source = model("openai", "gpt-synthetic", { name: "GPT Synthetic", time: { released: 1234 }, limit: { context: 200000, output: 32000 }, cost: [{ input: 2, output: 8, cache: { read: 1, write: 3 } }], variants: [{ id: "pro", body: { reasoning: { mode: "pro" } } }, { id: "high", settings: { reasoningEffort: "high" } }] });
  const models = subscriptionModels({ models: [{ slug: source.id, display_name: "Backend name", context_window: 500000, supported_reasoning_levels: [{ effort: "low" }, { effort: "high" }, { effort: "pro" }] }, { slug: "new-backend-model", context_window: 100000 }] }, [source, model("openai", "o3-pro")]);
  assert.deepEqual(models.filter(m => m.type !== "image").map(m => m.id), [source.id, "new-backend-model"]);
  assert.equal(models[0].name, source.name);
  assert.equal(models[0].time.released, 1234);
  assert.deepEqual(models[0].limit, { context: 532000, input: 500000, output: 32000 });
  assert.deepEqual(models[0].cost, [{ input: 0, output: 0, cache: { read: 0, write: 0 } }]);
  assert.deepEqual(models[0].variants, [{ id: "low", settings: { reasoningEffort: "low" } }, { id: "high", settings: { reasoningEffort: "high" } }]);
  assert.equal(models[1].time.released, 0);
  assert.deepEqual(subscriptionModels({ error: "synthetic-private-error" }, [source]).filter(m => m.type !== "image"), []);
  assert.deepEqual(subscriptionModels({ models: [{ slug: "../../steal" }, null, {}] }, [source]).filter(m => m.type !== "image"), []);
});

test("image catalog remains available without text-inventory membership, including new 2.5 models", () => {
  const image = model("openai", "dall-e-synthetic", { type: "image", time: { released: 5678 } });
  const models = subscriptionModels({ models: [] }, [image]);
  for (const id of [image.id, "gpt-image-2", "gpt-image-2.5-flare", "gpt-image-2.5-sunburst"]) {
    const entry = models.find(m => m.id === id);
    assert.equal(entry.modelID, id);
    assert.equal(entry.type, "image");
    assert.deepEqual(entry.cost, [{ input: 0, output: 0, cache: { read: 0, write: 0 } }]);
  }
  assert.equal(models.find(m => m.id === image.id).time.released, 5678);
});

test("backend modelID membership preserves catalog Fast modes but excludes Pro modes", () => {
  const source = model("openai", "gpt-6.1-sol", { name: "GPT-6.1 Sol", limit: { context: 1050000, input: 922000, output: 128000 }, variants: [{ id: "high", settings: { reasoningEffort: "high", reasoningSummary: "auto", include: ["reasoning.encrypted_content"] } }] });
  const fast = { ...source, id: "gpt-6.1-sol-fast", name: "GPT-6.1 Sol Fast", body: { service_tier: "priority" } };
  const pro = { ...source, id: "gpt-6.1-sol-pro", name: "GPT-6.1 Sol Pro", body: { reasoning: { mode: "pro" } } };
  const models = subscriptionModels({ models: [{ slug: source.modelID, context_window: 500000, supported_reasoning_levels: [{ effort: "high" }] }] }, [source, fast, pro]);
  assert.deepEqual(models.filter(m => m.type !== "image").map(m => m.id), [source.id, fast.id]);
  assert.equal(models[0].name, source.name);
  assert.equal(models[1].modelID, source.modelID);
  assert.deepEqual(models[1].body, { service_tier: "priority" });
  assert.deepEqual(models[1].variants, source.variants);
  assert.deepEqual(models[0].limit, { context: 628000, input: 500000, output: 128000 });
  assert.deepEqual(models[1].limit, models[0].limit);
});

test("Claude discovery sends initialize control only, bounds output, and closes without a prompt", async t => {
  let oversize = false;
  const mock = t.mock.method(childProcess, "spawn", (executable, args, options) => {
    assert.equal(executable, "synthetic-claude-shim");
    assert.ok(args.includes("--no-session-persistence"));
    assert.ok(args.includes("--strict-mcp-config"));
    assert.equal(options.shell, false);
    assert.equal(Object.keys(options.env).some(key => key.startsWith("SUBSWAPPER_")), false, "discovery fixture must not inherit Subswapper environment");
    const child = Object.assign(new EventEmitter(), { stdin: new PassThrough(), stdout: new PassThrough(), kill() { this.killed = true; } });
    child.stdin.once("data", chunk => {
      const message = JSON.parse(chunk);
      assert.equal(message.type, "control_request");
      assert.equal(message.request.subtype, "initialize");
      assert.equal(message.request.prompt, undefined);
      queueMicrotask(() => child.stdout.write(oversize ? "x".repeat(1024 * 1024 + 1) : JSON.stringify({ type: "control_response", response: { subtype: "success", request_id: message.request_id, response: { models: [{ value: "opus", resolvedModel: "claude-opus-synthetic", displayName: "Opus" }], account: { token: "must not be retained" } } } }) + "\n"));
    });
    return child;
  });
  assert.deepEqual(await discoverClaude("synthetic-claude-shim"), [{ value: "opus", resolvedModel: "claude-opus-synthetic", displayName: "Opus" }]);
  assert.equal(mock.mock.calls[0].result.killed, true);
  oversize = true;
  assert.deepEqual(await discoverClaude("synthetic-claude-shim"), []);
});
