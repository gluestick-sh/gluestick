# <img src="assets/appicon.png" width="32" height="32" alt="Gluestick icon" /> Gluestick

**基于 Scoop 生态的轻量、高效 Windows 包管理器**

Gluestick 是 Windows 上的包管理器，从 4,500+ 个 Scoop 软件包安装、更新和管理 Claude Code、Codex CLI
及其他 Windows 软件。它直接使用 Scoop 仓库生态，同时拥有独立的工作目录和缓存，不影响已有的 Scoop 环境。

- **轻量** — `glue.exe` 与 `shim.exe` 合计约 3 MB，无需 VC++ 运行库。
- **快速** — 并行下载、内容寻址的文件复用和本地缓存。
- **兼容** — Scoop 仓库、软件清单及常见安装脚本。
- **安全** — 文件哈希校验、路径安全检查和原子更新。

## 目录

- [快速开始](#快速开始)
- [安装软件包](#安装软件包)
- [模板](#模板)
- [CLI 参考](#cli-参考)
- [配置](#配置)
- [数据布局](#数据布局)
- [从源码构建](#从源码构建)
- [安装 Gluestick](#安装-gluestick)
- [性能](#性能)
- [项目结构](#项目结构)
- [桌面应用与套餐](#桌面应用与套餐)
- [贡献](#贡献)
- [许可证](#许可证)

## 快速开始

打开 PowerShell 运行：

```powershell
irm https://gluestick.sh/install.ps1 | iex
```

脚本会自动识别 CPU 架构，在缺少 7-Zip 或 MinGit 时先行安装，把 `glue.exe` 和 `shim.exe` 下载到
`%USERPROFILE%\.glue`，并把该目录加入用户 `PATH`。无需管理员权限。更习惯图形安装包？可以从
[gluestick.sh/download](https://gluestick.sh/download.html) 下载对应 amd64 或 arm64 版本。

然后打开一个新的 shell：

```powershell
glue install claude-code     # 或 ripgrep、fd、vscode、yazi……
glue list
```

查看完整命令列表：

```powershell
glue --version
glue --help
```

## 安装软件包

对于独立软件，添加一个 Scoop 仓库并直接安装。`main` 仓库首次运行时已自动加好，所以 `glue bucket add main`
只在删掉它之后才需要执行：

```powershell
glue bucket add main
glue search ripgrep
glue install ripgrep
glue list
glue update --all
```

软件包引用可以是 `name` 或 `bucket/name`：

```powershell
glue install extras/vscode
glue install ripgrep fd jq        # 多个软件包并发安装
glue uninstall ripgrep
```

### 依赖

与 Scoop 一样，`glue install` 会先安装清单的 `depends`，这样链接到其他软件包目录的安装脚本能找到它。已经安装的
依赖不会被改动（绝不会作为副作用被升级），`--independent` 会完全跳过依赖展开。`suggest` 条目从不自动安装：
安装成功后会以可选配套软件及添加它们的命令作为结尾。

### 安装指定版本

`pkg@version` 可安装仓库曾经发布过的任意版本，也用于卸载和在**已安装**版本之间切换：要删除某个具体版本，传入
`pkg@version`；如果它正被使用，先用 `glue reset <pkg>@<version>` 切走。

### 环境检查

运行 `glue doctor` 检查环境 — 长路径支持、`git`、下载工具及其他依赖 — 并列出已安装但清单 `depends`
未安装的软件包。

## 模板

模板是为某个领域或工作流准备的一组具名软件包，例如搭建一个 AI 编程环境。安装前先查看模板，然后把它的软件包
作为一个整体来管理：

```powershell
glue template list
glue template show vibe-coding
glue template install vibe-coding
glue template update vibe-coding
glue template uninstall vibe-coding
```

内置模板覆盖常见场景。要添加自己的模板，往 `<workspace>\templates\` 放一个 JSON 文件：

```json
{
  "kind": "gluestick.template",
  "name": "Editor",
  "title": { "en": "Editor", "zh": "编辑器" },
  "summary": { "en": "Code editors", "zh": "代码编辑器" },
  "description": { "en": "Code editors and language servers.", "zh": "代码编辑器与语言服务器。" },
  "packages": [
    { "name": "vscode", "bucket": "extras", "label": { "en": "VS Code", "zh": "VS Code 编辑器" } },
    { "name": "nodejs-lts" }
  ]
}
```

## CLI 参考

| 命令 | 说明 |
| --- | --- |
| `glue install <pkg>...` | 安装一个或多个软件包。参数：`--arch`、`--version`、`--force`、`--interactive`、`--independent`、`--timings`。 |
| `glue uninstall <pkg>` | 删除软件包，或用 `pkg@version` 删除某个版本。 |
| `glue update [pkg]...` | 不带参数时列出可用更新；`--all` 升级全部。别名：`up`。 |
| `glue list` | 列出已安装软件包。参数：`-d/--detailed`、`--all`（所有版本）。 |
| `glue search <query>` | 搜索仓库清单。参数：`--bucket <name>`（可重复）、`--limit <n>`。 |
| `glue info <pkg>...` | 显示已安装软件包的信息。 |
| `glue depends <pkg>...` | 报告缺失的依赖和可选的建议。 |
| `glue hold <pkg>...` / `glue unhold <pkg>...` | 将软件包固定在当前版本，或解除固定。 |
| `glue reset <pkg>@<version>` | 切换到已安装的版本，并重建该版本的 shim、`env_set` 值和开始菜单快捷方式。必须显式指定版本。 |
| `glue doctor` | 运行环境与依赖检查，包括缺失的 `depends`。 |
| `glue bucket <sub>` | 管理仓库：`add`、`remove`、`list`、`update`、`check`、`deepen`、`known`。 |
| `glue template <sub>` | 管理软件包模板：`list`、`show`、`install`、`update`、`uninstall`。 |
| `glue cache <sub>` | 管理缓存索引与内容存储：`list`、`clear`、`rebuild`、`gc`、`purge-partials`、`verify [--repair]`。 |
| `glue config <sub>` | 管理配置：`get`、`set`、`unset`、`list`。 |
| `glue root <sub>` | 管理工作区：`show`、`set <dir>`、`reset`。 |
| `glue path <sub>` | 管理 `PATH` 集成：`show`、`check`、`setup`、`remove`。 |
| `glue completion <shell>` | 输出 `bash`、`zsh`、`fish` 或 `powershell` 的补全脚本。 |

表中简写参数的完整说明：

- `install --arch <64bit|arm64|32bit>`、`--version <v>`（任意已发布版本，或由 `autoupdate` 生成的版本）、
  `--force`（重装）、`--interactive`（见下）、`--independent`（不先安装 `depends`）、
  `--timings`。
- `root set <dir>` 在该处创建工作区并为此安装保存路径（`<install dir>\data-root.txt`，独立二进制还会写入
  `HKCU\Software\Gluestick`）。
- `completion` 脚本在每次按 `TAB` 时回调 `glue __complete`，因此能补全命令与嵌套命令名、参数（以及 `--arch` /
  `--bucket` 的值）、软件包名与 `bucket/name`、已安装软件包及其 `pkg@version`、仓库和模板 — 始终基于当前工作区。

每个 shell 会话加载一次补全脚本：

```powershell
# PowerShell — 加入 $PROFILE 可持久化
glue completion powershell | Out-String | Invoke-Expression
```

```bash
# bash — 加入 ~/.bashrc 可持久化
source <(glue completion bash)
```

zsh：把输出保存为 `$fpath` 中名为 `_glue` 的文件。fish：`glue completion fish | source`。

脚本调用的是你输入的那个 `glue`，所以本地构建无需在 `PATH` 中也能用
（`.\target\release\glue.exe completion powershell | Out-String | Invoke-Expression`）。应答的二进制必须带隐藏的
`__complete` 命令 — 如果 `PATH` 仍指向旧版本，直接输入 `glue` 不会得到候选，请升级它后重新加载脚本。

### 安装输出与耗时

安装会打印分阶段报告（以 `→` 开头阶段行，缩进的明细行），并在终端上显示实时进度条；当输出被重定向时，进度会
降级为周期性纯文本行，使日志保持可读。`glue install --timings` 会为每个软件包追加机器可读的汇总。

### 交互式安装程序

大多数软件包静默安装。少数清单只有 GUI 向导（没有静默开关的 `.exe` 安装程序）：`glue install <pkg> --interactive`
会以 `$interactive = $true` 运行它们的 `installer.script` 和安装前后钩子 — 即 Scoop 对
`if (!$interactive) { $args += '/S' }` 的契约。这只作用于你执行的命令：`glue update`、模板安装和定时仓库刷新始终
无人值守，因此不会在无人观看的窗口中弹出对话框。标记 `"innosetup": true` 的软件包忽略该参数 — 安装程序只会被解包，
从不执行。

## 配置

```powershell
glue config list
glue config get proxy
glue config set proxy http://127.0.0.1:8080
glue config set proxy_packages claude-code,codex
glue config unset proxy
```

这些设置保存在程序旁的 `config.json` 中 — 位于 `%LOCALAPPDATA%\Programs\Gluestick CLI` — 属于程序级配置而非工作区
数据，所以重装或切换工作区都会保留。全新安装第一次运行时读取旧的工作区 `config.json`，此后写入自己的文件。设置
`GLUE_CONFIG_DIR` 可把文件移到别处（开发和测试用）；没有安装目录布局的独立二进制则把它留在工作区。

| 键 | 用途 |
| --- | --- |
| `github_proxy` | 仅用于 GitHub 下载的镜像前缀（不是 HTTP 代理；无法访问 `storage.googleapis.com` 等非 GitHub 主机）。 |
| `proxy` | 代理模式：`auto`（默认）、`direct` 或一个 `http://` 代理 URL。 |
| `proxy_packages` | 软件包名（或 `bucket/name`）的 JSON 数组，其下载使用 `proxy`；其他下载保持直连。 |
| `self_update_check_url` | 可选端点（纯文本或 `{"version":"x.y.z"}`），指明最新发布的 Gluestick 版本。未设置（默认）时不发网络请求；结果缓存 24 小时，且只会产生一行提示。 |
| `bucket_sync_mode` | `auto` 在到期时自动刷新仓库；`manual`（默认）仅在命令触发时刷新。 |
| `bucket_check_interval_minutes` | 自动仓库刷新检查之间的最小间隔。 |
| `parallel_download` | 启用并行分段下载。 |
| `download_workers` | 并发下载工作线程数。 |
| `cancel_direct_max_mb` | 可取消下载所用单次 GET 路径的大小上限（MiB，0–32，默认 16）。更大的文件通过 `.partial` 保持可续传；`0` 强制所有大小都走可续传路径。 |
| `color` | 交互式终端的彩色输出。 |
| `verbose` | 详细诊断信息。 |

`cancel_direct_max_mb` 用可续传性换延迟：在该上限及以下时，可取消下载是一次 GET — 在高延迟链路上更快，但取消后
需要重新下载而不是续传；超过上限则使用可续传的 `.partial` 路径。

### GitHub 镜像

首次运行时会探测内置镜像与 GitHub 直连并保留最快选项，也可用 `glue config set github_proxy …` 显式设置。前缀只
作用于 GitHub URL——`claude-code` 由 `storage.googleapis.com` 提供，请用 `proxy` 加 `proxy_packages` 访问。

### 代理

下载支持 HTTP `CONNECT` 代理；不支持 SOCKS 和 PAC。只想让部分下载走代理时，把 `proxy` 设为 URL 并在
`proxy_packages` 中列出这些软件包：

```json
{
  "proxy": "http://127.0.0.1:7897",
  "proxy_packages": ["claude-code"]
}
```

`proxy` 默认 `auto`，此时还会参考 `HTTPS_PROXY` / `HTTP_PROXY` / `ALL_PROXY` 和 Windows 系统代理。

## 数据布局

工作区是 Gluestick 唯一拥有的**数据**目录（默认 `%USERPROFILE%\.glue`，用 `glue root show` 查看、`glue root set
<dir>` 移动）：

```
<workspace>/
  apps/<package>/<version>/     已安装的软件包文件（current/ 指向当前版本）
  shims/                        生成的 shim 启动器
  shims-meta/<name>.json        shim 目标元数据（由 shim.exe 读取）
  buckets/<name>/bucket/        克隆的 Scoop 仓库清单
  bin/                          引导的辅助工具（7-Zip、MinGit、innounp、WiX dark）
  cache/                        SQLite 缓存索引和跨进程锁
  store/                        内容寻址存储（SHA-256）
  templates/                    用户模板（按名称替换内置模板）
  persist/<package>/            持久化的软件包数据
```

软件包文件从 `store/` 硬链接到 `apps/`，因此重装通常复用缓存内容而不是重新下载。缺失的辅助工具会按需引导到
`bin/`。

## 从源码构建

从 [rustup.rs](https://rustup.rs/) 获取 `rustup-init.exe`，运行它并接受默认选项（"Proceed with standard
installation"）。这会安装稳定版 MSVC 工具链，并把 `cargo`、`rustc`、`rustup` 和 `rustfmt` 放到 `PATH` 中；如果没有
Visual C++ 生成工具，`rustup-init` 会提出为你安装。

仓库在 [`rust-toolchain.toml`](rust-toolchain.toml) 中固定了 Rust 1.99.1、`rustfmt`、`clippy` 以及两个 MSVC 目标，
rustup 会在检出目录中第一次运行 `cargo` 时准确安装。MSVC 工具链还需要带 Windows SDK 的 Visual C++ 生成工具
（Visual Studio 安装程序中的 "Desktop development with C++"），包含你要构建的架构的 C++ 工具集 — x64 主机即 x64 加
ARM64。打包还需要提供 `makensis.exe` 的 NSIS 3.x。

```powershell
cargo fmt --all -- --check
cargo clippy --workspace --all-targets -- -D warnings
cargo test --workspace
installer\cli\build.ps1 -Arch amd64
```

输出为 `installer\cli\output\GluestickSetup-<arch>.exe` 及配套的 `.sha256` 文件。

工具链细节、把单个架构交叉构建到安装脚本期望的布局、配套的 `CARGO_TARGET_DIR` / `RUSTC` 固定方式，以及两种架构的
打包，见 [CONTRIBUTING.md](CONTRIBUTING.md)（英文）。生成的安装包运行时做了什么，见[安装 Gluestick](#安装-gluestick)。

## 安装 Gluestick

CLI 有两种安装方式，程序文件所在的位置不同。两者都面向 amd64 与 arm64 的 Windows 10/11，且全程按用户安装：
不需要管理员权限，不改动系统级设置，也不触及已有的 Scoop 环境。

| | PowerShell 脚本 | 图形安装包 |
| --- | --- | --- |
| 程序文件 | `%USERPROFILE%\.glue` | `%LOCALAPPDATA%\Programs\Gluestick CLI` |
| `config.json` | `<workspace>` | `glue.exe` 旁 |
| “应用和功能”条目 | — | 有，含 `Uninstall.exe` |
| 工作区 | `%USERPROFILE%\.glue` | `%USERPROFILE%\.glue` |

`glue path setup` 会向用户 `PATH` 添加两项 — `<workspace>\shims` 和 `glue.exe` 所在目录 — 因此两种布局的
差别只在程序文件的位置。

### 图形安装包

图形安装包由 `installer\cli\build.ps1` 构建，把二进制、`.glue-install` 标记、`data-root.txt` 和 `config.json`
放进安装目录，工作区（`apps/`、`shims/`、`store/`、`buckets/`、`cache/`）则在 `%USERPROFILE%\.glue`。它会在
二进制旁写入 `Uninstall.exe` 并注册到“应用和功能”；卸载只删除程序文件（含 `config.json`），从不删除工作区。

组件页安装 CLI（必选）和 Vibe Coding 模板（默认选中，`/NOTEMPLATE=1` 可跳过）。随后软件包页列出
`claude-code`、`codex` 和 `extras/magpie-ai`：三个默认全选，只安装选中的软件包。

安装前会检查磁盘空间（至少 64 MB，选中模板时为 1 GB）。模板下载失败只报告，不会回滚 CLI。GUI 在详情窗格显示
模板进度，整个运行写入 `<install dir>\install.log`，完成页可打开该日志。

升级就是运行更新的安装包；Gluestick 从不下载或替换自己的可执行文件。自行构建请见[从源码构建](#从源码构建)。

## 性能

独立的 `gluestick-bench` 工具让 glue 与 Scoop 在相同夹具下对比并报告中位数。日常命令——启动、`search`、`list`——
只需几十毫秒，比 Scoop 快约一个数量级，安装也快数倍。方法、夹具与完整数字见该工具仓库自己的文档。

## 项目结构

```
Cargo.toml                  workspace、共享依赖和发布配置
.cargo/config.toml          内置 SQLite 的编译参数（体积裁剪）
crates/gluestick-cli/       `glue` 命令行（clap），基于 gluestick-core
crates/gluestick-core/      引擎：清单、下载器、CAS 存储、缓存索引、shim
crates/gluestick-shim/      shim 运行器（`shim.exe`），复制到每个已安装工具旁
installer/cli/              MSVC 构建脚本、NSIS 定义以及生成的载荷/输出
docs/                       安装包参考文档
```

CLI 本身不含软件包逻辑：参数解析、进度渲染和表格输出在 `glue` 中，而每个操作都是对 `gluestick-core` 的 `Engine`
调用。

## 桌面应用与套餐

本仓库中的 `glue` CLI 是引擎。Gluestick 桌面应用把仓库、软件浏览、模板、已安装软件、更新、存储和活动记录集中
到一个窗口，并提供 7 套官方主题、11 套官方模板以及本地快照备份与回滚。

社区版永久免费。Pro 增加云端备份与主/从设备的软件清单同步；Team 增加环境健康监控、共享配置模板和成员权限
管理。

## 贡献

欢迎贡献。[CONTRIBUTING.md](CONTRIBUTING.md)（英文）涵盖工具链、提交拉取请求前要运行的检查、按架构构建与打包。

## 许可证

MIT 许可。详见 [LICENSE](LICENSE)。
