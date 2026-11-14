# 本项目踩过的坑（错误总结）

按「出过什么错 → 为什么 → 现在怎么防」记录，避免同一个坑踩第二次。发布相关的操作细节见 [releasing.md](releasing.md)。

## 1. 编辑代码前没读全文件

- **错**：改重启逻辑时直接重写 `cmd/octo-desktop/relaunch_windows.go`，把平台实现覆盖掉，还自造了一个 `relaunch_other.go`，导致 `detachRelaunch` 未定义、编译失败。
- **因**：只看了调用点，没确认哪些文件是平台相关的既有实现。
- **防**：动手前先读目标文件与同目录的平台变体（`_windows.go` / `_posix.go`）；改动后用 `git diff --stat` 核对影响面。误删的部分用 `git checkout -- <file>` 恢复，恢复后重跑 `go build`。

## 2. 备份导入：空分区导致 rename 失败

- **错**：导入时先删旧目录再改暂存目录，空「工作目录」没有暂存目录，报 `rename Work.restore-tmp Work: The system cannot find the file specified`，且旧数据已被删。
- **因**：暂存目录只在有文件时才创建；删除与替换顺序不可回滚。
- **防**：`extractRegion` 先 `MkdirAll`；Apply 改成「先写暂存 → 旧目录改名 `.restore-old` → 交换 → 成功后删除 / 失败回滚」。补了「空 Work 恢复」单测。

## 3. 导入后不自动重启

- **错**：前端恢复完成后界面数据仍是旧的。
- **因**：server 跑在桌面进程内，没有重启入口。
- **防**：新增 `POST /api/app/restart` + `SetRestartHook`，复用既有 `relaunchSelf()`；前端 3 秒倒计时后调用。

## 4. B 站曲库显示 0 首

- **错**：`fav/resource/list` 被风控返回 412，界面就显示 0 首。
- **因**：只依赖单一接口，没有兜底。
- **防**：回退到 `fav/resource/ids` + 逐条 `x/web-interface/view`（并发 4、200ms 节奏重建，实测 1232 首）。

## 5. CI 失败：测试仍按旧目录断言

- **错**：数据目录改成 `Data/User|Records|Tools|Work|Cache` 后，桌面模块测试还断言 `~/.octo`，Desktop 工作流五个平台全红。
- **因**：改实现时没同步测试；测试依赖 `HOME` 而不是可注入的根目录。
- **防**：测试统一用 `OCTO_PORTABLE_ROOT` 隔离，断言新布局；本地先跑 `go test ./...` + `gofmt -l` 再提交。

## 6. PowerShell 的编码坑（发布说明乱码）

- **错**：Release 正文出现 `å...` 乱码；`fav/resource` 被吃成 `av/resource`；`→` 变成 `?`。
- **因**：PS 5.1 的 `Invoke-RestMethod` 按本地代码页编码请求体；JSON 通道把 `\f` 当转义；非 UTF-8 通道表达不了箭头。
- **防**：正文先写 UTF-8 文件，用 `curl --data-urlencode "body@file"`（Gitee）或 `--data-binary "@file"`（GitHub）；跨平台文案用 `->`；改完必须浏览器验收。

## 7. Gitee API 的两个坑

- **错**：`PATCH /repos/{owner}/{repo}` 只传 `private` 返回 400；`PATCH /releases/{id}` 把 token 放 query 返回 401。
- **因**：仓库接口要求带 `name`；发布接口只认表单体里的 `access_token`（或 `Authorization` 头）。
- **防**：仓库改动带 `name`；token 放表单体或头部，别放 query。

## 8. GitHub push 反复失败

- **错**：`Empty reply from server` / `Connection was reset`。
- **因**：本机到 `github.com:443` 不稳定。
- **防**：`git -c http.version=HTTP/1.1 -c http.postBuffer=524288000 push github main`；失败就重试一次，仍失败则说明网络并给出兜底，不要闷头反复重试。

## 9. README 直接照搬上游

- **错**：首页放了 Octo 的介绍视频、官网/安装脚本、徽章、微信群二维码和贡献者墙，看起来像把上游原封不动搬过来。
- **因**：改品牌时只改了标题，没有逐段审内容归属。
- **防**：README 只写本项目是什么、怎么下载、怎么构建、协议与联系；上游内容只提一句「基于 Octo」并给链接。

## 10. 清理过头 / 清理不足

- **错**：第一次清理把 `dist/octo-setup*.exe` 和中间目录一起删了，后来又要重新装 Inno Setup 打安装包；另一次又漏了 `.trash/`（73MB webdist 旧副本）和审计截图。
- **因**：清理前没按「产物 / 工具链 / 运行时垃圾」分类。
- **防**：只删运行时垃圾（`.trash/`、`*.log`、`.audit/evidence|shots`、过期中间目录）；工具链（`.tools/`）与最终产物（`dist/portable/`）保留。

## 11. 残留 Pages 工作流

- **错**：`.github/workflows/pages.yml` 在 push 到 `docs/**` 时部署 GitHub Pages，仓库没开 Pages，任务失败并发失败邮件。
- **因**：fork 只做 Windows，上游的站点部署没有一起裁掉。
- **防**：已删除该工作流；发布/CI 只保留与 Windows 产物相关的任务。
