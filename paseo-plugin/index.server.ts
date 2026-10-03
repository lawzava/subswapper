import type { PluginServerContext } from "@getpaseo/plugin/server";
import { readStatus, switchAccount } from "./server/handlers";
import { statusRpc, switchRpc } from "./shared/subswapper";

export default function contribute(server: PluginServerContext) {
  server.handle(statusRpc, readStatus);
  server.handle(switchRpc, switchAccount);
  return () => {};
}
