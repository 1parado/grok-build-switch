# 内置生图 MCP 工具（generate_image）

## 目标

让 Grok Agent（以及任何支持 MCP 的 Harness）把 **生图** 作为**原生工具**使用：
模型自行判断何时生图、撰写提示词、选择模型与宽高比，生成结果（图片）直接返回
给模型继续对话——与内置的 bash/edit/web_search 等工具体验一致。

## 现状与设计选择

- grok_switch 的生图引擎（`internal/server/imagine_local.go`）走 grok.com 网页端
  WebSocket 协议，使用 `registrar/cookies` 下的浏览器 Cookie 自动轮询账号，与
  聊天模型 API（`xai_api_base_url`）完全独立。
- grok_switch 是 ACP 客户端，通过 `grok agent stdio` 连接 Grok Agent；ACP 的
  `NewSessionRequest` / `LoadSessionRequest` 支持 `mcpServers` 字段，Agent 会在
  会话内连接这些 MCP 服务器并自动发现其工具。这是暴露自定义工具的标准入口。
- 因此不修改 Grok CLI 本身，也不改动聊天 API：新增一个 **stdio MCP 服务器**，
  由 Agent 按配置拉起，把 `generate_image` 工具暴露给 Agent。

## 架构

```
grok_switch（主进程，HTTP 127.0.0.1:<port>）
  │
  ├── 启动时把 [mcp_servers.image_generator] 写入 config.toml（原生注册）
  │        （command = 自身 exe + "mcp"，env 携带 GROK_SWITCH_BASE_URL）
  │
  └── /api/imagine/generate（已有，ImagineEngine 账号池轮询）
        ▲
        │ HTTP（本机回环）
        │
grok CLI（TUI / grok agent stdio 会话）
  └── 原生加载 [mcp_servers] → spawn `grok_switch mcp`
        └── internal/mcpserver（标准 MCP stdio 服务器，暴露 generate_image 工具）
```

- 注册方式使用 **Grok CLI 原生的 `[mcp_servers.<name>]` 配置**（`~/.grok/config.toml`），
  Grok CLI 自己 spawn MCP 进程、握手、发现工具，对 TUI 与 Agent 会话都生效，
  无需在 ACP 客户端侧注入（`Bridge.SetMcpServers` 已移除）。
- 服务器名 `image_generator`（曾用名 `grok_switch_imagine` 会在启动时自动迁移
  清理，避免重复加载）。主进程每次启动调用 `config.EnsureMcpServerToFile`
  同步一次（幂等，重复段只保留一个），保证 `command` 指向最新的可执行文件；
  config 切换 profile 时 `[mcp_servers]` 作为未知段被原样保留。
- `tools/call` → MCP 子进程 POST 本机 `/api/imagine/generate` → 下载生成的图片
  → 以 MCP `image` content（base64）返回给模型，模型在对话中直接"看到"图片。
- 同一 MCP 服务器是标准 MCP stdio 实现，任何支持 MCP 的 Harness
  （Claude Desktop、Cursor、自研 Agent 等）都可直接配置使用。

## 工具定义

- 名称：`generate_image`
- 参数（JSON Schema）：
  - `prompt`（必填）：图片提示词
  - `model`（可选）：生图模式
    - `grok-imagine-image-lite`：快速
    - `grok-imagine-image`：标准（默认）
    - `grok-imagine-image-quality`：高清
  - `aspect_ratio`（可选）：`1:1` / `16:9` / `9:16` / `4:3` / `3:4` / `3:2` / `2:3` / `4:5` / `5:4` / `21:9` / `9:21`（默认 `1:1`）

三档模式与全部比例均经网页端 /imagine 协议实测可用；`quality` 走
`enable_pro`，`lite` 透传模型名由网页端以快速模式渲染，代理接管的
`size` 参数同样映射到上述比例。

## 安全

- MCP 子进程只与本机回环地址通信；`server.withAccess` 对回环请求总是放行，
  局域网配对鉴权不受影响。
- 图片仅保存在 `~/.grok_switch/imagine_outputs/`，通过 `/imagine-output/` 提供，
  与现有生图链路一致。

## 兼容性

- 注入走 Grok CLI 原生 `[mcp_servers]` 配置（stdio transport，所有 Agent 必须
  支持）；MCP 服务器本体是标准 stdio 实现，任何支持 MCP 的 Harness
  （Claude Desktop、Cursor、自研 Agent 等）均可直接配置同一命令使用。
- 诊断：`grok mcp list` / `grok mcp doctor image_generator` 可查看与
  验证服务器健康状态。

## 内置 image_gen 工具的代理接管

Grok CLI 自带的内置生图工具 `image_gen` 无法通过 config 或启动参数禁用
（`--disallowed-tools` 仅 headless 模式可用，`features` 无 image_gen 开关），
且模型在多个生图工具里倾向于优先调用它。它的请求格式为 OpenAI Images API：

```
POST <xai_api_base_url>/v1/images/generations
body: {"model":"grok-imagine-image","prompt":"...","n":1,"size":"1024x1024"}
```

对 Grok Auth 本地代理 profile，`xai_api_base_url` 就是 grok_switch 自己的
`/grok/v1` 反代（`handleGrokProxy`）。因此在该代理里对 `images/generations`
请求做了接管：

- 当本地账号池（`ImagineEngine`，即 `registrar/cookies` 账号）可用时，把该
  请求改为用账号池生图，并按 OpenAI Images API 格式返回
  （`data[0].url` + `b64_json`，图片存于 `imagine_outputs/`）。
- 视频模型（`*video*`）与无法解析的请求不接管，原样转发官方。
- 图片编辑（`image_edit`，POST `/images/edits`，需要参考图）当前引擎未实现
  编辑协议，代理层返回可读错误（`image_edit_unavailable`）引导模型改用
  `generate_image` 重新生成，避免 403 让模型困惑。引导文案必须**显式禁止
  重试 image_edit 与查找/传递参考图**——实测中模型在收到普通失败后会自行
  `bash find` 会话目录里的 `1.jpg` 找参考图并重复尝试，浪费多轮工具调用；
  明确指令后模型立即转向 `generate_image`。
- 已知隐患（实现参考图编辑前）：Grok CLI 会把生成的图片保存到各会话目录
  `~/.grok/sessions/<path>/images/1.jpg`，同一工作区多个历史会话都有同名
  `1.jpg`。将来若实现基于参考图的编辑，接收模型传来的文件路径时无法仅凭
  文件名区分会话归属，需结合会话 ID 校验路径。
- 普通聊天请求完全不受影响；其他 profile（第三方上游）的 `image_gen` 仍走
  各自配置的上游。
- 生成的图片除保存到 `imagine_outputs/` 外，还会额外复制一份到系统默认
  下载目录（`~/Downloads`，Windows 为 `%USERPROFILE%\Downloads`），响应中
  附带 `saved_to` 路径。
- 比例解析同时支持 `aspect_ratio`（Grok CLI image_gen 的真实字段，如
  `"16:9"`、`"auto"`）与 OpenAI 兼容的 `size`（像素或比例字符串）。

效果：模型照常调用内置 `image_gen` 工具，但实际生图由本地账号池完成，
无需官方订阅额度，对模型与用户完全透明。

## 两种生图通道

| 通道 | 触发方式 | 数据源 | 适用 |
|------|----------|--------|------|
| 内置 `image_gen` 代理接管 | 模型调用 `image_gen` 工具 | `/grok/v1` 代理 → 账号池 | Grok Auth 本地代理 profile 下的默认行为 |
| MCP `generate_image` 工具 | Grok CLI 原生 `[mcp_servers]` 加载 | `registrar/cookies` 账号池 | Grok CLI 会话及任何支持 MCP 的 Harness |

## OpenAI 兼容 Harness（API 层）工具注入

三方平台（如 Dify、自研 Agent、各类 OpenAI 兼容前端）通常只调用
`/grok/v1/chat/completions`，**没有 MCP 客户端能力**，且不会主动声明生图
工具——此时模型只会文本回复"没有生图能力"。为此在 `handleGrokProxy` 对
`chat/completions` 做工具注入：

1. 请求到达时，若本地账号池可用，自动把 `generate_image` 工具声明
   （OpenAI function calling 标准格式，含模式/比例枚举）合并进请求的
   `tools`（已有同名工具则不重复注入；`tool_choice` 缺省 `auto`）。
2. 上游响应若含 `generate_image` 的 `tool_calls`，代理**代为执行**生图
   （账号池），并把结果写成最终 assistant 消息：`content` 内嵌 base64
   图片（`![generated image](data:image/jpeg;base64,...)`）+ 文本说明，
   附带非标准 `images` 数组（`url` + `b64_json`）。
3. 调用方自己声明的其它工具调用（如 `web_search`）**原样保留**在响应
   的 `tool_calls` 中，按标准 function calling 流程继续；生图结果对
   调用方完全透明。
4. 普通对话（模型未发起生图调用）与流式请求原样透传，不注入不改写。

实测：纯 API 请求（不带任何 tools）"Create an image of Superman" → 模型
自动调用 `generate_image` → 返回 960x960 图片（base64 内嵌）；带自有
工具 + 生图意图的请求 → 自有 `tool_calls` 保留、图片同时返回。

## 生图能力全局开关

旧的"独立生图供应商"表单（BaseURL / API Key / Backend / 模型 / 拉取模型）
随账号池接管而废弃，替换为**全局开关**（`settings.image_gen_enabled`，
默认开启，供应商表单与设置页均可切换）：

- **开启**：注册 `[mcp_servers.image_generator]`（Grok CLI 原生 MCP）、代理
  接管内置 image_gen / image_edit、chat/completions 注入 generate_image
  工具——三条通道全部可用，走账号池。
- **关闭**：移除 `[mcp_servers]` 注册、代理不接管、不注入任何工具声明——
  模型回到无生图工具的原始状态（不注入任何"没有生图"说明，不做硬约束）。
- 切换**即时生效**（`/api/settings` 更新时同步 config.toml 与代理行为），
  无需重启；对所有供应商生效（生图是全局能力，与 profile 无关）。
- 供应商表单的生图区域保留账号池状态展示与「测试生图」（走
  `/api/imagine/generate` 账号池）。

旧的 profile `image_generation` 字段（BaseURL/Key 等）不再由 UI 编辑，
结构保留以兼容旧数据导入。
