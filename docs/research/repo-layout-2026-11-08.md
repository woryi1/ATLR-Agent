# octo-agent 位置与配置研究报告

> 生成：2026-11-08 · 仓库：D:\caoxiang\trea\octo-agent（克隆自 github.com/open-octo/octo-agent，main 分支，ghfast.top 加速下载）
> 本报告由 3 个 Explore 子智能体研究汇总，全部结论带 文件:行号 证据。

## 0. 同一性判定：octo-agent 就是桌面 octo-desktop.exe 的源项目 ✅

| 证据 | 结果 |
|---|---|
| 桌面 `octo-desktop.exe`（59,454,464 字节）二进制字符串扫描 | 命中 `octo-agent`、`open-octo`、`octorules`、`octo-desktop` |
| 源码 `cmd\octo-desktop\` | 存在，Wails v3 GUI 桌面程序，独立嵌套 Go module（`cmd\octo-desktop\go.mod`，依赖 wails v3.0.0-beta.30） |
| 源码单实例锁 ID | `dev.octo-agent.desktop`（`cmd\octo-desktop\main.go:247-256`） |
| 本机旁证 | `~/.octo/` 目录在本机真实存在（AGENTS.md 即由 `~/.octo` 迁移而来），与该项目数据根约定一致 |
| 构建产物名 | `.goreleaser.yaml`：CLI 产物 `octo.exe`；Makefile `make desktop` 产物 `octo-desktop`（即桌面 EXE） |

## 1. 项目是什么

开源（MIT）、单二进制、自托管 Go AI 编码助手/个人助理：约 40MB 纯 Go 单文件，对标 Claude Code（shell、文件读写、搜索、MCP、技能、子 Agent 全内置），支持任意 OpenAI/Anthropic 协议兼容模型，8 种入口（TUI/CLI/Web/桌面/IM/编辑器/移动端），数据全部留本机、零遥测。

## 2. 整体结构

| 目录 | 职责 |
|---|---|
| `cmd/octo` | 主 CLI/TUI/服务程序（serve 内嵌 Web UI，端口 8088） |
| `cmd/octo-desktop` | Wails v3 桌面壳（托盘、profile 切换、更新器），= 桌面 octo-desktop.exe |
| `cmd/octo-eval` | 评测工具 |
| `internal/` | 36 个模块：agent、app、provider、tools、prompt、permission、sandbox、server、mcp、skills、memory、config、sqlitedb、datahome 等 |
| `web/` | Web UI（Vite+Svelte+TS），构建产物 go:embed 进 `internal/server/webdist/` |
| `landing/` | 官网静态站（含 install.sh / install.ps1） |
| `mobile/` | iOS/Android Capacitor 壳（直连用户自己的 octo serve，无中继） |
| `packaging/` | systemd/launchd/Inno Setup(windows)/macOS .pkg |
| `infra/dl-worker/` | Cloudflare Worker 下载代理（GitHub 被墙地区回退） |
| `docs/` `dev-docs/` `evals/` | 文档/设计笔记/评测任务 |

## 3. 位置总表（运行时所有读写位置）

### 3.1 数据根（一切的锚点）
- 默认 `~/.octo`，命名 profile 为 `~/.octo-<name>`（`--profile` / `OCTO_PROFILE` 控制）
  - `internal\datahome\datahome.go:16,73-79,95-111`
- 机器级共享路径无视 profile 永远指 `~/.octo`：`datahome.go:127-136`
- 主配置文件 `~/.octo/config.yml`（兼容旧 config.yaml）：`internal\config\config.go:1203-1211`
- GUI 环境变量注入文件 `~/.octo/serve.env`：`internal\serveenv\serveenv.go:31-37`

### 3.2 数据/数据库
- SQLite：`~/.octo/databases/<name>.db`（WAL）— `internal\sqlitedb\sqlitedb.go:67-79`
- 会话转录：`~/.octo/sessions/<id>.jsonl` + `<id>.chunks/` — `internal\agent\session.go:550-588`
- 项目分组注册表 `~/.octo/session-groups.json` — `internal\server\session_groups.go:135-145`
- 定时任务 `~/.octo/tasks/` — `internal\server\tasks_handlers.go:69`
- 工作流 `~/.octo/workflows/`、内置物化 `workflows-default/`、执行日志 `workflow-journals/` — `internal\tools\workflow_registry.go:64` 等
- 项目记忆 `~/.octo/memories/<slug>/MEMORY.md` — `internal\memory\memory.go:56-77`
- MCP 配置 `~/.octo/mcp.json`、令牌 `mcp-tokens/` — `internal\mcp\config.go:129`
- IM 渠道 `channels.yml`、`im-bindings.json`、`weixin-credentials.json` — `internal\channel\config.go:69` 等
- hooks：`hooks.yml`、`hooks-trust.json`、`hooks-pending/` — `internal\hooks\config.go:44` 等
- 权限 `~/.octo/permissions.yml`、审计 `audit.log`、回收站 `trash/<project_hash>/` — `cmd\octo\permission_gate.go:41`、`internal\audit\audit.go:65`、`internal\trash\trash.go:104-116`

### 3.3 工作目录（主人最关心的"新建/工作目录"逻辑）
- **默认工作区根 = `~/Octo`**（config `workspace_dir` 为空或 "auto" 时）— `internal\tools\workspacedir.go:11-31`
- 新建项目 → `<工作区根>/<净化项目名>`，重名加 `-2`、`-3` 后缀 — `internal\server\project_workspace.go:18-46`
- 无项目会话首次需要目录 → `<工作区根>/tasks/<sessionID>` — `internal\server\handlers.go:301-330`、`task_workspace.go:22-52`
- 会话目录优先级：自己选的 > 项目工作区 > 默认 `~/Octo` > 进程启动目录 — `internal\server\server.go:1955-2001`
- 桌面版从盘根启动时 chdir 到 home — `cmd\octo-desktop\main.go:102-132`
- 热更新工作区：`PUT /api/config/workspace_dir` — `internal\server\onboard_config_handlers.go:564-602`

### 3.4 日志/缓存/临时/上传
- `serve.pid` / `serve.log` / `cli.log` / `crash.log` 全在 `~/.octo/` — `internal\serveproc\serveproc.go:33-74`
- 端口 pin `~/.octo/serve.addr`（默认 8088，命名 profile 从 8089 起）— `internal\serveproc\addr.go:31-41`
- 日志轮转 10MiB×3 — `internal\logfile\rotating.go:29-32`；crash.log 1MiB×2 — `internal\crashlog\crashlog.go:32-35`
- MCP 子进程日志 `~/.octo/logs/mcp.log` — `cmd\octo\chat.go:311-328`
- 上传 `~/.octo/uploads/`（0700）— `internal\server\attachments.go:102-112`
- IM 附件暂存 `%TEMP%/octo-<adapter>/` — `internal\uploads\uploads.go:17-27`
- 终端溢出 `~/.octo/tmp/term-*.log` — `internal\tools\spill.go:151-178`
- 浏览器录制 `~/.octo/browser-recordings/`（`OCTO_BROWSER_RECORDINGS_DIR` 可覆盖）— `internal\tools\browser.go:159-187`
- 各类临时目录：`%TEMP%/octo-upgrade-*`、`octo-chrome-*`、`octo-clip-*.png` 等
- 主题 `~/.octo/themes`、Light App `light-apps/`、落地页 `landing/`、输入历史 `input_history`/`history`

### 3.5 技能/提示词/规则的磁盘位置
- 用户技能 `~/.octo/skills/<name>/SKILL.md`（Claude Code 兼容格式，优先级最高）— `internal\skills\skills.go:10-14,68-86`
- 内置 21 个默认技能物化到 `~/.octo/skills-default/`（版本戳 `.octo-version` 整体重写）— `internal\skills\defaults.go:41-61`
- 26 个专家技能 → `~/.octo/skills-expert/`
- 优先级：user > expert > default — `internal\skills\skills.go:248`
- 项目规则 `<cwd>/.octorules`（octo 版 CLAUDE.md）— `internal\prompt\prompt.go:55,257-262`
- 全局规则 `~/.octo/octorules.md`、身份 `~/.octo/soul.md`、`~/.octo/user.md` — `internal\prompt\prompt.go:62-72,210-224`
- 专家 agent 配置：内置物化 `~/.octo/agents-default/`，用户自定义 `~/.octo/agents/` — `internal\agentprofile\defaults.go:26-40`、`cmd\octo\chat.go:1624-1631`
- 旧 web-access 站点笔记抢救到 `~/.octo/site-patterns/` — `internal\skills\defaults.go:145-176`

### 3.6 提示词体系
- 内置基础提示词 `internal\prompt\base.md`（go:embed，`prompt.go:23-24`）
- 目标模板 `internal\prompt\goals\{continuation,budget_limit,objective_updated}.md`（go:text/template）
- 组装 10 层（`prompt.go:74-121`）：base → coauthor → soul → env → skills → MCP 工具 → memory → user.md → octorules.md → .octorules → --system
- 工作目录不是模板占位符，而是运行时 env 层注入 `- Working directory: %s` — `internal\tools\envcontext.go:28-77`
- 内置子智能体 3 种：`explore`（只读探索）/ `general`（全工具）/ `code-review`（审查）— `internal\agentprofile\builtins.go:9-53`
- 内置专家 agent 8 个（中文人格）：copywriter、learning-coach、legal-helper、life-assistant、productivity-coach、resume-coach、trip-planner、writing-partner — `internal\agentprofile\defaults\`
- `@include` 递归内联（深度≤5）— `prompt.go:295-350`

### 3.7 影响路径的环境变量（关键项）
- `OCTO_PROFILE`（决定整个数据根，最重要）
- `OCTO_BROWSER_RECORDINGS_DIR`、`OCTO_TRASH_DIR`、`OCTO_HISTORY_FILE`、`OCTO_INPUT_HISTORY_FILE`、`TMPDIR`、`APPDIR`（AppImage）、`OCTO_SANDBOX_POLICY`
- 供应商 key：`OPENAI_API_KEY`、`ANTHROPIC_API_KEY`、`DEEPSEEK_API_KEY`、`ZHIPU_API_KEY` 等 12 个（`internal\app\provider.go:79-319`）
- 搜索 key：`BRAVE_SEARCH_API_KEY`、`TAVILY_API_KEY`、`SERPER_API_KEY`
- 行为：`OCTO_LOG_LEVEL`、`OCTO_SERVE_ADDR`、`OCTO_ACCESS_KEY`、`OCTO_HOOK_PRE_TURN/POST_TURN` 等

### 3.8 桌面版（octo-desktop.exe）特有
- 固定连接 hub `127.0.0.1:8088` — `main.go:63`
- 桌面设置 `~/.octo[-profile]/desktop.json`（窗口/宠物/后台运行）— `cmd\octo-desktop\settings.go:46-85`
- 机器级 profile 选择文件 `~/.octo/desktop-profile` — `cmd\octo-desktop\profile.go:19,47-71`
- 单实例锁 `dev.octo-agent.desktop` — `main.go:247-256`
- 捆绑 uv 播种到 `~/.octo/bin/uv(.exe)`；macOS/Linux 播种 CLI 到 `~/.local/bin/octo` — `cmd\octo-desktop\uv.go:20-49`、`octo_cli.go:33-101`
- 更新器走 GitHub release + SHA-256，暂存目录由 Wails 基于 `os.MkdirTemp` 管理 — `cmd\octo-desktop\update.go:147-206`
- 无控制台启动时 stderr → `~/.octo/crash.log` — `main.go:429-444`

## 4. 旁注
- 本机 `~/.octo/` 已真实存在（主人的 AGENTS.md 即由 `~/.octo/AGENTS.md` 等迁移而来），说明桌面 EXE 此前已在本机初始化过数据根——与源码约定完全吻合。
- `infra/dl-worker/` 的存在说明项目自带 GitHub 下载代理，供被墙网络升级用。
