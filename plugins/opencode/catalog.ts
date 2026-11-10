import { Model, Provider } from "@opencode/plugin";
import childProcess from "node:child_process";
import { randomBytes } from "node:crypto";
import { Schema, type Types } from "effect";

const record = (value: unknown): Record<string, unknown> => value !== null && typeof value === "object" && !Array.isArray(value) ? value as Record<string, unknown> : {};
const positive = (value: unknown): value is number => Number.isSafeInteger(value) && (value as number) > 0;
const modelID = (value: unknown): value is string => typeof value === "string" && /^[A-Za-z0-9][A-Za-z0-9._-]*(?:\[1m\])?$/.test(value);
const effortNames = new Set(["none", "minimal", "low", "medium", "high", "xhigh", "max"]);
const zeroCost = [Schema.decodeUnknownSync(Model.Cost)({ input: 0, output: 0, cache: { read: 0, write: 0 } })];
type ClaudeModel = Record<string, string | number | string[]>;

/** The SDK's supportedModels() reads this initialization response. No user
 * message is sent, and account/login fields are neither returned nor stored. */
export async function discoverClaude(executable: string): Promise<ClaudeModel[]> {
  return new Promise(resolve => {
    const child = childProcess.spawn(executable, ["--print", "--input-format", "stream-json", "--output-format", "stream-json", "--verbose",
      "--setting-sources", "", "--settings", '{"disableAllHooks":true}', "--strict-mcp-config", "--mcp-config", '{"mcpServers":{}}',
      "--tools", "", "--permission-mode", "dontAsk", "--no-session-persistence"], {
      shell: false, detached: process.platform !== "win32", stdio: ["pipe", "pipe", "ignore"],
      env: { ...process.env, CLAUDE_CODE_DISABLE_CLAUDE_MDS: "1", CLAUDE_CODE_DISABLE_AUTO_MEMORY: "1", DISABLE_AUTO_COMPACT: "1" },
    });
    let pending = "", size = 0, done = false;
    const id = randomBytes(16).toString("hex");
    const finish = (models: ClaudeModel[] = []) => {
      if (done) return;
      done = true;
      clearTimeout(timer);
      child.stdin.destroy();
      child.stdout.destroy();
      // A configured shim may have children. End the private discovery group,
      // never the host server's process group.
      if (child.pid && process.platform !== "win32") { try { process.kill(-child.pid, "SIGKILL"); } catch { /* already exited */ } }
      else child.kill();
      resolve(models);
    };
    const timer = setTimeout(finish, 15000);
    child.once("error", () => finish());
    child.once("exit", () => finish());
    child.stdin.on("error", () => finish());
    child.stdout.on("error", () => finish());
    child.stdout.on("data", (chunk: Buffer) => {
      size += chunk.length;
      if (size > 1024 * 1024) { finish(); return; }
      pending += chunk.toString("utf8");
      let newline: number;
      while ((newline = pending.indexOf("\n")) >= 0) {
        const line = pending.slice(0, newline); pending = pending.slice(newline + 1);
        try {
          const message = record(JSON.parse(line));
          const response = record(message.response);
          if (message.type !== "control_response" || response.request_id !== id) continue;
          const models = record(response.response).models;
          finish(response.subtype === "success" && Array.isArray(models) ? models.flatMap(value => {
            const entry = record(value);
            if (!modelID(entry.value)) return [];
            const model: ClaudeModel = { value: entry.value };
            if (modelID(entry.resolvedModel)) model.resolvedModel = entry.resolvedModel;
            if (typeof entry.displayName === "string") model.displayName = entry.displayName;
            if (positive(entry.contextWindow)) model.contextWindow = entry.contextWindow;
            if (Array.isArray(entry.supportedEffortLevels)) model.supportedEffortLevels = entry.supportedEffortLevels.filter((effort): effort is string => typeof effort === "string" && effortNames.has(effort));
            return [model];
          }) : []);
        } catch { finish(); }
      }
    });
    child.stdin.write(JSON.stringify({ type: "control_request", request_id: id, request: { subtype: "initialize", hooks: {}, sdkMcpServers: [], agents: {} } }) + "\n");
  });
}

function base(provider: string, id: string, source?: Model.Info): Types.DeepMutable<Model.Info> {
  return structuredClone({ ...(source ?? Model.Info.default(Provider.ID.make(provider), Model.ID.make(id))),
    id: Model.ID.make(id), modelID: Model.ID.make(id), providerID: Provider.ID.make(provider), enabled: true,
    ...(!source ? { time: { released: 0 }, limit: { context: 0, output: 0 } } : {}), cost: zeroCost }) as Types.DeepMutable<Model.Info>;
}

/** Join only exact resolved IDs. An older family member is not version evidence. */
export function claudeModels(inventory: unknown, catalog: readonly Model.Info[]): Model.Info[] {
  if (!Array.isArray(inventory)) return [];
  const sources = new Map(catalog.map(model => [model.id.replace(/\[1m\]$/, ""), model]));
  const models = new Map<string, Model.Info>();
  for (const value of inventory) {
    const entry = record(value);
    if (!modelID(entry.value)) continue;
    const resolved = modelID(entry.resolvedModel) ? entry.resolvedModel : entry.value;
    const source = sources.get(resolved.replace(/\[1m\]$/, ""));
    const model = base("claude-agent", entry.value, source);
    // Metadata from the API catalog must not replace the harness driver.
    delete model.package; delete model.canonical; delete model.settings; delete model.headers; delete model.body;
    const name = source?.name ?? (typeof entry.displayName === "string" ? entry.displayName : resolved);
    model.name = entry.value === resolved ? name : `${source ? name : `${name}: ${resolved}`} (${entry.value})`;
    model.capabilities = { tools: true, input: ["text", "image", "pdf"], output: ["text"] };
    if (positive(entry.contextWindow)) model.limit = { ...model.limit, context: entry.contextWindow };
    model.variants = (Array.isArray(entry.supportedEffortLevels) ? entry.supportedEffortLevels : [])
      .filter((effort): effort is string => typeof effort === "string" && effortNames.has(effort) && !["none", "minimal"].includes(effort))
      .map(effort => ({ id: Model.VariantID.make(effort), settings: { effort } }));
    models.set(entry.value, model);
    if (resolved !== entry.value && !models.has(resolved)) models.set(resolved, { ...structuredClone(model), id: Model.ID.make(resolved), modelID: Model.ID.make(resolved), name: source?.name ?? resolved });
  }
  return [...models.values()];
}

/** Backend inventory decides availability; the catalog supplies metadata only. */
export function subscriptionModels(inventory: unknown, catalog: readonly Model.Info[]): Model.Info[] {
  const entries = record(inventory).models;
  const sources = new Map(catalog.map(model => [model.id, model]));
  const models = new Map<string, Model.Info>();
  for (const value of Array.isArray(entries) ? entries : []) {
    const entry = record(value);
    if (!modelID(entry.slug) || entry.visibility === "hide" || entry.visibility === "hidden") continue;
    const matches = catalog.filter(model => model.modelID === entry.slug && model.type !== "image" && record(model.body?.reasoning).mode !== "pro");
    for (const source of matches.length ? matches : [undefined]) {
      const model = base("openai", source?.id ?? entry.slug, source);
      model.modelID = Model.ID.make(entry.slug);
      model.name = source?.name ?? (typeof entry.display_name === "string" ? entry.display_name : entry.slug);
      const output = positive(entry.max_output_tokens) ? entry.max_output_tokens : model.limit.output;
      const input = positive(entry.context_window) ? Math.min(model.limit.input ?? entry.context_window, entry.context_window) : model.limit.input;
      model.limit = { ...model.limit, output, ...(input !== undefined ? { input, context: input + output } : {}) };
      model.variants = (Array.isArray(entry.supported_reasoning_levels) ? entry.supported_reasoning_levels : [])
        .map(value => record(value).effort)
        .filter((effort): effort is string => typeof effort === "string" && effortNames.has(effort))
        .map(reasoningEffort => {
          const original = source?.variants.find(variant => variant.id === reasoningEffort && record(variant.body?.reasoning).mode !== "pro");
          return { ...structuredClone(original), id: Model.VariantID.make(reasoningEffort), settings: { ...original?.settings, reasoningEffort } };
        });
      models.set(model.id, model);
    }
  }
  // ChatGPT's Codex inventory lists text models, not the image API catalog.
  // Listing is not a subscription-entitlement claim: backend rejection is
  // relayed unchanged, with no API-key fallback.
  const imageIDs = new Set([...catalog.filter(model => model.type === "image").map(model => model.modelID),
    ...["gpt-image-2", "gpt-image-2.5-flare", "gpt-image-2.5-sunburst"].map(id => Model.ID.make(id))]);
  for (const id of imageIDs) {
    const source = sources.get(id);
    const model = base("openai", id, source);
    model.type = "image";
    model.name = source?.name ?? id;
    model.variants = [];
    model.capabilities = { tools: false, input: ["text", "image"], output: ["image"] };
    models.set(id, model);
  }
  return [...models.values()];
}
