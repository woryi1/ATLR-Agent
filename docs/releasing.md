# 发布与提交注意事项

本仓库的双端发布（Gitee / GitHub）踩过的坑，记在这里，下次照做。其他历史错误见 [lessons.md](lessons.md)。

## 1. 版本与产物

- 版本号在 `internal/version/version.go`（`var Version = "0.7"`）。
- 便携版：`scripts/build-desktop-windows.ps1` → `dist/portable/ATLR-Agent.exe`。
- 安装版：`scripts/build-setup-windows.ps1` → `dist/portable/ATLR-Agent-Setup-<版本>.exe`（同时生成 `.sha256`）。
- 两个产物都要挂到 Release，`.sha256` 一起上传。安装器需要 Inno Setup 6（`ISCC.exe`）。

## 2. 编码坑（乱码的真正原因）

1. **PowerShell 5.1 的 `Invoke-RestMethod` 发中文会按本地代码页编码**，导致服务端收到乱码。请求正文一律先写成 UTF-8 文件，再用：
   - Gitee：`curl --data-urlencode "body@file.md" ...`
   - GitHub：`curl --data-binary "@body.json"`（JSON 文件本身按 UTF-8 无 BOM 写入）
2. **Gitee 返回的 JSON 不带 charset**，`Invoke-RestMethod` 会按 ISO-8859-1 解码，看到 `å...` 不代表服务端存错了。用浏览器打开页面，或把响应落盘后按 UTF-8 读取来确认。
3. **Gitee 的 PATCH 接口把 `access_token` 放在 query 会返回 401**，必须放进表单体，或改用 `Authorization: token <token>` 头。
4. **正文避免 `\f`、`\t` 这类转义**：曾经 `fav/resource` 被吃成 `av/resource`，箭头 `→` 在非 UTF-8 通道里变成 `?`。跨平台正文用 `->` 更稳。
5. 改完文案必须**用浏览器打开两端发布页**确认中文正常、附件齐全，别只看 API 回显。

## 3. GitHub

- push 偶发 `Empty reply from server` / 连接超时，用 HTTP/1.1 重试：
  `git -c http.version=HTTP/1.1 -c http.postBuffer=524288000 push github main`
- 本机直连 `github.com` 不稳定；附件上传走 `uploads.github.com` 反而更稳。
- 下载/存在性验证可走 `api.github.com/repos/<owner>/<repo>/releases/assets/<id>`，带 `Accept: application/octet-stream` 与 `Range: bytes=0-1023`。
- 工作流（`desktop.yml` / `go.yml` / `release.yml`）只保留 PR 与手动触发；不要恢复 push 自动触发，否则历史测试漂移会反复发失败邮件。

## 4. Gitee

- 把仓库改公开：`PATCH /repos/{owner}/{repo}`，表单**必须带 `name`**，只传 `private` 会 400。
- 上传附件：`POST /repos/{owner}/{repo}/releases/{id}/attach_files`（multipart，字段名 `file`）。
- 发布页的「最后提交信息」是 Gitee 自动生成的，不归我们维护。

## 5. 提交与同步

- 提交信息：英文一句话标题 + 说明段。
- 推送顺序：先 `gitee`，再 `github`；推完核对两端 `main` 的 short sha 一致。
- `D:\Git\ATLR-Agent` 是本地克隆（origin 指向本工作区），改动后 `git pull --ff-only` 同步。

## 6. 交付前检查

- [ ] 两端 Release 页面浏览器确认：中文正常、附件（exe / setup / 两个 sha256）齐全
- [ ] 附件探测返回 200，大小与本地一致
- [ ] `git status` 干净，无残留进程与 `.tmp`

## 7. 用 API 自动化发布（v0.71 实操记录）

### 7.1 固定顺序

1. 改版本号：`internal/version/version.go` 的 `var Version`（v0.71 就是 `"0.71"`，不要自作主张写成 `0.7.1`）。
2. 构建便携版：`powershell -File scripts/build-desktop-windows.ps1 -Version 0.71 -Commit <short-sha>` → `dist/portable/ATLR-Agent.exe`。
3. 构建安装包（需要时）：`powershell -File scripts/build-setup-windows.ps1 -Version 0.71`。`ISCC.exe` 不用另外安装，脚本会先找项目内的 `.tools/InnoSetup/ISCC.exe`。
4. 生成便携版校验文件：`Get-FileHash dist\portable\ATLR-Agent.exe -Algorithm SHA256`，按 `ATLR-Agent-v<ver>.sha256` 命名，内容为 `<hex>  ATLR-Agent.exe`，ASCII 编码。
5. 打附注标签并推送：`git tag -a v0.71 -m "ATLR-Agent v0.71" <sha>`，先 `git push gitee v0.71`，再 `git push github v0.71`。
6. 建 Release：GitHub `POST /repos/{owner}/{repo}/releases`，Gitee `POST /repos/{owner}/{repo}/releases`（`Authorization: token <token>` 放头部）。
7. 传附件：GitHub `POST https://uploads.github.com/repos/{owner}/{repo}/releases/{id}/assets?name=<file>`；Gitee `POST /repos/{owner}/{repo}/releases/{id}/attach_files`，表单字段名 `file`。
8. 核对：两端 `git ls-remote ... refs/tags/v0.71`、Release API 的附件名与字节数、真浏览器看页面。

### 7.2 命令要点

```powershell
# 令牌统一从凭据存储取，取完只放进环境变量，不落盘、不打印
$g = ("protocol=https`nhost=github.com`n`n" | git credential fill)
$env:GHTOKEN = ((@($g) -split "`r?`n" | Where-Object { $_ -match '^password=' }) -replace '^password=','').Trim()

# GitHub：JSON 正文写 UTF-8 无 BOM 文件，再用 --data-binary 发
[System.IO.File]::WriteAllText($json, $payload | ConvertTo-Json -Depth 4, (New-Object System.Text.UTF8Encoding($false)))
curl.exe -s -X POST -H "Authorization: token $env:GHTOKEN" -H "Content-Type: application/json" `
    --data-binary "@$json" "https://api.github.com/repos/woryi1/ATLR-Agent/releases"

# Gitee：正文走表单，中文用 --data-urlencode 从文件读
curl.exe -s -X POST -H "Authorization: token $env:GITEETOK" `
    --data-urlencode "tag_name=v0.71" --data-urlencode "name=ATLR-Agent v0.71" `
    --data-urlencode "body@.audit/release-gitee.md" --data-urlencode "target_commitish=main" `
    "https://gitee.com/api/v5/repos/woryi2/ATLR-Agent/releases"
```

### 7.3 踩过的坑

- **`Get-Content` 的返回值带注记属性**：`$body = Get-Content x.md -Raw` 之后 `ConvertTo-Json` 会把 `PSPath`/`PSDrive` 一起序列化，服务端报 `Invalid request. For 'properties/body' ...`。改用 `[System.IO.File]::ReadAllText($path, [System.Text.Encoding]::UTF8)`。
- **子进程里喂 `git credential fill` 会失败**：`powershell -File` 里用管道传 `protocol=https...` 会报 `refusing to work with credential missing protocol field`。在父 shell 取好令牌，通过环境变量传给子进程。
- **`cmd /c` 在本环境被禁用**：不要用它做重定向，直接用 PowerShell 或 curl。
- **GitHub 标签推送可能连续失败**（`Connection was reset` / `Could not connect ... :443`）。两次失败就换成 Release API 的 `target_commitish`，由 GitHub 自动建轻量标签；两端仍指向同一 commit，只是 GitHub 上是轻量标签、Gitee 上是附注标签。
- **`.tmp/setup-stage` 是安装包构建残留**：`.gitignore` 只忽略 `/tmp/`，不包含 `.tmp/`，打包完记得删，否则 `git status` 会多一条未跟踪目录。
- **版本号与标签保持一致**：`Version = "0.71"` 对应 tag `v0.71`，产物名 `ATLR-Agent-Setup-0.71.exe`，三者不要混用 `0.7.1`。

### 7.4 v0.71 的实测数据

| 产物 | 字节数 | sha256 前 8 位 |
|---|---|---|
| ATLR-Agent.exe | 63,932,928 | `1a941764` |
| ATLR-Agent-Setup-0.71.exe | 29,640,297 | `8586b2aa` |

GitHub 会额外挂 Source code (zip/tar.gz)，Gitee 同理；这两个不算上传附件，核对时按「4 个上传文件 + 2 个源码包」看。
