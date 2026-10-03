import type { AccountReport, StatusReport, UsageWindow } from "./subswapper";

// The binding number for an account: its score (worst window) when it can
// take requests, else the fullest window it reports.
export function accountLoad(account: AccountReport): number | null {
  if (account.score !== undefined) {
    return account.score;
  }
  const used = [account.five_hour, account.weekly, account.fable_weekly]
    .filter((window): window is UsageWindow => window !== undefined)
    .map((window) => window.used_percent);
  return used.length > 0 ? Math.max(...used) : null;
}

export function percent(value: number | null | undefined): string {
  return value === null || value === undefined ? "-" : `${Math.round(value)}%`;
}

// relativeTime renders how far an ISO time is from now: "3m", "5h", "2d".
export function relativeTime(iso: string | undefined, now: number): string | null {
  if (!iso) {
    return null;
  }
  const at = Date.parse(iso);
  if (Number.isNaN(at)) {
    return null;
  }
  const minutes = Math.round(Math.abs(at - now) / 60_000);
  if (minutes < 1) {
    return "now";
  }
  if (minutes < 60) {
    return `${minutes}m`;
  }
  if (minutes < 48 * 60) {
    return `${Math.round(minutes / 60)}h`;
  }
  return `${Math.round(minutes / (24 * 60))}d`;
}

export function windowText(label: string, window: UsageWindow | undefined, now: number): string | null {
  if (!window) {
    return null;
  }
  const reset = relativeTime(window.resets_at, now);
  return `${label} ${percent(window.used_percent)}${reset ? ` · resets in ${reset}` : ""}`;
}

// headerLabel summarizes the selected account of every service, e.g.
// "claude 86% · codex 1%".
export function headerLabel(report: StatusReport | null): string {
  if (!report) {
    return "subswapper";
  }
  const parts = report.services
    .map((service) => {
      const selected = service.accounts.find((account) => account.selected);
      return selected ? `${service.name} ${percent(accountLoad(selected))}` : null;
    })
    .filter((part): part is string => part !== null);
  return parts.length > 0 ? parts.join(" · ") : "subswapper";
}
