# subswapper for Paseo

Shows the usage of every Claude and ChatGPT subscription that
[subswapper](https://github.com/lawzava/subswapper) manages, and switches
accounts from inside Paseo.

- A **usage button** in every workspace header, e.g. `claude 41% · codex 1%`,
  with a menu to switch a service to its best account.
- A **Subscriptions** screen in the sidebar: each account's five-hour, weekly,
  and Fable windows with reset times, its state, and **Use** buttons.
- **Add account** in the header menu and on the screen: signs a new Claude or
  Codex account in through a terminal in the workspace (Codex uses a device
  code, so any device can approve it), or adds a Claude setup token you
  already have without it appearing in a terminal.
- Command Center items to open the screen and to switch every service to its
  best account.
- **Connect Codex to Subswapper** on the Subscriptions screen points every
  Codex provider that launches subswapper at a `codex` shim, or adds one. Paseo
  then sees the real Codex version and turns on Auto-review and Goals, while
  subswapper still picks the account. Reload existing Codex sessions after
  connecting. Needs a subswapper version with `launch-config`.
- **Usage cards and sidebar bars** (Paseo 0.11 and later): `Claude
  (Subswapper)` and `Codex (Subswapper)` cards, one per service, showing the
  account the proxy uses now. Its five-hour and weekly windows go to the
  sidebar summary, labeled with the account name, e.g. `14% 5h 6% wk · foxy2`.
  The Subscriptions screen lists every account; Paseo pins sidebar windows per
  source, so per-account cards would repeat there.
- Sessions whose provider launches subswapper get `SUBSWAPPER_SERVICE` in
  their environment. Codex sessions also get `OPENAI_BASE_URL` set to OpenAI's
  default, which stops Paseo's builtin Codex card from sending subswapper's
  proxy placeholder to ChatGPT and reporting `Login rejected (HTTP 401)`.
  Reload sessions that were open before the plugin was updated. Paseo's Usage
  screen still shows that builtin card for `~/.codex/auth.json`, because the
  builtin source reads that file for every daemon.

The plugin runs `subswapper status -json` and `subswapper switch` on the
daemon host, so it works the same on a standalone machine, a hub, and a hub
client.

## Install

1. Install subswapper on the Paseo daemon host and set it up
   (`subswapper setup local`, `setup hub`, or `setup client <hub>`).
2. Turn on **Settings → Plugins → Enable plugins** for that daemon.
3. Install the plugin:

   ```sh
   paseo plugin install lawzava/subswapper:paseo-plugin
   ```

The plugin finds `subswapper` through `SUBSWAPPER_BIN`, the daemon's `PATH`,
`~/go/bin`, or `~/.local/bin`.

## Develop

```sh
npm install
npm run typecheck
npm test
paseo plugin install "$PWD"
paseo plugin reload subswapper
```
