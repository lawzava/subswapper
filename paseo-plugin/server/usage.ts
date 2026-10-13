import type { UsageAccount, UsageDetail, UsageReport, UsageScope, UsageWindow } from "@getpaseo/plugin/server/usage";
import { z } from "zod";
import { relativeTime } from "../shared/format.ts";
import type { AccountReport, ServiceReport, StatusReport } from "../shared/subswapper.ts";

export type ProviderKind = "claude" | "codex";

export const usageInputSchema = z.object({
  service: z.string().min(1),
  // Absent for the card that follows the service's selected account.
  account: z.string().min(1).optional(),
}).strict();

export type UsageInput = z.infer<typeof usageInputSchema>;

const harness = "Subswapper";

function accountKey(...parts: string[]): string {
  return parts.join(".").replace(/[^A-Za-z0-9._-]/g, "-").slice(0, 128);
}

function selectedCard(service: ServiceReport): UsageAccount {
  const label = service.name === service.kind ? "Selected account" : `${service.name} selected account`;
  return { key: accountKey(service.name), label, harness, input: { service: service.name } };
}

function accountCard(service: ServiceReport, account: AccountReport): UsageAccount {
  const name = service.name === service.kind ? account.name : `${service.name} ${account.name}`;
  return {
    key: accountKey(service.name, account.name),
    label: account.email ? `${name} · ${account.email}` : name,
    harness,
    input: { service: service.name, account: account.name },
  };
}

// usageAccounts lists the usage cards for one provider kind. The global scope
// gets a card per service that follows its selected account, plus one per
// account. A session gets only the card of the service it launches through,
// because the proxy sends its requests to that service's selected account.
export function usageAccounts(report: StatusReport, kind: ProviderKind, scope: UsageScope): UsageAccount[] {
  const services = report.services.filter((service) => service.kind === kind);
  if (scope.kind === "session") {
    const service = scope.provider === kind ? services.find((candidate) => candidate.name === scope.env.SUBSWAPPER_SERVICE) : undefined;
    return service ? [selectedCard(service)] : [];
  }
  return services.flatMap((service) => [selectedCard(service), ...service.accounts.map((account) => accountCard(service, account))]);
}

const windowNames = [
  { id: "five_hour", label: "5-hour", shortLabel: "5h", summary: true },
  { id: "weekly", label: "Weekly", shortLabel: "wk", summary: true },
  { id: "fable_weekly", label: "Fable weekly", shortLabel: "Fable", summary: false },
] as const;

// Matches toneFromUsedPct in @getpaseo/plugin/server/usage, which a Paseo
// 0.10 daemon cannot import.
function tone(usedPct: number): UsageWindow["tone"] {
  if (usedPct > 90) {
    return "danger";
  }
  return usedPct >= 70 ? "warning" : "ok";
}

function windows(account: AccountReport, summary: boolean, now: number): UsageWindow[] {
  return windowNames.flatMap((name) => {
    const window = account[name.id];
    if (!window) {
      return [];
    }
    const resetsAt = window.resets_at && Date.parse(window.resets_at) > now ? window.resets_at : null;
    return [{
      id: name.id,
      label: name.label,
      shortLabel: name.shortLabel,
      ...(summary && name.summary ? { summary: true } : {}),
      usedPct: window.used_percent,
      remainingPct: Math.max(0, 100 - window.used_percent),
      resetsAt,
      tone: tone(window.used_percent),
    }];
  });
}

function details(account: AccountReport, selected: boolean, now: number): UsageDetail[] {
  const rows: UsageDetail[] = [];
  if (selected) {
    rows.push({ id: "account", label: "Account", value: account.email ? `${account.name} · ${account.email}` : account.name });
  }
  rows.push({ id: "state", label: "State", value: account.state, tone: account.ready ? "ok" : "warning" });
  const updated = relativeTime(account.updated_at, now);
  if (updated) {
    rows.push({ id: "updated", label: "Updated", value: updated === "now" ? "just now" : `${updated} ago` });
  }
  return rows;
}

// usageReport turns one card's slice of `subswapper status -json` into a
// Paseo usage report.
export function usageReport(report: StatusReport, input: UsageInput, now: number): UsageReport {
  const service = report.services.find((candidate) => candidate.name === input.service);
  if (!service) {
    return { status: "error", error: `subswapper has no ${input.service} service` };
  }
  const following = input.account === undefined;
  const account = service.accounts.find((candidate) => candidate.name === (input.account ?? service.selected));
  if (!account) {
    const missing = following ? `${service.name} has no selected account` : `${service.name} has no account ${input.account}`;
    return { status: "error", error: service.note ?? missing };
  }
  return {
    status: "available",
    ...(following ? { planLabel: account.name } : {}),
    windows: windows(account, following, now),
    details: details(account, following, now),
  };
}

// cachedReader shares one status read across the cards Paseo fetches
// together, so a refresh runs the CLI once. Failures are not cached.
export function cachedReader(read: () => Promise<StatusReport>, ttlMs = 15_000, now: () => number = Date.now): () => Promise<StatusReport> {
  let cached: { at: number; report: Promise<StatusReport> } | null = null;
  return () => {
    if (cached && now() - cached.at < ttlMs) {
      return cached.report;
    }
    const report = read();
    const entry = { at: now(), report };
    cached = entry;
    report.catch(() => {
      if (cached === entry) {
        cached = null;
      }
    });
    return report;
  };
}
