/**
 * Pure HTTP client for the vibe-bridge connector shell: sends an ordinary
 * chat-completions request carrying x_vibe_connector / x_vibe_connectors
 * extension fields through a CLIProxyAPI gateway running the vibe-bridge
 * plugin. The gateway's client-key auth applies; no management key is needed.
 *
 * Requires the vibe-bridge plugin's connector_mode to be "directive" (default)
 * or "loop" for tool injection.
 */

/** POST /v1/chat/completions with connector extension fields. Returns the raw JSON. */
export async function connectorChat({ baseUrl, apiKey, model, fields, messages, timeoutMs = 60000, signal }) {
  const base = String(baseUrl ?? '').replace(/\/+$/, '')
  if (!base) throw new Error('vibe-search: base_url is not configured')
  const body = {
    model: model || 'zai-glm-5-3-flash',
    stream: false,
    ...fields,
    messages: messages ?? [{ role: 'user', content: 'go' }],
  }
  const ctl = new AbortController()
  const timer = setTimeout(() => ctl.abort(new Error('vibe-search: request timed out')), timeoutMs)
  if (signal) {
    if (signal.aborted) ctl.abort(signal.reason)
    else signal.addEventListener('abort', () => ctl.abort(signal.reason), { once: true })
  }
  let res
  try {
    res = await fetch(base + '/v1/chat/completions', {
      method: 'POST',
      headers: {
        Authorization: 'Bearer ' + (apiKey || ''),
        'Content-Type': 'application/json',
      },
      body: JSON.stringify(body),
      signal: ctl.signal,
    })
  } finally {
    clearTimeout(timer)
  }
  const text = await res.text()
  let json
  try { json = JSON.parse(text) } catch { json = { raw: text } }
  if (!res.ok) {
    const msg = json?.error?.message ?? text.slice(0, 400)
    throw new Error('vibe-search: gateway ' + res.status + ': ' + msg)
  }
  return json
}

/**
 * Execute one connector tool through the gateway (connector_mode: directive).
 * Returns the MCP result text produced by vibe-bridge (no LLM round-trip).
 */
export async function connectorToolCall({ baseUrl, apiKey, model, tool, args, timeoutMs, signal }) {
  const json = await connectorChat({
    baseUrl, apiKey, model, timeoutMs, signal,
    fields: { x_vibe_connector: { tool, args: args ?? {} } },
  })
  const msg = json?.choices?.[0]?.message
  return typeof msg?.content === 'string' ? msg.content : JSON.stringify(json)
}

export const name = 'dsh-vibe-search'
