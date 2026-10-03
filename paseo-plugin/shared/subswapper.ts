import { defineRpc } from "@getpaseo/plugin";
import { z } from "zod";

// Mirrors `subswapper status -json`. Percentages are 0-100.
const windowSchema = z.object({
  used_percent: z.number(),
  resets_at: z.string().optional(),
});

const accountSchema = z.object({
  name: z.string(),
  email: z.string().optional(),
  selected: z.boolean(),
  ready: z.boolean(),
  score: z.number().optional(),
  state: z.string(),
  five_hour: windowSchema.optional(),
  weekly: windowSchema.optional(),
  fable_weekly: windowSchema.optional(),
  updated_at: z.string().optional(),
});

const serviceSchema = z.object({
  name: z.string(),
  kind: z.string(),
  hub: z.string().optional(),
  note: z.string().optional(),
  selected: z.string().optional(),
  accounts: z.array(accountSchema),
});

export const reportSchema = z.object({
  generated_at: z.string(),
  services: z.array(serviceSchema),
});

export type StatusReport = z.infer<typeof reportSchema>;
export type ServiceReport = z.infer<typeof serviceSchema>;
export type AccountReport = z.infer<typeof accountSchema>;
export type UsageWindow = z.infer<typeof windowSchema>;

export const statusRpc = defineRpc({
  name: "subswapper.status",
  input: z.object({}),
  output: z.object({ report: reportSchema.nullable(), error: z.string().nullable() }),
});

// Names are validated again on the daemon before they reach the CLI.
const nameSchema = z.string().regex(/^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$/);

export const switchRpc = defineRpc({
  name: "subswapper.switch",
  input: z.object({ service: nameSchema, account: nameSchema }),
  output: z.object({ message: z.string() }),
});

const providerSchema = z.enum(["claude", "codex"]);

// signinRpc opens a workspace terminal that runs `subswapper add`, so the
// provider's interactive sign-in happens inside Paseo.
export const signinRpc = defineRpc({
  name: "subswapper.signin",
  input: z.object({ workspaceId: z.string().min(1), service: providerSchema, account: nameSchema }),
  output: z.object({ terminalId: z.string(), name: z.string() }),
});

// addTokenRpc registers a Claude setup token the user already has. The token
// goes to the CLI on stdin and is never echoed or logged.
export const addTokenRpc = defineRpc({
  name: "subswapper.add-token",
  input: z.object({
    account: nameSchema,
    token: z.string().regex(/^sk-ant-[A-Za-z0-9_-]{8,2048}$/),
  }),
  output: z.object({ message: z.string() }),
});
