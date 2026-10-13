import type { PluginServerContext } from "@getpaseo/plugin/server";
import { runSubswapper } from "./server/cli";
import { addToken, configureCodex, readStatus, signIn, switchAccount } from "./server/handlers";
import { sessionEnvironment } from "./server/session";
import { cachedReader, usageAccounts, usageInputSchema, usageReport, type ProviderKind } from "./server/usage";
import { addTokenRpc, configureCodexRpc, reportSchema, signinRpc, statusRpc, switchRpc } from "./shared/subswapper";

const usageSources: { kind: ProviderKind; label: string }[] = [
  { kind: "claude", label: "Claude (Subswapper)" },
  { kind: "codex", label: "Codex (Subswapper)" },
];

// contributeUsage shows subswapper's accounts in Paseo's usage cards and
// sidebar bars, and marks sessions that launch subswapper. Paseo 0.11 added
// both APIs.
function contributeUsage(server: PluginServerContext) {
  const status = cachedReader(async () => reportSchema.parse(JSON.parse(await runSubswapper(["status", "-json"]))));
  for (const source of usageSources) {
    server.registerUsageSource({
      id: `subswapper-${source.kind}`,
      label: source.label,
      icon: `icons/${source.kind}.svg`,
      input: usageInputSchema,
      discover: async (scope) => usageAccounts(await status(), source.kind, scope),
      fetch: async (input) => {
        try {
          return usageReport(await status(), usageInputSchema.parse(input), Date.now());
        } catch (error) {
          return { status: "error", error: error instanceof Error ? error.message : String(error) };
        }
      },
    });
  }
  server.before("agent.session_open", async ({ request }, { paseo }) => {
    // A failure here must never stop a session from opening.
    try {
      const { config } = await paseo.config.get();
      const env = sessionEnvironment(request.provider, config.providers?.[request.provider], request.env);
      return env ? { ...request, env } : undefined;
    } catch {
      return undefined;
    }
  });
}

export default function contribute(server: PluginServerContext) {
  server.handle(statusRpc, readStatus);
  server.handle(switchRpc, switchAccount);
  server.handle(signinRpc, signIn);
  server.handle(addTokenRpc, addToken);
  server.handle(configureCodexRpc, configureCodex);
  if (typeof server.registerUsageSource === "function" && typeof server.before === "function") {
    contributeUsage(server);
  }
  return () => {};
}
