# steady-relay

**Codex 的自动重试中间人代理** —— 在 Codex 客户端与服务器之间架设一道透明防线，
自动拦截容量错误（429 / "Selected model is at capacity"）并指数退避重试，
让你的任务永远不因瞬时错误而中断。

> **⚠️ 先把话说清楚：本工具不解决 429 的根本问题。**
> 如果你的 Codex 账号被服务端标记为低权重/限流，429 本身会持续存在——任何代理都无法改变
> 服务端对你账号的配额判定。steady-relay 能做的是**尽可能降低 429 带来的影响**：
> 自动重试、不打断进行中的任务、免手动点击重试，**让你少受点罪**。

[English](#english)

---

## 它解决什么问题

如果你在用 **ChatGPT 订阅登录的 Codex**，你大概率经历过：

- 高峰时段频繁弹出 *"Selected model is at capacity. Please try a different model."*
- 一次容量错误直接**杀掉整个正在进行的任务**——尤其是你设定好的长时间自治任务（goal task），
  跑了十几分钟，一个 429 全部归零
- 只能手动点重试，打断心流，还可能再次失败

**steady-relay 在 Codex 桌面端与 Codex 服务端之间充当中间人**：

```
Codex 桌面端 ──▶ steady-relay（本机） ──▶ chatgpt.com
                    │
                    ├─ 监测每一个请求/响应流
                    ├─ 发现可重试异常（429、5xx、容量错误、流中失败）
                    ├─ 指数退避自动重试（默认最多 40 次）
                    └─ 全程对 Codex 透明，无需任何人工干预
```

对使用端完全透明：**不打断正在进行的任务、不需要你手动点击重试**。
配合全量缓冲模式，可 **100% 拦截所有可重试场景**（重试次数设置充足时）。

## 核心特性

- 🔁 **自动重试**：覆盖 HTTP 429/5xx、伪装成 200 的 JSON 错误体、SSE 流开始前与流中间的失败
- ⏱ **指数退避**：遵循 `Retry-After`，退避上限可配；重试次数自定义（默认 40）
- 🫥 **完全透明**：Codex 无感知，凭据仍由 Codex 自己发送、代理原样转发，不落盘
- 🛡 **两种模式**：全量缓冲（流中失败 100% 可重试拦截）／ 实时流式（保留打字机效果）
- ⚡ **Go 开发**：单个静态二进制、零运行时依赖、常驻内存约 2MB、开箱即用
- 🖥 **跨平台**：macOS（Apple Silicon/Intel）、Windows、Linux，六种架构预编译

## 快速开始

### 1. 下载并启动

从 [Releases](../../releases) 下载对应平台的包，解压后：

**macOS**（M 系列芯片；Intel 机器用 amd64 包）：

```bash
tar -xzf steady-relay-*-darwin-arm64.tar.gz && cd steady-relay-*-darwin-arm64
xattr -d com.apple.quarantine steady-relay 2>/dev/null; chmod +x steady-relay
BUFFER_UNTIL_SUCCESS=true MAX_RETRIES=40 \
UPSTREAM_BASE_URL=https://chatgpt.com/backend-api/codex \
./steady-relay --listen 127.0.0.1:8080 --passthrough
```

**Windows**（PowerShell）：

```powershell
Expand-Archive steady-relay-*-windows-amd64.zip; cd steady-relay-*-windows-amd64
$env:BUFFER_UNTIL_SUCCESS="true"; $env:MAX_RETRIES="40"
$env:UPSTREAM_BASE_URL="https://chatgpt.com/backend-api/codex"
.\steady-relay.exe --listen 127.0.0.1:8080 --passthrough
```

**Linux**：同 macOS（无需 xattr 那步）。

看到 `listening on http://127.0.0.1:8080` 即成功，**保持窗口开着**。

### 2. 配置 Codex（关键步骤）

编辑 `~/.codex/config.toml`（Windows 为 `C:\Users\<用户名>\.codex\config.toml`）：

```toml
model_provider = "steady_relay"

[model_providers.steady_relay]
name = "steady-relay local retry proxy"
base_url = "http://127.0.0.1:8080"   # 末尾不要带 /v1
wire_api = "responses"
requires_openai_auth = true           # 沿用 ChatGPT 订阅登录，无需任何密钥
supports_websockets = false           # 必须关闭，否则 WS 握手会被拒导致重连风暴
```

然后**完全退出并重启 Codex**（配置仅在启动时读取）。

### 3. 验证

新开一个 Codex 会话发条消息，relay 窗口会滚出 `[request] ... forwarding`。
之后上游容量错误发生时，你会看到 `[retry] ...` 后请求成功——Codex 界面全程无感。

## 配置参考

| 环境变量 | 等效参数 | 默认 | 说明 |
|---|---|---|---|
| `UPSTREAM_BASE_URL` | `--upstream` | 无（必填） | 订阅模式固定 `https://chatgpt.com/backend-api/codex` |
| `BUFFER_UNTIL_SUCCESS` | `--buffer-until-success` | `false` | `true`=响应攒完整才交付，流中失败 100% 可重试（无打字机） |
| `MAX_RETRIES` | `--max-retries` | `10` | 重试次数上限（40 ≈ 极端拥堵硬扛约 4 分钟） |
| `PASSTHROUGH` | `--passthrough` | `false` | 订阅模式必须开启（上游无 `/v1` 前缀） |
| `LISTEN_ADDR` | `--listen` | `127.0.0.1:8080` | 本地监听地址 |
| `RETRY_BACKOFF` | `--retry-backoff` | `500ms` | 指数退避起始值 |
| `MAX_RETRY_AFTER` | `--max-retry-after` | `1m` | 单次退避上限 |

### 常驻运行（可选）

Linux 推荐 systemd 用户服务（`Restart=always` + `loginctl enable-linger` 开机自启），
配置经 `EnvironmentFile` 注入；macOS 可加入登录项或用 launchd。

### 日志与观测

每个请求一行 `[request]/[response]`：状态码、尝试次数、耗时、首字节、终止事件；
重试时 `[retry]` 记录原因与退避时长。`attempts=N` 即被重试救回的请求。

## 工作原理（相对上游的适配）

本项目基于开源项目 [937204197/steady-relay](https://github.com/937204197/steady-relay) v2.2.0
二次开发，原版的重试/退避/流式转发机制全部保留，新增对 ChatGPT 订阅后端的适配：

| # | 适配点 | 解决的问题 |
|---|---|---|
| 1 | `--passthrough` 路径透传 | 订阅后端端点无 `/v1` 前缀，原版强校验会 404 |
| 2 | 200 包错误体重试 | 订阅后端把容量错误伪装成 HTTP 200 + JSON 错误体 |
| 3 | SSE 体嗅探 | 订阅后端把 SSE 流标记为 `text/plain`，原版识别不出 |
| 4 | 工具参数帧缓冲 | 工具调用参数流中途失败可安全重试 |
| 5 | 输出项缓冲 | 扩大流中失败的可重试窗口 |

## 从源码构建

```bash
go build -trimpath -o steady-relay .          # 需要 Go ≥ 1.22
sh scripts/build-release.sh <版本号>            # 一次性构建全部 6 平台发布包
go test ./...                                  # 运行测试
```

## 已知边界

- **重试耗尽**：持续极端拥堵下（如连续数分钟全部拒绝）重试额度用尽仍会透传失败，重发即解
- **不代理 WebSocket**：WS 会绕过请求级重试保护，故配置中强制 `supports_websockets = false`
  （Codex 会自动回落到 SSE）
- **旧会话线程**保留创建时的连接方式（直连），仅新建会话走代理
- 需要 Codex 已通过 ChatGPT 账号登录（`~/.codex/auth.json` 存在）

## 致谢与许可

- 上游项目：[937204197/steady-relay](https://github.com/937204197/steady-relay)
- License 沿用上游，见 [LICENSE](LICENSE)。

---

# English

**An automatic-retry man-in-the-middle proxy for Codex** — a transparent layer between
the Codex client and its backend that intercepts capacity errors
(429 / "Selected model is at capacity") and retries with exponential backoff,
so your work is never interrupted by transient failures.

> **⚠️ To be clear up front: this tool does NOT fix the root cause of 429s.**
> If your Codex account is deprioritized or rate-limited server-side, the 429s themselves
> will keep happening — no proxy can change how the backend meters your account.
> What steady-relay does is **minimize the impact**: automatic retries, no interrupted
> tasks, no manual retry clicking. **It makes 429s far less painful — nothing more.**

## The Problem It Solves

If you use Codex with a ChatGPT subscription, you have probably seen:

- Frequent *"Selected model is at capacity"* errors during peak hours
- A single capacity error **killing an entire running task** — especially long-running
  autonomous/goal tasks that took minutes to build up
- Manual retry clicking that breaks your flow and may fail again

steady-relay sits between the Codex desktop client and the backend, watches every
request and response stream, and when it sees a retryable failure it silently retries
with exponential backoff (up to 40 attempts by default). **Fully transparent to the
client — no interrupted tasks, no manual retries.** With buffering mode enabled it can
intercept 100% of retryable scenarios.

## Key Features

- 🔁 **Automatic retries**: HTTP 429/5xx, capacity errors disguised as HTTP 200 + JSON
  error bodies, and failures before or mid-SSE-stream
- ⏱ **Exponential backoff**, honors `Retry-After`, configurable attempt count
- 🫥 **Transparent**: credentials stay in Codex and are forwarded verbatim
- 🛡 **Two modes**: full buffering (100% of mid-stream failures retryable) or live
  streaming (typewriter effect preserved)
- ⚡ **Written in Go**: single static binary, zero dependencies, ~2 MB RSS
- 🖥 **Cross-platform**: macOS (Apple Silicon & Intel), Windows, Linux — 6 prebuilt targets

## Quick Start

1. Download your platform archive from [Releases](../../releases), unpack, and run:

```bash
BUFFER_UNTIL_SUCCESS=true MAX_RETRIES=40 \
UPSTREAM_BASE_URL=https://chatgpt.com/backend-api/codex \
./steady-relay --listen 127.0.0.1:8080 --passthrough
```

(macOS: run `xattr -d com.apple.quarantine steady-relay` first if Gatekeeper blocks it.
Windows: use `steady-relay.exe` in PowerShell with the same environment variables.)

2. Point Codex at it — add to `~/.codex/config.toml`:

```toml
model_provider = "steady_relay"

[model_providers.steady_relay]
base_url = "http://127.0.0.1:8080"   # no trailing /v1
wire_api = "responses"
requires_openai_auth = true
supports_websockets = false
```

3. Fully restart Codex, then send any message — the relay log should show
`[request] ... forwarding`. Retries appear as `[retry]` lines followed by success.

## Configuration

| Env | Flag | Default | Description |
|---|---|---|---|
| `UPSTREAM_BASE_URL` | `--upstream` | required | `https://chatgpt.com/backend-api/codex` for subscription mode |
| `BUFFER_UNTIL_SUCCESS` | `--buffer-until-success` | `false` | `true` = buffer full response, 100% mid-stream retry coverage |
| `MAX_RETRIES` | `--max-retries` | `10` | Retry attempt limit |
| `PASSTHROUGH` | `--passthrough` | `false` | Required for the subscription backend (no `/v1` prefix) |
| `LISTEN_ADDR` | `--listen` | `127.0.0.1:8080` | Listen address |
| `RETRY_BACKOFF` | `--retry-backoff` | `500ms` | Initial backoff |
| `MAX_RETRY_AFTER` | `--max-retry-after` | `1m` | Backoff ceiling |

## Building from Source

```bash
go build -trimpath -o steady-relay .     # Go ≥ 1.22
sh scripts/build-release.sh <version>    # build all 6 platform archives
go test ./...
```

## Known Limits

- Retry exhaustion under sustained extreme congestion still surfaces the final error
- WebSockets are not proxied by design (they would bypass request-level retries);
  Codex falls back to SSE automatically
- Existing Codex threads keep their original connection mode; only new threads use the proxy
- Requires Codex signed in with a ChatGPT account

## Credits & License

- Upstream project: [937204197/steady-relay](https://github.com/937204197/steady-relay) (v2.2.0)
- License inherited from upstream — see [LICENSE](LICENSE).
