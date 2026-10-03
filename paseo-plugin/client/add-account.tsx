import type { PluginButtonContentProps, PluginHostProps } from "@getpaseo/plugin/client";
import { useRpc } from "@getpaseo/plugin/client";
import { useMutation, useQueryClient } from "@tanstack/react-query";
import { useMemo, useState } from "react";
import { Pressable, Text, TextInput, View } from "react-native";
import { addTokenRpc, signinRpc } from "../shared/subswapper";
import { statusQueryKey } from "./keys";

const accountPattern = /^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$/;

type Provider = "claude" | "codex";

// AddAccountForm signs a new account in through a workspace terminal, or
// adds a Claude setup token directly. Without a workspace only the token
// path is available.
export function AddAccountForm({ theme, workspaceId, onDone }: Pick<PluginHostProps, "theme"> & { workspaceId?: string; onDone?: () => void }) {
  const signIn = useRpc(signinRpc);
  const addToken = useRpc(addTokenRpc);
  const queryClient = useQueryClient();
  const [service, setService] = useState<Provider>("claude");
  const [account, setAccount] = useState("");
  const [token, setToken] = useState("");
  const [notice, setNotice] = useState<string | null>(null);
  const refresh = () => queryClient.invalidateQueries({ queryKey: statusQueryKey });
  const terminal = useMutation({
    mutationFn: signIn,
    onSuccess: ({ name }) => {
      setNotice(
        service === "codex"
          ? `Opened terminal "${name}" in this workspace. It shows a link and a code; approve them on any device.`
          : `Opened terminal "${name}" in this workspace. Sign in, then paste the printed token at its prompt. Close the terminal afterwards; its scrollback holds the token.`,
      );
      onDone?.();
    },
  });
  const direct = useMutation({
    mutationFn: addToken,
    onSuccess: ({ message }) => {
      setToken("");
      setNotice(message);
      void refresh();
    },
  });
  const styles = useMemo(
    () => ({
      form: { gap: 8 },
      row: { flexDirection: "row" as const, gap: 8, flexWrap: "wrap" as const },
      label: { color: theme.colors.foregroundMuted, fontSize: 12 },
      input: {
        color: theme.colors.foreground,
        borderWidth: 1,
        borderColor: theme.colors.border,
        borderRadius: 6,
        paddingHorizontal: 8,
        paddingVertical: 6,
      },
      choice: { paddingVertical: 6, paddingHorizontal: 10, borderRadius: 6, borderWidth: 1, borderColor: theme.colors.border },
      chosen: { paddingVertical: 6, paddingHorizontal: 10, borderRadius: 6, borderWidth: 1, borderColor: theme.colors.accent, backgroundColor: theme.colors.accent },
      choiceText: { color: theme.colors.foreground, fontSize: 12 },
      chosenText: { color: theme.colors.accentForeground, fontSize: 12 },
      button: { paddingVertical: 6, paddingHorizontal: 10, borderRadius: 6, backgroundColor: theme.colors.accent },
      disabled: { paddingVertical: 6, paddingHorizontal: 10, borderRadius: 6, backgroundColor: theme.colors.surface2 },
      buttonText: { color: theme.colors.accentForeground, fontSize: 12 },
      muted: { color: theme.colors.foregroundMuted, fontSize: 12 },
      error: { color: theme.colors.statusDanger, fontSize: 12 },
      ok: { color: theme.colors.statusSuccess, fontSize: 12 },
    }),
    [theme],
  );
  const validAccount = accountPattern.test(account) && account !== "auto";
  const busy = terminal.isPending || direct.isPending;
  const error = terminal.error ?? direct.error;

  function Button({ label, enabled, onPress }: { label: string; enabled: boolean; onPress: () => void }) {
    return (
      <Pressable accessibilityRole="button" accessibilityLabel={label} disabled={!enabled || busy} style={enabled && !busy ? styles.button : styles.disabled} onPress={onPress}>
        <Text style={styles.buttonText}>{label}</Text>
      </Pressable>
    );
  }

  return (
    <View style={styles.form}>
      <View style={styles.row}>
        {(["claude", "codex"] as const).map((option) => (
          <Pressable
            key={option}
            accessibilityRole="button"
            accessibilityState={{ selected: service === option }}
            style={service === option ? styles.chosen : styles.choice}
            onPress={() => {
              setService(option);
              setNotice(null);
            }}
          >
            <Text style={service === option ? styles.chosenText : styles.choiceText}>{option === "claude" ? "Claude" : "Codex"}</Text>
          </Pressable>
        ))}
      </View>
      <Text style={styles.label}>Account name</Text>
      <TextInput
        accessibilityLabel="Account name"
        style={styles.input}
        value={account}
        onChangeText={setAccount}
        autoCapitalize="none"
        autoCorrect={false}
        placeholder="work"
      />
      {workspaceId ? (
        <Button label="Sign in in a terminal" enabled={validAccount} onPress={() => terminal.mutate({ workspaceId, service, account })} />
      ) : (
        <Text style={styles.muted}>To sign in, open a workspace and use the gauge button in its header → Add account.</Text>
      )}
      {service === "claude" ? (
        <>
          <Text style={styles.label}>Or paste a setup token from `claude setup-token`</Text>
          <TextInput
            accessibilityLabel="Claude setup token"
            style={styles.input}
            value={token}
            onChangeText={setToken}
            secureTextEntry
            autoCapitalize="none"
            autoCorrect={false}
            placeholder="sk-ant-oat01-…"
          />
          <Button label="Add with token" enabled={validAccount && token.trim().startsWith("sk-ant-")} onPress={() => direct.mutate({ account, token: token.trim() })} />
        </>
      ) : null}
      {error ? <Text style={styles.error}>{String(error.message ?? error)}</Text> : null}
      {notice ? <Text style={styles.ok}>{notice}</Text> : null}
    </View>
  );
}

// AddAccountPopover is the header button's "Add account" page.
export function AddAccountPopover({ theme, workspaceId }: PluginButtonContentProps) {
  return <AddAccountForm theme={theme} workspaceId={workspaceId} />;
}
