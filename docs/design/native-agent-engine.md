# 自研 Agent 引擎（路径 C）技术方案

状态：v2 裁定稿（4 项开放问题已裁决，可作为实施依据）
日期：2026-08-28
前置阅读：Kimi Code 源码分析（会话记录）、`internal/agentbridge`（现有 ACP 桥）

## 1. 背景与动机

当前聊天工作台的 Agent 能力完全外包给 Grok CLI：`agentbridge` spawn `grok` 子进程，
通过 ACP 协议转发 prompt、回传事件。这带来三个结构性问题：

1. **能力天花板被锁死**。上下文管理、工具质量、子代理、权限粒度全部由 Grok CLI 决定，
   我们只能在壳层做灾难恢复（session load overflow 后降级重建 + bootstrap 注入）。
2. **脆**。ACP 连接层的 notification queue overflow、peer disconnected 等错误只能靠
   字符串匹配识别（`IsSessionLoadOverflow`），恢复路径不可控。
3. **依赖用户环境**。要求用户机器上装有 grok CLI 且已登录，终端里能跑 `grok`。

Kimi Code（Moonshot 开源）证明了正确的形态：引擎自研，模型层用窄接口隔离。
其 agent-core 约 71k 行 TS、230 个测试文件；但其中**可支撑一个生产级引擎的内核
只有约 3k 行 loop + 1.5k 行 provider 抽象**，其余是 TUI/插件/遥测等生态件。
用 Go 复刻这个内核，对我们是完全可行的工程量。

## 2. 目标与非目标

### 目标

- G1：聊天工作台的 Agent loop 由 grok_switch 进程内自研引擎驱动，摆脱对 grok CLI 的运行时依赖。
- G2：现有 Web UI（桌面 + 配对手机）零重写或极小改动接入。
- G3：在自研引擎上落地 Kimi Code 验证过的能力：主动上下文压实、权限规则 DSL、
  结构化 todo/plan、事件录制回放；后续 goal mode 与并行子代理。
- G4：acp 桥保留为可切换的旧引擎，灰度过渡，出问题可一键回退。

### 非目标

- 不做通用 Agent 框架 / 不发布为库（先服务本工具）。
- 不复刻 Kimi 的 plugin 市场、IDE ACP server、cron、swarm（列为远期）。
- 不做自定义模型接入的通用 provider 矩阵（只覆盖本工具实际服务的上游，见 §4 决策 D1）。

## 3. 现状盘点：可直接复用的资产

| 资产 | 位置 | 对引擎的价值 |
|---|---|---|
| `/grok/v1` 本地代理 | `internal/server/grok_proxy.go` | 已解决上游全部 wire 层难题：previous_response_id 丢失重写、加密 reasoning 剥离、跨账号故障转移（3 次尝试）、粘性会话绑定（resp_/sess_ key + 1h TTL 持久化）、`x-grok-switch-*` 可观测头。**引擎直接 loopback 调它，一行不用改。** |
| 号池 | `internal/grokpool` | 账号巡检/隔离/轮换/粘性，引擎天然继承"生图同款"的多账号能力 |
| 供应商 Profile | `internal/profiles/model.go` | 已有 `upstream_format`、`api_backend`、`context_window`、`max_completion_tokens`、`supports_reasoning_effort` —— 这就是 Kimi `ModelCapability` 的数据基础 |
| 生图引擎 | `internal/server/imagine*.go` | 原生工具化：不再需要 MCP 子进程绕行，`generate_image` 直接作为 builtin tool 调 `ImagineEngine` |
| AgentService 接口 | `internal/server/agent.go:25` | **关键接缝**：server 层已经通过接口消费 agent，替换实现即可，WS 事件协议可保持兼容 |
| UI 事件协议 | `agentbridge.Event` | type/text/tool/permission/plan/retry 结构已被前端消费，新引擎按同协议发事件 |
| 历史会话存储 | `agentbridge/history.go` | 存储布局与列表/回放 API 可沿用 |

## 4. 关键决策

### D1 上游协议：以 OpenAI Responses 为主，Profile 驱动适配

- 现状：Grok Auth 池上游（`cli-chat-proxy.grok.com/v1`）走 Responses wire 格式；
  供应商 Profile 已有 `upstream_format` 字段区分 `openai_responses` / 其他。
- 决策：引擎内部使用统一的消息表示（参考 kosong 的 `Message`/`ContentPart`），
  provider 适配器负责翻译。第一期只做 **Responses 适配器**（走 loopback `/grok/v1`），
  P4.5 补 `chat/completions` 适配器（见 D7——这不是可选优化，是覆盖第三方中转
  Profile 的必需件，one-api/new-api 系中转几乎只讲 chat/completions）。
- 理由：Responses 的服务端 reasoning 状态与粘性会话是 Grok 上游的实际行为，代理已
  处理其所有边界情况；先窄后宽。

### D1b 引擎侧会话状态：无状态优先（全量历史重放），`previous_response_id` 仅作优化

这是本方案最重要的协议层决策，直接影响正确性：

- **引擎自持全部历史**，每步以完整 `input` 数组请求上游；`prompt_cache_key` 用
  sessionID 固定，吃上游 prompt cache，摊平重放成本。
- **不默认使用 `previous_response_id`**。原因：
  1. 服务端状态把账号粘死——号池换号、上游丢弃响应（`isPreviousResponseNotFound`
     已证明会发生）都会打断链条，引擎要为"续链"维护一套易错的状态机；
  2. 无状态重放是 Claude API / kosong 的成熟形态，历史真实可控、可回放、可压实；
  3. 重放时的已知脏问题（assistant reasoning 项被拒）代理层 `stripEncryptedReasoning`
     已解决——loopback 方案的又一次红利。
- 保留优化通道：Provider 接口暴露 `WithContinuation(responseID)`，某天想省流量再开，
  且必须配"续链失败 → 自动降级全量重放"的阶梯（代理的 lost-continuation 信号做触发）。

### D2 引擎调用上游的方式：loopback HTTP，而不是直连上游

- 引擎以 HTTP 客户端身份请求 `http://127.0.0.1:<port>/grok/v1`，携带号池本地 key，
  与今天 grok CLI 的身份完全一致。
- 备选方案是引擎直接调 `grokpool`/`grokauth` 拿 token 直连上游——被否决：
  会复制代理里 content-repair、failover、sticky 三套逻辑，两处漂移。
- 代价：多一跳 loopback（localhost 开销可忽略）。测试时用注入的 base_url 指向 httptest server。

### D3 上下文管理：用上游回传的真实 usage，不做 tokenizer

- Kimi 用估算 token 触发 compaction（85% 窗口 + 50k 保留输出）。
- 我们每一步都能从 Responses 流拿到真实 `usage.input_tokens`（代理已透传）。
  触发条件直接用「上一步真实 input + 预留输出 ≥ context_window」，
  省掉一个 tokenizer 依赖（Go 生态 tokenizer 对 grok 词表也不保证准）。
- chat/completions 上游拿不到流式 usage 时（见 D7），按字符数/4 近似，
  触发阈值自动放宽 10%——宁可晚压不可误压。

### D4 工具面：第一期内置 9 个工具，不做 MCP 通用客户端

参考 Kimi builtin 工具集，第一期（对齐现有体验所需）：

| 工具 | 说明 |
|---|---|
| `read` | 文本分页读取，行号格式，图片/视频复用现有 media 压缩逻辑 |
| `write` | 整文件写入（原子写，复用 `switcher.atomicWrite` 模式） |
| `edit` | 精确串替换（`old_string` 唯一性校验；模糊匹配列为二期） |
| `glob` / `grep` | `doublestar` + 正则；数据量与超时预算 |
| `bash` | 平台 shell 执行，超时、输出截断、后台化（前台超时转后台，学 Kimi） |
| `todo_list` | 结构化任务列表，UI 已有渲染位（tool event） |
| `enter/exit_plan_mode` | 对接现有 plan 审批 UI |
| `generate_image` | 直连 `ImagineEngine`，支持快速/标准/高清 + 宽高比（对齐 MCP 版语义） |

第二期：`fetch_url`、`read_media` 独立化、skills。MCP 通用客户端（Kimi 的
registry/reconcile/OAuth 那套）列为远期——它单独立项都不小。

### D5 权限模型：规则 DSL + 模式，一期就做

现有会话级 autoApprove 布尔太粗。自研后直接落 Kimi 的三模式 + 规则 DSL：

- 模式：`manual`（默认，未命中规则就问）/ `yolo`（只拦 deny 规则）/ `auto`（全放行，等价今天的 autoApprove）。
- 规则：`Tool(pattern)` 形态，如 `read(**)` 免批、`bash(git *)` 免批、`bash(rm *)` 拒绝、
  `write(~/.ssh/**)` 拒绝。scope 分 `session`（内存）与 `user`（settings.json 持久）。
- 审批 UI 复用现有 permission event 流，扩展「本次会话记住」选项即对应 session 规则。

### D6 过渡策略：引擎选择器 + 会话级隔离

- `settings.json` 增加 `agent_engine: "acp" | "native"`。默认值定 **`native`**，
  但带运行时自检：MVP 联调通过后翻转默认；`native` 引擎启动失败（理论上只剩
  配置错误）自动降级 `acp` 并在 UI 顶条提示。不做长期双默认——两个"默认"等于
  没有默认，维护者会永远背着两套引擎的隐性兼容负担。
- 切换粒度：新会话用当前引擎；旧会话历史两边都只读支持，回放不混写。
- 收尾计划：P6 完成且稳定一个版本周期后，ACP 桥进入只修不增模式，
  作为「官方账号兼容模式」长期保留（部分用户用 grok CLI 自身能力如 skills，
  不应被强制迁移）。

### D7 第三方中转 Profile：P1 就必须支持 chat/completions

以维护者视角看这不是"加不加"的问题：本工具的核心场景之一就是接第三方中转
（one-api/new-api 系），而这些中转几乎只讲 chat/completions。native 引擎若只讲
Responses，等于把「供应商切换」这个产品根基在聊天工作台里砍掉一半，用户切到
中转 Profile 后 Agent 按钮直接不可用——不可接受。

- 落点：`internal/llm` 的 Provider 接口天生为多后端设计（kosong 有 4 个 provider
  实现，接口面很窄），chat/completions 适配器作为 P1 的第二交付物，与 Responses
  适配器共享 Message/usage/流式装配层，增量约 400 行。
- 差异处理：chat/completions 无服务端 reasoning 状态（与 D1b 无状态重放天然契合），
  usage 在流式 `chunk.usage`（需 `stream_options.include_usage`）拿不到时按字符
  近似并在能力表标记 `usage_accuracy: approximate`，compaction 阈值自动放宽 10%。

### D8 手机端：零改动假设成立，但补一条端到端验收

事件走同一条 WS 广播，配对手机用的是同一个前端 bundle，D6 的引擎切换对手机透明。
验收标准加一条：P4 联调必须含「手机配对 → 发消息 → 收流式回复 → 权限弹窗可审批」
的端到端用例，防止 server 层实现里埋了仅桌面可用的分支。

## 5. 总体架构

```
ui/app.js (不动)                    WS + REST
──────────────────────────────────────────────
server 层 (小改)
  /api/agent/*  ←→  AgentService 接口
                        │
        ┌───────────────┴────────────────┐
        │ acpBridgeService (现有)         │ nativeAgentService (新)
        │ spawn grok CLI                  │ agentloop.Engine
        └────────────────────────────────┘
─── 引擎边界（internal/ 新包，互不依赖 server）───
  internal/llm        kosong-go：Message/ContentPart/ToolCall/TokenUsage/
                      ModelCapability + Provider 接口 + Responses 适配器
  internal/agentloop  loop-go：RunTurn / Step / ToolCall 批次 / 调度 /
                      重试与 resend 策略 / 事件流（录制+直播分离）
  internal/tools      工具实现 + 注册表 + 参数校验 + 结果预算截断
  internal/agentfs    执行环境抽象（kaos-go 最小集：路径/读/写/进程，local 实现）
  internal/ctxmem     上下文记忆：PromptOrigin 标记、compaction handoff
──────────────────────────────────────────────
  loopback /grok/v1 ──→ grok_proxy ──→ grokpool ──→ cli-chat-proxy.grok.com
  imagine 引擎 ←── tools/generate_image
```

分层契约（抄 Kimi loop/README 的纪律，翻译成 Go 约定）：

- `agentloop` 不 import 任何 UI/存储/权限实现；通过接口注入。
- 事件单出口：`Dispatch(Event)`；录制（JSONL append）与直播（chan 广播）都在出口处分发，
  直播订阅者 panic/慢速不影响 loop。
- usage 在 LLM 返回后立即记录（不等工具跑完），abort 也报告已消耗 token。

## 6. 模块设计

### 6.1 internal/llm（kosong-go，约 1.5k 行）

```go
type Role string // system|user|assistant|tool

type ContentPart interface{} // TextPart | ThinkPart | ImagePart | ...（sealed）

type ToolCall struct { ID, Name string; Arguments json.RawMessage }

type Message struct { Role Role; Parts []ContentPart; ToolCalls []ToolCall; ToolCallID string }

type TokenUsage struct { InputOther, Output, InputCacheRead, InputCacheCreation int64 }
func (u TokenUsage) InputTotal() int64; func (u TokenUsage) GrandTotal() int64

type ModelCapability struct {
    ImageIn, VideoIn, Thinking, ToolUse bool
    MaxContextTokens int   // 0 = 未知
    MaxInputTokens   int   // 0 = 仅总量上限
}

type Provider interface {
    Name() string
    ModelName() string
    Capability() ModelCapability
    Generate(ctx, SystemPrompt string, Tools []Tool, History []Message,
             opts GenerateOptions) (*StreamResult, error) // 流式回调 + 最终装配
    WithThinking(effort string) Provider
}
```

- `ResponsesProvider`：POST loopback `/grok/v1/responses`，SSE 解析
  `response.output_text.delta` / `response.function_call_arguments.delta` /
  `response.completed`（usage 在这里）。思考流映射 `ThinkPart`（上游 reasoning summary）。
- 能力表来源：`profiles.Profile` 的 `context_window`/`max_completion_tokens`/
  `supports_reasoning_effort`/模型名前缀规则；未知模型返回 `UnknownCapability`，
  调用方自行 gate（与 kosong 的冻结 UNKNOWN 语义一致）。

### 6.2 internal/agentloop（loop-go，约 2k 行）

```go
type Engine struct { /* deps: Provider, ToolTable, PermGate, Recorder, CtxMemory */ }

func (e *Engine) RunTurn(ctx context.Context, in RunTurnInput) (TurnResult, error)

type RunTurnInput struct {
    SessionID   string          // 作为 prompt_cache_key（粘性会话复用代理绑定）
    Build       *ctxmem.Memory  // 每步重建 model-visible messages
    Hooks       Hooks           // beforeStep(compaction 切入点)/shouldContinueAfterStop/…
    MaxSteps    int             // 默认 100
    MaxRetry    int             // 默认 3（对齐代理 failover 次数）
}
```

- **RunTurn**（对应 run-turn.ts，255 行）：while 循环；`tool_use` 继续、其余终止；
  abort 区分 user_cancelled；max-step 抛错；usage 聚合。
- **Step**（对应 turn-step.ts，587 行）：pre/post hook、消息构建、原子 step 封套、
  流回调、**三级 resend 策略**：
  1. 工具相邻性 400 → strict rebuild 重发一次；
  2. 413/体积超限 → media-degraded（旧媒体换文本标记，保留最近一条）重发，
     成功后本 turn 后续步全部用降级投影；
  3. 图片格式错误 → media-stripped（全部媒体换标记）。
  这三条是 Kimi 踩坑换来的实战逻辑，全部照搬。
- **ToolCall 批次**（tool-call.ts）：provider 顺序记录 call 事件；执行调度一期从简——
  顺序执行 + 只读工具（read/glob/grep）并行小优化放二期；每个 call 必有 result 事件配对。
- **重试**（retry.ts 语义）：限流/网络错误指数退避，429 尊重 Retry-After；错误分类
  可恢复（pause 语义，见 6.5）vs 终止。
- **权限闸**：工具执行前过 `PermGate.Check(tool, args) → allow|deny|ask`；ask 通过
  审批回调（对应现有 WS permission request）挂起 turn。

### 6.3 internal/tools

```go
type Tool interface {
    Name() string
    Schema() map[string]any        // JSON Schema
    Doc() string                   // 工具用法说明（拼进 system prompt，学 Kimi 的 *.md 内嵌）
    Execute(ctx context.Context, args json.RawMessage, env agentfs.Env) ToolResult
}
type ToolResult struct { Output string; IsError bool; Truncated bool; Media []MediaPart }
```

- 结果预算：统一字符截断 + `[truncated]` 标记（Kimi `tool-result-budget` 的简化版）。
- bash：`context` 超时 → 转后台任务表（任务 ID + 完成事件推送），不直接 kill；
  每次调用独立 shell 环境，cwd 参数化。
- read：行号 + 分页 + 大文件守卫；图片走现有压缩管线。
- edit：`old_string` 必须唯一命中，否则报错并列出命中次数（比模糊匹配安全，先跑起来）。

### 6.4 internal/ctxmem + 会话持久化

- `PromptOrigin` 分类（学 Kimi context/types.ts）：`user / injection / shell /
  compaction_summary / system_trigger / background`。**这是 compaction 正确性的根基**：
  压实时只保留真实 user 消息原话，injection 类一律可重建。
- 持久化：每会话一个 JSONL（`~/.grok_switch/agent2/sessions/<id>/transcript.jsonl`），
  事件追加式（seq 严格递增），恢复 = 重放进 `ctxmem.Memory`。
  另存轻量 meta（标题/模型/cwd/统计），列表 API 直接读 meta。
- Compaction（二期，一期先做阈值提示）：
  - 触发：上一步真实 usage ≥ 85% 窗口，或 usage + 预留 50k ≥ 窗口；
  - handoff：保留用户原话（预算 20k token 按字符近似，最老 2k 强制保留形成 head/tail +
    省略标记）+ 摘要消息（带 `COMPACTION_SUMMARY:` 前缀告知模型这是交接上下文）；
  - 压实请求本身溢出时，诚实记录 droppedCount。
  - token 预算按「字符数/4」近似即可，误差对 85% 阈值安全。

### 6.5 稳定性与错误语义（对齐 Kimi goal 的停车分类，先用于 turn 级）

- 用户中断 → turn 终止，标记 `user_cancelled`；
- 限流/网络/上游 5xx → 指数退避重试，耗尽后 turn 暂停（会话可恢复）；
- 上下文溢出 → 触发 compaction 后重试（限 3 次）；
- 权限拒绝 → 工具结果以 `IsError` 回给模型继续 turn（不是终止）；
- 全部进遥测日志，`x-grok-switch-*` 头的换号信息透传到事件流。

### 6.6 权限（internal/permission，约 500 行）

规则编译：`Tool(pattern)` → 工具名精确匹配 + 参数 matcher（每工具提供
`MatchPattern(args, pattern) bool`；bash=命令前缀通配，read/write/edit/glob=glob 路径）。
决策顺序：deny 规则 > 模式 > allow 规则 > ask 默认。UI 上「总是允许/仅本会话/仅本次」
三按钮 → 分别写 user 规则 / session 规则 / 单次放行。

### 6.7 server 接入

`nativeAgentService` 实现 `AgentService` 全部 20 个方法：
- `Start/Stop`：懒加载引擎实例（不再管子进程生命周期）；
- `Prompt`：构造 RunTurn 异步执行，事件泵转发到 `Subscribe` chan（协议不变）；
- `RespondPermission*` / `RespondPlan`：唤醒挂起的闸；
- 历史方法走新存储；旧的 `ArmBootstrapFromSession` 在 native 引擎下是 no-op。

## 7. 分阶段实施

| 阶段 | 内容 | 产出/验收 | 预估 |
|---|---|---|---|
| P1 | `internal/llm` + ResponsesProvider + chat/completions 适配器（D7）+ 能力表 | httptest 上游跑通流式/usage/重试单测，双协议同一套 e2e | 5-6 天 |
| P2 | `internal/agentloop`（RunTurn/Step/resend/重试/事件录制） | 假工具 e2e：多步工具调用、abort、413 降级、限流退避 | 4-5 天 |
| P3 | `internal/tools` 9 工具 + `internal/agentfs` | 工具单测 + 冒烟：读改文件、跑测试命令 | 5-6 天 |
| P4 | 会话持久化 + `nativeAgentService` + 引擎选择器 | WS 协议对齐老事件；含手机端到端用例（D8） | 4-5 天 |
| P5 | 权限 DSL + 审批 UI 三按钮 | 规则单测；UI 联调 | 3-4 天 |
| P6 | ctxmem origin 标记 + compaction handoff | 长会话压测：85% 触发、恢复回放一致 | 4-5 天 |
| P7 | generate_image 原生工具化 | 脱离 MCP 子进程的生图链路 | 2 天 |
| 远期 | goal mode / swarm / MCP 客户端 / skills | 另立设计文档 | — |

MVP = P1–P4（约 3.5 周全职当量），此后每阶段独立可发布。默认引擎翻转（D6）
在 P5 结束、P6 开工前执行，让 compaction 直接按"默认引擎"验收，不做两遍。

## 8. 风险与对策

| 风险 | 对策 |
|---|---|
| 我们的 system prompt + 工具文档调教不如 xAI 官方 | 引擎选择器保底回退 ACP；工具 Doc 直接借用 Kimi 的成熟文案骨架逐条本地化迭代 |
| 上游 Responses 行为依赖 grok CLI 侧协议演进 | 引擎与 CLI 都走同一个代理，代理层已隔离差异；引擎不做协议假设的硬编码 |
| edit 精确替换在真实代码上成功率低 | P3 验收含 20 个真实编辑用例；不达标提前上模糊匹配（`fuzzy` 唯一命中+diff 预览） |
| 双引擎维护成本 | 原生稳定后（P6 完成）ACP 桥降级为「官方账号兼容模式」，只修不增 |
| 每会话 JSONL 无限增长 | 复用 compaction：transcript 全量保留（回放用），context memory 是投影，磁盘量级可控 |

## 9. 决策裁定（此前"待确认问题"的裁决结果）

原方案的 4 个开放问题，以维护者视角裁定如下，文档相应章节已同步：

1. **工具面（原问题 1）**：维持 9 工具。`fetch_url` 不进一期——它是独立能力
   （HTML→markdown、体积预算、robots 语义），塞进 MVP 只会稀释验收质量；
   权限 DSL（D5）让 bash 里 `curl` 的审批可控，一期够用。二期与 skills 同批进。
2. **默认引擎（原问题 2）**：裁定 MVP 后默认 `native`，带自检降级（D6）。
   理由：自研引擎是本项目的战略方向，长期 opt-in 意味着它永远吃不到真实流量、
   永远是"实验品"，双引擎的隐性维护成本反而更高。灰度用翻转前的版本周期完成。
3. **chat/completions（原问题 3）**：裁定 P1 必须交付（D7）。理由：供应商切换是
   产品根基，砍掉中转 Profile 的 Agent 能力等于自残；接口已为其设计，增量小。
4. **手机端（原问题 4）**：零改动假设成立，追加端到端验收用例（D8）。

另新增一条非开放性裁决——**D1b 无状态重放**：引擎自持全部历史、每步全量请求、
不用 `previous_response_id` 做默认路径（详见 §4）。这是号池多账号形态下唯一
自洽的会话状态模型，也是方案正确性的基石。
