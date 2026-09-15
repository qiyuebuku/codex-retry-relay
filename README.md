# steady-relay

**[中文文档](README.zh-CN.md)**

**An automatic-retry man-in-the-middle proxy for Codex** — a transparent layer between
the Codex client and its backend that intercepts capacity errors
(429 / "Selected model is at capacity") and retries with exponential backoff,
so your work is never interrupted by transient failures.

> **⚠️ To be clear up front: this tool does NOT fix the root cause of 429s.**
> If your Codex account is deprioritized or rate-limited server-side, the 429s themselves
> will keep happening — no proxy can change how the backend meters your account.
> What steady-relay does is **minimize the impact**: automatic retries, no interrupted
> tasks, no manual retry clicking. **It makes 429s far less painful — nothing more.**

## Battle-Tested in Production

Numbers from two days of real production use (one heavy Codex user, 100% of traffic):

| Metric | Value |
|---|---|
| Total requests proxied | **5,300+** |
| Overall success rate | **97.5%** (estimated 88.4% for the same traffic without retries) |
| Requests rescued by auto-retry | **487** (about 1 in every 11 requests) |
| Longest grind to success | 12 attempts over ~7 minutes, delivered intact |
| Mid-stream failure interception (buffering mode) | 100% |
| Proxy itself | single process, zero restarts, ~2 MB RSS, fully transparent |

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
- 📊 **Built-in metrics report**: every few minutes the proxy itself logs delta/today/
  lifetime stats — calls, successes, rescues, success rate, in-flight and stuck requests.
  No external tooling needed.
- ⚡ **Written in Go**: single static binary, zero dependencies, ~2 MB RSS
- 🖥 **Cross-platform**: macOS (Apple Silicon & Intel), Windows, Linux — 6 prebuilt targets

### Sample metrics output

```
[stats] 📊 2026/09/15 16:13:00
[stats] [增量 3m] 调用 +25 | 成功 +25 | 在途 2（重试中 1）| 救回 +3 | 成功率 100.0%
[stats] [今日] 调用 1894 | 成功 1844 | 失败(未救回) 26 | 救回 246 | 成功率 97.4%（若无重试保护 84.4%）
[stats] [累计] 调用 4867 | 成功 4741 | 失败(未救回) 28 | 救回 361 | 成功率 97.4%（若无重试保护 90.0%）
[stats] 卡死: 0
```

(Delta since the previous report / today / since first run. `若无重试保护` =
the success rate the same traffic would have had without retries — the gap is what
the proxy bought you. Stuck = requests with no progress for over 10 minutes.
Counters are persisted to `steady-relay.stats.json` on every report, so they
**survive restarts**; "today" resets on a new day while "lifetime" keeps accumulating.)

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
| `STATS_INTERVAL` | `--stats-interval` | `3m` | Metrics report interval; `0` disables |
| `LISTEN_ADDR` | `--listen` | `127.0.0.1:8080` | Listen address |
| `RETRY_BACKOFF` | `--retry-backoff` | `500ms` | Initial backoff |
| `MAX_RETRY_AFTER` | `--max-retry-after` | `1m` | Backoff ceiling |

### Run as a Background Service (Linux · systemd)

Running in a terminal window is easy to close by accident. The recommended setup is a
systemd user service (auto-restart, starts at boot). Ready-made templates are in
[deploy/](deploy/):

```bash
# 1. Place the binary and config (example dir ~/steady-relay/, adjust to yours)
cp deploy/relay.env              ~/steady-relay/
cp deploy/steady-relay.service   ~/.config/systemd/user/
#    Edit the service file: replace YOU with your username, check both paths

# 2. Enable + start at login
systemctl --user daemon-reload
systemctl --user enable --now steady-relay
loginctl enable-linger $USER     # optional: keep running when logged out

# 3. Day-to-day
systemctl --user status steady-relay    # status
systemctl --user restart steady-relay   # restart after editing relay.env
```

On macOS add the start command to Login Items or use launchd; on Windows use Task
Scheduler or NSSM.

### Viewing Logs

Under systemd all output is appended to `relay.log` in the program directory
(identical to what you would see in a foreground terminal):

```bash
tail -f relay.log                                        # everything: requests + retries + reports
tail -f relay.log | grep --line-buffered '\[stats\]'     # metrics reports only (every 3 min)
tail -f relay.log | grep --line-buffered -v '\[stats\]'  # traffic only, no reports
grep '\[stats\]' relay.log | tail -5                     # latest report on demand
```

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

- Upstream project: [937204197/steady-relay](https://github.com/937204197/steady-relay) (v2.2.0),
  with adaptations for the ChatGPT subscription backend (path passthrough, 200-wrapped
  error retry, SSE body sniffing, buffered tool-call frames)
- License inherited from upstream — see [LICENSE](LICENSE).
