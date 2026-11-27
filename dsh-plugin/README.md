# dsh-vibe-search

DSH cordis plugin exposing Mistral cloud connectors (web/news/weather/finance
search) as agent tools through a [vibe-bridge](https://github.com/bunpai/vibe-bridge)-enabled
CLIProxyAPI gateway. Billing rides the Vibe quota like the official Vibe CLI.

## Tools

| tool | args | 说明 |
|---|---|---|
| `vibe_web_search` | `query`, optional `search_context_size` | 直连 web_search connector，无 LLM 往返 |
| `vibe_connector_call` | `tool`, `args_json` | 任意 connector 工具透传（news_search / weather_search / finance_search / open_url） |

## cordis plugin API contract (verified)

- `apply(ctx, config)` must not touch `ctx.tools` unless the module also
  exports `inject = ['tools']` — otherwise activation fails with
  `cannot get property "tools" without inject`.
- `defineTool` (`@deepseek-ai/dsh-tools`) takes `parameters` as a plain
  literal map `{ name: { type, required?, description } }` — NOT a schemastery
  `z.object` (there is no `.describe()`).
- Every tool needs `output: { schema, render(args, value) }`; render returns
  `[{ type: 'text', text }]` and `execute` returns the value matching schema.
- Host deps (`@deepseek-ai/dsh-tools`, `@deepseek-ai/schemastery`) resolve
  from the dsh install. For a linked dev copy, symlink them into the plugin's
  own `node_modules` (not committed): see the DSH_HOME recipe below.
- `package.json` needs `"dsh": { "bundle": { "patch": "./cordis.patch.yml" } }`
  or it installs as a plain dependency with no patch mounted.

## Install

1. Gateway side: install vibe-bridge into CPA (store source
   `https://raw.githubusercontent.com/bunpai/vibe-bridge/main/store/registry.json`),
   keep `connector_mode: directive` (default) or `loop`.
2. This plugin: `dsh plugin --profile <name> add /path/to/dsh-plugin`, then add
   `"dsh-vibe-search"` to `dsh.profile.bundles` in the profile package.json —
   its cordis.patch.yml registers the host-plane tools.
3. Web sessions: add `- { id: vibe-search, name: 'dsh-vibe-search' }` to the
   agent preset's plugin list (same as dsh-tgrep).
4. Configure via the profile's cordis.patch.yml:

   ```yaml
   - id: dsh-vibe-search
     config:
       base_url: https://your-cpa-gateway
       api_key: <CPA client key>
       model: zai-glm-5-3-flash   # any mapped model; the request only carries the directive
   ```

## Self-contained test profile (workspace DSH_HOME)

Everything lands inside the workspace — no writes to ~/.dsh:

```bash
export DSH_HOME=/path/to/workspace/dsh-home
dsh testrun --from-default-profile headless          # creates the profile, then exits on missing task
dsh plugin --profile testrun add /path/to/dsh-plugin # links the plugin
# + append "dsh-vibe-search" to bundles, write cordis.patch.yml (plugin config + llm provider)
# + symlink host deps for the linked copy:
mkdir -p dsh-plugin/node_modules/@deepseek-ai
ln -sfn <dsh-install>/node_modules/@deepseek-ai/dsh-tools    dsh-plugin/node_modules/@deepseek-ai/dsh-tools
ln -sfn <dsh-install>/node_modules/@deepseek-ai/schemastery  dsh-plugin/node_modules/@deepseek-ai/schemastery
CPA_API_KEY=<key> dsh testrun "call vibe_web_search ..."
```

Verified end-to-end: `dsh testrun` agent called `vibe_web_search("今日科技新闻")`
through CPA :18317 → vibe-bridge directive → Mistral connectors MCP → Brave,
and received real structuredContent results.
