# dsh-vibe-search

DSH cordis plugin exposing Mistral cloud connectors (web/news/weather/finance
search) as agent tools through a [vibe-bridge](https://github.com/bunpai/vibe-bridge)-enabled
CLIProxyAPI gateway. Billing rides the Vibe quota like the official Vibe CLI.

## Tools

| tool | args | 说明 |
|---|---|---|
| `vibe_web_search` | `query`, optional `search_context_size` | 直连 web_search connector，无 LLM 往返 |
| `vibe_connector_call` | `tool`, `args_json` | 任意 connector 工具透传（news_search / weather_search / finance_search / open_url） |

## Install

1. Gateway side: install vibe-bridge into CPA (store source
   `https://raw.githubusercontent.com/bunpai/vibe-bridge/main/store/registry.json`),
   keep `connector_mode: directive` (default) or `loop`.
2. This plugin: place `dsh-plugin/` as `dsh-vibe-search` into the profile's
   `node_modules` (or `pnpm add ./dsh-plugin`), add `"dsh-vibe-search"` to
   `dsh.profile.bundles` in the profile package.json — its cordis.patch.yml
   registers the host-plane tools.
3. Web sessions: add `- { id: vibe-search, name: 'dsh-vibe-search' }` to the
   agent preset's plugin list (same as dsh-tgrep).
4. Configure: `base_url` (your CPA gateway), `api_key` (CPA client key),
   `model` (any mapped model — the request only carries the directive).
