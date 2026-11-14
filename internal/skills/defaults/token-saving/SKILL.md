---
name: token-saving
description: 省 Token 省事工作法 —— 任何项目开工前、每次回答前、每次继续任务时都必须先调用/先对照本技能。规定标准动作：先列项目根目录并读完 txt/md 摸底；禁止全盘搜索（禁止 find 全盘、禁止全盘 dir /s）；依赖优先复用系统已有，缺的从别处 site-packages 直接复制到项目 vendor（禁止 pip 重下/重装、禁止猜语法）；Python 用系统自带 3.13；BAT 保持纯 ASCII 且 pushd "%~dp0"；改完必须跑最小样本自验闭环；用最小改动交付，不做超出需求的重构。触发词：省token、省事、省点事、项目摸底、找不到目录、路径在哪、装依赖、pip install、Python 环境、bat 跑不起来、闪退、安装包、节省 token。
agent_created: true
---

# 省 Token 省事工作法

> **调用时机（强制）**：项目开始时、每次回答前、每次继续/恢复任务时，先过一遍本技能再动手。
> 目标：**用最少的读取、最少的下载、最少的来回，一次把事做对。**
> 违反下面任何一条 = 本次任务重做。

本技能由三份来源合并而成：
- `core/SKILL.md` — WorkBuddy `token-saving-skill` 的完整正文（省事/省时/依赖/Python/BAT/自验工作法）
- `ponytail-suite/` — Trae `ponytail节省token` 的 ponytail 全套（ponytail / -audit / -debt / -gain / -help / -review）
- 本文件 — 合并后的入口与强制调用规则

---

## 0. 动手前的固定顺序（不许跳步）

1. **列项目根目录**（用 `glob` / `ls`，不要用搜索）
2. **读完根目录所有 `.txt` 和 `.md`**：`常用路径.txt`、`常用目录.txt`、`AGENTS.md`
   —— 未知工具路径先在这里找，**找到了还去问用户 = 违规**
3. **列出用户提到的那一层目录**，确认哪些文件真实存在
4. **确认目标目录/文件**后再改代码，最后跑最小样本自验

> 铁律：项目根目录的 txt/md 就是权威路径表。**先读文件，再提问。**

---

## 1. 禁止全盘搜索（硬规则）

- 用户没特殊说明时，**文件一定在项目内**。默认假设：全盘搜索 = 违规。
- 禁止：`find /c /d /e -name ...`、全盘 `dir /s`、遍历整个磁盘。
- 允许：`glob` 带具体前缀、`ls` 指定目录、`grep` 指定目录/文件。
- 用户说"某某目录/某某文件"而该名字不存在时：
  **先在项目内找同名或名字相近的目录与文件**（根目录 + 刚改过代码的那一层），列出候选交给用户选，**而不是满盘找**。
- 反例（真实踩坑）：用户说的"Put Out"其实就是 `E:\ai2\vocal_separation\output`。
  正确做法是列出 `E:\ai2\vocal_separation\` 的内容，一眼就能对上，根本不用去别处找。

---

## 2. 目录/路径定位法

```
① ls 项目根目录                      → 有没有同名/近名条目
② ls 最近改过代码的那一层目录          → 最可能的答案在这
③ 读 常用路径.txt / 常用目录.txt       → 工具类路径在这里
④ 以上都没有 → 选项式提问（给出 2-4 个候选）
```
- 列目录优先级：`glob` > `ls`；**不要一上来 grep 全盘**。
- 判断"某工具在哪"时，顺序永远是：环境变量 → 项目内相对目录 → 常用路径.txt 里记录的安装位置 → PATH。

---

## 3. 依赖策略（核心：能不重下就不重下）

决策顺序，**自上而下逐个试，命中即停**：

| 顺序 | 情况 | 做法 |
|---|---|---|
| 1 | 目标 Python 自带 | 直接用，什么都不装 |
| 2 | 系统里另一个 Python / 别处 site-packages 已有 | **直接复制到项目 `vendor\`**，并把 vendor 注入 `sys.path` + `PYTHONPATH` |
| 3 | 版本号能在 常用路径.txt / 项目内找到 | **硬编码该绝对路径**（不要用散装的"自动探测"当唯一方案） |
| 4 | 真的都没有 | 先问用户，说明体积/来源/装到哪，**国内源优先**，禁止 GitHub |

- **禁止**为了一个包去 `pip install`（能复制就别重下，用户明确反感）。
- 复制的是**纯 Python 包 + `.pyd`/`.dll`**：同 CPython 小版本（3.13.x）之间 ABI 兼容，可直接拷。
- **不要复制 torch / numpy 这类已经在目标 Python 里存在的重包**，版本不一致会把环境搞坏。
- 只复制"目标解释器缺的"最小集合，复制完 `find vendor -name __pycache__ -exec rm -rf {} +`。

### 复制依赖的标准动作
```bash
# SRC = 已装好该包的另一个 Python 的 site-packages
# DST = 项目内 vendor
mkdir -p "$DST"
for item in demucs demucs-4.1.0.dist-info einops einops-0.8.2.dist-info ...; do
  cp -r "$SRC/$item" "$DST/"
done
cp "$SRC/lameenc.cp313-win_amd64.pyd" "$DST/"      # 单文件扩展模块单独拷
find "$DST" -name "__pycache__" -type d -prune -exec rm -rf {} +
```

### 脚本侧注入（必须放在 import 第三方包之前）
```python
import sys, os
SCRIPT_DIR = os.path.dirname(os.path.abspath(__file__))
VENDOR_DIR = os.path.join(SCRIPT_DIR, "vendor")
if os.path.isdir(VENDOR_DIR):
    if VENDOR_DIR not in sys.path:
        sys.path.insert(0, VENDOR_DIR)
    _old = os.environ.get("PYTHONPATH", "")
    if VENDOR_DIR not in _old.split(os.pathsep):
        # 子进程（如 python -m demucs）也要能找到
        os.environ["PYTHONPATH"] = VENDOR_DIR + (os.pathsep + _old if _old else "")
```
> 关键点：`sys.path` 只影响当前进程，**子进程必须靠 `PYTHONPATH` 环境变量**。

---

## 4. Python 解释器选择（必须用系统自带的）

- **必须**：`C:\Users\Administrator\AppData\Local\Programs\Python\Python313\python.exe`（系统 Python 3.13，带 tkinter）
- **禁止**：任何应用内置 runtime（含 WorkBuddy 的 `...\.workbuddy\binaries\python\versions\3.13.12\python.exe`）
  —— 它**没有 tkinter**（无 `Lib\tkinter`、无 `_tkinter.pyd`、无 `tcl`），任何 GUI/文件选择框都会 `ModuleNotFoundError`。
- 判断依据写死在项目里：需要 GUI 的工具一定用系统 Python；系统 Python 缺的包 → 走第 3 节的"复制到 vendor"。
- 全项目只保留**一个** Python 入口约定，不要一会儿内置一会儿系统。

---

## 5. BAT 规范（AGENTS.md 要求，逐条照做）

- **纯 ASCII 编码**。BAT 里禁止出现中文（含中文的路径交给 Python 处理），写完用
  `LC_ALL=C grep -qP '[^\x00-\x7F]' run.bat` 校验，有输出就是违规。
- 开头固定：`chcp 65001 >nul 2>&1` → `setlocal` → `pushd "%~dp0"`
- 结尾 `popd` + `pause`；退出码要透传（`set "RC=%ERRORLEVEL%"` → `exit /b %RC%`）
- 参数用 `%*` 透传，别写死 `"%~1"`
- **不要**在 BAT 里因为找不到某个工具就 `exit /b 1` 一闪而过 —— 要么交给 Python 报明确错误，要么打印可读的提示再 `pause`
- 编码：`set PYTHONUTF8=1` + `set PYTHONIOENCODING=utf-8`（中文路径/中文输出必备）
- 所有构建/打包/下载类脚本**必须有日志输出**，运行期间每 30 秒看一次日志

---

## 6. 输出与交付约定

- 需要"每次跑一份结果"的工具：输出根目录下按时间戳建子目录，并顺手打同名 ZIP
  ```
  output\20260926_190210\        <- 本次全部产物 + report.json + run_时间戳.log
  output\20260926_190210.zip     <- 同一个子目录的压缩包
  ```
- 每次运行**必须**生成 `run_YYYYmmdd_HHMMSS.log`（用 Tee 同时写 stdout/stderr）。
- 启动时打印 preflight 横幅：python / ffmpeg / models / vendor / run dir / log 的**真实绝对路径**，缺任何一项就明确报错退出（退出码 2），不要静默闪退。

---

## 7. 自验闭环（不跑不算完成）

- 改完代码必须**跑最小样本**（剪 60–90 秒音频），确认：导入成功 → 工具被正确定位 → 全流程出产物 → 退出码 0 → 读日志。
- 测试产物放在独立临时目录（如 `_verify\`），**验证完删掉**，不要污染正式输出目录。
- 报错就修，修完重跑，直到真正可用再交付；不许"看起来应该能跑"就交付。

---

## 8. 提问规范

- 只在**真的二义**时问，且必须用选项式提问（2–4 个候选 + 用户可自填）。
- 提问前先做完第 0 节的摸底 —— **能从项目文件里读出来的答案，不许拿去问用户**。
- 每步都有明确选项，并留一个"我自己补充"的出口。

---

## 9. 已知路径速查（已实测，直接用，不要再找）

| 用途 | 路径 |
|---|---|
| 系统 Python 3.13（带 tkinter） | `C:\Users\Administrator\AppData\Local\Programs\Python\Python313\python.exe` |
| octo 用户技能根 | `C:\Users\Administrator\.octo\skills\` |
| 人声分离流水线 | `E:\ai2\vocal_separation\`（入口 `run.bat`，核心 `vocal_sep.py`，依赖 `vendor\`，模型 `models\htdemucs.th`，输出 `output\`） |
| 复现手册 | `E:\ai2\复现手册\复现手册.md` |

> 换项目时以该项目根目录的 `常用路径.txt` 为准，本表只作兜底。

---

## 10. 反例清单（真实踩过的坑，别再犯）

1. 把 `%FFMPEG%` 写死成项目内 `..\ffmpeg\bin\ffmpeg.exe` 而该目录不存在 → BAT 直接 `exit /b 1` 闪退。
   **正解**：从 `常用路径.txt` 读真实安装路径，采用多候选 + PATH 兜底。
2. 用应用内置 Python 导致 `ModuleNotFoundError: No module named 'tkinter'`，GUI 永不弹出。
   **正解**：用系统 Python 3.13。
3. 缺包就去 `pip install`。
   **正解**：先看别处有没有，有就复制到项目 `vendor\`。
4. 用户说"Put Out"，跑去别处找目录。
   **正解**：列项目目录，它就是 `output`。
5. 输出全堆在一个目录里，分不清哪次跑的是哪次。
   **正解**：`output\<时间戳>\` + `output\<时间戳>.zip`。
6. BAT 里写了中文路径，编码一乱就找不到文件。
   **正解**：BAT 纯 ASCII，中文路径交给 Python。
7. 把整个项目读进上下文再改三行。
   **正解**：只读要改的文件 + 它直接依赖的那几个。

---

## 11. ponytail 全套（子技能，按需读）

`ponytail-suite/` 下是 Trae 的 ponytail 技能包，每个子目录是一个独立技能，需要时读对应的 `SKILL.md`：

| 子技能 | 用途 |
|---|---|
| `ponytail/` | 主角技能 —— 懒人高级工程师风格：少写、能跑就行、拒绝过度设计 |
| `ponytail-audit/` | 审计当前改动 |
| `ponytail-debt/` | 技术债盘点 |
| `ponytail-gain/` | 收益盘点（省了多少） |
| `ponytail-help/` | 使用帮助 |
| `ponytail-review/` | 代码审查 |

核心口号：**He says nothing. He writes one line. It works.**
