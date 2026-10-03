import { spawn } from "node:child_process";
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

// addArgs builds `subswapper add`. Codex signs in with a device code because
// the terminal runs on the daemon host, which may not be the device with
// the browser.
export function addArgs(service: string, account: string): string[] {
  if (service !== "claude" && service !== "codex") {
    throw new Error("service must be claude or codex");
  }
  if (!namePattern.test(account)) {
    throw new Error("account names may contain only letters, digits, dots, dashes, and underscores");
  }
  return service === "codex" ? ["add", service, account, "-device"] : ["add", service, account];
}

export interface RunOptions {
  signal?: AbortSignal;
  // input is written to the CLI's stdin, e.g. a setup token.
  input?: string;
}

export async function runSubswapper(args: string[], options: RunOptions = {}): Promise<string> {
  const binary = await findSubswapper();
  if (binary === null) {
    throw new Error("subswapper is not installed on the daemon host; install it or set SUBSWAPPER_BIN for the Paseo daemon");
  }
  return new Promise((resolve, reject) => {
    const child = spawn(binary, args, { signal: options.signal, timeout: cliTimeoutMs, stdio: ["pipe", "pipe", "pipe"] });
    let stdout = "";
    let stderr = "";
    child.stdout.setEncoding("utf8").on("data", (chunk: string) => {
      stdout = (stdout + chunk).slice(-1 << 22);
    });
    child.stderr.setEncoding("utf8").on("data", (chunk: string) => {
      stderr = (stderr + chunk).slice(-1 << 16);
    });
    child.on("error", (error) => reject(new Error(`subswapper ${args[0]} failed: ${error.message}`)));
    child.on("close", (code) => {
      if (code === 0) {
        resolve(stdout);
        return;
      }
      const detail = (stderr || stdout).trim().split("\n").slice(-3).join(" ");
      reject(new Error(`subswapper ${args[0]} failed: ${detail || `exit ${code}`}`));
    });
    child.stdin.end(options.input ?? "");
  });
}
