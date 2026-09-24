# gluestick.sh 转型路线图：Agent-Ready on Windows

> 状态：**已批准生效**（v1.0，2026-09-24 评审通过，见第 8 节决策记录）
> 依据：对 `c:\github.com`（cli / core / shim 等仓）的实际代码审计 + gluestick.sh 官网现状
> 原则：**不推翻重来**。命令动词层保持稳定，转型 = 接口强化 + 新增 agent 原生入口。

---

## 1. 新定位

**主标语**：**Glue makes Windows agent-ready.**
**副标语**：Scoop-compatible · `--json` · MCP server · zero interaction

**中文释义**：Glue 让一台 Windows 机器达到"agent 可直接使用"的状态——装好工具链、配好 PATH、可体检、机器可读、零交互。

**"Agent-ready" 的产品级定义（可检验，非口号）**：

> 一台 Windows 机器是 agent-ready 的，当且仅当：一个 AI agent 能在**零人工介入**下完成"安装工具链 → 验证 PATH → 体检环境 → 执行工具"全流程。

产品闭环由此打通：`glue install` / `glue path setup`（准备）→ `glue doctor`（体检，Phase 1；传统环境检查在 `glue env`）→ `glue mcp`（agent 原生调用，Phase 2）。`glue doctor` 的退出码即"这台机器是否 agent-ready"的机器可读答案——定位与产品功能闭环。

**定位表达规范**（避免歧义，评审 2026-09-24 定稿）：
- ✅ 主语必须是产品（Glue），Windows 是宾语，agent 是受益者：`Glue makes Windows agent-ready.`
- ❌ 不用 "An agent makes agent-ready on Windows"——主语错误（暗示 Glue 本身是个 agent；实际它是 agent 的工具层/环境层），且 "agent" 二现指代不清。
- ❌ 不用 "AI-ready"（太泛）、"agent-native"（暗示产品本身是 agent）。
- 旧表述 "the agent-ready package manager for Windows"（产品属性视角）降级为备选，仅在需要明说"包管理器"品类时使用。

**为什么这个转型成立**：

1. **刚需对齐**：agent（Cline / Claude Code / Cursor / Codex 等）在 Windows 上最高频的实操需求就是：安装工具链（git、node、python、7zip…）、配置 PATH、体检环境。这正是包管理器的本职。
2. **语料优势**：Scoop 风格命令（install/search/list/bucket）大量存在于 agent 训练语料与现有 CLAUDE.md/规则文件中，agent 遇到 `glue install git` 几乎零学习成本——"零学习曲线"从人类用户延伸到了 agent。
3. **地基已存在**：现有 CLI 已具备 `--json` 全局 flag、结构化 `jsonCommandResult`、稳定退出码（0 成功 / 1 操作失败 / 2 用法错误）、`NO_COLOR`、非交互设计。转型是强化而非重写。
4. **差异化**：Scoop 是 PowerShell 脚本（agent 调用输出不可预测、慢、无 JSON）；WinGet/Chocolatey 面向人类 GUI 场景。"为 agent 设计的 Windows 包管理器"目前是空位。

**转型后的产品分层**：

```
        ┌─ Cline / Claude Code / Cursor 等 agent
        │      │
        │   glue mcp  (MCP server, stdio)      ← 新增：agent 原生入口
        │      │
agent ──┼── glue CLI (--json, 稳定退出码)        ← 强化：机器可读、幂等、无交互
        │      │
        │   core engine (embeddable Go 库)      ← 不变：唯一事实来源
        │      │
        │   CAS store / buckets / shims / SQLite
```

CLI 与 MCP server 都只是 core engine 的**薄壳**——同一套引擎保证两条路径行为完全一致。

---

## 2. 现状盘点（代码审计结论）

### 2.1 模块现状

| 模块 | 内容 | 转型相关性 | 处置 |
|---|---|---|---|
| `cli`（glue.exe） | Scoop 风格 CLI，约 20 个命令 | ⭐ 核心 | 迁入新工程，强化 |
| `core`（engine） | 可嵌入引擎：manifest/CAS/并行下载/SQLite/24 个子包 | ⭐ 核心 | 迁入新工程，作为 MCP 宿主 |
| `shim` | PATH shim runner | ⭐ 核心 | 迁入新工程 |
| `gateway` / `api` | Node 网关 + API（服务 Desktop 同步） | 🔕 | 冻结，不迁移 |
| `desktop` / `desktop-pro` | 桌面客户端 | 🔕 非重点 | 冻结，不迁移 |
| `web` | 营销官网 | 🔕 非重点 | 冻结；仅后期改一句定位文案 |

### 2.2 已有的 agent 友好基础（保留并扩展）

- `--json` 全局 flag + `jsonCommandResult{command, ok, results[], error}` 结构
- 退出码约定：`0` 成功、`1` 操作失败、`2` 用法错误（`exitcode.go`）
- `NO_COLOR` / `FORCE_COLOR` / 非 TTY 自动降级——对管道与 transcript 天然友好
- 配置持久化于 `~/.glue/config.json`，`device.json` 提供稳定设备身份

### 2.3 `--json` 覆盖审计（逐命令实测）

**已有 JSON 输出（7 个）**：`install`、`uninstall`、`update`、`search`、`list`、`info`、`doctor`

**尚无 JSON 输出（其余全部）**：`bucket *`、`cache *`、`config *`、`depends`、`hold`/`unhold`、`home`、`path *`、`reset`、`completion`、`engine`

> 审计勘误（2026-09-24）：`installed.go` 是辅助函数文件而非命令（已装包检查走 `list`），初版误记为命令，无需单独补 JSON。

**结论**：agent 最高频的读路径（search/list/info/doctor）已就绪，写路径大半就绪；缺的主要是 bucket/cache/config/path 这类环境准备动作——恰恰是 agent 装环境时必用的。

---

## 3. 原有命令的三层处理（问题 1 的答案）

### 3.1 第一层：保留层 —— 全部照留，一个不删

**结论：所有现有命令、动词拼写、Scoop 兼容语义全部保留。**

理由：
- agent 语料 + 现有用户零迁移成本；
- 包管理动词本身就是 agent 在 Windows 干活的原语，不需要发明新动词；
- 转型的差异在**接口**（JSON/退出码/MCP），不在**动词**。

### 3.2 第二层：强化层 —— 逐命令 agent 化改造

统一硬化标准（每条命令都要过这个清单）：

- [ ] `--json` 全覆盖，包括**错误路径**（错误必须进 JSON `error` 字段，同时退出码正确）
- [ ] 错误结构化：`{code, message, hint}`（machine-readable code，而非仅人类文本）
- [ ] 无任何隐式交互确认；需要确认的命令加 `--yes`
- [ ] 幂等：重复执行同一条命令结果稳定、无副作用
- [ ] 进度条在非 TTY 下降级为单行摘要（agent 读不了 `\r` 刷屏动画）

逐命令处置表：

| 命令 | JSON 现状 | 硬化动作 | 优先级 | 完成时间 |
|---|---|---|---|---|
| `install` | ✅ | 错误结构化 ✅（code/hint）；非 TTY 单行化 ✅（JSON 模式 silent reporter）；`--yes` 随策略闸门（Phase 2，§4.6.2） | P0 | - |
| `uninstall` | ✅ | 同上；缺包错误已类型化（`package_not_installed`） | P0 | - |
| `update` | ✅ | `--all` 幂等；错误结构化 ✅（code/hint） | P0 | - |
| `search` | ✅ | 输出 schema 固定并写入文档 | P0 | - |
| `list` | ✅ | schema 固定入文档 | P0 | - |
| `info` | ✅ | schema 固定入文档 | P1 | - |
| `doctor` | ✅ | 增补 agent 视角检查项（见 3.3） | P0 | - |
| `depends` | ✅ | 补 JSON（agent 判断装什么依赖）；错误结构化 ✅ | P0 | 2026-09-24 |
| `bucket add/list/update/check/known` | 全 ✅ | 补 JSON；`add` 幂等化（already_installed） | P0 | 2026-09-24 |
| `path show/check/setup` | 全 ✅ | 补 JSON；`setup` 输出 PATH 变更明细（bin_dir/changed） | P0 | 2026-09-24 |
| `config get/set/unset/list` | 全 ✅ | 补 JSON（三态布尔以真实布尔+set 标记输出；set 幂等可写全新根） | P1 | 2026-09-24 |
| `cache list/clear/gc/rebuild` | 全 ✅ | 补 JSON（gc/clear 空 reporter，stdout 纯净） | P2 | 2026-09-24 |
| `hold` / `unhold` / `reset` | 全 ✅ | 补 JSON；hold 缺包错误类型化（`package_not_installed`） | P2 | 2026-09-24 |
| `home` | ✅ | 补 JSON + `--no-open`；JSON 模式**永不弹浏览器**（URL 入 `results[].url`） | P2 | 2026-09-24 |
| `completion` | — | 不动 | P3 | - |

### 3.3 第三层：新增层 —— agent 原生入口

| 新增 | 说明 | 优先级 |
|---|---|---|
| `glue mcp` | MCP server 子命令（见第 4 节完整设计） | P0，转型的旗帜能力 |
| `glue doctor`（默认=就绪视图；`glue env`=传统环境） | agent 视角体检 JSON（23 项分组检查，`checks[].group` = Machine/Shell/Runtime/Agent compatibility/Workspace/Glue：PATH/shims、bucket、网络与代理、工具链与版本、重复运行时探测、agent CLI 共存、编码/执行策略/PowerShell profile 集成/git 桶配置/文件系统/WSL/路径形态）、就绪评分 `score`（additive）、`--fix` 安全修复计划（修后重跑、逐项列出标题：Configure UTF-8 / Configure PowerShell 7 / Repair PATH / Configure Git Bash / Configure Git / Fix shell integration / Detect duplicate runtimes 等；含 Glue 原生安装 main/pwsh 与 HKLM UTF-8，均需显式 `--fix`，agent 永不自动安装；`GLUE_DOCTOR_FIX_SKIP` 逐项跳过）；传统 7 项环境检查由 `glue env` 提供 | P1 |
| `AGENTS.md` | 仓库级 agent 说明（构建/测试/约定），让任何 agent 进仓即懂 | P0（成本极低） |
| `glue audit list` | 审计查询命令：谁（actor/source）何时执行了什么、结果如何（§4.6.1）；`--json` 人机同 schema | P2（随 MCP 上线） |
| JSON Schema 文档 | 每个命令的输出 schema 在文档中固定并纳入 semver | P1 |

---

## 4. `glue mcp` —— MCP Server 设计

### 4.1 技术选型

| 决策点 | 选择 | 理由 |
|---|---|---|
| SDK | `github.com/modelcontextprotocol/go-sdk`（官方 SDK，已发布 v1.x） | 官方维护、Go 原生、与 core 同语言可直接嵌入 |
| 传输 | **stdio** | Windows 上零端口占用、零防火墙弹窗，agent 客户端直接拉起进程 |
| 进程形态 | `glue mcp` 子命令（同二进制内） | 不新增分发物；`glue mcp` 即可在 Cline/Claude 的 MCP 配置中引用 |
| 引擎 | 直接调用 core `engine` 包 | 与 CLI 同一事实来源，行为保证一致 |

### 4.2 Tools 一览（首版）

| MCP tool | 映射 | 说明 |
|---|---|---|
| `glue_search` | engine.Search | 搜索 bucket manifests |
| `glue_list` | engine.List | 已装包列表 |
| `glue_info` | engine.Info | 包详情（版本、路径、shims） |
| `glue_install` | engine.Install | 装包；含 `yes` 参数默认需确认 |
| `glue_uninstall` | engine.Uninstall | 卸载；默认需确认 |
| `glue_update` | engine.Update | 更新检查/执行 |
| `glue_depends` | engine.Depends | 依赖体检 |
| `glue_doctor` | engine 体检 | agent 视角环境体检 |
| `glue_bucket_list` / `glue_bucket_add` / `glue_bucket_update` | engine.Bucket | bucket 管理 |
| `glue_path_check` | engine/shim | PATH 与 shim 可用性检查 |

首版刻意**不做**的：
- 任意 shell 执行类 tool（安全边界）；
- `config set`（agent 不应静默改用户全局配置；只读 `config get/list` 首版可不做，第二版再评估）。

### 4.3 输出与错误约定

- 所有 tool 返回**结构化 content**（JSON），字段与 CLI `--json` 输出同源同一 schema——文档只需维护一份；
- 错误统一：`{ "code": "...", "message": "...", "hint": "..." }`，`code` 取自 core `apperr` 体系的稳定错误码；
- 破坏性操作（install/uninstall/update 执行）暴露 `confirm` 语义：MCP tool 参数中默认 `yes=false` 需 agent 二次确认，用户也可在 `config.json` 中设 `agent.auto_yes=true` 关掉确认（供 CI/受信环境）。

### 4.4 agent 端接入示例（写入官网文档）

```json
{
  "mcpServers": {
    "glue": { "command": "glue", "args": ["mcp"] }
  }
}
```

### 4.5 调用链路：谁在调用这些命令

```
agent ─── MCP (stdio) ─── glue mcp ─┐
                                   ├─ core engine ── SQLite / CAS / shims
agent ─── shell ─────── glue CLI ───┘
```

- 两条路径都会被 agent 使用：配置了 MCP 的 agent 走 tool call（结构化、零解析成本）；未配置的 agent 直接跑 CLI（`--json` + 退出码）。
- **关键原则：审计与安全必须放在 core engine 层（唯一事实来源），不是 MCP 壳层**——无论哪条路径进来、无论未来加什么新入口（Desktop、第三方），规则只实现一次、处处生效。

### 4.6 审计与安全设计

#### 4.6.1 审计（在已有 SQLite 地基上补齐）

已有：`activity_log`（全局活动流，无外键，卸载不级联删除）+ `install_history`（按包审计行），见 `core/cache/index.go`、`core/engine/activity.go`。Phase 2 补齐四件事：

1. `RecordActivity` 增加 **`source`**（`cli` | `mcp`）与 **`actor`**（MCP clientInfo 的名称/版本，如 `cline/3.7`）字段——回答"是谁干的"；
2. 破坏性操作记录**完整决策链**：`confirm_required → granted/denied`（含 confirm token 与拒绝原因）；
3. 双写 append-only 的 **`~/.glue/logs/audit.jsonl`**——SQLite 会被 `glue cache clear` 类操作误伤，JSONL 只追加、可做防篡改基线；
4. 新增 **`glue audit list`** 查询命令（`--json`、`--source mcp`、`--since`、`--package`），人与 agent 读同一 schema。

同时：MCP server 把每次 tool call 的 request/result 单行摘要写 stderr——agent 客户端的 transcript 天然可见，属零成本旁路审计。

#### 4.6.2 安全拦截（policy gate，放 core engine 层）

每个 tool/命令先过**操作分级**：

| 级别 | 操作 | 默认策略 |
|---|---|---|
| `read` | search/list/info/depends/doctor/path check | 无确认，自由调用 |
| `write` | install/bucket add/update | 默认需确认 |
| `destructive` | uninstall/reset/cache clear | 默认需确认 + **可被 denylist 硬拦截** |
| `config` | config set | MCP 首版不暴露（§4.2）；如暴露默认 deny |

`config.json` 增加 `agent.policy` 段（决策 2 的完整化）：

```json
{
  "agent": {
    "auto_yes": false,
    "policy": {
      "mode": "confirm",
      "deny": [],
      "protected": ["git"]
    }
  }
}
```

- `mode`：`strict`（全部需确认）| `confirm`（默认）| `auto`（仅受 deny/protected 约束，供 CI）；
- `deny`：**硬拦截清单**——命中 deny 的操作无论 `yes` 与否直接拒绝，返回结构化错误 `denied_by_policy` 并写审计。**这就是"拦截某些删除操作"的机制**（如 `deny: ["uninstall"]` 即全量禁删）；
- `protected`：**包级保护**——受保护的包在 MCP 路径不可 uninstall/reset；人类经 CLI 用 `--force` 可越过（人机权限不对称：agent 低权、人高权）。

**确认机制（client 无关的二次确认）**：destructive/write 操作默认返回
`{ "status": "pending", "confirm_token": "...", "expires_in": 300 }`，agent 须带 token 调用 `glue_confirm` 才真正执行。token 一次性、短时效、绑定操作摘要。选择 token 而非依赖 MCP elicitation：**任何 MCP 客户端都走同一流程**，不受客户端能力差异影响。`agent.auto_yes=true` 跳过整个流程（决策 2）。

**hold 语义扩展**（Phase 1 落地）：现有 `hold` 只挡升级（`setVersionLock`）；扩展为对 MCP 路径同时挡 uninstall——作为包级"防 agent 误删"的第二道闸，与 `protected` 互补（hold 是 Scoop 兼容动词、agent 也能理解，protected 是策略层的硬约束）。

**既有边界继续生效**：不提供 shell 执行类 tool（§4.2）；`safepath` 保证安装路径不可逃出 `~/.glue` 根（路径穿越/zip-slip 已有测试）；错误全部走 `apperr` 结构化。

---

## 5. 工程结构建议（问题 2 的答案）

### 5.1 推荐：单仓 monorepo（新工作区 `c:\gluestick-sh`）

```
c:\gluestick-sh\                 # 单一 .git
├── go.work
├── AGENTS.md                    # agent 进仓即懂
├── README.md                    # 新定位
├── docs\
│   ├── agent-ready-roadmap.md   # 本文档
│   ├── json-schema.md           # 各命令 JSON schema（P1 固化）
│   └── mcp.md                   # MCP 接入文档
├── cli\          # 由 github.com\cli 迁入（glue 命令 + glue mcp）
├── core\         # 由 github.com\core 迁入（engine）
├── shim\         # 由 github.com\shim 迁入
└── .github\      # 统一 CI：build + test（三模块矩阵）
```

**推荐单仓的理由**：
1. 转型期改动必然跨 cli/core 频繁联动（JSON schema、错误码、MCP 都横跨两仓），单仓消除"发布 core 版本 → 升 cli 依赖"的往返；
2. `go.work` 在仓内即可用，本地开发不再依赖"放在兄弟仓旁边"的约定；
3. 新增 `glue mcp` 需要同时动 cli 与 core，单仓原子提交保证一致性；
4. 冻结的 web/desktop/gateway/api **不迁移**，天然隔离。

**何时选方案 B（保持三仓）**：如果你要严格保持 `gluestick.sh/core` 作为独立可 `go get` 的公开库版本（README 中已宣传此用法），则三仓 + 顶层 go.work 维持现状，仅在旧仓上开发。代价是跨仓联动发版慢。

> 折中方案：单仓起步，`core` 模块保持独立 module path（`gluestick.sh/core`），未来需要独立发版时再拆出——Go workspace 内单仓多 module 两者兼得。

### 5.2 非重点模块的冻结策略

| 模块 | 处置 |
|---|---|
| `web` | 不迁移；官网保持在线；仅 Phase 3 改首页一句话定位 + 增加 "For AI agents" 文档页 |
| `desktop` / `desktop-pro` | 不迁移、不投入；现有下载链接继续指旧 release；README 顶部加 "maintenance freeze" 说明 |
| `gateway` / `api` | 不迁移；它们服务于 Desktop 同步，随 Desktop 一并冻结 |
| `gluestick-sh-old`（c:\ 下的旧副本） | 保持只读归档，不动 |

### 5.3 开发/数据隔离机制（2026-09-24 已落地）

开发构建与真实安装的隔离**由代码强制**，不依赖人工备份/改名目录：

| 机制 | 规则 |
|---|---|
| 数据根按 exe 名推导 | `glue.exe → ~/.glue`；`glue-alpha.exe`（任意 `glue-<suffix>`）`→ ~/.glue-<suffix>`，后缀仅限字母/数字/横线，`glue.test` 等测试二进制回落默认根。实现：`cli/glue/root.go` |
| `--root` 隐藏 flag | 最高优先；单元测试用它指向临时目录 |
| shim 自定位 | shim 运行器按**自身位置**解析配置：`<root>/shims/<name>.exe → <root>/shims-meta/<name>.json`（正常安装与旧硬编码路径逐字节一致，行为零变化）；`GLUE_DATA_ROOT` env 可覆盖；`~/.glue` 仅作兜底。实现：`shim/main.go` |
| 规范 | 开发构建一律命名 `glue-alpha.exe`——已写入 AGENTS.md 硬规则，任何 agent 进仓遵守；实测真实 `~/.glue` 零触碰 |

该机制同时服务 §4 的 MCP/审计设计：未来 `glue mcp`、策略文件、`audit.jsonl` 都落在同一数据根下，天然按安装实例隔离（一个真实安装 + 一个 alpha 实验根可并存互不干扰）。

**背景**：原 shim 运行器硬编码 `~/.glue`——alpha 根安装的包，其 shim 运行时会去真实根找 `shims-meta` 配置，同名包会静默执行到真实环境配置。此漏洞已随本机制修复。

---

## 6. 阶段路线与验收标准

| 阶段 | 内容 | 验收标准 |
|---|---|---|
| **Phase 0：奠基**（~1 周） | 单仓落地（迁 cli/core/shim）；`AGENTS.md`；CI 跑通三模块 build+test；本路线图评审定稿 | 新仓一条命令全绿：`go work sync && go test ./...` |
| **Phase 1：CLI agent 化**（2–3 周） | P0 表逐项完成：全命令 `--json` 覆盖（含错误路径）、`--yes`、错误结构化、非 TTY 单行化、`doctor` 增补 agent 检查项 | 用一段 agent 模拟脚本（纯管道、无 TTY）完成"search→install→path check→doctor"全程仅靠 JSON+退出码判定成败 |
| **Phase 2：MCP 上线**（3–4 周） | `glue mcp` stdio server，首版 tools（4.2 表）；MCP 接入文档 | Cline/Claude Code 实机通过 MCP 完成一次"体检环境→装 git→验证 PATH" |
| **Phase 3：对外**（视情况） | 官网定位文案 + agent 接入页；JSON schema 文档固化；评估 desktop 是否接 MCP | 官网一键接入配置可复制即用 |

---

## 7. 兼容性承诺与风险

**兼容性承诺（对外口径）**：
- 命令动词与 Scoop 兼容语义**永不破坏**；
- JSON schema 纳入 semver：字段只增不改不删，重命名走新字段 + 弃用期；
- 退出码 0/1/2 约定不变。

**风险与对策**：

| 风险 | 对策 |
|---|---|
| MCP 规范仍在演进 | 用官方 go-sdk 承担协议兼容；tools schema 是我们自己控制的稳定层 |
| agent 误操作（错删/错装） | 四道闸（§4.6.2）：denylist 硬拦截（`denied_by_policy`）→ protected 包级保护 → confirm token 二次确认 → hold 语义扩展挡卸载；另不提供 shell 执行类 tool，`safepath` 禁止写路径逃出 `~/.glue` |
| 旧仓历史去向 | 新仓以无历史基线起步；旧历史保留在原独立仓（本地 `c:\github.com` 与 GitHub `gluestick-sh/{cli,core,shim}`）只读可查，README 提供原仓链接 |
| 官网下载入口指向旧 release | 冻结不等于下线，Phase 3 前不动线上资产 |

---

## 8. 决策记录（2026-09-24 已拍板）

| # | 决策点 | 结论 |
|---|---|---|
| 1 | 仓库结构 | ✅ **单仓 monorepo**（`c:\gluestick-sh`：cli/core/shim 子目录 + 仓内 `go.work`；**无历史快照重建**——单次基线提交，不带旧仓提交历史） |
| 2 | 破坏性操作 | ✅ **默认二次确认**（MCP install/uninstall/update 默认 `yes=false`；`agent.auto_yes=true` 可关，供 CI/受信环境） |
| 3 | core 独立发版 | ✅ **继续独立 `go get` 发版**，机制见下 |

### 8.1 core 独立发版机制（决策 3 的落地说明）

**审计事实**：`core/go.mod` 当前 module path 为 `github.com/gluestick-sh/core`；core README 宣传的 `go get gluestick.sh/core@v0.1.0` 目前**不可用**（`gluestick.sh/core?go-get=1` 返回 404，无 go-import meta）。

**机制**（分两步，Phase 1 完成协调切换）：

1. **Phase 0（已完成迁移时）**：单仓内 module path **保持 `github.com/gluestick-sh/core` 不变**，保证工作区模式立即可用、行为零变化；已发布的 `v0.1.x` 系列仍从旧独立仓 `gluestick-sh/core` 解析。
2. **Phase 1（协调切换）**：module path 迁移为 **`gluestick.sh/core`**（vanity，与 README 既有宣传一致），配套两件事：
   - `gluestick.sh` 官网加 go-import meta：`<meta name="go-import" content="gluestick.sh/core git https://github.com/gluestick-sh/gluestick-sh">`（指向本 monorepo）；
   - 本仓发版使用子目录 tag：`core/v0.2.0`、`cli/v0.2.0`、`shim/v0.2.0`。
   - 旧 `github.com/gluestick-sh/core` 保留最后版本只读，README 注明迁移指引（`go get gluestick.sh/core`）。

**约束**：module path 切换是破坏性的，必须与官网 meta 上线**同一时间窗口**完成，并写入发版 CHANGELOG。

---

## 9. 实施进展（随工作滚动更新）

| 日期 | 事项 | 验证 |
|---|---|---|
| 2026-09-24 | **Phase 0 完成**：单仓落地（无历史基线重建，决策见 §8）、`AGENTS.md`、CI、路线图 v1.0 定稿 | 三模块 build + 30 测试包全绿 |
| 2026-09-24 | **Phase 1 第一批**：`depends`、`bucket list`、`path show`、`path check` 补 `--json`（含错误路径与退出码契约，`ok:false → exit 1`）；`bucket list` 的 git 警告不再污染 stdout | 新增 7 项测试全过；实机冒烟 4 命令 JSON + 退出码正确 |
| 2026-09-24 | **开发/数据隔离机制**（§5.3）：数据根按 exe 名推导（`glue-alpha.exe → ~/.glue-alpha`）+ shim 自定位 + `GLUE_DATA_ROOT` env；堵住 shim 硬编码 `~/.glue` 的隔离漏洞 | 实测真实 `~/.glue` 运行前后零变化；shim 自定位 E2E 通过 |
| 2026-09-24 | **审计勘误**（§2.3）：`installed.go` 为辅助函数而非命令 | — |
| 2026-09-24 | **Phase 1 第二批**：`bucket add`（幂等）/`update`/`check`/`known`、`path setup` 补 JSON；**结构化错误**（`code`/`hint` 字段）接入全部 JSON 输出（install/uninstall/update/depends/bucket）；core 卸载与重置的缺包错误类型化（`apperr.PackageNotInstalled`，文案不变）；`SilenceUsage/SilenceErrors` 补齐 depends/doctor/path\*/bucket\*（stderr 无 Usage 噪音，实测确认） | 新增 6 项测试全过（bucket 4 + uninstall 错误码 + depends code 断言）；实机冒烟：`package_not_installed`/`bucket_unknown`+hint 正确，退出码 1 |

| 2026-09-24 | **Phase 1 第三批（P1/P2 清尾）**：`config`（get/set/unset/list）、`cache`（list/clear/gc/rebuild）、`hold`/`unhold`、`reset`、`home` 补 JSON——**全命令 `--json` 覆盖至此 100%**（completion P3 除外）。附带：config 三态布尔以真实布尔输出（弃 "true (default)" 字符串）；`home --no-open` 新 flag，JSON 模式永不弹浏览器；`config set` 可写全新根（自动建目录）；hold 缺包错误类型化；SilenceFlags 补齐 config/cache/hold/home/info | 新增 9 项测试全过（config 3 + cache 3 + hold/reset 2 + home 1）；实机冒烟：config set 新根 ok、hold 缺包 code=`package_not_installed` |

| 2026-09-24 | **Phase 1 P0 收尾**：`glue agent doctor`（§1 定位的可检验落地）——10 项检查（6 required + 4 optional）、`agentReady` + 退出码 0/1、`--json` 报告（`schemaVersion`/`summary`/`nextActions`/`error:null`）、`--offline`/`--probe-shim`；`DoctorCheck` 增 `level`/`status`/`data`（additive）；Store 别名遮蔽检测与 `PathDirPrecedes` 上移 core（CLI 委托去重）；`isDoctorCommand` 识别 `agent doctor` | 新增 core 6 项 + CLI 3 项测试全过；全量 33 包绿（`-count=1`）；实机冒烟：temp 根 required 失败项正确且 exit=1、dev 根 `~/.glue-alpha` 隔离、真实 `~/.glue` 零变更 |

| 2026-09-24 | **doctor 命令合并（评审定稿）**：`glue agent doctor` 并入 `glue doctor` 默认视图（JSON `command` 改为 `"doctor"`），传统 7 项环境检查保留为 `--env`（flag 名定稿 `--env` 而非 `--legacy`——描述内容而非"旧版"）；`--env` 与 `--offline`/`--probe-shim` 组合 → usage error 2（打印自解释 hint）；`doctor` Args 收紧（多余位置参数 → exit 2，原静默忽略）；`isDoctorCommand` 回归仅识别 `doctor`；core 注释与 README/cli README/§1/§3.3 同步 | 全量 33 包绿（31 ok + 2 无测试，`-count=1`，0 FAIL）；实机冒烟 8 项：默认视图 exit=1、`command=doctor`、`error:null`、checks=10、`--env` exit=0 传统 schema（无 `agentReady`）、`--env --offline` exit=2 + hint、`doctor extra` exit=2 + hint、`glue agent doctor` unknown command exit=2 |

| 2026-09-24 | **doctor 分组视图 + 就绪评分 + `--fix`**（评审定稿“最值得做的产品”全量落地）：默认视图改为分组展示（Machine / Shell / Runtime / Agent compatibility / Workspace / Glue），新增 10 项环境就绪检查（`machine_os`/`machine_arch`、`shell_pwsh`/`shell_utf8`/`shell_exec_policy`/`shell_git_bash`、`agents`（Claude Code/Codex/OpenCode 共存探测）、`ws_fs`/`ws_wsl`/`ws_path`，全部 advisory——6 项 blocking 集合与退出码 0/1/2 语义不变）；toolchain/agents 渲染 per-tool 子行（git/node/npm/python 版本探测，`data.tools`）；报告 additive 字段 `score`（blocking×2、advisory×1、skipped 不进分母；`agentReady` 仍独自决定退出码）与 `checks[].group`；`--fix` 安全子集（`shim_path→path setup`、`buckets→bucket add main`、`data_root→mkdir`，修后重跑，`fixes[]` 进 JSON，`GLUE_DOCTOR_FIX_SKIP` 可跳过指定项；不自动装包、不改执行策略）；修复中发现并解决 `e.BucketRegistry` 同进程内存态陈旧问题（fix 直接使用引擎自身 registry）；marks 统一 ✓/✗/⚠/—（`color.go` 增 markWarn/markSkip，沿用 NO_COLOR/TTY 既有解析）；Result 区（Agent Ready yes/no + score + Next + `Run glue doctor --fix` footer）；`--env`+`--fix` → usage 2 | 全量 33 包绿（31 ok + 2 无测试，`-count=1`，`core/git` 一次网络抖动重跑即绿）；实机冒烟 10 项：分组人视图 exit=0/`Agent Ready: yes — score 72%`、`--json`（command=doctor/score=72/checks=20/六组/fixes 缺省/error:null）、`--env` exit=0、`--env --fix` exit=2+hint、`doctor extra` exit=2、`glue agent doctor` unknown exit=2、`--help` 含 `--fix`、dev 根 `--fix` 无修复提示 exit=0、temp 根 `--fix --json` 真实 `bucket add main`：applied=true → 重跑 buckets pass（1 buckets, 1638 packages）、blocking=[shim_path]、score 56→64；gofmt 内容级全净（`keys.go` 仅剩 HEAD 预存漂移，未触碰） |
| 2026-09-24 | **`--fix` 全量修复计划（roadmap §3.3 扩展，“先 doctor 再 fix、逐项列出”产品原则）**：检查 20→23——新增 `runtime_duplicates`（PATH 重复运行时探测，排除 glue 自有目录，仅报告不移除）、`git_config`（桶仓库 dubious ownership + local autocrlf 未固定，桶作用域不碰用户全局配置）、`shell_integration`（$PROFILE glue 块：PATH 守卫 + chcp 65001 + UTF-8 stdin，标记幂等）；`--fix` 改为有序步骤表 `doctorFixSteps`（10 个动作带英文标题：Create data root / Repair PATH / Configure Git Bash（与 shim_path 共享一次 path_setup）/ Add main bucket / Configure UTF-8（控制台 CP + HKLM ACP·OEMCP=65001，无权限优雅降级）/ Configure PowerShell 7（`engine.Install` 安装 `main/pwsh`，仅 `--fix` 路径；`--offline` 时跳过并记录原因）/ Configure PowerShell execution policy（HKCU RemoteSigned）/ Configure Git（桶内 local autocrlf 固定到当前生效值 + 仅 dubious 时追加 safe.directory）/ Fix shell integration（带 `.glue.bak` 备份幂等写入）/ Detect duplicate runtimes（report-only））；人视图修复列表移至分组与 Result 之间逐项 `✓/✗ 标题`；`fixes[]` additive 增 `detail`；agent 永不自动安装；engine 导出 `ProbeGitBucket`/`GluePowerShellProfiles`/`GlueProfileBlock`/`GlueProfileHasBlock` 供 CLI 复用；Windows 侧 console/registry 与包安装全部可注入桩（单测不碰 HKLM/控制台，不联网不装包） | 全量 33 包绿×2（`-count=1`）；新增单测：重复探测（root 过滤+大小写折叠）、profile 隔离 home 往返（fail→写块→pass）、`ProbeGitBucket`（fresh repo unpinned→pin→healthy）、UTF-8 桩、包安装桩（包名/离线跳过/失败透传）、修复列表人视图（逐项在 Result 前列出）、`duplicatesFixDetail`；实机冒烟 temp 根 `--fix`（USERPROFILE/OneDrive 隔离，跳过 PATH/HKLM/包安装项）：`✓ Add main bucket`/`✓ Configure Git`/`✓ Fix shell integration`/`✓ Detect duplicate runtimes` 逐项列出，profile 块落盘隔离目录 |
| 2026-09-25 | **`glue env` 独立命令**：传统 7 项环境检查（data dir/git/7z/WiX dark/innounp/shim dir/GitHub）提为一级命令，发现性更好、JSON schema 独立；`glue doctor --env` 同步**移除**（刚加即删，不保留别名；`glue env` 是唯一入口）；启动 git/7z notes 对 `doctor` 与 `env` 同时抑制（`suppressesStartupToolNotes`）；顺带把 duplicate 子行标签改为 `git (4)`，避免与 toolchain 行的 `✓ git` 混淆 | 新增 env 命令注册/参数/notes 抑制/`--env` 已移除测试；`go build`/`go vet`/`go test ./cli/glue ./core/message` 绿 |
| 2026-09-25 | **`--fix` 诚实性 + Git Bash 语义修正**：`doctorFixStep` 增 `reportOnly`；`runtime_duplicates` 标记 report-only，`doctorFixableChecks` 不再把它计入 “safe fix(es)”，人视图改为 `— Detect duplicate runtimes — report only` + detail（JSON `fixes[]` 不变）；`shell_git_bash` 改为“仅当非 WSL `bash` 目录位于 shims 之前才告警”（新增 `gitBashShadowsShims`/`agentShimDir`，复用 `PathDirPrecedes`），因此 `path_setup` 真正能修复，hint 改为“把 shims 放到 Git Bash 之前”；新增文案 `agent.shell.git_bash_behind` | 新增 core `TestGitBashShadowsShims`、CLI `TestDoctorFixableChecks_excludesReportOnly`/`TestWriteDoctorFixLines_reportsReportOnly`；build/vet/test 绿 |
| 2026-09-25 | **`glue mcp` 首个可运行切片（Phase 2 开始）**：引入官方 SDK `github.com/modelcontextprotocol/go-sdk@v1.6.0`；新增 `glue mcp` stdio server（`mcp.StdioTransport`，stdout 只走 JSON-RPC，日志走 stderr，engine 以非 verbose 模式创建）；注册只读 tools `glue_search`/`glue_list`/`glue_doctor`，输出同时进 `structuredContent` 与 JSON text content，payload 与 CLI `--json` 同源 schema；`doctor` 的 `MCPAvailable` 翻为 true（MCP 检查转 pass），MCP hint 改为实际可用的 `glue mcp`；`suppressesStartupToolNotes` 增加 `mcp`；SDK 从 `jsonschema` tag 推断输入 schema 并在 handler 前校验 | 新增 CLI 测试：命令注册、tools 列表（描述/inputSchema）、`glue_doctor`/`glue_list` in-memory 端到端；`go vet`/`go build`/`go test ./cli/glue ./core/message` 绿。破坏性 tools（install/uninstall/update/bucket add）+ confirm token + policy gate 留作下一增量 |
| 2026-09-25 | **MCP 只读 tools 补齐（§4.2 read 全覆盖）**：新增 `glue_info`/`glue_depends`/`glue_path_check`/`glue_bucket_list`，read tools 3→7；`newGlueMCPServer(eng, root)` 携带 data root 供 PATH/bucket 检查；`glue_depends` 复用 CLI `jsonDependsPlan`/`jsonDependsResult`，`glue_info` 复用 `InstalledPackageDetail`，`glue_path_check` 复用 `shim.Manager` + `StoreAliasShadowsShims`，`glue_bucket_list` 只读 registry（不触发 git bootstrap，保持 side-effect free）；缺包 `glue_info` 以 tool error 返回 | 新增 in-memory 测试：7 tools 列表、`glue_path_check`/`glue_bucket_list` payload、`glue_info` 缺包 IsError；`go vet`/`go build`/`go test ./cli/glue -run TestMCP` 绿 |
| 2026-09-25 | **MCP 写操作 + confirm token + policy gate（§4.6.2 第一段）**：core config 增 `agent` 段（`auto_yes` + `policy.mode/deny/protected`，`ReadAgent` 规范化、默认 `confirm`）；MCP 新增 `glue_install`/`glue_uninstall` + `glue_confirm`：默认不执行，返回 `{status:"pending", confirm_token, expires_in:300, action, package}`；token 一次性、5 分钟 TTL、进程内存、绑定操作摘要；`agent.auto_yes` 或 `mode=auto` 跳过确认，但 deny/protected 始终硬拦截（confirm 执行时二次校验）；错误以 `{code,message,hint}` JSON 文本返回（`denied_by_policy`/`invalid_confirm_token`）；CLI 人类路径不受影响 | 新增 core policy 单测 + MCP 测试：policy 决策、install pending、confirm 执行并单次消费、deny/protected、auto_yes 执行；build/vet/test 绿 |
| 2026-09-25 | **Phase 2 收尾：审计 + 剩余写 tools + hold 闸门**：`activity_log` 增 `source`/`actor`（含老库 `ALTER TABLE` 迁移），`Engine.RecordAudit` 双写 SQLite + `<root>/logs/audit.jsonl`（append-only，`glue cache clear` 不伤 JSONL），engine 的 install/uninstall/upgrade/doctor/bucket/version 操作全部走它；MCP 写 tools 补齐 `glue_update`/`glue_bucket_add`/`glue_bucket_update`（与 install/uninstall 共用 confirm token + policy），并把 `confirm_required → granted/denied`、`source=mcp`、clientInfo actor 写入审计；`glue hold` 扩展为 MCP uninstall 第二道闸（返回 `denied_by_policy` + “held”）；新增 `glue audit list`（`--json`/`--source`/`--package`/`--since`/`--limit`） | 新增 core `TestRecordAudit_sqliteAndJSONL` + CLI `TestAuditList_jsonSourceFilter` + MCP audit/hold/new-tools 测试；build/vet/test 绿 |
| 2026-09-25 | **审计可靠性 + 通用 call logger + config agent keys**：所有 MCP tool 经 `mcpWrapTool` 统一记录 stderr 单行摘要，只读 tool 额外写 `tool_call` 审计行；`audit.jsonl` 改为 **hash chain**（每行 `prev`/`hash`，目录 0700/文件 0600），新增 `glue audit verify` 重算链并在篡改时 exit 1；审计写失败改为 stderr warning（`recordAuditWarn` + MCP 决策警告）；`glue config get/set/unset/list` 支持 `agent.auto_yes` / `agent.policy.mode|deny|protected`（`config.WriteAgent`）；`.gitignore` 跟踪整个 `docs/`，新增 `docs/mcp.md` | 新增 core `TestVerifyAuditLog_detectsTamper`、CLI `TestAuditVerify_json`/`TestConfigAgentKeys_roundTrip`、MCP `TestMCPReadToolCallAudited`；build/vet/test 绿 |
| 2026-09-25 | **审计轮转/保留 + JSONL 查询 + 执行级测试**：`audit.jsonl` 8 MiB 轮转为 `audit-<UTC>.jsonl`、保留最近 5 段，hash chain 跨段连续；`glue audit verify` 跨全部段校验；`glue audit list --jsonl` 直接读 JSONL（含轮转段，`format` 字段区分 sqlite/jsonl）；补齐执行级测试：本地 `file://` git bucket 的 `bucket add/update` E2E、空根 `glue_update` 执行路径 | 新增 core `TestAuditRotation_chainAcrossSegments`、CLI `TestAuditList_jsonlFlag`/`TestExecuteMCPUpdate_noUpdates`/`TestExecuteMCPBucketAddLocalRepo`；build/vet/test 绿 |
| 2026-09-25 | **代码健康：MCP 写路径拆分 + 审计轮转阈值可配置 + 剪枝锚点**：`cli/glue/mcp_write.go`（522 行）拆为 `mcp_policy.go`（策略/审计辅助）、`mcp_write.go`（tool 注册 + 调用）与 `mcp_exec.go`（执行函数），`mcp_confirm_store.go` → `mcp_confirm.go`；core config 增 `audit` 段（`max_bytes`/`keep_segments`，`ReadAudit`/`WriteAudit`/`NormalizeAuditSettings`；默认 8 MiB / 5 段，负值分别为“禁用轮转”“保留全部段”，段数上限 100），engine 按 config 轮转（按 config.json modtime+size 缓存，避免每次追加都读配置）；修复**剪枝后 `glue audit verify` 会误报断裂**的缺陷：剪枝前把被删段末条 hash 写入 `<root>/logs/audit.anchor`（0600、原子 rename），verify 从锚点起链，JSON 增 additive 字段 `anchor`（人视图打印短 hash），手工删除段仍能检出；`glue config get/set/unset/list` 支持 `audit.max_bytes`/`audit.keep_segments`（非整数 → `invalid_argument`，>100 段拒绝） | 新增/改写 core `TestReadAuditDefaultsAndNormalize`、`TestWriteAudit_preservesOtherKeys`、`TestAuditRotation_pruneKeepsVerifyGreen`、`TestAuditRotation_disabledByConfig`、`TestAuditRotation_keepAllSegments`、CLI `TestConfigAuditKeys_roundTrip`/`TestConfigAuditKeys_rejectInvalid`；`go build`/`go vet`/`go test ./core/config ./core/engine ./cli/glue` 绿 |
| 2026-09-25 | **多进程锁 + 持久化 confirm token + JSON schema 文档**：`audit.jsonl` append 走 `LockFileEx`（非 Windows 无操作桩），并发写入串行且链完整；`lastAuditHash` 改为只读文件尾部（O(1)），并在 active 为空时回退到最新轮转段以保持跨段连续；confirm token 落盘 `<root>/logs/pending/<token>.json`（0600，原子 rename 消费，过期清理），服务重启/另一进程仍可 confirm；新增 `docs/json-schema.md` 固化各命令 JSON schema | 新增 core `TestAuditConcurrentAppends_chainIntact`、CLI `TestMCPConfirmSurvivesRestart`/`TestMCPConsumeConfirm_expired`；build/vet/test 绿 |

**Phase 1 待办遗留**：
- search/list/info 的 JSON Schema 文档固化（§3.3 P1，纯文档）——需覆盖 `glue doctor` 默认视图报告 schema 与 `glue env` 传统 schema
- `path setup --json` 已实现（`bin_dir`/`changed`），因写用户注册表未做自动化测试，待人工实机验证
- `--yes` 随策略闸门（§4.6.2）在 Phase 2 落地
- **hold 语义扩展**（§4.6.2 标注"Phase 1 落地"，此前漏记）：随策略闸门在 Phase 2 一并补
- `go.work.sum` 为 workspace 生成物，下次提交一并纳入
- **docs/ 入库决策**：根 `.gitignore` 的 `/docs/` 已收窄为跟踪整个 `docs/`；`docs/mcp.md`、`docs/json-schema.md` 均已提交。

