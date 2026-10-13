import type { PluginServerContext } from "@getpaseo/plugin/server";
import { addToken, configureCodex, readStatus, signIn, switchAccount } from "./server/handlers";
import { addTokenRpc, configureCodexRpc, signinRpc, statusRpc, switchRpc } from "./shared/subswapper";

export default function contribute(server: PluginServerContext) {
  server.handle(statusRpc, readStatus);
  server.handle(switchRpc, switchAccount);
  server.handle(signinRpc, signIn);
  server.handle(addTokenRpc, addToken);
  server.handle(configureCodexRpc, configureCodex);
  return () => {};
}
