# cli-proxy

把三家 AI 编码服务的上游封装成 [CLIProxyAPI](https://github.com/router-for-me/CLIProxyAPI)(CPA) 插件，让任何支持 OpenAI 协议的客户端（Claude Code、Cursor、Cline、SDK……）都能直接调用它们背后的模型。

一个仓库，四个 provider，四个独立产物。

| 目录 | provider | 上游 | 登录方式 | 说明 |
|---|---|---|---|---|
| [`qoder/`](qoder/README.md) | `qoder` | Qoder（国内 / 国际） | PKCE 设备码登录，或填 PAT | 模型动态发现；请求走 COSY 签名 |
| [`cline/`](cline/README.md) | `cline` | Cline | WorkOS 设备码登录 | 模型公开接口，无需登录即可拉取 |
| [`workbuddy/`](workbuddy/README.md) | `workbuddy` | 腾讯 CodeBuddy（国内） | CodeBuddy 网页扫码登录 | 模型动态发现；自动绕过内容审查黑名单 |
| [`workbuddy-intl/`](workbuddy-intl/README.md) | `workbuddy-intl` | WorkBuddy 国际版 | WorkBuddy 网页扫码登录 | 同上，但上游额外强制首条消息为 system |

`workbuddy` 和 `workbuddy-intl` 是同一份代码的两个构建目标（只差上游域名 / UA / platform 参数），但**上游校验规则不同**，不能互相替代；差异见 [`workbuddy-intl/README.md`](workbuddy-intl/README.md) 的「上游校验」。

各 provider 的协议细节、字段含义、坑点都写在各自目录的 README 里，改代码前先看那个。

## 为什么是多个产物而不是一个

不是偷懒——CPA 的插件 ABI 限制**一个 `.so` 只能注册一个 provider**：

```go
// internal/pluginhost/auth_provider.go
identifier, _ := h.callAuthProviderIdentifier(record.id, authProvider)
if identifier == provider { return &record }
```

`auth.identifier` 只能返回一个字符串，硬合会坏掉 OAuth 登录路由、`auth.refresh` 路由和模型注册。所以是"一个仓库，四个构建目标"。

## 前置条件

- 运行中的 CLIProxyAPI v7.2.x（**带 CGO / 插件支持**）
- Go 1.26+
- 各 provider 对应的账号

交叉编译到 Windows 还需要 mingw：`x86_64-w64-mingw32-gcc`（Ubuntu 上是 `apt install gcc-mingw-w64-x86-64`）。

## 构建

在**仓库根目录**执行。

### 前置依赖（Ubuntu / Debian）

Ubuntu 24.04 自带的 `golang-go` 是 1.22，**版本不够**（`go.mod` 要求 `go 1.26.0`），要单独装 Go：

```bash
# Go 1.26+（1.27.1 实测可用）
curl -fsSL https://go.dev/dl/go1.27.1.linux-amd64.tar.gz -o /tmp/go.tgz
sudo rm -rf /usr/local/go && sudo tar -C /usr/local -xzf /tmp/go.tgz
export PATH=$PATH:/usr/local/go/bin
go version

# Windows 交叉编译器（提供 /usr/bin/x86_64-w64-mingw32-gcc）
sudo apt update && sudo apt install -y gcc-mingw-w64-x86-64

# 国内网络
export GOPROXY=https://goproxy.cn,direct
```

> 不想换 Go 也可以只装 `apt install golang-go`（1.22），Go 1.21+ 默认开启 `GOTOOLCHAIN=auto`，会自动去模块代理拉 1.26 工具链 —— 前提是 `GOPROXY` 能通。
>
> 编 Windows **arm64** 的 DLL 要装 `gcc-mingw-w64-aarch64`，`CC` 换成 `aarch64-w64-mingw32-gcc`，`GOARCH` 换成 `arm64`。

### Windows（.dll）

```bash
CGO_ENABLED=1 GOOS=windows GOARCH=amd64 CC=x86_64-w64-mingw32-gcc \
  go build -buildmode=c-shared -o qoder.dll ./qoder

CGO_ENABLED=1 GOOS=windows GOARCH=amd64 CC=x86_64-w64-mingw32-gcc \
  go build -buildmode=c-shared -o cline.dll ./cline

CGO_ENABLED=1 GOOS=windows GOARCH=amd64 CC=x86_64-w64-mingw32-gcc \
  go build -buildmode=c-shared -o workbuddy.dll ./workbuddy

CGO_ENABLED=1 GOOS=windows GOARCH=amd64 CC=x86_64-w64-mingw32-gcc \
  go build -buildmode=c-shared -ldflags="-s -w" -o workbuddy-intl.dll ./workbuddy-intl
```

### Linux（.so）

```bash
CGO_ENABLED=1 GOOS=linux GOARCH=amd64 go build -buildmode=c-shared -o qoder.so ./qoder
CGO_ENABLED=1 GOOS=linux GOARCH=amd64 go build -buildmode=c-shared -o cline.so ./cline
CGO_ENABLED=1 GOOS=linux GOARCH=amd64 go build -buildmode=c-shared -o workbuddy.so ./workbuddy

CGO_ENABLED=1 GOOS=linux GOARCH=amd64 go build -buildmode=c-shared -o workbuddy-intl.so ./workbuddy-intl
```

### macOS（.dylib）

```bash
CGO_ENABLED=1 GOOS=darwin GOARCH=arm64 go build -buildmode=c-shared -o qoder.dylib ./qoder
CGO_ENABLED=1 GOOS=darwin GOARCH=arm64 go build -buildmode=c-shared -o cline.dylib ./cline
CGO_ENABLED=1 GOOS=darwin GOARCH=arm64 go build -buildmode=c-shared -o workbuddy.dylib ./workbuddy

CGO_ENABLED=1 GOOS=darwin GOARCH=arm64 go build -buildmode=c-shared -o workbuddy-intl.dylib ./workbuddy-intl
```

> 架构必须跟**跑 CPA 的那台机器**一致，不是跟编译机一致。amd64 / arm64 别搞错。
> 每次 `go build` 还会生成一个同名的 `.h`，那是 cgo 的头文件，不用管，`.gitignore` 已忽略。
> 发布用的产物加 `-ldflags="-s -w"` 去掉符号表，体积从 ~13.5MB 降到 ~7MB（上面各平台命令同样适用）。

## 装进 CPA

### 1. 放产物

把 `.dll` / `.so` / `.dylib` 复制到 CPA 的 `plugins/` 目录。

**文件名决定插件 ID**，必须跟下面 `config.yaml` 里的名字对上：`qoder.dll` → 插件 ID `qoder`。

### 2. 改 config.yaml

```yaml
plugins:
  enabled: true
  dir: "plugins"
  configs:
    qoder:     { enabled: true, priority: 100 }
    cline:     { enabled: true, priority: 100 }
    workbuddy:      { enabled: true, priority: 100 }
    workbuddy-intl: { enabled: true, priority: 100 }
```

用哪个就开哪个，不用全开。

### 3. 重启 CPA

日志里看到 `plugin loaded ... plugin_id=qoder` 这类字样就是加载成功了。

### 4. 添加凭据

- **qoder / cline**：CPA 面板点「开始登录」，会给出授权链接，浏览器打开登录并**选完账号**
- **workbuddy / workbuddy-intl**：CPA 面板扫码登录
- **qoder 也可以手动填**：在 CPA 的 auth 目录放 `qoder.json`，内容 `{"variant":"cn","personalToken":"pt-..."}`

具体步骤见各自目录的 README。

### 5. 验证

```bash
# 看模型列表（qoder 的模型要登录后才有；workbuddy / workbuddy-intl 无需登录即可拉取）
curl http://localhost:8317/v1/models -H "Authorization: Bearer <api-key>"

# 发一条消息
curl http://localhost:8317/v1/chat/completions \
  -H "Authorization: Bearer <api-key>" -H "Content-Type: application/json" \
  -d '{"model":"Qwen3.8-Max","messages":[{"role":"user","content":"你好"}],"stream":true}'
```

CPA 默认端口 `8317`，API key 在 `config.yaml` 的 `api-keys`。

## 客户端怎么配

| 协议 | Base URL |
|------|----------|
| OpenAI | `http://<host>:8317/v1` |
| Anthropic | `http://<host>:8317`（不带 `/v1`，走 `x-api-key`） |

## 多账号

同一个 provider 可以加多个账号，CPA 自己会在它们之间调度。界面上靠账号名区分——插件会把账号身份写进 `Metadata["email"]`（列表显示）和 `FileName`（`qoder-<账号>.json`，详情显示）。

## 排错：客户端说"用不了"，但插件日志里什么都没有

先看 CPA 的错误信息属于哪一层，三层的处置完全不同：

| 错误 | 谁拒绝的 | 处置 |
|---|---|---|
| `auth_not_found: no auth available (providers=workbuddy, model=…)` | CPA 路由层，**请求没到插件** | 该 provider 下没有可用凭据，见下 |
| `auth_unavailable: … last upstream error: upstream 400 {"code":11133"…}` | CPA 收到上游 400 后把凭据拉黑了 | 先修请求（本仓库已处理 11133），再重新启用账号 |
| `upstream 400: {"code":11128…}` | 上游渠道校验 | 见各 provider README 的「渠道校验」 |

`auth_not_found` 是最容易被误判成"插件坏了"的一种：**它意味着插件根本没被调用**。凭据 JSON 里的 `"disabled": true` 就会造成这个结果——常见来源是 CPA 因为上游 400 自动拉黑，或手工禁用后忘了开回来。

排查顺序：

```bash
# 1. 看凭据是不是被禁用了（disabled 为 true 就是它）
jq '.disabled' ~/.cli-proxy-api/workbuddy-*.json

# 2. 绕过 CPA 直接打上游，确认 token 本身还有效
#    （有 200 就说明账号没问题，纯粹是 CPA 侧没启用）

# 3. 启用后 CPA 会热加载，日志里有 auth file changed (WRITE) 即可，不必重启
```

一个账号被拉黑会连坐它下面的**所有**模型，症状看起来像"整个 provider 挂了"，实际只是那一个 JSON 的 `disabled` 字段。

## 目录结构

```
cli-proxy/
├── go.mod                 四个 provider 共用一个模块
├── go.sum
├── internal/
│   ├── channelgate/       两家上游共用的「渠道校验」绕过逻辑（11128）
│   └── wire/              出站身份：会话三元组、归属头、设备对
├── qoder/
│   ├── main.go            C ABI、RPC 分发
│   ├── cosy.go            自定义 base64、RSA/AES、COSY 签名
│   ├── auth.go            凭据结构、PAT 换 job token
│   ├── login.go           PKCE 设备码登录、userinfo
│   ├── models.go          模型动态发现
│   ├── executor.go        请求体构建、SSE 解析
│   ├── upstream.go        域名、签名请求
│   ├── baseprompt.json    上游请求骨架（go:embed）
│   └── README.md
├── cline/
│   ├── main.go
│   └── README.md
├── workbuddy/
│   ├── main.go
│   └── README.md
└── workbuddy-intl/
    ├── main.go           同一份逻辑，换国际版上游
    └── README.md
```

`internal/` 下的两个包是 workbuddy / workbuddy-intl 共用的：`channelgate` 负责过「渠道校验」（`code 11128`），`wire` 负责让出站请求长得像官方客户端。两家的上游校验规则不同，但这两块是同一份实测结论，改一处两边一起生效。

## 测试

```bash
go test ./internal/...    # 渠道闸门与出站身份的单元测试，不需要 CGO
```

## 安全

- 凭据文件（`*-<账号>.json`）含 access/refresh token，`.gitignore` 已忽略，**不要提交**
- 各 provider 的 token 都会过期（qoder 的 job token 约一天、设备令牌 30 天），插件通过 `auth.refresh` 自动续期，续期时会轮换，旧的立刻失效

## License

MIT。
