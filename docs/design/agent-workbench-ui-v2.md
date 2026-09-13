# Agent 工作台 UI v2:效率 · 呈现 · 功能 · 移动端

日期:2026-09-13 · 状态:已批准(对话内确认)

对标 Claude Code / Cursor 级 harness 的"手感细节",在现有工作台(项目侧栏 / 会话树 / 节点轨道 / 查找 / 计划与权限 bar / 富渲染)之上做四层整改。14 项,按依赖关系拆 4 个 PR 顺序合入。

## 背景:确认的缺陷

| 现象 | 根因 |
|---|---|
| 会话列表全是"未命名会话" | 前端首消息推导的标题只写内存,从未调 `/api/agent/session/rename` |
| 生成中发消息被拒 | `sendAgentMessage` busy 时 toast 拦截,无队列 |
| 切会话丢草稿 | 无草稿持久化 |
| token/上下文不可见 | `turnUsageHolder` 仅录制不广播(nativeagent_events.go 注释明确) |
| todo_list 只内联渲染 | `TodoStore` 快照无广播,无常驻面板 |
| 权限只有"本会话允许" | 引擎已有 `AddUserRule`(permissions.json 持久)但 UI/协议未暴露 |
| 无桌面通知 | `notify.Info` helper 存在但从未被 agent 事件调用 |

## 范围(PR 切分)

### PR1 · 服务端数据链路(纯 Go)

- `EventUsage` → 广播 `usage` WS 事件(input/output/reasoning tokens)
- `SessionMeta.Preview` 字段;`AppendRecord` 时按 user/assistant 文本写末条摘要
- 自动标题:native 持久化首条 user 记录时若 meta.Title 空 → 推导并写回
- `POST /api/agent/session/fork {session_id, upto_seq}`:复制 ≤seq 的 transcript 到新会话(native 限定,meta 记 forked_from)
- `GET /api/agent/session/export?id=` → transcript/history 渲染为 Markdown 下载(native 走 records,ACP 走 readChatHistory)
- `permission_response` 扩展 `scope` 字段:`"user"` → `AddUserRule(tool+"(*)", Allow)`;不带 scope 的 `remember` 维持会话语义(兼容)
- `todo_list` 工具结果时广播 `todos` 事件(TodoStore 快照)
- WS 新增 `client_visibility {hidden}`:服务端记录;页面不可见时 `permission_request`/`turn_end` 触发 `notify.Info`

### PR2 · 效率层(ui/app.js)

- 发送后若无标题 → 推导标题 POST rename(覆盖 ACP 会话;native 已由服务端兜底)
- 生成中发送 → 消息进排队 chip(composer 状态行上方):可编辑、× 取消、"停止并发送";turn 结束自动按序发送
- 草稿:`localStorage["draft:<sessionId>"]`,输入节流 300ms 落,发送清;新会话用 `draft:_new`
- 输入历史:空输入/光标首行按 ↑ 回忆已发消息,每会话环形 50 条
- 会话置顶:侧栏项右键/菜单 pin,localStorage 存 id 集合,排序置顶
- 失败回合内联"重试"按钮 → 复用 `regenerateLastAssistant`
- WS 连上后上报 `client_visibility`,`visibilitychange` 时同步

### PR3 · 信息呈现(ui/)

- 上下文栏加用量区:input/output/reasoning tokens + 上下文占用 meter(按 profile context 上限,近上限变色);native 限定,无数据时隐藏
- 会话列表项第二行:Preview 摘要 + 相对时间;活动会话"生成中"呼吸点
- 上下文栏"任务清单"区块:`todos` 事件实时渲染;切会话从最近 todo 工具调用回填
- 回合文件改动卡:turn 内聚合 write/edit 工具调用 → assistant 尾部"改动 N 文件",+/- 行数,点击跳转对应 diff
- 状态行遥测:`正在执行 <tool> · <目标> · <耗时> · ↓<tok> · Esc 中断`;工具卡片自带耗时;回合结束 assistant 角标 `12s · 4.1k tok`

### PR4 · 功能与视觉(ui/ + index.html)

- ⌘K 命令面板:新建会话/搜索跳转会话/停止/压缩/切面板/切模型/切访问/导出/分叉;↑↓+Enter
- 消息菜单"从此分叉"(native 会话显示)→ fork API → 切新会话;"导出 Markdown"入口(顶栏菜单/面板)
- composer 收紧:空载单行,autogrow 上限内收,chip 行与状态行合并
- 空态:有项目上下文时显示最近 3 会话一键续聊
- 移动端(≤820px):侧栏/上下文栏覆盖层化、chip 行横向滚动、node rail 隐藏、权限/计划 bar 全宽贴底

## 明确不做(YAGNI)

多会话并行(单引擎架构)、ACP 会话 fork(Grok CLI 存储不透明)、mid-turn 注入(WS 协议不支持)、文件级 checkpoint/undo(`restore_files` native 未接)、ACP 会话 preview(summary.json 无摘要,逐条读 chat_history 太贵)。

## 兼容性

- WS 协议只加字段/事件类型,旧客户端忽略未知事件,向后兼容
- `SessionMeta.Preview` 缺省空串,旧 meta.json 无需迁移
- fork 仅限 `nat-` 前缀会话;ACP 会话 UI 隐藏入口、API 返回 400
- ACP 引擎:rename sidecar / export / 标题回写 / pin / 草稿 / 排队 均可用;usage / todos / fork 仅 native
