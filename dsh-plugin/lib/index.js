/**
 * dsh-vibe-search — exposes Mistral cloud connectors (web/news/weather/finance
 * search) as DSH tools through a vibe-bridge-enabled CLIProxyAPI gateway.
 *
 * Two tools:
 * - vibe_web_search: quick web search (query in, results text out).
 * - vibe_connector_call: generic passthrough to any connector tool
 *   (e.g. "web_search.news_search") with a JSON arguments string.
 */
import { defineTool } from '@deepseek-ai/dsh-tools'
import z from '@deepseek-ai/schemastery'

import { connectorToolCall, name } from './search.js'

export { name }

/** cordis service injections this plugin consumes. */
export const inject = ['tools']

export const Config = z.object({
  // Provider name inside llm-pi-ai config to inherit the endpoint from.
  // Empty = 'cpa' (falls back to the first provider when absent).
  provider: z.string().default('').volatile(),
  // Empty = inherit from the selected provider.
  base_url: z.string().default('').volatile(),
  api_key: z.string().default('').volatile(),
  model: z.string().default('').volatile(),
  timeout_ms: z.number().default(60000).volatile(),
})

const SEARCH_MODEL = 'vibe-search'

/**
 * Resolve gateway connection: explicit config wins, empty base_url/api_key
 * fall back to the profile's llm-pi-ai 'cpa' provider (baseURL + apiKeyEnv/
 * apiKey). An empty model defaults to the gateway's virtual 'vibe-search'
 * model, which auto-loops connector searches; set an explicit backing model
 * (e.g. zai-glm-5-3-flash) only as an escape hatch for gateways where the
 * virtual model is unavailable (connector mode off / plugin older than v0.1.3).
 */
function resolveGateway(ctx, config) {
  let { base_url, api_key, model } = config
  if (!model) model = SEARCH_MODEL
  if (base_url && api_key) return { baseUrl: base_url.replace(/\/+$/, '').replace(/\/v1$/, ''), apiKey: api_key, model }
  try {
    const editor = ctx.get('configEditor', false)
    const rows = editor?.configuration?.() ?? []
    const row = rows.find((r) => r?.entry?.options?.id === 'llm-pi-ai')
    const merged = { ...(row?.inherited ?? {}), ...(row?.override ?? {}) }
    const providers = merged?.providers ?? {}
    const names = Object.keys(providers)
    const want = config.provider || 'cpa'
    const cpa = providers[want] ?? providers[names[0]]
    if (cpa) {
      if (!base_url && cpa.baseURL) base_url = String(cpa.baseURL).replace(/\/+$/, '').replace(/\/v1$/, '')
      if (!api_key) {
        if (cpa.apiKey) api_key = cpa.apiKey
        else if (cpa.apiKeyEnv && process.env[cpa.apiKeyEnv]) api_key = process.env[cpa.apiKeyEnv]
      }
    }
  } catch { /* fall through to config-only values */ }
  return { baseUrl: String(base_url ?? '').replace(/\/+$/, '').replace(/\/v1$/, ''), apiKey: api_key ?? '', model: model ?? '' }
}

function unbox(v) { return v !== null && typeof v === 'object' && typeof v.get === 'function' ? v.get() : v }

export function apply(ctx, rawConfig) {
  const merged = { provider: '', base_url: '', api_key: '', model: '', timeout_ms: 60000, ...(rawConfig ?? {}) }
  const config = Object.fromEntries(Object.entries(merged).map(([k, v]) => [k, unbox(v)]))

  ctx.tools.register(defineTool({
    name: 'vibe_web_search',
    description:
      'Search the live web through Mistral cloud connectors (runs inside the vibe-bridge CPA gateway, billed to the Vibe quota). '
      + 'Prefer this over web_search when you need current news, weather or finance snippets from the Mistral connector network.',
    parameters: {
      query: { type: 'string', required: true, description: 'Search query in keyword form' },
      start_date: { type: 'string', description: 'Optional: only results newer than this date, format YYYY-MM-DD' },
      end_date: { type: 'string', description: 'Optional: only results older than this date, format YYYY-MM-DD' },
    },
    timeoutMs: config.timeout_ms,
    output: {
      schema: { type: 'object', additionalProperties: false, properties: { text: { type: 'string', required: true } } },
      render: (_args, value) => [{ type: 'text', text: value.text }],
    },
    async execute(args, exec) {
      const toolArgs = { query: String(args.query ?? '') }
      // Only forward arguments the Mistral MCP web_search tool actually accepts
      // (query/start_date/end_date); unknown keys make the call fail validation.
      for (const k of ['start_date', 'end_date']) {
        if (typeof args[k] === 'string' && args[k]) toolArgs[k] = args[k]
      }
      const gw = resolveGateway(ctx, config)
      const text = await connectorToolCall({
        baseUrl: gw.baseUrl, apiKey: gw.apiKey, model: gw.model,
        tool: 'web_search', args: toolArgs,
        timeoutMs: config.timeout_ms, signal: exec.signal,
      })
      return { text }
    },
  }))

  ctx.tools.register(defineTool({
    name: 'vibe_connector_call',
    description:
      'Call any Mistral connector tool through the vibe-bridge gateway, e.g. tool="news_search" | "weather_search" | "finance_search" | "open_url" | "web_search". '
      + 'args_json is a JSON object string matching that tool\'s schema.',
    parameters: {
      tool: { type: 'string', required: true, description: 'Connector tool name, e.g. "web_search" | "news_search" | "weather_search" | "finance_search" | "open_url"' },
      args_json: { type: 'string', description: 'JSON object string with the tool arguments, e.g. "{\"query\":\"A股\"}"' },
    },
    timeoutMs: config.timeout_ms,
    output: {
      schema: { type: 'object', additionalProperties: false, properties: { text: { type: 'string', required: true } } },
      render: (_args, value) => [{ type: 'text', text: value.text }],
    },
    async execute(args, exec) {
      let toolArgs = {}
      const raw = String(args.args_json ?? '').trim()
      if (raw) {
        try { toolArgs = JSON.parse(raw) } catch (e) { throw new Error('vibe_connector_call: args_json is not valid JSON: ' + e.message) }
      }
      // Small models often pass the tool arguments as top-level keys instead of
      // nesting them in args_json — adopt any extras so the call still works.
      if (!Object.keys(toolArgs).length) {
        for (const [k, v] of Object.entries(args)) {
          if (k !== 'tool' && k !== 'args_json' && v !== undefined && v !== null && v !== '') toolArgs[k] = v
        }
      }
      const gw = resolveGateway(ctx, config)
      const text = await connectorToolCall({
        baseUrl: gw.baseUrl, apiKey: gw.apiKey, model: gw.model,
        tool: String(args.tool ?? ''), args: toolArgs,
        timeoutMs: config.timeout_ms, signal: exec.signal,
      })
      return { text }
    },
  }))
}
