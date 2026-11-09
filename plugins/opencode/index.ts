import { Plugin } from "@opencode/plugin";
import { constants } from "node:fs";
import { lstat, open } from "node:fs/promises";
import { createHash, randomBytes, timingSafeEqual } from "node:crypto";
import { Agent, createServer, request as httpRequest, type IncomingHttpHeaders } from "node:http";
import { homedir, platform } from "node:os";
import { isIP } from "node:net";
import type { Duplex } from "node:stream";
import { dirname, isAbsolute, join, parse, resolve } from "node:path";
import { pipeline } from "node:stream/promises";
import { isPrivateGroup } from "./private-group.ts";
import { claudeModels, discoverClaude, subscriptionModels } from "./catalog.ts";

const fail = (message: string) => new Error(`Subswapper: ${message}`);
const placeholderLimit = 16 * 1024;

function expand(path: string): string {
  return path.replace(/^~(?=\/|$)/, homedir()).replace(/\$(\w+)|\$\{([^}]+)\}/g, (_, a, b) => process.env[a ?? b] ?? "");
}
function defaultRoot(xdg: string | undefined, linux: string): string {
  if (xdg && isAbsolute(expand(xdg))) return join(expand(xdg), "subswapper");
  return join(homedir(), ...(platform() === "darwin" ? ["Library", "Application Support"] : linux.split("/")), "subswapper");
}
function safeName(name: string): string {
  let safe = name.replace(/[^a-zA-Z0-9._-]/gu, "_");
  if (!safe.replaceAll(".", "")) safe = "account";
  return safe === name ? safe : `${safe}-${createHash("sha256").update(name).digest("hex").slice(0, 12)}`;
}

// Group writes require the owner's exclusive primary group. Root-owned sticky
// temp roots are allowed: sticky mode protects our private child there.
async function validateAncestors(path: string): Promise<void> {
  const uid = process.getuid?.();
  if (uid === undefined || !isAbsolute(path) || resolve(path) !== path) throw new Error();
  const privateGroups = new Map<string, boolean>();
  for (let directory = dirname(path); ; directory = dirname(directory)) {
    const info = await lstat(directory);
    if (!info.isDirectory() || (info.uid !== uid && info.uid !== 0)) throw new Error();
    if (!(info.uid === 0 && (info.mode & 0o1000) !== 0)) {
      if ((info.mode & 0o002) !== 0) throw new Error();
      if ((info.mode & 0o020) !== 0) {
        const key = `${info.uid}:${info.gid}`;
        if (!privateGroups.has(key)) privateGroups.set(key, await isPrivateGroup(info.uid, info.gid));
        if (!privateGroups.get(key)) throw new Error();
      }
    }
    if (directory === parse(directory).root) break;
  }
}

async function readSafeJSON(path: string, privateFile: boolean): Promise<unknown> {
  try {
    await validateAncestors(path);
    if (privateFile && ((await lstat(dirname(path))).mode & 0o7777) !== 0o700) throw new Error();
    const before = await lstat(path);
    if (!before.isFile() || before.uid !== process.getuid?.() ||
        (privateFile ? (before.mode & 0o7777) !== 0o600 : (before.mode & 0o022) !== 0)) throw new Error();
    const file = await open(path, constants.O_RDONLY | constants.O_NOFOLLOW | constants.O_NONBLOCK);
    try {
      const info = await file.stat();
      if (info.dev !== before.dev || info.ino !== before.ino || info.mode !== before.mode || info.uid !== before.uid || info.gid !== before.gid) throw new Error();
      const limit = privateFile ? placeholderLimit : 1024 * 1024;
      if (info.size > limit) throw new Error();
      const buffer = Buffer.alloc(limit + 1);
      let size = 0;
      while (size < buffer.length) {
        const { bytesRead } = await file.read(buffer, size, buffer.length - size, size);
        if (!bytesRead) break;
        size += bytesRead;
      }
      if (size > limit) throw new Error();
      await validateAncestors(path);
      return JSON.parse(buffer.toString("utf8", 0, size));
    } finally { await file.close(); }
  } catch {
    throw fail(`unsafe, unreadable, or malformed ${privateFile ? "placeholder" : "config"}`);
  }
}

async function readPlaceholder(path: string): Promise<{ token: string; account_id: string }> {
  const value = await readSafeJSON(path, true) as Record<string, unknown> | null;
  if (!value || typeof value.token !== "string" || typeof value.account_id !== "string" ||
      !/^[A-Za-z0-9_-]+\.[A-Za-z0-9_-]+\.[A-Za-z0-9_-]+$/.test(value.token) || value.account_id !== "subswapper-proxy") {
    throw fail("unsafe, unreadable, or malformed placeholder");
  }
  return { token: value.token, account_id: value.account_id };
}

async function discover(options: Readonly<Record<string, unknown>>): Promise<{ baseURL: string; endpoint: string; placeholder: string }> {
  try {
    let origin = process.env.SUBSWAPPER_CODEX_PROXY_URL;
    let placeholder = process.env.SUBSWAPPER_CODEX_PLACEHOLDER;
    if (!origin || !placeholder) {
      const config = options.config ?? process.env.SUBSWAPPER_CONFIG ?? join(defaultRoot(process.env.XDG_CONFIG_HOME, ".config"), "config.json");
      if (typeof config !== "string") throw new Error();
      const cfg = await readSafeJSON(expand(config), false) as { state_path?: string; services?: Array<{ name: string; kind?: string; disabled?: boolean; proxy_listen?: string }> };
      const serviceName = options.service ?? "codex";
      const services = cfg.services?.filter(service => service.name === serviceName && (service.kind ?? service.name).toLowerCase() === "codex" && !service.disabled);
      if (services?.length !== 1) throw new Error();
      const service = services[0];
      origin ??= service.proxy_listen ? `http://${service.proxy_listen}` : undefined;
      const state = cfg.state_path ? expand(cfg.state_path) : join(defaultRoot(process.env.XDG_DATA_HOME, ".local/share"), "state.json");
      placeholder ??= join(dirname(state), "tokens", safeName(service.name), "proxy-placeholder.json");
    }
    if (!origin || !placeholder || !isAbsolute(placeholder) || resolve(placeholder) !== placeholder) throw new Error();
    const url = new URL(origin);
    // Numeric loopback only. No DNS, userinfo, path, query, or alternate scheme.
    if (url.protocol !== "http:" || !(url.hostname === "[::1]" || (isIP(url.hostname) === 4 && url.hostname.startsWith("127."))) || url.port === "0" ||
        url.username || url.password || url.search || url.hash || url.pathname !== "/" ||
        (origin !== url.origin && origin !== `${url.origin}/`)) throw new Error();
    const baseURL = `${url.origin}/backend-api/codex`;
    return { baseURL, endpoint: `${baseURL}/responses`, placeholder };
  } catch { throw fail("invalid Codex route configuration; configure a fixed loopback proxy or relay and placeholder path"); }
}

const bridgeHeader = "x-subswapper-bridge";
const hopHeaders = new Set(["connection", "keep-alive", "proxy-authenticate", "proxy-authorization", "te", "trailer", "transfer-encoding", "upgrade", "host", bridgeHeader]);
function cleanHeaders(headers: IncomingHttpHeaders): IncomingHttpHeaders {
  const blocked = new Set([...hopHeaders, ...(headers.connection?.split(",").map(value => value.trim().toLowerCase()) ?? [])]);
  return Object.fromEntries(Object.entries(headers).filter(([key]) => !blocked.has(key)));
}

function routePath(raw: string, baseURL: string): string {
  const url = new URL(raw);
  const base = new URL(baseURL);
  const prefix = base.pathname.replace(/\/$/, "");
  if (url.origin !== base.origin || url.username || url.password || url.hash ||
      !url.pathname.startsWith(prefix + "/")) throw fail("expected exact local Codex endpoint origin and path");
  const path = url.pathname.slice(prefix.length);
  if (!/^\/(?:[A-Za-z0-9._~-]+\/)*[A-Za-z0-9._~-]+$/.test(path) || path.split("/").some(part => part === "." || part === "..")) {
    throw fail("expected exact local Codex endpoint path");
  }
  return path + url.search;
}

// The native SDK selects JSON edits only for a literal chatgpt.com base URL.
// Our loopback URL would otherwise make it send multipart, which that backend
// rejects. Match its json-edits representation, without downloading media.
async function imageEditJSON(request: import("node:http").IncomingMessage): Promise<Buffer> {
  let size = 0;
  const chunks: Buffer[] = [];
  for await (const chunk of request) {
    size += chunk.length;
    if (size > 64 * 1024 * 1024) throw fail("image edit exceeds replay limit");
    chunks.push(chunk);
  }
  const form = await new Response(Buffer.concat(chunks), { headers: { "content-type": request.headers["content-type"]! } }).formData();
  const body: Record<string, unknown> = Object.create(null);
  const images: Array<{ image_url: string }> = [];
  for (const [key, value] of form.entries()) {
    if (typeof value !== "string") {
      if (!["image", "image[]", "mask"].includes(key) || !value.type.startsWith("image/")) throw fail("invalid image edit");
      const image = { image_url: `data:${value.type};base64,${Buffer.from(await value.arrayBuffer()).toString("base64")}` };
      if (key === "mask") {
        if (body.mask) throw fail("invalid image edit");
        body.mask = image;
      } else images.push(image);
    } else {
      if (["image", "image[]", "images", "mask"].includes(key) || Object.hasOwn(body, key)) throw fail("invalid image edit");
      body[key] = ["n", "output_compression", "partial_images", "stream"].includes(key) ? JSON.parse(value) : value;
    }
  }
  if (!images.length || typeof body.model !== "string" || typeof body.prompt !== "string") throw fail("invalid image edit");
  body.images = images;
  return Buffer.from(JSON.stringify(body));
}

// OpenCode 2.2.6 converts Web Request to Effect HttpClientRequest and loses
// redirect policy. Its native request must NEVER carry the proxy placeholder.
// A private per-plugin bridge owns credential attachment and Node HTTP (which
// does not follow redirects). No global fetch patch or new persistent service.
async function bridge(route: Awaited<ReturnType<typeof discover>>) {
  const capability = randomBytes(32).toString("hex");
  // Never inherit NODE_USE_ENV_PROXY/globalAgent routing for a shared secret.
  const agent = new Agent({ keepAlive: true, proxyEnv: {} });
  const controllers = new Set<AbortController>();
  const sockets = new Set<Duplex>();
  const server = createServer(async (request, response) => {
    const reject = (status: number, message: string) => { if (!response.headersSent) { response.writeHead(status, { "content-type": "text/plain", "x-should-retry": "false" }); response.end(`Subswapper: ${message}`); } else response.destroy(); };
    // Native stateless calls replace Authorization with their integration key.
    // Authenticate independently, then strip this private header upstream.
    const auth = Buffer.from(String(request.headers[bridgeHeader] ?? ""));
    const expected = Buffer.from(capability);
    if (auth.length !== expected.length || !timingSafeEqual(auth, expected)) { reject(401, "bridge authentication failed"); return; }
    let path: string;
    try { path = routePath(route.baseURL + request.url, route.baseURL); }
    catch { reject(400, "invalid HTTP route"); return; }
    if (request.headers.upgrade || !["GET", "HEAD", "POST", "PUT", "PATCH", "DELETE", "OPTIONS"].includes(request.method ?? "")) { reject(400, "expected HTTP request"); return; }
    const controller = new AbortController();
    controllers.add(controller);
    response.on("close", () => { if (!response.writableEnded) controller.abort(); });
    request.on("aborted", () => controller.abort());
    try {
      const edit = path.split("?", 1)[0] === "/images/edits" && request.headers["content-type"]?.split(";", 1)[0].trim().toLowerCase() === "multipart/form-data" ? await imageEditJSON(request) : undefined;
      const placeholder = await readPlaceholder(route.placeholder);
      if (controller.signal.aborted) return;
      const headers = cleanHeaders(request.headers);
      const imagePath = /^\/images\/(generations|edits)(?:\?|$)/.test(path);
      if (imagePath) delete headers["accept-encoding"];
      headers.authorization = `Bearer ${placeholder.token}`;
      headers["chatgpt-account-id"] = placeholder.account_id;
      if (edit) { headers["content-type"] = "application/json"; headers["content-length"] = String(edit.length); }
      const upstream = httpRequest(route.baseURL + path, { method: request.method, headers, signal: controller.signal, agent });
      const finished = new Promise<void>((resolve, rejectPromise) => {
        upstream.on("error", rejectPromise);
        upstream.on("response", backend => {
          if (backend.statusCode! >= 300 && backend.statusCode! < 400) {
            backend.destroy(); reject(502, "proxy redirects are blocked"); resolve(); return;
          }
          if (imagePath && backend.statusCode! >= 200 && backend.statusCode! < 300 && !backend.headers["content-type"]?.startsWith("text/event-stream")) {
            // The artifact layer downloads URL results outside provider hooks.
            // Require inline results rather than silently allowing that bypass.
            (async () => {
              let size = 0;
              const chunks: Buffer[] = [];
              for await (const chunk of backend) {
                size += chunk.length;
                if (size > 64 * 1024 * 1024) throw fail("image response exceeds limit");
                chunks.push(chunk);
              }
              const body = Buffer.concat(chunks);
              const value = JSON.parse(body.toString("utf8")) as { data?: Array<{ url?: unknown }> };
              if (value?.data?.some(item => item?.url !== undefined && item.url !== null)) {
                reject(502, "remote image URLs are unsupported; request inline image results"); return;
              }
              response.writeHead(backend.statusCode!, { ...cleanHeaders(backend.headers), "x-should-retry": "false" });
              response.end(body);
            })().then(resolve, rejectPromise);
            return;
          }
          // Native stateless generation retries independently of session hooks.
          // The selected-account proxy alone owns retries, including for 5xx.
          response.writeHead(backend.statusCode!, { ...cleanHeaders(backend.headers), "x-should-retry": "false" });
          pipeline(backend, response).then(resolve, rejectPromise);
        });
      });
      // Both directions stream with backpressure. Cancellation tears down both.
      if (edit) { upstream.end(edit); await finished; }
      else await Promise.all([pipeline(request, upstream), finished]);
    } catch (error) {
      if (!controller.signal.aborted) reject(502, error instanceof Error && error.message.includes("placeholder") ? "unsafe, unreadable, or malformed placeholder" : "proxy request failed");
    } finally { controller.abort(); controllers.delete(controller); }
  });
  server.on("upgrade", async (request, socket, head) => {
    const auth = Buffer.from(String(request.headers[bridgeHeader] ?? ""));
    const expected = Buffer.from(capability);
    if (auth.length !== expected.length || !timingSafeEqual(auth, expected) || request.method !== "GET" || request.headers.upgrade?.toLowerCase() !== "websocket") { socket.destroy(); return; }
    sockets.add(socket);
    socket.on("close", () => sockets.delete(socket));
    const controller = new AbortController();
    controllers.add(controller);
    const finish = () => { controller.abort(); controllers.delete(controller); socket.destroy(); };
    socket.once("close", finish);
    socket.once("error", finish);
    try {
      const path = routePath(route.baseURL + request.url, route.baseURL);
      const placeholder = await readPlaceholder(route.placeholder);
      if (controller.signal.aborted) return;
      const headers = cleanHeaders(request.headers);
      headers.authorization = `Bearer ${placeholder.token}`;
      headers["chatgpt-account-id"] = placeholder.account_id;
      headers.connection = "Upgrade";
      headers.upgrade = "websocket";
      const upstream = httpRequest(route.baseURL + path, { method: "GET", headers, signal: controller.signal, agent });
      upstream.once("error", finish);
      // Redirects and rejected upgrades never reach a second destination.
      upstream.once("response", response => { response.destroy(); finish(); });
      upstream.once("upgrade", (response, backend, backendHead) => {
        if (controller.signal.aborted || response.headers.upgrade?.toLowerCase() !== "websocket") { backend.destroy(); finish(); return; }
        sockets.add(backend);
        backend.once("close", () => { sockets.delete(backend); finish(); });
        backend.once("error", finish);
        socket.once("close", () => backend.destroy());
        const responseHeaders = cleanHeaders(response.headers);
        delete responseHeaders.authorization;
        delete responseHeaders["chatgpt-account-id"];
        responseHeaders["x-should-retry"] = "false";
        socket.write("HTTP/1.1 101 Switching Protocols\r\nConnection: Upgrade\r\nUpgrade: websocket\r\n");
        for (const [key, value] of Object.entries(responseHeaders)) {
          for (const item of Array.isArray(value) ? value : value === undefined ? [] : [value]) socket.write(`${key}: ${item}\r\n`);
        }
        socket.write("\r\n");
        if (backendHead.length) socket.write(backendHead);
        if (head.length) backend.write(head);
        socket.pipe(backend).pipe(socket);
      });
      upstream.end();
    } catch { finish(); }
  });
  await new Promise<void>((resolve, reject) => { server.once("error", reject); server.listen(0, "127.0.0.1", resolve); });
  server.unref();
  const address = server.address();
  if (!address || typeof address === "string") throw fail("bridge unavailable");
  const baseURL = `http://127.0.0.1:${address.port}`;
  return {
    baseURL, capability,
    inventory: () => new Promise<unknown>(resolve => {
      const request = httpRequest(baseURL + "/models?client_version=2.2.6", { headers: { [bridgeHeader]: capability }, agent, signal: AbortSignal.timeout(10000) });
      request.on("error", () => resolve(undefined));
      request.on("response", response => {
        let size = 0;
        const chunks: Buffer[] = [];
        response.on("error", () => resolve(undefined));
        response.on("data", (chunk: Buffer) => {
          size += chunk.length;
          if (size > 1024 * 1024) { response.destroy(); resolve(undefined); return; }
          chunks.push(chunk);
        });
        response.on("end", () => { try { resolve(response.statusCode === 200 ? JSON.parse(Buffer.concat(chunks).toString("utf8")) : undefined); } catch { resolve(undefined); } });
      });
      request.end();
    }),
    close: () => new Promise<void>(resolve => { for (const controller of controllers) controller.abort(); for (const socket of sockets) socket.destroy(); agent.destroy(); server.closeAllConnections(); server.close(() => resolve()); }),
  };
}

export default Plugin.define({
  id: "subswapper.codex",
  async setup(ctx) {
    const route = await discover(ctx.options);
    const transport = await bridge(route);
    try {
      const [inventory, discoveredClaude] = await Promise.all([transport.inventory(), discoverClaude(process.env.OPENCODE_CLAUDE_PATH || "claude").catch(() => [])]);
      const now = Date.now();
      const maxCacheAge = 24 * 60 * 60 * 1000;
      let claudeInventory: unknown[] = discoveredClaude;
      let claudeSource = discoveredClaude.length ? "runtime" : "unavailable";
      let claudeFetchedAt = discoveredClaude.length ? now : 0;
      if (claudeInventory.length) await ctx.storage.set("claude-models-v1", { fetchedAt: now, models: discoveredClaude });
      else {
        const cached = await ctx.storage.get("claude-models-v1") as { fetchedAt?: unknown; models?: unknown } | null;
        if (cached && typeof cached.fetchedAt === "number" && cached.fetchedAt <= now && now - cached.fetchedAt <= maxCacheAge && Array.isArray(cached.models)) {
          claudeInventory = cached.models; claudeFetchedAt = cached.fetchedAt; claudeSource = "cache";
        }
      }
      const allowed = new Map(subscriptionModels(inventory, []).map(model => [model.id, model]));
      await ctx.storage.set("catalog-discovery-v1", {
        checkedAt: now,
        openai: { source: inventory !== null && typeof inventory === "object" && "models" in inventory && Array.isArray(inventory.models) ? "backend" : "unavailable",
          fetchedAt: now, wireModelIDs: [...allowed.values()].filter(model => model.type !== "image").map(model => model.modelID), textFallback: false },
        claude: { source: claudeSource, fetchedAt: claudeFetchedAt, ageMs: claudeFetchedAt ? now - claudeFetchedAt : 0, maxCacheAgeMs: maxCacheAge,
          models: claudeModels(claudeInventory, []).map(model => ({ id: model.id, modelID: model.modelID, name: model.name })) },
      });
      await ctx.provider.transform(editor => {
        const openai = editor.get("openai");
        if (!openai) return;
        editor.update("openai", provider => {
          provider.activation = "enabled";
          provider.settings = { ...provider.settings, baseURL: transport.baseURL, apiKey: transport.capability, compaction: { type: "native" } };
          provider.headers = { ...Object.fromEntries(Object.entries(provider.headers ?? {}).filter(([key]) => key.toLowerCase() !== bridgeHeader)), authorization: `Bearer ${transport.capability}`, [bridgeHeader]: transport.capability, originator: "opencode", "x-codex-beta-features": "remote_compaction_v2" };
          delete provider.settings.encoding;
        });
        const models = subscriptionModels(inventory, [...openai.models.values()]);
        for (const model of models) allowed.set(model.id, model);
        editor.models.set("openai", models);
        const claude = editor.get("claude-agent");
        const anthropic = editor.get("anthropic");
        if (claude && claudeInventory.length) {
          editor.update("claude-agent", provider => { provider.activation = "enabled"; });
          editor.models.set("claude-agent", claudeModels(claudeInventory, [...(anthropic?.models.values() ?? []), ...claude.models.values()].filter(model => model.time.released > 0 || model.limit.output > 0)));
        }
      });
      await ctx.model.transform(editor => {
        for (const model of editor.list("openai")) {
          const supported = allowed.get(model.id) ?? allowed.get(model.modelID);
          if (!supported) { editor.remove("openai", model.id); continue; }
          editor.update("openai", model.id, value => {
            value.cost = structuredClone(supported.cost) as typeof value.cost;
            value.variants = structuredClone(supported.variants) as typeof value.variants;
            value.settings = { ...value.settings, baseURL: transport.baseURL, apiKey: transport.capability, compaction: { type: "native" } };
            delete value.settings.encoding;
            value.headers = Object.fromEntries(Object.entries(value.headers ?? {}).filter(([key]) => !["authorization", "chatgpt-account-id", bridgeHeader].includes(key.toLowerCase())));
            value.headers.authorization = `Bearer ${transport.capability}`;
            value.headers[bridgeHeader] = transport.capability;
            if (value.body?.reasoning?.mode === "pro") delete value.body.reasoning.mode;
          });
        }
      });
      const scope = { providerID: "openai" };
      await ctx.session.hook("model.request", event => { event.baseURL = transport.baseURL; event.headers = Object.fromEntries(Object.entries(event.headers).filter(([key]) => key.toLowerCase() !== bridgeHeader)); event.headers.authorization = `Bearer ${transport.capability}`; event.headers[bridgeHeader] = transport.capability; event.headers["session-id"] = event.sessionID; }, scope);
      for (const hook of ["context", "compaction"] as const) await ctx.session.hook(hook, event => { delete event.options.maxTokens; }, scope);
      await ctx.session.hook("http.request", async event => {
        const request = event.request;
        const path = routePath(request.url, request.url.startsWith(transport.baseURL + "/") ? transport.baseURL : route.baseURL);
        if (request.headers.has("upgrade") || !["GET", "HEAD", "POST", "PUT", "PATCH", "DELETE", "OPTIONS"].includes(request.method)) throw fail("expected HTTP request");
        const headers = new Headers(request.headers);
        headers.set("authorization", `Bearer ${transport.capability}`);
        headers.set(bridgeHeader, transport.capability);
        headers.delete("chatgpt-account-id");
        event.request = new Request(transport.baseURL + path, { method: request.method, headers, body: request.body, signal: request.signal, redirect: "error", duplex: "half" } as RequestInit);
      }, scope);
      await ctx.session.hook("experimental.ws.handshake", event => {
        const url = event.url.replace(/^ws:/, "http:");
        const path = routePath(url, url.startsWith(transport.baseURL + "/") ? transport.baseURL : route.baseURL);
        event.url = transport.baseURL.replace(/^http:/, "ws:") + path;
        event.headers = Object.fromEntries(Object.entries(event.headers).filter(([key]) => !["authorization", "chatgpt-account-id", bridgeHeader].includes(key.toLowerCase())));
        event.headers.authorization = `Bearer ${transport.capability}`;
        event.headers[bridgeHeader] = transport.capability;
      }, scope);
      // Account retries belong to the proxy, not independent OpenCode attempts.
      await ctx.session.hook("retry", event => { event.decision = { retry: false }; }, scope);
      return transport.close;
    } catch (error) { await transport.close(); throw error; }
  },
});
