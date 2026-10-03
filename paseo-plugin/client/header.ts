import type { PluginButtonMenuEntry, PluginButtonRegistration, PluginClientContext } from "@getpaseo/plugin/client";
import { headerLabel } from "../shared/format";
import { AddAccountPopover } from "./add-account";
import { statusRpc, switchRpc, type StatusReport } from "../shared/subswapper";

const refreshMs = 60_000;

// startHeaderButtons keeps one usage button in every workspace header and
// refreshes its label every minute. It returns the cleanup.
export function startHeaderButtons(client: PluginClientContext, surfaceId: string): () => void {
  const buttons = new Map<string, PluginButtonRegistration>();
  let report: StatusReport | null = null;
  let stopped = false;

  const menu = (): PluginButtonMenuEntry[] => [
    {
      kind: "item",
      id: "open",
      title: "Open subscriptions",
      icon: "Gauge",
      behavior: { kind: "action", onPress: () => client.openSurface(surfaceId) },
    },
    {
      kind: "item",
      id: "add-account",
      title: "Add account…",
      icon: "UserPlus",
      behavior: { kind: "popover", Content: AddAccountPopover },
    },
    { kind: "separator", id: "switch-divider" },
    ...(report?.services ?? [])
      .filter((service) => service.accounts.length > 1)
      .map(
        (service): PluginButtonMenuEntry => ({
          kind: "item",
          id: `best-${service.name.toLowerCase().replace(/[^a-z0-9-]/g, "-")}`,
          title: `Switch ${service.name} to the best account`,
          icon: "Shuffle",
          behavior: {
            kind: "action",
            onPress: async () => {
              await client.rpc(switchRpc, { service: service.name, account: "auto" });
              await refresh();
            },
          },
        }),
      ),
  ];

  const present = () => ({ label: headerLabel(report), behavior: { kind: "menu" as const, items: menu() } });

  async function refresh(): Promise<void> {
    try {
      report = (await client.rpc(statusRpc, {})).report;
    } catch {
      report = null;
    }
    let workspaceIds: string[] = [];
    try {
      workspaceIds = (await client.paseo.workspaces.list()).entries.map((workspace) => workspace.id);
    } catch {
      workspaceIds = [...buttons.keys()];
    }
    if (stopped) {
      return;
    }
    for (const [id, button] of buttons) {
      if (!workspaceIds.includes(id)) {
        button.remove();
        buttons.delete(id);
      }
    }
    for (const workspaceId of workspaceIds) {
      const existing = buttons.get(workspaceId);
      if (existing) {
        existing.update(present());
        continue;
      }
      buttons.set(
        workspaceId,
        client.addHeaderButton({
          id: "usage",
          workspaceId,
          button: { title: "Subscription usage", icon: "Gauge", ...present() },
        }),
      );
    }
  }

  void refresh();
  const timer = setInterval(() => void refresh(), refreshMs);
  return () => {
    stopped = true;
    clearInterval(timer);
    for (const button of buttons.values()) {
      button.remove();
    }
    buttons.clear();
  };
}
