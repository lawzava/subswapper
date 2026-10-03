import type { RpcInput } from "@getpaseo/plugin";
import { reportSchema, statusRpc, switchRpc } from "../shared/subswapper";
import { runSubswapper, switchArgs } from "./cli";

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
