import type { RpcInput } from "@getpaseo/plugin";
import type { PluginHandlerContext } from "@getpaseo/plugin/server";
import { addTokenRpc, reportSchema, signinRpc, statusRpc, switchRpc } from "../shared/subswapper";
import { addArgs, findSubswapper, runSubswapper, switchArgs } from "./cli";

export async function readStatus(_input: RpcInput<typeof statusRpc>) {
  try {
    const output = await runSubswapper(["status", "-json"]);
    return { report: reportSchema.parse(JSON.parse(output)), error: null };
  } catch (error) {
    return { report: null, error: error instanceof Error ? error.message : String(error) };
  }
}

export async function switchAccount({ service, account }: RpcInput<typeof switchRpc>) {
  const output = await runSubswapper(switchArgs(service, account));
  return { message: output.trim() || `switched ${service}` };
}

// signIn opens a terminal in the workspace that runs `subswapper add`; the
// provider's sign-in and the token prompt happen there.
export async function signIn({ workspaceId, service, account }: RpcInput<typeof signinRpc>, { paseo }: PluginHandlerContext) {
  const binary = await findSubswapper();
  if (binary === null) {
    throw new Error("subswapper is not installed on the daemon host; install it or set SUBSWAPPER_BIN for the Paseo daemon");
  }
  const name = `subswapper add ${service} ${account}`;
  const terminal = await paseo.terminals.create({ workspaceId, command: binary, args: addArgs(service, account), name });
  return { terminalId: terminal.id, name };
}

export async function addToken({ account, token }: RpcInput<typeof addTokenRpc>) {
  const output = await runSubswapper(addArgs("claude", account), { input: `${token}\n` });
  return { message: output.trim() || `added claude account ${account}` };
}
