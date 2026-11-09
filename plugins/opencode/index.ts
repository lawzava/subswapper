import { Plugin } from "@opencode/plugin";
import { constants } from "node:fs";
import { lstat, open } from "node:fs/promises";
import { createHash, randomBytes, timingSafeEqual } from "node:crypto";
import { Agent, createServer, request as httpRequest, type IncomingHttpHeaders } from "node:http";
import { homedir, platform } from "node:os";
import { isIP } from "node:net";
import { dirname, isAbsolute, join, parse, resolve } from "node:path";
import { pipeline } from "node:stream/promises";
import { isPrivateGroup } from "./private-group.ts";

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

const hopHeaders = new Set(["connection", "keep-alive", "proxy-authenticate", "proxy-authorization", "te", "trailer", "transfer-encoding", "upgrade", "host"]);
function cleanHeaders(headers: IncomingHttpHeaders): IncomingHttpHeaders {
  const blocked = new Set([...hopHeaders, ...(headers.connection?.split(",").map(value => value.trim().toLowerCase()) ?? [])]);
  return Object.fromEntries(Object.entries(headers).filter(([key]) => !blocked.has(key)));
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
  const server = createServer(async (request, response) => {
    const reject = (status: number, message: string) => { if (!response.headersSent) { response.writeHead(status, { "content-type": "text/plain" }); response.end(`Subswapper: ${message}`); } else response.destroy(); };
    const auth = Buffer.from(request.headers.authorization ?? "");
    const expected = Buffer.from(`Bearer ${capability}`);
    if (auth.length !== expected.length || !timingSafeEqual(auth, expected)) { reject(401, "bridge authentication failed"); return; }
    if (request.url !== "/responses" || request.method !== "POST" || request.headers.upgrade ||
        request.headers["content-type"]?.split(";", 1)[0].trim().toLowerCase() !== "application/json") { reject(400, "expected JSON POST"); return; }
    const controller = new AbortController();
    controllers.add(controller);
    response.on("close", () => { if (!response.writableEnded) controller.abort(); });
    request.on("aborted", () => controller.abort());
    try {
      const placeholder = await readPlaceholder(route.placeholder);
      if (controller.signal.aborted) return;
      const headers = cleanHeaders(request.headers);
      headers.authorization = `Bearer ${placeholder.token}`;
      headers["chatgpt-account-id"] = placeholder.account_id;
      const upstream = httpRequest(route.endpoint, { method: "POST", headers, signal: controller.signal, agent });
      const finished = new Promise<void>((resolve, rejectPromise) => {
        upstream.on("error", rejectPromise);
        upstream.on("response", backend => {
          if (backend.statusCode! >= 300 && backend.statusCode! < 400) {
            backend.destroy(); reject(502, "proxy redirects are blocked"); resolve(); return;
          }
          response.writeHead(backend.statusCode!, cleanHeaders(backend.headers));
          pipeline(backend, response).then(resolve, rejectPromise);
        });
      });
      // Both directions stream with backpressure. Cancellation tears down both.
      await Promise.all([pipeline(request, upstream), finished]);
    } catch (error) {
      if (!controller.signal.aborted) reject(502, error instanceof Error && error.message.includes("placeholder") ? "unsafe, unreadable, or malformed placeholder" : "proxy request failed");
    } finally { controller.abort(); controllers.delete(controller); }
  });
  server.on("upgrade", (_, socket) => socket.destroy());
  await new Promise<void>((resolve, reject) => { server.once("error", reject); server.listen(0, "127.0.0.1", resolve); });
  server.unref();
  const address = server.address();
  if (!address || typeof address === "string") throw fail("bridge unavailable");
  return {
    endpoint: `http://127.0.0.1:${address.port}/responses`, capability,
    close: () => new Promise<void>(resolve => { for (const controller of controllers) controller.abort(); agent.destroy(); server.closeAllConnections(); server.close(() => resolve()); }),
  };
}

export default Plugin.define({
  id: "subswapper.codex",
  async setup(ctx) {
    const route = await discover(ctx.options);
    const transport = await bridge(route);
    try {
      await ctx.provider.transform(editor => {
        editor.update("openai", provider => {
          provider.settings = { ...provider.settings, transport: "http", compaction: { type: "summary" } };
          delete provider.settings.encoding;
        });
      });
      const scope = { providerID: "openai" };
      await ctx.session.hook("model.request", event => { event.baseURL = route.baseURL; }, scope);
      await ctx.session.hook("http.request", async event => {
        const request = event.request;
        if (request.url !== route.endpoint) throw fail("expected exact local Codex responses endpoint; disable direct OpenAI OAuth routing");
        if (request.method !== "POST" || request.headers.has("upgrade") || request.headers.get("content-type")?.split(";", 1)[0].trim().toLowerCase() !== "application/json") throw fail("expected JSON POST over HTTP only");
        const headers = new Headers(request.headers);
        headers.set("authorization", `Bearer ${transport.capability}`);
        headers.delete("chatgpt-account-id");
        event.request = new Request(transport.endpoint, { method: request.method, headers, body: request.body, signal: request.signal, redirect: "error", duplex: "half" } as RequestInit);
      }, scope);
      await ctx.session.hook("experimental.ws.handshake", () => { throw fail("HTTP only; WebSocket handshake blocked"); }, scope);
      // Account retries belong to the proxy, not independent OpenCode attempts.
      await ctx.session.hook("retry", event => { event.decision = { retry: false }; }, scope);
      return transport.close;
    } catch (error) { await transport.close(); throw error; }
  },
});
