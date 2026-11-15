# CLAUDE.md

Guidance for Claude Code working in this repository. The octo-agent equivalent is `.octorules`.

## ⚠️ 构建目标硬约束（本机定制 · 2026-11-08 · 优先级最高）

本副本是**主人专用的便携化桌面版**，产品名 **ATLR-Agent**（caoxiang 的重编译版本，基于 Octo），与上游发布形态不同。**任何打包/构建/交付操作都必须遵守本节，违反即任务驳回。**

1. **唯一打包目标 = 桌面版 `ATLR-Agent.exe`**（源码 `cmd/octo-desktop/`，Wails v3 GUI）。
   - **默认且唯一必需的产物形态 = 单 EXE 便携版**：`dist/portable/ATLR-Agent.exe`（单文件、免安装、`Data/` 与 exe 同级、U 盘可携带）。**另外每次发版都必须产出安装包** `ATLR-Agent-Setup-<版本>.exe`（`scripts/build-setup-windows.ps1`，ISCC 用项目内 `.tools/InnoSetup/`），Release 固定挂 4 件附件（exe + setup + 两个 sha256），口径见 `docs/releasing.md` 第 0 节；**不产 zip**，其它分发格式仅在明确要求时才产出。
   - **禁止**打包成纯命令行版（`cmd/octo` → `octo.exe`）、移动端或任何其他形态。CLI 构建只允许用于编译验证和冒烟测试，不是交付物。
   - **唯一构建入口**：`pwsh scripts/build-desktop-windows.ps1`（或 `make desktop`，已转发到同一脚本）。脚本会依次 `npm run build` → 确保 rg 嵌入 → `generate-syso` → `CGO_ENABLED=0 go build -tags embedrg -ldflags "-H windowsgui -s -w -X …Version=… -X …brand.*"` → 自检 PE `Subsystem==2` + 品牌标识（`internal/brand`）已嵌入 → 产出 `dist/windows-amd64/ATLR-Agent.exe`（含随附 `octo.exe`，仅编译验证用）与交付物 `dist/portable/ATLR-Agent.exe`（单文件，无 zip），任一失败即 exit 1。
   - **禁止手抄 go build**。历史事故：手抄命令漏 `-H windowsgui` 导致 GUI 启动弹黑控制台；漏 `-tags embedrg` 导致目标机 grep/glob 直接报错；把 `-H` 与 `-X` 拆成两个 `-ldflags` 只有最后一个生效（版本注入了、子系统没改）。
   - `packaging/windows/octo.iss:60-61` 同时安装 `octo-desktop.exe` 与 `octo.exe`，所以脚本也产出 `octo.exe`（同为 `-tags embedrg`）。**实际发版的安装包走 `packaging/windows/atlr.iss`**，由 `scripts/build-setup-windows.ps1` 生成，只装 `ATLR-Agent.exe` + LICENSE/NOTICE；`octo.iss` 里的 `taskkill /IM octo-desktop.exe` 不在交付路径上。CLI 仍只用于编译验证与冒烟。
2. **便携化布局（已改代码，不是配置）**：所有持久数据在 **EXE 所在目录**下：
   - 数据根：`<EXE 目录>\Data\`；命名 profile 为 `Data-<name>`。
   - 默认工作区：`<EXE 目录>\Workspace\`。
   - 核心锚点：`internal/datahome/datahome.go` 的 `Root()`（EXE 目录，可用 `OCTO_PORTABLE_ROOT` 覆盖——测试就用它钉临时目录）。
3. **本副本特有功能（上游没有，勿在升级时丢掉）**：
   - **每模型 RPM 限速**：`EndpointModel.RPM`（yaml `rpm`，config.yml），0=不限；填 N 实际 N-1 次/分（`effectivePerModelRPM`），按模型独立限速键（`SenderOptions.LimiterScope`）。UI 在 设置→模型 chip 编辑行。
   - **主题选择随 Data 迁移**：`Data\themes\selection.json`（`GET/PUT /api/theme-selection`，`internal/server/theme_selection.go`），server 为准、localStorage 仅离线兜底；无存档时默认回白色 azure。
   - **首启引导跳过**：模型配置步右下"跳过"→ `POST /api/onboard/skip` → `Data\.onboard_skipped` 标记（与 `.onboard_attempted` 独立）。
   - **背景图系统**：两张内置壁纸（`internal/server/backgrounds/` go:embed，id `bloom`/`mystery`）+ 用户上传（`Data/backgrounds/custom.jpeg`，POST /api/backgrounds/custom，魔数校验 JPEG/PNG/WebP ≤15MB）；选择存 selection.json `background` 字段（off/bloom/mystery/custom）；渲染层 `#atlr-bg`（低透明度水印覆盖，theme.ts 管理）。
   - **开机大肥鱼启动动画**：vendored 插件 `dsh-startup-screen/`（DeepSeek Harness 插件，MIT，仅作源参考），运行时已接入 `web/public/dsh-startup/`（splash.js/css + asset/*.mp3，注意音频目录名是单数 `asset/` 与 splash.js 的 fetch 路径一致）；index.html 内联首屏黑幕+9 秒兜底，main.ts 决策（localStorage `octo.startupAnim` 同步快路径 + selection.json `startupAnim` 服务端权威，nil=默认开）；设置→开关「开机大肥鱼启动动画喵~」。
   - **品牌**：`internal/brand` 是唯一真源（`Name`/`AppID`/`SingleInstanceID`/`DesktopPort`/`UpstreamUpdates`/`ExeName`，全部 `var`，由脚本 `-ldflags -X` 注入并在构建后断言）。UI 名 ATLR-Agent（i18n.ts / lang.go / main.go / bridge.go / index.html / Sidebar）；唯一保留 "Octo" 字样处 = 设置→关于 的 version_desc（caoxiang 重编译版本，基于 Octo）。图标：`build/windows/icon.ico`（主人提供的图，Pillow 圆角生成）、web 樱花 SVG（`OctoLogo.svelte` + `favicon.svg`）。
   - **与旧版 Octo 双开隔离**：SingleInstance ID / AppID / manifest / macOS bundle id = `dev.atlr.agent.desktop`（**改它会破坏与旧版共存**，旧版 `octo-desktop.exe` 用的是 `dev.octo-agent.desktop`）；桌面端默认端口 `127.0.0.1:9088`（CLI `octo serve` 的 8088 不动）；localStorage 前缀 `atlr.`；**数据目录名保持 `Data`**（靠 exe 所在文件夹天然隔离）；`UpstreamUpdates=false`，品牌构建不查、也不接受上游更新。
   - **交付命令**：
     ```bash
     pwsh scripts/build-desktop-windows.ps1        # 唯一打包入口（make desktop 同）
     ls -l dist/portable/ATLR-Agent.exe            # 唯一产物（单 EXE，无 zip）
     ```
4. **验证口径**：改动后 `go build ./...`（两模块）+ 冒烟：空目录跑 EXE，确认 `Data\` 生成、hub HTTP 200、`/api/theme-selection` 可写。禁止只改不验。
5. **已知残留**：`internal/server` 的 channel 测试组等仍按 home 布局写断言（上游测试基建假设 `datahome` 跟随 HOME，与便携锚点冲突），不影响编译与运行时；已修复 config/tools/datahome/主题/onboard/endpoint 相关测试。Git 未装导致 worktree 测试跳过。
6. **Go 依赖代理**：构建必须设置 `GOPROXY=https://goproxy.cn,direct`。

## Project

`octo-agent` — a Go 1.22+ AI agent CLI distributed as a single binary. Module path: `github.com/open-octo/octo-agent`. Ships as CLI + embedded Web UI + IM bridges via `octo serve`. Per-feature design notes live under `dev-docs/`.

## Commands

```bash
make build                                                  # ./octo (or set VERSION=0.x.y for releases)
make test                                                   # go test -race ./...
make vet                                                    # go vet ./...
make fmt-check                                              # gofmt -l . must print nothing
make fmt                                                    # gofmt -w .
make tidy                                                   # go mod tidy

go test ./internal/agent/                                   # single package
go test ./internal/provider/anthropic/ -run TestSendStream  # single test
go test -race -v ./internal/tools/                          # verbose race
```

## Architecture

Five-layer stack with one-directional dependencies:

1. **CLI (`cmd/octo/`)** — entry point (`main.go`), flag parsing, REPL loop (`repl.go`), session resume/list flags, slash-command dispatch, output streaming. Reaches the LLM through `internal/app` rather than importing `provider` directly.

2. **App bootstrap (`internal/app/`)** — the single place that constructs provider clients and adapts them to `agent.Sender`. Every entry point (`cmd/octo`, `internal/server`, IM channels) reaches the LLM through it rather than importing `provider` directly. Also owns the permission gate, sub-agent spawner, and `WireTools` unification.

3. **Agent core (`internal/agent/`)** — the loop, plus everything stateful:
   - `agent.go` — `Agent`, `Turn`, `TurnStream`, `Run`, `RunStream`. History rollback on error.
   - `history.go` — message log; goroutine-safe.
   - `content.go` — `ContentBlock` union (text / tool_use / tool_result). `Message.Blocks` overrides `Message.Content` when set; nil falls back to plain string for backward-compatible session JSON.
   - `session.go` — JSON persistence under `~/.octo/sessions/`.
   - `tool.go` — `ToolDefinition`, `ToolExecutor` interfaces.
   - `Sender` interface stack: `Sender` → `StreamingSender` → `ToolSender` → `ToolStreamingSender`. Each builds on the previous; type-assertion in callers picks the highest available capability.

4. **Providers (`internal/provider/`)** — per-vendor wire-format adapters. `provider.go` defines the interfaces; each subdirectory implements one protocol:
   - `anthropic/` — Messages API. `x-api-key` + `anthropic-version` headers. `system` as top-level field. Content blocks `[{type:"text", text}]`. SSE aggregator dispatches on `message_start`/`content_block_delta`/`message_delta`. Tool calls land as `content_block_start` of type `tool_use` with subsequent `input_json_delta` deltas.
   - `openai/` — Chat Completions. `Authorization: Bearer`. `system` carried as `messages[0]`. SSE aggregator parses `chat.completion.chunk`; tolerates missing `[DONE]` sentinel (some third-party servers omit it). Tool calls arrive in `delta.tool_calls[]` with chunked JSON arguments.
   - `responses/` — OpenAI Responses API (`/v1/responses`), offered as the Custom vendor's `openai-responses` protocol and used by the OpenAI vendor whenever its base URL is `api.openai.com` itself (from GPT-5.4 on, Chat Completions rejects function tools unless `reasoning_effort` is `none`; a redirected base URL keeps Chat Completions). Always stateless (`store:false`): system prompt in `instructions`, flat function tools sent with `strict:false`, tool calls/outputs as standalone input items. Reasoning items come back as `thinking` blocks carrying the item `ID`, `encrypted_content` in `Signature`, and the producing `Model`; they are replayed only to that same model, and the anthropic adapter drops any thinking block with an `ID`. An assistant message's `phase` is kept on its text block and resent.

   Provider wire quirks are encapsulated here — the agent layer never branches on protocol.

5. **Tools (`internal/tools/`)** — concrete `ToolExecutor` implementations.
   - `terminal.go` — current canonical example. Tool name `terminal` rather than `bash` because the implementation shells out via the platform shell — `sh -c` on macOS/Linux, PowerShell (`pwsh`, else `powershell`) on Windows — not a hard `/bin/bash` dependency. The shell binary is selected in one place — `executil.PowerShell()` — shared by the terminal tool, hook scripts, and clipboard capture so a session never straddles two PowerShells; `shellCommand` in `sandbox.go` is the single place the command is wrapped. The model is told which shell it's on via the environment context (`cmd/octo/envcontext.go`).
   - `DefaultRegistry` dispatches by tool name. `DefaultTools()` returns the set sent to the LLM (tools are on by default; `--no-tools` disables them).

## Adding capability

- **New provider** — implement `provider.Provider` (required) and optionally `provider.StreamingProvider`, `provider.ToolProvider`, `provider.ToolStreamingProvider`. Put it under `internal/provider/<name>/`. Each protocol's wire-format quirks are isolated inside the package; the agent layer must not learn about them.
- **New tool** — implement `agent.ToolExecutor` and `Definition() agent.ToolDefinition` returning the JSON Schema the LLM sees. Place it under `internal/tools/<name>.go`. Register it in `tools.DefaultRegistry` and add it to `tools.DefaultTools()` if it belongs in the default set.
- **New skill** — `~/.octo/skills/<name>/SKILL.md` with the same frontmatter format Claude Code uses. The skill loader composes existing tools — adding a skill should not require new tool code.

## Conventions

From `.octorules`:

- **One-directional deps.** `provider → agent` is enforced; `agent` must not import `provider`. Tests verify this implicitly by living in the same package as the code they test.
- **Test placement.** `*_test.go` siblings of source files. No external test frameworks beyond the stdlib + `httptest`.
- **No live network in `go test`.** All HTTP tests use `httptest.NewServer`. Integration tests against real APIs are run by hand with a real key, not in CI.
- **Comments in English.** Prefer self-documenting names; only comment the **why**, not the **what**.
- **gofmt is the formatter.** `gofmt -l .` must be empty before push.
- **Branch off latest main.** Never commit directly on `main`. Squash-and-merge is the default.
- **No new third-party dependencies** without justification in the PR description.
- **One concept per PR.** Mass mechanical changes (rename, move) can ride together but should be a single self-contained change set.
- **Commit messages and PR descriptions in English.**

## Common pitfalls (from prior incidents)

- **Sending `Accept-Encoding: gzip` to Bing's HTML search endpoint** returns a ~39 KB JavaScript skeleton instead of the ~120 KB real results page. The `web_search` tool must omit this header.
- **OpenAI streaming + `stream_options.include_usage`.** We send `include_usage=true` on streaming requests — DashScope (Bailian) and real OpenAI emit no usage chunk at all without it, so the streamed turn would report zero tokens. DeepSeek sends usage regardless. A server that omits the usage chunk anyway just leaves the counts at zero; one that hard-rejects the field would fail the stream (none of the tested backends — DeepSeek, DashScope, Kimi — do).
- **Cache token semantics differ by protocol, not vendor.** Anthropic-protocol endpoints (real Anthropic, Kimi `…/coding`, DeepSeek `…/anthropic`) report `input_tokens` as the *uncached remainder* with `cache_read_input_tokens` separate. OpenAI-protocol endpoints (DeepSeek default, DashScope) report `prompt_tokens` as the *whole* input with `cached_tokens` a subset. The agent treats `InputTokens` and `CacheReadTokens` as non-overlapping buckets (context occupancy is their sum), so the openai adapter subtracts cached from prompt (`apiUsage.nonCachedInput`); the anthropic adapter needs no adjustment.
- **OpenAI tool calls in streaming.** Function arguments arrive as JSON **fragments** across multiple chunks. The aggregator must concatenate by `tool_calls[i].index` before parsing.
- **`finish_reason: "tool_calls"` (OpenAI) vs `stop_reason: "tool_use"` (Anthropic).** The OpenAI adapter normalises `tool_calls` → `tool_use` on the agent-facing surface; the agent loop only ever sees `"tool_use"`.

## When in doubt

- Verify external claims (API endpoints, third-party SDK existence, dates) before committing them.
- If `go test ./...` fails because of an environment issue (missing key, blocked network), say so explicitly rather than commenting out the test.
