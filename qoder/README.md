# qoder-proxy

把 **Qoder**（`qoder.sh` / `qoder.com.cn`）封装成 [CLIProxyAPI](https://github.com/router-for-me/CLIProxyAPI)(CPA) 插件，任何支持 OpenAI 协议的客户端（Claude Code、Cursor、Cline、SDK……）都能调用 Qoder 背后的模型。

上游协议来自 [qoder2api](https://github.com/emptysuns/qoder2api)（Node.js 桥接服务），本插件是它核心能力的 Go 重新实现，按 cliproxy 插件 ABI 重写。

## 工作原理

在 CPA 里注册为 `qoder` provider：用个人访问令牌（PAT）在认证中心换一个短期的 job token，再用 RSA + AES 构造会话密钥、给每个请求打 COSY 签名，最后转发到 Qoder 的 `agent_chat_generation` SSE 端点。凭据存为 `qoder.json`。

## 国内版 / 国际版

两个版本是两套独立的部署，除域名外完全一致，所以凭据里只记一个 `variant`：

| variant | 认证中心 | API | 模型列表 |
|---|---|---|---|
| `intl`（默认） | `https://center.qoder.sh` | `https://api3.qoder.sh` | `https://api1.qoder.sh` |
| `cn` | `https://gateway.qoder.com.cn` | `https://gateway.qoder.com.cn` | `https://gateway.qoder.com.cn` |

未指定或非 `cn` 的值都按国际版处理，这与上游自己的默认值一致。

## 安装

**前置**：运行中的 CLIProxyAPI v7.2.x（带 CGO / 插件支持）、Qoder 账号、Go 1.26+ 与 gcc；编译架构需与 CPA 实例一致（amd64 / arm64）。

```bash
git clone https://github.com/lovingfish/qoder-proxy.git
# 在本仓库根目录执行
CGO_ENABLED=1 GOOS=linux GOARCH=amd64 \
  go build -buildmode=c-shared -o qoder.so ./qoder
```

产物：`.so`（Linux）/ `.dylib`（macOS）/ `.dll`（Windows）。放到 CPA 的 `plugins/` 目录，在 `config.yaml` 启用：

```yaml
plugins:
  enabled: true
  dir: "plugins"
  configs:
    qoder: { enabled: true, priority: 100 }
```

## 添加凭据

### 方式一：面板点"开始登录"（推荐）

插件实现了 Qoder 官方的 **PKCE 设备码登录**，就是 `qodercn` CLI 用的那套：

1. 点 CPA 面板上的「开始登录」，会拿到一个链接，形如
   `https://qoder.com.cn/device/selectAccounts?machine_id=…&challenge=…&challenge_method=S256&nonce=…`
2. 浏览器打开，登录并**选择要用的账号**
3. 插件自动轮询 `deviceToken/poll` 拿到 device token，再换成 job token 存下

`challenge` 是 `base64url(SHA256(verifier))`，`verifier` 只留在插件内存和 CPA 的登录 state 里，不会上传。

### 方式二：手动放 `qoder.json`

在 CPA 的 auth 目录放一个 `qoder.json`：

```json
{ "variant": "cn", "personalToken": "pt-xxxxxxxx" }
```

国际版把 `variant` 改成 `intl` 或整个字段去掉。

### PAT 从哪来（方式二用）

Qoder CLI 登录后会把它缓存在本地，用同目录的机器 ID 解密得到。原文项目读的就是这个文件：

```bash
# 国内版：~/.qoder-cn/.auth/   国际版：~/.qoder/.auth/
node -e "
const {readUserInfo} = await import('./src/local-auth.js');
console.log(readUserInfo().personal_access_token);
" --input-type=module
```

（在 qoder2api 仓库目录下执行；国内版会优先读 `~/.qoder-cn`。）

## 模型

模型列表**不硬编码**，从 `GET /algo/api/v2/model/list` 拉取，按账号缓存 5 分钟。

这个接口**需要签名会话**，也就是说没有凭据时读不到。所以插件注册阶段（`model.static`）返回空列表——**模型要等添加凭据之后才会出现在 `/v1/models`**。这是协议决定的，不是 bug。

发布的是上游 `chat` 分组，条目形如：

| 发布 id | 上游 key | 说明 |
|---|---|---|
| `Qwen3.8-Max` | `qmodel_38max` | |
| `Qwen3.7-Max` | `qmodel_latest` | 默认，强制开推理 |
| `DeepSeek-V4-Pro` | `dmodel` | |
| `GLM-5.3-Flash` | `gfmodel` | |
| `Kimi-K2.7-Code` | `kmodel` | |

客户端可以填发布 id 也可以直接填上游 key，插件会自动解析。

上游每个模型带 `enable` 字段表示**当前套餐能否使用**，免费版账号下大部分是 `false`。插件按原样全量发布（上游有什么就显示什么），不做过滤——调用未启用的模型会得到上游的错误。

## 使用

CPA 默认端口 `8317`，API key 见 `config.yaml` 的 `api-keys`。

| 协议 | Base URL |
|------|----------|
| OpenAI | `http://<host>:8317/v1` |
| Anthropic | `http://<host>:8317`（不带 `/v1`，走 `x-api-key`） |

```bash
curl http://localhost:8317/v1/chat/completions \
  -H "Authorization: Bearer <api-key>" -H "Content-Type: application/json" \
  -d '{"model":"Qwen3.7-Max","messages":[{"role":"user","content":"你好"}],"stream":true}'
```

上游只提供 SSE，客户端请求非流式时插件会在内部流式拉取再聚合成一个 `chat.completion`。

## 实现上必须知道的五点

1. **请求不是简单 Bearer，是 COSY 签名**：随机 AES key 用 RSA 公钥包成 `cosyKey`，身份信息用 AES-128-CBC（key 即 IV）加密成 `info`，每个请求再算 `md5(payload.cosyKey.date.body.path)`。
2. **认证中心用另一套静态签名**：`md5("cosy" & secret & RFC1123 date)`，请求体还是自定义 base64 变体（标准 base64 之后做三段轮转 + 字母表替换）。
3. **两种凭据，两套续期**：设备登录拿到的 `dt-` 有效期 30 天，续期走 `deviceToken/refresh`；PAT 换来的 `jt-` 只有一天，续期走认证中心。两者**每次刷新都会轮换**，旧的立刻失效，所以必须把新令牌写回 `StorageJSON`。CPA 按 `NextRefreshAfter` 调度。
4. **`cosy-user` 头必须是 uid**。设备登录从 poll 的 `user_id` 拿（注意响应里的 `id` 是设备会话 id，不是 uid）；PAT 路径从认证中心响应拿；两者都能再调 `/api/v1/userinfo` 补显示名。不填 uid 时每个请求都会得到 `403 Signature invalid`。
5. **上游 SSE 是双层信封**：HTTP 状态永远 200，真正的错误在外层的 `statusCodeValue` 里（配额耗尽就是 `403` + `code 112`）。内层 `body` 是 JSON 字符串，解开后才是 `choices[].delta`。

## 验证到什么程度

**已实测通过**（用本机真实凭据）：
- PAT → job token 换发、COSY 会话构造、模型列表（14 个，字段与分组正确）
- 登录链接：`/device/selectAccounts` 返回 302 并带上我生成的全部参数跳到 `/users/sign-in`
- 轮询：用生成的 state 打 `deviceToken/poll` 返回 404（= 等待登录），被正确识别为 pending

**设备登录链路已实测**（真实登录 + 真实凭据，每一步都跑通）：
- 登录页 `/device/selectAccounts` → 302 → `/users/sign-in`，参数原样带回
- `deviceToken/poll` 需 `machine_id` + **`verifier`**（不是 challenge）→ 返回 `token` = `dt-…`（设备令牌，30 天）、`refresh_token` = `drt-…`、`user_id`
- **`dt-` 直接就能签名调 API，不需要任何 exchange** —— 实测拉到 14 个模型。`jobToken/exchange` 对设备令牌的各种请求形状全部返回 400
- 刷新走 `deviceToken/refresh` + `{"refresh_token":"drt-…"}` → 200，轮换出新的一对，刷新后照样能签名调用

PAT 那条路同样实测通过：`{"personal_token":"pt-…"}` → `jobToken/exchange` → `jt-` → 签名调用 14 个模型。

**登录区域默认是国内版**（`defaultLoginVariant = "cn"`）。`auth.login.start` 发生在凭据存在之前，读不到 variant，只能给个默认值。国际版用户改 `upstream.go` 里那个常量即可。

## 与 qoder2api 的差异

原项目是独立服务，本插件只保留 CPA 不提供的部分：

| 原项目能力 | 本插件 |
|---|---|
| PAT → job token → 签名转发 | ✅ 保留 |
| 动态模型列表 | ✅ 保留 |
| 工具调用（结构化 + 文本回退解析） | ✅ 结构化部分保留 |
| OpenAI / Claude / Gemini 三协议 | ❌ 只出 chat-completions，协议转换交给 CPA |
| 多 PAT 轮询、配额耗尽自动切换 | ❌ 交给 CPA 调度器 |
| `/v1/user/status` / `/v1/user/quota` | ❌ 不移植 |
| 接口密钥、代理支持 | ❌ 交给 CPA |
| 读本地 CLI 登录缓存 | ❌ 改为设备码登录或显式填 PAT（CPA 可能在容器里，读不到家目录） |

## License

MIT。
