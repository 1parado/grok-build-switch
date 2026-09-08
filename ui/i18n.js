/* grok_switch UI i18n: zh (source) <-> en.
 *
 * The codebase keeps Chinese as the source language. This module translates
 * the static markup once at startup (data-i18n* attributes in index.html)
 * and exposes t()/tf() for strings that app.js renders dynamically. A
 * MutationObserver re-translates nodes that app.js rewrites from Chinese
 * literals while English is active, so re-rendered views stay in sync.
 *
 * Language choice is a per-device display preference stored in localStorage;
 * it never touches profiles, config.toml or server settings.
 */
(function () {
  "use strict";

  const LANG_KEY = "gs_lang_v1";
  const SUPPORTED = ["zh", "en"];

  // ---------------------------------------------------------------------------
  // Dictionary. Keys are the exact Chinese source strings that appear in the
  // markup / app.js; values are the English rendering.
  // ---------------------------------------------------------------------------
  const DICT = {
    "配置中没有启用模型": "No enabled models in config",
    "自动（配置默认）": "Auto (config default)",
    "已加载": "Loaded",
    "切换供应商时会自动创建。暂无历史备份。": "Created automatically on provider switch. No backups yet.",
    "暂无项目。点击 ＋ 添加工作目录后，会话会归入对应项目。": "No projects. Click ＋ to add a working directory — sessions will be grouped under it.",
    "没有匹配的账号。可一次选择多个 CPA xai-*.json；也支持 Grok CLI auth.json。": "No matching accounts. You can pick multiple CPA xai-*.json files; Grok CLI auth.json is also supported.",
    "未找到局域网地址": "No LAN address found",
    "无匹配文件": "No matching files",
    "无匹配项": "No matches",
    "试试 /plan /compact /skills": "Try /plan /compact /skills",
    "先拉取模型": "Fetch models first",
    "没有匹配": "No match",
    "已置顶": "Pinned",
    "只读": "Read-only",
    "测试连通": "Test",
    "巡检": "inspected",
    "导出": "Export",
    "高": "High",
    "这是 grok": "This is the",
    "当前生效": "live",
    "的唯一配置文件（不是「每个供应商各一份」）。各供应商档案在列表里；启用某个供应商后，其 URL/Key/模型会写进此文件。": " config file grok actually reads (not one per provider). Provider profiles live in the list; activating one writes its URL/key/models here.",
    "尚未导入号池账号。": "No pool accounts imported yet.",
    "切换会话列表": "Toggle session list",
    "斜杠命令与 Skills": "Slash commands & Skills",
    "在": "Install a skill in",
    "或": "or",
    "安装 Skill 后会出现在这里。": "and it will appear here.",
    "· Skills 插入为": "· insert skills as",
    "Quality（高清）": "Quality (HD)",
    "Speed（快速）": "Speed (Fast)",
    "https://你的-临时邮箱-API 地址": "https://your-temp-mailbox-API",
    "邮箱----密码----ClientID----refresh_token": "email----password----ClientID----refresh_token",
    "一行一个，例如：\nhttp://127.0.0.1:7897\nhttp://user:pass@1.2.3.4:8080\nhost:port:user:pass": "One per line, e.g.:\nhttp://127.0.0.1:7897\nhttp://user:pass@1.2.3.4:8080\nhost:port:user:pass",
    "0 个账号": "0 accounts",
    "← 返回": "← Back",
    "登录方式": "Sign-in methods",
    "聊天": "Chat",
    "添加供应商": "Add provider",
    "导入配置": "Import config",
    "账号": "Accounts",
    "生图": "Imagine",
    "设置": "Settings",
    "账号管理": "Accounts",
    "切换主题": "Toggle theme",
    "主题：亮色（点击切换为跟随系统）": "Theme: light (click for system)",
    "主题：暗色（点击切换为跟随系统）": "Theme: dark (click for system)",
    "GitHub 仓库": "GitHub repository",
    "界面语言": "UI language",
    "Switch language": "切换语言",
    "最新版已发布，可直接下载更新。": "A new version is out. Download to update.",
    "关闭更新提示": "Dismiss update notice",
    "配置与当前供应商不一致": "Config differs from active provider",
    "config.toml 内容与当前启用的供应商不匹配": "config.toml no longer matches the active provider",
    "用供应商覆盖文件": "Overwrite with provider",
    "编辑 config.toml": "Edit config.toml",
    "在官方账号与 API 供应商之间切换，拖动调整顺序": "Switch between the official account and API providers; drag to reorder",
    "搜索登录方式…": "Search sign-in methods…",
    "布局": "Layout",
    "卡片": "Cards",
    "列表": "List",
    "还没有供应商": "No providers yet",
    "从当前 config.toml 导入，或添加一个新的上游配置。": "Import from the current config.toml, or add a new upstream.",
    "导入现有配置": "Import existing config",
    "没有匹配的供应商": "No matching providers",
    "名称、类型与 API Key 即可开始": "Name, type and API key are enough to start",
    "名称": "Name",
    "例如 OpenRouter": "e.g. OpenRouter",
    "类型模板": "Template",
    "服务地址": "Base URL",
    "显示": "Show",
    "隐藏": "Hide",
    "粘贴 API Key": "Paste API key",
    "连接与模型（可选）": "Connection & models (optional)",
    "测试上游连接": "Test connection",
    "拉取模型列表": "Fetch models",
    "保护隐私安全": "Privacy guard",
    "拉取列表后点 chip 启用；不会自动设置默认模型。每个已启用模型可单独测试连通。": "After fetching, click a chip to enable a model; the default model is not set automatically. Each enabled model can be tested individually.",
    "默认模型": "Default model",
    "（请先启用模型）": "(enable a model first)",
    "协议格式": "Upstream format",
    "联网搜索模型": "Web search model",
    "（可选）": "(optional)",
    "explore 子代理模型": "explore subagent model",
    "plan 子代理模型": "plan subagent model",
    "（继承主模型）": "(inherit main model)",
    "写入 [subagents.models] explore": "Written to [subagents.models] explore",
    "写入 [subagents.models] plan": "Written to [subagents.models] plan",
    "生图能力": "Image generation",
    "全局开关：开启后模型可通过 MCP 工具（generate_image）与内置 image_gen 生成图片，走本机账号池（无需官方订阅）；关闭后模型回到无生图能力的原始状态。对所有供应商生效。": "Global switch: when enabled, models can generate images via the MCP tool (generate_image) and the built-in image_gen, using the local account pool (no official subscription needed); when disabled, models return to their original state without image generation. Applies to all providers.",
    "启用生图": "Enable image generation",
    "测试生图": "Test image generation",
    "搜索上游模型…": "Search upstream models…",
    "尚未拉取模型。点 chip 启用后，会出现在上方下拉框中。": "No models fetched yet. Enabled chips appear in the dropdowns above.",
    "已启用模型": "Enabled models",
    "高级字段": "Advanced fields",
    "手动添加": "Add manually",
    "更多操作": "More actions",
    "复制": "Copy",
    "导出 JSON": "Export JSON",
    "导入 JSON": "Import JSON",
    "从 config 导入": "Import from config",
    "清空": "Clear",
    "config.toml（预览）": "config.toml (preview)",
    "刷新预览": "Refresh preview",
    "完整 config.toml": "Full config.toml",
    "填写名称、地址、Key 并启用模型后，点刷新预览": "Fill in name, URL and key, enable models, then refresh the preview",
    "取消": "Cancel",
    "保存": "Save",
    "保存并启用": "Save & activate",
    "已启用": "Active",
    "启用": "Activate",
    "OAuth 官方模型": "Official OAuth models",
    "已登录 grok.com": "Signed in to grok.com",
    "尚未登录": "Not signed in",
    "未设默认模型": "No default model",
    "上移": "Move up",
    "下移": "Move down",
    "编辑": "Edit",
    "删除": "Delete",
    "搜索会话": "Search sessions",
    "搜索": "Search",
    "隐藏会话列表": "Hide session list",
    "隐藏侧栏": "Hide sidebar",
    "新建会话": "New chat",
    "搜索对话、目录、模型": "Search chats, directories, models",
    "项目": "Projects",
    "添加项目（工作空间）": "Add project (workspace)",
    "暂无项目，点击 ＋ 添加工作目录": "No projects yet. Click ＋ to add a workspace",
    "其他工作空间": "Other workspaces",
    "无未登记工作空间": "No unregistered workspaces",
    "← 返回供应商": "← Back to providers",
    "调整会话列表宽度": "Resize session list",
    "拖动调整宽度，双击恢复默认": "Drag to resize, double-click to reset",
    "Grok Build AI 工作台": "Grok Build AI workspace",
    "历史会话": "Chat history",
    "项目与会话": "Projects and sessions",
    "新对话": "New chat",
    "尚未选择工作目录": "No working directory selected",
    "Agent 状态": "Agent status",
    "未启动": "Not started",
    "打开位置": "Open location",
    "在资源管理器中打开当前工作目录": "Reveal the current working directory",
    "选择工作目录": "Choose working directory",
    "设置聊天背景": "Chat background",
    "聊天背景": "Chat background",
    "打开会话信息": "Open session info",
    "会话信息": "Session info",
    "对话节点": "Chat outline",
    "在当前会话中查找": "Find in this chat",
    "在当前会话中查找…": "Find in this chat…",
    "上一个匹配": "Previous match",
    "上一个 (Shift+Enter)": "Previous (Shift+Enter)",
    "下一个匹配": "Next match",
    "下一个 (Enter)": "Next (Enter)",
    "关闭查找": "Close find",
    "关闭 (Esc)": "Close (Esc)",
    "回到最新": "Jump to latest",
    "回到最新 · Esc 之外": "Jump to latest",
    "↓ 最新": "↓ Latest",
    "开始对话": "Start a conversation",
    "在右侧填写工作目录并启动 Agent，或从左侧打开历史会话。": "Pick a working directory on the right and start the Agent, or open a past session from the left.",
    "启动 Agent": "Start Agent",
    "正在恢复引擎上下文…": "Restoring engine context…",
    "执行计划": "Plan",
    "等待确认": "Waiting for confirmation",
    "忽略": "Dismiss",
    "请求修改": "Request changes",
    "批准并执行": "Approve & run",
    "工具权限": "Tool permission",
    "拒绝": "Deny",
    "本会话允许": "Allow for session",
    "允许一次": "Allow once",
    "消息": "Message",
    "随心输入…  输入 / 打开命令与 Skills": "Type anything…  / for commands & Skills",
    "命令 / Skills": "Commands / Skills",
    "↑↓ 选择 · Enter 确认 · Esc 关闭": "↑↓ select · Enter confirm · Esc close",
    "工作区文件": "Workspace files",
    "点击插入 @路径": "Click to insert @path",
    "筛选文件名…": "Filter files…",
    "添加附件": "Add attachment",
    "引用工作区文件": "Reference workspace file",
    "@ 引用文件": "@ reference file",
    "当前项目 / 选择目录": "Current project / choose directory",
    "选择项目": "Choose project",
    "模型": "Model",
    "模型…": "Model…",
    "推理强度": "Reasoning effort",
    "强度…": "Effort…",
    "工具权限：每次确认 / 本会话自动 / 完全访问": "Tool access: ask every time / auto for session / full access",
    "访问方式": "Access mode",
    "每次确认": "Ask every time",
    "本会话自动": "Auto this session",
    "完全访问": "Full access",
    "停止生成": "Stop generating",
    "发送": "Send",
    "调整会话信息宽度": "Resize session info",
    "关闭会话信息": "Close session info",
    "当前模型": "Current model",
    "会话 ID": "Session ID",
    "自动允许 Grok Build 执行工具（进程级 YOLO）": "Auto-allow Grok Build to run tools (process-wide YOLO)",
    "自动批准工具（YOLO）": "Auto-approve tools (YOLO)",
    "本会话自动允许工具，无需重启进程": "Auto-allow tools for this session without restarting",
    "本会话自动批准": "Auto-approve this session",
    "停止": "Stop",
    "本轮活动": "Activity this turn",
    "暂无工具活动": "No tool activity yet",
    "正在输入": "typing",
    "正在连接": "connecting",
    "运行中": "running",
    "已完成": "done",
    "已停止": "stopped",
    "出错": "error",
    "空闲": "idle",
    "已断开": "disconnected",
    "未运行": "not running",
    "思考中": "Thinking",
    "已取消": "Cancelled",
    "重试中": "Retrying",
    "连接已断开，正在重连…": "Connection lost, reconnecting…",
    "正在生成回复…": "Generating response…",
    "等待模型响应…": "Waiting for model…",
    "Enter 发送 · Shift+Enter 换行": "Enter to send · Shift+Enter for newline",
    "会话": "Session",
    "今天": "Today",
    "昨天": "Yesterday",
    "无标题会话": "Untitled chat",
    "历史会话加载失败": "Failed to load chat history",
    "附件": "attachment",
    "文件": "File",
    "（已截断）": " (truncated)",
    "权限请求": "Permission request",
    "执行计划待确认": "Plan awaiting approval",
    "压缩上下文": "Compact context",
    "上下文已压缩": "Context compacted",
    "会话已重置": "Session reset",
    "命令不存在": "Unknown command",
    "工作目录": "Working directory",
    "信任项目": "Trust project",
    "创建会话前需要信任该目录，以允许 Agent 读写。": "This directory must be trusted before creating a session so the Agent may read and write.",
    "首次打开需要信任该目录，以允许 Agent 读写。": "First open requires trusting this directory so the Agent may read and write.",
    "点击预览": "Click to preview",
    "复制代码": "Copy code",
    "已复制": "Copied",
    "复制失败": "Copy failed",
    "重新发送": "Resend",
    "删除消息": "Delete message",
    "编辑消息": "Edit message",
    "保存并重新发送": "Save & resend",
    "取消编辑": "Cancel edit",
    "查看思考过程": "Show reasoning",
    "收起思考过程": "Hide reasoning",
    "思考过程": "Reasoning",
    "工具调用": "Tool call",
    "参数": "Arguments",
    "结果": "Result",
    "错误": "Error",
    "展开": "Expand",
    "收起": "Collapse",
    "加载更早": "Load earlier",
    "会话已过期或不存在": "Session expired or missing",
    "计划": "Plan",
    "批准": "Approve",
    "会话已结束": "Session ended",
    "浏览本机 Agent Skills，复制路径或卸载用户安装的技能": "Browse local Agent Skills, copy paths or uninstall user-installed skills",
    "搜索名称或路径…": "Search name or path…",
    "刷新": "Refresh",
    "用户": "User",
    "内置": "Built-in",
    "聊天中输入": "Type",
    "可快速插入技能；内置技能仅可浏览，不可删除。": "in chat to insert a skill quickly; built-in skills are browse-only and cannot be removed.",
    "还没有 Skills": "No Skills yet",
    "没有匹配的 Skills": "No matching Skills",
    "正在加载 Skills…": "Loading Skills…",
    "返回供应商列表": "Back to providers",
    "复制路径": "Copy path",
    "已复制路径": "Path copied",
    "自启与外观": "Startup & appearance",
    "Agent 引擎": "Agent engine",
    "Grok CLI（ACP 桥，默认）": "Grok CLI (ACP bridge, default)",
    "内置引擎（无需安装 Grok CLI）": "Built-in engine (no Grok CLI needed)",
    "切换引擎后重启应用生效；已开启生图时两种引擎均可生图（走账号池）。内置引擎还在灰度阶段，遇到问题可随时切回。": "Engine changes apply after an app restart; image generation works on both engines when enabled (via the account pool). The built-in engine is still in preview — switch back anytime if you hit issues.",
    "开机自启": "Launch at login",
    "自启时静默": "Start silently",
    "启动时打开面板": "Open panel on launch",
    "允许同一局域网的手机访问": "Allow phone access on the same LAN",
    "关闭后会立即注销所有已配对手机。": "Turning this off immediately signs out all paired phones.",
    "端口": "Port",
    "保存设置": "Save settings",
    "账号、号池与注册机": "Accounts, pool & registrar",
    "Grok Auth 导入、号池巡检和自动注册已移至独立的「账号」页面，支持搜索、分类筛选与大规模账号管理。": "Grok Auth import, pool inspection and auto-registration moved to the dedicated Accounts page, with search, category filters and large-scale account management.",
    "前往账号管理": "Go to Accounts",
    "手机连接": "Phone pairing",
    "手机与电脑连接同一网络后，扫描二维码打开供应商管理页面。": "With the phone on the same network, scan the QR code to open the provider page.",
    "未开启": "Off",
    "已开启": "On",
    "仅允许本机访问": "Localhost only",
    "勾选上方“允许同一局域网的手机访问”并保存设置后，才会生成配对二维码。": "Enable “Allow phone access on the same LAN” above and save to generate a pairing QR code.",
    "一次性二维码": "One-time QR code",
    "电脑局域网地址": "Computer LAN address",
    "手机配对地址": "Phone pairing URL",
    "一次性配对码": "One-time pairing code",
    "重新生成二维码": "Regenerate QR code",
    "首次连接请检查 Windows 防火墙": "Check the Windows firewall on first connect",
    "只允许 grok_switch 通过“专用网络”。不要勾选公共网络。": "Only allow grok_switch on “Private networks”. Do not check Public.",
    "加载中…": "Loading…",
    "重新加载": "Reload",
    "保存到文件": "Save to file",
    "这是 grok 当前生效的唯一配置文件（不是「每个供应商各一份」）。各供应商档案在列表里；启用某个供应商后，其 URL/Key/模型会写进此文件。": "This is the only live config grok reads (not one file per provider). Provider profiles live in the list; activating one writes its URL/key/models here.",
    "config.toml 内容": "config.toml content",
    "高级 · 备份与恢复": "Advanced · backup & restore",
    "切换供应商或保存 config 时会自动备份。备份含 API Key，请勿外传。": "Switching providers or saving the config creates an automatic backup. Backups contain API keys — keep them private.",
    "暂无备份": "No backups",
    "还原": "Restore",
    "下载": "Download",
    "xAI OAuth 号池：导入、注册、巡检与大规模账号管理": "xAI OAuth pool: import, register, inspect and manage accounts at scale",
    "统一 xAI OAuth 号池入口：导入后自动进入下方巡检池，不再依赖 CPA。": "Unified xAI OAuth pool entry: imported accounts join the inspection pool below; CPA is no longer required.",
    "未配置": "Not configured",
    "已配置": "Configured",
    "本地 Base URL": "Local base URL",
    "本地 API Key": "Local API key",
    "导入 auth JSON": "Import auth JSON",
    "选择目录导入号池": "Import pool from folder",
    "启用本地 Grok": "Activate local Grok",
    "刷新 token": "Refresh token",
    "支持 CPA 的": "Supports CPA",
    "和 Grok CLI 的": "and Grok CLI",
    "。单文件和目录导入都进入同一个自动巡检号池；页面不会把 xAI access token 当作 API Key 暴露。": ". Single-file and folder imports join the same auto-inspected pool; the page never exposes xAI access tokens as API keys.",
    "Grok 注册机": "Grok registrar",
    "默认使用 Cloudflare 临时邮箱。一般只需填写「网络代理」和「API Base」，成功后账号会自动铸造并进入号池。": "Uses a Cloudflare temp mailbox by default. Usually you only need to fill in “Proxy” and “API Base”; on success the account is minted into the pool automatically.",
    "准备环境": "Prepare",
    "：本机安装 Chrome 或 Edge；开启可访问外网的代理（如 Clash，端口与下方一致）。": ": install Chrome or Edge; run a proxy with internet access (e.g. Clash, same port as below).",
    "填写两项": "Fill two fields",
    "：下方填写": ": fill in",
    "与": "and",
    "。单代理时浏览器会": ". With a single proxy the browser",
    "自动串行": "runs serially",
    "（避免多开抢同一端口导致 CONNECTION_CLOSED）。": " (to avoid multiple instances fighting over one port and causing CONNECTION_CLOSED).",
    "开始注册": "Start registration",
    "：点「开始注册」；弹出浏览器若出现人机验证请手动勾选。完成后自动进入号池。": ": click “Start registration”; if a captcha appears in the browser, solve it manually. Accounts join the pool automatically when done.",
    "网络代理（支持多行代理池）": "Proxy (multi-line pool supported)",
    "一行一个，例如：": "One per line, e.g.:",
    "留空=直连。多条时按下方「代理策略」轮换；失败的代理会短暂冷却。": "Empty = direct connection. Multiple entries rotate per the “Proxy strategy” below; failed proxies cool down briefly.",
    "临时邮箱 API Base": "Temp mailbox API base",
    "认证默认「无」；若你的服务需要 Key，请在下方高级选项中填写。": "Auth defaults to “none”; if your service needs a key, set it in Advanced options below.",
    "当前不是 Cloudflare 模式，请在「高级选项」中配置对应邮箱服务。": "Not in Cloudflare mode — configure the mailbox service in “Advanced options”.",
    "注册数量": "Count",
    "高级选项（注册引擎、代理策略、浏览器、邮箱类型等）": "Advanced options (engine, proxy strategy, browser, mailbox type…)",
    "注册引擎": "Engine",
    "浏览器全流程（默认）": "Full browser flow (default)",
    "协议优先（邮箱走 gRPC-web，失败回退浏览器）": "Protocol-first (mailbox via gRPC-web, browser fallback)",
    "协议邮箱 + 浏览器资料（不整页回退）": "Protocol mailbox + browser profile (no full-page fallback)",
    "自动（同协议优先）": "Auto (same as protocol-first)",
    "代理策略": "Proxy strategy",
    "轮询 round_robin": "Round robin",
    "随机 random": "Random",
    "按序号粘性 sticky": "Sticky by index",
    "代理冷却（秒）": "Proxy cooldown (s)",
    "Clash 控制器 URL（单代理多线程专用）": "Clash controller URL (single proxy, multi-thread)",
    "http://127.0.0.1:9090（留空=不启用节点轮换）": "http://127.0.0.1:9090 (empty = no node rotation)",
    "Clash 选择器组名": "Clash selector group",
    "如：🔰 选择节点（FlClash 默认）": "e.g. 🔰 Select Node (FlClash default)",
    "🔍 自动检测": "🔍 Auto detect",
    "填了上面两项后，每个账号注册前会自动把该组切到不同节点（需 FlClash/Clash 已开启外部控制器）。单端口也能让连续注册走不同出口 IP。点「自动检测」可自动填入。": "With both fields set, the group switches to a different node before each registration (requires FlClash/Clash external controller). A single port can still rotate exit IPs across runs. “Auto detect” fills both in.",
    "浏览器模式": "Browser mode",
    "可见浏览器（推荐）": "Visible browser (recommended)",
    "自动（可见优先）": "Auto (visible preferred)",
    "仅无窗口（易被 403）": "Headless only (prone to 403)",
    "浏览器路径（可选）": "Browser path (optional)",
    "留空自动查找 Chrome / Edge": "Empty = auto-detect Chrome / Edge",
    "邮箱服务": "Mailbox service",
    "Cloudflare 临时邮箱（推荐）": "Cloudflare temp mailbox (recommended)",
    "Hotmail 凭证": "Hotmail credentials",
    "单邮箱最大地址数": "Max aliases per mailbox",
    "CloudMail URL": "CloudMail URL",
    "管理员邮箱": "Admin email",
    "管理员密码": "Admin password",
    "Catch-all 域名": "Catch-all domains",
    "API Key（可选）": "API key (optional)",
    "认证方式": "Auth mode",
    "无认证": "No auth",
    "URL key 参数": "URL key param",
    "指定域名（可选，逗号分隔）": "Specific domains (optional, comma-separated)",
    "域名接口": "Domains endpoint",
    "创建邮箱接口": "Create-mailbox endpoint",
    "Token 接口": "Token endpoint",
    "邮件列表接口": "Messages endpoint",
    "GPTMail API Base": "GPTMail API base",
    "YYDS Mail API Base": "YYDS Mail API base",
    "固定域名（可选，留空自动分配）": "Fixed domain (optional, auto-assigned if empty)",
    "并发数": "Workers",
    "邮件超时（秒）": "Mail timeout (s)",
    "SSO 协议铸造优先": "Prefer SSO protocol minting",
    "协议失败不回退授权页": "No auth-page fallback on protocol failure",
    "检测环境": "Check environment",
    "等待任务": "Waiting for tasks",
    "尚未运行注册任务。": "No registration run yet.",
    "Grok 号池自动巡检": "Grok pool auto-inspection",
    "多账号轮换与定时健康检查；明确坏号会自动退出代理可用集合，但不会被自动删除。": "Multi-account rotation with scheduled health checks; confirmed bad accounts leave the proxy set automatically but are never auto-deleted.",
    "自动巡检": "Auto inspect",
    "间隔（分钟）": "Interval (min)",
    "网络代理（巡检、token 刷新、铸造和号池转发共用）": "Proxy (shared by inspection, token refresh, minting and pool forwarding)",
    "例如 http://127.0.0.1:7890 或 socks5://127.0.0.1:1080": "e.g. http://127.0.0.1:7890 or socks5://127.0.0.1:1080",
    "CPA 认证目录（号池导入 / 铸造输出 / 热加载）": "CPA auth directory (pool import / mint output / hot reload)",
    "默认 %USERPROFILE%\\.grok_switch\\cpa_auths": "Default %USERPROFILE%\\.grok_switch\\cpa_auths",
    "目录热加载（监视 CPA 目录新文件）": "Directory hot reload (watch CPA dir for new files)",
    "递归子目录": "Include subdirectories",
    "保存号池设置": "Save pool settings",
    "认证目录加载中…": "Loading auth directory…",
    "从认证目录导入": "Import from auth dir",
    "打开认证目录": "Open auth dir",
    "从路径导入…": "Import from path…",
    "CPA 设备授权": "CPA device authorization",
    "Go 服务负责 device-auth、token 轮询和 CPA 文件写入；系统浏览器只用于登录、Turnstile 与「允许」。此流程不会创建新的 xAI 账号。": "The Go service handles device-auth, token polling and CPA file writing; the system browser is only used for sign-in, Turnstile and “Allow”. This flow does not create a new xAI account.",
    "备注邮箱（可选，写入文件名）": "Note email (optional, used in file name)",
    "例如 user@example.com": "e.g. user@example.com",
    "开始铸造": "Start minting",
    "打开验证页": "Open verification page",
    "尚未开始铸造。": "Minting not started.",
    "用户码": "User code",
    "验证链接": "Verification link",
    "号池 Base URL": "Pool base URL",
    "号池 API Key": "Pool API key",
    "批量导入 JSON": "Batch import JSON",
    "选择目录导入": "Import folder",
    "立即巡检": "Inspect now",
    "逐个重新登录全部号池账号并重新铸造 CPA（需密码保存在 registrar 账本 accounts_cli.txt）": "Re-login every pooled account and re-mint CPA (requires passwords stored in registrar ledger accounts_cli.txt)",
    "批量刷新": "Batch refresh",
    "停止巡检": "Stop inspection",
    "启用号池": "Activate pool",
    "批量禁用异常": "Disable abnormal",
    "批量删除异常": "Delete abnormal",
    "批量导出 JSON": "Batch export JSON",
    "「从认证目录导入」使用上方配置的本机路径；「选择目录导入」通过浏览器选取（不写回路径）。铸造产出与热加载共用 CPA 认证目录。原文件不会被移动。": "“Import from auth dir” uses the local path configured above; “Import folder” picks via the browser (path not saved). Mint output and hot reload share the CPA auth directory. Original files are never moved.",
    "搜索邮箱 / 文件名 / ID": "Search email / file name / ID",
    "账号分类筛选": "Filter accounts by category",
    "排序": "Sort",
    "最近巡检": "Last inspected",
    "导入时间": "Import time",
    "邮箱": "Email",
    "Token 有效期": "Token expiry",
    "加载更多": "Load more",
    "全部": "All",
    "额度用尽": "Quota exhausted",
    "权限被拒": "Permission denied",
    "需重新登录": "Re-auth required",
    "已禁用": "Disabled",
    "禁用": "Disable",
    "启用账号": "Enable",
    "可用": "available",
    "个账号": "accounts",
    "基于 grok.com /imagine 协议，多账号并行生成；后台任务不受切换页面/刷新影响": "Based on the grok.com /imagine protocol with parallel multi-account generation; background jobs survive page switches and refreshes",
    "提示词": "Prompt",
    "描述你想生成的画面…": "Describe the image you want…",
    "比例": "Aspect ratio",
    "张数": "Count",
    "1 张": "1",
    "2 张": "2",
    "4 张 · 抽卡": "4 · gacha",
    "6 张 · 抽卡": "6 · gacha",
    "生成图片": "Generate",
    "清空画廊": "Clear gallery",
    "还没有图片。输入提示词，点击「生成图片」试试。": "No images yet. Enter a prompt and hit “Generate”.",
    "背景只保存在当前设备，不会进入会话或同步账号。": "Backgrounds stay on this device — never saved into sessions or synced to accounts.",
    "关闭聊天背景设置": "Close background settings",
    "背景主题": "Background theme",
    "纯净": "Pure",
    "原始工作台": "Original workspace",
    "霜蓝": "Frost",
    "清醒、低干扰": "Crisp, low distraction",
    "深空": "Deep space",
    "夜间专注": "Night focus",
    "余晖": "Afterglow",
    "柔暖、长阅读": "Soft, warm, long reads",
    "选择本地图片": "Pick a local image",
    "遮罩强度": "Shade",
    "背景模糊": "Blur",
    "水平焦点": "Horizontal focus",
    "垂直焦点": "Vertical focus",
    "选择图片": "Choose image",
    "移除自定义图": "Remove custom image",
    "完成": "Done",
    "支持 PNG、JPEG、WebP，最大 16 MB。图片会在本机压缩后保存；建议使用不含文字和界面元素的 16:9 背景。": "PNG, JPEG or WebP up to 16 MB. Images are compressed and stored locally; a 16:9 background without text or UI elements works best.",
    "确定": "OK",
    "确认": "Confirm",
    "提示": "Notice",
    "警告": "Warning",
    "成功": "Success",
    "未知错误": "Unknown error",
    "未知原因": "Unknown reason",
    "操作失败": "Operation failed",
    "网络错误": "Network error",
    "请求失败": "Request failed",
    "正在处理…": "Working…",
    "请稍候": "Please wait",
    "是": "Yes",
    "否": "No",
    "继续": "Continue",
    "返回": "Back",
    "输入": "Input",
    "请输入": "Please enter",
    "必填": "Required",
    "名称不能为空": "Name is required",
    "服务地址不能为空": "Base URL is required",
    "此操作不可恢复。": "This cannot be undone.",
    "此操作不可撤销。": "This cannot be undone.",
    "路径: ": "Path: ",
    "个": "",
    "个自动备份": "backups",
    "个会话": "sessions",
    "个可用账号": "available",
    "已复制到剪贴板": "Copied to clipboard",
    "已复制 Base URL": "Base URL copied",
    "已复制 API Key": "API key copied",
    "已复制配对地址": "Pairing URL copied",
    "设置已保存": "Settings saved",
    "设置保存失败": "Failed to save settings",
    "供应商已保存": "Provider saved",
    "供应商已删除": "Provider deleted",
    "已恢复默认会话名": "Default session name restored",
    "本地历史将被移除，且不可恢复。": "Local history will be removed permanently.",
    "config 已保存": "config saved",
    "config 已重新加载": "config reloaded",
    "备份已还原": "Backup restored",
    "导入成功": "Import succeeded",
    "导入失败": "Import failed",
    "导出成功": "Export succeeded",
    "已启用，新开 grok 会话生效": "Activated; new grok sessions will use it",
    "模板已应用，请自行启用模型": "Template applied; enable models yourself",
    "未保存": "unsaved",
    "尚无对话": "No messages yet",
    "0 个": "0",
    "<span>任务清单</span>": "<span>Task list</span>",
    "<span>暂无工具活动</span>": "<span>No tool activity yet</span>",
    "Agent 尚未就绪，请先启动": "Agent is not ready — start it first",
    "Agent 未就绪": "Agent not ready",
    "Base URL 已复制": "Base URL copied",
    "CPA xAI 凭据": "CPA xAI credential",
    "CPA 凭据已写入并导入号池": "CPA credential written and imported into the pool",
    "CPA 铸造失败": "CPA minting failed",
    "Grok Agent 出错": "Grok Agent error",
    "Grok Auth（本地代理）": "Grok Auth (local proxy)",
    "Grok auth 已删除": "Grok auth deleted",
    "Grok auth 已导入统一号池，已进入自动巡检": "Grok auth imported into the unified pool with auto-inspection",
    "Grok 生成的图片": "Image generated by Grok",
    "JSON 解析失败": "JSON parse failed",
    "OpenAI 兼容": "OpenAI compatible",
    "Plan 模式": "Plan mode",
    "[Cloudflare 人机验证]": "[Cloudflare captcha]",
    "[提交邮箱]": "[Submit email]",
    "[资料页 Turnstile]": "[Profile page Turnstile]",
    "config.toml 已保存（已自动备份）": "config.toml saved (auto-backup created)",
    "xAI token 已刷新": "xAI token refreshed",
    "❌ 检测失败：": "❌ Detection failed: ",
    "上次会话": "Last session",
    "上游已恢复": "Upstream recovered",
    "上游模型 ID": "Upstream model ID",
    "上游请求失败": "Upstream request failed",
    "上游重试中": "Retrying upstream",
    "上游重试已耗尽": "Upstream retries exhausted",
    "下一个更高版本仍会提醒。": "You'll still be notified about the next newer version.",
    "与供应商服务地址保持一致": "Same as the provider base URL",
    "中": "Medium",
    "为从 config.toml 导入的配置起一个名称：": "Name the config imported from config.toml:",
    "主题：亮色（点击切换为暗色）": "Theme: light (click for dark)",
    "主题：暗色（点击切换为亮色）": "Theme: dark (click for light)",
    "主题：跟随系统（点击切换为亮色）": "Theme: system (click for light)",
    "仅截断下方界面气泡；引擎仍保留原对话上下文": "Only truncates the bubbles below; the engine keeps the original context",
    "仅显示本地历史，引擎上下文恢复失败。请开启新对话后再发送消息。": "Local history only — engine context restore failed. Start a new chat before sending.",
    "仅显示此供应商会覆盖的段落": "Only shows the sections this provider overwrites",
    "仅结构": "Structure only",
    "仍未检测到 Grok Build": "Grok Build still not detected",
    "代理可用": "Proxy available",
    "代码已复制": "Code copied",
    "任务创建失败": "Failed to create task",
    "会话已删除": "Session deleted",
    "会话过大，已启用历史摘要续聊": "Session too large — continuing with a history summary",
    "低": "Low",
    "你": "You",
    "例如 grok-chat": "e.g. grok-chat",
    "供应商": "Provider",
    "保存中…": "Saving…",
    "信任后可在此创建会话": "Once trusted, you can create sessions here",
    "信任并继续": "Trust & continue",
    "信任此项目以创建会话": "Trust this project to create sessions",
    "修复路径后可继续使用": "Fix the path to continue",
    "修改后可保存，或保存并启用": "Save your changes, or save & activate",
    "停止中": "Stopping",
    "停止中…": "Stopping…",
    "健康": "Healthy",
    "先填写 API Key": "Fill in the API key first",
    "先填写名称与服务地址": "Fill in name and base URL first",
    "先填写服务地址": "Fill in the base URL first",
    "关闭": "Close",
    "其他": "Other",
    "内容": "Content",
    "内置 Skills": "Built-in Skills",
    "内置技能不可删除": "Built-in skills cannot be removed",
    "内置生图别名不能作为默认、搜索或子代理模型": "The built-in image alias cannot be the default, search or subagent model",
    "凭据已配置": "Credential configured",
    "切换中…": "Switching…",
    "切换自动批准工具": "Toggle tool auto-approval",
    "创建中…": "Creating…",
    "创建新会话": "Create new session",
    "删除 Grok OAuth 凭据？": "Delete the Grok OAuth credential?",
    "删除中…": "Deleting…",
    "删除会话": "Delete session",
    "删除后需要重新导入才能恢复。": "You'll need to re-import to restore it.",
    "删除失败": "Delete failed",
    "删除失败：": "Delete failed: ",
    "删除本条界面气泡并重发上一轮用户消息；引擎仍保留上一轮回复上下文": "Deletes this bubble and resends the previous user message; the engine keeps the previous reply context",
    "刷新中…": "Refreshing…",
    "刷新任务超时": "Refresh task timed out",
    "刷新失败": "Refresh failed",
    "卡片顺序已保存": "Card order saved",
    "压缩上下文？": "Compact context?",
    "原凭据仍会保留。": "The original credential is kept.",
    "发送 /mcp 查看 MCP": "Send /mcp to view MCP",
    "发送将开启新对话": "Sending will start a new chat",
    "取消中…": "Cancelling…",
    "取消置顶": "Unpin",
    "可选，例如 Grok Imagine Image": "Optional, e.g. Grok Imagine Image",
    "号池 API Key 已复制": "Pool API key copied",
    "号池 Base URL 已复制": "Pool base URL copied",
    "号池中还没有账号": "No accounts in the pool yet",
    "号池巡检已启动": "Pool inspection started",
    "号池巡检设置已保存": "Pool inspection settings saved",
    "号池账号已删除": "Pool account deleted",
    "名称、类型与 API Key 即可开始；模型可稍后设置": "Name, type and API key are enough; models can be set later",
    "向 Agent 发送 /compact 以压缩长对话历史。": "Sends /compact to the Agent to compress long chat history.",
    "含密钥导出": "Export with keys",
    "启动 Agent 后即可发送消息": "Start the Agent to send messages",
    "启动中": "Starting",
    "启动中…": "Starting…",
    "启用中…": "Activating…",
    "命令": "Commands",
    "图片": "Image",
    "图片压缩后仍然过大，请选择尺寸更小的图片": "Image still too large after compression — pick a smaller one",
    "图片尺寸过大：单边不能超过 16384 像素，总像素不能超过 5000 万": "Image too large: max 16384px per side, 50MP total",
    "在此工作空间下新建会话": "New session in this workspace",
    "在此项目下新建会话": "New session in this project",
    "处理中": "Processing",
    "处理中…": "Processing…",
    "复制中…": "Copying…",
    "复制计划": "Copy plan",
    "失败": "Failed",
    "媒体无法预览": "Media cannot be previewed",
    "媒体无法预览，点击打开": "No preview — click to open",
    "完全访问（YOLO）：下次启动 Agent 后生效": "Full access (YOLO): takes effect on next Agent start",
    "官方账号": "Official account",
    "对话": "Chat",
    "对话连接尚未就绪": "Chat connection not ready",
    "对话连接已断开，请稍后重试": "Chat connection lost — try again shortly",
    "导入中…": "Importing…",
    "导入供应商": "Import provider",
    "导入认证目录": "Import auth directory",
    "导出中…": "Exporting…",
    "导出包含 API Key？": "Include API keys in the export?",
    "导出完成": "Export complete",
    "尚未导入号池账号": "No pool accounts imported yet",
    "尚未拉取模型。点 chip 仅启用，不会自动设置默认模型。": "No models fetched yet. Chips only enable models — the default is not set automatically.",
    "工作目录已失效，打开后请在会话信息中修正路径": "The working directory is invalid — fix the path in session info after opening",
    "工作空间": "Workspace",
    "工具执行请求": "Tool execution request",
    "工具权限确认": "Tool permission confirmation",
    "工具：本会话自动允许 · ": "Tools: auto-allowed this session · ",
    "已从 config.toml 导入": "Imported from config.toml",
    "已保存": "Saved",
    "已保存为项目": "Saved as project",
    "已保存并启用。新开 grok 会话生效。": "Saved & activated. New grok sessions will use it.",
    "已保存本地图片": "Local image saved",
    "已停止生成": "Generation stopped",
    "已允许本次工具执行": "Tool execution allowed once",
    "已允许，并在本会话自动批准后续工具": "Allowed — later tools auto-approved this session",
    "已关闭生图能力，模型回到无生图状态": "Image generation disabled — models back to no-image state",
    "已切换到官方账号。新开 grok 会话生效。": "Switched to the official account. New grok sessions will use it.",
    "已删除": "Deleted",
    "已刷新 Skills": "Skills refreshed",
    "已勾选 YOLO（下次启动生效）": "YOLO enabled (takes effect on next start)",
    "已发现": "Found",
    "已取消 YOLO": "YOLO cancelled",
    "已取消修复": "Fix cancelled",
    "已取消选择": "Selection cancelled",
    "已启用生图（账号池 MCP 工具）": "Image generation enabled (account-pool MCP tool)",
    "已回退引擎上下文并重新发送": "Engine context rewound and message resent",
    "已复制消息": "Message copied",
    "已导入的凭据和本地代理 profile 将被移除。": "The imported credential and local proxy profile will be removed.",
    "已导出（不含密钥）": "Exported (without keys)",
    "已导出（含密钥）": "Exported (with keys)",
    "已忽略计划": "Plan dismissed",
    "已批准计划": "Plan approved",
    "已拒绝本次工具执行": "Tool execution denied",
    "已提交，后台生成中…": "Submitted — generating in background…",
    "已检测到 Grok Build": "Grok Build detected",
    "已移除自定义背景": "Custom background removed",
    "已请求修改计划": "Plan changes requested",
    "已请求停止巡检": "Stop inspection requested",
    "已请求停止注册": "Stop registration requested",
    "已载入 JSON，确认后点保存": "JSON loaded — save to confirm",
    "已载入副本，保存后生效": "Copy loaded — takes effect after saving",
    "已还原备份": "Backup restored",
    "已连接": "Connected",
    "已重新加载": "Reloaded",
    "应用中…": "Applying…",
    "开启自动批准？": "Enable auto-approval?",
    "异常": "Abnormal",
    "当前仅显示本地历史，原会话上下文没有恢复。继续发送这条消息？": "Only local history is shown — the original context was not restored. Send this message anyway?",
    "当前会话": "Current session",
    "当前会话不可用，请先启动 Agent": "Current session unavailable — start the Agent first",
    "当前使用内置引擎（进程内，无需 Grok CLI）。切换引擎后重启应用生效。": "Using the built-in engine (in-process, no Grok CLI). Engine changes apply after restart.",
    "当前启用": "Currently active",
    "当前工作区 = 会话 cwd": "Current workspace = session cwd",
    "当前没有已巡检的异常账号": "No inspected abnormal accounts right now",
    "当前没有正在生成的回复": "No response is being generated",
    "当前浏览器无法处理背景图片": "This browser cannot process the background image",
    "当前配置会先自动备份。": "The current config is backed up first.",
    "待巡检": "Pending",
    "总账号": "Total accounts",
    "截断下方界面气泡并重发；引擎上下文不会真正回退": "Truncates the bubbles below and resends; the engine context is not actually rewound",
    "所有已生成的图片文件将被删除。": "All generated image files will be deleted.",
    "所有账号均失败": "All accounts failed",
    "所选目录中没有 JSON 文件": "No JSON files in the selected folder",
    "手动禁用": "Manually disabled",
    "手动输入项目目录": "Enter project directory manually",
    "手机配对地址已复制": "Phone pairing URL copied",
    "打开中…": "Opening…",
    "打开原图": "Open original",
    "打开媒体文件": "Open media file",
    "执行中": "Running",
    "批量刷新失败": "Batch refresh failed",
    "批量刷新已在运行": "Batch refresh already running",
    "拉取中…": "Fetching…",
    "拖动 ${escapeHtml(profile.name)} 排序": "Drag to reorder ${escapeHtml(profile.name)}",
    "拖动排序": "Drag to reorder",
    "指定目录已导入号池": "The specified folder was imported into the pool",
    "探测异常": "Probe abnormal",
    "提交失败：": "Submit failed: ",
    "收起高级": "Hide advanced",
    "文件夹选择器不可用，请输入项目目录绝对路径：": "Folder picker unavailable — enter the absolute project path:",
    "文件尚不存在，保存后将创建。": "File does not exist yet — it will be created on save.",
    "新模型": "New model",
    "新的配对二维码已生成": "New pairing QR code generated",
    "无工作目录": "No working directory",
    "无效工作目录": "Invalid working directory",
    "无效的供应商 JSON": "Invalid provider JSON",
    "无法获取生图引擎状态": "Cannot get image engine status",
    "无法读取这张图片，请选择 PNG、JPEG 或 WebP 文件": "Cannot read this image — choose PNG, JPEG or WebP",
    "暂无会话，点 ✎ 新建": "No sessions — click ✎ to create one",
    "更新": "Update",
    "有效期至": "Valid until",
    "未信任": "Not trusted",
    "未命名会话": "Untitled session",
    "未命名项目": "Untitled project",
    "未巡检": "Not inspected",
    "未找到": "Not found",
    "未找到 Grok Build": "Grok Build not found",
    "未检测到 grok 可执行文件，请安装 Grok Build 并加入 PATH": "grok executable not found — install Grok Build and add it to PATH",
    "未知": "Unknown",
    "未选择": "Not selected",
    "未选模型": "No model selected",
    "本会话自动批准工具调用": "Auto-approve tool calls this session",
    "本地存储空间不足，请选择更小的图片": "Not enough local storage — pick a smaller image",
    "本地记录已删除。Agent 仍在运行，建议点「新对话」避免继续旧上下文": "Local record deleted. The Agent is still running — start a new chat to avoid the old context",
    "本张失败，重试下一账号中…": "This one failed — retrying with the next account…",
    "本批生图失败：": "Batch image generation failed: ",
    "本机未检测到 grok 可执行文件。请先安装并登录 Grok CLI，然后重试。": "grok executable not found on this machine. Install and sign in to Grok CLI, then retry.",
    "本轮包含工具调用": "This turn includes tool calls",
    "查看当前 Agent 状态": "View current Agent status",
    "检测中…": "Checking…",
    "模型不可用": "Model unavailable",
    "模型名为空": "Model name is empty",
    "模型请求失败": "Model request failed",
    "模型请求失败，正在重试": "Model request failed — retrying",
    "模型请求重试已耗尽": "Model request retries exhausted",
    "正在准备设备授权…": "Preparing device authorization…",
    "正在扫描 9090 / 9097 / 9091 等常见 Clash 控制器端口…": "Scanning common Clash controller ports 9090 / 9097 / 9091…",
    "正在生成… Esc 或点击停止": "Generating… Esc or click to stop",
    "正在生成…可点击停止": "Generating… click to stop",
    "正在生成…点击停止 · Esc 也可停止": "Generating… click to stop · Esc also works",
    "正在生成中，请先停止或稍后再试": "Generation in progress — stop it or wait",
    "正在生成中，请先停止或等待完成": "Generation in progress — stop it or wait for completion",
    "正在生成回复，请先停止或等待完成": "A response is being generated — stop it or wait",
    "正在重新生成": "Regenerating",
    "正在重新生成…（已回退引擎上一轮）": "Regenerating… (engine rewound one turn)",
    "每次工具调用需确认": "Every tool call needs confirmation",
    "没有可复制的内容": "Nothing to copy",
    "没有可重新生成的用户消息": "No user message to regenerate",
    "没有找到号池本地 profile，请重新导入账号": "Pool local profile not found — re-import the account",
    "没有找到本地 Grok profile，请重新导入 JSON": "Local Grok profile not found — re-import the JSON",
    "没有检测结果": "No detection result",
    "注册中": "Registering",
    "注册任务失败": "Registration task failed",
    "注册任务已停止": "Registration task stopped",
    "注册任务已完成，账号已进入号池": "Registration finished — account added to the pool",
    "注册机配置已保存": "Registrar config saved",
    "注册环境可用": "Registration environment ready",
    "注册环境检测未通过": "Registration environment check failed",
    "测试中…": "Testing…",
    "测试生图：一只可爱的小猫": "Test image: a cute kitten",
    "浏览并插入 Skill": "Browse & insert a Skill",
    "消息不能为空": "Message cannot be empty",
    "添加为项目": "Add as project",
    "清空失败：": "Clear failed: ",
    "清空画廊？": "Clear the gallery?",
    "点击展开查看输入/输出": "Click to expand input/output",
    "状态": "Status",
    "状态未知": "Status unknown",
    "环境自检提示": "Environment self-check",
    "生图中…": "Generating image…",
    "生图引擎未就绪：registrar/cookies 中未找到可用账号": "Image engine not ready: no usable account in registrar/cookies",
    "生图测试失败": "Image generation test failed",
    "生图测试成功": "Image generation test succeeded",
    "生成中…": "Generating…",
    "生成预览…": "Generating preview…",
    "用户 Skills": "User Skills",
    "画廊已清空": "Gallery cleared",
    "留空：config 中不写入，Grok 使用默认（新模型约 20 万）": "Empty = not written to config; Grok uses the default (~200k for new models)",
    "目录": "Directory",
    "目录失效": "Directory invalid",
    "确定 = 含密钥（仅私用）\\n取消 = 仅结构（适合分享）": "OK = include keys (private use only)\\nCancel = structure only (safe to share)",
    "确定启动": "Confirm start",
    "空为默认": "Empty = default",
    "突然上色": "Suddenly colored",
    "等待": "Waiting",
    "等待日志…": "Waiting for logs…",
    "等待浏览器完成登录与授权…": "Waiting for sign-in and authorization in the browser…",
    "等待首次巡检": "Waiting for first inspection",
    "编辑供应商": "Edit provider",
    "置顶": "Pin",
    "聊天背景已保存在当前设备": "Chat background saved on this device",
    "背景图片不能超过 16 MB": "Background image must be under 16 MB",
    "自动批准会允许 Grok Build 无需确认即可修改文件和执行命令。": "Auto-approval lets Grok Build modify files and run commands without confirmation.",
    "计划更新": "Plan update",
    "认证文件已写入": "Auth file written",
    "认证目录尚未扫描": "Auth directory not scanned yet",
    "认证目录已导入号池": "Auth directory imported into the pool",
    "设定目标": "Set goal",
    "设置已保存（引擎切换重启后生效）": "Settings saved (engine switch applies after restart)",
    "该会话的工作目录已失效，请在右侧修正路径后再打开": "This session's working directory is invalid — fix the path on the right before opening",
    "该组会话没有有效工作目录": "This session group has no valid working directory",
    "说明：只会截断下方界面内容并追加新一轮消息，引擎上下文不会真正回退。": "Note: this only truncates the UI below and appends a new turn — the engine context is not actually rewound.",
    "请先启用模型": "Enable a model first",
    "请先填写供应商信息": "Fill in the provider info first",
    "请先填写或选择工作目录": "Fill in or pick a working directory first",
    "请先添加或选择一个项目，会话会创建在该工作空间下": "Add or pick a project first — sessions are created under that workspace",
    "请先设置工作目录": "Set a working directory first",
    "请先输入提示词": "Enter a prompt first",
    "请先选择工作目录": "Choose a working directory first",
    "请先选择或添加一个项目（工作空间）": "Pick or add a project (workspace) first",
    "请在浏览器完成官方账号登录": "Finish signing in to the official account in the browser",
    "请填写名称": "Please enter a name",
    "请填写服务地址": "Please enter the base URL",
    "请提供工作目录": "Please provide a working directory",
    "请求 Agent 压缩上下文": "Ask the Agent to compact context",
    "请求修改计划": "Request plan changes",
    "请求已取消": "Request cancelled",
    "请说明希望如何修改计划（可留空）": "Describe the plan changes you want (may be empty)",
    "账号已启用": "Account enabled",
    "账号已禁用": "Account disabled",
    "账号池：无可用账号，生图不可用": "Account pool: no available accounts — image generation unavailable",
    "跟随系统": "Follow system",
    "路径不可用，点击重新选择目录": "Path unavailable — click to re-pick a directory",
    "路径失效": "Path invalid",
    "输入服务器本机上的认证目录绝对路径：": "Enter the absolute auth-directory path on this server:",
    "输出": "Output",
    "还原中…": "Restoring…",
    "进入规划模式": "Enter plan mode",
    "连接中…": "Connecting…",
    "连接失败": "Connection failed",
    "连接异常": "Connection error",
    "连接异常，请检查工作目录后重新启动 Agent。": "Connection error — check the working directory and restart the Agent.",
    "重启 Agent": "Restart Agent",
    "重命名会话": "Rename session",
    "重新登录该账号并更新 Cookie / 铸造 CPA 凭据": "Re-login this account and refresh cookies / mint CPA credentials",
    "重新选择目录（修复中文/失效路径）": "Re-pick directory (fix broken path)",
    "铸造已取消。": "Minting cancelled.",
    "附件消息": "Attachment message",
    "隐私保护配置已写入 config.toml": "Privacy-guard config written to config.toml",
    "需要刷新": "Needs refresh",
    "需要确认执行计划": "Plan needs confirmation",
    "预览": "Preview",
    "❌ 未检测到正在运行的 Clash/mihomo 控制器。请确认 FlClash / ClashVerge / ClashX 已启动并开启了外部控制器（默认端口 9090）。": "❌ No running Clash/mihomo controller detected. Make sure FlClash / ClashVerge / ClashX is running with the external controller enabled (default port 9090).",
    "仅显示本地历史：会话过大或恢复通知过多，引擎上下文未挂载。Agent 已恢复，可开启新对话。": "Local history only: session too large or too many restore notices — engine context not attached. The Agent has recovered; start a new chat.",
    "仅显示本地历史：引擎上下文未挂载，Agent 自动重启也未成功。请手动启动新对话。": "Local history only: engine context not attached and Agent auto-restart failed. Please start a new chat manually.",
    "将逐个处理：先用现有 refresh_token 直接续期（秒级、不弹浏览器）；续期失败（吊销/过期）才回退浏览器重新登录铸造（每个约 40~90 秒）。\\n没有在 registrar 账本（accounts_cli.txt）中保存密码的账号会被跳过。": "Processed one by one: first try direct refresh with the existing refresh_token (seconds, no browser); only on refresh failure (revoked/expired) fall back to browser re-login and minting (~40–90s each).\\nAccounts without a password saved in the registrar ledger (accounts_cli.txt) are skipped.",
    "引擎上下文未能完整加载。已展示本地历史；下一条消息将自动注入历史摘要以续聊。也可开启全新对话。": "Engine context could not be fully loaded. Local history is shown; the next message auto-injects a history summary to continue. You can also start a fresh chat.",
    "当前使用 Grok CLI（ACP 桥）。切换引擎后重启应用生效；内置引擎无需安装 Grok CLI。": "Using Grok CLI (ACP bridge). Engine changes apply after restart; the built-in engine needs no Grok CLI.",
    "点击顶部「打开位置」旁的 ▾ 选择工作目录，或点输入框旁项目 chip · 再启动 Agent。": "Pick a working directory via the ▾ next to “Open location” at the top, or the project chip near the input box — then start the Agent.",
    "留空：config 中不写入；可在 [models] 设全局 max_completion_tokens": "Empty = not written to config; set a global max_completion_tokens in [models]",
    "选择认证文件后，会生成稳定的本地 URL/key 和一个可直接启用的 Responses profile。": "After picking an auth file, a stable local URL/key and a ready-to-activate Responses profile are generated.",
    "<button type=\"button\" class=\"btn sm\" data-action=\"edit\">编辑</button><button type=\"button\" class=\"btn sm ghost\" data-action=\"copy\">复制</button><button type=\"button\" class=\"btn sm ghost\" data-action=\"export\">导出</button><button type=\"button\" class=\"btn sm danger\" data-action=\"delete\">删除</button>": "<button type=\"button\" class=\"btn sm\" data-action=\"edit\">Edit</button><button type=\"button\" class=\"btn sm ghost\" data-action=\"copy\">Copy</button><button type=\"button\" class=\"btn sm ghost\" data-action=\"export\">Export</button><button type=\"button\" class=\"btn sm danger\" data-action=\"delete\">Delete</button>",
    "<option value=\"\">配置中没有启用模型</option>": "<option value=\"\">No enabled models in config</option>",
    "<span class=\"pinBadge\">已置顶</span>": "<span class=\"pinBadge\">Pinned</span>",
    "<button type=\"button\" class=\"chip mutedChip\">先拉取模型</button>": "<button type=\"button\" class=\"chip mutedChip\">Fetch models first</button>",
    "<button type=\"button\" class=\"chip mutedChip\">没有匹配</button>": "<button type=\"button\" class=\"chip mutedChip\">No match</button>",
    "<div class=\"skillsPopupEmpty\"><strong>无匹配文件</strong></div>": "<div class=\"skillsPopupEmpty\"><strong>No matching files</strong></div>",
    "<div class=\"skillsPopupEmpty\"><strong>无匹配项</strong><span>试试 /plan /compact /skills</span></div>": "<div class=\"skillsPopupEmpty\"><strong>No matches</strong><span>Try /plan /compact /skills</span></div>",
    "<div class=\"skillsPopupLoading\">加载中…</div>": "<div class=\"skillsPopupLoading\">Loading…</div>",
    "<div class=\"skillsPopupLoading\">正在加载 Skills…</div>": "<div class=\"skillsPopupLoading\">Loading Skills…</div>",
    "<option value=\"\">未找到局域网地址</option>": "<option value=\"\">No LAN address found</option>",
    "<p class=\"muted tiny\">切换供应商时会自动创建。暂无历史备份。</p>": "<p class=\"muted tiny\">Created automatically on provider switch. No backups yet.</p>",
    "<p class=\"muted tiny\">没有匹配的账号。可一次选择多个 CPA xai-*.json；也支持 Grok CLI auth.json。</p>": "<p class=\"muted tiny\">No matching accounts. You can pick multiple CPA xai-*.json files; Grok CLI auth.json is also supported.</p>",
    "<p class=\"sessionListEmpty\">暂无项目。点击 ＋ 添加工作目录后，会话会归入对应项目。</p>": "<p class=\"sessionListEmpty\">No projects. Click ＋ to add a working directory — sessions will be grouped under it.</p>",
    "从左侧添加项目（工作空间），在项目下新建或打开会话。": "Add a project (workspace) on the left, then create or open a session under it.",
    "重新检测": "Re-check",
    "网络代理": "Proxy",
    "标签": "Tags",
    "描述": "Description",
    "来源": "Source",
    "版本": "Version",
    "路径": "Path",
    "操作": "Actions",
    "卸载": "Uninstall",
    "查看": "View",
    "运行": "Run",
    "继续生成": "Continue generating",
    "跳过": "Skip",
    "知道了": "Got it",
    "详情": "Details",
    "原因": "Reason",
    "建议": "Suggestion",
    "重试": "Retry",
    "重试中…": "Retrying…",
    "不可用": "Unavailable",
    "开启": "Enable",
    "开启新对话": "Start a new chat",
    "检查更新": "Check for updates",
    "当前版本": "Current version",
    "最新版本": "Latest version",
    "已是最新": "Up to date",
    "检查中…": "Checking…",
    "发布于": "Published",
    "查看详情": "View details",
    "马上更新": "Update now",
    "稍后再说": "Later",
    "更多": "More",
    "语言": "Language",
    "English": "English",
    "中文": "中文",
    "默认": "Default",
    "自定义": "Custom",
    "无": "None",
    "开启后": "When enabled",
    "关闭后": "When disabled",
    "说明": "Note",
    "示例": "Example",
    "高级": "Advanced",
    "基础": "Basic",
    "通用": "General",
    "外观": "Appearance",
    "行为": "Behavior",
    "快捷键": "Shortcuts",
    "关于": "About",
    "帮助": "Help",
    "文档": "Docs",
    "反馈": "Feedback",
    "官网": "Website",
    "社区": "Community",
    "日志": "Logs",
    "诊断": "Diagnostics",
    "导出诊断": "Export diagnostics",
    "数据目录": "Data directory",
    "打开数据目录": "Open data directory",
    "打开日志目录": "Open log directory",
    "版本信息": "Version info",
    "检查更新中…": "Checking for updates…",
    "发现新版本": "New version available",
    "跳过此版本": "Skip this version",
    "下载最新版": "Download latest",
    "查看更新说明": "Release notes",
  };

  // ---------------------------------------------------------------------------
  // Pattern layer: app.js builds many strings by interpolation, so the rendered
  // text (e.g. a toast "已启用 OpenRouter。新开 grok 会话生效。") never appears
  // in the dictionary. These regex rules translate the rendered whole string.
  // Ordered most-specific first; each entry is [regex, replacement] where the
  // replacement is a $-template string or a function receiving the match array
  // (a function may return undefined to fall through to the next rule).
  // ---------------------------------------------------------------------------
  const VERB_ING = {
    "保存": "Saving", "删除": "Deleting", "导入": "Importing", "导出": "Exporting",
    "启用": "Enabling", "禁用": "Disabling", "切换": "Switching", "创建": "Creating",
    "复制": "Copying", "打开": "Opening", "还原": "Restoring", "刷新": "Refreshing",
    "拉取": "Fetching", "连接": "Connecting", "应用": "Applying", "检测": "Checking",
    "测试": "Testing", "处理": "Processing", "取消": "Cancelling", "停止": "Stopping",
    "启动": "Starting", "生成": "Generating", "注册": "Registering", "巡检": "Inspecting",
    "铸造": "Minting", "重命名": "Renaming", "上传": "Uploading", "下载": "Downloading",
    "加载": "Loading", "读取": "Reading", "写入": "Writing", "解析": "Parsing",
  };

  const trName = (s) => {
    if (typeof s !== "string") return s;
    const t = s.trim();
    const hit = lookup(t);
    if (hit !== undefined) return hit;
    return patternTranslate(t) ?? s;
  };

  const PATTERNS = [
    // --- counts / badges -----------------------------------------------------
    [/^(\d+(?:\.\d+)?) 个自动备份$/, "$1 backups"],
    [/^(\d+) 个会话$/, "$1 sessions"],
    [/^(\d+) \/ (\d+) 个$/, "$1 / $2"],
    [/^(\d+) 个$/, "$1"],
    [/^(\d+) 个账号 · (\d+) 可用$/, "$1 accounts · $2 available"],
    [/^共 (\d+) 个账号 · 批量刷新完成（成功 (\d+) \/ 失败 (\d+)）$/, "$1 accounts · batch refresh done ($2 ok / $3 failed)"],
    [/^共 (\d+) 个账号 · 已加载 (.*)$/, "$1 accounts · $2 loaded"],
    [/^已加载 (\d+) \/ (\d+)$/, "$1 / $2 loaded"],
    [/^统一号池 (\d+) 个账号 · 已启用自动巡检$/, "Unified pool: $1 accounts · auto-inspection on"],
    [/^热加载 (\d+) 个文件$/, "Hot-reloading $1 files"],
    [/^生图引擎就绪 · 可用账号 (\d+) 个$/, "Image engine ready · $1 accounts available"],
    [/^账号池：(\d+) 个可用账号$/, "Account pool: $1 available"],
    [/^\[(\d+) 项\] (.*)$/, "[$1 items] $2"],

    // --- verb + 中… ----------------------------------------------------------
    [/^(保存|删除|导入|导出|启用|禁用|切换|创建|复制|打开|还原|刷新|拉取|连接|应用|检测|测试|处理|取消|停止|启动|生成|注册|巡检|铸造|重命名|上传|下载|加载|读取|写入|解析)中…$/,
      (m) => (VERB_ING[m[1]] || m[1]) + "…"],

    // --- dialogs / confirms --------------------------------------------------
    [/^删除会话「(.*)」$/, (m) => `Delete session "${trName(m[1])}"?`],
    [/^删除会话\s*(.*)$/, (m) => m[1] ? `Delete session "${m[1]}"?` : "Delete session?"],
    [/^删除号池账号 (.*)$/, (m) => `Delete pool account "${trName(m[1])}"?`],
    [/^删除「(.*)」$/, (m) => `Delete "${trName(m[1])}"?`],
    [/^删除 "(.*)"$/, (m) => `Delete "${trName(m[1])}"?`],
    [/^信任项目「(.*)」？$/, (m) => `Trust project "${trName(m[1])}"?`],
    [/^批量(禁用|删除) (\d+) 个异常账号？$/,
      (m) => `${m[1] === "禁用" ? "Disable" : "Delete"} ${m[2]} abnormal accounts in batch?`],
    [/^批量刷新 (\d+) 个号池账号？$/, "Batch-refresh $1 pool accounts?"],
    [/^跳过 (.*)？$/, "Skip $1?"],
    [/^还原备份 (.*)$/, "Restore backup $1"],
    [/^重命名会话\s*(.*)$/, (m) => m[1] ? `Rename session "${m[1]}"` : "Rename session"],
    [/^路径: (.*)\n此操作不可恢复。$/, "Path: $1\nThis cannot be undone."],

    // --- toasts: success / failure -------------------------------------------
    [/^已启用 (.*)。新开 grok 会话生效。$/, (m) => `Activated ${trName(m[1])}. New grok sessions will use it.`],
    [/^已重命名为「(.*)」$/, (m) => `Renamed to "${trName(m[1])}"`],
    [/^已套用「(.*)」地址与协议，请自行启用模型$/, (m) => `Applied "${trName(m[1])}" URL and protocol — enable models yourself`],
    [/^已处理 (\d+) 个 JSON 文件$/, "Processed $1 JSON files"],
    [/^已导出 (\d+) 个 JSON 文件到：(.*)$/, "Exported $1 JSON files to: $2"],
    [/^已批量删除 (\d+) 个异常账号（含 (\d+) 个源 JSON 文件）$/,
      "Deleted $1 abnormal accounts (incl. $2 source JSON files)"],
    [/^已批量(禁用|删除) (\d+) 个异常账号$/,
      (m) => `${m[1] === "禁用" ? "Disabled" : "Deleted"} ${m[2]} abnormal accounts in batch`],
    [/^已提交 (\d+) 张，后台并行生成中…$/, "Submitted $1 image(s) — generating in parallel…"],
    [/^已添加项目：(.*)$/, "Project added: $1"],
    [/^已跳过 (.*)$/, "Skipped $1"],
    [/^已选择工作目录：(.*)$/, "Working directory selected: $1"],
    [/^已修复路径：(.*)$/, "Path fixed: $1"],
    [/^已切换到项目：(.*)$/, "Switched to project: $1"],
    [/^已切换工作空间：(.*)$/, "Switched to workspace: $1"],
    [/^已删除: (.*)$/, "Deleted: $1"],
    [/^已引用 (.*)$/, "Referenced $1"],
    [/^已打开：(.*)$/, "Opened: $1"],
    [/^打开：(.*)$/, "Open: $1"],
    [/^创建 Grok 会话失败：(.*)$/, "Failed to create Grok session: $1"],
    [/^已重新发送（引擎 rewind 未成功(.*)，可能仍保留旧上下文）$/,
      (m) => `Resent (engine rewind failed${m[1] || ""}, old context may remain)`],
    [/^正在重新生成…（rewind 未成功(.*)）$/,
      (m) => `Regenerating… (rewind failed${m[1] || ""})`],
    [/^批量刷新完成：成功 (\d+)，失败\/跳过 (\d+)$/, "Batch refresh done: $1 succeeded, $2 failed/skipped"],
    [/^批量刷新 (\d+)\/(\d+) …$/, "Batch refresh $1/$2 …"],
    [/^批量刷新中…（(\d+)）$/, "Batch refreshing… ($1)"],
    [/^注册部分完成：成功 (\d+)，失败 (\d+)$/, "Registration partially done: $1 succeeded, $2 failed"],
    [/^本批完成：成功 (\d+) 张(，失败 (\d+) 张)?$/,
      (m) => `Batch done: ${m[1]} image(s) ok${m[2] ? `, ${m[3]} failed` : ""}`],
    [/^导入 (\d+)，更新 (\d+)$/, "Imported $1, updated $2"],
    [/^成功 (\d+)$/, "$1 succeeded"],
    [/^失败 (\d+)$/, "$1 failed"],
    [/^操作完成，但有文件清理失败：(.*)$/, "Done, but some files failed to clean up: $1"],
    [/^部分文件失败：(.*)$/, "Some files failed: $1"],
    [/^新图片已生成（\+(\d+)）$/, "New images generated (+$1)"],
    [/^（跳过 (\d+) 个）$/, "($1 skipped)"],
    [/^切换失败：(.*)$/, "Switch failed: $1"],
    [/^删除失败：(.*)$/, "Delete failed: $1"],
    [/^清空失败：(.*)$/, "Clear failed: $1"],
    [/^提交失败：(.*)$/, "Submit failed: $1"],
    [/^失败：(.*)$/, "Failed: $1"],
    [/^铸造成功：(.*)$/, "Minted: $1"],
    [/^铸造失败：(.*)$/, "Minting failed: $1"],
    [/^Cookie 已刷新：(.*)$/, "Cookie refreshed: $1"],
    [/^刷新失败：(.*)$/, "Refresh failed: $1"],
    [/^❌ 检测失败：(.*)$/, "❌ Detection failed: $1"],

    // --- connection / model tests --------------------------------------------
    [/^(.*) 连通（(\d+)ms）$/, "$1 reachable ($2ms)"],
    [/^(.*) 不通$/, "$1 unreachable"],
    [/^连通 (\d+)ms$/, "Connected in $1ms"],
    [/^连接成功（(\d+)ms）$/, "Connected ($1ms)"],
    [/^成功 · (\d+)ms · (\d+) 模型$/, "OK · $1ms · $2 models"],
    [/^已缓存 (\d+) 个模型。点 chip 启用\/取消；默认模型请手动填写。$/,
      "$1 models cached. Click chips to enable/disable; set the default manually."],
    [/^(已获取|获取到) (\d+) 个模型(.*)$/, (m) => `Fetched ${m[2]} models${m[3] || ""}`],
    [/^失败 (\d+)ms：(.*)$/, "Failed after $1ms: $2"],
    [/^生图成功（(\d+)x(\d+)，账号 (.*)）$/, "Image generated ($1x$2, account $3)"],
    [/^上传失败 HTTP (\d+)$/, "Upload failed: HTTP $1"],
    [/^Markdown 渲染失败: (.*)$/, "Markdown render failed: $1"],
    [/^Mermaid 图表无法渲染\n(.*)$/s, "Mermaid diagram could not render\n$1"],
    [/^Mermaid 库加载失败\n(.*)$/s, "Mermaid library failed to load\n$1"],

    // --- statuses / hints ------------------------------------------------------
    [/^(已登录 grok\.com|尚未登录) · OAuth 官方模型$/,
      (m) => (m[1] === "已登录 grok.com" ? "Signed in to grok.com" : "Not signed in") + " · Official OAuth models"],
    [/^(.*?) · ([^·]+?) · (\d+) 模型$/,
      (m) => `${lookup(m[1].trim()) ?? m[1]} · ${m[2].trim() === "自定义" ? "Custom" : m[2].trim()} · ${m[3]} models`],
    [/^(OK|失败) · (.*?) · (.*)$/, (m) => `${m[1] === "OK" ? "OK" : "Failed"} · ${m[2]}${m[3] ? " · " + m[3] : ""}`],
    [/^(.*)（默认）$/, "$1 (default)"],
    [/^(.*)（未启用）$/, "$1 (not active)"],
    [/^(.*) 副本$/, "$1 copy"],
    [/^(.*) 导入$/, "$1 import"],
    [/^拖动 (.*) 排序$/, (m) => `Drag to reorder ${trName(m[1])}`],
    [/^在 (.*) 下新建会话$/, (m) => `New session under ${trName(m[1])}`],
    [/^将 (.*) 添加为项目$/, (m) => `Add ${trName(m[1])} as project`],
    [/^移除 (.*)$/, "Remove $1"],
    [/^打开 (.*)$/, (m) => `Open ${trName(m[1])}`],
    [/^修复 (.*) 的路径$/, (m) => `Fix the path of ${trName(m[1])}`],
    [/^请重新选择「(.*)」的目录$/, (m) => `Re-pick a directory for "${trName(m[1])}"`],
    [/^重新登录 (.*) 并更新 Cookie \/ 铸造 CPA 凭据$/,
      (m) => `Re-login ${trName(m[1])} and refresh cookies / mint CPA credentials`],
    [/^工作目录：(.*)。启动 Agent 后即可发送消息，或从左侧打开历史会话。$/,
      "Working directory: $1. Start the Agent to send messages, or open a past session on the left."],
    [/^(.*?)Grok CLI 未登录或凭据不可用。请在终端运行 grok 登录后重试。$/,
      "$1Grok CLI is not signed in or its credentials are unavailable. Run grok in a terminal to sign in, then retry."],
    [/^工具：本会话自动允许 · Enter 发送 · Shift\+Enter 换行$/,
      "Tools: auto-allowed this session · Enter to send · Shift+Enter for newline"],
    [/^实际端口 (\d+)（配置 (\d+) 可能被占用）$/, "Actual port $1 (configured $2 may be in use)"],
    [/^对话事件无效：(.*)$/, "Invalid chat event: $1"],
    [/^巡检中 (\d+)\/(\d+)$/, "Inspecting $1/$2"],
    [/^合并到 (.*) 后的完整文件预览（未保存）$/, "Full preview after merging into $1 (unsaved)"],
    [/^图片过大（([\d.]+) MB），上限 16 MB$/, "Image too large ($1 MB) — limit is 16 MB"],
    [/^文件过大（([\d.]+) MB），上限 1 MB$/, "File too large ($1 MB) — limit is 1 MB"],
    [/^多块编辑 (\d+) 处 · (.*)$/, "Multi-edit: $1 changes · $2"],
    [/^↑ 加载更早 (\d+) 条（还剩 (\d+)）$/, "↑ Load $1 earlier messages ($2 left)"],
    [/^↑ 加载更早 (\d+) 条会话$/, "↑ Load $1 earlier messages"],
    [/^上次巡检 (.*)$/, "Last inspected $1"],
    [/^下次 (.*)$/, "Next $1"],
    [/^最近导入 (.*)$/, "Last imported $1"],
    [/^有效至 (.*)$/, "Valid until $1"],
    [/^版本 (.*)$/, "Version $1"],
    [/^端口 (\d+)$/, "Port $1"],
    [/^CPA 目录：(.*?) · 账本：(.*)$/, "CPA directory: $1 · Ledger: $2"],
    [/^Grok 可用：(.*)$/, "Grok available: $1"],
    [/^CPA 目录：(.*)$/, "CPA directory: $1"],
    [/^账本：(.*)$/, "Ledger: $1"],
    [/^(.* · )后台生成中 (\d+) 批 · 待出图 (\d+) 张（切换页面不受影响，可继续提交新任务）$/,
      "$1$2 batch(es) running · $3 image(s) pending (safe to switch pages; submit anytime)"],
    [/^后台生成中 (\d+) 批 · 待出图 (\d+) 张（切换页面不受影响，可继续提交新任务）$/,
      "$1 batch(es) running · $2 image(s) pending (safe to switch pages; submit anytime)"],
    [/^✅ 已检测到 Clash 核心 v(.*)，控制器=$/, "✅ Clash core v$1 detected, controller="],
    [/^，选择器组=$/, ", selector group="],
    [/^（(\d+) 个可用节点）(.*)。配置已自动填入。$/, "($1 usable nodes)$2. Config auto-filled."],
    [/^请用 read 工具分段查看 (.*) 的关键错误$/, "Use the read tool to inspect key errors in $1 in chunks"],
    [/^用 read 查看完整输出 \((.*)\)$/, "View full output with read ($1)"],
    [/^📎 (.*?)(（已截断）)?$/, (m) => `📎 ${m[1]}${m[2] ? " (truncated)" : ""}`],
    [/^(\d+) 个账号$/, "$1 accounts"],
    [/^(.*?) · 巡检 (.*?) · Token (.*)$/, "$1 · inspected $2 · token $3"],
    [/^(\d+) 模型$/, "$1 models"],
    [/^(.*?) · 端口 (\d+) · 版本 (.*)$/, "$1 · Port $2 · Version $3"],
  ];

  function patternTranslate(s) {
    for (const [re, rep] of PATTERNS) {
      const m = s.match(re);
      if (!m) continue;
      if (typeof rep === "function") {
        const out = rep(m);
        if (out !== undefined) return out;
        continue;
      }
      return s.replace(re, rep);
    }
    return undefined;
  }

  // ---------------------------------------------------------------------------
  // State
  // ---------------------------------------------------------------------------
  let currentLang = loadLang();
  let dictLookup = null; // lazily-built lowercase map
  let observer = null;
  let observerTimer = null;
  const pendingNodes = new Set();

  function loadLang() {
    try {
      const saved = localStorage.getItem(LANG_KEY);
      if (SUPPORTED.includes(saved)) return saved;
    } catch (e) { /* storage may be unavailable */ }
    return "zh";
  }

  function saveLang(lang) {
    try {
      localStorage.setItem(LANG_KEY, lang);
    } catch (e) { /* ignore */ }
  }

  function lang() {
    return currentLang;
  }

  function isEn() {
    return currentLang === "en";
  }

  // Exact dictionary translation. Returns undefined when no mapping exists.
  function lookup(source) {
    if (typeof source !== "string") return undefined;
    const direct = DICT[source];
    if (direct !== undefined) return direct;
    // Tolerate surrounding whitespace in dynamically-built strings.
    const trimmed = source.trim();
    if (trimmed !== source && Object.prototype.hasOwnProperty.call(DICT, trimmed)) {
      const hit = DICT[trimmed];
      const lead = source.slice(0, source.length - source.trimStart().length);
      const tail = source.slice(source.trimEnd().length);
      return lead + hit + tail;
    }
    return undefined;
  }

  // t: exact-match translation, falls back to the Chinese source.
  function t(source) {
    if (!isEn()) return source;
    const hit = lookup(source);
    return hit !== undefined ? hit : source;
  }

  // tf: template translation for strings app.js builds with interpolation.
  //   tf("已缓存 {n} 个模型", { n: 3 })  ->  "Cached 3 models"
  // The zh template doubles as the key and as the zh rendering ({name} is
  // substituted in both languages).
  function tf(zhTemplate, vars) {
    let template = zhTemplate;
    if (isEn()) {
      const hit = lookup(zhTemplate);
      if (hit !== undefined) template = hit;
    }
    return template.replace(/\{(\w+)\}/g, (m, name) =>
      vars && Object.prototype.hasOwnProperty.call(vars, name) ? String(vars[name]) : m
    );
  }

  // ---------------------------------------------------------------------------
  // Static markup translation via data-i18n attributes.
  // ---------------------------------------------------------------------------
  const ATTR_PROPS = {
    "i18nPlaceholder": "placeholder",
    "i18nTitle": "title",
    "i18nAria": "aria-label",
    "i18nValue": "value",
    "i18nHtml": "innerHTML",
  };

  function applyElement(el) {
    if (!el || !el.dataset) return;
    const key = el.dataset.i18n;
    if (key) {
      const hit = lookup(key);
      if (hit !== undefined) el.textContent = hit;
      else if (isEn()) el.textContent = key;
      else el.textContent = key;
    }
    for (const dataName of Object.keys(ATTR_PROPS)) {
      const k = el.dataset[dataName];
      if (!k) continue;
      const prop = ATTR_PROPS[dataName];
      const hit = lookup(k);
      el[prop] = hit !== undefined ? hit : k;
    }
  }

  function applyStatic(root) {
    const scope = root || document;
    if (scope.nodeType === 1 && scope.dataset) applyElement(scope);
    const nodes = scope.querySelectorAll
      ? scope.querySelectorAll("[data-i18n],[data-i18n-placeholder],[data-i18n-title],[data-i18n-aria],[data-i18n-value],[data-i18n-html]")
      : [];
    for (const el of nodes) applyElement(el);
  }

  // ---------------------------------------------------------------------------
  // Dynamic translation: app.js renders Chinese literals long after load. When
  // English is active we watch mutations and translate the exact strings we
  // know; unknown strings are left untouched (they stay Chinese).
  // ---------------------------------------------------------------------------
  const TEXT_ATTRS = ["title", "placeholder", "aria-label"];
  // Never translate inside these: rendered chat content (user/assistant
  // messages, outline labels), code/config/logs, script/style.
  const EXCLUDE_SELECTOR =
    "script,style,textarea,pre,code,.markdownBody,.chatNodeLabel,.chatNodeIdx,.registrarLog,.planBody,.chatMessageEditArea,.imaginePrompt,#configEditor,#providerConfigPreview,.langSwitch";

  function translateTree(root) {
    if (!isEn()) return;
    if (!root) return;
    if (root.nodeType === 3) {
      translateTextNode(root);
      return;
    }
    if (root.nodeType !== 1 && root.nodeType !== 11) return;
    if (root.dataset) applyElement(root);
    if (root.querySelectorAll) {
      for (const el of root.querySelectorAll(
        "[data-i18n],[data-i18n-placeholder],[data-i18n-title],[data-i18n-aria],[data-i18n-value],[data-i18n-html]"
      )) {
        applyElement(el);
      }
    }
    // Translate plain text nodes that exactly match dictionary entries.
    const walkerRoot = root.nodeType === 11 ? root : root;
    const walker = document.createTreeWalker(
      walkerRoot,
      NodeFilter.SHOW_TEXT,
      {
        acceptNode(node) {
          if (!node.nodeValue) return NodeFilter.FILTER_REJECT;
          const parent = node.parentElement;
          if (!parent) return NodeFilter.FILTER_REJECT;
          const tag = parent.tagName;
          if (tag === "SCRIPT" || tag === "STYLE" || tag === "TEXTAREA") {
            return NodeFilter.FILTER_REJECT;
          }
          if (parent.closest && parent.closest(EXCLUDE_SELECTOR)) {
            return NodeFilter.FILTER_REJECT;
          }
          return NodeFilter.FILTER_ACCEPT;
        },
      }
    );
    const batch = [];
    while (walker.nextNode()) batch.push(walker.currentNode);
    for (const node of batch) translateTextNode(node);
    // Common attributes rendered from JS.
    if (root.querySelectorAll) {
      for (const el of root.querySelectorAll("[title],[placeholder],[aria-label]")) {
        translateAttrs(el);
      }
    }
    if (root.attributes) translateAttrs(root);
  }

  function translateTextNode(node) {
    const value = node.nodeValue;
    if (!value || !/[一-鿿]/.test(value)) return;
    const trimmed = value.trim();
    let hit = lookup(trimmed);
    if (hit === undefined) hit = patternTranslate(trimmed);
    if (hit === undefined) return;
    const lead = value.slice(0, value.length - value.trimStart().length);
    const tail = value.slice(value.trimEnd().length);
    const next = lead + hit + tail;
    if (next !== value) node.nodeValue = next;
  }

  function translateAttrs(el) {
    if (!el.attributes) return;
    for (const attr of TEXT_ATTRS) {
      const value = el.getAttribute(attr);
      if (!value || !/[一-鿿]/.test(value)) continue;
      let hit = lookup(value);
      if (hit === undefined) hit = patternTranslate(value);
      if (hit !== undefined && hit !== value) el.setAttribute(attr, hit);
    }
  }

  function scheduleObserverFlush() {
    if (observerTimer) return;
    observerTimer = setTimeout(() => {
      observerTimer = null;
      const nodes = Array.from(pendingNodes);
      pendingNodes.clear();
      for (const node of nodes) {
        if (node && (node.isConnected || node.nodeType === 11)) translateTree(node);
      }
    }, 30);
  }

  function startObserver() {
    if (observer || typeof MutationObserver === "undefined") return;
    observer = new MutationObserver((mutations) => {
      if (!isEn()) return;
      for (const m of mutations) {
        for (const added of m.addedNodes) {
          if (added.nodeType === 1 || added.nodeType === 11 || added.nodeType === 3) {
            pendingNodes.add(added);
          }
        }
        if (m.type === "characterData" && m.target) {
          pendingNodes.add(m.target);
        }
        if (m.type === "attributes" && m.target) {
          pendingNodes.add(m.target);
        }
      }
      scheduleObserverFlush();
    });
    observer.observe(document.body, {
      childList: true,
      subtree: true,
      characterData: true,
      attributes: true,
      attributeFilter: ["title", "placeholder", "aria-label"],
    });
  }

  // ---------------------------------------------------------------------------
  // Language switching
  // ---------------------------------------------------------------------------
  function updateLangButton() {
    const btn = document.getElementById("langToggleBtn");
    if (!btn) return;
    const label = btn.querySelector(".langToggleLabel");
    if (label) label.textContent = isEn() ? "中" : "EN";
    const tip = isEn() ? "切换语言 (Switch language)" : "界面语言 (UI language)";
    btn.title = tip;
    btn.setAttribute("aria-label", tip);
    const menu = document.getElementById("langMenu");
    if (!menu) return;
    for (const opt of menu.querySelectorAll(".langOption")) {
      const active = opt.dataset.lang === currentLang;
      opt.classList.toggle("active", active);
      opt.setAttribute("aria-checked", active ? "true" : "false");
    }
  }

  function closeLangMenu() {
    const sw = document.getElementById("langSwitch");
    const btn = document.getElementById("langToggleBtn");
    if (sw) sw.classList.remove("open");
    if (btn) btn.setAttribute("aria-expanded", "false");
  }

  function initLangMenu() {
    const sw = document.getElementById("langSwitch");
    const btn = document.getElementById("langToggleBtn");
    if (!sw || !btn) return;
    // Click pins the menu open (touch devices have no hover); hover is pure CSS.
    btn.addEventListener("click", (e) => {
      e.stopPropagation();
      const open = !sw.classList.contains("open");
      sw.classList.toggle("open", open);
      btn.setAttribute("aria-expanded", open ? "true" : "false");
    });
    for (const opt of sw.querySelectorAll(".langOption")) {
      opt.addEventListener("click", () => {
        setLang(opt.dataset.lang);
        closeLangMenu();
      });
    }
    document.addEventListener("click", (e) => {
      if (!sw.contains(e.target)) closeLangMenu();
    });
    document.addEventListener("keydown", (e) => {
      if (e.key === "Escape") closeLangMenu();
    });
  }

  function setLang(next) {
    if (!SUPPORTED.includes(next) || next === currentLang) return;
    currentLang = next;
    saveLang(next);
    document.documentElement.lang = next === "en" ? "en" : "zh-CN";
    // Static markup: restore Chinese keys or apply English.
    applyStatic(document);
    if (isEn()) {
      translateTree(document.body);
    } else {
      // Back to Chinese: reverse the dictionary for what's on screen, then ask
      // app.js to re-render its dynamic views — every render path re-emits the
      // Chinese source strings, including the interpolated ones the pattern
      // layer translated and cannot be reversed.
      restoreDynamicChinese(document.body);
      if (typeof window.refreshAll === "function") {
        Promise.resolve(window.refreshAll()).catch(() => {});
      }
    }
    updateLangButton();
    document.dispatchEvent(new CustomEvent("gs:lang-changed", { detail: { lang: next } }));
  }

  // Reverse dictionary lookup for zh restoration of dynamic nodes.
  function buildReverseMap() {
    if (!dictLookup) {
      dictLookup = new Map();
      for (const [zh, en] of Object.entries(DICT)) {
        if (en) dictLookup.set(en, zh);
      }
    }
    return dictLookup;
  }

  function restoreDynamicChinese(root) {
    const reverse = buildReverseMap();
    const walker = document.createTreeWalker(root, NodeFilter.SHOW_TEXT, {
      acceptNode(node) {
        const parent = node.parentElement;
        if (!parent) return NodeFilter.FILTER_REJECT;
        const tag = parent.tagName;
        if (tag === "SCRIPT" || tag === "STYLE" || tag === "TEXTAREA") return NodeFilter.FILTER_REJECT;
        if (parent.closest && parent.closest(EXCLUDE_SELECTOR)) {
          return NodeFilter.FILTER_REJECT;
        }
        return NodeFilter.FILTER_ACCEPT;
      },
    });
    const batch = [];
    while (walker.nextNode()) batch.push(walker.currentNode);
    for (const node of batch) {
      const value = node.nodeValue;
      if (!value) continue;
      const trimmed = value.trim();
      const zh = reverse.get(trimmed);
      if (zh !== undefined) {
        const lead = value.slice(0, value.length - value.trimStart().length);
        const tail = value.slice(value.trimEnd().length);
        node.nodeValue = lead + zh + tail;
      }
    }
    if (root.querySelectorAll) {
      for (const el of root.querySelectorAll("[title],[placeholder],[aria-label]")) {
        for (const attr of TEXT_ATTRS) {
          const value = el.getAttribute(attr);
          if (!value) continue;
          const zh = reverse.get(value);
          if (zh !== undefined) el.setAttribute(attr, zh);
        }
      }
    }
  }

  function toggle() {
    setLang(isEn() ? "zh" : "en");
  }

  function init() {
    document.documentElement.lang = isEn() ? "en" : "zh-CN";
    applyStatic(document);
    if (isEn()) {
      translateTree(document.body);
      startObserver();
    } else {
      // Observer only matters in English; Chinese is the source language.
      startObserver(); // harmless: callbacks no-op unless isEn()
    }
    initLangMenu();
    updateLangButton();
  }

  // Public API
  window.I18N = {
    t,
    tf,
    lang,
    setLang,
    toggle,
    apply: applyStatic,
    translateTree,
    isEn,
  };

  if (document.readyState === "loading") {
    document.addEventListener("DOMContentLoaded", init);
  } else {
    init();
  }
})();
