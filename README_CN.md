# ATLR-Agent

基于 [Octo](https://github.com/open-octo/octo-agent)（MIT）重新编译的 Windows 桌面端 AI Agent。本仓库是作者自己的构建：沿用上游的 Agent 内核，打包方式、界面细节与数据目录布局由本项目维护。

English: **[README.md](README.md)**。

> **声明**：本项目在开发过程中大量使用 AI 辅助。内置的美术素材与音乐版权归原作者所有（音乐来自哔哩哔哩），详见 [NOTICE.md](NOTICE.md)。
>
> **平台支持**：只维护 Windows —— 单文件便携 EXE 与按用户安装的 SET 版。`packaging/` 与 `scripts/` 下的 macOS、Linux 打包脚本仅为参考保留，本仓库不构建、不测试；需要请自行适配。

## 这是什么

- Windows x64 单文件 EXE，无需 Node / Python / Ruby；首次启动会在 EXE 旁创建 `Data/`。
- Wails 原生桌面壳，Web UI 与 ripgrep 全部内嵌，干净机器上不需要任何额外依赖。
- 接入任意 OpenAI / Anthropic 兼容端点；服务与数据都留在本机。
- 上游能力齐全：TUI、CLI、Web UI、技能、MCP、子智能体、记忆、浏览器自动化、IM 频道；本构建另有 B 站音乐播放器与加密数据备份/恢复。

## 下载

从最新 Release 任选一个：

| 文件 | 用途 |
| --- | --- |
| `ATLR-Agent.exe` | 单文件便携版，双击即用，`Data/` 生成在同目录。 |
| `ATLR-Agent-Setup-<版本>.exe` | 按当前用户安装（无需管理员），带开始菜单入口与卸载程序；卸载不删除 `Data/`。 |

两个文件旁都附有 SHA-256 校验文件。安装包未做代码签名，首次运行可能触发 SmartScreen 提示。

## 从源码构建

需要 Go 1.25+、Node 22（构建 Web UI），仅打安装包时需要 Inno Setup 6。

```bash
make build                                  # CLI：./octo
scripts/build-desktop-windows.ps1           # 便携单文件 -> dist/portable/
scripts/build-setup-windows.ps1             # 安装版 SET -> dist/portable/
```

## 数据目录

`Data/` 分为 `User/`、`Records/`、`Tools/`、`Work/`、`Cache/` 五个区，所以「该备份什么」「什么能删」是目录名而不是一串文件名。设置 → 数据管理 可导出加密的 `.atlrbak` 包，之后拖回来即可恢复。

## 上游与协议

基于 [Octo](https://github.com/open-octo/octo-agent)，MIT 协议。本仓库代码同样为 MIT（见 [LICENSE.txt](LICENSE.txt)）；内置美术素材、启动动画素材与音乐/歌词**不在** MIT 范围内，版权归各自作者，归属说明见 [NOTICE.md](NOTICE.md)。

## 联系

B 站：[woryi](https://space.bilibili.com/3493283561095240)（推荐私信） · 邮箱：13243355121@163.com
