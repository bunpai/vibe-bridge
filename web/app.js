(() => {
  const $ = id => document.getElementById(id);
  const apiRoot = "/v0/management/plugins/vibe-bridge";
  const sessionKeyName = "vibe-bridge-management-key";
  const panelStorageKey = "cli-proxy-auth";
  const storagePrefixes = ["enc::v1::", "enc::v2::"];
  const storageSalt = "cli-proxy-api-webui::secure-storage";
  let sessionKeySuppressed = false;
  let managementKey = readSessionKey() || readPanelKey();
  let snapshot = { keys: [], catalog: [], models: [], debug_mode: false, proxy_url: "" };
  let range = "all";
  let page = 1;
  let totalPages = 1;
  let logsController;
  let toastTimer;
  let searchTimer;
  let publishing = false;
  let debugExchange;
  let debugPane = "request";
  const selectedModels = new Set();
  const refreshingKeys = new Set();
  const switchingKeys = new Set();
      function readSessionKey() {
        if (sessionKeySuppressed) return "";
        try { return sessionStorage.getItem(sessionKeyName) || ""; }
        catch { return ""; }
      }

      function decodePanelStorage(value) {
        const prefix = storagePrefixes.find(candidate => value.startsWith(candidate));
        if (!prefix) return value;
        try {
          const binary = atob(value.slice(prefix.length));
          const bytes = Uint8Array.from(binary, character => character.charCodeAt(0));
          const keyMaterial = prefix === "enc::v1::"
            ? `${storageSalt}|${window.location.host}|${navigator.userAgent}`
            : `${storageSalt}|v2|${window.location.host}`;
          const salt = new TextEncoder().encode(keyMaterial);
          for (let index = 0; index < bytes.length; index += 1) bytes[index] ^= salt[index % salt.length];
          return new TextDecoder().decode(bytes);
        } catch { return ""; }
      }

      function readPanelKey() {
        try {
          if (window.self === window.top) return "";
          const stored = localStorage.getItem(panelStorageKey);
          if (!stored) return "";
          const parsed = JSON.parse(decodePanelStorage(stored));
          const state = parsed?.state || parsed || {};
          return typeof state.managementKey === "string" ? state.managementKey : "";
        } catch { return ""; }
      }

      function storeSessionKey(value) {
        try {
          sessionStorage.setItem(sessionKeyName, value);
          sessionKeySuppressed = false;
          return true;
        } catch { return false; }
      }

      function clearSessionKey() {
        sessionKeySuppressed = true;
        try { sessionStorage.removeItem(sessionKeyName); }
        catch {}
      }


  function escapeHTML(value) {
    return String(value ?? "").replace(/[&<>"']/g, character => ({ "&": "&amp;", "<": "&lt;", ">": "&gt;", '"': "&quot;", "'": "&#39;" })[character]);
  }
  const number = value => Number(value || 0).toLocaleString("zh-CN");
  const duration = value => value == null ? "—" : `${(Number(value) / 1000).toFixed(2)} s`;
  const dateText = value => value ? new Date(value).toLocaleString("zh-CN", { hour12: false }) : "—";
  function notify(message, error = false) {
    $("toast").textContent = message;
    $("toast").className = `toast${error ? " error-text" : ""}`;
    clearTimeout(toastTimer);
    toastTimer = setTimeout(() => $("toast").classList.add("hidden"), error ? 6000 : 2800);
  }
  function showError(error) {
    if (error.name === "AbortError") return;
    $("pageError").textContent = error.message;
    $("pageError").classList.remove("hidden");
  }
  async function request(path, options = {}) {
    const headers = new Headers(options.headers);
    headers.set("Authorization", `Bearer ${managementKey}`);
    if (options.body) headers.set("Content-Type", "application/json");
    const response = await fetch(path, { ...options, headers });
    const data = await response.json().catch(() => ({}));
    if (response.status === 401 || response.status === 403) {
      clearSessionKey(); managementKey = ""; showAuth();
    }
    if (!response.ok) throw new Error(data.error || data.message || `请求失败（HTTP ${response.status}）`);
    return data;
  }
  const api = (path, options) => request(`${apiRoot}${path}`, options);
  const jsonOptions = (method, body) => ({ method, body: JSON.stringify(body) });
  async function busy(button, action) {
    if (button.disabled) return;
    button.disabled = true; button.setAttribute("aria-busy", "true");
    try { await action(); }
    catch (error) { if (error.name !== "AbortError") notify(error.message, true); }
    finally { button.disabled = false; button.removeAttribute("aria-busy"); }
  }
  function showAuth() {
    $("authGate").classList.remove("hidden"); $("app").classList.add("hidden");
  }
  async function connect() {
    try {
      await loadState();
      $("authGate").classList.add("hidden"); $("app").classList.remove("hidden");
    } catch (error) { $("authError").textContent = error.message; showAuth(); return; }
    loadLogs().catch(showError);
  }
  async function loadState() {
    snapshot = await api("/state");
    snapshot.keys ||= []; snapshot.models ||= []; snapshot.catalog ||= [];
    snapshot.debug_mode = Boolean(snapshot.debug_mode);
    snapshot.proxy_url ||= "";
    renderProxy();
    $("debugModeToggle").setAttribute("aria-checked", String(snapshot.debug_mode));
    renderKeys(); renderModels();
    $("catalogOptions").innerHTML = snapshot.catalog.map(model => `<option value="${escapeHTML(model)}"></option>`).join("");
    const keyValue = $("mappingKey").value;
    $("mappingKey").innerHTML = '<option value="">按优先级自动选择</option>' + snapshot.keys.map(key => `<option value="${escapeHTML(key.id)}">${escapeHTML(key.label)} · ${escapeHTML(key.masked)}</option>`).join("");
    $("mappingKey").value = keyValue;
    $("publishNotice").classList.toggle("hidden", snapshot.models_revision === snapshot.published_revision);
  }
  function showView(name) {
    document.querySelectorAll("[data-page]").forEach(element => { element.hidden = element.dataset.page !== name; });
    document.querySelectorAll("[data-view]").forEach(element => {
      const selected = element.dataset.view === name;
      element.setAttribute("aria-selected", String(selected)); element.tabIndex = selected ? 0 : -1;
    });
    if (name === "requests") loadLogs().catch(showError);
    if (name === "keys") refreshQuotas(false);
  }
  function renderProxy() {
    // Background quota refreshes must not overwrite an unsaved proxy edit.
    if (!$("proxyForm").contains(document.activeElement) && $("proxyUrl").dataset.dirty !== "true") {
      $("proxyUrl").value = snapshot.proxy_url;
    }
    $("proxyStatus").textContent = snapshot.proxy_url ? "独立代理已配置" : "沿用 CPA 配置";
    $("proxyStatus").classList.toggle("success", Boolean(snapshot.proxy_url));
  }
  async function saveProxy(proxyUrl) {
    const controls = [...$("proxyForm").querySelectorAll("button, input")];
    controls.forEach(control => { control.disabled = true; });
    $("proxyForm").setAttribute("aria-busy", "true");
    $("proxyError").textContent = "";
    try {
      const result = await api("/proxy", jsonOptions("PATCH", { proxy_url: proxyUrl }));
      snapshot.proxy_url = result.proxy_url || "";
      $("proxyUrl").value = snapshot.proxy_url;
      delete $("proxyUrl").dataset.dirty;
      renderProxy();
      notify(snapshot.proxy_url ? "独立代理已保存，新请求将使用此代理" : "已恢复沿用 CPA 的代理配置");
    } catch (error) { $("proxyError").textContent = error.message; }
    finally {
      controls.forEach(control => { control.disabled = false; });
      $("proxyForm").removeAttribute("aria-busy");
    }
  }
  function logQuery() {
    const query = new URLSearchParams({ page: String(page), page_size: $("pageSize").value });
    for (const [id, name] of [["logSearch", "search"], ["logStatus", "status"], ["logModel", "model"], ["logKey", "key_id"], ["logThinking", "thinking"]]) {
      if ($(id).value.trim()) query.set(name, $(id).value.trim());
    }
    const now = new Date();
    if (range === "today") { const start = new Date(now); start.setHours(0, 0, 0, 0); query.set("from", start.toISOString()); }
    if (["24h", "7d", "30d"].includes(range)) {
      const days = { "24h": 1, "7d": 7, "30d": 30 }[range];
      query.set("from", new Date(now.getTime() - days * 86400000).toISOString());
    }
    if (range === "custom") {
      if (!$("rangeFrom").value || !$("rangeTo").value) throw new Error("请选择完整的起止时间");
      const from = new Date($("rangeFrom").value), to = new Date($("rangeTo").value);
      if (from > to) throw new Error("结束时间不能早于起始时间");
      query.set("from", from.toISOString()); query.set("to", new Date(to.getTime() + 60000).toISOString());
    }
    return query;
  }
  function setOptions(id, items, placeholder) {
    const value = $(id).value;
    $(id).innerHTML = `<option value="">${placeholder}</option>` + items.map(([key, label]) => `<option value="${escapeHTML(key)}">${escapeHTML(label)}</option>`).join("");
    if (items.some(([key]) => key === value)) $(id).value = value;
  }
  async function loadLogs() {
    const query = logQuery();
    logsController?.abort();
    const controller = new AbortController(); logsController = controller;
    const data = await api(`/logs?${query}`, { signal: controller.signal });
    if (controller.signal.aborted) return;
    $("pageError").classList.add("hidden");
    const stats = data.stats;
    $("statRequests").textContent = number(stats.requests);
    $("statTokens").textContent = number(stats.total_tokens);
    $("statUsage").textContent = `输入 ${number(stats.input_tokens)} · 输出 ${number(stats.output_tokens)} · 缓存 ${stats.cache_samples ? number(stats.cached_tokens) : "—"}`;
    $("statCache").textContent = stats.cache_rate == null ? "—" : `${stats.cache_rate.toFixed(1)}%`;
    $("statSpeed").textContent = stats.token_rate == null ? "—" : `${stats.token_rate.toFixed(1)} t/s`;
    $("cacheSamples").textContent = stats.requests ? `${stats.cache_samples} 条请求返回缓存用量` : "";
    $("speedSamples").textContent = stats.requests ? `${stats.speed_samples} 条流式请求` : "";
    $("logCount").textContent = `${number(data.total)} 条`;
    $("logRetention").textContent = `已保留 ${number(data.retained)} 条 · 最多 ${number(data.retention_limit)} 条`;
    page = data.page; totalPages = data.pages;
    $("pageInfo").textContent = `第 ${page} 页，共 ${totalPages} 页`;
    $("prevPage").disabled = page <= 1; $("nextPage").disabled = page >= totalPages;
    const keys = new Map(Object.entries(data.keys || {}));
    snapshot.keys.forEach(key => keys.set(key.id, key.label));
    setOptions("logKey", [...keys], "全部凭证");
    setOptions("logModel", Object.keys(data.models || {}).sort().map(model => [model, model]), "全部模型");
    $("logRows").innerHTML = data.logs.length ? data.logs.map(log => `<tr>
      <td class="nowrap">${escapeHTML(new Date(log.created_at).toLocaleDateString("zh-CN"))}<div class="sub">${escapeHTML(new Date(log.created_at).toLocaleTimeString("zh-CN", {hour12:false}))}${log.stream ? " · 流式" : ""}</div></td>
      <td class="mono model-cell">${escapeHTML(log.model)}</td><td class="mono model-cell">${escapeHTML(log.upstream_model || "—")}</td>
      <td><span class="badge">${escapeHTML(log.thinking || "默认")}</span></td>
      <td><span class="badge ${log.status === "error" ? "error" : "success"}">${log.status === "error" ? "失败" : "成功"}${log.http_status ? ` · ${Number(log.http_status)}` : ""}</span>${log.error ? `<details class="log-error"><summary>错误详情</summary>${escapeHTML(log.error)}</details>` : ""}</td>
      <td>${escapeHTML(log.key_label || "—")}<div class="sub mono">${escapeHTML(log.key_masked || "")}</div></td>
      <td class="nowrap">${duration(log.first_token_ms)}</td><td class="nowrap">${duration(log.duration_ms)}</td>
      <td class="nowrap">${log.usage_known || log.input_tokens || log.output_tokens ? `${number(log.total_tokens || (log.input_tokens || 0) + (log.output_tokens || 0))}<div class="sub">输入 ${number(log.input_tokens)} · 输出 ${number(log.output_tokens)}</div><div class="sub">缓存 ${log.cached_tokens == null ? "—" : number(log.cached_tokens)}</div>` : "—"}</td>
      <td>${log.debug_available ? `<button class="btn small" type="button" data-debug-log="${escapeHTML(log.id)}">查看</button>` : "—"}</td>
    </tr>`).join("") : '<tr><td colspan="10" class="empty"><strong>没有匹配的请求</strong>调整筛选条件或稍后刷新</td></tr>';
  }
  function formatDebugBody(body) {
    if (!body) return "（空响应体）";
    try { return JSON.stringify(JSON.parse(body), null, 2); }
    catch { return body; }
  }
  function debugText() {
    if (!debugExchange) return "";
    if (debugPane === "request") {
      const request = debugExchange.request || {};
      const headers = Object.entries(request.headers || {}).map(([name, values]) => `${name}: ${values.join(", ")}`).join("\n");
      return [`${request.method || ""} ${request.url || ""}`.trim(), headers, formatDebugBody(request.body)].filter(Boolean).join("\n\n");
    }
    const response = debugExchange.response || {};
    const headers = Object.entries(response.headers || {}).map(([name, values]) => `${name}: ${values.join(", ")}`).join("\n");
    const state = response.complete ? "响应完整" : "响应不完整";
    const lines = [`HTTP ${response.status_code || "—"} · ${state}`, headers, formatDebugBody(response.body)];
    if (response.error) lines.splice(2, 0, `读取错误：${response.error}`);
    return lines.filter(Boolean).join("\n\n");
  }
  function renderDebugPane() {
    document.querySelectorAll("[data-debug-pane]").forEach(button => button.setAttribute("aria-pressed", String(button.dataset.debugPane === debugPane)));
    $("debugOutput").textContent = debugText();
  }
  async function openDebugLog(id) {
    debugExchange = await api(`/logs/debug?id=${encodeURIComponent(id)}`);
    debugPane = "request";
    $("debugSummary").textContent = `${dateText(debugExchange.created_at)} · ${duration(debugExchange.duration_ms)} · ${debugExchange.id}`;
    renderDebugPane();
    $("debugDialog").showModal();
  }
  async function copyDebugText() {
    const text = debugText();
    if (navigator.clipboard?.writeText) {
      await navigator.clipboard.writeText(text);
    } else {
      const input = document.createElement("textarea");
      input.value = text; input.style.position = "fixed"; input.style.opacity = "0";
      document.body.append(input); input.select(); document.execCommand("copy"); input.remove();
    }
    notify("调试内容已复制");
  }
  function activeKeyChannel(key) {
    if (key.channel === "vibe" || key.channel === "studio") return key.channel;
    const source = key.source || key.quota?.kind;
    return source === "chat" ? "vibe" : source === "api" ? "studio" : "";
  }
  function renderKeyQuota(key, channel) {
    const kind = channel === "vibe" ? "chat" : "api";
    const quota = key.quotas?.[channel] || (key.quota?.kind === kind ? key.quota : null);
    const available = quota?.available;
    const active = activeKeyChannel(key) === channel;
    const remaining = available ? `${quota.remaining.toFixed(2)} ${escapeHTML(quota.currency || "")}` : "—";
    return `<div class="quota-pool ${active ? "active" : ""}" role="button" tabindex="0" aria-label="${escapeHTML(key.label)}：切换至 ${channel === "vibe" ? "Vibe" : "Studio / API"} 渠道" aria-pressed="${active}" aria-disabled="${switchingKeys.has(key.id)}" data-channel-key="${escapeHTML(key.id)}" data-channel="${channel}" title="点击切换至 ${channel === "vibe" ? "Vibe" : "Studio / API"} 渠道"><div class="quota-title"><span>${channel === "vibe" ? "Vibe" : "Studio / API"} 月度额度</span>${active ? '<span class="badge success">当前渠道</span>' : '<span class="muted">剩余</span>'}</div><div class="quota-value">${remaining}</div><div class="quota-track"><div class="quota-fill ${quota?.used_percent >= 90 ? "low" : ""}" style="width:${available ? Math.max(0, Math.min(100, 100 - quota.used_percent)) : 0}%"></div></div><div class="sub">${available ? `已用 ${quota.used.toFixed(2)} / ${quota.limit.toFixed(2)} ${escapeHTML(quota.currency || "")} · ${quota.used_percent.toFixed(1)}%` : escapeHTML(quota?.error || "尚未获取额度")}</div>${available && quota.error ? `<div class="sub error-text">${escapeHTML(quota.error)}，显示上次结果</div>` : ""}<div class="sub">${escapeHTML(quota?.plan || "套餐待查询")} · 重置 ${escapeHTML(dateText(quota?.reset_at))}</div>${available ? `<div class="sub">${quota.pay_as_you_go ? "已开启超额按量付费" : "按套餐额度使用"}</div>` : ""}<div class="sub">${quota?.updated_at ? `更新于 ${escapeHTML(dateText(quota.updated_at))}` : "额度来自 Mistral"}</div></div>`;
  }
  async function switchKeyChannel(id, channel) {
    if (switchingKeys.has(id) || snapshot.keys.find(key => key.id === id)?.channel === channel) return;
    const focused = document.activeElement;
    const restoreFocus = focused?.classList.contains("quota-pool") && focused.dataset.channelKey === id && focused.dataset.channel === channel;
    switchingKeys.add(id); renderKeys();
    try {
      const updated = await api("/keys/channel", jsonOptions("PATCH", { id, channel }));
      const index = snapshot.keys.findIndex(key => key.id === id);
      if (index >= 0) snapshot.keys[index] = updated;
      notify(channel === "auto" ? "已恢复按 Key 来源选择渠道，新请求生效" : `已切换至 ${channel === "vibe" ? "Vibe" : "Studio / API"} 请求渠道，新请求生效`);
    } catch (error) { notify(error.message, true); }
    finally {
      switchingKeys.delete(id); renderKeys();
      if (restoreFocus) [...$("keyList").querySelectorAll(".quota-pool")].find(card => card.dataset.channelKey === id && card.dataset.channel === channel)?.focus();
    }
  }
  function renderKeys() {
    const query = $("keySearch").value.trim().toLowerCase();
    const keys = [...snapshot.keys].sort((a, b) => (b.priority || 0) - (a.priority || 0)).filter(key => `${key.label} ${key.masked} ${key.base_url}`.toLowerCase().includes(query));
    $("keyCount").textContent = `${keys.length} 个凭证`;
    $("keyList").innerHTML = keys.length ? keys.map(key => {
      const source = key.source || key.quota?.kind;
      const sourceName = source === "chat" ? "Vibe Key" : source === "api" ? "Studio Key" : source ? `其他来源（${source}）` : "待查询";
      const active = activeKeyChannel(key);
      const channelName = active === "vibe" ? "Vibe" : active === "studio" ? "Studio / API" : "待识别，可手动选择";
      return `<article class="key-card"><div class="key-main"><div><div class="key-name">${escapeHTML(key.label)}<span class="badge ${key.enabled ? "success" : ""}">${key.enabled ? "已启用" : "已停用"}</span></div><div class="sub mono">${escapeHTML(key.masked)}</div><div class="sub mono">${escapeHTML(key.base_url || "https://api.mistral.ai/v1")}</div><div class="key-meta"><span class="badge">优先级 ${Number(key.priority || 0)}</span></div></div>
      ${renderKeyQuota(key, "vibe")}${renderKeyQuota(key, "studio")}</div>
      <div class="key-channel"><div><div class="sub">创建来源：${escapeHTML(sourceName)}</div><div>当前请求渠道：<strong>${escapeHTML(channelName)}</strong>${key.enabled ? "" : "（凭证已停用）"}</div>${active === "studio" && source === "chat" ? '<div class="sub channel-note">Studio 渠道使用上游默认计费；Vibe 来源 Key 仍可能扣 Vibe 池。</div>' : ""}</div><div class="segments" role="group" aria-label="${escapeHTML(key.label)} 的使用渠道">${[["auto", "按来源"], ["vibe", "Vibe"], ["studio", "Studio / API"]].map(([channel, label]) => `<button type="button" data-channel-key="${escapeHTML(key.id)}" data-channel="${channel}" aria-pressed="${(key.channel || "auto") === channel}" ${switchingKeys.has(key.id) ? "disabled" : ""}>${label}</button>`).join("")}</div></div>
      <div class="key-bottom"><span class="sub">切换渠道后，新请求按所选渠道发送。</span><div class="actions"><button class="switch" role="switch" aria-checked="${key.enabled}" data-toggle-key="${escapeHTML(key.id)}" type="button"><span class="switch-track"></span>${key.enabled ? "已启用" : "已停用"}</button><button class="btn small" data-quota-key="${escapeHTML(key.id)}" type="button" ${refreshingKeys.has(key.id) ? "disabled" : ""}>${refreshingKeys.has(key.id) ? "刷新中…" : "↻ 额度"}</button><button class="btn small" data-test-key="${escapeHTML(key.id)}" type="button">测试</button><button class="btn small" data-edit-key="${escapeHTML(key.id)}" type="button">编辑</button><button class="btn small btn-danger" data-delete-key="${escapeHTML(key.id)}" type="button">删除</button></div></div></article>`;
    }).join("") : '<div class="panel empty"><strong>暂无匹配凭证</strong>添加 Mistral API Key 后即可配置模型。</div>';
  }
  async function refreshKeyQuota(id, force) {
    if (!id || refreshingKeys.has(id)) return;
    refreshingKeys.add(id); renderKeys();
    try {
      const updated = await api("/keys/quota", jsonOptions("POST", { id, force }));
      const index = snapshot.keys.findIndex(key => key.id === id);
      if (index >= 0) {
        // A slow quota response must not undo a channel switch or credential edit.
        const current = snapshot.keys[index];
        snapshot.keys[index] = { ...current, source: updated.source, quotas: updated.quotas };
      }
    } catch (error) { notify(error.message, true); }
    finally { refreshingKeys.delete(id); renderKeys(); }
  }
  async function refreshQuotas(force) {
    const pending = snapshot.keys.filter(key => force || !key.quotas || Date.now() - new Date(key.quotas.updated_at).getTime() >= 300000).map(key => key.id);
    const worker = async () => { while (pending.length) await refreshKeyQuota(pending.shift(), force); };
    await Promise.all([worker(), worker()]);
  }
  function openKey(key = null) {
    $("keyForm").reset(); $("keyId").value = key?.id || "";
    $("keyLabel").value = key?.label || "";
    $("baseUrl").value = key?.base_url || "https://api.mistral.ai/v1";
    $("keyPriority").value = key?.priority || 0;
    $("keyChannel").value = key?.channel || "auto";
    $("apiSecret").required = !key;
    $("apiSecret").placeholder = key ? "留空保留当前密钥" : "粘贴 API Key";
    $("keyDialogTitle").textContent = key ? "编辑凭证" : "添加凭证";
    $("secretHint").textContent = key ? "更换密钥后会重新获取额度。" : "保存后仅显示密钥尾号。";
    $("keyError").textContent = ""; $("keyDialog").showModal();
  }
  function visibleModels() {
    const query = $("modelSearch").value.trim().toLowerCase(), status = $("modelStatus").value;
    return snapshot.models.filter(model => `${model.client_model} ${model.upstream_model}`.toLowerCase().includes(query) && (!status || model.enabled === (status === "enabled")));
  }
  function renderModels() {
    const rows = visibleModels(), existing = new Set(snapshot.models.map(model => model.id));
    for (const id of selectedModels) if (!existing.has(id)) selectedModels.delete(id);
    $("modelCount").textContent = `${snapshot.models.filter(model => model.enabled).length} 个已启用 · ${rows.length} 个映射`;
    $("modelRows").innerHTML = rows.length ? rows.map(model => {
      const key = snapshot.keys.find(item => item.id === model.key_id);
      return `<tr><td><input type="checkbox" data-select-model="${escapeHTML(model.id)}" aria-label="选择 ${escapeHTML(model.client_model)}" ${selectedModels.has(model.id) ? "checked" : ""}></td><td class="mono model-cell">${escapeHTML(model.client_model)}</td><td class="mono model-cell">${escapeHTML(model.upstream_model)}</td><td class="mono">${escapeHTML(model.thinking_map || "自动传递")}</td><td>${escapeHTML(key?.label || "自动选择")}</td><td><button class="switch" role="switch" aria-label="启用 ${escapeHTML(model.client_model)}" aria-checked="${model.enabled}" data-toggle-model="${escapeHTML(model.id)}" type="button"><span class="switch-track"></span>${model.enabled ? "已启用" : "未启用"}</button></td><td><div class="actions"><button class="btn small" data-edit-model="${escapeHTML(model.id)}" type="button">编辑</button><button class="btn small btn-danger" data-delete-model="${escapeHTML(model.id)}" type="button">删除</button></div></td></tr>`;
    }).join("") : '<tr><td colspan="7" class="empty"><strong>暂无匹配映射</strong>拉取上游目录后，添加需要使用的模型。</td></tr>';
    $("catalogHint").textContent = `目录中有 ${snapshot.catalog.length} 个模型。拉取目录后，添加需要使用的映射，客户端使用“请求模型”名称。`;
    updateSelection();
  }
  function updateSelection() {
    const rows = visibleModels(), selected = rows.filter(model => selectedModels.has(model.id)).length;
    $("selectModels").checked = rows.length > 0 && selected === rows.length;
    $("selectModels").indeterminate = selected > 0 && selected < rows.length;
    $("selectionCount").textContent = `已选 ${selectedModels.size} 项`;
    $("enableSelected").disabled = selectedModels.size === 0;
    $("disableSelected").disabled = selectedModels.size === 0;
  }
  async function publishModels() {
    if (publishing) return;
    publishing = true;
    $("publishNotice").classList.remove("hidden"); $("publishText").textContent = "正在同步模型到 CPA…"; $("retryPublish").disabled = true;
    try {
      let requestedRevision = snapshot.models_revision;
      await request("/v0/management/plugins/vibe-bridge/config", jsonOptions("PATCH", { models_revision: requestedRevision }));
      for (let attempt = 0; attempt < 15; attempt++) {
        await new Promise(resolve => setTimeout(resolve, 400));
        await loadState();
        if (snapshot.models_revision === snapshot.published_revision) { notify("模型列表已同步到 CPA"); return; }
        if (snapshot.models_revision !== requestedRevision) {
          requestedRevision = snapshot.models_revision;
          await request("/v0/management/plugins/vibe-bridge/config", jsonOptions("PATCH", { models_revision: requestedRevision }));
        }
      }
      throw new Error("映射已保存，CPA 模型列表尚未完成同步，请点击重新同步");
    } catch (error) { $("publishText").textContent = error.message; $("publishNotice").classList.remove("hidden"); }
    finally { publishing = false; $("retryPublish").disabled = false; }
  }
  async function modelMutation(path, options) {
    await api(path, options); await loadState(); await publishModels();
  }
  function openMapping(model = null) {
    $("modelForm").reset(); $("mappingId").value = model?.id || "";
    $("upstreamModel").value = model?.upstream_model || ""; $("clientModel").value = model?.client_model || "";
    $("thinkingMap").value = model?.thinking_map || ""; $("mappingKey").value = model?.key_id || ""; $("imageLimit").value = model?.image_limit ?? "";
    $("mappingEnabled").checked = model ? model.enabled : true;
    updateMappingEnabledLabel();
    $("modelDialogTitle").textContent = model ? "编辑模型映射" : "添加模型映射";
    $("modelError").textContent = ""; $("modelDialog").showModal();
  }
  function updateMappingEnabledLabel() {
    $("mappingEnabledLabel").textContent = $("mappingEnabled").checked ? "已启用" : "未启用";
  }
  $("mappingEnabled").addEventListener("change", updateMappingEnabledLabel);
  $("proxyUrl").addEventListener("input", () => { $("proxyUrl").dataset.dirty = "true"; $("proxyError").textContent = ""; });
  $("proxyForm").addEventListener("submit", event => {
    event.preventDefault();
    if ($("proxyForm").getAttribute("aria-busy") !== "true") saveProxy($("proxyUrl").value.trim());
  });
  $("resetProxy").addEventListener("click", () => saveProxy(""));
  $("authForm").addEventListener("submit", async event => {
    event.preventDefault();
    const form = event.currentTarget;
    await busy(form.querySelector('[type="submit"]'), async () => {
      managementKey = $("managementKey").value.trim(); storeSessionKey(managementKey); await connect();
    });
  });
  $("keyForm").addEventListener("submit", async event => {
    event.preventDefault();
    const form = event.currentTarget, id = $("keyId").value;
    const old = snapshot.keys.find(key => key.id === id);
    const input = { id, label: $("keyLabel").value.trim(), secret: $("apiSecret").value.trim(), base_url: $("baseUrl").value.trim(), priority: Number($("keyPriority").value), channel: $("keyChannel").value, enabled: old ? old.enabled : true };
    await busy(form.querySelector('[type="submit"]'), async () => {
      $("keyError").textContent = "";
      let key;
      try { key = await api("/keys", jsonOptions(id ? "PATCH" : "POST", input)); }
      catch (error) { $("keyError").textContent = error.message; return; }
      form.reset(); $("keyDialog").close(); notify(id ? "凭证已更新" : "凭证已添加");
      await loadState(); refreshKeyQuota(key.id, false);
    });
  });
  $("modelForm").addEventListener("submit", async event => {
    event.preventDefault();
    const form = event.currentTarget;
    const model = { id: $("mappingId").value, client_model: $("clientModel").value.trim(), upstream_model: $("upstreamModel").value.trim(), thinking_map: $("thinkingMap").value.trim(), image_limit: $("imageLimit").value === "" ? 0 : parseInt($("imageLimit").value, 10), key_id: $("mappingKey").value, enabled: $("mappingEnabled").checked };
    await busy(form.querySelector('[type="submit"]'), async () => {
      $("modelError").textContent = "";
      try { await api("/models", jsonOptions("POST", model)); }
      catch (error) { $("modelError").textContent = error.message; return; }
      $("modelDialog").close(); await loadState(); await publishModels();
    });
  });
  $("addKey").addEventListener("click", () => openKey());
  $("addModel").addEventListener("click", () => openMapping());
  $("debugModeToggle").addEventListener("click", event => {
    const enabled = !snapshot.debug_mode;
    if (enabled && !confirm("调试模式会保存完整的上游请求和响应，其中可能包含提示词及模型输出。认证信息会脱敏，最多保留最近 100 条或 256 MiB。继续启用？")) return;
    busy(event.currentTarget, async () => {
      await api("/debug", jsonOptions("PATCH", { enabled }));
      await loadState(); await loadLogs();
      notify(enabled ? "调试模式已启用" : "调试模式已关闭");
    });
  });
  $("clearDebugLogs").addEventListener("click", event => {
    if (!confirm("清除已保存的全部调试明细？普通请求日志会保留。")) return;
    busy(event.currentTarget, async () => {
      await api("/debug", { method: "DELETE" });
      await loadLogs();
      notify("调试明细已清除");
    });
  });
  $("copyDebug").addEventListener("click", event => busy(event.currentTarget, copyDebugText));
  $("keySearch").addEventListener("input", renderKeys);
  $("modelSearch").addEventListener("input", renderModels);
  $("modelStatus").addEventListener("change", renderModels);
  $("refreshQuotas").addEventListener("click", event => busy(event.currentTarget, () => refreshQuotas(true)));
  $("refreshLogs").addEventListener("click", event => busy(event.currentTarget, loadLogs));
  $("retryPublish").addEventListener("click", publishModels);
  $("syncModels").addEventListener("click", event => busy(event.currentTarget, async () => {
    const key = [...snapshot.keys].filter(item => item.enabled).sort((a, b) => (b.priority || 0) - (a.priority || 0))[0];
    if (!key) { showView("keys"); openKey(); return; }
    await api("/models/sync", jsonOptions("POST", { key_id: key.id })); await loadState();
    notify(`已拉取 ${snapshot.catalog.length} 个上游模型`);
  }));
  $("selectModels").addEventListener("change", event => {
    visibleModels().forEach(model => event.currentTarget.checked ? selectedModels.add(model.id) : selectedModels.delete(model.id)); renderModels();
  });
  for (const [id, enabled] of [["enableSelected", true], ["disableSelected", false]]) {
    $(id).addEventListener("click", event => busy(event.currentTarget, () => modelMutation("/models", jsonOptions("PATCH", { ids: [...selectedModels], enabled }))));
  }
  const reloadFirstPage = () => { page = 1; loadLogs().catch(showError); };
  $("logSearch").addEventListener("input", () => { clearTimeout(searchTimer); searchTimer = setTimeout(reloadFirstPage, 250); });
  ["logStatus", "logModel", "logKey", "logThinking", "pageSize"].forEach(id => $(id).addEventListener("change", reloadFirstPage));
  $("applyRange").addEventListener("click", reloadFirstPage);
  $("prevPage").addEventListener("click", () => { if (page > 1) { page--; loadLogs().catch(showError); } });
  $("nextPage").addEventListener("click", () => { if (page < totalPages) { page++; loadLogs().catch(showError); } });
  document.querySelector(".plugin-nav").addEventListener("keydown", event => {
    if (!["ArrowLeft", "ArrowRight", "Home", "End"].includes(event.key)) return;
    event.preventDefault();
    const tabs = [...document.querySelectorAll("[data-view]")];
    let index = tabs.indexOf(document.activeElement);
    if (event.key === "Home") index = 0;
    else if (event.key === "End") index = tabs.length - 1;
    else index = (index + (event.key === "ArrowRight" ? 1 : -1) + tabs.length) % tabs.length;
    tabs[index].focus(); showView(tabs[index].dataset.view);
  });
  document.body.addEventListener("change", event => {
    const checkbox = event.target.closest("[data-select-model]");
    if (checkbox) { checkbox.checked ? selectedModels.add(checkbox.dataset.selectModel) : selectedModels.delete(checkbox.dataset.selectModel); updateSelection(); }
  });
  $("keyList").addEventListener("keydown", event => {
    const card = event.target.closest(".quota-pool[data-channel-key]");
    if (!card || (event.key !== "Enter" && event.key !== " ")) return;
    event.preventDefault();
    if (card.getAttribute("aria-disabled") !== "true") switchKeyChannel(card.dataset.channelKey, card.dataset.channel);
  });
  document.body.addEventListener("click", event => {
    const button = event.target.closest("button, .quota-pool[data-channel-key]");
    if (!button || button.disabled || button.getAttribute("aria-disabled") === "true") return;
    const data = button.dataset;
    if (data.close) $(data.close).close();
    if (data.view) showView(data.view);
    if (data.debugLog) busy(button, () => openDebugLog(data.debugLog));
    if (data.debugPane) { debugPane = data.debugPane; renderDebugPane(); }
    if (data.range) {
      range = data.range;
      document.querySelectorAll("[data-range]").forEach(item => item.setAttribute("aria-pressed", String(item.dataset.range === range)));
      $("customRange").classList.toggle("hidden", range !== "custom");
      if (range !== "custom") reloadFirstPage();
    }
    if (data.editKey) openKey(snapshot.keys.find(key => key.id === data.editKey));
    if (data.quotaKey) refreshKeyQuota(data.quotaKey, true);
    if (data.channelKey) switchKeyChannel(data.channelKey, data.channel);
    if (data.testKey) busy(button, async () => { const result = await api("/keys/test", jsonOptions("POST", { id: data.testKey })); notify(`连接成功，可用 ${result.model_count} 个模型`); });
    if (data.toggleKey) busy(button, async () => {
      const key = snapshot.keys.find(key => key.id === data.toggleKey);
      await api("/keys/toggle", jsonOptions("PATCH", { id: key.id, enabled: !key.enabled })); await loadState();
    });
    if (data.deleteKey && confirm("删除此凭证？绑定它的模型将改为自动选择凭证。")) busy(button, async () => { await api("/keys/delete", jsonOptions("DELETE", { id: data.deleteKey })); await loadState(); notify("凭证已删除"); });
    if (data.editModel) openMapping(snapshot.models.find(model => model.id === data.editModel));
    if (data.toggleModel) busy(button, () => {
      const model = snapshot.models.find(item => item.id === data.toggleModel);
      return modelMutation("/models", jsonOptions("PATCH", { ids: [model.id], enabled: !model.enabled }));
    });
    if (data.deleteModel && confirm("删除此模型映射并从 CPA 模型列表移除？")) busy(button, () => modelMutation("/models/delete", jsonOptions("DELETE", { id: data.deleteModel })));
  });
  if (managementKey) connect(); else showAuth();
})();
