import type { PluginHandlerContext } from "@getpaseo/plugin/server";
import { z } from "zod";
import { launchConfigurationSchema, type LaunchConfiguration } from "../shared/subswapper.ts";

type ConfigApi = Pick<PluginHandlerContext["paseo"], "config">;

// The fields this plugin reads; Paseo validates the rest.
const paseoProviderSchema = z.object({
  extends: z.string().optional(),
  command: z.array(z.string()).optional(),
  env: z.record(z.string(), z.string()).optional(),
});
type Provider = z.infer<typeof paseoProviderSchema>;

// launchedService names the Subswapper service a Paseo provider launches, or
// null when it does not launch Subswapper. It reads a shim's environment, and
// the `-service` flag or provider shortcut of a wrapper command.
export function launchedService(provider: Provider): string | null {
  if (provider.env?.SUBSWAPPER_SERVICE) {
    return provider.env.SUBSWAPPER_SERVICE;
  }
  const command = provider.command ?? [];
  if (command.length === 0 || !command[0].includes("subswapper")) {
    return null;
  }
  for (const [index, arg] of command.entries()) {
    if (arg === "-service" || arg === "--service") {
      return command[index + 1] ?? null;
    }
    const inline = /^--?service=(.+)$/.exec(arg);
    if (inline) {
      return inline[1];
    }
  }
  const binary = command[0].split(/[\\/]/).pop()?.replace(/\.exe$/i, "");
  if (binary === "codex" || binary === "claude") {
    return binary;
  }
  return command.slice(1).find((arg) => arg === "codex" || arg === "claude") ?? null;
}

// configureCodexProvider points every Paseo Codex provider that launches the
// service at the shim, keeping their IDs so saved sessions and model choices
// survive. Without one, it adds a provider.
export async function configureCodexProvider(paseo: ConfigApi, service: string, configuration: LaunchConfiguration) {
  const launch = launchConfigurationSchema.parse(configuration);
  const { config } = await paseo.config.get();
  const providers = Object.fromEntries(Object.entries(config.providers ?? {})
    .map(([id, provider]) => [id, paseoProviderSchema.safeParse(provider).data ?? {}] as const));
  const update = { command: launch.command, env: launch.env };
  const targets = Object.entries(providers)
    .filter(([id, provider]) => (id === "codex" || provider.extends === "codex") && launchedService(provider) === service)
    .map(([id]) => id);
  if (targets.length > 0) {
    await paseo.config.patch({ providers: Object.fromEntries(targets.map((id) => [id, update])) });
    return { message: `${targets.join(", ")} now run Codex through Subswapper with Auto-review and Goals. Reload existing Codex sessions to refresh their features.` };
  }
  const id = service === "codex" ? "codex-subswapper" : `codex-subswapper-${service.toLowerCase().replace(/[^a-z0-9-]/g, "-")}`;
  if (providers[id]) {
    throw new Error(`Paseo provider ${id} does not launch Subswapper's ${service} service; point it at Subswapper or remove it`);
  }
  const label = service === "codex" ? "Codex (Subswapper)" : `Codex (Subswapper ${service})`;
  await paseo.config.patch({ providers: { [id]: { extends: "codex", label, ...update } } });
  return { message: `Added the Paseo provider ${label}, which runs Codex through Subswapper with Auto-review and Goals.` };
}
