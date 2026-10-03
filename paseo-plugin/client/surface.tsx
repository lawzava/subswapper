import type { PluginSurfaceProps } from "@getpaseo/plugin/client";
import { useRpc } from "@getpaseo/plugin/client";
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { useMemo } from "react";
import { Pressable, ScrollView, Text, View } from "react-native";
import { accountLoad, percent, relativeTime, windowText } from "../shared/format";
import { statusRpc, switchRpc, type AccountReport, type ServiceReport } from "../shared/subswapper";
import { AddAccountForm } from "./add-account";
import { statusQueryKey } from "./keys";

const refreshMs = 60_000;

export function SubscriptionsSurface({ theme, layout }: PluginSurfaceProps) {
  const readStatus = useRpc(statusRpc);
  const switchAccount = useRpc(switchRpc);
  const queryClient = useQueryClient();
  const status = useQuery({
    queryKey: statusQueryKey,
    queryFn: () => readStatus({}),
    refetchInterval: refreshMs,
  });
  const switching = useMutation({
    mutationFn: switchAccount,
    onSettled: () => queryClient.invalidateQueries({ queryKey: statusQueryKey }),
  });
  const styles = useMemo(
    () => ({
      screen: { flex: 1, backgroundColor: theme.colors.surface0 },
      content: { padding: layout.compact ? 16 : 24, gap: 16 },
      service: {
        gap: 8,
        padding: 12,
        borderRadius: 8,
        borderWidth: 1,
        borderColor: theme.colors.border,
        backgroundColor: theme.colors.surface1,
      },
      row: { flexDirection: "row" as const, alignItems: "center" as const, justifyContent: "space-between" as const, gap: 8 },
      account: { gap: 2, paddingVertical: 6, borderTopWidth: 1, borderTopColor: theme.colors.border },
      title: { color: theme.colors.foreground, fontSize: 16, fontWeight: "600" as const },
      text: { color: theme.colors.foreground },
      muted: { color: theme.colors.foregroundMuted, fontSize: 12 },
      warning: { color: theme.colors.statusWarning, fontSize: 12 },
      danger: { color: theme.colors.statusDanger },
      badge: { color: theme.colors.statusSuccess, fontSize: 12 },
      button: { paddingVertical: 6, paddingHorizontal: 10, borderRadius: 6, backgroundColor: theme.colors.accent },
      buttonText: { color: theme.colors.accentForeground, fontSize: 12 },
      code: { color: theme.colors.foreground, fontFamily: "monospace", fontSize: 12 },
    }),
    [theme, layout.compact],
  );
  const now = Date.now();
  const report = status.data?.report ?? null;

  function SwitchButton({ service, account, label }: { service: string; account: string; label: string }) {
    return (
      <Pressable
        accessibilityRole="button"
        accessibilityLabel={`${label} for ${service}`}
        disabled={switching.isPending}
        style={styles.button}
        onPress={() => switching.mutate({ service, account })}
      >
        <Text style={styles.buttonText}>{label}</Text>
      </Pressable>
    );
  }

  function AccountRow({ service, account }: { service: ServiceReport; account: AccountReport }) {
    const windows = [
      windowText("5h", account.five_hour, now),
      windowText("week", account.weekly, now),
      windowText("fable", account.fable_weekly, now),
    ].filter((line): line is string => line !== null);
    const updated = relativeTime(account.updated_at, now);
    return (
      <View style={styles.account}>
        <View style={styles.row}>
          <Text style={styles.text}>
            {account.name} {account.selected ? <Text style={styles.badge}>● selected</Text> : null}
          </Text>
          {!account.selected && account.ready ? <SwitchButton service={service.name} account={account.name} label="Use" /> : null}
        </View>
        {account.email ? <Text style={styles.muted}>{account.email}</Text> : null}
        <Text style={styles.muted}>{windows.length > 0 ? windows.join("   ") : "no usage yet"}</Text>
        <Text style={account.ready ? styles.muted : styles.warning}>
          {account.state}
          {account.ready ? ` · load ${percent(accountLoad(account))}` : ""}
          {updated ? ` · updated ${updated} ago` : ""}
        </Text>
      </View>
    );
  }

  return (
    <ScrollView style={styles.screen} contentContainerStyle={styles.content}>
      {status.isLoading ? <Text style={styles.muted}>Reading subscription usage…</Text> : null}
      {status.data?.error ? <Text style={styles.danger}>{status.data.error}</Text> : null}
      {switching.error ? <Text style={styles.danger}>{String(switching.error.message ?? switching.error)}</Text> : null}
      {switching.data ? <Text style={styles.muted}>{switching.data.message}</Text> : null}
      {report?.services.map((service) => (
        <View key={service.name} style={styles.service}>
          <View style={styles.row}>
            <Text style={styles.title}>{service.name}</Text>
            {service.accounts.length > 1 ? <SwitchButton service={service.name} account="auto" label="Use best" /> : null}
          </View>
          {service.hub ? <Text style={styles.muted}>accounts on the hub at {service.hub}</Text> : null}
          {service.note ? <Text style={styles.warning}>{service.note}</Text> : null}
          {service.accounts.map((account) => (
            <AccountRow key={account.name} service={service} account={account} />
          ))}
        </View>
      ))}
      <View style={styles.service}>
        <Text style={styles.title}>Add a subscription</Text>
        <AddAccountForm theme={theme} />
        <Text style={styles.muted}>Or run on any machine that uses these accounts:</Text>
        <Text style={styles.code}>subswapper add claude &lt;name&gt;</Text>
        <Text style={styles.code}>subswapper add codex &lt;name&gt;</Text>
      </View>
      <View style={styles.row}>
        <Text style={styles.muted}>
          {report ? `updated ${relativeTime(report.generated_at, now) ?? "now"} ago · refreshes every minute` : ""}
        </Text>
        <Pressable accessibilityRole="button" style={styles.button} onPress={() => void status.refetch()}>
          <Text style={styles.buttonText}>Refresh</Text>
        </Pressable>
      </View>
    </ScrollView>
  );
}
