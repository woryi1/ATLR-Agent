# ATLR-Agent

A Windows desktop AI agent, rebuilt from [Octo](https://github.com/open-octo/octo-agent) (MIT). This repository is the author's own build: same agent core, different packaging, UI details and data layout.

简体中文见 **[README_CN.md](README_CN.md)**.

> **Notice:** developed with heavy AI assistance. Bundled artwork and music belong to their original owners (music is streamed from Bilibili) — see [NOTICE.md](NOTICE.md).
>
> **Platform support:** Windows only — the portable single EXE and the per-user installer. The macOS and Linux packaging under `packaging/` and `scripts/` is kept for reference; it is not built or tested here, so adapt it yourself if you need those builds.

## What it is

- One self-contained EXE for Windows x64 — no Node, Python or Ruby; on first launch it creates `Data/` next to itself.
- Native desktop shell (Wails) with the web UI bundled and ripgrep embedded, so a clean machine needs nothing else.
- Works with any OpenAI- or Anthropic-compatible endpoint; the server and your data stay on your machine.
- Everything the upstream agent ships: TUI, CLI, web UI, skills, MCP servers, sub-agents, memory, browser automation, IM channels — plus a Bilibili music player and encrypted data backup/restore in this build.

## Download

Grab either file from the latest release:

| File | What it is |
| --- | --- |
| `ATLR-Agent.exe` | Portable single EXE. Double-click and use; `Data/` is created beside it. |
| `ATLR-Agent-Setup-<version>.exe` | Per-user installer (no admin rights). Adds a Start-menu entry and an uninstaller; uninstalling keeps the `Data/` folder. |

SHA-256 files are published next to both. The installers are not code-signed, so Windows SmartScreen may warn on first run.

## Build from source

Requires Go 1.25+, Node 22 (for the web UI) and, for the installer only, Inno Setup 6.

```bash
make build                                  # CLI: ./octo
scripts/build-desktop-windows.ps1           # portable single EXE -> dist/portable/
scripts/build-setup-windows.ps1             # installer (SET)  -> dist/portable/
```

## Data layout

`Data/` is split into `User/`, `Records/`, `Tools/`, `Work/` and `Cache/`, so "what to back up" and "what is safe to delete" are folder names rather than file lists. Settings → Data management can export an encrypted `.atlrbak` package and restore it later.

## Upstream and license

Based on [Octo](https://github.com/open-octo/octo-agent), MIT licensed. Code in this repository is MIT (see [LICENSE.txt](LICENSE.txt)); bundled artwork, startup animation assets and music/lyrics are **not** MIT — they remain with their respective owners, with attribution in [NOTICE.md](NOTICE.md).

## Contact

Bilibili: [woryi](https://space.bilibili.com/3493283561095240) (DM preferred) · Email: 13243355121@163.com
