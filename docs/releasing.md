# 发布与提交注意事项

本仓库的双端发布（Gitee / GitHub）踩过的坑，记在这里，下次照做。

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
