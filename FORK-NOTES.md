# Vibe Bridge — fork 说明

从 RzMY/MistralBridge fork，改动在分支 `vibe-fingerprint`（commit ea3da47）。
指纹规范文档: `../vibe-fingerprint.md`

## 相对上游的改动 (mistral.go)

1. **developer → system 折叠**：transformRequest 消息循环里把 OpenAI Responses
   的 developer role 转成 system。这是原 bug —— Mistral 只收
   system/user/assistant/tool，裸 developer 直接 422。
2. **injectVibeMetadata**：channel=vibe 时把官方 Vibe CLI 的完整请求归因写进
   metadata —— call_source=vibe_code（始终覆盖）、call_type=main_call、
   host_kind=local、version/agent_version/client_version=3.1.4、
   agent_entrypoint=cli、client_name=vibe-cli、session_id（进程期 UUID）、
   os/arch/os_version。客户端自带 metadata 键保留（只有 call_source 强制）。
3. **vibeRequestHeaders**：channel=vibe 时上游请求加
   `User-Agent: mistral-client-python/Mistral-Vibe/3.1.4` 和
   `X-Affinity: <session_id>`（官方 Vibe 的缓存路由头）。studio/auto 不变。

## 本地验证 (cpa-test/)

- 本地 CPA 8.0.64 (源码 build 于 ../cpa-local) + 插件 + mock 上游
- 非流式、流式、/v1/responses 三条路径 wire capture 全部命中指纹
- studio channel 负向验证：无 metadata、UA 为 Go 默认
- 原 422 根因修复：developer role 在三条路径都折叠为 system

## 部署到 VPS

1. 构建 linux/amd64: `GOOS=linux GOARCH=amd64 CGO_ENABLED=1 CC=? `
   —— c-shared 需要交叉 C 工具链；或者在 VPS 上直接 go build。
   （VPS 是 Linux；arm64 则 GOARCH=arm64）
2. 覆盖 `plugins/linux/<arch>/mistral-bridge.so`（pluginID 没变，state.json 兼容）
3. 重启 CPA，在管理页把 key 的 channel 设为 **vibe**
4. 验证：请求任意 mistral 模型，看 requests.jsonl / upstream 日志

## 注意

- API-key 路径的 Vibe 不带 tenant headers（x-external-organization-id 等），
  这是官方行为 —— 只有 OIDC 登录的 key 才带。插件 key 是 API key，正确省略。
- vibeClientVersion="3.1.4" 需跟随 Vibe 版本演进（vibe/__init__.py __version__）。
