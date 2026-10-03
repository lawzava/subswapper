import type { PluginServerContext } from "@getpaseo/plugin/server";
import { addToken, readStatus, signIn, switchAccount } from "./server/handlers";
import { addTokenRpc, signinRpc, statusRpc, switchRpc } from "./shared/subswapper";

export default function contribute(server: PluginServerContext) {
  server.handle(statusRpc, readStatus);
  server.handle(switchRpc, switchAccount);
  server.handle(signinRpc, signIn);
  server.handle(addTokenRpc, addToken);
  return () => {};
}
