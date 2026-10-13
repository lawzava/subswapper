import { launchedService, paseoProviderSchema } from "./configure.ts";

// OpenAI's default API base URL. Anything else that reads OPENAI_BASE_URL
// behaves as if it were unset, and Codex routes through its -c overrides.
export const openAIDefaultBaseURL = "https://api.openai.com/v1";

// sessionEnvironment returns the environment for a Paseo session whose
// provider launches subswapper, or null when nothing changes.
//
// SUBSWAPPER_SERVICE lets the usage sources find the session's service.
// Paseo's builtin Codex usage source reads the session's auth.json, which
// holds subswapper's proxy placeholder, and sends it to chatgpt.com, which
// answers 401. That source skips sessions whose environment sets
// OPENAI_BASE_URL. The provider config cannot set it: Paseo turns a provider's
// OPENAI_BASE_URL into a model provider override that bypasses the proxy.
export function sessionEnvironment(providerId: string, config: unknown, env: Record<string, string>): Record<string, string> | null {
  const provider = paseoProviderSchema.safeParse(config).data;
  if (!provider) {
    return null;
  }
  const kind = ["claude", "codex"].find((name) => providerId === name || provider.extends === name);
  const service = kind ? launchedService(provider) : null;
  if (!service) {
    return null;
  }
  const next = { ...env };
  if (!next.SUBSWAPPER_SERVICE) {
    next.SUBSWAPPER_SERVICE = service;
  }
  if (kind === "codex" && !next.OPENAI_BASE_URL && !provider.env?.OPENAI_BASE_URL) {
    next.OPENAI_BASE_URL = openAIDefaultBaseURL;
  }
  const changed = Object.keys(next).some((key) => next[key] !== env[key]);
  return changed ? next : null;
}
