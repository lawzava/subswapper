import assert from "node:assert/strict";
import childProcess from "node:child_process";
import { EventEmitter } from "node:events";
import { PassThrough } from "node:stream";
import { chmod, mkdir, mkdtemp, readFile, rm, symlink, writeFile } from "node:fs/promises";
import { createServer, globalAgent, request as httpRequest } from "node:http";
import { tmpdir } from "node:os";
import { join } from "node:path";
import test from "node:test";
import { Effect } from "effect";
import { FetchHttpClient, HttpClient, HttpClientRequest } from "effect/http";
import { make as makeAI, Media } from "@opencode/ai";
import * as OpenAI from "@opencode/ai/providers/openai";
import { Model } from "@opencode/plugin";
import plugin from "./index.ts";
import { isPrivateGroup, privateGroupRecords, supportsPrivateGroupNSS } from "./private-group.ts";

const placeholder = { token: "synthetic.fixture.signature", account_id: "subswapper-proxy" };
// Never let the caller's integration environment turn synthetic tests into
// reads of an installed placeholder or calls to a running proxy.
for (const key of Object.keys(process.env)) {
  if (key.startsWith("SUBSWAPPER_")) delete process.env[key];
}
function secretEqual(actual, expected, message = "credential must match the synthetic fixture") {
  assert.equal(actual === expected, true, message);
}
async function rejectsSafely(promise, accepts = () => true) {
  let matched = false;
  try { await promise; } catch (error) { matched = accepts(error); }
  assert.equal(matched, true, "operation must reject with the expected safe error");
}
async function server(t, handler) {
  const server = createServer(handler);
  await new Promise(resolve => server.listen(0, "127.0.0.1", resolve));
  t.after(() => new Promise(resolve => { server.closeAllConnections(); server.close(resolve); }));
  return `http://127.0.0.1:${server.address().port}`;
}
async function fixture(t, handler = (_, response) => response.end("synthetic response")) {
  assert.equal(Object.keys(process.env).some(key => key.startsWith("SUBSWAPPER_")), false, "fixture must not inherit Subswapper environment");
  const directory = await mkdtemp(join(tmpdir(), "subswapper-opencode-test-"));
  await chmod(directory, 0o700);
  t.after(() => rm(directory, { recursive: true, force: true }));
  const proxy = await server(t, (r, w) => {
    assert.equal(r.headers["x-subswapper-bridge"] === undefined, true, "upstream must not receive the bridge capability header");
    if (r.url === "/backend-api/codex/models?client_version=2.2.6") { secretEqual(r.headers.authorization, `Bearer ${placeholder.token}`); w.setHeader("content-type", "application/json"); w.end(JSON.stringify({ models: [{ slug: "gpt-synthetic", supported_reasoning_levels: [{ effort: "high" }] }] })); return; }
    handler(r, w);
  });
  const path = join(directory, "tokens", "codex", "proxy-placeholder.json");
  await mkdir(join(directory, "tokens", "codex"), { recursive: true, mode: 0o700 });
  await writeFile(path, JSON.stringify(placeholder), { mode: 0o600 });
  const config = join(directory, "config.json");
  await writeFile(config, JSON.stringify({ state_path: join(directory, "state.json"), services: [
    { name: "codex", kind: "codex", proxy_listen: proxy.slice(7) },
  ] }), { mode: 0o600 });
  return { directory, path, config, baseURL: `${proxy}/backend-api/codex`, endpoint: `${proxy}/backend-api/codex/responses` };
}
async function harness(t, options, overrides = [], { storageEntries = [], claudeCatalog = [] } = {}) {
  // Do not start a real Claude installation from any synthetic routing test.
  t.mock.method(childProcess, "spawn", () => {
    const child = Object.assign(new EventEmitter(), { stdin: new PassThrough(), stdout: new PassThrough(), kill() {} });
    queueMicrotask(() => child.emit("error", new Error("synthetic missing Claude executable")));
    return child;
  });
  const hooks = new Map();
  const provider = { settings: { transport: "websocket", encoding: "gzip", retained: true } };
  const claudeProvider = { activation: "disabled" };
  const models = new Map([["claude-agent", claudeCatalog]]);
  const storage = new Map(storageEntries);
  const cleanup = await plugin.setup({ options, provider: {
    async transform(callback) { callback({
      get(id) { return { provider: { id }, models: new Map((id === "claude-agent" ? claudeCatalog : []).map(model => [model.id, model])) }; },
      update(id, update) { assert.equal(["openai", "claude-agent"].includes(id), true); update(id === "openai" ? provider : claudeProvider); },
      models: { set(id, inventory) { models.set(id, inventory); } },
    }); },
  }, model: { async transform(callback) { models.set("openai", [...(models.get("openai") ?? []), ...overrides]); callback({
    list() { return models.get("openai") ?? []; },
    remove(id, modelID) { models.set(id, models.get(id).filter(model => model.id !== modelID)); },
    update(id, modelID, callback) { callback(models.get(id).find(model => model.id === modelID)); },
  }); } }, storage: { async get(key) { return storage.get(key); }, async set(key, value) { storage.set(key, value); } }, session: {
    async hook(name, callback, options) { assert.deepEqual(options, { providerID: "openai" }); hooks.set(name, callback); },
  } });
  if (cleanup) t.after(cleanup);
  return { hooks, provider, claudeProvider, models, storage };
}
function request(endpoint, raw = '\n{ "instructions": "keep", "stream": true, "tools": [], "max_output_tokens":123 }\n', signal) {
  return new Request(endpoint, { method: "POST", headers: { authorization: "Bearer dummy", "content-type": "application/json", "x-keep": "yes" }, body: raw, signal });
}
async function nativeDispatch(request) {
  // The real SDK conversion drops Request.redirect. Exercise that boundary,
  // not a direct fetch of the hook's original Web Request.
  return Effect.runPromise(Effect.scoped(Effect.gen(function* () {
    const response = yield* HttpClient.execute(HttpClientRequest.fromWeb(request));
    return { status: response.status, body: yield* response.text, headers: response.headers };
  }).pipe(Effect.provide(FetchHttpClient.layer))));
}

test("inherited Subswapper environment cannot enter synthetic fixtures", () => {
  for (const [file, name] of [
    ["index.test.mjs", "real SDK pin and provider settings; config option alone derives proxy and placeholder"],
    ["catalog.test.mjs", "Claude discovery sends initialize control only, bounds output, and closes without a prompt"],
  ]) {
    const inherited = {
      SUBSWAPPER_CODEX_PLACEHOLDER: "/synthetic/inherited-placeholder.json",
      SUBSWAPPER_CODEX_PROXY_URL: "http://127.0.0.1:1",
      SUBSWAPPER_ARBITRARY: "synthetic-inherited-value",
      SUBSWAPPER_LIVE_TEST: "1",
    };
    const child = childProcess.spawnSync(process.execPath, ["--test", `--test-name-pattern=^${name}$`, file], {
      cwd: new URL(".", import.meta.url), env: { ...Object.fromEntries(Object.keys(process.env).filter(key => !key.startsWith("SUBSWAPPER_") && key !== "NODE_TEST_CONTEXT").map(key => [key, process.env[key]])), ...inherited }, encoding: "utf8", timeout: 30000, maxBuffer: 1024 * 1024,
    });
    // Keep captured output private, including on failure. Only booleans reach
    // assertion diagnostics, never headers, paths, or inherited values.
    assert.equal(child.status === 0, true, `${file} must isolate inherited Subswapper environment`);
    assert.equal(child.stdout.includes(name), true, "selected synthetic child test must actually run");
    assert.equal(Object.values(inherited).filter(value => value !== "1").some(value => `${child.stdout}${child.stderr}`.includes(value)), false, "child output must not expose inherited fixture values");
  }
});

test("real SDK pin and provider settings; config option alone derives proxy and placeholder", async t => {
  const packageJSON = JSON.parse(await readFile(new URL("./package.json", import.meta.url)));
  assert.equal(packageJSON.dependencies["@opencode/plugin"], "2.2.6");
  const f = await fixture(t);
  const { hooks, provider, storage } = await harness(t, { config: f.config });
  assert.equal(provider.settings.transport, "websocket");
  assert.equal(provider.settings.retained, true);
  assert.equal("encoding" in provider.settings, false);
  assert.deepEqual(provider.settings.compaction, { type: "native" });
  const event = { headers: {}, sessionID: "synthetic-session" };
  await hooks.get("model.request")(event);
  assert.equal(event.baseURL, provider.settings.baseURL);
  secretEqual(event.headers.authorization, provider.headers.authorization);
  secretEqual(event.headers["x-subswapper-bridge"], provider.headers["x-subswapper-bridge"]);
  secretEqual(provider.headers["x-subswapper-bridge"], provider.settings.apiKey);
  assert.equal(/^Bearer [0-9a-f]{64}$/.test(event.headers.authorization), true, "request must use a bridge capability");
  const provenance = storage.get("catalog-discovery-v1");
  assert.equal(provenance.openai.source, "backend");
  assert.equal(provenance.openai.textFallback, false);
  assert.deepEqual(provenance.openai.wireModelIDs, ["gpt-synthetic"]);
  assert.equal(provenance.claude.source, "unavailable");
  assert.equal(JSON.stringify(provenance).includes(placeholder.token), false);
  assert.equal(JSON.stringify(provenance).includes(provider.settings.apiKey), false);
  assert.equal(hooks.has("title"), false, "production plugin must not replace user titles");
  const retry = { decision: { retry: true, delay: 5 } };
  hooks.get("retry")(retry);
  assert.deepEqual(retry.decision, { retry: false });
});

test("credential assertion failures never include compared values", () => {
  const values = ["synthetic-private-actual", "synthetic-private-expected"];
  let failure;
  try { secretEqual(...values); } catch (error) { failure = error; }
  assert.equal(failure instanceof assert.AssertionError, true, "credential mismatch must fail");
  assert.equal(values.some(value => String(failure).includes(value)), false, "credential failure message must not expose compared values");
  assert.equal(typeof failure.actual === "boolean" && typeof failure.expected === "boolean", true, "credential assertion diagnostics must contain only booleans");
});

test("Claude cache fallback accepts the 24-hour boundary but rejects stale and future snapshots", async t => {
  const now = 1800000000000;
  const maxAge = 24 * 60 * 60 * 1000;
  const native = [{ ...Model.Info.default("claude-agent", "claude-opus-synthetic"), name: "Native Opus", time: { released: 123 }, limit: { context: 200000, output: 64000 } }];
  for (const [name, age, accepted] of [["fresh", 0, true], ["exactly 24 hours", maxAge, true], ["stale", maxAge + 1, false], ["future", -1, false]]) {
    await t.test(name, async t => {
      t.mock.method(Date, "now", () => now);
      const f = await fixture(t);
      const { models, storage, claudeProvider } = await harness(t, { config: f.config }, [], {
        claudeCatalog: native,
        storageEntries: [["claude-models-v1", { fetchedAt: now - age, models: [{ value: "opus", resolvedModel: "claude-opus-synthetic", supportedEffortLevels: ["high"] }] }]],
      });
      const provenance = storage.get("catalog-discovery-v1").claude;
      assert.equal(provenance.source, accepted ? "cache" : "unavailable");
      assert.equal(provenance.fetchedAt, accepted ? now - age : 0);
      assert.equal(provenance.ageMs, accepted ? age : 0);
      assert.equal(provenance.maxCacheAgeMs, maxAge);
      if (accepted) {
        assert.equal(claudeProvider.activation, "enabled");
        const inventory = models.get("claude-agent");
        assert.deepEqual(inventory.map(model => [model.id, model.modelID, model.name]), [["opus", "opus", "Native Opus (opus)"], ["claude-opus-synthetic", "claude-opus-synthetic", "Native Opus"]]);
        for (const model of inventory) {
          assert.deepEqual(model.time, native[0].time);
          assert.deepEqual(model.limit, native[0].limit);
          assert.deepEqual(model.variants, [{ id: "high", settings: { effort: "high" } }]);
        }
      } else {
        assert.equal(claudeProvider.activation, "disabled");
        assert.deepEqual(models.get("claude-agent"), native);
        assert.deepEqual(provenance.models, []);
      }
    });
  }
});

test("final model transform removes unavailable text and overrides public routing and pro settings", async t => {
  const f = await fixture(t);
  const source = Model.Info.default("openai", "gpt-synthetic");
  const overrides = [
    { ...source, id: "custom", settings: { baseURL: "https://api.openai.com/v1", apiKey: "synthetic-private-key", encoding: "gzip" }, headers: { Authorization: "Bearer synthetic-private-key", "ChatGPT-Account-ID": "not-selected", "X-Subswapper-Bridge": "synthetic-private-key" }, variants: [{ id: "pro", body: { reasoning: { mode: "pro" } } }], body: { reasoning: { mode: "pro" } } },
    { ...source, id: "o3-pro", modelID: "o3-pro" },
  ];
  const { models, provider } = await harness(t, { config: f.config }, overrides);
  const inventory = models.get("openai");
  assert.equal(inventory.some(model => model.id === "o3-pro"), false);
  const custom = inventory.find(model => model.id === "custom");
  assert.equal(custom.settings.baseURL, provider.settings.baseURL);
  secretEqual(custom.settings.apiKey, provider.settings.apiKey);
  assert.equal(custom.settings.encoding, undefined);
  assert.deepEqual(custom.variants, [{ id: "high", settings: { reasoningEffort: "high" } }]);
  assert.equal(custom.body.reasoning.mode, undefined);
  assert.deepEqual(Object.keys(custom.headers), ["authorization", "x-subswapper-bridge"]);
  secretEqual(custom.headers.authorization, provider.headers.authorization);
  secretEqual(custom.headers["x-subswapper-bridge"], provider.headers["x-subswapper-bridge"]);
  assert.deepEqual(custom.cost, [{ input: 0, output: 0, cache: { read: 0, write: 0 } }]);
});

test("session HTTP guard accepts the root-based bridge URL used by native model requests", async t => {
  const f = await fixture(t);
  const { hooks, provider } = await harness(t, { config: f.config });
  const event = { request: request(provider.settings.baseURL + "/responses") };
  await hooks.get("http.request")(event);
  assert.equal((await nativeDispatch(event.request)).body, "synthetic response");
  const handshake = { url: provider.settings.baseURL.replace("http:", "ws:") + "/responses", headers: {} };
  await hooks.get("experimental.ws.handshake")(handshake);
  assert.equal(handshake.url, provider.settings.baseURL.replace("http:", "ws:") + "/responses");
});

test("WebSocket handshake carries only bridge capability and streams duplex bytes", async t => {
  const f = await fixture(t);
  // A synthetic HTTP upgrade exercises the same Node transport as real WS
  // without introducing a WebSocket implementation dependency.
  const sockets = new Set();
  const upstream = createServer((_, response) => response.end('{"models":[]}'));
  upstream.on("upgrade", (r, socket, head) => {
    sockets.add(socket); socket.on("close", () => sockets.delete(socket));
    secretEqual(r.headers.authorization, `Bearer ${placeholder.token}`);
    secretEqual(r.headers["chatgpt-account-id"], placeholder.account_id);
    assert.equal(r.headers["x-subswapper-bridge"] === undefined, true, "WebSocket upstream must not receive the bridge capability header");
    assert.equal(r.url, "/backend-api/codex/responses?fixture=1");
    socket.write("HTTP/1.1 101 Switching Protocols\r\nConnection: Upgrade\r\nUpgrade: websocket\r\n\r\n");
    if (head.length) socket.write(head);
    socket.pipe(socket);
  });
  await new Promise(resolve => upstream.listen(0, "127.0.0.1", resolve));
  t.after(() => new Promise(resolve => { for (const socket of sockets) socket.destroy(); upstream.close(resolve); }));
  const baseURL = `http://127.0.0.1:${upstream.address().port}/backend-api/codex`;
  await writeFile(f.config, JSON.stringify({ state_path: join(f.directory, "state.json"), services: [{ name: "codex", proxy_listen: `127.0.0.1:${upstream.address().port}` }] }));
  const { hooks } = await harness(t, { config: f.config });
  const event = { url: baseURL.replace("http:", "ws:") + "/responses?fixture=1", headers: { authorization: "Bearer dummy", "chatgpt-account-id": "not-selected" } };
  await hooks.get("experimental.ws.handshake")(event);
  assert.equal(event.headers.authorization !== `Bearer ${placeholder.token}`, true, "handshake must not expose the placeholder");
  assert.equal(event.headers["chatgpt-account-id"] === undefined, true, "handshake must not expose an account header");
  const connection = httpRequest(event.url.replace("ws:", "http:"), { headers: { ...event.headers, connection: "Upgrade", upgrade: "websocket" } });
  const connected = new Promise((resolve, reject) => { connection.on("upgrade", (_, socket) => resolve(socket)); connection.on("response", r => reject(new Error(`upgrade rejected: ${r.statusCode}`))); connection.on("error", reject); });
  connection.end();
  const socket = await connected;
  t.after(() => socket.destroy());
  const echoed = new Promise((resolve, reject) => { socket.once("data", data => resolve(data.toString())); socket.once("error", reject); });
  socket.write("synthetic frame bytes");
  assert.equal(await echoed, "synthetic frame bytes");
  socket.destroy();
});

test("native HTTP dispatch preserves request bytes, headers and response stream without exposing placeholder", async t => {
  const raw = '\n{ "instructions": "keep", "stream": true, "tools": [], "max_output_tokens":123 }\n';
  const f = await fixture(t, async (r, w) => {
    assert.equal(r.url, "/backend-api/codex/responses");
    secretEqual(r.headers.authorization, `Bearer ${placeholder.token}`);
    secretEqual(r.headers["chatgpt-account-id"], placeholder.account_id);
    assert.equal(r.headers["x-keep"] === "yes", true, "custom header must survive dispatch");
    let body = ""; for await (const chunk of r) body += chunk;
    assert.equal(body, raw);
    w.setHeader("content-type", "text/event-stream");
    w.write("data: first\n\n");
    w.end("data: second\n\n");
  });
  const { hooks } = await harness(t, { config: f.config });
  const event = { request: request(f.endpoint, raw) };
  event.request.headers.set("content-length", String(Buffer.byteLength(raw)));
  await hooks.get("http.request")(event);
  assert.equal(event.request.headers.get("authorization") !== `Bearer ${placeholder.token}`, true, "native request must not expose the placeholder");
  assert.equal(event.request.headers.has("chatgpt-account-id"), false);
  const response = await nativeDispatch(event.request);
  assert.equal(response.status, 200);
  assert.equal(response.body, "data: first\n\ndata: second\n\n");
});

test("redirects cannot leak placeholder even after native conversion loses redirect policy", async t => {
  let leaked = 0;
  const sink = await server(t, (_, w) => { leaked++; w.end("leaked"); });
  const f = await fixture(t, (_, w) => { w.writeHead(307, { location: sink + "/steal" }); w.end(); });
  const { hooks } = await harness(t, { config: f.config });
  const event = { request: request(f.endpoint) };
  await hooks.get("http.request")(event);
  const response = await nativeDispatch(event.request);
  assert.equal(response.status, 502);
  assert.equal(leaked, 0);
  assert.equal(response.body.includes(placeholder.token), false);
  assert.equal(response.headers.location === undefined, true, "redirect location must be removed");
});

test("placeholder forwarding does not use an ambient global HTTP proxy agent", async t => {
  const f = await fixture(t);
  const { hooks } = await harness(t, { config: f.config });
  const original = globalAgent.addRequest;
  t.after(() => { globalAgent.addRequest = original; });
  globalAgent.addRequest = () => { throw new Error("ambient agent must not receive placeholder"); };
  const event = { request: request(f.endpoint) };
  await hooks.get("http.request")(event);
  assert.equal((await nativeDispatch(event.request)).status, 200);
});

test("native compact, discovery, uploads and media HTTP preserve paths, methods and bytes", async t => {
  const calls = [];
  const f = await fixture(t, async (r, w) => {
    let body = ""; for await (const chunk of r) body += chunk;
    calls.push([r.method, r.url, body, r.headers["content-type"]]);
    secretEqual(r.headers.authorization, `Bearer ${placeholder.token}`);
    w.end("ok");
  });
  const { hooks } = await harness(t, { config: f.config });
  for (const [path, method, body, type] of [
    ["/responses/compact", "POST", "{}", "application/json"],
    ["/models?client_version=synthetic", "GET", undefined, undefined],
    ["/files", "POST", "--synthetic multipart bytes--", "multipart/form-data; boundary=synthetic"],
    ["/files/synthetic/content", "GET", undefined, undefined],
  ]) {
    const event = { request: new Request(f.baseURL + path, { method, body, headers: type ? { "content-type": type } : {} }) };
    await hooks.get("http.request")(event);
    assert.equal((await nativeDispatch(event.request)).body, "ok");
    assert.deepEqual(calls.at(-1), [method, "/backend-api/codex" + path, body ?? "", type]);
  }
});

test("real native SDK images and decisions route despite integration API-key replacement and without session hooks", async t => {
  const calls = [];
  const f = await fixture(t, async (r, w) => {
    let body = ""; for await (const chunk of r) body += chunk;
    calls.push({ path: r.url, body, type: r.headers["content-type"] });
    secretEqual(r.headers.authorization, `Bearer ${placeholder.token}`);
    secretEqual(r.headers["chatgpt-account-id"], placeholder.account_id);
    w.writeHead(400, { "content-type": "application/json" });
    w.end('{"error":{"message":"synthetic backend rejection","type":"invalid_request_error"}}');
  });
  const { provider } = await harness(t, { config: f.config });
  // Native ModelResolver replaces settings.apiKey with the integration key.
  // Stateless requests have no session hook to repair Authorization afterward.
  const settings = { ...provider.settings, headers: provider.headers, apiKey: "subswapper-dummy" };
  const ai = makeAI();
  t.after(() => ai.dispose());
  await rejectsSafely(ai.image.generate({ model: OpenAI.imageModel("gpt-image-synthetic", settings), prompt: "synthetic test only" }), error => /synthetic backend rejection/.test(error.message));
  assert.equal(calls.at(-1).path, "/backend-api/codex/images/generations");
  assert.equal(JSON.parse(calls.at(-1).body).model, "gpt-image-synthetic");
  await rejectsSafely(ai.image.generate({ model: OpenAI.imageModel("gpt-image-synthetic", settings), prompt: "synthetic edit", images: [Media.bytes(Buffer.from("synthetic-image"), "image/png")], n: 1 }), error => /synthetic backend rejection/.test(error.message));
  assert.equal(calls.at(-1).path, "/backend-api/codex/images/edits");
  assert.equal(calls.at(-1).type, "application/json");
  assert.deepEqual(JSON.parse(calls.at(-1).body).images, [{ image_url: "data:image/png;base64,c3ludGhldGljLWltYWdl" }]);
  assert.equal(JSON.parse(calls.at(-1).body).n, 1);
  await rejectsSafely(ai.decision.generate({ model: OpenAI.decisionModel("gpt-decision-synthetic", settings), input: { fixture: true }, questions: [{ name: "fixture", type: "predicate", instructions: "synthetic" }] }), error => /synthetic backend rejection/.test(error.message));
  assert.equal(calls.at(-1).path, "/backend-api/codex/decisions");
});

test("native image results reject remote media URLs before artifact download", async t => {
  let downloads = 0;
  const sink = await server(t, (_, response) => { downloads++; response.end("must not download"); });
  const f = await fixture(t, (_, response) => { response.setHeader("content-type", "application/json"); response.end(JSON.stringify({ data: [{ url: sink + "/synthetic-image" }] })); });
  const { provider } = await harness(t, { config: f.config });
  const ai = makeAI(); t.after(() => ai.dispose());
  await rejectsSafely(ai.image.generate({ model: OpenAI.imageModel("gpt-image-synthetic", { ...provider.settings, headers: provider.headers }), prompt: "synthetic" }), error => /remote image URLs are unsupported/.test(error.message));
  assert.equal(downloads, 0);
});

for (const inline of [false, true]) {
  test(`native image SSE ${inline ? "materializes inline bytes and ignores accompanying URLs" : "rejects URL-only frames before artifact download"}`, { timeout: 20000 }, async t => {
    const png = Buffer.from("iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAQAAAC1HAwCAAAAC0lEQVR42mP8/x8AAwMCAO+aC14AAAAASUVORK5CYII=", "base64");
    for (const family of ["image_generation", "image_edit"]) {
      for (const frameType of ["completed", "partial_image"]) {
        await t.test(`${family}.${frameType}`, async t => {
          let downloads = 0, calls = 0;
          const sink = await server(t, (_, response) => { downloads++; response.end("must not download"); });
          const frame = type => ({ type: `${family}.${type}`, url: sink + "/synthetic-image", output_format: "png", ...(type === "partial_image" ? { partial_image_index: 0 } : {}), ...(inline ? { b64_json: png.toString("base64") } : {}) });
          const f = await fixture(t, async (r, response) => {
            calls++;
            secretEqual(r.headers.authorization, `Bearer ${placeholder.token}`);
            assert.equal(r.url === `/backend-api/codex/images/${family === "image_edit" ? "edits" : "generations"}`, true, "native stream must use the configured image route");
            let body = ""; for await (const chunk of r) body += chunk;
            const payload = JSON.parse(body);
            assert.equal(payload.stream === true && payload.partial_images === 2, true, "native image stream request fields must survive the bridge");
            if (family === "image_edit") assert.equal(payload.images?.[0]?.image_url === `data:image/png;base64,${png.toString("base64")}`, true, "streaming edit must preserve inline input bytes");
            response.setHeader("content-type", "text/event-stream");
            response.write(`event: ${family}.${frameType}\ndata: ${JSON.stringify(frame(frameType))}\n\n`);
            if (inline && frameType === "partial_image") response.write(`data: ${JSON.stringify(frame("completed"))}\n\n`);
            response.end();
          });
          const { provider } = await harness(t, { config: f.config });
          const ai = makeAI(); t.after(() => ai.dispose());
          let images = 0;
          const types = [];
          const consume = async () => {
            for await (const event of ai.image.stream({ model: OpenAI.imageModel("gpt-image-synthetic", { ...provider.settings, headers: provider.headers, apiKey: "subswapper-dummy" }), prompt: "synthetic stream only", ...(family === "image_edit" ? { images: [Media.bytes(png, "image/png")] } : {}) }, { signal: AbortSignal.timeout(5000) })) {
              types.push(event.type);
              if (!event.image) continue;
              images++;
              // Materialize as an artifact consumer would. A URL asset would
              // reach the sink here even if generation itself did not fetch it.
              const asset = await ai.materialize(event.image);
              assert.equal(asset.source.type === "bytes", true, "stream image must materialize as inline bytes");
              assert.equal(Buffer.from(await ai.bytes(asset)).equals(png), true, "materialized image must match inline fixture bytes");
            }
          };
          if (inline) {
            let completed = false;
            try { await consume(); completed = true; } catch { /* Never print SDK errors that may retain request headers. */ }
            assert.equal(completed, true, "native inline image stream must complete safely");
            assert.deepEqual(types, frameType === "partial_image" ? ["image-partial", "image", "finish"] : ["image", "finish"]);
            assert.equal(images, frameType === "partial_image" ? 2 : 1);
          } else {
            await rejectsSafely(consume(), error => /invalid stream event/.test(error.message));
            assert.equal(images, 0, "URL-only frame must fail before publishing an image asset");
          }
          assert.equal(calls, 1, "native image stream must dispatch once through the proxy");
          assert.equal(downloads, 0, "native stream and artifact materialization must never fetch the result URL");
        });
      }
    }
  });
}

test("guard rejects alternate destinations and malformed routes before reading credentials", async t => {
  const f = await fixture(t);
  const { hooks } = await harness(t, { config: f.config });
  await rm(f.path);
  for (const url of ["https://chatgpt.com/backend-api/codex/responses", f.endpoint+"#private", f.baseURL+"/%72esponses", f.baseURL+"/../admin", f.baseURL+"//responses", f.endpoint.replace("127.0.0.1", "localhost")]) {
    await rejectsSafely(hooks.get("http.request")({ request: request(url) }), error => /exact.*endpoint/.test(error.message));
  }
  for (const r of [new Request(request(f.endpoint), { headers: { "content-type": "application/json", upgrade: "websocket" } })]) {
    await rejectsSafely(hooks.get("http.request")({ request: r }), error => /HTTP/.test(error.message));
  }
});

test("private placeholder reloaded each request; unsafe leaf and ancestors fail secret-free", async t => {
  let expected = placeholder.token;
  const f = await fixture(t, (r, w) => { secretEqual(r.headers.authorization, `Bearer ${expected}`); w.end("ok"); });
  const { hooks } = await harness(t, { config: f.config });
  async function dispatch() { const event = { request: request(f.endpoint) }; await hooks.get("http.request")(event); return nativeDispatch(event.request); }
  assert.equal((await dispatch()).status, 200);
  expected = "rotated.synthetic.signature";
  await writeFile(f.path, JSON.stringify({ ...placeholder, token: expected }));
  assert.equal((await dispatch()).status, 200);
  async function rejected() {
    const response = await dispatch();
    assert.equal(response.status, 502);
    assert.match(response.body, /placeholder/);
    assert.equal(response.body.includes(f.path), false);
    assert.equal(response.body.includes(expected), false);
  }
  await chmod(f.path, 0o644); await rejected(); await chmod(f.path, 0o600);
  for (const value of ["null", "[]", "{}", "synthetic-private-json", JSON.stringify({ ...placeholder, account_id: "upstream-account" }), JSON.stringify({ ...placeholder, token: "invalid\r\nheader" }), "x".repeat(16385)]) {
    await writeFile(f.path, value); await rejected();
  }
  await writeFile(f.path, JSON.stringify(placeholder)); expected = placeholder.token;
  const leafTarget = join(f.directory, "leaf-target");
  await writeFile(leafTarget, JSON.stringify(placeholder), { mode: 0o600 });
  await rm(f.path); await symlink(leafTarget, f.path); await rejected();
  await rm(f.path); await writeFile(f.path, JSON.stringify(placeholder), { mode: 0o600 });
  for (const mode of [0o777, 0o702]) { await chmod(join(f.directory, "tokens"), mode); await rejected(); }
  await chmod(join(f.directory, "tokens"), 0o700);
  const target = join(f.directory, "target");
  await mkdir(target, { mode: 0o700 });
  await rm(join(f.directory, "tokens", "codex"), { recursive: true });
  await symlink(target, join(f.directory, "tokens", "codex"));
  await rejected();
});

test("bridge rejects unauthenticated callers and preserves upstream errors in full", async t => {
  const body = "private response ".repeat(90000);
  const f = await fixture(t, (_, w) => { w.writeHead(429, { "retry-after": "5" }); w.end(body); });
  const { hooks, provider } = await harness(t, { config: f.config });
  const event = { request: request(f.endpoint) };
  await hooks.get("http.request")(event);
  const unauthorized = await fetch(event.request.url, { method: "POST", body: "{}" });
  assert.equal(unauthorized.status, 401);
  assert.equal(unauthorized.headers.get("x-should-retry") === "false", true, "bridge errors must disable native retries");
  for (const authorization of ["Bearer subswapper-dummy", provider.headers.authorization]) {
    const bearerOnly = await fetch(event.request.url, { method: "POST", body: "{}", headers: { authorization } });
    assert.equal(bearerOnly.status, 401, "Bearer authentication alone must never grant bridge access");
  }
  const wrongCapability = await fetch(event.request.url, { method: "POST", body: "{}", headers: { authorization: provider.headers.authorization, "x-subswapper-bridge": "synthetic-wrong-capability" } });
  assert.equal(wrongCapability.status, 401, "wrong bridge capability must be rejected");
  const response = await nativeDispatch(event.request);
  assert.equal(response.status, 429);
  assert.equal(response.body, body);
  assert.equal(response.headers["retry-after"] === "5", true, "retry-after must survive dispatch");
});

test("bridge overrides native retry hints while preserving upstream 429 and 5xx replies", async t => {
  let status;
  const body = "synthetic upstream failure body";
  const f = await fixture(t, (_, response) => {
    response.writeHead(status, { "x-should-retry": "true" });
    response.end(body);
  });
  const { hooks } = await harness(t, { config: f.config });
  for (status of [429, 500, 503]) {
    const event = { request: request(f.endpoint) };
    await hooks.get("http.request")(event);
    const response = await nativeDispatch(event.request);
    assert.equal(response.status, status);
    assert.equal(response.body === body, true, "backend error body must remain unchanged");
    assert.equal(response.headers["x-should-retry"] === "false", true, "proxy must own retries even when the backend requests a native retry");
  }
});

test("owner-private group writable ancestor permits native dispatch", async t => {
  if (!await isPrivateGroup(process.getuid(), process.getgid())) {
    t.skip("current primary group is not verifiably private through supported Linux NSS");
    return;
  }
  const f = await fixture(t);
  const { hooks } = await harness(t, { config: f.config });
  await chmod(join(f.directory, "tokens"), 0o775);
  const event = { request: request(f.endpoint) };
  await hooks.get("http.request")(event);
  assert.equal((await nativeDispatch(event.request)).status, 200);
});

test("private group requires exclusive primary ownership and empty explicit membership", () => {
  const owner = "owner:x:1000:1000::/synthetic/owner:/bin/sh\n";
  const group = "owner:x:1000:\n";
  assert.equal(privateGroupRecords(1000, 1000, owner, group), true);
  for (const [name, passwd, groups, gid = 1000] of [
    ["wrong primary gid", owner.replace(":1000::", ":1001::"), group],
    ["different directory gid", owner, group, 1001],
    ["explicit member", owner, "owner:x:1000:someone\n"],
    ["explicit owner member", owner, "owner:x:1000:owner\n"],
    ["other primary user", owner + "other:x:1001:1000::/synthetic/other:/bin/sh\n", group],
    ["duplicate owner", owner + owner, group],
    ["missing owner", "other:x:1001:1001::/synthetic/other:/bin/sh\n", group],
    ["duplicate group", owner, group + group],
    ["gid alias with members", owner, group + "alias:x:1000:someone\n"],
    ["malformed passwd", owner + "malformed\n", group],
    ["malformed gid", owner, "owner:x:not-a-gid:\n"],
    ["whitespace member", owner, "owner:x:1000: \n"],
  ]) assert.equal(privateGroupRecords(1000, gid, passwd, groups), false, name);
  assert.equal(privateGroupRecords(1000, 1000, owner + "other:x:1001:1001::/synthetic/other:/bin/sh\n", group), true);
  assert.equal(privateGroupRecords(1000, 1000, owner, group + "unrelated:x:1001:someone\n"), true);
});

test("private group NSS verification fails closed for missing or non-enumerable sources", () => {
  assert.equal(supportsPrivateGroupNSS("passwd: files systemd\ngroup: files systemd\n"), true);
  assert.equal(supportsPrivateGroupNSS("passwd: files\ngroup: files # comment\n"), true);
  assert.equal(supportsPrivateGroupNSS("passwd: files\ngroup: files\ninitgroups: files systemd\n"), true);
  for (const config of ["", "passwd: files\n", "passwd: ldap files\ngroup: files\n",
    "passwd: files\ngroup: compat\n", "passwd: files [SUCCESS=return]\ngroup: files\n",
    "passwd: systemd\ngroup: files\n", "passwd: files\npasswd: files\ngroup: files\n",
    "passwd: files\ngroup: files\ninitgroups: ldap\n"]) {
    assert.equal(supportsPrivateGroupNSS(config), false, config);
  }
});

test("non-private groups and failed identity queries block dispatch deterministically", async t => {
  let upstreamCalls = 0;
  const f = await fixture(t, (_, w) => { upstreamCalls++; w.end("unexpected"); });
  const { hooks } = await harness(t, { config: f.config });
  await chmod(join(f.directory, "tokens"), 0o775);
  const uid = process.getuid(), gid = process.getgid();
  const owner = `owner:x:${uid}:${gid}::/synthetic:/bin/sh\n`;
  let scenario;
  const mock = t.mock.method(childProcess, "execFile", (executable, args, options, callback) => {
    assert.equal(executable, "/usr/bin/getent");
    assert.equal(options.timeout, 2000);
    assert.equal(options.maxBuffer, 1024 * 1024);
    queueMicrotask(() => {
      if (["failure", "timeout", "oversize"].includes(scenario)) {
        callback(Object.assign(new Error("synthetic identity failure"), { code: scenario }), "", "");
      } else {
        const passwd = scenario === "other primary user" ? owner + `other:x:${uid + 1}:${gid}::/synthetic:/bin/sh\n` : owner;
        const group = `owner:x:${gid}:${scenario === "explicit member" ? "other" : ""}\n`;
        callback(null, args[0] === "passwd" ? passwd : group, "");
      }
    });
  });
  for (scenario of ["explicit member", "other primary user", "failure", "timeout", "oversize"]) {
    const event = { request: request(f.endpoint) };
    await hooks.get("http.request")(event);
    const response = await nativeDispatch(event.request);
    assert.equal(response.status, 502, scenario);
    assert.match(response.body, /placeholder/);
    assert.equal(response.body.includes(placeholder.token), false);
  }
  // With identical filesystem modes, an exclusive owner-primary group works
  // regardless of the test host's actual account database.
  scenario = "private";
  const event = { request: request(f.endpoint) };
  await hooks.get("http.request")(event);
  assert.equal((await nativeDispatch(event.request)).status, 200);
  assert.equal(upstreamCalls, 1);
  assert.ok(mock.mock.callCount() >= 12);
});

test("cancellation reaches streaming proxy and concurrent calls remain isolated", { timeout: 10000 }, async t => {
  let cancelled;
  const closed = new Promise(resolve => { cancelled = resolve; });
  const f = await fixture(t, async (r, w) => {
    let body = ""; for await (const chunk of r) body += chunk;
    if (body === '"cancel"') { w.write("first"); w.on("close", cancelled); return; }
    w.end(body);
  });
  const { hooks } = await harness(t, { config: f.config });
  await Promise.all(Array.from({ length: 8 }, async (_, i) => {
    const event = { request: request(f.endpoint, JSON.stringify(i)) };
    await hooks.get("http.request")(event);
    assert.equal((await nativeDispatch(event.request)).body, JSON.stringify(i));
  }));
  const controller = new AbortController();
  const event = { request: request(f.endpoint, '"cancel"', controller.signal) };
  await hooks.get("http.request")(event);
  const response = await fetch(event.request);
  const reader = response.body.getReader();
  await reader.read(); controller.abort();
  await rejectsSafely(reader.read());
  await closed;
});

test("explicit environment route and placeholder override discovery without reading any config", async t => {
  const f = await fixture(t);
  const keys = ["SUBSWAPPER_CODEX_PROXY_URL", "SUBSWAPPER_CODEX_PLACEHOLDER"];
  const previous = keys.map(key => process.env[key]);
  t.after(() => keys.forEach((key, i) => { if (previous[i] === undefined) delete process.env[key]; else process.env[key] = previous[i]; }));
  process.env.SUBSWAPPER_CODEX_PROXY_URL = f.baseURL.replace("/backend-api/codex", "");
  process.env.SUBSWAPPER_CODEX_PLACEHOLDER = f.path;
  const { hooks } = await harness(t, { config: "/does-not-exist" });
  const event = { request: request(f.endpoint) };
  await hooks.get("http.request")(event);
  assert.equal((await nativeDispatch(event.request)).body, "synthetic response");
});

test("configuration rejects non-loopback targets, missing/disabled/ambiguous service without secrets", async t => {
  const f = await fixture(t);
  for (const services of [[], [{ name: "codex", disabled: true, proxy_listen: "127.0.0.1:7879" }], [{ name: "codex", proxy_listen: "example.com:7879" }], [{ name: "codex", proxy_listen: "127.0.0.1:7879/private?token=secret" }]]) {
    await writeFile(f.config, JSON.stringify({ services }));
    await rejectsSafely(harness(t, { config: f.config }), error => !error.message.includes("secret"));
  }
});
