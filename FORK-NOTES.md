# Vibe Bridge — fork 说明

从 RzMY/MistralBridge fork（分支 `vibe-fingerprint`）。pluginID 已改名
`vibe-bridge`（官方商店已占 `mistral-bridge`，避免冲突）。
指纹规范文档: `../vibe-fingerprint.md`

## 相对上游的改动 (mistral.go)

1. **developer → system 折叠**（修 Mistral 422）
2. **injectVibeMetadata**：channel=vibe 时写官方 Vibe 全套请求归因
   （call_source=vibe_code、call_type、host_kind、version 3.1.4、
   session_id、os/arch/os_version）；客户端 metadata 键保留
3. **vibeRequestHeaders**：channel=vibe 时加
   `User-Agent: mistral-client-python/Mistral-Vibe/3.1.4` + `X-Affinity`
4. **改名**：pluginID/mistral-bridge → vibe-bridge（plugin.go、artifacts.py、
   build.yml、web/app.js、tests 全量替换）

## 商店分发（direct 类型，自托管）

- `store/registry.json`：schema_version 2，direct install，含 linux/amd64、
  linux/arm64、darwin/arm64 三个产物 URL+sha256（URL 里 BASE_URL 占位符待替换）
- `dist/`：商店格式 zip（`vibe-bridge_0.1.0_<os>_<arch>.zip`，包内只有动态库）
- 部署：把 registry.json + zip 放到任意静态 URL（例：VPS nginx
  `https://ai.8823823.xyz/plugin-store/`），CPA config 加：
  ```yaml
  plugins:
    enabled: true
    dir: plugins
    store-sources:
      - "https://ai.8823823.xyz/plugin-store/registry.json"
    configs:
      vibe-bridge:
        enabled: true
  ```
  → 面板商店出现 "Mistral Vibe Bridge" → 点安装 → 无需重启生效
- http 源需加 `store-auth` 规则 `allow-insecure: true`（https 不需要）

## 已验证

- 本地 CPA 8.0.64 从自建商店安装 → 无需重启加载 → 请求全指纹 ✓
- developer→system / 流式 / /v1/responses / studio 负向全过 ✓
- linux .so 用 zig cc 交叉编译（仅需 GLIBC 2.3.2，Ubuntu 22.04+ 无虞）✓

## VPS 上线步骤

1. 上传 registry.json（BASE_URL→实际 URL）+ 两个 linux zip 到静态路径
2. CPA config.yaml 加 store-sources（或面板 config.yaml 编辑器）
3. 面板插件商店点安装 → 凭证管理加 Mistral key，channel 设 vibe → 模型映射

## 注意

- API-key 路径的 Vibe 不带 tenant headers（官方行为，正确省略）
- vibeClientVersion 跟随官方 Vibe __version__（当前 3.1.4）
- 若推 GitHub：上游 build.yml 是全平台发布流水线，tag push 自动生成
  release 资产 + checksums.txt（git tag v0.1.0 && push）

## GitHub 发布（已完成）
- Repo: https://github.com/bunpai/vibe-bridge （main = vibe-fingerprint 分支内容）
- Release v0.1.0 已发布，CI 自动生成 6 平台 zip + checksums.txt
- Store registry: https://raw.githubusercontent.com/bunpai/vibe-bridge/main/store/registry.json （github-release 类型）
- 本地端到端已验证：自定义源 → 列表 → install(github-release) → 热载 → 指纹/折叠正常
- GitHub token: CPA 的 githubauth 自动读 GITHUB_TOKEN / github_token 环境变量；store-auth 支持 type: github-token + token-env

## VPS Docker CPA 部署步骤
1. 找到挂载进容器的 config.yaml（docker run -v 或 compose volumes）
2. 添加插件源：
   plugins:
     enabled: true
     store-sources:
       - "https://raw.githubusercontent.com/bunpai/vibe-bridge/main/store/registry.json"
3. （强烈建议）docker env 加 GITHUB_TOKEN=<PAT>——GitHub API 未认证限 60 次/小时/IP，VPS 共享出口易触发 plugin_store_rate_limited
4. 重启容器，管理面板 → 插件 → 商店，来源选 "raw.githubusercontent.com"，安装 Mistral Vibe Bridge 0.1.0（热载，无需再重启）
5. 在 vibe-bridge 插件控制台添加 Mistral key：channel=vibe、base_url=https://api.mistral.ai/v1、models 映射
6. 官方源里 RzMY 的 mistral-bridge 与本插件 id 不同（vibe-bridge），可共存
