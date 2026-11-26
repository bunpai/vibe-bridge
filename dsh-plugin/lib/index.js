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
    parameters: z.object({
      query: z.string().describe('Search query in natural language'),
      search_context_size: z.string().optional().describe('Optional connector hint, e.g. "low" | "medium" | "high"'),
    }),
    timeoutMs: config.timeout_ms,
    async execute(args, exec) {
      const toolArgs = { query: String(args.query ?? '') }
      if (typeof args.search_context_size === 'string' && args.search_context_size) toolArgs.search_context_size = args.search_context_size
      return connectorToolCall({
        baseUrl: config.base_url, apiKey: config.api_key, model: config.model,
        tool: 'web_search', args: toolArgs,
        timeoutMs: config.timeout_ms, signal: exec.signal,
      })
    },
  }))

  ctx.tools.register(defineTool({
    name: 'vibe_connector_call',
    description:
      'Call any Mistral connector tool through the vibe-bridge gateway, e.g. tool="news_search" | "weather_search" | "finance_search" | "open_url" | "web_search". '
      + 'args_json is a JSON object string matching that tool\'s schema.',
    parameters: z.object({
      tool: z.string().describe('Connector tool name, e.g. "web_search" | "news_search" | "weather_search" | "finance_search" | "open_url"'),
      args_json: z.string().describe('JSON object string with the tool arguments, e.g. "{\"query\":\"A股\"}"'),
    }),
    timeoutMs: config.timeout_ms,
    async execute(args, exec) {
      let toolArgs = {}
      const raw = String(args.args_json ?? '').trim()
      if (raw) {
        try { toolArgs = JSON.parse(raw) } catch (e) { throw new Error('vibe_connector_call: args_json is not valid JSON: ' + e.message) }
      }
      return connectorToolCall({
        baseUrl: config.base_url, apiKey: config.api_key, model: config.model,
        tool: String(args.tool ?? ''), args: toolArgs,
        timeoutMs: config.timeout_ms, signal: exec.signal,
      })
    },
  }))
}
