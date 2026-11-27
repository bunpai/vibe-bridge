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
  provider: z.string().default(''),
  // Empty = inherit from the selected provider.
  base_url: z.string().default(''),
  api_key: z.string().default(''),
  model: z.string().default(''),
  timeout_ms: z.number().default(60000),
})

/**
 * Resolve gateway connection: explicit config wins, empty fields fall back to
 * the profile's llm-pi-ai 'cpa' provider (baseURL + apiKeyEnv/apiKey + first model).
 */
function resolveGateway(ctx, config) {
  let { base_url, api_key, model } = config
  if (base_url && api_key && model) return { baseUrl: base_url.replace(/\/+$/, '').replace(/\/v1$/, ''), apiKey: api_key, model }
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
      if (!model && Array.isArray(cpa.models) && cpa.models.length) model = cpa.models[0].id ?? cpa.models[0].name
    }
  } catch { /* fall through to config-only values */ }
  return { baseUrl: String(base_url ?? '').replace(/\/+$/, '').replace(/\/v1$/, ''), apiKey: api_key ?? '', model: model ?? '' }
}

export function apply(ctx, rawConfig) {
  const config = { provider: '', base_url: '', api_key: '', model: '', timeout_ms: 60000, ...(rawConfig ?? {}) }

  ctx.tools.register(defineTool({
    name: 'vibe_web_search',
    description:
      'Search the live web through Mistral cloud connectors (runs inside the vibe-bridge CPA gateway, billed to the Vibe quota). '
      + 'Prefer this over web_search when you need current news, weather or finance snippets from the Mistral connector network.',
    parameters: {
      query: { type: 'string', required: true, description: 'Search query in natural language' },
      search_context_size: { type: 'string', description: 'Optional connector hint, e.g. "low" | "medium" | "high"' },
    },
    timeoutMs: config.timeout_ms,
    output: {
      schema: { type: 'object', additionalProperties: false, properties: { text: { type: 'string', required: true } } },
      render: (_args, value) => [{ type: 'text', text: value.text }],
    },
    async execute(args, exec) {
      const toolArgs = { query: String(args.query ?? '') }
      if (typeof args.search_context_size === 'string' && args.search_context_size) toolArgs.search_context_size = args.search_context_size
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
