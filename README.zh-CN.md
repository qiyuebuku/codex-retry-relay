# steady-relay

**[English](README.md)**

**Codex 的自动重试中间人代理** —— 在 Codex 客户端与服务器之间架设一道透明防线，
自动拦截容量错误（429 / "Selected model is at capacity"）并指数退避重试，
让你的任务永远不因瞬时错误而中断。

> **⚠️ 先把话说清楚：本工具不解决 429 的根本问题。**
> 如果你的 Codex 账号被服务端标记为低权重/限流，429 本身会持续存在——任何代理都无法改变
> 服务端对你账号的配额判定。steady-relay 能做的是**尽可能降低 429 带来的影响**：
> 自动重试、不打断进行中的任务、免手动点击重试，**让你少受点罪**。

## 实战战况（真实生产数据）

一名重度用户 **2 天全量流量**的真实统计（非模拟）：

| 指标 | 数值 |
|---|---|
| 累计代理请求 | **5,300+** |
| 整体成功率 | **97.5%**（同等流量若无重试保护，估算仅 88.4%） |
| 被自动重试救回的请求 | **487 个**（约每 11 个请求就有 1 个） |
| 最长硬扛后成功 | 单请求 12 次尝试、近 7 分钟，最终完整交付 |
| 流中失败拦截率（全量缓冲模式） | **100%** |
| 代理本体 | 单进程常驻、零重启、内存约 2MB、对客户端完全透明 |

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
- 📊 **内置指标报表**：程序自身每隔几分钟输出一次 增量/今日/累计 三档指标——
  调用、成功、救回、成功率、在途与卡死检测，无需任何外部工具
- ⚡ **Go 开发**：单个静态二进制、零运行时依赖、常驻内存约 2MB、开箱即用
- 🖥 **跨平台**：macOS（Apple Silicon/Intel）、Windows、Linux，六种架构预编译

### 指标报表示例（程序运行时自动输出）

```
[stats] 📊 2026/09/15 16:13:00
[stats] [增量 3m] 调用 +25 | 成功 +25 | 在途 0 | 救回 +3 | 成功率 100.0%
[stats] [今日] 调用 1894 | 成功 1844 | 失败(未救回) 26 | 救回 246 | 成功率 97.4%（若无重试保护 84.4%）
[stats] [累计] 调用 4867 | 成功 4741 | 失败(未救回) 28 | 救回 361 | 成功率 97.4%（若无重试保护 90.0%）
[stats] 卡死: 0
```

（增量=距上次报表 / 今日 / 累计=进程启动以来；"若无重试保护"=同等流量不装本代理的估算成功率，
两者差值就是它帮你挡掉的；卡死=超过 10 分钟无进展的在途请求。）

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

新开一个 Codex 会话发条消息，relay 窗口会滚出 `[request] ... forwarding`，
随后每几分钟自动打印一份 `[stats]` 指标报表。上游容量错误发生时，
你会看到 `[retry] ...` 后请求成功——Codex 界面全程无感。

## 配置参考

| 环境变量 | 等效参数 | 默认 | 说明 |
|---|---|---|---|
| `UPSTREAM_BASE_URL` | `--upstream` | 无（必填） | 订阅模式固定 `https://chatgpt.com/backend-api/codex` |
| `BUFFER_UNTIL_SUCCESS` | `--buffer-until-success` | `false` | `true`=响应攒完整才交付，流中失败 100% 可重试（无打字机） |
| `MAX_RETRIES` | `--max-retries` | `10` | 重试次数上限（40 ≈ 极端拥堵硬扛约 4 分钟） |
| `PASSTHROUGH` | `--passthrough` | `false` | 订阅模式必须开启（上游无 `/v1` 前缀） |
| `STATS_INTERVAL` | `--stats-interval` | `3m` | 指标报表输出间隔，`0` 关闭 |
| `LISTEN_ADDR` | `--listen` | `127.0.0.1:8080` | 本地监听地址 |
| `RETRY_BACKOFF` | `--retry-backoff` | `500ms` | 指数退避起始值 |
| `MAX_RETRY_AFTER` | `--max-retry-after` | `1m` | 单次退避上限 |

### 后台常驻运行（Linux · systemd）

程序跑在自己的终端窗口里难免误关，推荐交给 systemd 管理（崩溃自动拉起、开机自启）。
仓库提供现成模板（[deploy/](deploy/)）：

```bash
# 1. 放置程序与配置（示例目录 ~/steady-relay/，按实际解压位置调整）
cp deploy/relay.env          ~/steady-relay/
cp deploy/steady-relay.service ~/.config/systemd/user/
#    编辑 service 文件：把 YOU 替换成你的用户名、核对两处路径

# 2. 启用 + 开机自启
systemctl --user daemon-reload
systemctl --user enable --now steady-relay
loginctl enable-linger $USER    # 可选：未登录也常驻

# 3. 日常管理
systemctl --user status steady-relay     # 看状态
systemctl --user restart steady-relay    # 改完 relay.env 后重启生效
```

macOS 可将启动命令加入"登录项"或使用 launchd；Windows 可用任务计划程序或 NSSM。

### 日志查看

systemd 模式下所有输出写入程序目录的 `relay.log`（内容与前台运行完全一致）：

```bash
tail -f relay.log                                        # 全部：请求 + 重试 + 报表
tail -f relay.log | grep --line-buffered '\[stats\]'     # 只看每 3 分钟的指标报表
tail -f relay.log | grep --line-buffered -v '\[stats\]'  # 只看流量明细（不要报表）
grep '\[stats\]' relay.log | tail -5                     # 随时看最新一份报表
```

建议加两个别名到 `~/.bashrc`，以后敲 `relay` 看报表、`relaylog` 看流量：

```bash
echo "alias relay='tail -f ~/steady-relay/relay.log | grep --line-buffered stats'" >> ~/.bashrc
echo "alias relaylog='tail -f ~/steady-relay/relay.log | grep --line-buffered -v stats'" >> ~/.bashrc
```

### 日志与观测

每个请求一行 `[request]/[response]`：状态码、尝试次数、耗时、首字节、终止事件；
重试时 `[retry]` 记录原因与退避时长；`attempts=N` 即被重试救回的请求；
`[stats]` 为周期性指标报表。

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
| 6 | 内置指标报表 | 运行状况开箱可观测 |

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
