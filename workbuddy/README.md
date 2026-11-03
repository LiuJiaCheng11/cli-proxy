# workbuddy-cliproxy

把**腾讯 CodeBuddy**（`copilot.tencent.com`）封装成 [CLIProxyAPI](https://github.com/router-for-me/CLIProxyAPI)(CPA)插件,任何支持 OpenAI / Anthropic 协议的客户端(Claude Code、Cursor、Cline、SDK……)都能直接调用 CodeBuddy 背后的模型。

对 [Sliverkiss/cpa-plugin](https://github.com/Sliverkiss/cpa-plugin) 公开 `workbuddy.so` 的 clean-room 逆向重写,补齐了源码与 x86_64 支持;workbuddy 的原始设计归属 Sliverkiss。

## 工作原理

在 CPA 里注册为 `workbuddy` provider:负责 CodeBuddy 扫码登录、token 刷新,并把请求转发到 `copilot.tencent.com/v2/chat/completions`。登录后凭据存为 `workbuddy.json`。

## 模型

模型列表**不硬编码**,启动时从 CodeBuddy 的 `GET /v3/config` 拉取真实目录,`/v1/models` 看到的就是上游当下发布的。上下文长度、输出上限、是否支持图片等元信息同样来自该接口,随上游调整自动生效。

这个接口只需要 `X-User-Id` 头存在(值不被校验),无需登录即可拿到完整目录,且返回内容与账号无关。

具体可用性仍以 CodeBuddy 账号权限为准。

## 安装

**前置**:运行中的 CLIProxyAPI v7.2.x(带 CGO / 插件支持)、CodeBuddy 账号、Go 1.26+ 与 gcc;编译架构需与 CPA 实例一致(amd64 / arm64)。

```bash
git clone https://github.com/lovingfish/workbuddy-cliproxy.git
# 在本仓库根目录执行
CGO_ENABLED=1 GOOS=linux GOARCH=amd64 \
  go build -buildmode=c-shared -o workbuddy.so ./workbuddy
```

产物:`.so`(Linux)/ `.dylib`(macOS)/ `.dll`(Windows)。放到 CPA 的 `plugins/` 目录,在 `config.yaml` 启用:

```yaml
plugins:
  enabled: true
  dir: "plugins"
  configs:
    workbuddy: { enabled: true, priority: 100 }
```

重启 CPA,日志出现 `plugin loaded ... plugin_id=workbuddy` 即成功,`GET /v1/models` 也能看到上面的模型。然后到 CPA 面板添加 workbuddy 凭据,扫码登录 CodeBuddy。

## 使用

CPA 默认端口 `8317`,API key 见 `config.yaml` 的 `api-keys`。

| 协议 | Base URL |
|------|----------|
| OpenAI | `http://<host>:8317/v1` |
| Anthropic | `http://<host>:8317`(不带 `/v1`,走 `x-api-key`) |

```bash
# Claude Code
export ANTHROPIC_BASE_URL=http://localhost:8317
export ANTHROPIC_API_KEY=<api-key>
export ANTHROPIC_MODEL=hy4-preview
claude
```

```bash
# curl / OpenAI
curl http://localhost:8317/v1/chat/completions \
  -H "Authorization: Bearer <api-key>" -H "Content-Type: application/json" \
  -d '{"model":"hy4-preview","messages":[{"role":"user","content":"你好"}],"stream":true}'
```

流式 / 非流式都支持;非流式请求会被内部转成流式再聚合(CodeBuddy 上游 `code 11101` 拒绝非流式)。

## 渠道校验(11128)

上游对 chat 请求跑一次「渠道校验」:命中即回 HTTP 400 / 业务码 `11128`,提示语是「这次调用没通过渠道校验,可能不是官方客户端发出的」——**这句话是误导,失败原因在提示词内容里,不在头上**。

2026-11-02 用一份真实的 DeepSeek Harness system prompt(5,923 字)逐项实测,其余字段全部固定:

| 发出去的 prompt | 结果 |
|---|---|
| 原样 | 400 / 11128 |
| 只改三句身份句 | 400 / 11128 |
| 只改 `@` 路径段落 | 400 / 11128 |
| 只在最前面加一句中性开场 | 400 / 11128 |
| 只插零宽字符 | 400 / 11128 |
| **整张改写表全上** | **200** |

所以闸门同时匹配多段文本,任何单一变换都不够。插件因此每次请求都跑完整张表(`internal/channelgate` 的 `gatePhrases`),并保留一条三级兜底阶梯,只在上游仍然回 11128 时逐级花掉:

1. 在每个被扫描的 message 前加强制中性开场;
2. 在句号/逗号/专有名词边界插零宽空格;
3. 同上,但词与词之间全插(最贵,10.8k 字的 prompt 会涨约 70%)。

被扫描的 role 是 `system` 和 `assistant`;`user` / `tool` 不动——改用户自己的话只会改变对话,换不来任何闸门收益。

除内容外还有两条形状要求,插件都会满足:`messages[0]` 必须是 `system`(缺了就补 `You are a helpful assistant.`,空串会被 kimi 系回 `11151`),以及 `developer` role 会被改写成 `system`。

属于 cat-and-mouse:上游哪天加新指纹,得跟着改 `gatePhrases`;兜底阶梯负责那些还没被测出来的。

## 出站身份

上游按「这条请求像不像官方客户端」做风控,插件因此对齐官方的三族头:

- **会话三元组**:`X-Conversation-ID`(每次新 UUID)+ `X-Conversation-Request-ID` / `X-Root-Request-ID` / `X-Conversation-Message-ID`(同一个 32 hex 值)。缺了它,国际版被抽样时会回 11128「conversation id missing」——这正是「某个账号忽然不能用了」的真身。
- **归属头**:`X-Agent-Purpose` / `X-IDE-Name` / `X-IDE-Type` / `X-Product` 同值 `WorkBuddy`,外加 `X-CodeBuddy-Request` 与按区域切的 `Accept-Language`。官方客户端不会一个头说自己是 WorkBuddy、另一个头说自己是 CLI。
- **设备对**:`X-Machine-ID` / `X-Session-ID`,按账号 uid 派生(`internal/wire`),两个账号在同一台机器上也不会看起来是同一台设备;uid 只做哈希,不上线。

## Claude Code 兼容性

腾讯 CodeBuddy 的内容审核把 Claude Code 的两句固定 system 模板逐字加进了黑名单,命中即回"敏感内容"拒答:

- `You are Claude Code, Anthropic's official CLI for Claude.`(身份句)
- `Main branch (you will usually use this for PRs)`(git 注入句)

任何一字改动都绕过(精确匹配,非语义审核)。这两句的改写现在是 `internal/channelgate` 里 `claudeCodePhrases` 的一部分,和上面那张表一起在每次请求上跑。

## 思考模式

混元系列(`hy3` / `hy3-x` / `hy4-preview` / `hy4-preview-x` 等 `hy3`、`hy4` 前缀模型)自动开最大思考:workbuddy 转发前强制 `reasoning_effort=high`,覆盖客户端任何设置。CodeBuddy 只对 `high` 真正开深度思考(`medium` / `max` / `xhigh` 等档位它直接忽略),所以这已是 hy3 能用的最高档。思考内容走 SSE 的 `delta.reasoning_content`,客户端要支持渲染思考块才看得到。

## 流式

真流式(async):转发上游时边读边通过 `host.stream.emit` 把每个 chunk 实时推给 CPA,客户端逐字收到(不是等收齐了一股脑)。hy3 几千字的思考过程也是实时流出的,不是憋半天再刷出来。

## License

MIT。
