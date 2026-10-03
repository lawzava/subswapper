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
