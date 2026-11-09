# Agent Engine 技术设计与实施规范

> 项目定位: 基于 tRPC-Agent-Go 的可组合 Go Agent SDK
>
> 文档用途: 直接提供给 Cursor 作为独立项目的完整需求、架构、实施和验收依据
>
> 基准日期: 2026-11-09
>
> 语言: Go; 框架: `trpc.group/trpc-go/trpc-agent-go`; 参考基线: `v1.12.0`

## 1. 项目目标

开发一个**独立仓库、单 Go Module、多个扁平 Package** 的 Agent Engine SDK。SDK 提供构建通用 Agent 和本地 Coding Agent 所需的核心能力, 面向未来独立维护的 TUI、Desktop、Web、IDE、自动化服务和其他 Go 应用。

项目应能够被第三方项目以常规 Go Module 方式引用, 例如:

```go
import (
    "github.com/<owner>/agent-engine/agent"
    "github.com/<owner>/agent-engine/plugin"
    "github.com/<owner>/agent-engine/skill"
    "github.com/<owner>/agent-engine/mcp"
    "github.com/<owner>/agent-engine/runtime/trpc"
)
```

这里的 `<owner>` 仅为示例占位符。Cursor 在初始化时根据实际 Git 仓库地址设置 `module` 路径; 没有地址时使用可本地构建的暂定路径, 并集中配置, 不在代码各处散落占位符。

**最重要的交付结果**:

1. 可作为 Go Library 引用, 无 UI 和常驻服务依赖
2. 直接复用 tRPC-Agent-Go 的 Agent、Runner、Graph、Session、Model、Tool、MCP、Skill、Plugin、CodeExecutor 等现有能力
3. SDK 原生管理 Plugin、MCP、Skill、Hooks、Rules、Memory 等扩展域; 不推给 TUI 或 Desktop
4. 各 Package 可独立使用, 不强迫调用一个庞大的 `engine.New()` 才能使用 Skill、Plugin 或 Memory
5. 通过 `runtime/trpc` 将 SDK 配置和扩展能力装配到 tRPC; 框架类型不泄漏到稳定的公共领域接口
6. 能够对真实项目执行 Coding 任务, 返回流式事件, 处理权限, 保存和恢复 Session
7. Windows 的受管原生沙箱缺口必须真实标注; 未实现时拒绝冒充已隔离执行
8. 未来 TUI / Desktop / CLI 可各建独立仓库, 通过 SDK 引用, 不需要复制 Engine

## 2. 基本原则与非目标

### 2.1 基本原则

- **复用优先**: tRPC 有现成实现, 直接引用; 不造平行的 Loop、Graph Scheduler、Session DB、MCP Client 或 Skill 解释器
- **小接口**: 只围绕真实跨产品需求定义少量稳定 API, 不做与框架一一对应的影子接口层
- **扁平独立**: 一个仓库、一个根 `go.mod`, 每个一级能力使用自己的 Package
- **按需组合**: 不引入某个功能, 就不必初始化该功能
- **数据可迁移**: 外部插件、Skills、Memory、用户配置不以 tRPC 内部 Go 类型作为持久化格式
- **执行安全**: 非可信命令必须经过统一 Workspace / Permission / Sandbox 决策; 不能悄然降级
- **长期演化**: 允许更换底层 Runtime, 但不承诺未实现的跨框架 Session/Checkpoint 恢复
- **务实交付**: 每个开发阶段都要有真实可运行、可调用、可验证的功能, 不接受纯接口和空壳

### 2.2 明确不做

- 本仓库不开发完整 TUI、Desktop 或 Web 产品
- 不建立多个 Go Module 或 `go.work` 作为默认架构
- 不建立微服务、服务发现、Kubernetes、消息队列等基础设施
- 不自行实现独立 LLM 推理引擎、Tokenizer、Graph Scheduler、ORM 或通用向量库
- 不把 tRPC Agent Loop 全部再包装一遍或照抄源码
- 不提供不受信任 Go 二进制插件的进程内动态热加载
- 不在主任务的每轮 Model Call 中执行自动总结 / Dream
- 不承诺完全兼容任何第三方插件平台的所有扩展语义; 逐项列明兼容范围
- 不因未来支持多端而提前强制实现 HTTP/RPC Server

## 3. 框架基线与实际能力

框架依赖固定在经过构建验证的具体发布版本, 初始候选使用 `trpc-agent-go v1.12.0`, 不使用 `@main`、`@latest` 作为不可控的自动依赖升级策略。

| 能力 | 直接复用的 tRPC 模块 | SDK 自己需要补充的部分 |
| --- | --- | --- |
| Agent Loop / Model | `agent/llmagent`, `model/*` | 模型配置与能力装配 |
| 运行控制 / 事件 | `runner`, `event` | 稳定的调用/事件 DTO 转换; 需要时补持久任务索引 |
| Graph / Team | `graph`, `agent/graphagent`, `team` | 常用执行模式的组合配置; 不重写调度 |
| Session | `session`, `session/sqlite` | 路径选择、生命周期与数据迁移策略 |
| Plugin Hooks | `plugin`, `agent/extension` | 产品级插件 Manifest、安装、启停、组合装配 |
| MCP | `tool/mcp` | 多来源 MCP 配置、服务清单、凭据引用、启停 |
| Skills | `skill`, `llmagent.WithSkills` | 多目录发现、插件安装联动、配置与权限 |
| Tools / Code | `tool/*`, `codeexecutor` | 工作区策略、Coding 工具组合、Git/Diff 展示数据 |
| OS Sandbox | `codeexecutor/sandbox` | 平台探测、策略、Windows 缺口和未来扩展 |
| Long-term Memory | `memory`, `session.Ingestor` | 独立 Markdown 知识文件与显式 Recall 策略 |
| Evolution | `evolution` | 独立启动、审批和输出位置策略; 默认关闭 |
| Frontend Transport | `server/agui` 可选 | 未来独立产品仓库接入, SDK 不强制依赖 |

**必须核对的关键事实**:

- tRPC 的公开 `agent.Agent` 接口可扩展, Runner 的 `ManagedRunner` 可取消运行, `SteerableRunner` 可在工具调用边界安全插入用户消息
- tRPC 提供 Runner 级 `plugin.Plugin` 和 Agent 级 `agent/extension.Extension`, 分别承担不同作用域
- `llmagent.WithSkills` 在某些配置下会自动启用本地 CodeExecutor; 安全模式必须显式指定 CodeExecutor 或禁用执行工具, 禁止无意回落到宿主执行
- `codeexecutor/sandbox` 在 Linux 使用 Bubblewrap, macOS 使用 Seatbelt / `sandbox-exec`; Windows 当前没有受管 OS 原生 Backend
- tRPC 的 `session/sqlite` 和 `server/agui` 是独立 Go 子模块。**这是上游项目的模块边界**, 不要求本项目也变成 Multi-Module; 依赖需按各模块正式发布的版本管理
- tRPC 的代码执行响应处理器默认可能执行 Markdown 代码块。若 Agent 只通过明确的工具调用执行命令, 必须设置 `llmagent.WithEnableCodeExecutionResponseProcessor(false)`

## 4. 整体架构

```text
独立产品仓库 (未来)
  agent-tui         agent-desktop       agent-web / agent-cli
       \                 |                    /
        \________________|___________________/
                         |
                      Go imports
                         |
                    agent-engine
  +----------------------------------------------------+
  | 公共 SDK                                            |
  |                                                    |
  | agent   plugin   mcp   skill   hooks   rules        |
  | memory  coding   sandbox   config                   |
  |                                                    |
  | runtime/trpc  (组合与适配)                          |
  +-------------------------+--------------------------+
                            |
                   tRPC-Agent-Go
  +----------------------------------------------------+
  | Agent / Runner / Graph / Team / Model / Tool        |
  | Session / MCP / Skill / Plugin / CodeExecutor      |
  | Memory / Evolution / AG-UI(可选)                   |
  +----------------------------------------------------+
                            |
      SQLite / 文件系统 / Linux & macOS OS Sandbox
                 Windows: 显式能力缺失
```

**运行方式**: 默认 In-Process Go Library。使用者创建 Session, 发起 Run, 消费 Event, 处理 Approval。后续独立应用可用相同 SDK 实现终端、桌面或网络交互。

**框架隔离的实际边界**:

- `agent` 等公共 DTO 不 import `trpc.group/...`
- `runtime/trpc` 是 tRPC 与 SDK 公共类型转换、组件装配的主要位置
- 不能要求所有功能必须经过 `runtime/trpc`: 单独使用 `skill` 解析、`plugin` 安装或 `memory` 读取时, 不应该创建 Runner
- 可直接复用独立且稳定的 Go 标准接口 (`context.Context`, `io.Reader`, `fs.FS` 等), 避免无意义抽象
- 隐藏底层实现不等于重新实现它; 例如 Session API 委托 tRPC Service, 而不是复制 Session 表与读写事务

## 5. 仓库与 Package 结构

以下为**目标结构**, 按需求渐进创建, 不提前堆放空目录:

```text
agent-engine/
├── go.mod                        # 根目录只有一个主 go.mod
├── README.md
├── AGENTS.md                     # 项目稳定工程约束
├── config/                       # 配置模型、合并与校验
├── agent/                        # 公共 Run / Event / Session / Task API
├── plugin/                       # Manifest、来源、安装、状态、版本
├── mcp/                          # MCP 服务器配置与生命周期
├── skill/                        # SKILL.md 发现与管理
├── hooks/                        # 外部 Hook 声明、策略与执行
├── rules/                        # 项目/全局规则与加载顺序
├── memory/                       # Markdown 经验文件与按需 Recall
├── coding/                       # Workspace / 文件 / Git / Diff 辅助
├── sandbox/                      # 平台无关能力与策略接口
├── runtime/
│   └── trpc/                     # 使用 tRPC 组装 Agent、Runner 和 Executor
├── internal/                     # 非稳定实现细节
├── examples/
│   ├── basic/                    # 最小运行示例 (非产品)
│   ├── coding/                   # 执行真实 Coding 任务
│   └── extensions/               # Plugin + MCP + Skill 组合示例
├── testdata/
├── .github/workflows/           # CI: Linux / macOS / Windows
└── docs/
    ├── architecture.md
    ├── api.md
    ├── plugins.md
    └── sandbox.md
```

依赖原则:

```text
config/rules/skill/plugin/memory  -> 标准库优先
             ↓ (必要时引用小领域接口)
         agent / sandbox / coding
             ↑
         runtime/trpc  -> tRPC-Agent-Go
             ↑
    examples / 外部产品
```

不要把这张图理解为强制每个 Package 依赖 `agent`。能不依赖就不依赖。`plugin` 不应直接依赖 `mcp`、`skill`、`hooks` 的具体实现; 它输出「已安装包 + 组件清单」, 由装配模块装载对应能力。

### 5.1 公开包命名和模块边界

- `agent`: 最小 Task/Run/Session/Event 协议; 不能成为一切类型的垃圾桶
- `plugin`: 软件包生命周期, 不是 tRPC Callback 的重复实现
- `mcp`: 配置与连接管理, 不自写 MCP JSON-RPC 协议栈
- `skill`: SKILL.md 资源管理, 不重写 tRPC 的 Skill Tools
- `hooks`: Hook 的产品侧规则与脚本执行; SDK 自行处理外部 Hook, 但运行时回调交给 tRPC Plugin/Extension
- `memory`: 独立知识资产的文件语义, 不替代 Session 和 Graph Checkpoint
- `coding`: Workspace、Patch/Diff、Git 状态查询等 Coding 场景扩展
- `sandbox`: 统一操作系统隔离能力和策略, 由 `runtime/trpc` 映射现有 CodeExecutor

## 6. 对外 API 契约

以下是**需要实现的 SDK 自有 API 设计草案**, 不是声称 tRPC 已存在的接口签名。Cursor 可以根据 Go 实现调整命名和类型, 但不得改变语义边界。

```go
// package agent

type RunRequest struct {
    UserID    string
    SessionID string
    Workspace string
    Message   string
    Mode      Mode
}

type Mode string
const (
    ModeDefault Mode = "default"
    ModePlan    Mode = "plan"
    ModeGoal    Mode = "goal"
)

type Event struct {
    RunID     string
    SessionID string
    Kind      EventKind
    Sequence  uint64
    Time      time.Time
    Payload   json.RawMessage
}

type RunHandle interface {
    ID() string
    Events() <-chan Event
    Cancel() error
}

type Runtime interface {
    Start(ctx context.Context, req RunRequest) (RunHandle, error)
    Close() error
}
```

设计说明:

1. `Start` 返回只读的运行句柄; 模型和工具的执行保持异步, 不把 UI 阻塞在一次同步 RPC 中
2. `ctx` 生命周期属于本次调用。上下文取消应中止当前运行; 想在关闭客户端后继续执行, 由独立 Task Host 负责生命周期, 不允许偷偷 `context.Background()` 丢弃调用者约束
3. `EventKind` 只包含跨产品稳定的语义: 文本增量、工具开始/结束、权限请求、任务终态、错误、Artifact 更新等; 对不上号的 tRPC 特殊事件可以有 `Raw`/扩展字段
4. `Sequence` 是本地当前运行的单调序号; **不因有序号就宣称事件已经持久化或可断线重放**
5. 只有需要多客户端连接或持久后台任务时, 才添加 `TaskStore` / `Subscribe(runID, afterSeq)`; 不要提前开发消息总线
6. 显式定义终态: `completed` / `failed` / `cancelled`; 最后一个终态事件发出后关闭事件 Channel, 错误不可被吞掉
7. 同一 Session 并发写入默认串行化或显式拒绝, 避免模型历史和工具结果交错
8. `SessionID` 与 `RunID` 分离; 同一 Session 可以有多次 Run, 子 Agent 可以有独立分支/会话

### 6.1 组件装配示例

调用风格应接近:

```go
cfg, err := config.Load(configFile)
if err != nil { return err }

rt, err := trpcruntime.New(trpcruntime.Options{
    Config: cfg,
    // 按需注入 Plugin、Skill、MCP、Sandbox 等组件
})
if err != nil { return err }
defer rt.Close()

run, err := rt.Start(ctx, agent.RunRequest{
    UserID: "local-user", SessionID: "demo",
    Workspace: "/path/to/repo", Message: "修复失败测试",
})
if err != nil { return err }
for evt := range run.Events() {
    consume(evt)
}
```

以上为目标 API 草图, 由 Cursor 写出真实可编译实现及示例。`trpcruntime` 是调用方给 `runtime/trpc` 设置的 import alias。

### 6.2 模型层

- 初期使用 tRPC `model/openai` 支持 OpenAI-compatible API, 同时允许 Gemini/Anthropic 等 tRPC 原生 Provider
- 模型配置暴露 `provider`, `model`, `base_url`, `credential_ref`, `timeout`, `generation` 等基础字段
- API Key 只从环境变量、系统密钥存储或注入 Provider 读取, 不写入公开配置和 Event Journal
- Tool Calling、流式输出和重试优先使用 tRPC 原生能力; 不再包装一套通用模型 SDK

## 7. tRPC Runtime Adapter

`runtime/trpc` 只负责以下事情:

1. 校验 SDK 配置与能力组合
2. 创建/持有 tRPC Model、Agent、Runner、Session Service、Toolset、CodeExecutor
3. 将启用的 Plugin/Skill/MCP/Rules/Hooks 装配到对应 tRPC API
4. 调用 `runner.Run(ctx, userID, sessionID, message, ...)`, 转换 `event.Event` 到 SDK `agent.Event`
5. 使用 `ManagedRunner.Cancel` 实现取消, 对支持的 Runner 使用 `SteerableRunner.EnqueueUserMessage`
6. 负责它创建的资源的 `Close` 和错误回收, 明确谁拥有 SQLite DB、MCP Client、Evolution Service
7. 映射 tRPC 内部 `AgentID`、`RequestID`、`SessionKey` 到 SDK 的稳定标识

明确禁止:

- 不重写原生 Runner 或 Flow 逻辑
- 不把 `*event.Event`、`*runner.Runner` 等类型泄漏到 `agent`、`plugin`、`skill` 的公共 API
- 不实现与 tRPC Graph 对应的第二套调度器
- 不把所有配置、所有事件都复制存两份

**公开代码组织约束**: SDK 的 `runtime/trpc` 可以有内部子目录帮助组织转换与构造; 外部使用者应能通过 `runtime/trpc` 直接完成完整装配, 不要求引用上游 `internal` 包。

## 8. Session、事件与存储

### 8.1 Session

- 默认本地持久化选择 SQLite, 直接复用 tRPC 的 `session/sqlite`
- 提供应用级路径配置、Session 创建/查找/列表/删除、保留策略
- 使用 tRPC `session.Service` 已有事务、事件、摘要与状态实现; 不新增同构数据表
- 默认关闭不必要的异步持久化优化, 除非能证明退出与崩溃恢复语义; 初始化按上游公开选项
- SQLite 驱动涉及 `go-sqlite3` / CGO。安装文档明确开发和交叉编译约束; 不声称 CGO-free
- `session/sqlite` 是上游独立 module: 本项目可以只有一个 `go.mod`, 但必须同时解析它的模块版本, 不要将上游仓库源码里的相对 `replace` 复制到发布的 SDK 中

### 8.2 SDK 自有数据

只持久化 tRPC 不负责的 SDK 业务数据:

- 插件安装记录、来源与内容指纹
- MCP Server 启停和参数引用
- 用户配置 / 项目配置
- 可选持久后台 Task 的生命周期索引
- 独立 Memory 文件与 Evolution 审批结果

建议 SDK 自有元数据先使用 JSON/SQLite 中的最小一种, 不追求统一 ORM。不要直接修改 tRPC 的 Session 表。

### 8.3 Event Journal

原始 Execution Event 优先留在 tRPC Session 中; 如需调试或跨 Runtime 导出, 允许显式开启 NDJSON Journal。

- 默认不把超大 Tool Output 完整复制到 Journal
- 敏感字段必须脱敏或通过 Blob 引用
- Journal 不等于 Memory, 也不等于可恢复的 Graph Checkpoint
- 若以后需要可重连事件流, 必须设计稳定持久事件序号和去重, 不能直接把内存 Channel 假装成可恢复总线

## 9. Coding 能力

### 9.1 必需能力

第一批 SDK Coding 场景应支持:

- 工作区解析与边界校验
- 读取/列出/搜索文件
- 精确修改文件或应用 Patch, 保留编码与现有内容
- 执行命令、编译、测试, 返回 stdout/stderr、退出码、超时
- Git status、diff、变更摘要, 不自动 `git add`、commit、push、reset、clean
- 文件写入前后变更摘要; 避免覆盖其他已有修改
- 能执行多轮工具调用并最终报告验证结果

优先组合 tRPC 现有 `tool/file`、`tool/workspaceexec`、`tool/hostexec`、`codeexecutor` 等。仅在缺失的具体操作上补少量工具; 不创建第二套通用 Tool Calling Framework。

### 9.2 Workspace 与权限

定义可解释的执行策略:

| 策略 | 含义 |
| --- | --- |
| `read_only` | 仅读取工作区, 不允许写入或启动产生副作用的命令 |
| `workspace_write` | 只允许在指定工作区内写入; 命令按受管 Sandbox 规则执行 |
| `danger_full_access` | 显式申请的高风险模式, 无系统级隔离保证 |

权限策略不仅在 Tool 名字上判断, 还必须在**最终实际执行路径**生效, 包括 Shell、Hook、Skill 脚本和第三方工具调用。

审批由 Engine 定义交互 DTO, 具体 UI 可以是终端、TUI、Desktop 或 API; SDK 不使用 `fmt.Scanln` 直接等待用户输入。任务暂停、审批、超时、拒绝和继续的实际接线优先采用 tRPC 原生可用的机制。

## 10. 跨平台 OS Sandbox

这是本项目的一条**硬性能力边界**, 需要明确写入 README 和执行路径。

### 10.1 当前支持事实

| 系统 | 受管原生 OS 隔离 | 实现与说明 |
| --- | --- | --- |
| Linux | 已有上游实现 | tRPC Bubblewrap, 依赖系统能力与外部可执行文件 |
| macOS | 已有上游实现 | tRPC `sandbox-exec` / Seatbelt, 需运行时预检 |
| Windows | **没有上游实现** | `backend_other.go` 会返回不支持; 本 SDK 不得伪装成已支持 |

SDK 的 `sandbox` 提供 `Capabilities` 查询, 至少包含平台、受管隔离是否可用、文件系统隔离、网络隔离、PTY、交互式执行能力和不可用原因。实际能力由后端运行时预检得出, 不能只根据 `GOOS` 猜测。

### 10.2 默认行为

- Linux/macOS: 在 `workspace_write` 模式优先使用 tRPC 的受管 OS Sandbox, 预检失败则报告错误
- Windows: 允许读取、Session、Skill 解析、MCP 配置等非命令功能; 需要受管沙箱的命令**明确返回 `ErrSandboxUnavailable`**
- 不允许将 `localexec`、WSL2、Docker 或普通 Windows Job Object 自动标注成「Windows 原生受管沙箱」
- 只有用户明确选择 `danger_full_access` 或明确指定外部隔离 Backend, 才可改变执行策略; 结果中必须携带实际隔离级别
- 不把外部 Sandbox 的存在仅靠布尔配置宣称为可信, 它需要自己的隔离责任方和健康/能力检查

### 10.3 Windows 原生扩展点

预留 `sandbox.Backend` / `sandbox.Executor` 的**最小稳定接口**, 允许后续补 Windows 原生实现。候选路线为微软 MXC 或经过验证的 Windows 原生安全机制, 可能需要独立 Helper 进程/FFI。

Windows 原生 Backend 只有在真实 Windows 环境完成以下验证后, 才能正式标记支持:

1. 未授权读写工作区外路径受到拒绝
2. 敏感文件访问和环境变量继承受到策略控制
3. 网络访问满足显式策略
4. 子进程、子进程树与命令超时可终止
5. 权限拒绝与用户审批可正确传递
6. 预检失败时 fail closed, 不悄然退化成直接执行

这个能力作为独立阶段交付, 不要求为了实现它重写 Agent Runtime。没有 Windows CI/实体环境时, 只能提交「未验证」状态, 不得标注完成。

## 11. Engine 原生 Plugin System

**Plugin 是 SDK 的一级能力**, 与 MCP、Skill、Hooks、Memory 并列; 不是某个 UI 的专属功能, 也不等于 tRPC 的 Runner Plugin。

### 11.1 两层概念

- **产品/SDK Plugin**: 可安装、可更新、可启停的扩展包, 可以包含 Skills、MCP 配置、Hooks、Rules、Commands、Agent Templates
- **tRPC Runtime Plugin / Agent Extension**: 编译期提供的 Go 回调扩展, 负责 Runner / Agent 的生命周期拦截

产品 Plugin 是分发单元; 其 Skills/MCP/Hooks 由各自模块解释, 不一律转换成 `trpc.plugin.Plugin`。

### 11.2 插件目录

优先接受 Claude Code 风格资源型目录, 同时使用清晰的兼容能力声明:

```text
my-plugin/
├── .claude-plugin/
│   └── plugin.json
├── skills/
│   └── code-review/
│       └── SKILL.md
├── commands/
│   └── review.md
├── agents/
│   └── reviewer.md
├── rules/
│   └── style.md
├── hooks/
│   └── hooks.json
└── .mcp.json
```

支持基本 Manifest 字段: `name`, `version`, `description`, `author`, `license`, 以及可发现的组件位置。实际兼容 Claude 风格 Manifest 时要读取实际结构/官方示例, **不能假定所有第三方字段同义**。

### 11.3 安装与生命周期

`plugin` Package 应实现以下公开能力:

```text
ListSources / AddSource / RemoveSource
Discover / ListInstalled / Inspect
Install / Update / Uninstall
Enable / Disable
Validate / ResolveComponents
```

实施约束:

- 来源首先支持本地目录和 Git URL; 私有 Marketplace/登录体系不作为初期强制任务
- 安装到专用的 SDK 数据目录, 原子写入、隔离临时文件; 验证文件路径无 `../`、不可信符号链接逃逸和冲突覆盖
- 安装记录保存来源、提交/版本、内容指纹、启用状态和组件清单
- Git 资源安装默认固定具体 revision 或记录 resolved commit, 不在每次启动时隐式拉取新代码
- 启用/禁用影响**之后新建的 Agent/Run 配置快照**; 当前正在执行的 Run 不强制被热替换
- Hook/脚本代码属于可执行权限, 安装本身不代表立即授权执行
- 第三方纯 Go 回调插件按编译期注册, 不实现动态加载 `.so`/`.dll` 任意代码
- 插件列表可以由任何上层产品调用 SDK API 获取, 不需要 SDK 自己附带管理 UI

### 11.4 组件装配与覆盖规则

装配时按确定性顺序处理:

```text
全局配置 → 用户配置 → 工作区配置 → 已启用的安装插件
     ↓
组件清单去重 / 冲突检测 / 权限合并
     ↓
Skills  → tRPC Skill Repository
MCP     → tRPC MCP Toolset
Hooks   → SDK Hook Runner + tRPC Lifecycle callbacks
Rules   → Agent instruction / prompt composition
Agents  → tRPC Agent / Subagent constructors
Commands→ 解析为可复用指令模板, 由调用方决定何时触发
```

同名冲突默认报错或采用文档明确的优先级, 不依赖 Map 随机遍历次序; 冲突优先级应可在配置层显式指定。

## 12. MCP、Skill、Hooks、Rules

### 12.1 MCP Package

- 支持配置多个 Server, 通过 tRPC `tool/mcp` 接入 `stdio`、SSE/Streamable HTTP 等上游支持的传输
- 配置来源: 用户、项目、插件; 合并/覆盖规则必须可预测
- 提供 `List`, `Add`, `Remove`, `Enable`, `Disable`, `Inspect` 和连接状态
- Header、Token 以凭据引用存在, 不保存明文到插件 Manifest 和公开事件中
- 对 `stdio` Server, 启动命令属于外部可执行程序, 必须执行权限检查; 不默认信任所有插件自带命令
- 不重写 MCP SDK 的连接、工具发现、协议消息处理和重连逻辑

### 12.2 Skill Package

- 支持全局、项目、插件目录下的 `SKILL.md`
- 使用 tRPC `skill.NewFSRepository` 和现有 Skill 工具; 安装/卸载后按需要 `Refresh()`
- 维持 progressive disclosure: 初始只提供摘要, 正文和资料按需读取
- 默认不向模型完整注入全部 Skills
- Skill 资源包与 Skill 执行权限分离; 文档文件可读, 脚本执行遵守 CodeExecutor/Sandbox 策略
- **强制显式配置 CodeExecutor**, 禁止 `WithSkills` 自动回退到未隔离本地 Executor; 对不执行脚本的产品可选择 KnowledgeOnly
- 明确禁用自动执行响应 Markdown 代码块, 统一通过 Tool Call 启动受控执行

### 12.3 Hooks Package

- 支持 `BeforeRun`, `AfterRun`, `BeforeModel`, `AfterModel`, `BeforeTool`, `AfterTool`, `OnEvent`, `OnError` 等语义中具有对应实现的位置
- 对应 Runtime Callback 直接使用 tRPC 原生 `plugin.Plugin` 或 `agent/extension.Extension`
- Shell Hook 是独立资源型机制, 通过统一权限与受控执行器启动, 不直接调用裸 `os/exec`
- Hook 需要明确超时、输入/输出 JSON Schema、退出码语义、阻断策略、并发限制和去重
- 对上游不支持的 Hook 事件明确标记 unsupported, 不创造假回调

### 12.4 Rules Package

- 规则来源: 全局规则、工作区规则、插件规则, 按确定顺序加载
- 仅处理指令文本与路径匹配; 不重新设计模型内部上下文压缩
- 能输出当前 Run 实际生效的规则摘要, 方便调试

## 13. Context、Memory 与 Evolution

### 13.1 Context

- 使用 tRPC 已有 Session Summary、Token Tailor、Context Compaction、Tool Transcript 策略
- 按模型能力和 Token Budget 配置参数, 默认避免无意义的重复摘要
- 显式控制 Skill、Memory、历史会话内容进入 Prompt 的时机
- Context 是**当前调用的模型输入视图**, 不是长期经验的权威存储

### 13.2 Memory

长期知识采用文件优先:

```text
~/.agent-engine/
├── memory/
│   ├── MEMORY.md          # 索引
│   ├── practices/
│   ├── projects/
│   └── references/
└── ...
```

- Memory 的持久本体是 Markdown, 支持直接读写、版本审查和跨产品复用
- 初期仅实现读取、更新、路径组织和按需 Recall; 检索可从关键字/SQLite FTS 起步, 不强制向量数据库
- 索引可以重建, 原始 Markdown 是权威数据
- 默认不自动把全部 Memory 注入当前 Session; 未经明确启用不调用模型做记忆回写
- Session、Memory、Graph Checkpoint 各自存储各自的责任数据, 不相互混淆

### 13.3 Evolution / Dream

- 直接复用 tRPC `evolution.NewService` 和现有 Review / Skill 发布能力
- **默认关闭**, 由单独配置启用; 不在主 Agent Loop 中同步执行
- 学习数据来自已完成会话或显式投递的 Journal/任务记录
- 提炼出的 Skill 先进入候选区, 通过现有安全与质量门禁或用户审批后发布
- `evolution.Service` 的生命周期独立管理, Runner 不自动为调用方关闭该服务
- 禁止无条件将候选产物自动覆盖用户维护的 Skills 或 Memory
- 「自动生成可执行 Graph」不属于 tRPC Evolution 已验证的默认能力; 未来可作为独立研究性扩展, 不列为当前验收目标

## 14. Agent 执行模式与高级能力

借鉴成熟 Coding Agent 的使用方式, 但在 Runtime 上复用已有组件:

| 能力 | 实现策略 | 交付优先级 |
| --- | --- | --- |
| Default | tRPC LLMAgent / Runner | 必需 |
| Plan | 指令策略 + 写入前审批/计划产物; 不重写 Loop | 首批 |
| Goal | 复用 `agent/extension/goal` 或 Runner Ralph Loop, 加预算与完成验证 | 首批后 |
| Subagent | tRPC AgentTool / SubAgents / Team | 后续 |
| Graph | tRPC `graph` / graphagent, 支持节点配置与状态恢复 | 后续 |
| Side Query | 独立 Session/Context 快照, 不污染主 Session | 后续 |
| Background Task | 独立任务生命周期持有者, 可选 SQLite Task 索引 | 后续 |
| Artifact Review | tRPC Artifact + SDK 批注/审批 DTO | 后续 |
| Remote Control | 单独产品/Daemon 仓库通过 RPC/SSE/AG-UI 接入 | 未来产品 |

**执行策略原则**: 不是每个请求都必须经过复杂 Graph、多个 Agent 或自动规划。默认单 Agent; 只有显式配置或有明确价值时启用其他方式。

任务完成策略应优先利用 tRPC 的 Goal Extension 或 Ralph Loop, 并始终设置预算、超时与终止条件。不要在外面叠加无限递归 Agent Loop。

## 15. 产品与进程边界

本仓库提供 In-Process SDK, **不**包含某个产品的交互代码。未来各产品独立仓库:

```text
agent-engine/         # SDK, 无 UI
agent-tui/            # Bubble Tea, 独立 go.mod
agent-desktop/        # Wails + Web UI, 独立 go.mod
agent-cli/            # 可选 Headless 产品, 独立 go.mod
agentd/               # 可选驻留服务, 独立 go.mod
```

当前 SDK 必须满足:

- 所有用户输入通过参数或回调传入, 不在基础库直接读 `stdin`
- 所有运行信息通过事件/查询 API 获取, 不把 `fmt.Printf` 作为正式输出协议
- 权限审批通过异步请求对象与结果通道/回调, 不依赖 UI 组件
- 单次调用支持取消; Session 可复用
- API 的 DTO 可序列化, 以后能无损映射 HTTP、SSE、WebSocket 或 IPC
- 只有明确引入独立持久 Task Host 时, 才保证 UI/进程退出之后任务继续执行
- Runner 与插件装配可以按应用/会话配置构造; 不建立进程级不可控全局单例

## 16. 配置与目录约定

建议提供 `config.Load(...)` + 环境变量覆盖 + 明确校验, 优先 YAML 或 TOML 其中一种, 不同时设计多套配置 DSL。

以下只是产品无关的配置语义示例, 字段由具体实现文档固定:

```yaml
runtime:
  provider: trpc
model:
  provider: openai_compatible
  name: example-model
  base_url: ${MODEL_BASE_URL}
  api_key_env: MODEL_API_KEY
session:
  backend: sqlite
  path: ~/.agent-engine/session.db
workspace:
  root: .
  permission: workspace_write
sandbox:
  mode: managed
  network: restricted
skills:
  paths: [~/.agent-engine/skills, .agent/skills]
plugins:
  dirs: [~/.agent-engine/plugins]
  enabled: []
mcp:
  servers: []
memory:
  enabled: false
  root: ~/.agent-engine/memory
evolution:
  enabled: false
```

配置要求:

- 按用户、工作区两级配置合并, 输出有效配置用于排查问题
- 对未知字段返回可诊断错误或警告, 不默默吞掉关键配置
- 路径以平台正确的用户数据目录解析, 不强制在 Windows 使用 POSIX `~` 路径
- `MODEL_BASE_URL` 等变量须由实现明确展开规则; 不将示例 `${}` 原样当成必然支持的 YAML 语法
- Token 绝不打印到日志
- 改动 Plugin/Skill/MCP 配置时, 新 Run 使用新快照; 已有 Run 按旧快照完成或明确取消

## 17. 错误模型与可观测性

- 公共错误至少区分 `InvalidConfig`, `PermissionDenied`, `SandboxUnavailable`, `RunCancelled`, `ModelError`, `ToolError`, `SessionError`, `PluginError`
- Go 错误保持 `%w` 包装, `errors.Is` / `errors.As` 可判断常见类型
- Event 包含最小关联 ID: `RunID`, `SessionID`, 可选 `AgentID`, `ToolCallID`, TraceID
- 统计耗时、模型调用数、Tokens、工具调用数、失败数; 能直接复用 tRPC Telemetry 则不另起指标系统
- 对错误中可能包含的 API Key、环境变量、文件内容进行脱敏, 不把用户所有代码上传到外部遥测
- 日志和 Trace 功能默认可关闭, 不能成为基础 Library 运行的硬依赖

## 18. 测试与验证标准

**不能以能够编译、HTTP 200、单次 Prompt 成功作为完成标准。**

### 18.1 单元测试

- Config 优先级、校验与密钥脱敏
- Manifest 解析、路径穿越/软链接保护、安装回滚
- MCP/Skill/Hook 合并顺序和名称冲突
- `event.Event` 到公共事件 DTO 的转换及终态语义
- Runner 取消、并发 Session 限制、事件 Channel 关闭
- 文件 Patch、编码保持、Workspace 路径边界
- Memory 文件索引与引用关系
- 沙箱能力探测和 unsupported 行为

### 18.2 集成测试

- 使用真实 tRPC Runner 和可控 Mock Model 验证 Agent → Tool → Event
- SQLite 创建 Session、跨进程重启后继续对话
- 安装并启用一个包含 Skill、MCP、Hook 的本地测试插件
- 非可信 Hook/Skill 脚本不能绕过执行权限
- Linux/macOS 的受管沙箱在可运行 CI 环境下拒绝越界读写和网络行为
- Windows 无受管 Backend 时必须显式拒绝受管 Shell, 不能静默执行
- 运行过程中取消后没有遗留受控进程, Session 状态一致
- 必要时以独立测试项目通过 `go get` 引用 SDK, 确认不是只能在单仓库构建

### 18.3 端到端验收任务

准备真实但可重复的 Go Fixture 工程, 至少覆盖:

1. 定位一个失败测试、修改最少代码、重新执行 `go test ./...`, 最终报告与 Diff 一致
2. 发现已有未提交修改, 只编辑目标文件, 保留其他工作区内容
3. 工具请求受权限限制时能够生成审批, 拒绝后不执行
4. 新建会话 → 多轮对话 → 结束进程 → 再启动并恢复
5. 通过独立包 API 完成插件安装、Skill 发现、MCP 配置、取消 Run
6. 禁止执行的 Windows/沙箱能力明确报告 unavailable

对需要真实第三方 LLM Key 的测试, 提供可选环境变量驱动; CI 核心正确性应有无外部网络的 Mock Model 验证。依赖网络的测试注明真实环境和未运行项。

### 18.4 跨平台 CI

- 至少 Linux、macOS、Windows 运行静态检查、构建和可移植单元测试
- 对需要 Bubblewrap、Seatbelt 的集成测试按真实能力跳过或失败分类, 不把未运行写成通过
- SQLite 使用 CGO 时为 Windows 提供明确编译工具链要求; CI 必须真实构建 Windows 产物
- `go test ./...`、`go vet ./...`、`go build ./...` 应在适用的环境执行

## 19. 开发交付顺序

本项目从零开始, **不使用“版本 1/版本 2”或迁移历史叙述**。按照能力依赖顺序实施, 每一步都构建真实代码和测试。

### 阶段 A: SDK 可运行核心

完成内容:

- 初始化单 Module 仓库与扁平包规范
- 集成 tRPC Agent / Runner、OpenAI-compatible Model
- 统一 `RunRequest` / `RunHandle` / `Event` DTO
- SQLite Session, 多轮对话, 基础流式事件, 取消
- 最小 `examples/basic` 和无网络测试

完成条件: 外部 Go 项目能 import SDK, 执行模型/Tool Call, 正确输出事件, Session 可恢复。没有独立 UI 产品。

### 阶段 B: 本地 Coding 与安全执行

完成内容:

- 组合 tRPC 的文件与执行工具, 实现 Workspace、Diff/Git 能力
- 接入 Linux/macOS 受管 OS Sandbox, 统一权限策略
- Windows 平台探测及 fail-closed; 公开能力查询
- 受控执行、超时、取消、审批数据契约
- `examples/coding` 和真实 Go Fixture 验收

完成条件: 可在真实工作区完成 Coding 任务, 不覆盖无关修改; 受限命令不越权。

### 阶段 C: 原生扩展生态

完成内容:

- Plugin Manifest、来源、安装/更新/卸载、启停、组件发现
- MCP Server 配置、连接管理、权限检查
- SKILL.md 多目录 Repository 和按需加载
- Hooks / Rules / Commands / Agent Templates 的可支持子集
- 装配到 tRPC Runner Plugin、Agent Extension、MCP、Skill
- 本地组合插件端到端验收, 兼容性文档

完成条件: 外部产品只调用 SDK 就能管理并使用插件, 不需要自行实现插件 Loader。

### 阶段 D: 执行策略与独立知识

完成内容:

- Plan / Goal 模式; 复用现有 Goal Extension 或 Ralph Loop
- Subagent/Graph 的组合入口和官方执行示例
- Markdown Memory 的按需 Recall
- 可选 Evolution Worker, 候选 Skill 审核, 默认不启用
- 明确单次 Run 与长生命周期 Background Task 的区别

完成条件: 增强模式能明确按需启用; 关闭 Memory/Evolution 时不增加额外调用链或模型成本。

### 阶段 E: Windows 原生沙箱与多端扩展接口

完成内容:

- 保持 OS Sandbox 适配器独立, 不改变已有 SDK 领域 API
- 评估/接入真实 Windows 原生受管 Sandbox 后端, 优先考虑有维护来源的现成实现或 Helper; 未实现则保留明确 unsupported
- 完成 Windows 原生隔离、进程树和权限策略的实际测试后, 才把支持状态改为可用
- 为 TUI/Desktop 另仓库引用提供公开事件/审批协议文档

完成条件: 在 Windows 实机/CI 满足第 10.3 节安全验收, 才能宣称「Windows 原生沙箱已实现」。不因为阶段安排而编造能力。

## 20. 公共 API 与发布规范

- 仅根目录一个 `go.mod`, 明确主 Module 路径
- 稳定公开 API 只放在必要 Package; 不暴露内部结构体或大量 Runtime 专有类型
- 依赖 tRPC 版本固定; 发布前验证上游 `session/sqlite` 等独立子模块的版本兼容
- 公共功能提供 Go Doc、最小示例和边界说明
- 每次 API 变更运行单元/集成测试, `go vet` 与格式检查
- Tag 按 Go Semantic Versioning 管理; 本项目未发布前不需要虚构多代方案或迁移指南
- README 说明依赖环境、Windows 能力状态、沙箱权限风险和示例
- 不自动推送、发布 Release、创建 PR 或修改远程仓库, 除非用户明确授权

## 21. Cursor 执行要求

下面的要求是开发任务的一部分, 而不是一段供讨论的建议。

1. 阅读本文档后直接实施, 不重新展开 ADK/Eino/tRPC 框架选型
2. 首先查看工作区实际内容, 遵守已有项目组织和 `AGENTS.md`; 仓库为空时按本规范初始化
3. 只创建当前实现需要的目录; 不提前生成大量空接口、空文件或不工作的占位实现
4. 优先检索/阅读**固定版本 tRPC 源码和 GoDoc**, 严格以真实公开 API 为准; 文档中的 API 草图不保证直接编译
5. `trpc-agent-go` 的原生能力能满足需求时直接复用, 禁止重写同构子系统
6. Engine 的 Plugin/MCP/Skill/Hooks/Rules/Memory 必须是可独立引用的 SDK 能力, 不允许转嫁给未来的 TUI/Desktop
7. Linux/macOS 沙箱和 Windows 缺口的真实状态必须准确; 不允许使用未隔离 Local Executor 冒充 Sandbox
8. 在运行实例中禁用响应 Markdown 代码块的隐式自动执行, 不依赖 Skills 自动本地执行回退
9. 每个阶段实施完整的「代码 → 测试 → 修复 → 再验证」闭环, 记录未运行测试和阻塞条件
10. 优先实现真实调用链, 不要只创建 README/接口/Mock 却没有可用的运行时集成
11. 对与需求无关的目录、工具链、依赖不要顺手重构或升级
12. 不自动提交、推送、发布或修改生产资源; 完成后汇报实际改动、构建结果、测试结果和已知限制

## 22. 验收总表

| 编号 | 验收要求 | 检验方式 |
| --- | --- | --- |
| A | 单仓库、单 Go Module、扁平 Package | 根 `go.mod` + `go list ./...` |
| B | 外部项目可直接 import SDK | 独立消费工程构建 |
| C | 真实 Agent Tool Calling + Event Stream | 集成/端到端测试 |
| D | tRPC 原生 Session 持久化与恢复 | 进程重启测试 |
| E | 本地 Coding + Git Diff | Go Fixture 工程 |
| F | 实际受管沙箱执行与拒绝越权 | Linux/macOS 平台安全测试 |
| G | Windows 不假装支持原生沙箱 | Windows 平台行为测试 |
| H | Plugin 安装/卸载/启停 + 组件装配 | 组合插件测试 |
| I | MCP / Skill / Hooks / Rules 独立管理 | API 与集成测试 |
| J | 默认不开启 Memory/Evolution | 配置和请求次数检查 |
| K | tRPC 不泄漏到公共领域 DTO | 源码依赖检查 |
| L | 未来产品无需实现 Engine 功能 | 独立调用示例 |
| M | 错误、取消、终态可观察 | 流式和故障注入测试 |
| N | 跨平台构建与实际限制记录 | CI 报告 |

## 23. 一手资料与实施参考

以下链接是开发时的核对依据, **以本项目选定的固定版本源码为准**, 不应将 `main` 的新 API 直接混入旧依赖。

- tRPC-Agent-Go: https://github.com/trpc-group/trpc-agent-go/tree/v1.12.0
- Go Module: https://github.com/trpc-group/trpc-agent-go/blob/v1.12.0/go.mod
- Runner API: https://github.com/trpc-group/trpc-agent-go/blob/v1.12.0/runner/runner.go
- Agent / LLMAgent: https://github.com/trpc-group/trpc-agent-go/tree/v1.12.0/agent/llmagent
- Runner Plugin: https://github.com/trpc-group/trpc-agent-go/blob/v1.12.0/plugin/manager.go
- Agent Extension: https://github.com/trpc-group/trpc-agent-go/tree/v1.12.0/agent/extension
- Graph: https://github.com/trpc-group/trpc-agent-go/tree/v1.12.0/graph
- Skill: https://github.com/trpc-group/trpc-agent-go/tree/v1.12.0/skill
- MCP: https://github.com/trpc-group/trpc-agent-go/tree/v1.12.0/tool/mcp
- CodeExecutor: https://github.com/trpc-group/trpc-agent-go/tree/v1.12.0/codeexecutor
- OS Sandbox: https://github.com/trpc-group/trpc-agent-go/tree/v1.12.0/codeexecutor/sandbox
- Windows 缺口源码: https://github.com/trpc-group/trpc-agent-go/blob/v1.12.0/codeexecutor/sandbox/backend_other.go
- SQLite Session: https://github.com/trpc-group/trpc-agent-go/tree/v1.12.0/session/sqlite
- Evolution: https://github.com/trpc-group/trpc-agent-go/tree/v1.12.0/evolution
- AG-UI 独立模块: https://github.com/trpc-group/trpc-agent-go/tree/v1.12.0/server/agui
- 官方文档: https://trpc-group.github.io/trpc-agent-go/zh/
- Go Modules 官方文档: https://go.dev/doc/modules/managing-dependencies

---

**最终交付定义**: 一个可以由外部 Go 产品直接引入、在真实项目中执行 Coding Agent 任务、拥有原生插件与扩展生态、严格区分已实现/未实现平台能力的 SDK。开发重点是能力组合、接口稳定、实际行为和安全边界, 不是重新开发另一套 Agent Framework。
