/*
 * dsh-vibe-search settings page, browser half. Registers a "plugins.item" tab
 * while the Host serves the dsh-vibe-search namespace. The provider picker is
 * populated from the llm-pi-ai namespace's configured providers, so it tracks
 * whatever endpoints the profile defines.
 */
window.__ModuleLoader__.load({
  id: "dsh-vibe-search",
  factory: (require) => {
    const module = { exports: {} };
    const exports = module.exports;
    Object.defineProperty(exports, Symbol.toStringTag, { value: "Module" });
    const runtime = require("react/jsx-runtime");
    const react = require("react");
    const jsx = runtime.jsx;
    const jsxs = runtime.jsxs;

    const OWN_NS = "dsh-vibe-search";
    const PI_NS = "llm-pi-ai";
    const DICT_NS = "settings.vibeSearch";

    const zh = {
      title: "Vibe 搜索",
      description: "Mistral connector 搜索（web/news/weather/finance），走 vibe-bridge CPA 网关。",
      provider: "复用端点",
      providerHint: "从 llm-pi-ai 的 provider 继承 base_url / api_key；留空继承 cpa，cpa 不存在时取第一个。",
      baseUrl: "网关地址",
      baseUrlHint: "留空 = 继承所选端点的 baseURL。",
      apiKey: "API Key",
      apiKeyHint: "留空 = 继承所选端点的 apiKeyEnv/apiKey。",
      model: "模型",
      modelHint: "留空 = vibe-search 虚拟搜索模型（网关自动联网搜索）。仅当网关关闭 connector 或插件版本低于 v0.1.3 时才需填底层模型（如 zai-glm-5-3-flash）。",
      timeout: "超时 (ms)",
      overridden: "已覆盖",
      reset: "恢复继承",
      save: "保存",
      saving: "保存中…",
      saveOk: "已保存",
      saveFailed: "保存未生效，请重试。",
      unavailable: "插件未加载，暂不能配置。",
      readOnly: "本部署的设置为只读。",
      auto: "自动 (cpa)",
      none: "（当前 profile 没有 provider）"
    };
    const en = {
      title: "Vibe Search",
      description: "Mistral connector search (web/news/weather/finance) via a vibe-bridge CPA gateway.",
      provider: "Inherit endpoint",
      providerHint: "Inherits base_url / api_key from an llm-pi-ai provider; empty inherits 'cpa', or the first provider.",
      baseUrl: "Gateway URL",
      baseUrlHint: "Empty = inherit the selected provider's baseURL.",
      apiKey: "API key",
      apiKeyHint: "Empty = inherit the selected provider's apiKeyEnv/apiKey.",
      model: "Model",
      modelHint: "Empty = the virtual vibe-search model (gateway auto-searches the web). Set a backing model (e.g. zai-glm-5-3-flash) only when the gateway has connectors off or a plugin older than v0.1.3.",
      timeout: "Timeout (ms)",
      overridden: "Overridden",
      reset: "Reset to inherit",
      save: "Save",
      saving: "Saving…",
      saveOk: "Saved",
      saveFailed: "Save was not applied; retry.",
      unavailable: "Plugin not loaded; cannot be configured.",
      readOnly: "This deployment stores settings read-only.",
      auto: "Auto (cpa)",
      none: "(no providers in this profile)"
    };

    const FIELDS = ["provider", "base_url", "api_key", "model", "timeout_ms"];
    const styles = {
      form: { display: "flex", flexDirection: "column", gap: 14, maxWidth: 520 },
      field: { display: "flex", flexDirection: "column", gap: 4 },
      head: { display: "flex", justifyContent: "space-between", alignItems: "center" },
      label: { fontSize: 13, fontWeight: 500 },
      reset: { fontSize: 11, border: "none", background: "none", cursor: "pointer", color: "var(--dsw-alias-text-accent, #5b8def)", padding: 0 },
      input: {
        font: "inherit", fontSize: 13, padding: "6px 8px", borderRadius: 6,
        border: "1px solid var(--dsw-alias-border-l3, #ccc)",
        background: "var(--dsw-alias-bg-elevated, transparent)", color: "inherit",
      },
      hint: { margin: 0, fontSize: 12, opacity: 0.65 },
      badge: { fontSize: 11, opacity: 0.7 },
      row: { display: "flex", gap: 10, alignItems: "center", marginTop: 4 },
      btn: {
        font: "inherit", fontSize: 13, padding: "6px 14px", borderRadius: 6, cursor: "pointer",
        border: "1px solid var(--dsw-alias-border-l3, #ccc)",
        background: "var(--dsw-alias-bg-accent, #2f5fd0)", color: "#fff",
      },
      status: { fontSize: 12, opacity: 0.75 },
    };

    function providerRows(mirror) {
      const view = mirror?.view;
      const ns = (view?.namespaces ?? []).find((row) => row.ns === PI_NS);
      const providers = ns?.value?.providers ?? {};
      return Object.entries(providers).map(([name, p]) => ({
        name,
        baseURL: p?.baseURL ?? "",
        model: Array.isArray(p?.models) && p.models.length ? (p.models[0].id ?? p.models[0].name ?? "") : "",
      }));
    }

    function Field(props) {
      const overridden = props.user !== void 0 && props.user !== "";
      return jsxs("div", {
        style: styles.field, children: [
          jsxs("div", {
            style: styles.head, children: [
              jsx("label", { style: styles.label, htmlFor: props.id, children: props.label }),
              overridden ? jsxs("span", { style: { display: "flex", gap: 8, alignItems: "center" }, children: [
                jsx("span", { style: styles.badge, children: props.overriddenLabel }),
                jsx("button", { type: "button", style: styles.reset, disabled: props.disabled, onClick: () => props.onDraft(""), children: props.resetLabel }),
              ] }) : null,
            ],
          }),
          props.control,
          jsx("p", { style: styles.hint, children: props.hint }),
        ],
      });
    }

    function VibeSearchCard(props) {
      const { t } = props;
      const snap = props.useForm((s) => s);
      const mirror = props.useMirror((s) => s);
      const [draft, setDraft] = react.useState(null);
      const [state, setState] = react.useState("idle"); // idle | saving | ok | failed

      react.useEffect(() => { setDraft(null); setState("idle"); }, [snap.revision]);

      if (props.view === "summary") return t("description");
      if (!snap || snap.status === "unavailable" || snap.status === "loading") {
        return jsx("p", { style: styles.hint, children: t("unavailable") });
      }
      const disabled = !snap.writable;
      const value = { provider: "", base_url: "", api_key: "", model: "", timeout_ms: 60000, ...(snap.value ?? {}) };
      const user = snap.user ?? {};
      const d = draft ?? value;
      const dirty = draft !== null && FIELDS.some((k) => String(d[k] ?? "") !== String(value[k] ?? ""));
      const rows = providerRows(mirror);
      const selProvider = rows.find((r) => r.name === d.provider);

      const onDraft = (k, v) => { setDraft({ ...d, [k]: v }); setState("idle"); };
      const save = async () => {
        setState("saving");
        const ops = [];
        for (const k of FIELDS) {
          let v = d[k];
          if (k === "timeout_ms") { const n = Number(v); v = v === "" || v === void 0 ? "" : (Number.isFinite(n) ? Math.max(1000, Math.trunc(n)) : ""); }
          if (v === "" || v === void 0) ops.push({ op: "unset", path: [k] });
          else ops.push({ op: "set", path: [k], value: v });
        }
        const ok = await props.scope.mutate(ops);
        setState(ok ? "ok" : "failed");
        if (ok) setDraft(null);
      };

      const field = (k, label, hint, control) => jsx(Field, {
        id: "vibe-search-" + k, label, hint, disabled,
        overriddenLabel: t("overridden"), resetLabel: t("reset"),
        user: user[k], onDraft: (v) => onDraft(k, v), control,
      }, k);

      return jsxs("div", {
        style: styles.form, children: [
          field("provider", t("provider"), t("providerHint"), jsxs("select", {
            style: styles.input, value: d.provider ?? "", disabled,
            onChange: (e) => onDraft("provider", e.target.value),
            children: [
              jsx("option", { value: "", children: t("auto") }, "auto"),
              rows.map((r) => jsx("option", { value: r.name, children: r.name + (r.baseURL ? "  —  " + r.baseURL : "") }, r.name)),
              rows.length === 0 ? jsx("option", { value: "", disabled: true, children: t("none") }, "none") : null,
            ],
          })),
          selProvider ? jsx("p", { style: styles.hint, children: "→ " + (selProvider.baseURL || "?") + (selProvider.model ? "  ·  " + selProvider.model : "") }) : null,
          field("base_url", t("baseUrl"), t("baseUrlHint"), jsx("input", {
            style: styles.input, type: "text", value: d.base_url ?? "", disabled,
            placeholder: selProvider?.baseURL ?? "", onChange: (e) => onDraft("base_url", e.target.value),
          })),
          field("api_key", t("apiKey"), t("apiKeyHint"), jsx("input", {
            style: styles.input, type: "password", autoComplete: "new-password",
            value: d.api_key ?? "", disabled, onChange: (e) => onDraft("api_key", e.target.value),
          })),
          field("model", t("model"), t("modelHint"), jsx("input", {
            style: styles.input, type: "text", value: d.model ?? "", disabled,
            placeholder: "vibe-search", onChange: (e) => onDraft("model", e.target.value),
          })),
          field("timeout_ms", t("timeout"), "", jsx("input", {
            style: { ...styles.input, maxWidth: 160 }, type: "text", inputMode: "numeric",
            value: d.timeout_ms ?? "", disabled, onChange: (e) => onDraft("timeout_ms", e.target.value),
          })),
          jsxs("div", {
            style: styles.row, children: [
              jsx("button", { type: "button", style: styles.btn, disabled: disabled || !dirty || state === "saving", onClick: save, children: state === "saving" ? t("saving") : t("save") }),
              jsx("span", { style: styles.status, children: disabled ? t("readOnly") : state === "ok" ? t("saveOk") : state === "failed" ? t("saveFailed") : "" }),
            ],
          }),
        ],
      });
    }

    const inject = ["slots", "locale", "configForms"];

    function apply(ctx) {
      const t = ctx.locale.bind(DICT_NS);
      ctx.effect(() => ctx.locale.register(DICT_NS, { zh, en }), "vibe-search: dictionaries");
      const scope = ctx.configForms.get(OWN_NS);
      const mirror = ctx.configForms.describe();
      ctx.effect(() => ctx.configForms.whileServed([OWN_NS], () => ctx.slots.inject("plugins.item", () => ctx.slots.register({
        name: "plugins.item",
        id: "vibe-search",
        order: 45,
        label: () => t("title"),
        locale: DICT_NS,
        inject: () => ({ hooks: { form: scope, mirror }, scope }),
      }, VibeSearchCard))), "vibe-search: page");
    }

    exports.apply = apply;
    exports.inject = inject;
    exports.NS = DICT_NS;
    return module.exports;
  },
});
