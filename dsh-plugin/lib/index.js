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
  base_url: z.string().default('https://ai.8823823.xyz'),
  api_key: z.string().default(''),
  model: z.string().default('zai-glm-5-3-flash'),
  timeout_ms: z.number().default(60000),
})

export function apply(ctx, rawConfig) {
  const config = { base_url: 'https://ai.8823823.xyz', api_key: '', model: 'zai-glm-5-3-flash', timeout_ms: 60000, ...(rawConfig ?? {}) }

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
      const text = await connectorToolCall({
        baseUrl: config.base_url, apiKey: config.api_key, model: config.model,
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
      const text = await connectorToolCall({
        baseUrl: config.base_url, apiKey: config.api_key, model: config.model,
        tool: String(args.tool ?? ''), args: toolArgs,
        timeoutMs: config.timeout_ms, signal: exec.signal,
      })
      return { text }
    },
  }))
}
