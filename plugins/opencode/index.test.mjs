import assert from "node:assert/strict";
import childProcess from "node:child_process";
import { chmod, mkdir, mkdtemp, readFile, rm, symlink, writeFile } from "node:fs/promises";
import { createServer, globalAgent } from "node:http";
import { tmpdir } from "node:os";
import { join } from "node:path";
import test from "node:test";
import { Effect } from "effect";
import { FetchHttpClient, HttpClient, HttpClientRequest } from "effect/http";
import plugin from "./index.ts";
import { isPrivateGroup, privateGroupRecords, supportsPrivateGroupNSS } from "./private-group.ts";

const placeholder = { token: "synthetic.fixture.signature", account_id: "subswapper-proxy" };
async function server(t, handler) {
  const server = createServer(handler);
  await new Promise(resolve => server.listen(0, "127.0.0.1", resolve));
  t.after(() => new Promise(resolve => { server.closeAllConnections(); server.close(resolve); }));
  return `http://127.0.0.1:${server.address().port}`;
}
async function fixture(t, handler = (_, response) => response.end("synthetic response")) {
  const directory = await mkdtemp(join(tmpdir(), "subswapper-opencode-test-"));
  await chmod(directory, 0o700);
  t.after(() => rm(directory, { recursive: true, force: true }));
  const proxy = await server(t, handler);
  const path = join(directory, "tokens", "codex", "proxy-placeholder.json");
  await mkdir(join(directory, "tokens", "codex"), { recursive: true, mode: 0o700 });
  await writeFile(path, JSON.stringify(placeholder), { mode: 0o600 });
  const config = join(directory, "config.json");
  await writeFile(config, JSON.stringify({ state_path: join(directory, "state.json"), services: [
    { name: "codex", kind: "codex", proxy_listen: proxy.slice(7) },
  ] }), { mode: 0o600 });
  return { directory, path, config, baseURL: `${proxy}/backend-api/codex`, endpoint: `${proxy}/backend-api/codex/responses` };
}
async function harness(t, options) {
  const hooks = new Map();
  const provider = { settings: { transport: "websocket", encoding: "gzip", retained: true } };
  const cleanup = await plugin.setup({ options, provider: {
    async transform(callback) { callback({ update(id, update) { assert.equal(id, "openai"); update(provider); } }); },
  }, session: {
    async hook(name, callback, options) { assert.deepEqual(options, { providerID: "openai" }); hooks.set(name, callback); },
  } });
  if (cleanup) t.after(cleanup);
  return { hooks, provider };
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

test("real SDK pin and provider settings; configurable route from Subswapper config", async t => {
  const packageJSON = JSON.parse(await readFile(new URL("./package.json", import.meta.url)));
  assert.equal(packageJSON.dependencies["@opencode/plugin"], "2.2.6");
  const f = await fixture(t);
  const { hooks, provider } = await harness(t, { config: f.config });
  assert.deepEqual(provider.settings, { transport: "http", retained: true, compaction: { type: "summary" } });
  const event = { headers: {} };
  await hooks.get("model.request")(event);
  assert.equal(event.baseURL, f.baseURL);
  assert.deepEqual(event.headers, {});
  assert.equal(hooks.has("title"), false, "production plugin must not replace user titles");
  const retry = { decision: { retry: true, delay: 5 } };
  hooks.get("retry")(retry);
  assert.deepEqual(retry.decision, { retry: false });
  assert.throws(() => hooks.get("experimental.ws.handshake")({}), /HTTP only/);
});

test("native HTTP dispatch preserves request bytes, headers and response stream without exposing placeholder", async t => {
  const raw = '\n{ "instructions": "keep", "stream": true, "tools": [], "max_output_tokens":123 }\n';
  const f = await fixture(t, async (r, w) => {
    assert.equal(r.url, "/backend-api/codex/responses");
    assert.equal(r.headers.authorization, `Bearer ${placeholder.token}`);
    assert.equal(r.headers["chatgpt-account-id"], placeholder.account_id);
    assert.equal(r.headers["x-keep"], "yes");
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
  assert.notEqual(event.request.headers.get("authorization"), `Bearer ${placeholder.token}`);
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
  assert.equal(response.headers.location, undefined);
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

test("guard rejects alternate destinations and non-JSON POST before reading credentials", async t => {
  const f = await fixture(t);
  const { hooks } = await harness(t, { config: f.config });
  await rm(f.path);
  for (const url of ["https://chatgpt.com/backend-api/codex/responses", f.endpoint+"/", f.endpoint+"?x=private", f.endpoint+"#private", f.baseURL+"/responses/compact", f.baseURL+"/%72esponses", f.endpoint.replace("127.0.0.1", "localhost")]) {
    await assert.rejects(hooks.get("http.request")({ request: request(url) }), /exact.*endpoint/);
  }
  for (const r of [new Request(f.endpoint), new Request(f.endpoint, { method: "POST", body: "{}" }), new Request(request(f.endpoint), { headers: { "content-type": "application/json", upgrade: "websocket" } })]) {
    await assert.rejects(hooks.get("http.request")({ request: r }), /JSON POST/);
  }
});

test("private placeholder reloaded each request; unsafe leaf and ancestors fail secret-free", async t => {
  let expected = placeholder.token;
  const f = await fixture(t, (r, w) => { assert.equal(r.headers.authorization, `Bearer ${expected}`); w.end("ok"); });
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
  const { hooks } = await harness(t, { config: f.config });
  const event = { request: request(f.endpoint) };
  await hooks.get("http.request")(event);
  const unauthorized = await fetch(event.request.url, { method: "POST", body: "{}" });
  assert.equal(unauthorized.status, 401);
  const response = await nativeDispatch(event.request);
  assert.equal(response.status, 429);
  assert.equal(response.body, body);
  assert.equal(response.headers["retry-after"], "5");
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
  await assert.rejects(reader.read());
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
    await assert.rejects(harness(t, { config: f.config }), error => !error.message.includes("secret"));
  }
});
