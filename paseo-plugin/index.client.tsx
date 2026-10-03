import type { PluginClientContext } from "@getpaseo/plugin/client";
import { startHeaderButtons } from "./client/header";
import { SubscriptionsSurface } from "./client/surface";
import { switchRpc } from "./shared/subswapper";

const surfaceId = "subscriptions";

export default function contribute(client: PluginClientContext) {
  client.addSurface(surfaceId, SubscriptionsSurface);
  client.addSidebarItem({ id: surfaceId, title: "Subscriptions", icon: "Gauge", surface: surfaceId });
  client.addCommandCenterItem({
    id: "open-subscriptions",
    title: "Subswapper: open subscription usage",
    icon: "Gauge",
    context: "global",
    onSelect: () => client.openSurface(surfaceId),
  });
  client.addCommandCenterItem({
    id: "switch-best",
    title: "Subswapper: switch every service to its best account",
    icon: "Shuffle",
    context: "global",
    onSelect: async () => {
      await client.rpc(switchRpc, { service: "all", account: "auto" });
    },
  });
  return startHeaderButtons(client, surfaceId);
}
