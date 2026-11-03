# workbuddy-intl

把 **WorkBuddy 国际版**（`www.workbuddy.ai`）封装成 [CLIProxyAPI](https://github.com/router-for-me/CLIProxyAPI)(CPA) 插件，任何支持 OpenAI / Anthropic 协议的客户端（Claude Code、Cursor、Cline、Cherry Studio、SDK……）都能直接调用它背后的模型。

跟同仓库的 [`workbuddy/`](../workbuddy)（国内版，`copilot.tencent.com`）是同一份代码的两个构建目标，**协议形状一致，但上游校验规则不同**——见下面「上游校验」。国际版不是国内版的换域名版，两者不能互相替代。

## 工作原理

在 CPA 里注册为 `workbuddy-intl` provider：负责 WorkBuddy 网页扫码登录、token 刷新，并把请求转发到 `www.workbuddy.ai/v2/chat/completions`。登录后凭据存为 `workbuddy-intl.json`。

上游网关要求 `Origin` / `Referer` 存在，否则 `/v3/config` 直接 400。

## 模型

模型列表**不硬编码**，启动时从 `GET /v3/config` 拉取真实目录，`/v1/models` 看到的就是上游当下发布的。上下文长度、输出上限、是否支持图片等元信息同样来自该接口，随上游调整自动生效。

这个接口只需要 `X-User-Id` 头存在（值不被校验，插件填全零 UUID），**无需登录**即可拿到完整目录，且返回内容与账号无关。目录里 `maxInputTokens` 为空的条目是画图 / 视频模型（`gpt-image-2.5-sunburst`、`seedance-2.5`），不能承载 chat completion，插件会把它们过滤掉。

具体可用性仍以 WorkBuddy 账号权限为准。

## 安装

**前置**：运行中的 CLIProxyAPI v7.2.x（带 CGO / 插件支持）、WorkBuddy 国际版账号、Go 1.26+ 与 gcc；编译架构需与 CPA 实例一致（amd64 / arm64）。

Ubuntu / Debian 上装依赖（注意 Ubuntu 24.04 自带的 `golang-go` 是 1.22，**版本不够**）：

```bash
curl -fsSL https://go.dev/dl/go1.27.1.linux-amd64.tar.gz -o /tmp/go.tgz
sudo rm -rf /usr/local/go && sudo tar -C /usr/local -xzf /tmp/go.tgz
export PATH=$PATH:/usr/local/go/bin

sudo apt update && sudo apt install -y gcc-mingw-w64-x86-64
export GOPROXY=https://goproxy.cn,direct   # 国内网络
```

编译（在**仓库根目录**执行）：

```bash
CGO_ENABLED=1 GOOS=windows GOARCH=amd64 CC=x86_64-w64-mingw32-gcc \
  go build -buildmode=c-shared -ldflags="-s -w" \
  -o workbuddy-intl.dll ./workbuddy-intl
```

编译到 Linux / macOS 换成：

```bash
CGO_ENABLED=1 GOOS=linux  GOARCH=amd64 go build -buildmode=c-shared -ldflags="-s -w" -o workbuddy-intl.so    ./workbuddy-intl
CGO_ENABLED=1 GOOS=darwin GOARCH=arm64 go build -buildmode=c-shared -ldflags="-s -w" -o workbuddy-intl.dylib ./workbuddy-intl
```

产物：`.so`(Linux) / `.dylib`(macOS) / `.dll`(Windows)。**文件名决定插件 ID**，必须是 `workbuddy-intl`，要跟 `config.yaml` 里的键对上。放到 CPA 的 `plugins/` 目录：

```yaml
plugins:
  enabled: true
  dir: "plugins"
  configs:
    workbuddy-intl: { enabled: true, priority: 100 }
```

重启 CPA，日志出现 `plugin loaded ... plugin_id=workbuddy-intl` 即成功。然后到 CPA 面板添加 workbuddy-intl 凭据，扫码登录 WorkBuddy。

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
export ANTHROPIC_MODEL=gpt-5.6-sol
claude
```

```bash
# curl / OpenAI
curl http://localhost:8317/v1/chat/completions \
  -H "Authorization: Bearer <api-key>" -H "Content-Type: application/json" \
  -d '{"model":"deepseek-v4.1-flash","messages":[{"role":"user","content":"你好"}],"stream":true}'
```

## 上游校验

国际版网关比国内版严，四个坑都已实测确认，插件里都有对应处理。改代码前先看完这节。

### 1. 首条消息必须是 system（`code 11128`）

`messages[0].role` 不是 `system` 时，上游回：

```json
{"code":11128,"msg":"first message is not system prompt", ...}
```

客户端看到的是被包装过的 `displayMsg`：「请求被安全策略拦截，请稍后重试或联系支持。」——**这句提示跟真实原因无关**，别被带偏。国内版 `copilot.tencent.com` **没有**这条校验，所以同一个客户端对着 workbuddy 正常、对着 workbuddy-intl 报错。

受影响的典型场景：Cherry Studio 没设助手提示词、裸 SDK 调用、`messages` 直接从 user 开头的请求。插件在转发前调 `ensureSystemFirst`：已经在别处的 system 消息会被**提到最前**（保留客户端自己的提示词），整段没有 system 时才补一条 `defaultSystemPrompt`。

补的这条**不能是空串或纯空白**：`kimi-k3` / `kimi-k2.6` / `kimi-k2.8-preview` / `balanced-model` 对空 system 会回 400 `code 11151`（`a message has empty content`），而目录里其它模型都接受空串——所以只按「空串能过」来选默认值会踩到 kimi 系。目前用的是 `You are a helpful assistant.`，是实测在全部模型上都通过的最短形态。自带 system 的请求永远看不到它。

### 2. 拒绝非流式（`code 11101`）

`stream` 不为 `true` 时回 `Non-stream chat request is currently not supported`。跟国内版一样，插件统一把请求改写成 `stream:true`，非流式请求在本地把 SSE 聚合成一个 `chat.completion` 返回。

### 3. Claude Code 两句模板被逐字拉黑（`code 11128`）

`code 11128` 在国际版是个通用拦截码，会复用在多个原因上。内容审核把 Claude Code 的两句固定 system 模板加进了黑名单：

- `You are Claude Code, Anthropic's official CLI for Claude.`（身份句）
- `Main branch (you will usually use this for PRs)`（git 注入句）

命中回 `Illegal API invocation from an unapproved channel`。**精确匹配，非语义审核**：任何一字改动都绕过。这两句的改写现在是 `internal/channelgate` 里 `claudeCodePhrases` 的一部分。

### 3.5 内容指纹（`code 11128`，2026-11-02 实测）

同一个码还负责「prompt 里有我们不想认的东西」。用一份真实的 DeepSeek Harness system prompt（5,923 字）逐项实测，其余字段全部固定：

| 发出去的 prompt | 结果 |
|---|---|
| 原样 | 400 / 11128 |
| 只改三句身份句 | 400 / 11128 |
| 只改 `@` 路径段落 | 400 / 11128 |
| 只在最前面加一句中性开场 | 400 / 11128 |
| 只插零宽字符 | 400 / 11128 |
| **整张改写表全上** | **200** |

闸门同时匹配多段文本，任何单一变换都不够。插件因此每次请求都跑完整张表（`gatePhrases`），并保留三级兜底阶梯，只在上游仍然回 11128 时逐级花掉：强制中性开场 → 句读/专有名词边界零宽 → 词间全插零宽。失败时错误信息会写明阶梯花到哪一级（`[channel-gate ladder exhausted: ...]`），便于区分「重试过三次」和「根本没重试」。

被扫描的 role 是 `system` 和 `assistant`；`user` / `tool` 不动——改用户自己的话只会改变对话，换不来任何闸门收益。

### 3.6 出站身份

国际版被抽样时还会查「这条请求像不像官方客户端」：

- **会话三元组**：`X-Conversation-ID`（每次新 UUID）+ `X-Conversation-Request-ID` / `X-Root-Request-ID` / `X-Conversation-Message-ID`（同一个 32 hex 值）。缺了它回 11128「conversation id missing」，而且只在抽样时回——所以表现为「某个账号忽然不能用了」。
- **归属头**：`X-Agent-Purpose` / `X-IDE-Name` / `X-IDE-Type` / `X-Product` 同值 `WorkBuddy`，外加 `X-CodeBuddy-Request` 与按区域切的 `Accept-Language`。
- **设备对**：`X-Machine-ID` / `X-Session-ID`，按账号 uid 派生（`internal/wire`），两个账号在同一台机器上也不会看起来是同一台设备；uid 只做哈希，不上线。

属于 cat-and-mouse：上游哪天加新指纹，得跟着改 `gatePhrases`；兜底阶梯负责那些还没被测出来的。

### 4. 模型名校验比国内版严格

国际版目录里的别名（`default-model` / `fast-model` / `primary-model` 等）可以用，但**不要照搬国内版的模型名**。填了上游不存在的模型会回 `code 11102 model [...] service info not found`，同样被包装成「当前模型不可用」。以 `GET /v1/models` 的实际返回为准。

## 思考模式

混元系列（`hy3`、`hy4-preview` 等 `hy3` / `hy4` 前缀模型）自动开最大思考：插件转发前强制 `reasoning_effort=high`，覆盖客户端任何设置。CodeBuddy 只对 `high` 真正开深度思考（`medium` / `max` / `xhigh` 等档位它直接忽略），所以这已是 hy3 能用的最高档。思考内容走 SSE 的 `delta.reasoning_content`，客户端要支持渲染思考块才看得到。

## 流式

真流式（async）：转发上游时边读边通过 `host.stream.emit` 把每个 chunk 实时推给 CPA，客户端逐字收到（不是等收齐了一股脑）。

跨协议入口（Claude / Gemini / Codex 等）的 chunk 由插件自己补 `data: ` 帧——CPA 的 chat-completions 直通会自己加前缀，但各格式转换器只吃已经带帧的 payload，所以插件按 `metadata.request_path` 判断要不要自己加。见 `clientNeedsSSEFrame`。

## License

MIT。
