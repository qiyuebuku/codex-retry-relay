# steady-relay（ChatGPT 订阅适配 fork）

基于开源项目 [937204197/steady-relay](https://github.com/937204197/steady-relay) v2.2.0 的二次开发版本，
专用于代理 **ChatGPT 订阅登录的 Codex**（`chatgpt.com/backend-api/codex`）：容量错误
（"Selected model is at capacity"）自动重试救回，任务不再被打断。

原版只适用于 API-key 型 OpenAI 兼容上游；本 fork 加了 5 个补丁使其支持订阅模式。

## 补丁清单（相对上游 v2.2.0）

| # | 补丁 | 解决的问题 |
|---|---|---|
| 1 | `--passthrough` / `PASSTHROUGH` | 跳过本地路径必须 `/v1` 前缀的校验（订阅后端端点无 `/v1`） |
| 2 | `retryableEmbeddedError` | HTTP 200 + JSON 错误体（容量/限流/超时类）也触发重试 |
| 3 | `bodyLooksLikeSSE` | 嗅探 `text/plain` 伪装的 SSE 流（chatgpt 后端特性），使其进入流式重试路径 |
| 4 | 工具参数帧缓冲 | `custom_tool_call_input.delta` / `function_call_arguments.delta` 不提前提交，流中失败可重试 |
| 5 | `output_item.done` 缓冲 | 完整输出项同样缓到 `response.completed`，扩大可重试窗口 |

## 构建

```bash
go build -trimpath -ldflags "-X main.buildVersion=自定义版本" -o steady-relay .
# 多平台发布包（6 平台 + SHA256SUMS）
sh scripts/build-release.sh <版本号>
```

## 运行

```bash
BUFFER_UNTIL_SUCCESS=true MAX_RETRIES=40 \
UPSTREAM_BASE_URL=https://chatgpt.com/backend-api/codex \
./steady-relay --listen 127.0.0.1:8080 --passthrough
```

| 配置 | 说明 |
|---|---|
| `BUFFER_UNTIL_SUCCESS` | `true`=响应攒完整才交付（流中失败 100% 可重试，无打字机）；`false`=实时流式 |
| `MAX_RETRIES` | 重试次数上限（40 ≈ 极端硬扛 4 分钟） |
| `--passthrough` | 订阅模式必须 |
| `UPSTREAM_BASE_URL` | 订阅后端固定为 `https://chatgpt.com/backend-api/codex` |

生产部署建议 systemd 用户服务（`Restart=always` + linger），配置经 `EnvironmentFile` 注入。

## 配套的 Codex 配置（必须）

`~/.codex/config.toml`：

```toml
model_provider = "steady_relay"

[model_providers.steady_relay]
base_url = "http://127.0.0.1:8080"   # 末尾不能带 /v1
wire_api = "responses"
requires_openai_auth = true           # 沿用 ChatGPT 订阅登录
supports_websockets = false           # 必须关闭，否则 WS 405 重连风暴
```

改后重启 Codex（配置仅启动时读取）。**旧会话线程保留创建时的 provider（直连），
仅新建会话走代理。**

## 已知边界

- 可见文本实时流式模式下（`BUFFER_UNTIL_SUCCESS=false`），思考摘要/正文已输出后的流中失败
  无法重试（重试必重复内容），会透传给 Codex
- 重试耗尽（如 40 次全拒）时透传最终失败，重发即解
- 仅代理 HTTP/SSE；不代理 WebSocket（走 WS 会绕过重试保护，故配置中强制关闭）

## 致谢

上游作者 [937204197](https://github.com/937204197)。原版的重试/退避/流式转发机制全部保留，
本 fork 只做订阅模式适配。License 沿用上游。
