import type { UsageAccount, UsageDetail, UsageReport, UsageScope, UsageWindow } from "@getpaseo/plugin/server/usage";
import { z } from "zod";
import { relativeTime } from "../shared/format.ts";
import type { AccountReport, ServiceReport, StatusReport } from "../shared/subswapper.ts";

export type ProviderKind = "claude" | "codex";

export const usageInputSchema = z.object({ service: z.string().min(1) }).strict();

export type UsageInput = z.infer<typeof usageInputSchema>;

function selectedCard(service: ServiceReport): UsageAccount {
  const label = service.name === service.kind ? "Selected account" : `${service.name} selected account`;
  const key = service.name.replace(/[^A-Za-z0-9._-]/g, "-").slice(0, 128);
  return { key, label, harness: "Subswapper", input: { service: service.name } };
}

// usageAccounts lists one usage card per service of a provider kind, following
// the account the proxy sends requests to. Paseo pins sidebar windows by
// source and window rather than by card, so a card per account would repeat
// in the sidebar; the Subscriptions screen lists every account instead. A
// session gets only the card of the service it launches through.
export function usageAccounts(report: StatusReport, kind: ProviderKind, scope: UsageScope): UsageAccount[] {
  const services = report.services.filter((service) => service.kind === kind);
  if (scope.kind === "session") {
    const service = scope.provider === kind ? services.find((candidate) => candidate.name === scope.env.SUBSWAPPER_SERVICE) : undefined;
    return service ? [selectedCard(service)] : [];
  }
  return services.map(selectedCard);
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

// windows names the account in the short label of the last sidebar window,
// because the sidebar shows only an icon, percentages, and short labels, and
// the card's account label is fixed when Paseo discovers the card.
function windows(account: AccountReport, now: number): UsageWindow[] {
  const result = windowNames.flatMap((name): UsageWindow[] => {
    const window = account[name.id];
    if (!window) {
      return [];
    }
    const resetsAt = window.resets_at && Date.parse(window.resets_at) > now ? window.resets_at : null;
    return [{
      id: name.id,
      label: name.label,
      shortLabel: name.shortLabel,
      ...(name.summary ? { summary: true } : {}),
      usedPct: window.used_percent,
      remainingPct: Math.max(0, 100 - window.used_percent),
      resetsAt,
      tone: tone(window.used_percent),
    }];
  });
  const last = result.findLast((window) => window.summary);
  if (last) {
    last.shortLabel = `${last.shortLabel} · ${account.name}`;
  }
  return result;
}

function details(account: AccountReport, now: number): UsageDetail[] {
  const rows: UsageDetail[] = [
    { id: "account", label: "Account", value: account.email ? `${account.name} · ${account.email}` : account.name },
    { id: "state", label: "State", value: account.state, tone: account.ready ? "ok" : "warning" },
  ];
  const updated = relativeTime(account.updated_at, now);
  if (updated) {
    rows.push({ id: "updated", label: "Updated", value: updated === "now" ? "just now" : `${updated} ago` });
  }
  return rows;
}

// usageReport turns a service's selected account in `subswapper status -json`
// into a Paseo usage report.
export function usageReport(report: StatusReport, input: UsageInput, now: number): UsageReport {
  const service = report.services.find((candidate) => candidate.name === input.service);
  if (!service) {
    return { status: "error", error: `subswapper has no ${input.service} service` };
  }
  const account = service.accounts.find((candidate) => candidate.name === service.selected);
  if (!account) {
    return { status: "error", error: service.note ?? `${service.name} has no selected account` };
  }
  return { status: "available", planLabel: account.name, windows: windows(account, now), details: details(account, now) };
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
