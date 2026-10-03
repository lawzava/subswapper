import { execFile } from "node:child_process";
import { constants } from "node:fs";
import { access } from "node:fs/promises";
import { homedir } from "node:os";
import { delimiter, join } from "node:path";

const cliTimeoutMs = 120_000;
const namePattern = /^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$/;

async function executable(path: string): Promise<boolean> {
  try {
    await access(path, constants.X_OK);
    return true;
  } catch {
    return false;
  }
}

// findSubswapper prefers SUBSWAPPER_BIN, then PATH, then the install
// locations `go install` and `subswapper setup` use.
export async function findSubswapper(env: NodeJS.ProcessEnv = process.env): Promise<string | null> {
  const candidates: string[] = [];
  if (env.SUBSWAPPER_BIN) {
    candidates.push(env.SUBSWAPPER_BIN);
  }
  for (const dir of (env.PATH ?? "").split(delimiter)) {
    if (dir) {
      candidates.push(join(dir, "subswapper"));
    }
  }
  candidates.push(join(homedir(), "go", "bin", "subswapper"), join(homedir(), ".local", "bin", "subswapper"));
  for (const candidate of candidates) {
    if (await executable(candidate)) {
      return candidate;
    }
  }
  return null;
}

export function switchArgs(service: string, account: string): string[] {
  if (!namePattern.test(service) || !namePattern.test(account)) {
    throw new Error("service and account names may contain only letters, digits, dots, dashes, and underscores");
  }
  return ["switch", service, account];
}

export async function runSubswapper(args: string[], signal?: AbortSignal): Promise<string> {
  const binary = await findSubswapper();
  if (binary === null) {
    throw new Error("subswapper is not installed on the daemon host; install it or set SUBSWAPPER_BIN for the Paseo daemon");
  }
  return new Promise((resolve, reject) => {
    execFile(binary, args, { timeout: cliTimeoutMs, maxBuffer: 4 << 20, signal }, (error, stdout, stderr) => {
      if (error) {
        const detail = String(stderr || stdout || error.message).trim().split("\n").slice(-3).join(" ");
        reject(new Error(`subswapper ${args[0]} failed: ${detail}`));
        return;
      }
      resolve(String(stdout));
    });
  });
}
