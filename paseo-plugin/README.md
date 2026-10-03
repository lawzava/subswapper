# subswapper for Paseo

Shows the usage of every Claude and ChatGPT subscription that
[subswapper](https://github.com/lawzava/subswapper) manages, and switches
accounts from inside Paseo.

- A **usage button** in every workspace header, e.g. `claude 41% · codex 1%`,
  with a menu to switch a service to its best account.
- A **Subscriptions** screen in the sidebar: each account's five-hour, weekly,
  and Fable windows with reset times, its state, and **Use** buttons.
- Command Center items to open the screen and to switch every service to its
  best account.

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
