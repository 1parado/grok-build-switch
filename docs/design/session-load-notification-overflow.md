# 历史会话恢复失败：ACP 通知队列溢出（notification queue overflow）

> 状态：**已实现**（传输层预过滤 + 失败自愈）
> 日期：2026-07-19
> 影响版本：grok_switch v0.5.0+（引入 ACP 聊天与 `session/load` 后）
> 依赖：`github.com/coder/acp-go-sdk v0.13.5`、`grok agent stdio`
> 相关代码：
> - `internal/agentbridge/bridge.go` — `loadSessionLocked` / `Start`
> - `internal/agentbridge/client.go` — `SessionUpdate` / `suppressUpdates`
> - `internal/server/agent.go` — `POST /api/agent/session/load`
> - `ui/app.js` — `resumeAgentSession`
> 现场日志：`%USERPROFILE%\.grok_switch\grok_switch.log`

---

## 1. 结论（先看这个）

| 问题 | 结论 |
|:---|:---|
| **用户看到什么？** | 点开历史会话后提示：`恢复 Grok 会话失败: ... peer disconnected before response`（JSON-RPC `-32603 Internal error`） |
| **日志里对应什么？** | `grok_switch.log` 出现 `notification queue overflow` / `connection closed cause="notification queue overflow"` |
| **根因是什么？** | ACP 客户端通知队列固定 **1024**；`session/load` 时 Grok Agent 短时间回放大量 session update notification，入队速度超过处理速度，队列满后 **SDK 主动关闭连接**，正在等待的 `LoadSession` 表现为 peer 断开 |
| **`agent.log` 为空说明什么？** | 多半正常：溢出日志由 **switch 进程内的 acp-go-sdk** 打到 stderr（已重定向到 `grok_switch.log`），不是 grok 子进程 stderr |
| **`suppressUpdates` 能否防止？** | **不能。** 它只在 `SessionUpdate` 处理阶段丢弃 UI 广播；通知仍须先进入 1024 队列 |
| **是不是网络/别人电脑的问题？** | **否。** peer 指本机 stdio 上的 ACP 连接对端；溢出发生在运行 grok_switch 的那台机器上 |
| **最终如何修复？** | 在 Grok stdout 与 ACP SDK 之间增加仅在 `session/load` 期间启用的过滤层，让回放型 `session/update` 在进入 SDK 的 1024 队列前被丢弃；响应、请求和正常对话通知保持不变。溢出仍保留自动重启与只读历史兜底。 |

**一句话：**

> 恢复大会话时 Agent 回放洪水淹没 ACP 客户端 1024 通知队列 → 连接被 SDK 掐断 → UI 报「恢复会话失败 / peer disconnected」。

---

## 2. 现象与现场证据

### 2.1 用户可见错误

```text
恢复 Grok 会话失败：
  code: -32603
  message: "Internal error"
  data.error: "peer disconnected before response"
```

包装点（`bridge.go`）：

```go
response, err := conn.LoadSession(ctx, acp.LoadSessionRequest{...})
if err != nil {
    return fmt.Errorf("恢复 Grok 会话失败: %w", err)
}
```

### 2.2 诊断日志（关键）

路径：`%USERPROFILE%\.grok_switch\grok_switch.log`

```text
ERROR failed to queue notification; closing connection
  err="notification queue overflow" capacity=1024 queued=1024
INFO  connection closed cause="notification queue overflow"
```

同源文案来自 `acp-go-sdk` `connection.go`（队列非阻塞入队失败即 `shutdownReceive`）。

### 2.3 次要噪声（可忽略）

```text
systray error: unable to removeMenuItem: Invalid menu handle.
```

托盘菜单刷新句柄问题，与会话恢复无关。

### 2.4 日志文件分工

| 文件 | 写入方 | 本问题是否有用 |
|:---|:---|:---|
| `~\.grok_switch\grok_switch.log` | switch + ACP SDK（stderr 重定向） | **是**，溢出与断连在这里 |
| `~\.grok_switch\agent.log` | `grok agent` 子进程 stderr | 常为空，不代表 Agent 未启动 |

---

## 3. 技术根因

### 3.1 调用链

```
用户点击历史会话
  → UI: GET /api/agent/sessions/:id     （读 chat_history.jsonl，只读展示）
  → UI: POST /api/agent/session/load    （真正恢复 ACP 上下文）
  → Bridge.Start(session_id=...)
  → loadSessionLocked
       suppressUpdates = true
       conn.LoadSession(...)            ← 同步等待 JSON-RPC 响应
       suppressUpdates = false
  → Grok Agent 在 load 过程中推送大量 session/update notification
  → acp-go-sdk receive 协程把 notification 写入有界队列（cap=1024）
  → processNotifications 顺序处理（调用 Client.SessionUpdate 等）
  → 入队 > 出队 时队列打满
  → SDK: closing connection (overflow)
  → LoadSession 等待中的请求失败：peer disconnected before response
```

### 3.2 为何 UI 已显示历史仍报失败

恢复是 **两步**：

1. **只读历史**：从 `~\.grok\sessions\...\chat_history.jsonl` 渲染气泡（不依赖 ACP load 成功）。
2. **引擎恢复**：`session/load` 把 Agent 内部上下文载入，才能继续带状态对话。

因此会出现「记录看得见，但恢复失败 / 后续无法真正续聊」的割裂体验。

### 3.3 为何 `suppressUpdates` 救不了

```go
// client.go SessionUpdate
if suppress {
    return nil  // 仅跳过 broadcast 到 WebSocket UI
}
```

SDK 侧流程是：

1. 从 stdout **读到** notification
2. **入队**（队列满则断连）← 瓶颈在此
3. 出队后调用 `SessionUpdate`
4. 此时 `suppressUpdates` 才生效

即：load 期间「不往前端刷」≠「不占队列」。最终实现因此将过滤点前移到 SDK 的输入流，而不是只依赖 `SessionUpdate` 回调。

### 3.4 SDK 约束（v0.13.5）

| 项 | 现状 |
|:---|:---|
| 队列容量 | `defaultMaxQueuedNotifications = 1024`（**包内常量**） |
| 是否可配置 | **否**（公开 API 仅有 `SetLogger` 等，无 `WithQueueSize`） |
| 溢出策略 | 非阻塞入队失败 → 打 Error 日志 → **关闭整个 Connection** |
| 处理模型 | 单消费者顺序处理，保证 notification 与 response barrier 顺序 |

结论：在不改 SDK / 不换版本的前提下，**无法通过配置项把 1024 调大**。

### 3.5 何种会话最容易炸

- 消息轮次多、assistant 流式 chunk 多
- 大量 tool_call / tool_update
- 含 subagent、长推理（thought）回放
- Grok 某版本在 load 时 **全量重放** session update，而非静默 hydrate

短会话可能从不触发；长会话在 1 秒内即可灌满 1024。

---

## 4. 修复目标

| 目标 | 说明 |
|:---|:---|
| **P0 不崩** | 恢复大会话时 ACP 连接不被队列溢出掐断，或失败后可自愈 |
| **P0 可理解** | 失败时提示明确（会话过大 / 仅只读 / 请开新对话），而不是裸 JSON-RPC |
| **P1 可续聊** | 在能力允许时真正 `session/load` 成功，保留上下文 |
| **P2 体验** | 只读预览与「已挂载引擎」状态在 UI 上可区分 |

非目标（本问题范围外）：

- 修复 systray Invalid menu handle
- 重写历史过滤 / subagent 会话混列表（另文）
- 修改 Grok CLI 内部 load 实现（可跟进上游，但不阻塞 switch 侧缓解）

---

## 5. 修复方案（分层，建议按阶段做）

### 阶段 A — 产品降级与自愈（优先，纯 switch 可做）

**目标：** 即使 load 失败，也不让 Agent 卡死；用户知道发生了什么。

#### A1. 识别溢出类错误

在 `loadSessionLocked` / `Start` 错误路径匹配：

- `notification queue overflow`
- `peer disconnected before response`
- `connection closed`

映射为稳定错误码 / 文案，例如：

```text
会话过大或恢复时通知过多，引擎上下文未能加载。
已展示本地历史（只读）。可开新对话，或稍后重试较短会话。
```

API 建议增加字段（向后兼容）：

```json
{
  "error": "...",
  "code": "session_load_overflow",
  "readonly_history": true,
  "recoverable": true
}
```

#### A2. load 失败后自动重启 Agent

溢出后 Connection 已废，进程可能半死：

1. `stopLocked()` 杀掉当前 `grok agent`
2. 重新 `Start`（无 session_id）到同一 cwd，进入 **新会话 ready**
3. 前端保留已渲染的历史气泡，标记 **只读 / 未挂载**
4. 用户发消息时：
   - 方案 a（稳妥）：提示「当前为只读历史，发送将开启新会话」并 `NewSession`
   - 方案 b：直接新会话 + 可选附带摘要（后续增强）

#### A3. UI：只读历史 vs 引擎已挂载

| 状态 | 展示 |
|:---|:---|
| 历史已加载 + load 成功 | 正常，可输入 |
| 历史已加载 + load 失败 | 横幅：「仅本地历史，上下文未恢复」；输入框可禁用或发送即新开 |
| load 进行中 | 「正在恢复引擎上下文…」 |

对应改动：`resumeAgentSession` 对 `/session/load` 失败 **不要** 当成整页失败清掉历史；分离 `historyOk` 与 `engineLoaded`。

#### A4. 超大会话可选「不强制 load」

启发式（可配置，默认开启）：

| 信号 | 建议 |
|:---|:---|
| `message_count` 或 jsonl 行数 > N（如 200） | 默认只读打开，按钮「尝试恢复引擎」 |
| jsonl 体积 > M MB | 同上 |
| 上次 load 对该 id 失败过（本地记忆） | 默认只读 |

这样避免一打开就炸连接。

---

### 阶段 B — 降低 load 路径的队列压力（中期）

**目标：** 提高真实 `session/load` 成功率。

#### B0. 已采用：SDK 入队前过滤 load 回放通知

`grok agent` 使用按行 JSON-RPC stdout，因此 switch 在 stdout 与 `acp.NewClientSideConnection` 之间加入透明 `io.Reader`：

1. 正常状态下透明转发，不解析、不改变消息。
2. `session/load` 在途时，只丢弃无请求 ID 的 `session/update` 与 `_x.ai/session/update`。
3. load response、Agent 发起的 request、其它 notification 全部保留。
4. load 完成后立即恢复透明模式，后续正常对话 update 不受影响。

这样回放洪峰不会进入 SDK 的有界队列，同时不需要 fork SDK，也不需要设置一个仍可能被更大会话打满的新容量。专项测试已覆盖 10,000 条连续回放通知，`LoadSession` 正常返回且连接保持可用。

#### B1. 确认 SDK 是否后续版本可配队列

| 动作 | 说明 |
|:---|:---|
| 查 `acp-go-sdk` 新版本 changelog / API | 是否出现 `WithMaxQueuedNotifications` 或类似选项 |
| 若有 | 升级依赖并设为更大值（如 8192 / 65536），并压测内存 |
| 若无 | 见 B2 / B3 |

**现状（v0.13.5）：不可配置。** 文档应在升级依赖时复查本节。

#### B2. 上游 / fork 选项（仅当必要）

1. **向 coder/acp-go-sdk 提 PR**：队列大小可配置；或溢出时丢弃 notification 而非断连（需评估 response barrier 语义）。
2. **临时 replace 模块**：`go.mod` replace 到 fork，仅放大 `defaultMaxQueuedNotifications`。
   - 优点：改动小
   - 缺点：维护成本；治标不治本（超长会话仍可能超过任意上限）

**注意：** 单纯放大队列会增加内存峰值；load 回放若有数万 chunk，仍可能 OOM 或极慢。

#### B3. load 期间「快速吞掉」notification（switch 侧，已实现）

在 `SessionUpdate` / extension handler 中，`suppressUpdates==true` 时已经 `return nil`（很快）。
若仍溢出，瓶颈更可能是：

- SDK 单线程 `processNotifications` 仍有固定开销
- 或 extension 路径 / 其它 handler 较慢
- 或 **入队速率本身** 高于任意合理处理（必须丢弃或 Agent 侧少推）

可做的 switch 侧优化：

1. 确保 suppress 路径 **零分配、无锁争用最小化**（现在已较轻）。
2. 通过 line-delimited JSON-RPC 输入过滤器在 SDK 入队前丢弃 load 回放；无需修改 SDK barrier。
3. 与 Grok 侧确认：load 是否可 **silent hydrate**（不回放 chunk），仅返回 session ready——这是最干净的协议层解法，但属 Grok CLI 行为，switch 无法单独完成。

#### B4. 超时与并发策略

- `session/load` 当前 HTTP 超时约 45s；溢出往往更快。失败后务必 A2 重启，避免后续请求全挂。
- 禁止在 busy 时并发 load；UI 连点历史应 debounce / 队列化。

---

### 阶段 C — 架构增强（可选，长期）

#### C1. 双通道恢复语义

| 模式 | 行为 | 适用 |
|:---|:---|:---|
| `preview` | 只读 jsonl，不 load | 默认打开、大会话 |
| `attach` | `session/load` | 用户明确「继续此任务」 |
| `fork` | 新会话 + 注入摘要/关键摘录 | load 失败后的续聊 |

#### C2. 会话列表元数据

列表展示 `message_count`、体积、是否 subagent（`agent_name`），对高风险会话标「仅预览」。
（可与 subagent 混列表问题一并做。）

#### C3. 可观测性

- load 开始/结束写 `grok_switch.log`：session id、耗时、是否 overflow
- 前端展示 `engineLoaded`
- 可选：统计 notification 粗略速率（若 SDK 暴露）

---

## 6. 推荐落地顺序（实施清单）

### 第一期（建议尽快，1–2 PR）

| # | 项 | 文件（预期） | 验收 |
|:---|:---|:---|:---|
| 0 | load 回放在 SDK 入队前过滤 | `notification_filter.go`、`bridge.go` | 已实现：10,000 条回放通知不溢出，load 成功 |
| 1 | 错误分类 + 友好文案 `session_load_overflow` | `bridge.go`、`agent.go`、`app.js` | 已实现：不再只显示裸 -32603 |
| 2 | load 失败自动 Stop + 重启到 ready 新会话 | `bridge.go` | 已实现：溢出后仍可新对话 |
| 3 | UI 分离历史展示与引擎挂载状态 | `app.js`、`index.html`/`style.css` | 已实现：失败保留气泡 + 横幅 |
| 4 | 发送消息时若未挂载则新建会话（或明确确认） | `app.js` | 已实现：确认后安全切换新会话 |

### 第二期

| # | 项 | 验收 |
|:---|:---|:---|
| 5 | 大会话默认只读 +「尝试恢复」按钮 | 打开超长会话不再必炸 |
| 6 | 检查/升级 acp-go-sdk；若可配队列则调大并文档化 | 中等会话 load 成功率上升 |
| 7 | 失败会话 id 本地记一笔，下次默认 preview | 减少重复炸连接 |

### 第三期（视上游）

| # | 项 | 验收 |
|:---|:---|:---|
| 8 | SDK PR 或 Grok silent-load 跟进 | 超长会话也可 attach |
| 9 | fork / replace 仅作临时手段并设移除期限 | 不长期漂移 |

---

## 7. 详细设计要点

### 7.1 Bridge：`loadSessionLocked` 伪代码

```text
func loadSessionLocked(ctx, sessionID, cwd):
    // ... 现有 cwd / summary 校验 ...
    suppressUpdates = true
    defer suppressUpdates = false

    resp, err := conn.LoadSession(...)
    if err == nil:
        // 现有成功路径
        return nil

    if isOverflowOrPeerDisconnect(err):
        log.Printf("session load overflow session=%s: %v", sessionID, err)
        // 连接可能已废：强制停进程，避免后续请求全失败
        stopLocked()
        // 可选：立即拉起无 session 的 agent，或留给上层 Start
        return &LoadError{Code: "session_load_overflow", Cause: err, ReadonlyOK: true}

    return fmt.Errorf("恢复 Grok 会话失败: %w", err)
```

`isOverflowOrPeerDisconnect` 应匹配错误链字符串（SDK 未导出哨兵错误时）：

- `notification queue overflow`
- `peer disconnected before response`
- `peer connection closed`（视 SDK 文案）

### 7.2 HTTP / UI 契约

`POST /api/agent/session/load`：

| 结果 | HTTP | body |
|:---|:---|:---|
| 成功 | 200 | 现有 `Status`，可加 `engine_loaded: true` |
| 溢出/断连 | 建议 **200 或 409 二选一**（见下） | 明确 code + `engine_loaded: false` + status |

**建议用 409（Conflict）或 503 + 结构化 JSON**，前端用 `code === "session_load_overflow"` 分支；避免 500 被当成「整页挂了」而 `clearAgentTranscript`。

当前 `resumeAgentSession` 在 load 失败时会 toast error；应改为：

1. 历史已 `renderStoredHistory` 的保留
2. toast / banner 说明只读
3. `state.engineLoaded = false`

### 7.3 启发式阈值（初值，可配置进 settings）

| 键 | 建议默认 | 含义 |
|:---|:---|:---|
| `agent_load_auto_max_messages` | `150` | 超过则默认 preview |
| `agent_load_auto_max_jsonl_bytes` | `2_000_000` | 超过则默认 preview |
| `agent_load_retry_enabled` | `true` | 失败后是否自动重启 agent |

实现时从 `summary.num_chat_messages` 与 `chat_history.jsonl` 文件大小读取，无需解析全文。

### 7.4 测试计划

| 类型 | 内容 |
|:---|:---|
| 单元 | `isOverflowOrPeerDisconnect` 字符串/错误链 |
| 单元 | 启发式：消息数/体积阈值 |
| 集成（可选） | mock ACP：灌 >1024 notification，断言 load 返回 overflow code 且进程可重启 |
| 手工 | 真实大会话：只读打开不崩；点「尝试恢复」；失败后新对话可用 |
| 手工 | 短会话：load 仍成功，可续聊 |

### 7.5 风险

| 风险 | 缓解 |
|:---|:---|
| 放大队列导致内存暴涨 | 优先 A/C 策略；放大仅作辅 |
| 只读后用户以为仍在原会话上下文 | UI 明确横幅 + 发送确认 |
| 自动重启杀掉正在跑的 turn | 仅在 load 失败路径重启；busy 时拒绝 load |
| 错误字符串随 SDK 变更 | 多模式匹配 + 日志保留原文 |

---

## 8. 与既有行为的关系

| 行为 | 保持 / 调整 |
|:---|:---|
| 历史来自本地 jsonl | **保持**；作为可靠只读源 |
| `session/load` 才有完整 Agent 状态 | **保持**语义；失败时明确「未挂载」 |
| `suppressUpdates` 避免 load 时 UI 刷屏 | **保持**；并文档标明不能防溢出 |
| 权限 / always-approve | 不变 |
| 局域网手机聊天 | 同机 Agent；同样受队列限制 |

---

## 9. 给支持 / 用户的临时绕过（修复前）

1. 优先 **新对话**，不要恢复超长历史。
2. 若必须看记录：打开后若报错，把界面当 **只读**；需要执行任务则新开。
3. 报错后 **完全退出 grok_switch 再开**（清掉已废的 ACP 连接）。
4. 排查时看 `~\.grok_switch\grok_switch.log` 是否有 `notification queue overflow`。
5. 更新 Grok Build / grok_switch 到较新版本后再试中等长度会话。

---

## 10. 决策记录（建议实现时勾选）

- [x] 第一期是否采用「load 失败 → 自动重启 Agent + 只读历史」？ **已实现**
- [ ] 大会话是否默认不 load？ **建议：是（带阈值）**
- [x] 是否立即 fork acp-go-sdk 放大队列？ **否；已用 SDK 入队前过滤解决根因**
- [ ] 是否向上游提 silent-load / 可配队列？ **建议：是（并行，不阻塞发版）**

---

## 11. 参考

- 本机模块：`github.com/coder/acp-go-sdk@v0.13.5`
  - `connection.go`：`defaultMaxQueuedNotifications = 1024`
  - 溢出：`failed to queue notification; closing connection`
- grok_switch：
  - `docs/design/chat-agent-integration.md`（聊天集成总设计）
  - `docs/blog/posts/v0.5.0.md`（历史会话通过真实 `session/load` 恢复）
  - `internal/crash/crash.go`（stderr → `grok_switch.log`）

---

## 12. 修订历史

| 日期 | 说明 |
|:---|:---|
| 2026-07-19 | 初稿：根据用户现场 `grok_switch.log` 溢出日志与代码路径整理问题与分层修复方案 |
| 2026-07-19 | 实现：新增 load 期间传输层 notification 预过滤、结构化错误、自动重启、只读历史 UI 与 10,000 条通知压力测试 |
