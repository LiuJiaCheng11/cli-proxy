# cline-proxy

把 **Cline**（`api.cline.bot`）封装成 [CLIProxyAPI](https://github.com/router-for-me/CLIProxyAPI)(CPA) 插件，任何支持 OpenAI 协议的客户端（Claude Code、Cursor、Cline、SDK……）都能直接调用 Cline 背后的模型。

上游协议来自 [Cline Go 反向代理](https://github.com/lovingfish/cline-proxy)，本插件是它核心能力（登录、刷新、模型发现、转发）的重新实现，按 cliproxy 插件 ABI 重写。

## 工作原理

在 CPA 里注册为 `cline` provider：走 WorkOS 设备码登录、用 WorkOS 身份换 Cline 凭据、自动刷新 token，并把请求转发到 `api.cline.bot/api/v1/chat/completions`。凭据存为 `cline.json`。

## 模型

模型列表**不硬编码**，从 `GET /api/v1/ai/cline/recommended-models` 拉取。这个接口是**公开的**，不需要登录，所有人返回同一份，所以插件注册阶段（还没有任何账号时）就能拿到完整目录。

当前 25 个模型，按上游分组排序，免费组排最前：

| 分组 | 数量 | `owned_by` | 说明 |
|---|---:|---|---|
| `free` | 4 | `cline-free` | 不消耗额度 |
| `recommended` | 4 | `cline-recommended` | 官方推荐 |
| `clinePass` | 14 | `cline-pass` | 需要 cline-pass 订阅 |
| `clineCloud` | 3 | `cline-cloud` | 需要 cline-cloud |

`clinePass` / `clineCloud` 组的模型照样发布——上游有什么就显示什么。没订阅的账号调用会拿到 403，这是上游的行为，不是插件的问题。

上游**不提供上下文长度**，所以 `/v1/models` 里的 context length 是 0（不编造数字）。

## 安装

**前置**：运行中的 CLIProxyAPI v7.2.x（带 CGO / 插件支持）、Cline 账号、Go 1.26+ 与 gcc；编译架构需与 CPA 实例一致（amd64 / arm64）。

```bash
git clone https://github.com/lovingfish/cline-proxy.git
# 在本仓库根目录执行
CGO_ENABLED=1 GOOS=linux GOARCH=amd64 \
  go build -buildmode=c-shared -o cline.so ./cline
```

产物：`.so`（Linux）/ `.dylib`（macOS）/ `.dll`（Windows）。放到 CPA 的 `plugins/` 目录，在 `config.yaml` 启用：

```yaml
plugins:
  enabled: true
  dir: "plugins"
  configs:
    cline: { enabled: true, priority: 100 }
```

重启 CPA，日志出现 `plugin loaded ... plugin_id=cline` 即成功，`GET /v1/models` 也能看到上面的模型。然后到 CPA 面板添加 cline 凭据：会给出一个授权链接，点开完成 WorkOS 登录即可（链接已带好 user code，不用手动输入）。

## 使用

CPA 默认端口 `8317`，API key 见 `config.yaml` 的 `api-keys`。

| 协议 | Base URL |
|------|----------|
| OpenAI | `http://<host>:8317/v1` |
| Anthropic | `http://<host>:8317`（不带 `/v1`，走 `x-api-key`） |

```bash
# Claude Code
export ANTHROPIC_BASE_URL=http://localhost:8317
export ANTHROPIC_API_KEY=<api-key>
export ANTHROPIC_MODEL=deepseek/deepseek-v4-flash
claude
```

```bash
# curl / OpenAI
curl http://localhost:8317/v1/chat/completions \
  -H "Authorization: Bearer <api-key>" -H "Content-Type: application/json" \
  -d '{"model":"deepseek/deepseek-v4-flash","messages":[{"role":"user","content":"你好"}],"stream":true}'
```

模型 ID 用上游原样（如 `deepseek/deepseek-v4-flash`、`cline-free/longcat-2.0`），插件不加前缀。流式 / 非流式都支持，上游原生支持非流式。

## 与 Cline Go 反向代理的差异

原项目是一个独立服务，本插件只保留 CPA 不提供的部分：

| 原项目能力 | 本插件 |
|---|---|
| WorkOS 设备码登录 / token 刷新 | ✅ 保留 |
| 模型发现 | ✅ 保留，而且是动态的（原项目有 3 个模型的兜底列表） |
| chat 转发（流式 / 非流式） | ✅ 保留 |
| 多账号轮询（round_robin / fill / random） | ❌ 交给 CPA，它按自己的调度器在多个凭据间选 |
| Anthropic Messages 协议转换 | ❌ 交给 CPA 的 claude 译器 |
| 管理后台 / API Key 鉴权 | ❌ 交给 CPA |
| `override.md` 系统提示词覆盖 | ❌ 不移植 |
| 请求头自定义 | ❌ 不移植 |

## 实现上两个必须知道的点

1. **Authorization 头是 `Bearer workos:<accessToken>`**，不是裸 token。少了 `workos:` 前缀上游直接 401。
2. **刷新会轮换 refresh token**，旧的立刻失效。所以 `auth.refresh` 必须把上游返回的新 refresh token 写回 `StorageJSON`（CPA 会持久化），否则下一次刷新就 400。

## License

MIT。
