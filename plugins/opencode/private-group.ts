import childProcess from "node:child_process";
import { open } from "node:fs/promises";
import { platform } from "node:os";

const identityLimit = 1024 * 1024;

function run(database: string): Promise<string> {
  return new Promise((resolve, reject) => {
    childProcess.execFile("/usr/bin/getent", [database], { timeout: 2000, maxBuffer: identityLimit, encoding: "utf8" }, (error, stdout) => {
      if (error) reject(error); else resolve(stdout);
    });
  });
}

// Only enumerable local NSS sources are supported. Remote/compat sources may
// hide primary-group users from enumeration, so an empty group is not proof.
export function supportsPrivateGroupNSS(config: string): boolean {
  const seen = new Set<string>();
  for (const line of config.split("\n")) {
    const fields = line.split("#", 1)[0].trim().split(/\s+/);
    if (fields[0] !== "passwd:" && fields[0] !== "group:" && fields[0] !== "initgroups:") continue;
    if (seen.has(fields[0]) || fields.length < 2 || !fields.slice(1).includes("files") ||
        fields.slice(1).some(source => source !== "files" && source !== "systemd")) return false;
    seen.add(fields[0]);
  }
  return seen.has("passwd:") && seen.has("group:");
}

function id(value: string): number {
  if (!/^(0|[1-9][0-9]*)$/.test(value)) throw new Error();
  const result = Number(value);
  if (!Number.isSafeInteger(result) || result > 0xffffffff) throw new Error();
  return result;
}

export function privateGroupRecords(uid: number, gid: number, passwd: string, group: string): boolean {
  try {
    let matchingGroups = 0;
    for (const line of group.replace(/\n+$/, "").split("\n")) {
      const fields = line.split(":");
      if (fields.length !== 4 || !fields[0]) return false;
      if (id(fields[2]) === gid) {
        if (fields[3] !== "") return false;
        matchingGroups++;
      }
    }
    if (matchingGroups !== 1) return false;
    let owners = 0;
    for (const line of passwd.replace(/\n+$/, "").split("\n")) {
      const fields = line.split(":");
      if (fields.length !== 7 || !fields[0]) return false;
      const accountUID = id(fields[2]), accountGID = id(fields[3]);
      if (accountUID === uid) { if (accountGID !== gid) return false; owners++; }
      else if (accountGID === gid) return false;
    }
    return owners === 1;
  } catch { return false; }
}

export async function isPrivateGroup(uid: number, gid: number): Promise<boolean> {
  if (platform() !== "linux") return false;
  try {
    const file = await open("/etc/nsswitch.conf", "r");
    let config: string;
    try {
      const buffer = Buffer.alloc(identityLimit + 1);
      let size = 0;
      while (size < buffer.length) {
        const { bytesRead } = await file.read(buffer, size, buffer.length - size, size);
        if (!bytesRead) break;
        size += bytesRead;
      }
      if (size > identityLimit) return false;
      config = buffer.toString("utf8", 0, size);
    } finally { await file.close(); }
    if (!supportsPrivateGroupNSS(config)) return false;
    const [passwd, group] = await Promise.all([run("passwd"), run("group")]);
    return privateGroupRecords(uid, gid, passwd, group);
  } catch { return false; }
}
