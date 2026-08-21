const state = {
  profiles: [],
  settings: null,
  status: null,
  grokAuth: null,
  grokPool: null,
  registrar: null,
  lanAccess: null,
  availableModels: [],
  imageAvailableModels: [],
  backups: [],
  showAdvanced: false,
  view: "home",
  layout: localStorage.getItem("gs_layout") || "card",
  search: "",
  draggedProviderKey: "",
  agentStatus: null,
  agentMessages: [],
  agentSessions: [],
  activeAgentSession: null,
  agentEngineState: "none",
  agentFallbackSessionReady: false,
  agentNeedsBootstrap: false,
  agentPermission: null,
  agentPlan: null,
  lastUserMessage: "",
  chatStickToBottom: true,
  agentAutoRestoring: false,
  pendingAttachments: [],
  projects: [],
  activeProjectId: "",
  /** projectId -> expanded (default true when missing) */
  expandedProjects: {},
  /** orphan workspace pathKey -> expanded (default true) */
  expandedOrphanWorkspaces: {},
  orphanSessionsOpen: true,
  update: null,
  updateHidden: false,
  // Monotonic token so out-of-order session switches discard stale UI updates.
  sessionSwitchToken: 0,
};

const OFFICIAL_PROVIDER_KEY = "official";

const $ = (id) => document.getElementById(id);
let toastTimer = null;
let refreshTimer = null;
let grokPoolPollTimer = null;
let registrarPollTimer = null;
let registrarTerminalNotice = "";
let registrarFormDirty = false;
let cpaMintPollTimer = null;
let cpaMintSession = null;
let cpaMintTerminalNotice = "";
let agentSocket = null;
let agentReconnectTimer = null;

// Account list state for viewAccounts (paginated/grouped, supports thousands).
const ACCOUNT_FILTERS = [
  { key: "", label: "全部" },
  { key: "healthy", label: "健康" },
  { key: "quota_exhausted", label: "额度用尽" },
  { key: "permission_denied", label: "权限被拒" },
  { key: "reauth", label: "需重新登录" },
  { key: "abnormal", label: "异常" },
  { key: "uninspected", label: "待巡检" },
  { key: "disabled", label: "已禁用" },
];
let accountListQuery = "";
let accountListFilter = "";
let accountListSort = "recent";
let accountListPage = 1;
const ACCOUNT_LIST_PAGE_SIZE = 100;
let accountListTotal = 0;
let accountSearchTimer = null;
let accountListInFlight = false;
let agentActiveAssistant = null;
let agentActiveThought = null;
let agentRetryNotice = null;
let agentSessionSearchTimer = null;
const MERMAID_SRC = "/vendor/mermaid.min.js?v=11.16.0";
let mermaidReady = false;
let mermaidRenderID = 0;
let mermaidLoadPromise = null;
let chatLayoutResizeTimer = null;
let lastAssistantMessageEl = null;
let composerConfigLoaded = false;
let composerConfigPromise = null;
let composerModelOverride = "";
let composerStrengthOverride = "";
let composerConfig = {
  models: [],
  defaultModel: "",
  defaultStrength: "",
  reasoningEfforts: {},
};
const chatNodes = []; // ordered list of { id, article, el } for navigation jumps

const HISTORY_PAGE_SIZE = 60; // messages rendered per incremental window
let pendingHistory = [];     // full message list for the current restored session
// Inclusive start / exclusive end of the slice currently mounted in the DOM.
let historyRenderedStart = 0;
let historyRenderedEnd = 0;
// When set, history mount helpers append into this fragment/element instead of #chatMessages.
let historyMountTarget = null;
let historyMountSilent = false; // suppress scroll / last-assistant tracking while mounting
let findMatches = [];        // articles matching the current find query
let findIndex = -1;
const agentTools = new Map();
const LAST_CHAT_CONTEXT_KEY = "gs_last_chat_context_v1";

const CHAT_PANEL_LAYOUT = {
  left: {
    property: "--session-sidebar-width",
    storage: "gs_chat_sidebar_width",
    panel: "sessionSidebar",
    resizer: "sessionSidebarResizer",
    defaultWidth: 264,
    minWidth: 200,
    maxWidth: 480,
  },
  right: {
    property: "--context-rail-width",
    storage: "gs_chat_context_width",
    panel: "contextRail",
    resizer: "contextRailResizer",
    defaultWidth: 252,
    minWidth: 220,
    maxWidth: 460,
  },
};

const CHAT_THEME_CONFIG_KEY = "gs_chat_theme_v1";
const CHAT_THEME_IMAGE_KEY = "gs_chat_theme_image_v1";
const CHAT_THEME_MODES = new Set(["none", "frost", "night", "warm", "custom"]);
const DEFAULT_CHAT_THEME = Object.freeze({
  version: 2,
  mode: "none",
  shade: 24,
  blur: 0,
  focusX: 50,
  focusY: 50,
  imageName: "",
});
let chatTheme = { ...DEFAULT_CHAT_THEME };
let chatThemeImageData = "";

function clampThemeNumber(value, minimum, maximum, fallback) {
  const number = Number(value);
  return Number.isFinite(number) ? Math.min(maximum, Math.max(minimum, number)) : fallback;
}

function normalizeChatTheme(value) {
  const source = value && typeof value === "object" ? value : {};
  const mode = CHAT_THEME_MODES.has(source.mode) ? source.mode : DEFAULT_CHAT_THEME.mode;
  const legacyCustom = mode === "custom" && Number(source.version || 1) < 2;
  return {
    version: 2,
    mode,
    shade: legacyCustom ? Math.min(8, Math.round(clampThemeNumber(source.shade, 0, 70, 8))) : Math.round(clampThemeNumber(source.shade, 0, 70, DEFAULT_CHAT_THEME.shade)),
    blur: legacyCustom ? 0 : Math.round(clampThemeNumber(source.blur, 0, 20, DEFAULT_CHAT_THEME.blur)),
    focusX: Math.round(clampThemeNumber(source.focusX, 0, 100, DEFAULT_CHAT_THEME.focusX)),
    focusY: Math.round(clampThemeNumber(source.focusY, 0, 100, DEFAULT_CHAT_THEME.focusY)),
    imageName: typeof source.imageName === "string" ? source.imageName.slice(0, 120) : "",
  };
}

function readChatThemeStorage(key) {
  try {
    return localStorage.getItem(key) || "";
  } catch {
    return "";
  }
}

function loadChatTheme() {
  try {
    const raw = readChatThemeStorage(CHAT_THEME_CONFIG_KEY);
    return normalizeChatTheme(raw ? JSON.parse(raw) : DEFAULT_CHAT_THEME);
  } catch {
    return { ...DEFAULT_CHAT_THEME };
  }
}

function persistChatTheme() {
  try {
    localStorage.setItem(CHAT_THEME_CONFIG_KEY, JSON.stringify(chatTheme));
  } catch {
    // A disabled or full local store should not prevent the chat UI from working.
  }
}

function chatThemeImageCSS() {
  if (!chatThemeImageData) {
    return "linear-gradient(135deg, #d9dce2, #aa9bc8)";
  }
  return `url(${JSON.stringify(chatThemeImageData)})`;
}

function syncChatThemeControls() {
  document.querySelectorAll("[data-chat-theme-choice]").forEach((button) => {
    button.setAttribute("aria-pressed", String(button.dataset.chatThemeChoice === chatTheme.mode));
  });
  const values = {
    chatThemeShade: chatTheme.shade,
    chatThemeBlur: chatTheme.blur,
    chatThemeFocusX: chatTheme.focusX,
    chatThemeFocusY: chatTheme.focusY,
  };
  for (const [id, value] of Object.entries(values)) {
    if ($(id)) $(id).value = String(value);
  }
  if ($("chatThemeShadeValue")) $("chatThemeShadeValue").textContent = `${chatTheme.shade}%`;
  if ($("chatThemeBlurValue")) $("chatThemeBlurValue").textContent = `${chatTheme.blur}px`;
  if ($("chatThemeFocusXValue")) $("chatThemeFocusXValue").textContent = `${chatTheme.focusX}%`;
  if ($("chatThemeFocusYValue")) $("chatThemeFocusYValue").textContent = `${chatTheme.focusY}%`;
  if ($("chatThemeImageName")) {
    $("chatThemeImageName").textContent = chatThemeImageData ? (chatTheme.imageName || "已保存本地图片") : "选择本地图片";
  }
  if ($("clearChatThemeImageBtn")) $("clearChatThemeImageBtn").disabled = !chatThemeImageData;
}

function applyChatTheme(next = chatTheme, persist = false) {
  chatTheme = normalizeChatTheme(next);
  if (chatTheme.mode === "custom" && !chatThemeImageData) chatTheme.mode = "none";
  const root = document.documentElement;
  root.dataset.chatTheme = chatTheme.mode;
  root.style.setProperty("--chat-theme-shade", String(chatTheme.shade / 100));
  root.style.setProperty("--chat-theme-blur", `${chatTheme.blur}px`);
  root.style.setProperty("--chat-theme-position", `${chatTheme.focusX}% ${chatTheme.focusY}%`);
  root.style.setProperty("--chat-theme-custom-image", chatThemeImageCSS());
  if (persist) persistChatTheme();
  syncChatThemeControls();
}

function openChatThemeDialog() {
  const dialog = $("chatThemeDialog");
  if (!dialog) return;
  syncChatThemeControls();
  if (typeof dialog.showModal === "function") dialog.showModal();
  else dialog.setAttribute("open", "");
}

function closeChatThemeDialog() {
  const dialog = $("chatThemeDialog");
  if (!dialog) return;
  if (typeof dialog.close === "function") dialog.close();
  else dialog.removeAttribute("open");
}

function loadThemeImage(file) {
  return new Promise((resolve, reject) => {
    const objectURL = URL.createObjectURL(file);
    const image = new Image();
    image.onload = () => {
      URL.revokeObjectURL(objectURL);
      resolve(image);
    };
    image.onerror = () => {
      URL.revokeObjectURL(objectURL);
      reject(new Error("无法读取这张图片，请选择 PNG、JPEG 或 WebP 文件"));
    };
    image.src = objectURL;
  });
}

function encodeThemeImage(image, maximumEdge, maximumPixels, quality) {
  const pixelScale = Math.sqrt(maximumPixels / (image.naturalWidth * image.naturalHeight));
  const scale = Math.min(1, maximumEdge / image.naturalWidth, maximumEdge / image.naturalHeight, pixelScale);
  const width = Math.max(1, Math.round(image.naturalWidth * scale));
  const height = Math.max(1, Math.round(image.naturalHeight * scale));
  const canvas = document.createElement("canvas");
  canvas.width = width;
  canvas.height = height;
  const context = canvas.getContext("2d", { alpha: false });
  if (!context) throw new Error("当前浏览器无法处理背景图片");
  context.fillStyle = "#e8e7e3";
  context.fillRect(0, 0, width, height);
  context.drawImage(image, 0, 0, width, height);
  return canvas.toDataURL("image/jpeg", quality);
}

async function importChatThemeImage(file) {
  if (!file) return;
  if (file.size > 16 * 1024 * 1024) throw new Error("背景图片不能超过 16 MB");
  const image = await loadThemeImage(file);
  if (image.naturalWidth > 16384 || image.naturalHeight > 16384 || image.naturalWidth * image.naturalHeight > 50_000_000) {
    throw new Error("图片尺寸过大：单边不能超过 16384 像素，总像素不能超过 5000 万");
  }

  const attempts = [
    { edge: 3200, pixels: 6_000_000, quality: 0.92 },
    { edge: 2560, pixels: 4_000_000, quality: 0.88 },
    { edge: 1920, pixels: 2_400_000, quality: 0.78 },
    { edge: 1440, pixels: 1_500_000, quality: 0.72 },
  ];
  let storageError = null;
  for (const attempt of attempts) {
    const data = encodeThemeImage(image, attempt.edge, attempt.pixels, attempt.quality);
    if (data.length > 3_800_000) continue;
    try {
      const replacingCustomImage = chatTheme.mode === "custom" && !!chatThemeImageData;
      localStorage.setItem(CHAT_THEME_IMAGE_KEY, data);
      chatThemeImageData = data;
      applyChatTheme({
        ...chatTheme,
        mode: "custom",
        shade: replacingCustomImage ? chatTheme.shade : 8,
        blur: replacingCustomImage ? chatTheme.blur : 0,
        imageName: file.name,
      }, true);
      toast("聊天背景已保存在当前设备", "success");
      return;
    } catch (err) {
      storageError = err;
    }
  }
  throw new Error(storageError ? "本地存储空间不足，请选择更小的图片" : "图片压缩后仍然过大，请选择尺寸更小的图片");
}

function clearChatThemeImage() {
  try {
    localStorage.removeItem(CHAT_THEME_IMAGE_KEY);
  } catch {
    // Keep reset usable even when storage is unavailable.
  }
  chatThemeImageData = "";
  applyChatTheme({ ...chatTheme, mode: "none", imageName: "" }, true);
  toast("已移除自定义背景", "success");
}

function initialiseChatThemes() {
  chatTheme = loadChatTheme();
  chatThemeImageData = readChatThemeStorage(CHAT_THEME_IMAGE_KEY);
  applyChatTheme(chatTheme);

  document.querySelectorAll("[data-chat-theme-choice]").forEach((button) => {
    button.addEventListener("click", () => {
      const mode = button.dataset.chatThemeChoice;
      if (mode === "custom" && !chatThemeImageData) {
        $("chatThemeImageFile")?.click();
        return;
      }
      applyChatTheme({ ...chatTheme, mode }, true);
    });
  });

  const rangeBindings = {
    chatThemeShade: "shade",
    chatThemeBlur: "blur",
    chatThemeFocusX: "focusX",
    chatThemeFocusY: "focusY",
  };
  for (const [id, field] of Object.entries(rangeBindings)) {
    $(id)?.addEventListener("input", (event) => {
      applyChatTheme({ ...chatTheme, [field]: Number(event.target.value) }, true);
    });
  }

  $("openChatThemeBtn")?.addEventListener("click", openChatThemeDialog);
  $("closeChatThemeBtn")?.addEventListener("click", closeChatThemeDialog);
  $("doneChatThemeBtn")?.addEventListener("click", closeChatThemeDialog);
  $("chooseChatThemeImageBtn")?.addEventListener("click", () => $("chatThemeImageFile")?.click());
  $("clearChatThemeImageBtn")?.addEventListener("click", clearChatThemeImage);
  $("chatThemeImageFile")?.addEventListener("change", async (event) => {
    const file = event.target.files?.[0];
    event.target.value = "";
    if (!file) return;
    try {
      await importChatThemeImage(file);
    } catch (err) {
      toast(err.message || String(err), "error");
    }
  });
  $("chatThemeDialog")?.addEventListener("click", (event) => {
    if (event.target === $("chatThemeDialog")) closeChatThemeDialog();
  });
  window.addEventListener("storage", (event) => {
    if (event.key !== CHAT_THEME_CONFIG_KEY && event.key !== CHAT_THEME_IMAGE_KEY) return;
    chatThemeImageData = readChatThemeStorage(CHAT_THEME_IMAGE_KEY);
    applyChatTheme(loadChatTheme());
  });
}

const TEMPLATES = {
  openai: {
    name: "OpenAI 兼容",
    upstream_format: "openai_chat",
    base_url: "https://api.openai.com/v1",
    default_model: "",
    web_search_model: "",
    subagents_models: { explore: "", plan: "" },
    models: [],
    available_models: [],
  },
  responses: {
    name: "OpenAI Responses",
    upstream_format: "openai_responses",
    base_url: "https://api.openai.com/v1",
    default_model: "",
    web_search_model: "",
    subagents_models: { explore: "", plan: "" },
    models: [],
    available_models: [],
  },
  anthropic: {
    name: "Anthropic",
    upstream_format: "anthropic",
    base_url: "https://api.anthropic.com",
    default_model: "",
    web_search_model: "",
    subagents_models: { explore: "", plan: "" },
    models: [],
    available_models: [],
  },
};

/** Normalize profile subagents models (supports legacy subagents_default_model). */
function subagentsModelsOf(profile) {
  const models = profile?.subagents_models || {};
  let explore = models.explore || "";
  let plan = models.plan || "";
  if (!explore && !plan && profile?.subagents_default_model) {
    explore = profile.subagents_default_model;
    plan = profile.subagents_default_model;
  }
  return { explore, plan };
}

const TEMPLATE_KEYS = new Set(["custom", ...Object.keys(TEMPLATES)]);

function newProfileDraft() {
  return {
    template: "responses",
    upstream_format: "openai_responses",
    models: [],
    available_models: [],
    image_generation: { enabled: false, api_backend: "chat_completions", available_models: [] },
  };
}

function imageGenerationOf(profile) {
  if (profile?.image_generation && typeof profile.image_generation === "object") {
    return {
      enabled: !!profile.image_generation.enabled,
      base_url: profile.image_generation.base_url || "",
      api_key: profile.image_generation.api_key || "",
      api_backend: profile.image_generation.api_backend || "chat_completions",
      model: profile.image_generation.model || "",
      available_models: [...(profile.image_generation.available_models || [])],
    };
  }
  const legacy = profile?.feature_models || {};
  const selected = profile?.media_models?.["grok-imagine-image"] || legacy.image_gen || "";
  const model = (profile?.models || []).find((item) => item.name === selected || item.model === selected || item.name === "grok-imagine-image");
  return {
    enabled: !!profile?.features?.image_gen,
    base_url: model?.base_url || profile?.base_url || "",
    api_key: model?.api_key || profile?.api_key || "",
    api_backend: model?.api_backend || "chat_completions",
    model: model?.model || selected,
    available_models: [],
  };
}

async function api(path, options = {}) {
  const { headers: extraHeaders, ...rest } = options || {};
  const res = await fetch(path, {
    headers: { "Content-Type": "application/json", ...(extraHeaders || {}) },
    ...rest,
  });
  if (rest.signal?.aborted) {
    const abortError = new Error("请求已取消");
    abortError.name = "AbortError";
    abortError.code = "aborted";
    throw abortError;
  }
  const data = await res.json().catch(() => ({}));
  if (!res.ok) {
    const error = new Error(data.error || res.statusText || "请求失败");
    error.code = data.code || "";
    error.status = res.status;
    error.data = data;
    throw error;
  }
  return data;
}

function isAbortError(err) {
  return !!err && (err.name === "AbortError" || err.code === "aborted");
}

function toast(message, type = "info") {
  const el = $("toast");
  el.textContent = message;
  el.classList.remove("error", "success", "show");
  if (type === "error") el.classList.add("error");
  if (type === "success") el.classList.add("success");
  requestAnimationFrame(() => el.classList.add("show"));
  clearTimeout(toastTimer);
  toastTimer = setTimeout(() => el.classList.remove("show"), type === "error" ? 4200 : 2800);
}

function setBusy(button, busy, labelWhenBusy) {
  if (!button) return;
  if (busy) {
    if (!button.dataset.label) button.dataset.label = button.textContent;
    button.disabled = true;
    button.classList.add("busy");
    if (labelWhenBusy) button.textContent = labelWhenBusy;
  } else {
    button.disabled = false;
    button.classList.remove("busy");
    if (button.dataset.label) {
      button.textContent = button.dataset.label;
      delete button.dataset.label;
    }
  }
}

async function run(fn, { button, busyLabel, success } = {}) {
  try {
    setBusy(button, true, busyLabel);
    const result = await fn();
    if (result === false) return;
    if (success) toast(success, "success");
  } catch (err) {
    toast(err.message || String(err), "error");
  } finally {
    setBusy(button, false);
  }
}

// ---- 界面主题（light / dark / auto）----
// 设置持久化在 settings.theme；localStorage 仅用于首屏防闪烁的即时恢复。
const THEME_STORAGE_KEY = "gs_theme_v1";
const themeMedia = window.matchMedia("(prefers-color-scheme: dark)");
const THEME_ICONS = {
  light: '<svg viewBox="0 0 20 20" width="18" height="18" aria-hidden="true"><circle cx="10" cy="10" r="4.2" fill="currentColor"/><g stroke="currentColor" stroke-width="1.6" stroke-linecap="round"><line x1="10" y1="1.6" x2="10" y2="3.6"/><line x1="10" y1="16.4" x2="10" y2="18.4"/><line x1="1.6" y1="10" x2="3.6" y2="10"/><line x1="16.4" y1="10" x2="18.4" y2="10"/><line x1="4" y1="4" x2="5.4" y2="5.4"/><line x1="14.6" y1="14.6" x2="16" y2="16"/><line x1="4" y1="16" x2="5.4" y2="14.6"/><line x1="14.6" y1="5.4" x2="16" y2="4"/></g></svg>',
  dark: '<svg viewBox="0 0 20 20" width="18" height="18" aria-hidden="true"><path fill="currentColor" d="M17.2 12.4A7.6 7.6 0 0 1 7.6 2.8 7.6 7.6 0 1 0 17.2 12.4Z"/></svg>',
  auto: '<svg viewBox="0 0 20 20" width="18" height="18" aria-hidden="true"><circle cx="10" cy="10" r="7.6" fill="none" stroke="currentColor" stroke-width="1.6"/><path fill="currentColor" d="M10 2.4a7.6 7.6 0 0 1 0 15.2Z"/></svg>',
};
const THEME_TITLES = {
  light: "主题：亮色（点击切换为暗色）",
  dark: "主题：暗色（点击切换为跟随系统）",
  auto: "主题：跟随系统（点击切换为亮色）",
};

function themeSetting() {
  const value = (state.settings && state.settings.theme) ||
    localStorage.getItem(THEME_STORAGE_KEY) || "light";
  return value === "dark" || value === "auto" ? value : "light";
}

function applyTheme() {
  const setting = themeSetting();
  const resolved = setting === "auto" && themeMedia.matches ? "dark" : setting === "dark" ? "dark" : "light";
  document.documentElement.dataset.theme = resolved;
  localStorage.setItem(THEME_STORAGE_KEY, setting);
  const button = $("themeToggleBtn");
  if (button) {
    button.innerHTML = THEME_ICONS[setting];
    button.title = THEME_TITLES[setting];
    button.setAttribute("aria-label", THEME_TITLES[setting]);
  }
}

async function cycleTheme() {
  const order = ["light", "dark", "auto"];
  const next = order[(order.indexOf(themeSetting()) + 1) % order.length];
  state.settings = await api("/api/settings", {
    method: "PUT",
    body: JSON.stringify({ ...(state.settings || {}), theme: next }),
  });
  applyTheme();
}

themeMedia.addEventListener("change", () => {
  if (themeSetting() === "auto") applyTheme();
});

async function refreshAll() {
  const [status, profiles, backups, settings, grokAuth, grokPool, registrar, lanAccess] = await Promise.all([
    api("/api/status"),
    api("/api/profiles"),
    api("/api/backups"),
    api("/api/settings"),
    api("/api/grok-auth"),
    api("/api/grok-pool"),
    api("/api/registrar"),
    api("/api/lan-access"),
  ]);
  state.status = status;
  state.profiles = profiles;
  state.backups = backups;
  state.settings = settings;
  state.grokAuth = grokAuth;
  state.grokPool = grokPool;
  state.registrar = registrar;
  state.lanAccess = lanAccess;
  composerConfigLoaded = false;
  applyTheme();
  // Coerce to strict boolean for UI.
  if (state.status && typeof state.status.config_matches_active !== "boolean") {
    state.status.config_matches_active = true;
  }
  renderDrift();
  renderEmptyState();
  renderProfiles();
  populateComposerModelSelect();
  renderBackups(backups);
  renderSettings(settings);
  renderGrokAuth(grokAuth);
  renderGrokPool(grokPool);
  renderRegistrar(registrar);
  renderLANAccess(lanAccess);
  syncAdvancedUI();
  const detail = [];
  if (state.status?.config_path) detail.push(state.status.config_path);
  if (state.status?.port) detail.push(`端口 ${state.status.port}`);
  if (state.status?.version) detail.push(`版本 ${state.status.version}`);
  if ($("statusDetail")) $("statusDetail").textContent = detail.join(" · ");
}

function renderUpdate(info) {
  const banner = $("updateBanner");
  if (!banner) return;
  const available = !!info?.update_available && !info?.skipped && !!info?.latest_version;
  if (!available || state.updateHidden) {
    banner.hidden = true;
    banner.style.display = "none";
    return;
  }
  $("updateVersion").textContent = info.latest_version;
  $("updateDetail").textContent = info.release_name || "最新版已发布，可直接下载更新。";
  const download = $("updateDownloadBtn");
  const release = $("updateReleaseBtn");
  download.href = info.download_url || info.release_url || "https://github.com/1parado/grok-build-switch/releases/latest";
  release.href = info.release_url || "https://github.com/1parado/grok-build-switch/releases/latest";
  banner.hidden = false;
  banner.style.display = "flex";
}

async function checkForUpdates() {
  try {
    const info = await api("/api/update");
    state.update = info;
    state.updateHidden = false;
    renderUpdate(info);
  } catch {
    // Update checks are best-effort and must not block the management UI.
  }
}

const SKILL_SOURCE_META = {
  "agents/skills": { label: "Agent Skills", path: "~/.agents/skills", tone: "agents", short: "Agent" },
  "grok/skills": { label: "用户 Skills", path: "~/.grok/skills", tone: "user", short: "用户" },
  "grok/bundled": { label: "内置 Skills", path: "~/.grok/bundled/skills", tone: "bundled", short: "内置" },
  agents: { label: "Agent Skills", path: "~/.agents", tone: "agents", short: "Agent" },
  "grok/.skills": { label: "用户 Skills", path: "~/.grok/.skills", tone: "user", short: "用户" },
  "grok/agents": { label: "Grok Agents", path: "~/.grok/agents", tone: "user", short: "Agents" },
};

let skillsCache = [];
let skillsSearchQuery = "";
const skillsGroupExpanded = new Set();

function skillSourceMeta(source) {
  return SKILL_SOURCE_META[source] || {
    label: source || "其他",
    path: source || "",
    tone: "other",
    short: source || "其他",
  };
}

function skillMatchesQuery(sk, query) {
  if (!query) return true;
  const q = query.toLowerCase();
  return [sk.name, sk.path, sk.source]
    .filter(Boolean)
    .some((part) => String(part).toLowerCase().includes(q));
}

function updateSkillsCount(total, shown) {
  const el = $("skillsCount");
  if (!el) return;
  if (!total) {
    el.textContent = "0 个";
    return;
  }
  el.textContent = shown === total ? `${total} 个` : `${shown} / ${total} 个`;
}

function renderSkillsList() {
  const list = $("skillsList");
  const empty = $("skillsEmpty");
  const searchEmpty = $("skillsSearchEmpty");
  if (!list) return;

  const query = skillsSearchQuery.trim();
  const filtered = skillsCache.filter((sk) => skillMatchesQuery(sk, query));
  updateSkillsCount(skillsCache.length, filtered.length);

  if (empty) empty.hidden = skillsCache.length > 0;
  if (searchEmpty) searchEmpty.hidden = !(skillsCache.length > 0 && filtered.length === 0);

  if (!filtered.length) {
    list.innerHTML = "";
    return;
  }

  const groups = {};
  const order = [];
  for (const sk of filtered) {
    const src = sk.source || "other";
    if (!groups[src]) {
      groups[src] = [];
      order.push(src);
    }
    groups[src].push(sk);
  }

  let html = "";
  for (const [groupIndex, source] of order.entries()) {
    const items = groups[source];
    const meta = skillSourceMeta(source);
    const expanded = skillsGroupExpanded.has(source);
    const groupID = `skills-group-${groupIndex}`;
    html += `<section class="skillsGroup" data-tone="${escapeHtml(meta.tone)}">
      <button type="button" class="skillsGroupHead" data-source="${escapeHtml(source)}" aria-expanded="${expanded ? "true" : "false"}" aria-controls="${groupID}">
        <span class="skillsGroupHeadLeft">
          <span class="skillsGroupDisclosure" aria-hidden="true"></span>
          <span class="skillsGroupHeadCopy">
            <span class="skillsGroupTitle">${escapeHtml(meta.label)}</span>
            <span class="skillsGroupPath">${escapeHtml(meta.path)}</span>
          </span>
        </span>
        <span class="skillsGroupCount">${items.length}</span>
      </button>
      <div id="${groupID}" class="skillsItems"${expanded ? "" : " hidden"}>`;
    for (const sk of items) {
      const isBundled = sk.source === "grok/bundled";
      const kind = sk.is_dir ? "dir" : "file";
      html += `<div class="skillItem" data-kind="${kind}">
        <span class="skillIcon" aria-hidden="true"><img class="skillIconSvg" src="/skill.svg" alt=""></span>
        <div class="skillInfo">
          <div class="skillNameRow">
            <strong class="skillName">${escapeHtml(sk.name)}</strong>
            <span class="skillKindBadge">${sk.is_dir ? "目录" : "文件"}</span>
          </div>
          <span class="skillPath" title="${escapeHtml(sk.path)}">${escapeHtml(sk.path)}</span>
        </div>
        <div class="skillActions">
          <button type="button" class="btn sm ghost copySkillPathBtn" data-path="${escapeHtml(sk.path)}" title="复制路径">复制</button>
          ${isBundled
            ? `<span class="skillReadonlyBadge" title="内置技能不可删除">只读</span>`
            : `<button type="button" class="btn sm danger deleteSkillBtn" data-path="${escapeHtml(sk.path)}" data-name="${escapeHtml(sk.name)}" title="删除">删除</button>`}
        </div>
      </div>`;
    }
    html += `</div></section>`;
  }

  list.innerHTML = html;
  list.querySelectorAll(".skillsGroupHead").forEach((button) => {
    button.onclick = () => {
      const source = button.dataset.source || "";
      const panel = document.getElementById(button.getAttribute("aria-controls"));
      if (!panel) return;
      const expanded = button.getAttribute("aria-expanded") === "true";
      button.setAttribute("aria-expanded", expanded ? "false" : "true");
      panel.hidden = expanded;
      if (expanded) skillsGroupExpanded.delete(source);
      else skillsGroupExpanded.add(source);
    };
  });
  list.querySelectorAll(".copySkillPathBtn").forEach((btn) => {
    btn.onclick = () => {
      navigator.clipboard.writeText(btn.dataset.path).then(() => {
        toast("路径已复制", "success");
      }).catch(() => {
        toast("复制失败", "error");
      });
    };
  });
  list.querySelectorAll(".deleteSkillBtn").forEach((btn) => {
    btn.onclick = async () => {
      const name = btn.dataset.name;
      const path = btn.dataset.path;
      if (!confirm(`确定要删除 "${name}" 吗？\n\n路径: ${path}\n\n此操作不可恢复。`)) {
        return;
      }
      btn.disabled = true;
      btn.textContent = "删除中…";
      try {
        await api("/api/skills/delete", {
          method: "POST",
          body: JSON.stringify({ path }),
        });
        toast(`已删除: ${name}`, "success");
        await loadSkills();
        await loadSkillsForPopup();
      } catch (err) {
        toast(err.message || "删除失败", "error");
        btn.disabled = false;
        btn.textContent = "删除";
      }
    };
  });
}

async function loadSkills() {
  const loading = $("skillsLoading");
  const list = $("skillsList");
  const empty = $("skillsEmpty");
  const searchEmpty = $("skillsSearchEmpty");
  if (!list) return;
  try {
    if (loading) loading.hidden = false;
    list.innerHTML = "";
    if (empty) empty.hidden = true;
    if (searchEmpty) searchEmpty.hidden = true;
    const skills = await api("/api/skills");
    skillsCache = Array.isArray(skills) ? skills : [];
    if (loading) loading.hidden = true;
    renderSkillsList();
  } catch (err) {
    if (loading) loading.hidden = true;
    skillsCache = [];
    updateSkillsCount(0, 0);
    if (empty) empty.hidden = true;
    if (searchEmpty) searchEmpty.hidden = true;
    list.innerHTML = `<div class="alert warn"><strong>加载失败</strong><span>${escapeHtml(err.message || String(err))}</span></div>`;
  }
}

function activeProfile() {
  return state.profiles.find((p) => p.is_active) || state.status?.active_profile || null;
}

function renderDrift() {
  const banner = $("driftBanner");
  if (!banner) return;
  const matches = state.status?.config_matches_active;
  const activeID = state.status?.active_profile?.id;
  // Strict: only show for explicit boolean false.
  const drifted = Boolean(activeID) && matches === false;
  banner.hidden = !drifted;
  banner.style.display = drifted ? "" : "none";
}

async function loadConfigEditor() {
  const data = await api("/api/config");
  if ($("configPathLabel")) {
    $("configPathLabel").textContent = data.path || "";
  }
  if ($("configEditor")) {
    $("configEditor").value = data.content ?? "";
  }
  if ($("configEditorStatus")) {
    $("configEditorStatus").textContent = data.exists === false ? "文件尚不存在，保存后将创建。" : "已加载";
  }
}

async function saveConfigEditor(button) {
  await run(async () => {
    const content = $("configEditor")?.value ?? "";
    await api("/api/config", {
      method: "PUT",
      body: JSON.stringify({ content }),
    });
    await refreshAll();
    await loadConfigEditor();
  }, { button, busyLabel: "保存中…", success: "config.toml 已保存（已自动备份）" });
}

let previewTimer = null;
async function refreshProviderConfigPreview() {
  const status = $("providerConfigPreviewStatus");
  const area = $("providerConfigPreview");
  if (!area) return;
  try {
    const profile = readForm();
    if (!profile.base_url && !profile.name) {
      area.value = "";
      if (status) status.textContent = "先填写名称与服务地址";
      return;
    }
    if (status) status.textContent = "生成预览…";
    const data = await api("/api/config/preview", {
      method: "POST",
      body: JSON.stringify(profile),
    });
    const full = $("previewFullConfig")?.checked;
    area.value = full ? (data.full || "") : (data.snippet || "");
    if (status) {
      status.textContent = full
        ? `合并到 ${data.path || "config.toml"} 后的完整文件预览（未保存）`
        : "仅显示此供应商会覆盖的段落";
    }
  } catch (err) {
    if (status) status.textContent = err.message || String(err);
  }
}

function scheduleProviderPreview() {
  clearTimeout(previewTimer);
  previewTimer = setTimeout(() => {
    if (state.view === "edit" && $("configPreviewBlock")?.open) {
      refreshProviderConfigPreview();
    }
  }, 400);
}

function showView(name) {
  state.view = name;
  document.body.classList.toggle("chatMode", name === "chat");
  const home = $("viewHome");
  const edit = $("viewEdit");
  const settings = $("viewSettings");
  const chat = $("viewChat");
  const skills = $("viewSkills");
  const accounts = $("viewAccounts");
  if (home) {
    home.hidden = name !== "home";
    home.style.display = name === "home" ? "" : "none";
  }
  if (edit) {
    edit.hidden = name !== "edit";
    edit.style.display = name === "edit" ? "" : "none";
  }
  if (settings) {
    settings.hidden = name !== "settings";
    settings.style.display = name === "settings" ? "" : "none";
  }
  if (accounts) {
    accounts.hidden = name !== "accounts";
    accounts.style.display = name === "accounts" ? "" : "none";
  }
  if (chat) {
    chat.hidden = name !== "chat";
    chat.style.display = name === "chat" ? "" : "none";
  }
  if (skills) {
    skills.hidden = name !== "skills";
    skills.style.display = name === "skills" ? "" : "none";
  }
  const imagine = $("viewImagine");
  if (imagine) {
    imagine.hidden = name !== "imagine";
    imagine.style.display = name === "imagine" ? "" : "none";
  }
  if ($("navHomeBtn")) $("navHomeBtn").hidden = name === "home";
  document.querySelectorAll("[data-home-only]").forEach((el) => {
    el.hidden = name !== "home";
  });
  // Keep header add/import only on home list.
  if ($("headerSubtitle")) {
    $("headerSubtitle").textContent =
      name === "settings" ? "设置" : name === "accounts" ? "账号管理" : name === "edit" ? ( $("profileId")?.value ? "编辑供应商" : "添加供应商") : name === "chat" ? "对话" : name === "skills" ? "Skills" : "供应商";
  }
  if (name === "settings") {
    loadConfigEditor().catch((err) => toast(err.message, "error"));
  }
  if (name === "accounts") {
    loadAccountsView().catch((err) => toast(err.message, "error"));
  }
  if (name === "skills") {
    loadSkills().catch((err) => toast(err.message, "error"));
  }
  if (name === "imagine") {
    refreshImagineStatus().catch(() => {});
  }
  if (name === "chat") {
    openAgentView().catch((err) => toast(err.message, "error"));
    loadSkillsForPopup().catch(() => {});
  } else {
    closeNativeChatPanels();
  }
}

function readLastChatContext() {
  try {
    const raw = localStorage.getItem(LAST_CHAT_CONTEXT_KEY);
    if (!raw) return null;
    const parsed = JSON.parse(raw);
    if (!parsed || typeof parsed !== "object") return null;
    return {
      sessionId: typeof parsed.sessionId === "string" ? parsed.sessionId : "",
      cwd: typeof parsed.cwd === "string" ? parsed.cwd : "",
      title: typeof parsed.title === "string" ? parsed.title : "",
      model: typeof parsed.model === "string" ? parsed.model : "",
      alwaysApprove: !!parsed.alwaysApprove,
    };
  } catch {
    return null;
  }
}

function persistLastChatContext(partial = {}) {
  const current = readLastChatContext() || {};
  const next = {
    sessionId: partial.sessionId ?? current.sessionId ?? state.activeAgentSession?.id ?? state.agentStatus?.session_id ?? "",
    cwd: partial.cwd ?? current.cwd ?? state.activeAgentSession?.cwd ?? state.agentStatus?.cwd ?? $("agentCwd")?.value?.trim() ?? "",
    title: partial.title ?? current.title ?? state.activeAgentSession?.title ?? "",
    model: partial.model ?? current.model ?? state.activeAgentSession?.model ?? state.agentStatus?.model ?? "",
    alwaysApprove: partial.alwaysApprove ?? current.alwaysApprove ?? !!$("agentAlwaysApprove")?.checked,
  };
  try {
    localStorage.setItem(LAST_CHAT_CONTEXT_KEY, JSON.stringify(next));
  } catch {
    // Ignore storage failures.
  }
}

async function openAgentView() {
  const [status] = await Promise.all([
    api("/api/agent/status"),
    loadAgentSessions(),
    loadAgentProjects().catch(() => {}),
    loadSkillsForPopup().catch(() => {}),
  ]);
  state.agentStatus = status;
  const last = readLastChatContext();
  const cwdInput = $("agentCwd");
  if (cwdInput && !cwdInput.value.trim()) {
    cwdInput.value = status.cwd || last?.cwd || state.settings?.agent_default_cwd || status.default_cwd || "";
  }
  if ($("agentAlwaysApprove") && last && typeof last.alwaysApprove === "boolean" && !agentIsRunning(status)) {
    $("agentAlwaysApprove").checked = last.alwaysApprove;
  }
  renderAgentStatus(status);
  renderSidebarTree();
  updateConversationIdentity();
  applyStoredChatPanelWidths();
  bindChatScrollTracking();
  updateChatJumpBottomBtn();
  bindChatExtras();
  bindChatEmptyActions();
  renderChatEmptyState(status);
  if (window.matchMedia("(min-width: 821px)").matches) {
    const shell = $("viewChat")?.querySelector(".nativeChatShell");
    shell?.classList.toggle("sidebarCollapsed", localStorage.getItem("gs_chat_sidebar_hidden") === "1");
    // Right context rail defaults collapsed (Grok App style); open via ··· only.
    if (window.matchMedia("(min-width: 1181px)").matches) {
      shell?.classList.add("contextCollapsed");
    }
  }
  bindComposerFloatPad();
  syncComposerAccessSelect();
  updateComposerProjectLabel();
  connectAgentSocket();
  await restoreLastChatContext(status, last);
}

async function restoreLastChatContext(status, last = readLastChatContext()) {
  if (state.agentAutoRestoring) return;
  const running = agentIsRunning(status);
  if (running && status.session_id) {
    if (!state.activeAgentSession || state.activeAgentSession.id !== status.session_id) {
      const match = state.agentSessions.find((item) => item.id === status.session_id);
      state.activeAgentSession = match || {
        id: status.session_id,
        title: last?.title || "当前会话",
        cwd: status.cwd || last?.cwd || "",
        model: status.model || last?.model || "",
      };
      if ($("agentCwd") && status.cwd) $("agentCwd").value = status.cwd;
      if (!$("chatMessages")?.querySelector(".chatMessage")) {
        try {
          const history = await api(`/api/agent/sessions/${encodeURIComponent(status.session_id)}`);
          if (history?.session) state.activeAgentSession = { ...state.activeAgentSession, ...history.session };
          clearAgentTranscript(false);
          renderStoredHistory(history.messages || []);
          setAgentEngineState("attached");
        } catch {
          setAgentEngineState("attached");
        }
      } else {
        setAgentEngineState("attached");
      }
      updateConversationIdentity();
      persistLastChatContext({
        sessionId: status.session_id,
        cwd: status.cwd,
        model: status.model,
        title: state.activeAgentSession?.title,
      });
    }
    return;
  }
  if (running || !status.available) return;
  const cwd = $("agentCwd")?.value.trim() || last?.cwd || state.settings?.agent_default_cwd || status.default_cwd || "";
  if (!cwd) return;
  if ($("agentCwd") && !$("agentCwd").value.trim()) $("agentCwd").value = cwd;
  state.agentAutoRestoring = true;
  try {
    if (last?.sessionId && (!last.cwd || last.cwd === cwd)) {
      const session = state.agentSessions.find((item) => item.id === last.sessionId) || {
        id: last.sessionId,
        title: last.title || "上次会话",
        cwd: last.cwd || cwd,
        model: last.model || "",
      };
      try {
        await resumeAgentSession(session);
        return;
      } catch {
        // Avoid startAgent re-attempting the same broken session load.
        state.activeAgentSession = null;
        setAgentEngineState("none");
      }
    }
    await startAgent();
  } catch (err) {
    // Keep the page usable; user can start manually.
    console.warn("auto restore chat context failed", err);
  } finally {
    state.agentAutoRestoring = false;
  }
}

async function loadAgentSessions(query = $("agentSessionSearch")?.value || "") {
  const sessions = await api(`/api/agent/sessions?limit=100&query=${encodeURIComponent(query.trim())}`);
  state.agentSessions = Array.isArray(sessions) ? sessions : [];
  renderSidebarTree();
  return state.agentSessions;
}

/** Normalize filesystem paths for project ↔ session matching (Windows + Unicode). */
function normalizePathKey(path) {
  let p = String(path || "").trim();
  if (!p) return "";
  // Strip Win32 long-path / extended prefixes.
  p = p.replace(/^\\\\\?\\UNC\\/i, "//").replace(/^\\\\\?\\/i, "");
  p = p.replace(/\\/g, "/");
  // Collapse duplicate slashes except leading // for UNC.
  p = p.replace(/([^:]\/)\/+/g, "$1");
  p = p.replace(/\/+$/, "");
  return p.toLowerCase();
}

function projectPathKeys() {
  const keys = new Map();
  for (const project of state.projects || []) {
    const key = normalizePathKey(project.path);
    if (key) keys.set(key, project);
  }
  return keys;
}

/** Sessions whose cwd equals the project's workspace path. */
function sessionsForProject(project) {
  const key = normalizePathKey(project?.path);
  if (!key) return [];
  return (state.agentSessions || []).filter((session) => normalizePathKey(session.cwd) === key);
}

/** Sessions not belonging to any registered project workspace. */
function orphanSessions() {
  const keys = projectPathKeys();
  return (state.agentSessions || []).filter((session) => {
    const key = normalizePathKey(session.cwd);
    return !key || !keys.has(key);
  });
}

/**
 * Group orphan sessions by workspace cwd (same shape as projects, but unregistered).
 * Returns [{ key, path, name, missing, sessions }] sorted by latest session activity.
 */
function orphanWorkspaceGroups() {
  const groups = new Map();
  for (const session of orphanSessions()) {
    const path = String(session.cwd || "").trim();
    const key = normalizePathKey(path) || "__none__";
    let group = groups.get(key);
    if (!group) {
      group = {
        key,
        path,
        name: path ? workspaceFolderName(path) : "无工作目录",
        missing: !!session.cwd_missing,
        sessions: [],
      };
      groups.set(key, group);
    }
    if (session.cwd_missing) group.missing = true;
    group.sessions.push(session);
  }
  const list = Array.from(groups.values());
  for (const group of list) {
    group.sessions.sort((a, b) => {
      const ta = new Date(a.updated_at || a.created_at || 0).getTime();
      const tb = new Date(b.updated_at || b.created_at || 0).getTime();
      return tb - ta;
    });
  }
  list.sort((a, b) => {
    const ta = new Date(a.sessions[0]?.updated_at || 0).getTime();
    const tb = new Date(b.sessions[0]?.updated_at || 0).getTime();
    return tb - ta;
  });
  return list;
}

function workspaceFolderName(path) {
  const parts = String(path || "").replace(/\\/g, "/").split("/").filter(Boolean);
  return parts[parts.length - 1] || path || "工作空间";
}

function isProjectExpanded(projectId) {
  if (!projectId) return true;
  // Default expanded (grok-app style); only collapse when user toggles off.
  if (Object.prototype.hasOwnProperty.call(state.expandedProjects, projectId)) {
    return !!state.expandedProjects[projectId];
  }
  return true;
}

function setProjectExpanded(projectId, open) {
  if (!projectId) return;
  state.expandedProjects = { ...state.expandedProjects, [projectId]: !!open };
}

function isOrphanWorkspaceExpanded(key) {
  if (!key) return true;
  if (Object.prototype.hasOwnProperty.call(state.expandedOrphanWorkspaces, key)) {
    return !!state.expandedOrphanWorkspaces[key];
  }
  return true;
}

function setOrphanWorkspaceExpanded(key, open) {
  if (!key) return;
  state.expandedOrphanWorkspaces = { ...state.expandedOrphanWorkspaces, [key]: !!open };
}

function renderSidebarTree() {
  renderAgentProjects();
  renderAgentSessionList();
}

function buildSessionItemEl(session, { nested = false, showPath = true } = {}) {
  const button = document.createElement("div");
  const missing = !!session.cwd_missing;
  button.className = `sessionItem${nested ? " sessionItemNested" : ""}${session.id === state.activeAgentSession?.id ? " active" : ""}${missing ? " cwdMissing" : ""}`;
  button.role = "button";
  button.tabIndex = 0;
  button.dataset.sessionId = session.id;
  if (missing) button.title = "工作目录已失效，打开后请在会话信息中修正路径";
  else if (session.cwd) button.title = session.cwd;

  const title = document.createElement("span");
  title.className = "sessionItemTitle";
  title.textContent = session.title || "未命名会话";

  const meta = document.createElement("span");
  meta.className = "sessionItemMeta";
  const metaParts = [formatSessionTime(session.updated_at), session.model];
  if (missing) metaParts.unshift("目录失效");
  meta.textContent = metaParts.filter(Boolean).join(" · ");

  const actions = document.createElement("div");
  actions.className = "sessionItemActions";
  const renameBtn = document.createElement("button");
  renameBtn.type = "button";
  renameBtn.className = "sessionActionBtn sessionRenameBtn";
  renameBtn.title = "重命名会话";
  renameBtn.setAttribute("aria-label", `重命名会话 ${session.title || ""}`);
  renameBtn.textContent = "✎";
  renameBtn.onclick = (event) => {
    event.stopPropagation();
    startRenameSession(session);
  };
  const deleteBtn = document.createElement("button");
  deleteBtn.type = "button";
  deleteBtn.className = "sessionActionBtn sessionDeleteBtn";
  deleteBtn.title = "删除会话";
  deleteBtn.setAttribute("aria-label", `删除会话 ${session.title || ""}`);
  deleteBtn.textContent = "×";
  deleteBtn.onclick = (event) => {
    event.stopPropagation();
    deleteAgentSession(session).catch((err) => toast(err.message || String(err), "error"));
  };
  actions.append(renameBtn, deleteBtn);

  button.append(title, meta);
  if (showPath) {
    const path = document.createElement("span");
    path.className = "sessionItemPath";
    path.textContent = session.cwd || "";
    button.append(path);
  }
  button.append(actions);

  const activate = async () => {
    try {
      button.setAttribute("aria-busy", "true");
      button.classList.add("busy");
      await resumeAgentSession(session);
    } catch (err) {
      if (!isAbortError(err)) toast(err.message || String(err), "error");
    } finally {
      button.removeAttribute("aria-busy");
      button.classList.remove("busy");
    }
  };
  button.onclick = activate;
  button.onkeydown = (event) => {
    if (event.key === "Enter" || event.key === " ") {
      event.preventDefault();
      activate();
    }
  };
  return button;
}

/**
 * Unregistered workspaces: sessions grouped under their cwd, same tree shape as projects.
 * Section label: 其他工作空间.
 */
function renderAgentSessionList() {
  const list = $("agentSessionList");
  if (!list) return;
  list.innerHTML = "";
  const groups = orphanWorkspaceGroups();
  const totalSessions = groups.reduce((n, g) => n + g.sessions.length, 0);
  if ($("agentSessionCount")) $("agentSessionCount").textContent = String(totalSessions);
  const head = $("orphanSessionsHead");
  const toggle = $("toggleOrphanSessionsBtn");
  // Hide the whole block when every session already sits under a registered project.
  if (head) head.hidden = totalSessions === 0;
  if (!totalSessions) {
    list.hidden = true;
    return;
  }
  if (toggle) toggle.setAttribute("aria-expanded", state.orphanSessionsOpen ? "true" : "false");
  const chevron = toggle?.querySelector(".treeChevron");
  if (chevron) chevron.textContent = state.orphanSessionsOpen ? "▾" : "▸";

  if (!state.orphanSessionsOpen) {
    list.hidden = true;
    return;
  }
  list.hidden = false;

  const activePath = normalizePathKey($("agentCwd")?.value || state.activeAgentSession?.cwd || "");

  for (const group of groups) {
    const expanded = isOrphanWorkspaceExpanded(group.key);
    const isActive = !!group.path && normalizePathKey(group.path) === activePath;

    const folder = document.createElement("div");
    folder.className = `projectFolder orphanWorkspaceFolder${isActive ? " is-active" : ""}${group.missing ? " is-missing" : ""}${expanded ? " is-expanded" : ""}`;
    folder.dataset.workspaceKey = group.key;

    const row = document.createElement("div");
    row.className = "projectItem";
    row.role = "button";
    row.tabIndex = 0;
    row.setAttribute("aria-expanded", expanded ? "true" : "false");
    row.title = group.path || "无工作目录";

    const rowChevron = document.createElement("span");
    rowChevron.className = "projectItemChevron";
    rowChevron.setAttribute("aria-hidden", "true");
    rowChevron.textContent = expanded ? "▾" : "▸";

    const body = document.createElement("span");
    body.className = "projectItemBody";
    const name = document.createElement("span");
    name.className = "projectItemName";
    name.textContent = group.name;
    if (group.missing) {
      const badge = document.createElement("span");
      badge.className = "projectItemBadge";
      badge.textContent = "目录失效";
      name.append(" ", badge);
    }
    body.append(name);
    if (group.path) {
      const pathEl = document.createElement("span");
      pathEl.className = "projectItemPath mono";
      pathEl.textContent = group.path;
      body.append(pathEl);
    }

    const count = document.createElement("span");
    count.className = "projectItemCount";
    count.textContent = String(group.sessions.length);
    count.title = `${group.sessions.length} 个会话`;

    const actions = document.createElement("div");
    actions.className = "projectItemActions";

    if (group.path && !group.missing) {
      const newBtn = document.createElement("button");
      newBtn.type = "button";
      newBtn.className = "sessionActionBtn projectNewSessionBtn";
      newBtn.title = "在此工作空间下新建会话";
      newBtn.setAttribute("aria-label", `在 ${group.name} 下新建会话`);
      newBtn.textContent = "✎";
      newBtn.onclick = (event) => {
        event.stopPropagation();
        if ($("agentCwd")) $("agentCwd").value = group.path;
        setOrphanWorkspaceExpanded(group.key, true);
        run(() => newAgentSession(), { busyLabel: "创建中…" }).catch((err) => {
          if (!isAbortError(err)) toast(err.message || String(err), "error");
        });
      };
      actions.append(newBtn);

      const addProjBtn = document.createElement("button");
      addProjBtn.type = "button";
      addProjBtn.className = "sessionActionBtn projectPromoteBtn";
      addProjBtn.title = "添加为项目";
      addProjBtn.setAttribute("aria-label", `将 ${group.name} 添加为项目`);
      addProjBtn.textContent = "＋";
      addProjBtn.onclick = (event) => {
        event.stopPropagation();
        promoteWorkspaceToProject(group.path).catch((err) => toast(err.message || String(err), "error"));
      };
      actions.append(addProjBtn);
    }

    row.append(rowChevron, body, count, actions);

    const toggleExpand = () => {
      setOrphanWorkspaceExpanded(group.key, !isOrphanWorkspaceExpanded(group.key));
      renderSidebarTree();
    };
    row.onclick = (event) => {
      if (event.detail > 1 && group.path) {
        activateOrphanWorkspace(group).catch((err) => toast(err.message || String(err), "error"));
        return;
      }
      toggleExpand();
    };
    row.onkeydown = (event) => {
      if (event.key === "Enter" || event.key === " ") {
        event.preventDefault();
        toggleExpand();
      }
    };

    folder.append(row);

    if (expanded) {
      const children = document.createElement("div");
      children.className = "projectSessionList";
      for (const session of group.sessions) {
        children.append(buildSessionItemEl(session, { nested: true, showPath: false }));
      }
      folder.append(children);
    }

    list.append(folder);
  }
}

async function activateOrphanWorkspace(group) {
  if (!group?.path) {
    toast("该组会话没有有效工作目录", "error");
    return false;
  }
  if ($("agentCwd")) $("agentCwd").value = group.path;
  state.activeProjectId = "";
  setOrphanWorkspaceExpanded(group.key, true);
  updateConversationIdentity();
  toast(`已切换工作空间：${group.name}`, "info");
  return true;
}

/** Register an orphan cwd as a first-class project and move its sessions under 项目. */
async function promoteWorkspaceToProject(path) {
  const cwd = String(path || "").trim();
  if (!cwd) throw new Error("无效工作目录");
  const item = await api("/api/agent/projects", {
    method: "POST",
    body: JSON.stringify({ path: cwd, trusted: true }),
  });
  await loadAgentProjects();
  if (item?.id) {
    await openProjectById(item.id);
    setProjectExpanded(item.id, true);
  }
  renderSidebarTree();
  toast(`已添加项目：${workspaceFolderName(cwd)}`, "success");
}

function formatSessionTime(value) {
  if (!value) return "";
  const date = new Date(value);
  if (Number.isNaN(date.getTime())) return "";
  const now = new Date();
  if (date.toDateString() === now.toDateString()) {
    return date.toLocaleTimeString([], { hour: "2-digit", minute: "2-digit" });
  }
  return date.toLocaleDateString([], { month: "2-digit", day: "2-digit" });
}

async function startRenameSession(session) {
  if (!session?.id) return;
  const next = window.prompt("重命名会话", session.title || "");
  if (next === null) return;
  const title = next.trim();
  if (title === (session.title || "").trim() && title !== "") return;
  try {
    await api("/api/agent/session/rename", {
      method: "POST",
      body: JSON.stringify({ session_id: session.id, title }),
    });
    session.title = title || "未命名会话";
    if (state.activeAgentSession?.id === session.id) {
      state.activeAgentSession.title = session.title;
      updateConversationIdentity();
      persistLastChatContext({ title: session.title });
    }
    renderSidebarTree();
    toast(title ? `已重命名为「${title}」` : "已恢复默认会话名", "success");
  } catch (err) {
    toast(err.message || String(err), "error");
  }
}

async function deleteAgentSession(session) {
  if (!session?.id) return;
  const label = session.title || session.id;
  if (!confirm(`删除会话「${label}」？\n本地历史将被移除，且不可恢复。`)) return;
  await api("/api/agent/session/delete", {
    method: "POST",
    body: JSON.stringify({ session_id: session.id }),
  });
  state.agentSessions = state.agentSessions.filter((item) => item.id !== session.id);
  if (state.activeAgentSession?.id === session.id) {
    state.sessionSwitchToken += 1;
    if (state.sessionSwitchAbort) {
      try { state.sessionSwitchAbort.abort(); } catch { /* ignore */ }
      state.sessionSwitchAbort = null;
    }
    state.activeAgentSession = null;
    clearAgentTranscript(true);
    setAgentEngineState("none");
    updateConversationIdentity();
    persistLastChatContext({ sessionId: "", title: "" });
    renderChatEmptyState();
    if (agentIsRunning()) {
      toast("本地记录已删除。Agent 仍在运行，建议点「新对话」避免继续旧上下文", "info");
    } else {
      toast("会话已删除", "success");
    }
  } else {
    toast("会话已删除", "success");
  }
  renderSidebarTree();
}

async function resumeAgentSession(session) {
  if (!session?.id) return false;
  const token = ++state.sessionSwitchToken;
  if (state.sessionSwitchAbort) {
    try { state.sessionSwitchAbort.abort(); } catch { /* ignore */ }
  }
  const controller = new AbortController();
  state.sessionSwitchAbort = controller;
  const stillCurrent = () => token === state.sessionSwitchToken;

  // Prefer a user-corrected path from the context rail when the stored cwd is gone.
  const cwdInput = ($("agentCwd")?.value || "").trim();
  let cwd = cwdInput || session.cwd || "";
  if (session.cwd_missing) {
    if (!cwd || cwd === session.cwd) {
      if ($("agentCwd")) $("agentCwd").value = session.cwd || "";
      toggleContextRail(true);
      toast("该会话的工作目录已失效，请在右侧修正路径后再打开", "error");
      return false;
    }
  }

  let history;
  try {
    history = await api(`/api/agent/sessions/${encodeURIComponent(session.id)}`, { signal: controller.signal });
  } catch (err) {
    if (isAbortError(err) || !stillCurrent()) return false;
    throw err;
  }
  if (!stillCurrent()) return false;

  state.activeAgentSession = {
    ...(history.session || session),
    cwd: cwd || history.session?.cwd || session.cwd || "",
    cwd_missing: !!session.cwd_missing,
  };
  if ($("agentCwd") && state.activeAgentSession.cwd) {
    $("agentCwd").value = state.activeAgentSession.cwd;
  }
  // Align active project with this session's workspace when possible.
  const matchedProject = projectPathKeys().get(normalizePathKey(state.activeAgentSession.cwd));
  if (matchedProject) {
    state.activeProjectId = matchedProject.id;
    setProjectExpanded(matchedProject.id, true);
  }
  clearAgentTranscript(false);
  renderStoredHistory(history.messages || []);
  setAgentEngineState("loading", "正在恢复引擎上下文…");
  updateConversationIdentity();
  connectAgentSocket();

  let status;
  try {
    status = await api("/api/agent/session/load", {
      method: "POST",
      signal: controller.signal,
      body: JSON.stringify({
        cwd: state.activeAgentSession.cwd,
        session_id: state.activeAgentSession.id,
        always_approve: $("agentAlwaysApprove").checked,
      }),
    });
  } catch (err) {
    if (isAbortError(err) || !stillCurrent()) return false;
    if (handleSessionLoadFallback(err)) {
      closeNativeChatPanels();
      scrollChatToBottom();
      return false;
    }
    setAgentEngineState("readonly", "仅显示本地历史，引擎上下文恢复失败。请开启新对话后再发送消息。");
    const latestStatus = await api("/api/agent/status").catch(() => state.agentStatus);
    if (stillCurrent() && latestStatus) renderAgentStatus(latestStatus);
    throw err;
  }
  if (!stillCurrent()) return false;

  setAgentEngineState("attached");
  state.agentStatus = status;
  if (state.activeAgentSession) state.activeAgentSession.cwd_missing = false;
  renderAgentStatus({ ...status, model: status.model || state.activeAgentSession.model });
  persistLastChatContext({
    sessionId: state.activeAgentSession.id,
    cwd: state.activeAgentSession.cwd,
    title: state.activeAgentSession.title,
    model: status.model || state.activeAgentSession.model,
    alwaysApprove: $("agentAlwaysApprove")?.checked,
  });
  closeNativeChatPanels();
  forceScrollChatToBottom();
  return true;
}

function setAgentEngineState(mode, message = "") {
  state.agentEngineState = mode;
  if (mode !== "readonly" && mode !== "bootstrap") state.agentFallbackSessionReady = false;
  if (mode !== "bootstrap") state.agentNeedsBootstrap = false;
  const banner = $("agentEngineBanner");
  if (!banner) return;
  const visible = mode === "loading" || mode === "readonly" || mode === "bootstrap";
  banner.hidden = !visible;
  banner.dataset.state = mode;
  if ($("agentEngineBannerText")) $("agentEngineBannerText").textContent = message;
  if ($("agentReadonlyNewBtn")) $("agentReadonlyNewBtn").hidden = mode !== "readonly" && mode !== "bootstrap";
}

function handleSessionLoadFallback(err) {
  if (err?.code !== "session_load_overflow") return false;
  const status = err.data?.status;
  if (status) {
    state.agentStatus = status;
    renderAgentStatus(status);
  }
  const bootstrapReady = !!(err.data?.needs_bootstrap || status?.needs_bootstrap) &&
    !!err.data?.agent_restarted && status?.state === "ready" && !!status?.session_id;
  if (bootstrapReady) {
    state.agentNeedsBootstrap = true;
    state.agentFallbackSessionReady = true;
    if (state.activeAgentSession && status.session_id) {
      // Engine is a fresh session; keep UI history but track new engine id for sends.
      state.activeAgentSession.engine_session_id = status.session_id;
    }
    setAgentEngineState("bootstrap",
      "引擎上下文未能完整加载。已展示本地历史；下一条消息将自动注入历史摘要以续聊。也可开启全新对话。");
    renderAgentStatus(status || state.agentStatus);
    toast("会话过大，已启用历史摘要续聊", "info");
    return true;
  }
  const message = err.data?.agent_restarted
    ? "仅显示本地历史：会话过大或恢复通知过多，引擎上下文未挂载。Agent 已恢复，可开启新对话。"
    : "仅显示本地历史：引擎上下文未挂载，Agent 自动重启也未成功。请手动启动新对话。";
  setAgentEngineState("readonly", message);
  state.agentFallbackSessionReady = !!err.data?.agent_restarted && status?.state === "ready" && !!status?.session_id;
  renderAgentStatus(status || state.agentStatus);
  toast(err.message, "error");
  return true;
}

function updateConversationIdentity() {
  const session = state.activeAgentSession;
  if ($("activeChatTitle")) $("activeChatTitle").textContent = session?.title || "新对话";
  const cwd = session?.cwd || state.agentStatus?.cwd || $("agentCwd")?.value || "";
  if ($("activeChatPath")) $("activeChatPath").textContent = cwd || "尚未选择工作目录";
  if ($("contextSessionId")) $("contextSessionId").textContent = session?.id || state.agentStatus?.session_id || "—";
  // Keep active project in sync with current workspace path.
  const matched = projectPathKeys().get(normalizePathKey(cwd));
  state.activeProjectId = matched?.id || "";
  updateComposerProjectLabel();
  renderSidebarTree();
}

function currentWorkingDirectory() {
  return ($("agentCwd")?.value || state.activeAgentSession?.cwd || state.agentStatus?.cwd || "").trim();
}

function projectNameForPath(path) {
  const key = String(path || "").replace(/\\/g, "/").toLowerCase();
  if (!key) return "";
  const match = (state.projects || []).find((p) => String(p.path || "").replace(/\\/g, "/").toLowerCase() === key);
  if (match?.name) return match.name;
  const parts = key.split("/").filter(Boolean);
  return parts[parts.length - 1] || "";
}

function updateComposerProjectLabel() {
  const cwd = currentWorkingDirectory();
  const name = projectNameForPath(cwd) || (cwd ? cwd.split(/[/\\]/).filter(Boolean).pop() : "");
  if ($("composerProjectLabel")) $("composerProjectLabel").textContent = name || "选择项目";
  if ($("openLocationLabel")) {
    $("openLocationLabel").textContent = "打开位置";
    $("openLocationBtn")?.setAttribute("title", cwd ? `打开：${cwd}` : "请先选择工作目录");
  }
}

function syncComposerAccessSelect() {
  const sel = $("composerAccessSelect");
  if (!sel) return;
  const yolo = !!$("agentAlwaysApprove")?.checked;
  const session = !!$("agentSessionAutoApprove")?.checked;
  if (yolo) sel.value = "yolo";
  else if (session) sel.value = "session";
  else sel.value = "ask";
  sel.classList.toggle("is-elevated", sel.value !== "ask");
}

function applyComposerAccessSelect() {
  const mode = $("composerAccessSelect")?.value || "ask";
  const yoloBox = $("agentAlwaysApprove");
  const sessionBox = $("agentSessionAutoApprove");
  if (yoloBox) yoloBox.checked = mode === "yolo";
  if (sessionBox) {
    const enabled = mode === "session" || mode === "yolo";
    sessionBox.checked = enabled;
    if (agentSocket && agentSocket.readyState === WebSocket.OPEN) {
      agentSocket.send(JSON.stringify({ type: "set_session_auto_approve", allow: enabled, remember: enabled }));
    }
  }
  // No bottom toast here — it used to cover the 访问方式 chip. Mode is already
  // visible on the access select; YOLO still needs a restart (shown via title).
  const accessChip = $("composerAccessSelect")?.closest(".composerAccessChip");
  if (accessChip) {
    accessChip.title = mode === "yolo"
      ? "完全访问（YOLO）：下次启动 Agent 后生效"
      : mode === "session"
        ? "本会话自动批准工具调用"
        : "每次工具调用需确认";
  }
  syncComposerAccessSelect();
}

async function openWorkingDirectoryInExplorer() {
  const path = currentWorkingDirectory();
  if (!path) {
    await pickWorkingDirectory();
    return;
  }
  await api("/api/agent/open-path", {
    method: "POST",
    body: JSON.stringify({ path }),
  });
  toast(`已打开：${path}`, "info");
}

function bindComposerFloatPad() {
  const composer = $("chatComposer");
  const pane = $("viewChat")?.querySelector(".conversationPane");
  if (!composer || !pane || composer.dataset.floatPadBound === "1") return;
  composer.dataset.floatPadBound = "1";
  const apply = () => {
    const h = Math.ceil(composer.getBoundingClientRect().height || 140);
    pane.style.setProperty("--composer-float-pad", `${Math.max(120, h + 8)}px`);
  };
  apply();
  if (typeof ResizeObserver !== "undefined") {
    new ResizeObserver(apply).observe(composer);
  }
  window.addEventListener("resize", apply);
}

function setNativePanel(name, open) {
  const panel = name === "sessions" ? $("sessionSidebar") : $("contextRail");
  panel?.classList.toggle("open", open);
  const anyOpen = !!$("sessionSidebar")?.classList.contains("open") || !!$("contextRail")?.classList.contains("open");
  if ($("nativeChatScrim")) $("nativeChatScrim").hidden = !anyOpen;
}

function toggleSessionSidebar(forceOpen) {
  if (window.matchMedia("(max-width: 820px)").matches) {
    setNativePanel("sessions", forceOpen ?? !$("sessionSidebar")?.classList.contains("open"));
    return;
  }
  const shell = $("viewChat")?.querySelector(".nativeChatShell");
  if (!shell) return;
  const collapsed = forceOpen === true ? false : forceOpen === false ? true : !shell.classList.contains("sidebarCollapsed");
  shell.classList.toggle("sidebarCollapsed", collapsed);
  localStorage.setItem("gs_chat_sidebar_hidden", collapsed ? "1" : "0");
  requestAnimationFrame(applyStoredChatPanelWidths);
}

function toggleContextRail(forceOpen) {
  if (window.matchMedia("(max-width: 1180px)").matches) {
    setNativePanel("context", forceOpen ?? !$("contextRail")?.classList.contains("open"));
    return;
  }
  const shell = $("viewChat")?.querySelector(".nativeChatShell");
  if (!shell) return;
  const collapsed = forceOpen === true ? false : forceOpen === false ? true : !shell.classList.contains("contextCollapsed");
  shell.classList.toggle("contextCollapsed", collapsed);
  localStorage.setItem("gs_chat_context_hidden", collapsed ? "1" : "0");
  requestAnimationFrame(applyStoredChatPanelWidths);
}

function closeNativeChatPanels() {
  $("sessionSidebar")?.classList.remove("open");
  $("contextRail")?.classList.remove("open");
  if ($("nativeChatScrim")) $("nativeChatScrim").hidden = true;
}

function chatPanelWidthLimit(side, shell) {
  const config = CHAT_PANEL_LAYOUT[side];
  if (!config || !shell) return config?.maxWidth || 0;
  const desktop = window.matchMedia("(min-width: 1181px)").matches;
  const otherSide = side === "left" ? "right" : "left";
  const otherConfig = CHAT_PANEL_LAYOUT[otherSide];
  const otherDocked = otherSide === "left"
    ? window.matchMedia("(min-width: 821px)").matches && !shell.classList.contains("sidebarCollapsed")
    : desktop && !shell.classList.contains("contextCollapsed");
  const otherWidth = otherDocked ? $(otherConfig.panel)?.getBoundingClientRect().width || otherConfig.defaultWidth : 0;
  const dividerWidth = otherDocked ? 10 : 5;
  const roomForConversation = 400;
  return Math.max(config.minWidth, Math.min(config.maxWidth, shell.clientWidth - otherWidth - dividerWidth - roomForConversation));
}

function setChatPanelWidth(side, width, persist = false) {
  const config = CHAT_PANEL_LAYOUT[side];
  const shell = $("viewChat")?.querySelector(".nativeChatShell");
  if (!config || !shell || !Number.isFinite(width)) return config?.defaultWidth || 0;
  const maximum = chatPanelWidthLimit(side, shell);
  const nextWidth = Math.round(Math.min(maximum, Math.max(config.minWidth, width)));
  shell.style.setProperty(config.property, `${nextWidth}px`);
  const resizer = $(config.resizer);
  resizer?.setAttribute("aria-valuenow", String(nextWidth));
  resizer?.setAttribute("aria-valuemax", String(Math.round(maximum)));
  if (persist) localStorage.setItem(config.storage, String(nextWidth));
  return nextWidth;
}

function applyStoredChatPanelWidths() {
  for (const [side, config] of Object.entries(CHAT_PANEL_LAYOUT)) {
    const stored = Number.parseFloat(localStorage.getItem(config.storage));
    setChatPanelWidth(side, Number.isFinite(stored) ? stored : config.defaultWidth);
  }
}

function resetChatPanelWidth(side) {
  const config = CHAT_PANEL_LAYOUT[side];
  if (!config) return;
  localStorage.removeItem(config.storage);
  setChatPanelWidth(side, config.defaultWidth);
}

function bindChatPanelResizer(side) {
  const config = CHAT_PANEL_LAYOUT[side];
  const resizer = $(config?.resizer);
  if (!config || !resizer) return;

  resizer.addEventListener("pointerdown", (event) => {
    if (event.button !== 0) return;
    const shell = $("viewChat")?.querySelector(".nativeChatShell");
    const panel = $(config.panel);
    if (!shell || !panel) return;
    event.preventDefault();
    const startX = event.clientX;
    const startWidth = panel.getBoundingClientRect().width;
    let latestWidth = startWidth;
    resizer.classList.add("active");
    document.body.classList.add("resizingChatPanels");

    const handleMove = (moveEvent) => {
      const delta = moveEvent.clientX - startX;
      latestWidth = setChatPanelWidth(side, startWidth + (side === "left" ? delta : -delta));
    };
    const finish = () => {
      document.removeEventListener("pointermove", handleMove);
      document.removeEventListener("pointerup", finish);
      document.removeEventListener("pointercancel", finish);
      resizer.classList.remove("active");
      document.body.classList.remove("resizingChatPanels");
      setChatPanelWidth(side, latestWidth, true);
    };
    document.addEventListener("pointermove", handleMove);
    document.addEventListener("pointerup", finish);
    document.addEventListener("pointercancel", finish);
  });

  resizer.addEventListener("dblclick", () => resetChatPanelWidth(side));
  resizer.addEventListener("keydown", (event) => {
    if (!["ArrowLeft", "ArrowRight", "Home"].includes(event.key)) return;
    event.preventDefault();
    if (event.key === "Home") {
      resetChatPanelWidth(side);
      return;
    }
    const panelWidth = $(config.panel)?.getBoundingClientRect().width || config.defaultWidth;
    const direction = event.key === "ArrowRight" ? 1 : -1;
    const spatialDelta = (event.shiftKey ? 40 : 12) * direction;
    setChatPanelWidth(side, panelWidth + (side === "left" ? spatialDelta : -spatialDelta), true);
  });
}

function agentIsRunning(status = state.agentStatus) {
  return !!status?.running || ["starting", "ready", "busy", "stopping"].includes(status?.state);
}

function renderAgentStatus(status) {
  if (!status) return;
  state.agentStatus = status;
  const badge = $("agentStatusBadge");
  const stateName = status.state || "idle";
  const labels = {
    idle: "未启动",
    starting: "启动中",
    ready: "已连接",
    busy: "处理中",
    stopping: "停止中",
    dead: "连接异常",
  };
  if (badge) badge.dataset.state = stateName;
  if ($("agentStatusText")) $("agentStatusText").textContent = labels[stateName] || stateName;
  const model = status.model || state.activeAgentSession?.model || "";
  if (model && state.activeAgentSession && (!status.session_id || status.session_id === state.activeAgentSession.id)) {
    state.activeAgentSession.model = model;
  }
  if ($("agentModelBadge")) $("agentModelBadge").textContent = model ? `MODEL ${model}` : "MODEL —";
  if ($("contextModel")) $("contextModel").textContent = model || "—";
  populateComposerModelSelect();
  populateComposerStrengthSelect();
  if ($("contextSessionId")) $("contextSessionId").textContent = status.session_id || state.activeAgentSession?.id || "—";
  if ($("activeChatPath")) $("activeChatPath").textContent = status.cwd || state.activeAgentSession?.cwd || $("agentCwd")?.value || "尚未选择工作目录";
  const running = agentIsRunning(status);
  const busy = stateName === "busy" || !!status.busy;
  if ($("agentStartBtn")) {
    $("agentStartBtn").disabled = stateName === "starting" || stateName === "stopping" || busy;
    $("agentStartBtn").textContent = running ? "重启 Agent" : "启动 Agent";
  }
  if ($("agentNewSessionBtn")) $("agentNewSessionBtn").disabled = !running || busy || stateName === "stopping";
  if ($("agentStopBtn")) $("agentStopBtn").disabled = !running || stateName === "stopping";
  if ($("agentAlwaysApprove")) {
    $("agentAlwaysApprove").disabled = running;
    if (typeof status.always_approve === "boolean") $("agentAlwaysApprove").checked = status.always_approve;
  }
  if ($("agentSessionAutoApprove")) {
    $("agentSessionAutoApprove").disabled = !running || stateName === "stopping";
    if (typeof status.session_auto_approve === "boolean") {
      $("agentSessionAutoApprove").checked = status.session_auto_approve;
    }
  }
  syncComposerAccessSelect();
  updateComposerProjectLabel();
  if (status.needs_bootstrap) state.agentNeedsBootstrap = true;
  const loading = state.agentEngineState === "loading";
  const composerReady = (stateName === "ready" && !loading) || state.agentEngineState === "bootstrap";
  if ($("chatInput")) {
    $("chatInput").disabled = !composerReady || busy;
    $("chatInput").placeholder = busy
      ? "正在生成…可点击停止"
      : composerReady
        ? "随心输入…  输入 / 打开命令与 Skills"
        : "启动 Agent 后即可发送消息";
  }
  if ($("composerAccessSelect")) $("composerAccessSelect").disabled = !composerReady && !running;
  if ($("chatSendBtn")) {
    $("chatSendBtn").hidden = busy;
    const hasContent = !!$("chatInput")?.value.trim() || state.pendingAttachments.length > 0;
    $("chatSendBtn").disabled = !composerReady || busy || !hasContent;
  }
  if ($("chatAttachBtn")) $("chatAttachBtn").disabled = !composerReady || busy;
  if ($("composerModelSelect")) $("composerModelSelect").disabled = !composerReady || busy;
  if ($("composerStrengthSelect")) $("composerStrengthSelect").disabled = !composerReady || busy;
  if ($("chatStopBtn")) {
    $("chatStopBtn").hidden = !busy;
    $("chatStopBtn").disabled = !busy;
  }
  // Status line is ABOVE the chip row (never beside 访问方式 / send).
  // Idle keyboard tips stay on the textarea title so chips are never covered.
  const hint = $("composerHint");
  const statusRow = $("composerStatusRow");
  const input = $("chatInput");
  if (input) {
    const accessTip = status.session_auto_approve || $("agentAlwaysApprove")?.checked
      ? "工具：本会话自动允许 · "
      : "";
    input.title = busy
      ? "正在生成… Esc 或点击停止"
      : `${accessTip}Enter 发送 · Shift+Enter 换行`;
  }
  if (hint && statusRow) {
    hint.classList.toggle("composerBusyHint", busy);
    if (busy) {
      hint.textContent = "正在生成…点击停止 · Esc 也可停止";
      statusRow.hidden = false;
    } else {
      hint.textContent = "";
      statusRow.hidden = true;
    }
  }
  refreshMessageActionButtons();
  renderChatEmptyState(status);
}

function connectAgentSocket() {
  if (agentSocket && (agentSocket.readyState === WebSocket.OPEN || agentSocket.readyState === WebSocket.CONNECTING)) return;
  clearTimeout(agentReconnectTimer);
  const protocol = location.protocol === "https:" ? "wss:" : "ws:";
  const socket = new WebSocket(`${protocol}//${location.host}/api/agent/ws`);
  agentSocket = socket;
  socket.onopen = () => clearTimeout(agentReconnectTimer);
  socket.onmessage = (message) => {
    try {
      handleAgentEvent(JSON.parse(message.data));
    } catch (err) {
      toast(`对话事件无效：${err.message}`, "error");
    }
  };
  socket.onerror = () => socket.close();
  socket.onclose = () => {
    if (agentSocket === socket) agentSocket = null;
    if (state.view === "chat") {
      agentReconnectTimer = setTimeout(connectAgentSocket, 1500);
    }
  };
}

function handleAgentEvent(event) {
  switch (event.type) {
    case "agent_status": {
      const current = state.agentStatus || {};
      const stateName = event.status || current.state || "idle";
      renderAgentStatus({
        ...current,
        state: stateName,
        session_id: event.session_id || current.session_id,
        running: ["starting", "ready", "busy", "stopping"].includes(stateName),
        busy: stateName === "busy",
        model: event.model || current.model,
        error: event.error || (stateName === "dead" ? current.error : ""),
        session_auto_approve: typeof event.session_auto_approve === "boolean"
          ? event.session_auto_approve
          : current.session_auto_approve,
      });
      break;
    }
    case "assistant_chunk":
      appendAssistantChunk(event.text || "", event.session_id || "");
      break;
    case "assistant_media":
      appendAssistantMedia(event.media || [], event.session_id || "");
      break;
    case "thought_chunk":
      appendThoughtChunk(event.text || "");
      break;
    case "tool_call":
    case "tool_update":
      renderAgentTool(event.tool || {}, event.type === "tool_update", event.session_id || "");
      break;
    case "permission_request":
      showAgentPermission(event.permission);
      break;
    case "plan":
    case "plan_request":
      showAgentPlan(event.plan, event.type === "plan_request");
      break;
    case "retry_state":
      renderAgentRetry(event.retry);
      break;
    case "turn_done":
      finalizeAssistantMessage();
      agentActiveAssistant = null;
      agentActiveThought = null;
      agentRetryNotice = null;
      if (state.agentNeedsBootstrap) {
        state.agentNeedsBootstrap = false;
        if (state.agentEngineState === "bootstrap") setAgentEngineState("attached");
      }
      if (event.stop_reason === "cancelled") {
        appendAgentNotice("已停止生成");
      }
      renderAgentStatus({ ...state.agentStatus, state: "ready", running: true, busy: false, error: "", needs_bootstrap: false });
      rebuildChatNodesFromDom();
      loadAgentSessions().catch(() => {});
      break;
    case "error":
      finalizeAssistantMessage();
      appendAgentNotice(event.error || "Grok Agent 出错", true);
      renderAgentStatus({ ...state.agentStatus, state: agentIsRunning() ? "ready" : "dead", busy: false, error: event.error || "" });
      break;
  }
}

function clearAgentTranscript(showEmpty = true) {
  const messages = $("chatMessages");
  if (!messages) return;
  messages.innerHTML = showEmpty ? chatEmptyMarkup() : "";
  agentTools.clear();
  if ($("toolActivityCount")) $("toolActivityCount").textContent = "0";
  if ($("toolActivityList")) $("toolActivityList").innerHTML = "<span>暂无工具活动</span>";
  agentActiveAssistant = null;
  agentActiveThought = null;
  agentRetryNotice = null;
  lastAssistantMessageEl = null;
  state.agentPermission = null;
  state.agentPlan = null;
  state.lastUserMessage = "";
  state.chatStickToBottom = true;
  resetChatNodes();
  clearChatAttachments();
  if ($("permissionBar")) $("permissionBar").hidden = true;
  if ($("planBar")) $("planBar").hidden = true;
  historyRenderedStart = 0;
  historyRenderedEnd = 0;
  pendingHistory = [];
  updateChatJumpBottomBtn();
  if (showEmpty) {
    bindChatEmptyActions();
    renderChatEmptyState();
  }
}

function chatEmptyMarkup() {
  return `<div id="chatEmpty" class="chatEmpty" data-state="welcome">
    <img src="/icon.svg" alt="" class="chatEmptyMark chatEmptyIcon">
    <strong id="chatEmptyTitle">开始对话</strong>
    <span id="chatEmptyDesc">从左侧添加项目（工作空间），在项目下新建或打开会话。</span>
    <div id="chatEmptyActions" class="chatEmptyActions">
      <button type="button" id="chatEmptyStartBtn" class="btn sm primary">启动 Agent</button>
      <button type="button" id="chatEmptyOpenContextBtn" class="btn sm">会话信息</button>
    </div>
  </div>`;
}

function bindChatEmptyActions() {
  $("chatEmptyStartBtn")?.addEventListener("click", () => {
    run(startAgent, { button: $("chatEmptyStartBtn"), busyLabel: "连接中…" }).catch(() => {});
  });
  $("chatEmptyOpenContextBtn")?.addEventListener("click", () => toggleContextRail(true));
  $("chatEmptyRetryBtn")?.addEventListener("click", () => {
    run(async () => {
      const status = await api("/api/agent/status");
      renderAgentStatus(status);
      if (!status.available) throw new Error(status.error || "仍未检测到 Grok Build");
      return status;
    }, { button: $("chatEmptyRetryBtn"), busyLabel: "检测中…", success: "已检测到 Grok Build" }).catch(() => {});
  });
}

function renderChatEmptyState(status = state.agentStatus) {
  const empty = $("chatEmpty");
  if (!empty || !empty.isConnected) return;
  const title = empty.querySelector("#chatEmptyTitle") || empty.querySelector("strong");
  const desc = empty.querySelector("#chatEmptyDesc") || empty.querySelector("span");
  const actions = empty.querySelector("#chatEmptyActions");
  if (!title || !desc) return;

  if (status && status.available === false) {
    empty.dataset.state = "missing";
    title.textContent = "未找到 Grok Build";
    desc.textContent = status.error || "本机未检测到 grok 可执行文件。请先安装并登录 Grok CLI，然后重试。";
    if (actions) {
      actions.innerHTML = `
        <button type="button" id="chatEmptyRetryBtn" class="btn sm primary">重新检测</button>
        <button type="button" id="chatEmptyOpenContextBtn" class="btn sm">会话信息</button>`;
      bindChatEmptyActions();
    }
    return;
  }

  if (status && (status.state === "dead" || status.error) && !agentIsRunning(status)) {
    empty.dataset.state = "error";
    title.textContent = "Agent 未就绪";
    desc.textContent = status.error || "连接异常，请检查工作目录后重新启动 Agent。";
    if (actions) {
      actions.innerHTML = `
        <button type="button" id="chatEmptyStartBtn" class="btn sm primary">启动 Agent</button>
        <button type="button" id="chatEmptyOpenContextBtn" class="btn sm">会话信息</button>`;
      bindChatEmptyActions();
    }
    return;
  }

  empty.dataset.state = "welcome";
  title.textContent = "开始对话";
  const cwd = ($("agentCwd")?.value || status?.cwd || "").trim();
  desc.textContent = cwd
    ? `工作目录：${cwd}。启动 Agent 后即可发送消息，或从左侧打开历史会话。`
    : "点击顶部「打开位置」旁的 ▾ 选择工作目录，或点输入框旁项目 chip · 再启动 Agent。";
  if (actions && !actions.querySelector("#chatEmptyStartBtn")) {
    actions.innerHTML = `
      <button type="button" id="chatEmptyStartBtn" class="btn sm primary">启动 Agent</button>
      <button type="button" id="chatEmptyOpenContextBtn" class="btn sm">会话信息</button>`;
    bindChatEmptyActions();
  }
}

function removeChatEmpty() {
  $("chatEmpty")?.remove();
}

function bindChatScrollTracking() {
  const messages = $("chatMessages");
  if (!messages || messages.dataset.scrollBound === "1") return;
  messages.dataset.scrollBound = "1";
  messages.addEventListener("scroll", () => {
    state.chatStickToBottom = isChatNearBottom(messages);
    updateActiveChatNode();
    updateChatJumpBottomBtn();
  }, { passive: true });
}

function isChatNearBottom(container = $("chatMessages"), threshold = 80) {
  if (!container) return true;
  return container.scrollHeight - container.scrollTop - container.clientHeight <= threshold;
}

function scrollChatToBottom(force = false) {
  const messages = $("chatMessages");
  if (!messages) return;
  if (!force && !state.chatStickToBottom) return;
  requestAnimationFrame(() => {
    messages.scrollTop = messages.scrollHeight;
    state.chatStickToBottom = true;
  });
}

function forceScrollChatToBottom() {
  state.chatStickToBottom = true;
  scrollChatToBottom(true);
}

// --- 对话节点导航：每条用户消息生成一个可点击节点，点击平滑滚动到该消息 ---
function resetChatNodes() {
  chatNodes.length = 0;
  const scroller = $("chatNodeScroller");
  if (scroller) scroller.innerHTML = "";
  const rail = $("chatNodeRail");
  if (rail) rail.hidden = true;
}

function addChatNodeFor(article) {
  if (!article) return;
  const scroller = $("chatNodeScroller");
  const rail = $("chatNodeRail");
  if (!scroller || !rail) return;
  const id = `node-${Date.now()}-${chatNodes.length}`;
  article.dataset.nodeId = id;
  const preview = (article._rawText || "").replace(/\s+/g, " ").trim();
  const node = document.createElement("button");
  node.type = "button";
  node.className = "chatNode";
  node.dataset.nodeId = id;
  node.title = preview.slice(0, 200) || "（空消息）";
  const idx = document.createElement("span");
  idx.className = "chatNodeIdx";
  idx.textContent = String(chatNodes.length + 1);
  const label = document.createElement("span");
  label.className = "chatNodeLabel";
  label.textContent = preview.slice(0, 28) || "（空消息）";
  node.append(idx, label);
  node.onclick = () => jumpToChatNode(id);
  scroller.append(node);
  rail.hidden = false;
  chatNodes.push({ id, article, el: node });
}

function jumpToChatNode(id) {
  const entry = chatNodes.find((n) => n.id === id);
  const messages = $("chatMessages");
  if (!entry?.article?.isConnected || !messages) return;
  state.chatStickToBottom = false;
  const containerTop = messages.getBoundingClientRect().top;
  const delta = entry.article.getBoundingClientRect().top - containerTop;
  messages.scrollTo({ top: messages.scrollTop + delta - 8, behavior: "smooth" });
  entry.article.classList.remove("nodeFlash");
  void entry.article.offsetWidth; // reflow to restart animation
  entry.article.classList.add("nodeFlash");
  setTimeout(() => entry.article.classList.remove("nodeFlash"), 1700);
  setActiveChatNode(id);
}

function setActiveChatNode(id) {
  for (const entry of chatNodes) {
    entry.el.classList.toggle("active", entry.id === id);
  }
}

function updateActiveChatNode() {
  pruneChatNodes();
  const messages = $("chatMessages");
  if (!messages || chatNodes.length === 0) return;
  const containerTop = messages.getBoundingClientRect().top;
  let activeId = null;
  for (const entry of chatNodes) {
    if (!entry.article?.isConnected) continue;
    if (entry.article.getBoundingClientRect().top - containerTop <= 60) {
      activeId = entry.id;
    } else {
      break;
    }
  }
  setActiveChatNode(activeId);
}

function pruneChatNodes() {
  for (let i = chatNodes.length - 1; i >= 0; i--) {
    if (!chatNodes[i].article?.isConnected) {
      chatNodes[i].el?.remove();
      chatNodes.splice(i, 1);
    }
  }
  const rail = $("chatNodeRail");
  if (rail) rail.hidden = chatNodes.length === 0;
}

// Rebuild the whole node rail from the current DOM order. Used after
// truncation / incremental history prepend so indices and previews stay correct.
function rebuildChatNodesFromDom() {
  chatNodes.length = 0;
  const scroller = $("chatNodeScroller");
  const rail = $("chatNodeRail");
  if (scroller) scroller.innerHTML = "";
  const userArticles = [...$("chatMessages")?.querySelectorAll(".chatMessage[data-role='user']") || []];
  userArticles.forEach((article, index) => {
    const id = `node-${Date.now()}-${index}`;
    article.dataset.nodeId = id;
    const preview = (article._rawText || "").replace(/\s+/g, " ").trim();
    const node = document.createElement("button");
    node.type = "button";
    node.className = "chatNode";
    node.dataset.nodeId = id;
    node.title = preview.slice(0, 200) || "（空消息）";
    const idx = document.createElement("span");
    idx.className = "chatNodeIdx";
    idx.textContent = String(index + 1);
    const label = document.createElement("span");
    label.className = "chatNodeLabel";
    label.textContent = preview.slice(0, 28) || "（空消息）";
    node.append(idx, label);
    if (hasToolInTurn(article)) {
      node.classList.add("hasTool");
      const mark = document.createElement("span");
      mark.className = "chatNodeToolMark";
      mark.textContent = "⚙";
      mark.title = "本轮包含工具调用";
      node.append(mark);
    }
    node.onclick = () => jumpToChatNode(id);
    scroller?.append(node);
    chatNodes.push({ id, article, el: node });
  });
  if (rail) rail.hidden = chatNodes.length === 0;
}

// A "turn" is this user message plus everything until the next user message.
// Mark it as tool-heavy if any tool element appears in that range.
function hasToolInTurn(userArticle) {
  let el = userArticle.nextElementSibling;
  while (el) {
    if (el.classList?.contains("chatMessage") && el.dataset.role === "user") break;
    if (el.classList?.contains("agentTool")) return true;
    if (el.querySelector?.(".agentTool")) return true;
    el = el.nextElementSibling;
  }
  return false;
}

function setActiveChatNode(id) {
  for (const entry of chatNodes) {
    const active = entry.id === id;
    entry.el.classList.toggle("active", active);
    if (active) {
      const scroller = $("chatNodeScroller");
      if (scroller) {
        const nodeRect = entry.el.getBoundingClientRect();
        const scrollerRect = scroller.getBoundingClientRect();
        if (nodeRect.left < scrollerRect.left || nodeRect.right > scrollerRect.right) {
          entry.el.scrollIntoView({ behavior: "smooth", block: "nearest", inline: "center" });
        }
      }
    }
  }
}

// --- 长历史增量加载（初始渲染尾窗；加载更早时真正 prepend，保持滚动锚点） ---
function chatMessagesRoot() {
  return historyMountTarget || $("chatMessages");
}

function renderStoredHistory(messages) {
  pendingHistory = Array.isArray(messages) ? messages.slice() : [];
  const total = pendingHistory.length;
  historyRenderedStart = Math.max(0, total - HISTORY_PAGE_SIZE);
  historyRenderedEnd = total;
  mountHistoryRange(historyRenderedStart, historyRenderedEnd, { mode: "replace" });
}

/**
 * Mount [start, end) of pendingHistory.
 * mode "replace": clear transcript and append the range.
 * mode "prepend": insert the range after the load-more sentinel without rebuilding newer messages.
 */
function mountHistoryRange(start, end, { mode = "replace" } = {}) {
  const messages = $("chatMessages");
  if (!messages) return;
  start = Math.max(0, start);
  end = Math.max(start, Math.min(end, pendingHistory.length));

  if (mode === "replace") {
    state.chatStickToBottom = true;
    messages.innerHTML = "";
    agentTools.clear();
    if ($("toolActivityCount")) $("toolActivityCount").textContent = "0";
    if ($("toolActivityList")) $("toolActivityList").innerHTML = "<span>暂无工具活动</span>";
    agentActiveAssistant = null;
    agentActiveThought = null;
    agentRetryNotice = null;
    lastAssistantMessageEl = null;
    resetChatNodes();

    historyMountSilent = true;
    historyMountTarget = null;
    try {
      for (let i = start; i < end; i++) {
        appendHistoryMessage(pendingHistory[i]);
      }
    } finally {
      historyMountSilent = false;
      historyMountTarget = null;
    }
    agentActiveAssistant = null;
    agentActiveThought = null;
    lastAssistantMessageEl = messages.querySelector(".chatMessage.assistant:last-of-type");
    rebuildChatNodesFromDom();
    ensureHistorySentinel(start, pendingHistory.length);
    syncLastUserMessageFromHistory();
    refreshMessageActionButtons();
    forceScrollChatToBottom();
    return;
  }

  // prepend older window
  if (start >= historyRenderedStart || end !== historyRenderedStart) return;
  const prevHeight = messages.scrollHeight;
  const prevTop = messages.scrollTop;
  state.chatStickToBottom = false;

  const fragment = document.createDocumentFragment();
  historyMountSilent = true;
  historyMountTarget = fragment;
  // History tools/thoughts in older pages must not steal the "active" stream handles.
  const savedThought = agentActiveThought;
  const savedAssistant = agentActiveAssistant;
  agentActiveThought = null;
  agentActiveAssistant = null;
  try {
    for (let i = start; i < end; i++) {
      appendHistoryMessage(pendingHistory[i]);
    }
  } finally {
    historyMountSilent = false;
    historyMountTarget = null;
    agentActiveThought = savedThought;
    agentActiveAssistant = savedAssistant;
  }

  ensureHistorySentinel(start, pendingHistory.length);
  const sentinel = messages.querySelector("#chatHistorySentinel");
  // Capture nodes before insert — DocumentFragment empties when moved into the tree.
  const mountedNodes = [...fragment.childNodes];
  if (sentinel) {
    // Older messages sit right after the sentinel, before already-rendered content.
    sentinel.after(fragment);
  } else {
    messages.prepend(fragment);
  }
  // Mermaid needs a connected tree; re-run on just-prepended markdown bodies.
  for (const node of mountedNodes) {
    const body = node.querySelector?.(".chatMessageText.markdownBody");
    if (body) renderMermaidBlocks(body).catch(() => {});
  }

  historyRenderedStart = start;
  // Keep scroll position glued to the content the user was looking at.
  messages.scrollTop = prevTop + (messages.scrollHeight - prevHeight);
  lastAssistantMessageEl = messages.querySelector(".chatMessage.assistant:last-of-type");
  rebuildChatNodesFromDom();
  ensureHistorySentinel(historyRenderedStart, pendingHistory.length);
  refreshMessageActionButtons();
}

function appendHistoryMessage(message) {
  const sessionID = state.activeAgentSession?.id || "";
  switch (message.role) {
    case "user": {
      const { text, attachments, media } = historyUserPresentation(message);
      appendChatMessage("user", text, "", true, attachments, media, sessionID);
      break;
    }
    case "assistant":
      appendChatMessage("assistant", message.content || "", message.model || state.activeAgentSession?.model || "", true, null, message.media || [], sessionID);
      break;
    case "thought":
      appendThoughtChunk(message.content || "");
      agentActiveThought = null;
      break;
    case "tool":
      renderAgentTool(message.tool || {}, false, sessionID);
      break;
    case "tool_result":
      renderAgentTool({ ...(message.tool || {}), raw_output: message.content || "", media: message.media || [], status: "completed" }, true, sessionID);
      break;
  }
}

function syncLastUserMessageFromHistory() {
  let lastUserText = "";
  for (let i = pendingHistory.length - 1; i >= 0; i--) {
    if (pendingHistory[i].role === "user") {
      lastUserText = pendingHistory[i].content || "";
      break;
    }
  }
  state.lastUserMessage = lastUserText;
}

/** Rebuild user bubble attachments/media from stored history payloads. */
function historyUserPresentation(message) {
  const mediaIn = Array.isArray(message.media) ? message.media : [];
  const attachments = [];
  const media = [];
  for (const item of mediaIn) {
    if (!item || typeof item !== "object") continue;
    const mime = String(item.mime_type || item.mimeType || "").trim();
    const uri = String(item.uri || item.url || "").trim();
    const name = String(item.name || item.title || "").trim();
    const kind = String(item.kind || item.type || "").toLowerCase() || inferMediaKind("", mime, uri);
    const rawData = typeof item.data === "string" ? item.data.replace(/\s+/g, "") : "";
    if ((kind === "image" || mime.startsWith("image/")) && rawData) {
      const mimeType = mime || "image/png";
      attachments.push({
        kind: "image",
        data: rawData,
        dataUrl: `data:${mimeType};base64,${rawData}`,
        mimeType,
        name: name || "image",
      });
      continue;
    }
    if (kind === "resource" && name && !uri && !rawData) {
      attachments.push({ kind: "text_file", name, text: "", truncated: false });
      continue;
    }
    media.push(item);
  }

  let text = message.content || "";
  // Text-file attachments are folded into the prompt as 【附件：name】snippets.
  // Recover chips for display without double-showing the full body when empty name-only.
  const fileMarker = /【附件：([^\n】]+)】/g;
  let match;
  const seen = new Set(attachments.map((item) => item.name));
  while ((match = fileMarker.exec(text)) !== null) {
    const fileName = match[1].trim();
    if (fileName && !seen.has(fileName)) {
      seen.add(fileName);
      attachments.push({ kind: "text_file", name: fileName, text: "", truncated: false });
    }
  }
  return { text, attachments: attachments.length ? attachments : null, media };
}

function ensureHistorySentinel(start, total) {
  const messages = $("chatMessages");
  if (!messages) return;
  const existing = messages.querySelector("#chatHistorySentinel");
  if (start <= 0) {
    existing?.remove();
    return;
  }
  let sentinel = existing;
  if (!sentinel) {
    sentinel = document.createElement("button");
    sentinel.type = "button";
    sentinel.id = "chatHistorySentinel";
    sentinel.className = "chatHistorySentinel";
    sentinel.onclick = () => loadOlderHistory();
    messages.prepend(sentinel);
  } else if (sentinel !== messages.firstChild) {
    messages.prepend(sentinel);
  }
  const remaining = start;
  sentinel.disabled = false;
  sentinel.textContent = remaining > HISTORY_PAGE_SIZE
    ? `↑ 加载更早 ${HISTORY_PAGE_SIZE} 条（还剩 ${remaining}）`
    : `↑ 加载更早 ${remaining} 条会话`;
}

function loadOlderHistory() {
  if (!pendingHistory.length || historyRenderedStart <= 0) return;
  const messages = $("chatMessages");
  const sentinel = messages?.querySelector("#chatHistorySentinel");
  if (sentinel) {
    sentinel.disabled = true;
    sentinel.textContent = "加载中…";
  }
  // Yield so the button can paint "加载中…", then prepend without wiping newer DOM.
  requestAnimationFrame(() => {
    const newStart = Math.max(0, historyRenderedStart - HISTORY_PAGE_SIZE);
    const oldStart = historyRenderedStart;
    try {
      mountHistoryRange(newStart, oldStart, { mode: "prepend" });
    } catch (err) {
      toast(err.message || String(err), "error");
      ensureHistorySentinel(historyRenderedStart, pendingHistory.length);
    }
  });
}

// --- 用户消息编辑 + 重新发送（截断该消息之后的 UI 内容） ---
function enterUserEditMode(article) {
  if (!article?.isConnected || article.dataset.role !== "user") return;
  if (article.querySelector(".chatMessageEditArea")) return;
  if (state.agentStatus?.state === "busy" || state.agentStatus?.busy) {
    toast("正在生成回复，请先停止或等待完成", "error");
    return;
  }
  const body = article.querySelector(".chatMessageText");
  if (!body) return;
  article.dataset.editing = "1";
  const original = article._rawText || "";
  const textarea = document.createElement("textarea");
  textarea.className = "chatMessageEditArea";
  textarea.value = original;
  textarea.rows = Math.min(12, Math.max(3, original.split(/\n/).length));
  const actions = document.createElement("div");
  actions.className = "chatMessageEditActions";
  const cancelBtn = document.createElement("button");
  cancelBtn.type = "button";
  cancelBtn.className = "btn sm";
  cancelBtn.textContent = "取消";
  cancelBtn.onclick = () => exitUserEditMode(article, original);
  const sendBtn = document.createElement("button");
  sendBtn.type = "button";
  sendBtn.className = "btn sm primary";
  sendBtn.textContent = "重新发送";
  sendBtn.title = "仅截断下方界面气泡；引擎仍保留原对话上下文";
  sendBtn.onclick = () => resendEditedUserMessage(article, textarea.value).catch((err) => toast(err.message || String(err), "error"));
  const note = document.createElement("p");
  note.className = "chatEditNote";
  note.textContent = "说明：只会截断下方界面内容并追加新一轮消息，引擎上下文不会真正回退。";
  actions.append(cancelBtn, sendBtn);
  body.replaceChildren(textarea, note, actions);
  textarea.focus();
  textarea.selectionStart = textarea.value.length;
  textarea.addEventListener("keydown", (event) => {
    if (event.key === "Escape") { event.preventDefault(); exitUserEditMode(article, original); }
    else if (event.key === "Enter" && (event.metaKey || event.ctrlKey)) { event.preventDefault(); sendBtn.click(); }
  });
}

function exitUserEditMode(article, originalText) {
  if (!article?.isConnected) return;
  delete article.dataset.editing;
  article._rawText = originalText;
  const body = article.querySelector(".chatMessageText");
  if (body) body.replaceChildren();
  renderMessageMarkdown(article, true);
  rebuildChatNodesFromDom();
}

async function resendEditedUserMessage(article, rawText) {
  const text = (rawText || "").trim();
  if (!text) throw new Error("消息不能为空");
  if (state.agentStatus?.state === "busy" || state.agentStatus?.busy) throw new Error("正在生成中，请先停止或等待完成");
  if (state.agentStatus?.state !== "ready" || state.agentEngineState === "loading" || state.agentEngineState === "readonly") {
    throw new Error("当前会话不可用，请先启动 Agent");
  }
  if (!agentSocket || agentSocket.readyState !== WebSocket.OPEN) {
    toast("对话连接尚未就绪", "error");
    connectAgentSocket();
    return;
  }
  // Truncate everything after the edited user message in the UI.
  let next = article.nextElementSibling;
  while (next) {
    const after = next.nextElementSibling;
    next.remove();
    next = after;
  }
  lastAssistantMessageEl = $("chatMessages").querySelector(".chatMessage.assistant:last-of-type");
  // Update the bubble with the edited text.
  article._rawText = text;
  const body = article.querySelector(".chatMessageText");
  if (body) body.replaceChildren();
  renderMessageMarkdown(article, true);
  delete article.dataset.editing;
  rebuildChatNodesFromDom();
  state.lastUserMessage = text;
  agentActiveAssistant = null;
  agentActiveThought = null;
  agentRetryNotice = null;
  const rewind = await rewindDropLastUser();
  const _mSend = $("composerModelSelect")?.value || "";
  const _rawSSend = $("composerStrengthSelect")?.value || "";
  const _sSend = _rawSSend === "auto" ? "" : _rawSSend;
  agentSocket.send(JSON.stringify({ type: "user_message", text, model: _mSend, strength: _sSend }));
  renderAgentStatus({ ...state.agentStatus, state: "busy", running: true, busy: true });
  if (rewind?.ok) {
    appendAgentNotice("已回退引擎上下文并重新发送");
  } else {
    appendAgentNotice(`已重新发送（引擎 rewind 未成功${rewind?.error ? "：" + rewind.error : ""}，可能仍保留旧上下文）`);
  }
  forceScrollChatToBottom();
}

async function rewindDropLastUser() {
  try {
    return await api("/api/agent/rewind", {
      method: "POST",
      body: JSON.stringify({ drop_last_user: true, restore_files: false }),
    });
  } catch (err) {
    return { ok: false, soft: true, error: err.message || String(err) };
  }
}

// --- 会话内查找条 ---
function openChatFindBar(prefill = "") {
  const bar = $("chatFindBar");
  if (!bar) return;
  bar.hidden = false;
  const input = $("chatFindInput");
  if (input) {
    input.value = prefill !== "" ? prefill : (input.value || getSelection()?.toString() || "");
    input.focus();
    input.select();
  }
  runChatFind();
}

function closeChatFindBar() {
  const bar = $("chatFindBar");
  if (!bar) return;
  bar.hidden = true;
  const input = $("chatFindInput");
  if (input) input.value = "";
  findMatches = [];
  findIndex = -1;
  $("chatFindCount").textContent = "0 / 0";
  $("chatInput")?.focus();
}

function runChatFind() {
  const query = ($("chatFindInput")?.value || "").trim().toLowerCase();
  const countEl = $("chatFindCount");
  if (!query) {
    findMatches = [];
    findIndex = -1;
    if (countEl) countEl.textContent = "0 / 0";
    return;
  }
  const articles = [...$("chatMessages")?.querySelectorAll(".chatMessage[data-role='user'], .chatMessage[data-role='assistant']") || []];
  findMatches = articles.filter((a) => (a._rawText || "").toLowerCase().includes(query));
  findIndex = findMatches.length ? 0 : -1;
  if (countEl) countEl.textContent = findMatches.length ? `${findIndex + 1} / ${findMatches.length}` : "0 / 0";
  $("chatFindPrev").disabled = findMatches.length < 2;
  $("chatFindNext").disabled = findMatches.length < 2;
  if (findMatches.length) jumpToArticle(findMatches[findIndex]);
}

function cycleChatFind(direction) {
  if (!findMatches.length) { runChatFind(); return; }
  findIndex = (findIndex + direction + findMatches.length) % findMatches.length;
  $("chatFindCount").textContent = `${findIndex + 1} / ${findMatches.length}`;
  jumpToArticle(findMatches[findIndex]);
}

function jumpToArticle(article) {
  if (!article?.isConnected) return;
  const messages = $("chatMessages");
  if (!messages) return;
  state.chatStickToBottom = false;
  const delta = article.getBoundingClientRect().top - messages.getBoundingClientRect().top;
  messages.scrollTo({ top: messages.scrollTop + delta - 8, behavior: "smooth" });
  article.classList.remove("nodeFlash");
  void article.offsetWidth;
  article.classList.add("nodeFlash");
  setTimeout(() => article.classList.remove("nodeFlash"), 1700);
}

// --- 回到最新浮动按钮 ---
function updateChatJumpBottomBtn() {
  const btn = $("chatJumpBottomBtn");
  if (!btn) return;
  const messages = $("chatMessages");
  const hasMessages = !!messages?.querySelector(".chatMessage");
  btn.hidden = !hasMessages || state.chatStickToBottom;
}

let chatExtrasBound = false;
function bindChatExtras() {
  if (chatExtrasBound) return;
  chatExtrasBound = true;
  $("chatJumpBottomBtn")?.addEventListener("click", () => forceScrollChatToBottom());
  const composer = $("chatComposer");
  if (composer) {
    composer.addEventListener("dragover", (event) => {
      event.preventDefault();
      composer.classList.add("is-dragover");
    });
    composer.addEventListener("dragleave", () => composer.classList.remove("is-dragover"));
    composer.addEventListener("drop", (event) => {
      event.preventDefault();
      composer.classList.remove("is-dragover");
      const files = event.dataTransfer?.files;
      if (files?.length) handleChatFiles(files);
    });
  }
  $("chatFindClose")?.addEventListener("click", closeChatFindBar);
  $("chatFindNext")?.addEventListener("click", () => cycleChatFind(1));
  $("chatFindPrev")?.addEventListener("click", () => cycleChatFind(-1));
  $("chatFindInput")?.addEventListener("input", () => runChatFind());
  $("chatFindInput")?.addEventListener("keydown", (event) => {
    if (event.key === "Escape") { event.preventDefault(); closeChatFindBar(); }
    else if (event.key === "Enter") {
      event.preventDefault();
      cycleChatFind(event.shiftKey ? -1 : 1);
    }
  });
}

function createChatMessage(role, text, model = "", final = false, attachments = null, media = null, sessionID = "") {
  const article = document.createElement("article");
  article.className = `chatMessage ${role}`;
  article._rawText = text || "";
  article.dataset.role = role;
  if (sessionID) article.dataset.sessionId = sessionID;
  const header = document.createElement("div");
  header.className = "chatMessageHeader";
  const label = document.createElement("span");
  label.className = "chatMessageRole";
  label.textContent = role === "user" ? "你" : role === "assistant" ? "Grok" : "系统";
  header.append(label);
  if (role === "assistant" && model) {
    const modelLabel = document.createElement("span");
    modelLabel.className = "messageModel";
    modelLabel.textContent = model;
    header.append(modelLabel);
  }
  if (role === "user" || role === "assistant") {
    const actions = document.createElement("div");
    actions.className = "chatMessageActions";
    const copyBtn = document.createElement("button");
    copyBtn.type = "button";
    copyBtn.className = "messageActionBtn";
    copyBtn.dataset.action = "copy";
    copyBtn.textContent = "复制";
    copyBtn.onclick = () => copyText(article._rawText || "", "已复制消息");
    actions.append(copyBtn);
    if (role === "assistant") {
      const regenBtn = document.createElement("button");
      regenBtn.type = "button";
      regenBtn.className = "messageActionBtn";
      regenBtn.dataset.action = "regenerate";
      regenBtn.textContent = "重新生成";
      regenBtn.title = "删除本条界面气泡并重发上一轮用户消息；引擎仍保留上一轮回复上下文";
      regenBtn.onclick = () => regenerateLastAssistant(article).catch((err) => toast(err.message || String(err), "error"));
      actions.append(regenBtn);
      if (!historyMountSilent) lastAssistantMessageEl = article;
    } else {
      const editBtn = document.createElement("button");
      editBtn.type = "button";
      editBtn.className = "messageActionBtn";
      editBtn.dataset.action = "edit";
      editBtn.textContent = "编辑";
      editBtn.title = "截断下方界面气泡并重发；引擎上下文不会真正回退";
      editBtn.onclick = () => enterUserEditMode(article);
      actions.append(editBtn);
    }
    header.append(actions);
  }
  const body = document.createElement("div");
  body.className = "chatMessageText markdownBody";
  article.append(header, body);
  // Markdown / attachments are rendered in appendChatMessage after the article is
  // attached to the DOM. renderMessageMarkdown and renderMessageAttachments both
  // bail out when !article.isConnected, which left user (and history) bubbles
  // showing only the role label "你".
  refreshMessageActionButtons();
  return article;
}

function appendChatMessage(role, text, model = "", final = false, attachments = null, media = null, sessionID = "") {
  if (!historyMountSilent) removeChatEmpty();
  const article = createChatMessage(role, text, model, final, attachments, media, sessionID);
  chatMessagesRoot().append(article);
  renderMessageMarkdown(article, final);
  if (role === "user" && Array.isArray(attachments) && attachments.length) {
    renderMessageAttachments(article, attachments);
  }
  if (Array.isArray(media) && media.length) renderMessageMedia(article, media, sessionID);
  if (role === "user" && !historyMountSilent) addChatNodeFor(article);
  if (!historyMountSilent) scrollChatToBottom();
  return article;
}

function refreshMessageActionButtons() {
  const busy = state.agentStatus?.state === "busy" || !!state.agentStatus?.busy;
  const ready = state.agentStatus?.state === "ready" && state.agentEngineState !== "loading" && state.agentEngineState !== "readonly";
  document.querySelectorAll('.chatMessage.assistant .messageActionBtn[data-action="regenerate"]').forEach((button) => {
    const article = button.closest(".chatMessage");
    const isLast = article === lastAssistantMessageEl;
    button.hidden = !isLast;
    button.disabled = !isLast || busy || !ready || !state.lastUserMessage;
  });
}

function appendAssistantChunk(text, sessionID = "") {
  if (!text) return;
  markAgentRetryRecovered();
  document.querySelectorAll(".chatMessage.system").forEach((notice) => {
    if ((notice._rawText || "").includes("正在重新生成")) notice.remove();
  });
  if (!agentActiveAssistant || !agentActiveAssistant.isConnected) {
    agentActiveAssistant = appendChatMessage("assistant", "", state.agentStatus?.model || state.activeAgentSession?.model || "", false, null, null, sessionID);
  }
  if (sessionID) agentActiveAssistant.dataset.sessionId = sessionID;
  agentActiveAssistant._rawText = (agentActiveAssistant._rawText || "") + text;
  scheduleMessageMarkdown(agentActiveAssistant);
  scrollChatToBottom();
}

function scheduleMessageMarkdown(article) {
  clearTimeout(article._markdownTimer);
  // Throttle streaming re-renders: a single trailing pass per ~220ms avoids
  // re-parsing the whole assistant body on every token chunk.
  article._markdownTimer = setTimeout(() => renderMessageMarkdown(article, false), 220);
}

function finalizeAssistantMessage() {
  if (!agentActiveAssistant?.isConnected) return;
  clearTimeout(agentActiveAssistant._markdownTimer);
  renderMessageMarkdown(agentActiveAssistant, true);
  lastAssistantMessageEl = agentActiveAssistant;
  refreshMessageActionButtons();
}

async function renderMessageMarkdown(article, final = false) {
  if (!article) return;
  const body = article.querySelector(".chatMessageText");
  if (!body) return;
  const raw = article._rawText || "";
  // Generation stamp: history load / streaming can schedule overlapping renders;
  // a stale async pass must not wipe a newer body.
  const gen = (article._mdGen = (article._mdGen || 0) + 1);
  if (!window.marked?.parse || !window.DOMPurify) {
    if (article._mdGen === gen) body.textContent = raw;
    return;
  }
  try {
    // Always write text into the body — do not require isConnected. Gating on
    // isConnected previously left both user ("你") and assistant ("Grok") bubbles
    // with an empty .chatMessageText when createChatMessage rendered pre-append.
    const parsed = window.marked.parse(prepareMarkdownCitations(raw), { gfm: true, breaks: true });
    if (article._mdGen !== gen) return;
    body.innerHTML = window.DOMPurify.sanitize(parsed, { USE_PROFILES: { html: true } });
    body.querySelectorAll("a").forEach((link) => {
      link.target = "_blank";
      link.rel = "noopener noreferrer";
      if (/^\[?\d+\]?$/.test(link.textContent.trim()) || link.closest(".citation")) {
        link.classList.add("citationLink");
      }
    });
    body.querySelectorAll("img").forEach((image) => {
      image.loading = "lazy";
      image.decoding = "async";
      image.referrerPolicy = "no-referrer";
    });
    body.querySelectorAll("table").forEach((table) => {
      const wrapper = document.createElement("div");
      wrapper.className = "markdownTableWrap";
      table.replaceWith(wrapper);
      wrapper.append(table);
    });
    body.querySelectorAll('li > input[type="checkbox"]').forEach((checkbox) => {
      checkbox.disabled = true;
      checkbox.closest("li")?.classList.add("task-list-item");
      checkbox.closest("ul, ol")?.classList.add("contains-task-list");
    });
    if (final) {
      decorateCodeBlocks(body);
      renderMathBlocks(body);
      // Mermaid needs layout metrics; only run once the bubble is in the document.
      if (article.isConnected) await renderMermaidBlocks(body);
    } else {
      decorateCodeBlocksLite(body);
      renderMathBlocks(body);
    }
  } catch (err) {
    if (article._mdGen !== gen) return;
    body.textContent = raw;
    body.title = `Markdown 渲染失败: ${err.message}`;
  }
}

function decorateCodeBlocksLite(root) {
  root.querySelectorAll("pre").forEach((pre) => {
    if (pre.querySelector(".codeLanguageLabel")) return;
    const code = pre.querySelector("code");
    if (!code || code.classList.contains("language-mermaid")) return;
    const declaredLanguage = [...code.classList].find((name) => name.startsWith("language-"))?.slice(9) || "text";
    const languageLabel = document.createElement("span");
    languageLabel.className = "codeLanguageLabel";
    languageLabel.textContent = declaredLanguage;
    pre.append(languageLabel);
  });
}

function prepareMarkdownCitations(markdown) {
  const definitions = new Map();
  const lines = String(markdown || "").split(/\r?\n/);
  let fenced = false;
  const retained = [];
  for (const line of lines) {
    if (/^\s*(```|~~~)/.test(line)) fenced = !fenced;
    if (!fenced) {
      const match = line.match(/^\s*\[\^([^\]]+)\]:\s+(https?:\/\/\S+)(?:\s+(.+))?\s*$/);
      if (match) {
        try {
          const parsed = new URL(match[2]);
          if (parsed.protocol === "http:" || parsed.protocol === "https:") {
            definitions.set(match[1], { url: parsed.href, title: (match[3] || "").trim() });
            continue;
          }
        } catch {
          // Keep malformed definitions as ordinary Markdown text.
        }
      }
    }
    retained.push(line);
  }
  if (!definitions.size) return markdown;
  fenced = false;
  return retained.map((line) => {
    if (/^\s*(```|~~~)/.test(line)) fenced = !fenced;
    if (fenced) return line;
    return line.replace(/\[\^([^\]]+)\]/g, (whole, id) => {
      const citation = definitions.get(id);
      if (!citation) return whole;
      const title = citation.title ? ` title="${escapeAttr(citation.title)}"` : "";
      return `<sup class="citation"><a href="${escapeAttr(citation.url)}"${title}>[${escapeHtml(id)}]</a></sup>`;
    });
  }).join("\n");
}

function decorateCodeBlocks(root) {
  root.querySelectorAll("pre").forEach((pre) => {
    const code = pre.querySelector("code");
    if (!code || code.classList.contains("language-mermaid")) return;
    const declaredLanguage = [...code.classList].find((name) => name.startsWith("language-"))?.slice(9) || "";
    const highlighter = window.hljs;
    if (highlighter) {
      try {
        if (declaredLanguage && highlighter.getLanguage(declaredLanguage)) {
          highlighter.highlightElement(code);
        } else if (!declaredLanguage) {
          const result = highlighter.highlightAuto(code.textContent || "");
          code.innerHTML = result.value;
          code.classList.add("hljs");
          if (result.language) code.dataset.detectedLanguage = result.language;
        }
      } catch {
        // Keep the original escaped code if highlighting fails.
      }
    }
    const language = declaredLanguage || code.dataset.detectedLanguage || "text";
    const languageLabel = document.createElement("span");
    languageLabel.className = "codeLanguageLabel";
    languageLabel.textContent = language;
    pre.append(languageLabel);
    const button = document.createElement("button");
    button.type = "button";
    button.className = "codeCopyBtn";
    button.textContent = "复制";
    button.onclick = () => copyText(code.textContent || "", "代码已复制");
    pre.append(button);
  });
}

function renderMathBlocks(root) {
  if (typeof window.renderMathInElement !== "function") return;
  try {
    window.renderMathInElement(root, {
      delimiters: [
        { left: "$$", right: "$$", display: true },
        { left: "\\[", right: "\\]", display: true },
        { left: "$", right: "$", display: false },
        { left: "\\(", right: "\\)", display: false },
      ],
      ignoredTags: ["script", "noscript", "style", "textarea", "pre", "code"],
      throwOnError: false,
      strict: "warn",
      trust: false,
    });
  } catch {
    // Incomplete streaming formulas remain as source until the next chunk.
  }
}

/** Dynamically load the ~3.5MB Mermaid bundle once, only when a diagram is needed. */
function loadMermaid() {
  if (window.mermaid?.render) {
    return Promise.resolve(window.mermaid);
  }
  if (mermaidLoadPromise) {
    return mermaidLoadPromise;
  }
  mermaidLoadPromise = new Promise((resolve, reject) => {
    const existing = document.querySelector("script[data-gs-mermaid]");
    if (existing) {
      const settle = () => {
        if (window.mermaid?.render) resolve(window.mermaid);
        else reject(new Error("Mermaid loaded but API unavailable"));
      };
      if (window.mermaid?.render) {
        settle();
        return;
      }
      existing.addEventListener("load", settle, { once: true });
      existing.addEventListener("error", () => {
        mermaidLoadPromise = null;
        reject(new Error("Mermaid script failed to load"));
      }, { once: true });
      return;
    }
    const script = document.createElement("script");
    script.src = MERMAID_SRC;
    script.async = true;
    script.dataset.gsMermaid = "1";
    script.onload = () => {
      if (window.mermaid?.render) resolve(window.mermaid);
      else {
        mermaidLoadPromise = null;
        reject(new Error("Mermaid loaded but API unavailable"));
      }
    };
    script.onerror = () => {
      mermaidLoadPromise = null;
      reject(new Error("Mermaid script failed to load"));
    };
    document.head.appendChild(script);
  });
  return mermaidLoadPromise;
}

function ensureMermaidInitialized(mermaidAPI) {
  if (mermaidReady) return;
  mermaidAPI.initialize({
    startOnLoad: false,
    securityLevel: "strict",
    theme: "base",
    themeVariables: {
      background: "#ecebe7",
      primaryColor: "#dedaf7",
      primaryTextColor: "#202023",
      primaryBorderColor: "#665bb2",
      lineColor: "#54545b",
      secondaryColor: "#dce7df",
      tertiaryColor: "#f2e3ca",
      fontFamily: "Aptos, Segoe UI, sans-serif",
    },
    flowchart: { htmlLabels: true },
  });
  mermaidReady = true;
}

async function renderMermaidBlocks(root) {
  const blocks = [...root.querySelectorAll("pre > code.language-mermaid")];
  if (!blocks.length) return;

  let mermaidAPI;
  try {
    mermaidAPI = await loadMermaid();
  } catch (err) {
    for (const code of blocks) {
      const pre = code.parentElement;
      if (!pre) continue;
      const container = document.createElement("div");
      container.className = "mermaidDiagram mermaidError";
      container.textContent = `Mermaid 库加载失败\n${err.message || err}`;
      pre.replaceWith(container);
    }
    return;
  }

  ensureMermaidInitialized(mermaidAPI);

  for (const code of blocks) {
    const pre = code.parentElement;
    if (!pre || !pre.isConnected) continue;
    const container = document.createElement("div");
    container.className = "mermaidDiagram";
    pre.replaceWith(container);
    try {
      const id = `grok-mermaid-${++mermaidRenderID}`;
      const result = await mermaidAPI.render(id, code.textContent || "");
      // Mermaid strict mode sanitizes labels itself. DOMPurify 3.4+ removes
      // foreignObject contents on a second pass, which leaves node labels blank.
      container.innerHTML = result.svg;
      result.bindFunctions?.(container);
    } catch (err) {
      container.classList.add("mermaidError");
      container.textContent = `Mermaid 图表无法渲染\n${err.message}`;
    }
  }
}

function renderAgentRetry(retry) {
  if (!retry) return;
  removeChatEmpty();
  const stateName = retry.state || "retrying";
  if (!agentRetryNotice || !agentRetryNotice.isConnected) {
    agentRetryNotice = appendChatMessage("system", "");
    agentRetryNotice.classList.add("agentRetry", "error");
  }
  const label = agentRetryNotice.querySelector(".chatMessageRole");
  const body = agentRetryNotice.querySelector(".chatMessageText");
  if (stateName === "retrying") {
    label.textContent = retry.max_retries
      ? `上游重试 ${retry.attempt || 0}/${retry.max_retries}`
      : "上游重试中";
    agentRetryNotice._rawText = compactAgentError(retry.reason || retry.message || "模型请求失败，正在重试");
  } else if (stateName === "exhausted") {
    label.textContent = "上游重试已耗尽";
    agentRetryNotice._rawText = compactAgentError(retry.reason || retry.message || "模型请求重试已耗尽");
    agentRetryNotice = null;
  } else {
    label.textContent = "上游请求失败";
    agentRetryNotice._rawText = compactAgentError(retry.message || retry.reason || "模型请求失败");
    agentRetryNotice = null;
  }
  renderMessageMarkdown(agentRetryNotice || body.closest(".chatMessage"), true);
  scrollChatToBottom();
}

function compactAgentError(message) {
  const text = String(message || "").trim();
  const firstBlock = text.split(/\r?\n\r?\n/)[0] || text;
  return firstBlock.length > 600 ? `${firstBlock.slice(0, 600)}…` : firstBlock;
}

function markAgentRetryRecovered() {
  if (!agentRetryNotice || !agentRetryNotice.isConnected) return;
  agentRetryNotice.classList.remove("error");
  agentRetryNotice.classList.add("recovered");
  agentRetryNotice.querySelector(".chatMessageRole").textContent = "上游已恢复";
  agentRetryNotice = null;
}

function appendThoughtChunk(text) {
  if (!text) return;
  if (!historyMountSilent) removeChatEmpty();
  if (!agentActiveThought || !agentActiveThought.isConnected) {
    const details = document.createElement("details");
    details.className = "agentThought";
    // Keep thoughts collapsed by default — large reasoning blocks dominate history.
    details.open = false;
    const summary = document.createElement("summary");
    summary.textContent = "思考过程";
    const body = document.createElement("pre");
    details.append(summary, body);
    chatMessagesRoot().append(details);
    agentActiveThought = details;
  }
  agentActiveThought.querySelector("pre").textContent += text;
  if (!historyMountSilent) scrollChatToBottom();
}

function bindToolPayloadLazyLoad(details) {
  if (!details || details.dataset.lazyBound === "1") return;
  details.dataset.lazyBound = "1";
  details.addEventListener("toggle", () => {
    if (!details.open) return;
    const pre = details.querySelector("pre.agentToolDetail");
    if (!pre || details.dataset.payloadReady === "1") return;
    pre.textContent = details._toolPayload || "（无内容）";
    pre.classList.remove("agentToolDetailPlaceholder");
    details.dataset.payloadReady = "1";
  });
}

function setToolPayloadLazy(details, payload) {
  const pre = details.querySelector("pre.agentToolDetail");
  if (!pre) return;
  const formatted = payload == null ? "" : formatAgentPayload(payload);
  details._toolPayload = formatted;
  // Never auto-expand. While closed, keep a short placeholder so long histories
  // do not pay for multi-KB tool text in the layout until the user opens them.
  if (details.open) {
    pre.textContent = formatted || "（无内容）";
    pre.classList.remove("agentToolDetailPlaceholder");
    details.dataset.payloadReady = "1";
  } else {
    pre.textContent = formatted ? "点击展开查看输入/输出" : "（无内容）";
    pre.classList.add("agentToolDetailPlaceholder");
    details.dataset.payloadReady = "0";
  }
  pre.hidden = false;
  bindToolPayloadLazyLoad(details);
}

function renderAgentTool(tool, isUpdate, sessionID = "") {
  if (!historyMountSilent) removeChatEmpty();
  const id = tool.id || `tool-${agentTools.size + 1}`;
  let details = agentTools.get(id);
  if (!details) {
    details = document.createElement("details");
    details.className = "agentTool";
    details.open = false;
    details.innerHTML = `<summary><span class="agentToolSummaryMain"><span class="agentToolTitle"></span><span class="agentToolStatus"></span></span></summary><pre class="agentToolDetail agentToolDetailPlaceholder"></pre>`;
    chatMessagesRoot().append(details);
    agentTools.set(id, details);
    bindToolPayloadLazyLoad(details);
  }
  // Always force collapsed during history mount / streaming updates unless user opened it.
  if (!details.dataset.userOpened) details.open = false;
  details.addEventListener("toggle", () => {
    if (details.open) details.dataset.userOpened = "1";
  }, { once: true });

  const title = tool.title || details.querySelector(".agentToolTitle").textContent || "工具调用";
  const status = tool.status || details.dataset.status || (isUpdate ? "更新" : "等待");
  details.dataset.status = status;
  details.querySelector(".agentToolTitle").textContent = title;
  details.querySelector(".agentToolStatus").textContent = agentToolStatusLabel(status);
  const payload = tool.raw_output ?? tool.raw_input;
  if (payload != null) setToolPayloadLazy(details, payload);
  else if (!details._toolPayload) setToolPayloadLazy(details, null);

  if (!historyMountSilent) renderToolActivity(tool, id, title, status);
  const toolHint = `${tool.kind || ""} ${title}`;
  const structuredMedia = Array.isArray(tool.media) ? tool.media.map((item) => ({
    ...item,
    kind: inferMediaKind(item.kind === "resource" ? toolHint : item.kind, item.mime_type || item.mimeType || "", item.uri || item.url || ""),
  })) : [];
  // Avoid scanning huge raw_output for URLs while mounting long history; structured media still shows.
  const media = structuredMedia.length
    ? structuredMedia
    : (historyMountSilent ? [] : extractMediaFromPayload(tool.raw_output, toolHint));
  if (media.length) appendAssistantMedia(media, sessionID);
  if (!historyMountSilent) scrollChatToBottom();
}

function appendAssistantMedia(media, sessionID = "") {
  if (!Array.isArray(media) || !media.length) return;
  markAgentRetryRecovered();
  if (!agentActiveAssistant || !agentActiveAssistant.isConnected) {
    agentActiveAssistant = appendChatMessage("assistant", "", state.agentStatus?.model || state.activeAgentSession?.model || "", false, null, null, sessionID);
  }
  if (sessionID) agentActiveAssistant.dataset.sessionId = sessionID;
  renderMessageMedia(agentActiveAssistant, media, sessionID);
  if (!historyMountSilent) scrollChatToBottom();
}

function renderToolActivity(tool, id, title, status) {
  const list = $("toolActivityList");
  if (!list) return;
  if (list.children.length === 1 && list.firstElementChild?.tagName === "SPAN") list.innerHTML = "";
  let item = [...list.querySelectorAll(".toolActivityItem")].find((element) => element.dataset.toolId === id);
  if (!item) {
    item = document.createElement("div");
    item.className = "toolActivityItem";
    item.dataset.toolId = id;
    item.innerHTML = `<strong></strong><span></span>`;
    list.prepend(item);
  }
  item.classList.remove("pending", "in_progress", "completed", "failed");
  if (status) item.classList.add(status);
  item.querySelector("strong").textContent = title;
  item.querySelector("span").textContent = [agentToolStatusLabel(status), tool.kind].filter(Boolean).join(" · ");
  if ($("toolActivityCount")) $("toolActivityCount").textContent = String(agentTools.size);
}

function agentToolStatusLabel(status) {
  return ({ pending: "等待", in_progress: "执行中", completed: "完成", failed: "失败" })[status] || status || "";
}

function formatAgentPayload(payload) {
  if (typeof payload === "string") return payload;
  try {
    return JSON.stringify(payload, null, 2);
  } catch {
    return String(payload);
  }
}

function appendAgentNotice(text, isError = false) {
  const notice = appendChatMessage("system", text);
  if (isError) notice.classList.add("error");
}

function showAgentPermission(permission) {
  if (!permission) return;
  state.agentPermission = permission;
  $("permissionSummary").textContent = permission.summary || permission.tool?.title || "工具执行请求";
  $("permissionDetail").textContent = permission.tool?.raw_input == null ? "" : formatAgentPayload(permission.tool.raw_input);
  const options = Array.isArray(permission.options) ? permission.options : [];
  const once = options.find((o) => /allow.?once|allow_once/i.test(o.kind || "") || /一次|once/i.test(o.name || ""));
  const always = options.find((o) => /allow.?always|allow_always|session/i.test(o.kind || "") || /会话|always|session/i.test(o.name || ""));
  const reject = options.find((o) => /reject/i.test(o.kind || "") || /拒绝|deny|reject/i.test(o.name || ""));
  if ($("permissionAllowBtn")) {
    $("permissionAllowBtn").dataset.optionId = once?.id || "";
    $("permissionAllowBtn").textContent = once?.name || "允许一次";
  }
  if ($("permissionSessionBtn")) {
    $("permissionSessionBtn").dataset.optionId = always?.id || "";
    $("permissionSessionBtn").textContent = always?.name || "本会话允许";
    $("permissionSessionBtn").hidden = !always && options.length > 0 && !options.some((o) => /always/i.test(o.kind || ""));
  }
  if ($("permissionRejectBtn")) {
    $("permissionRejectBtn").dataset.optionId = reject?.id || "";
  }
  $("permissionBar").hidden = false;
  scrollChatToBottom(true);
}

function showAgentPlan(plan, waiting) {
  if (!plan) return;
  state.agentPlan = { ...plan, waiting: waiting || plan.waiting };
  if ($("planSummary")) $("planSummary").textContent = waiting ? "需要确认执行计划" : "计划更新";
  if ($("planBody")) {
    if (plan.body) {
      $("planBody").textContent = plan.body;
    } else if (Array.isArray(plan.entries) && plan.entries.length) {
      $("planBody").textContent = plan.entries.map((e, i) =>
        `${i + 1}. ${e.content || ""}${e.status ? ` [${e.status}]` : ""}`).join("\n");
    } else {
      $("planBody").textContent = "（无计划正文）";
    }
  }
  const showActions = !!(plan.request_id && (waiting || plan.waiting));
  ["planApproveBtn", "planReviseBtn", "planDismissBtn"].forEach((id) => {
    if ($(id)) $(id).hidden = !showActions;
  });
  if ($("planBar")) $("planBar").hidden = false;
  scrollChatToBottom(true);
}

function respondAgentPlan(outcome) {
  const plan = state.agentPlan;
  if (!plan?.request_id) {
    if ($("planBar")) $("planBar").hidden = true;
    return;
  }
  let feedback = "";
  if (outcome === "cancelled") {
    feedback = prompt("请说明希望如何修改计划（可留空）", "") || "";
  }
  if (!agentSocket || agentSocket.readyState !== WebSocket.OPEN) {
    toast("对话连接已断开，请稍后重试", "error");
    return;
  }
  agentSocket.send(JSON.stringify({
    type: "plan_response",
    request_id: plan.request_id,
    outcome,
    feedback,
  }));
  state.agentPlan = null;
  if ($("planBar")) $("planBar").hidden = true;
  appendAgentNotice(outcome === "approved" ? "已批准计划" : outcome === "cancelled" ? "已请求修改计划" : "已忽略计划");
}

function respondAgentPermission(allow, sessionScope = false) {
  const permission = state.agentPermission;
  if (!permission) return;
  if (!agentSocket || agentSocket.readyState !== WebSocket.OPEN) {
    toast("对话连接已断开，请稍后重试", "error");
    return;
  }
  const optionId = allow
    ? (sessionScope ? ($("permissionSessionBtn")?.dataset.optionId || "") : ($("permissionAllowBtn")?.dataset.optionId || ""))
    : ($("permissionRejectBtn")?.dataset.optionId || "");
  const remember = allow && sessionScope;
  agentSocket.send(JSON.stringify({
    type: "permission_response",
    request_id: permission.request_id,
    allow,
    remember,
    option_id: optionId || undefined,
  }));
  state.agentPermission = null;
  $("permissionBar").hidden = true;
  if (allow && remember) {
    renderAgentStatus({ ...state.agentStatus, session_auto_approve: true });
    if ($("agentSessionAutoApprove")) $("agentSessionAutoApprove").checked = true;
    appendAgentNotice("已允许，并在本会话自动批准后续工具");
  } else {
    appendAgentNotice(allow ? "已允许本次工具执行" : "已拒绝本次工具执行");
  }
}

async function startAgent() {
  const cwd = $("agentCwd").value.trim();
  if (!cwd) throw new Error("请提供工作目录");
  const alwaysApprove = $("agentAlwaysApprove").checked;
  if (alwaysApprove && !state.agentAutoRestoring && !confirm("自动批准会允许 Grok Build 无需确认即可修改文件和执行命令。确定启动？")) return false;
  const wasRunning = agentIsRunning();
  if (wasRunning) {
    await api("/api/agent/stop", { method: "POST", body: "{}" });
  }
  const resumable = state.activeAgentSession?.id && state.activeAgentSession?.cwd === cwd;
  if (resumable) setAgentEngineState("loading", "正在恢复引擎上下文…");
  let status;
  try {
    status = await api("/api/agent/start", {
      method: "POST",
      body: JSON.stringify({ cwd, always_approve: alwaysApprove, session_id: resumable ? state.activeAgentSession.id : "" }),
    });
  } catch (err) {
    if (resumable && handleSessionLoadFallback(err)) return false;
    if (resumable) setAgentEngineState("readonly", "仅显示本地历史，引擎上下文恢复失败。请开启新对话后再发送消息。");
    throw err;
  }
  setAgentEngineState("attached");
  if (!resumable) clearAgentTranscript();
  state.settings = { ...(state.settings || {}), agent_default_cwd: status.cwd || cwd };
  if (!resumable) {
    state.activeAgentSession = { id: status.session_id, title: "新对话", cwd: status.cwd || cwd, model: status.model || "" };
  }
  renderAgentStatus(status);
  updateConversationIdentity();
  persistLastChatContext({
    sessionId: status.session_id,
    cwd: status.cwd || cwd,
    title: state.activeAgentSession?.title,
    model: status.model,
    alwaysApprove,
  });
  connectAgentSocket();
  return true;
}

/**
 * Create a new chat under the current workspace (agentCwd).
 * Prefer an active project path; if none, require a selected directory.
 */
async function newAgentSession(projectOrNull) {
  // Optional project: set cwd first so the session belongs to that workspace.
  if (projectOrNull && projectOrNull.path) {
    const activated = await activateProjectWorkspace(projectOrNull, { createSession: false });
    if (!activated) return false;
  }
  const cwd = $("agentCwd")?.value.trim() || "";
  if (!cwd) {
    throw new Error("请先选择或添加一个项目（工作空间）");
  }
  const matched = projectPathKeys().get(normalizePathKey(cwd));
  if (matched && !matched.trusted) {
    if (!confirm(`项目「${matched.name}」尚未信任，创建会话前需要信任该目录。继续？`)) {
      return false;
    }
    await api("/api/agent/projects/trust", { method: "POST", body: JSON.stringify({ id: matched.id }) });
    matched.trusted = true;
    await loadAgentProjects();
  }
  if (matched) setProjectExpanded(matched.id, true);

  state.activeAgentSession = null;
  let status;
  if (!agentIsRunning()) {
    const started = await startAgent();
    if (started === false) return false;
    status = state.agentStatus;
  } else {
    status = await api("/api/agent/session", { method: "POST", body: JSON.stringify({ cwd }) });
  }
  clearAgentTranscript();
  setAgentEngineState("attached");
  state.activeAgentSession = { id: status.session_id, title: "新对话", cwd: status.cwd || cwd, model: status.model || "" };
  state.settings = { ...(state.settings || {}), agent_default_cwd: status.cwd || cwd };
  renderAgentStatus(status);
  updateConversationIdentity();
  persistLastChatContext({
    sessionId: status.session_id,
    cwd: status.cwd || cwd,
    title: "新对话",
    model: status.model,
    alwaysApprove: $("agentAlwaysApprove")?.checked,
  });
  closeNativeChatPanels();
  loadAgentSessions().catch(() => {});
  return true;
}

/** Switch agentCwd to a project workspace (trust + open bookkeeping). */
async function activateProjectWorkspace(project, { createSession = false } = {}) {
  if (!project?.id) return false;
  if (project.path_ok === false) {
    const fixed = await relocateProjectPath(project);
    if (!fixed) return false;
    project = (state.projects || []).find((p) => p.id === project.id) || project;
  }
  if (!project.trusted) {
    if (!confirm(`首次打开项目「${project.name}」需要信任该目录，以允许 Agent 读写。继续？`)) return false;
    await api("/api/agent/projects/trust", { method: "POST", body: JSON.stringify({ id: project.id }) });
    project.trusted = true;
  }
  try {
    await api("/api/agent/projects/open", { method: "POST", body: JSON.stringify({ id: project.id }) });
  } catch (err) {
    // Path became invalid (e.g. old garbled Chinese path) — offer re-pick.
    const msg = err?.message || String(err || "");
    if (/路径不可用|不是有效目录|重新选择/i.test(msg)) {
      const fixed = await relocateProjectPath(project);
      if (!fixed) return false;
      project = (state.projects || []).find((p) => p.id === project.id) || project;
    } else {
      // open may fail on race; path still usable for local cwd
    }
  }
  if ($("agentCwd")) $("agentCwd").value = project.path;
  state.activeProjectId = project.id;
  setProjectExpanded(project.id, true);
  updateComposerProjectLabel();
  if (createSession) {
    return newAgentSession(project);
  }
  renderSidebarTree();
  return true;
}

async function stopAgent() {
  const status = await api("/api/agent/stop", { method: "POST", body: "{}" });
  state.agentPermission = null;
  $("permissionBar").hidden = true;
  renderAgentStatus(status);
}

async function cancelAgentGeneration() {
  if (!(state.agentStatus?.state === "busy" || state.agentStatus?.busy)) {
    toast("当前没有正在生成的回复", "info");
    return;
  }
  if ($("chatStopBtn")) $("chatStopBtn").disabled = true;
  try {
    if (agentSocket && agentSocket.readyState === WebSocket.OPEN) {
      agentSocket.send(JSON.stringify({ type: "cancel" }));
    } else {
      await api("/api/agent/cancel", { method: "POST", body: "{}" });
    }
  } catch (err) {
    if ($("chatStopBtn")) $("chatStopBtn").disabled = false;
    throw err;
  }
}

async function regenerateLastAssistant(article = lastAssistantMessageEl) {
  const text = state.lastUserMessage;
  if (!text) throw new Error("没有可重新生成的用户消息");
  if (state.agentStatus?.state === "busy" || state.agentStatus?.busy) {
    throw new Error("正在生成中，请先停止或稍后再试");
  }
  if (state.agentStatus?.state !== "ready" || state.agentEngineState === "loading" || state.agentEngineState === "readonly") {
    throw new Error("当前会话不可用，请先启动 Agent");
  }
  if (!agentSocket || agentSocket.readyState !== WebSocket.OPEN) {
    toast("对话连接尚未就绪", "error");
    connectAgentSocket();
    return;
  }
  if (article?.isConnected && article.dataset.role === "assistant") {
    article.remove();
  }
  if (lastAssistantMessageEl === article) lastAssistantMessageEl = null;
  agentActiveAssistant = null;
  agentActiveThought = null;
  agentRetryNotice = null;
  const rewind = await rewindDropLastUser();
  const _mSend = $("composerModelSelect")?.value || "";
  const _rawSSend = $("composerStrengthSelect")?.value || "";
  const _sSend = _rawSSend === "auto" ? "" : _rawSSend;
  agentSocket.send(JSON.stringify({ type: "user_message", text, model: _mSend, strength: _sSend }));
  renderAgentStatus({ ...state.agentStatus, state: "busy", running: true, busy: true });
  if (rewind?.ok) {
    appendAgentNotice("正在重新生成…（已回退引擎上一轮）");
  } else {
    appendAgentNotice(`正在重新生成…（rewind 未成功${rewind?.error ? "：" + rewind.error : ""}）`);
  }
  forceScrollChatToBottom();
}

// --- 附件 / 截图上传（走 HTTP 落盘，WS 只传 path） ---
const TEXT_FILE_MAX = 1_000_000;     // 1 MB cap for text attachments
const IMAGE_FILE_MAX = 16_000_000;   // 16 MB cap for image attachments

async function uploadChatFile(file, kindHint = "") {
  const isImage = (kindHint || file.type || "").startsWith("image/") || /\.(png|jpe?g|gif|webp|bmp|avif)$/i.test(file.name || "");
  if (isImage && file.size > IMAGE_FILE_MAX) {
    throw new Error(`图片过大（${(file.size / 1_000_000).toFixed(1)} MB），上限 16 MB`);
  }
  if (!isImage && file.size > TEXT_FILE_MAX) {
    throw new Error(`文件过大（${(file.size / 1_000_000).toFixed(1)} MB），上限 1 MB`);
  }
  const form = new FormData();
  form.append("file", file, file.name || (isImage ? "image.png" : "file.txt"));
  form.append("kind", isImage ? "image" : "text_file");
  if (file.type) form.append("mime_type", file.type);
  if (state.agentStatus?.session_id) form.append("session_id", state.agentStatus.session_id);
  const response = await fetch("/api/agent/upload", { method: "POST", body: form, credentials: "same-origin" });
  const data = await response.json().catch(() => ({}));
  if (!response.ok) throw new Error(data.error || `上传失败 HTTP ${response.status}`);
  const previewUrl = isImage ? URL.createObjectURL(file) : "";
  return {
    kind: data.kind || (isImage ? "image" : "text_file"),
    name: data.name || file.name || "file",
    path: data.path,
    mimeType: data.mime_type || file.type || "",
    previewUrl,
    dataUrl: previewUrl,
  };
}

async function handleChatFiles(fileList) {
  const files = Array.from(fileList || []);
  if (!files.length) return;
  for (const file of files) {
    try {
      state.pendingAttachments.push(await uploadChatFile(file));
    } catch (err) {
      toast(`${file.name || "附件"}：${err.message || String(err)}`, "error");
    }
  }
  renderChatAttachments();
  updateChatComposerState();
}

function addPathAttachment(path, name = "") {
  path = String(path || "").trim();
  if (!path) return;
  const base = name || path.replace(/^.*[\\/]/, "") || path;
  if (state.pendingAttachments.some((a) => a.path === path)) return;
  state.pendingAttachments.push({ kind: "path", name: base, path });
  renderChatAttachments();
  updateChatComposerState();
}

function renderChatAttachments() {
  const wrap = $("chatAttachments");
  if (!wrap) return;
  wrap.innerHTML = "";
  if (!state.pendingAttachments.length) { wrap.hidden = true; return; }
  wrap.hidden = false;
  state.pendingAttachments.forEach((att, index) => {
    const chip = document.createElement("div");
    chip.className = "chatAttachment";
    if (att.kind === "image" && att.dataUrl) {
      const img = document.createElement("img");
      img.src = att.dataUrl;
      img.alt = att.name || "图片";
      chip.append(img);
    } else {
      const icon = document.createElement("span");
      icon.className = "chatAttachmentIcon";
      const ext = (att.name || "").split(".").pop() || "txt";
      icon.textContent = ext.slice(0, 4).toUpperCase();
      chip.append(icon);
    }
    const name = document.createElement("span");
    name.className = "chatAttachmentName";
    name.textContent = att.name || (att.kind === "image" ? "图片" : "文件");
    chip.append(name);
    const remove = document.createElement("button");
    remove.type = "button";
    remove.className = "chatAttachmentRemove";
    remove.textContent = "×";
    remove.setAttribute("aria-label", `移除 ${att.name || "附件"}`);
    remove.onclick = () => { state.pendingAttachments.splice(index, 1); renderChatAttachments(); updateChatComposerState(); };
    chip.append(remove);
    wrap.append(chip);
  });
}

function clearChatAttachments() {
  state.pendingAttachments = [];
  renderChatAttachments();
}

function renderMessageAttachments(article, attachments) {
  // Do not require isConnected — history prepend mounts into a DocumentFragment first.
  if (!article || !Array.isArray(attachments) || !attachments.length) return;
  const wrap = document.createElement("div");
  wrap.className = "chatMessageAttachments";
  for (const att of attachments) {
    if (att.kind === "image" && att.dataUrl) {
      const img = document.createElement("img");
      img.src = att.dataUrl;
      img.alt = att.name || "图片";
      img.loading = "lazy";
      img.onclick = () => window.open(att.dataUrl, "_blank");
      wrap.append(img);
    } else {
      const chip = document.createElement("span");
      chip.className = "chatMessageFileChip";
      chip.textContent = `📎 ${att.name || "文件"}${att.truncated ? "（已截断）" : ""}`;
      wrap.append(chip);
    }
  }
  article.append(wrap);
}

function renderMessageMedia(article, mediaItems, sessionID = "") {
  // Do not require isConnected — history prepend mounts into a DocumentFragment first.
  if (!article || !Array.isArray(mediaItems) || !mediaItems.length) return;
  sessionID = sessionID || article.dataset.sessionId || state.activeAgentSession?.id || state.agentStatus?.session_id || "";
  let wrap = [...article.children].find((child) => child.classList?.contains("chatMessageMedia"));
  if (!wrap) {
    wrap = document.createElement("div");
    wrap.className = "chatMessageMedia";
    wrap._mediaKeys = new Set();
    article.append(wrap);
  }
  if (!(wrap._mediaKeys instanceof Set)) wrap._mediaKeys = new Set();

  for (const media of mediaItems) {
    const normalized = normalizeStructuredMedia(media, sessionID);
    if (!normalized || wrap._mediaKeys.has(normalized.key)) continue;
    wrap._mediaKeys.add(normalized.key);
    const item = document.createElement("figure");
    item.className = `chatMediaItem ${normalized.kind}`;

    if (normalized.kind === "image") {
      const link = document.createElement("a");
      link.href = normalized.src;
      link.target = "_blank";
      link.rel = "noopener noreferrer";
      link.title = "打开原图";
      const image = document.createElement("img");
      image.src = normalized.src;
      image.alt = normalized.label || "Grok 生成的图片";
      image.loading = "lazy";
      image.decoding = "async";
      image.referrerPolicy = "no-referrer";
      image.onerror = () => markMediaUnavailable(item, normalized.fallback);
      link.append(image);
      item.append(link);
    } else if (normalized.kind === "video") {
      const video = document.createElement("video");
      video.controls = true;
      video.preload = "metadata";
      video.playsInline = true;
      video.src = normalized.src;
      if (normalized.mimeType) video.type = normalized.mimeType;
      video.onerror = () => markMediaUnavailable(item, normalized.fallback);
      item.append(video);
    } else if (normalized.kind === "audio") {
      const audio = document.createElement("audio");
      audio.controls = true;
      audio.preload = "metadata";
      audio.src = normalized.src;
      audio.onerror = () => markMediaUnavailable(item, normalized.fallback);
      item.append(audio);
    } else {
      const link = document.createElement("a");
      link.className = "chatMediaResource";
      link.href = normalized.src;
      link.target = "_blank";
      link.rel = "noopener noreferrer";
      link.textContent = normalized.label || "打开媒体文件";
      item.append(link);
    }

    if (normalized.label && normalized.kind !== "resource") {
      const caption = document.createElement("figcaption");
      caption.textContent = normalized.label;
      item.append(caption);
    }
    wrap.append(item);
  }
  if (!wrap.children.length) wrap.remove();
}

function normalizeStructuredMedia(media, sessionID = "") {
  if (!media || typeof media !== "object") return null;
  const mimeType = safeMediaMime(media.mime_type || media.mimeType || "");
  const uri = String(media.uri || media.url || "").trim();
  const kind = inferMediaKind(media.kind || media.type || "", mimeType, uri);
  const rawData = typeof media.data === "string" ? media.data.replace(/\s+/g, "") : "";
  const localSrc = localSessionMediaURL(uri, sessionID);
  const referenceSrc = localSrc || safeMediaURL(uri);
  let src = "";
  if (rawData && /^[A-Za-z0-9+/_-]+={0,2}$/.test(rawData)) {
    const dataMime = mimeType || ({ image: "image/png", video: "video/mp4", audio: "audio/mpeg" })[kind];
    if (dataMime) src = `data:${dataMime};base64,${rawData}`;
  }
  if (!src) src = referenceSrc;
  if (!src) return null;
  const label = String(media.title || media.name || "").trim().slice(0, 160);
  const dataKey = rawData ? `${rawData.length}:${rawData.slice(0, 24)}:${rawData.slice(-24)}` : "";
  const referenceKey = localSrc ? `session:${sessionID}:${mediaReferenceIdentity(uri, kind)}` : uri;
  return { kind, mimeType, uri, src, fallback: referenceSrc || src, label, key: [kind, mimeType, referenceKey, dataKey].join("|") };
}

function localSessionMediaURL(value, sessionID) {
  value = String(value || "").trim();
  sessionID = String(sessionID || "").trim();
  if (!value || !sessionID || value.startsWith("/api/agent/media?")) return "";
  const windowsPath = /^[a-z]:[\\/]/i.test(value);
  let local = windowsPath || /^file:/i.test(value) || !/^[a-z][a-z0-9+.-]*:/i.test(value);
  if (!local) {
    try {
      const parsed = new URL(value);
      const host = parsed.hostname.toLowerCase();
      local = (parsed.protocol === "http:" || parsed.protocol === "https:") &&
        (host === "localhost" || host === "::1" || host.startsWith("127."));
    } catch {
      return "";
    }
  }
  if (!local) return "";
  return `/api/agent/media?session_id=${encodeURIComponent(sessionID)}&path=${encodeURIComponent(value)}`;
}

function safeMediaMime(value) {
  const mimeType = String(value || "").trim().toLowerCase();
  return /^[a-z0-9][a-z0-9.+-]*\/[a-z0-9][a-z0-9.+-]*$/.test(mimeType) ? mimeType : "";
}

function safeMediaURL(value) {
  if (!value) return "";
  try {
    const parsed = new URL(value, location.href);
    if (parsed.protocol === "data:") {
      return /^data:(?:image|video|audio)\/[a-z0-9.+-]+(?:;base64)?,/i.test(value) ? parsed.href : "";
    }
    return ["http:", "https:", "blob:", "file:"].includes(parsed.protocol) ? parsed.href : "";
  } catch {
    return "";
  }
}

function inferMediaKind(hint, mimeType, uri) {
  const value = `${hint} ${mimeType}`.toLowerCase();
  if (value.includes("image") || /生图|图片|图像/.test(value)) return "image";
  if (value.includes("video") || /视频/.test(value)) return "video";
  if (value.includes("audio") || /音频|语音/.test(value)) return "audio";
  const path = String(uri || "").split(/[?#]/, 1)[0].toLowerCase();
  if (/\.(?:png|jpe?g|gif|webp|avif|bmp)$/.test(path)) return "image";
  if (/\.(?:mp4|webm|mov|m4v|ogv)$/.test(path)) return "video";
  if (/\.(?:mp3|wav|m4a|ogg|flac)$/.test(path)) return "audio";
  return "resource";
}

function markMediaUnavailable(item, fallbackURI) {
  if (!item || item.dataset.failed === "true") return;
  item.dataset.failed = "true";
  const source = safeMediaURL(fallbackURI);
  item.replaceChildren();
  const label = document.createElement(source ? "a" : "span");
  label.className = "chatMediaUnavailable";
  label.textContent = source ? "媒体无法预览，点击打开" : "媒体无法预览";
  if (source) {
    label.href = source;
    label.target = "_blank";
    label.rel = "noopener noreferrer";
  }
  item.append(label);
}

function extractMediaFromPayload(payload, hint = "", depth = 0, seen = new Set()) {
  if (payload == null || depth > 5) return [];
  if (typeof payload === "string") {
    const value = payload.trim();
    if ((value.startsWith("{") && value.endsWith("}")) || (value.startsWith("[") && value.endsWith("]"))) {
      try {
        return extractMediaFromPayload(JSON.parse(value), hint, depth + 1, seen);
      } catch {
        // Treat malformed JSON as ordinary text below.
      }
    }
    const kind = inferMediaKind(hint, "", payload);
    return kind !== "resource" && isPlausibleMediaReference(value) ? [{ kind, uri: value }] : [];
  }
  if (typeof payload !== "object" || seen.has(payload)) return [];
  seen.add(payload);
  if (Array.isArray(payload)) return payload.flatMap((value) => extractMediaFromPayload(value, hint, depth + 1, seen));

  const mimeType = payload.mime_type || payload.mimeType || payload.content_type || payload.contentType || "";
  const kindHint = payload.kind || payload.type || payload.media_type || hint;
  const media = [];
  const encoded = payload.b64_json || payload.base64;
  if (typeof encoded === "string" && inferMediaKind(kindHint, mimeType, "") === "image") {
    media.push({ kind: "image", data: encoded, mime_type: mimeType || "image/png", name: payload.name || "" });
  }
  const references = [];
  for (const [key, value] of Object.entries(payload)) {
    if (["b64_json", "base64", "mime_type", "mimeType", "content_type", "contentType"].includes(key)) continue;
    const nextHint = /image/i.test(key) ? "image" : /video/i.test(key) ? "video" : /audio/i.test(key) ? "audio" : kindHint;
    if (typeof value === "string" && /(?:url|uri|src|href|path|filename|file)$/i.test(key) && isPlausibleMediaReference(value)) {
      const kind = inferMediaKind(nextHint, mimeType, value);
      if (kind !== "resource") references.push({ kind, uri: value, mime_type: mimeType, name: payload.name || payload.filename || "" });
      continue;
    }
    if (value && typeof value === "object") media.push(...extractMediaFromPayload(value, nextHint, depth + 1, seen));
  }
  const referenceKeys = new Set();
  for (const item of references) {
    const identity = mediaReferenceIdentity(item.uri, item.kind);
    if (referenceKeys.has(identity)) continue;
    referenceKeys.add(identity);
    media.push(item);
  }
  return media;
}

function isPlausibleMediaReference(value) {
  value = String(value || "").trim();
  if (!value || /[\r\n]/.test(value)) return false;
  const withoutQuery = value.split(/[?#]/, 1)[0];
  if (/\.(?:png|jpe?g|gif|webp|avif|bmp|mp4|webm|mov|m4v|ogv|mp3|wav|m4a|ogg|flac)$/i.test(withoutQuery)) return true;
  return /^(?:https?|file):\/\//i.test(value) && value.length < 4096;
}

function mediaReferenceIdentity(value, kind) {
  const clean = String(value || "").split(/[?#]/, 1)[0].replace(/\\/g, "/");
  const name = clean.slice(clean.lastIndexOf("/") + 1).toLowerCase();
  return `${kind}|${name || clean.toLowerCase()}`;
}

function buildOutboundAttachments() {
  return state.pendingAttachments.map((att) => {
    if (att.path) {
      return {
        kind: att.kind || "path",
        name: att.name || "",
        path: att.path,
        mime_type: att.mimeType || att.mime_type || "",
      };
    }
    // Legacy tiny inline fallback (should be rare after upload path).
    if (att.kind === "image") {
      return { kind: "image", data: att.data || "", mime_type: att.mimeType || "", name: att.name || "" };
    }
    return { kind: "text_file", name: att.name || "", text: att.text || "" };
  });
}

function updateChatComposerState() {
  // Refresh send button enablement to account for attachments-only messages.
  if (state.agentStatus?.state !== "ready" || state.agentEngineState === "loading") return;
  const hasText = !!$("chatInput")?.value.trim();
  const hasAttachments = !!state.pendingAttachments.length;
  if ($("chatSendBtn")) $("chatSendBtn").disabled = (!hasText && !hasAttachments) || state.agentStatus?.busy;
}

async function sendAgentMessage() {
  const text = $("chatInput").value.trim();
  const attachments = buildOutboundAttachments();
  if (!text && !attachments.length) return;
  if (state.agentStatus?.state === "busy" || state.agentStatus?.busy) {
    toast("正在生成回复，请先停止或等待完成", "error");
    return;
  }
  if (state.agentEngineState === "bootstrap") {
    // Keep local history; engine already has a fresh session with bootstrap armed.
    setAgentEngineState("attached");
  } else if (state.agentEngineState === "readonly") {
    if (!confirm("当前仅显示本地历史，原会话上下文没有恢复。发送这条消息将开启新对话，是否继续？")) return;
    const status = state.agentStatus;
    if (state.agentFallbackSessionReady && status?.state === "ready" && status?.session_id) {
      clearAgentTranscript();
      state.activeAgentSession = {
        id: status.session_id,
        title: "新对话",
        cwd: status.cwd || $("agentCwd").value.trim(),
        model: status.model || "",
      };
      setAgentEngineState("attached");
      updateConversationIdentity();
      renderAgentStatus(status);
      loadAgentSessions().catch(() => {});
    } else {
      const created = await newAgentSession();
      if (created === false) return;
    }
  }
  if (state.agentStatus?.state !== "ready") {
    toast("Agent 尚未就绪，请先启动", "error");
    return;
  }
  if (!agentSocket || agentSocket.readyState !== WebSocket.OPEN) {
    toast("对话连接尚未就绪", "error");
    connectAgentSocket();
    return;
  }
  state.lastUserMessage = text;
  appendChatMessage("user", text, "", true, state.pendingAttachments.slice());
  if (state.activeAgentSession && (!state.activeAgentSession.title || state.activeAgentSession.title === "新对话")) {
    state.activeAgentSession.title = (text || "附件消息").replace(/\s+/g, " ").slice(0, 60);
    updateConversationIdentity();
  }
  persistLastChatContext({
    sessionId: state.activeAgentSession?.id || state.agentStatus?.session_id,
    cwd: state.activeAgentSession?.cwd || $("agentCwd")?.value.trim(),
    title: state.activeAgentSession?.title,
    model: state.activeAgentSession?.model || state.agentStatus?.model,
  });
  agentActiveAssistant = null;
  agentActiveThought = null;
  agentRetryNotice = null;
  $("chatInput").value = "";
  clearChatAttachments();
  forceScrollChatToBottom();
  const _m = $("composerModelSelect")?.value || "";
  const _rawS = $("composerStrengthSelect")?.value || "";
  const _s = _rawS === "auto" ? "" : _rawS;
  agentSocket.send(JSON.stringify({ type: "user_message", text, attachments, model: _m, strength: _s }));
  renderAgentStatus({ ...state.agentStatus, state: "busy", running: true, busy: true });
}

function renderEmptyState() {
	const empty = false;
	$("emptyState").hidden = true;
	if ($("listControls")) $("listControls").hidden = false;
	$("profiles").hidden = false;
	if ($("searchEmpty")) $("searchEmpty").hidden = true;
	$("profileCount").textContent = `${state.profiles.length + 1} 个`;
}

function providerCards() {
	const settings = state.settings || {};
	const order = Array.isArray(settings.provider_order) ? settings.provider_order : [];
	const pinned = new Set(Array.isArray(settings.pinned_provider_ids) ? settings.pinned_provider_ids : []);
	const position = new Map(order.map((key, index) => [key, index]));
	const cards = [
		{
			key: OFFICIAL_PROVIDER_KEY,
			kind: "official",
			name: "官方账号",
			is_active: !!state.status?.official_active,
			logged_in: !!state.status?.official_logged_in,
		},
		...state.profiles.map((profile) => ({
			...profile,
			key: `profile:${profile.id}`,
			kind: "profile",
		})),
	];
	cards.forEach((card, index) => {
		card.pinned = pinned.has(card.key);
		card.position = position.has(card.key) ? position.get(card.key) : order.length + index;
	});
	cards.sort((a, b) => Number(b.pinned) - Number(a.pinned) || a.position - b.position);
	return cards;
}

function filteredProfiles() {
	const q = (state.search || "").trim().toLowerCase();
	const cards = providerCards();
	if (!q) return cards;
	return cards.filter((p) => (p.name || "").toLowerCase().includes(q));
}

function applyLayoutUI() {
  const layout = state.layout === "list" ? "list" : "card";
  state.layout = layout;
  localStorage.setItem("gs_layout", layout);
  if ($("profiles")) $("profiles").dataset.layout = layout;
  if ($("layoutCardBtn")) $("layoutCardBtn").classList.toggle("active", layout === "card");
  if ($("layoutListBtn")) $("layoutListBtn").classList.toggle("active", layout === "list");
}

function formatUpstream(value) {
  if (value === "openai_responses") return "Responses";
  if (value === "anthropic") return "Anthropic";
  return "OpenAI";
}

function hostOf(url) {
  try {
    return new URL(url).host || url;
  } catch {
    return url || "—";
  }
}

// 中转站地址：去掉 base_url 末尾的 /v1，即为可访问的中转站首页
function stationUrlOf(url) {
  if (!url) return "";
  const raw = String(url).trim();
  try {
    const u = new URL(raw);
    if (u.pathname.endsWith("/v1")) u.pathname = u.pathname.slice(0, -3);
    if (u.pathname === "/") u.pathname = "";
    return u.toString();
  } catch {
    return raw.replace(/\/v1\/?$/i, "").replace(/\/+$/, "");
  }
}

// 渲染卡片 / 列表上的供应商地址超链接（指向去掉 /v1 的中转站）
function providerLinkHtml(baseUrl) {
  if (!baseUrl) return "—";
  const station = stationUrlOf(baseUrl);
  if (!station) return escapeHtml(hostOf(baseUrl));
  return `<a class="providerLink" href="${escapeAttr(station)}" target="_blank" rel="noopener noreferrer">${escapeHtml(station)}</a>`;
}

function renderProfiles() {
  applyLayoutUI();
  $("profiles").innerHTML = "";
	const list = filteredProfiles();
	const emptyAll = false;
  if ($("searchEmpty")) {
    $("searchEmpty").hidden = emptyAll || list.length > 0;
  }
  if (emptyAll) return;

	list.forEach((profile) => {
		const el = document.createElement("article");
		el.className = `provider${profile.is_active ? " active" : ""}${profile.pinned ? " pinned" : ""}`;
		el.dataset.providerKey = profile.key;
		el.dataset.pinned = profile.pinned ? "1" : "0";
		const official = profile.kind === "official";
		const meta = official
			? `${profile.logged_in ? "已登录 grok.com" : "尚未登录"} · OAuth 官方模型`
			: `${escapeHtml(profile.default_model || "未设默认模型")} · ${formatUpstream(profile.upstream_format)} · ${profile.models?.length || 0} 模型`;
		el.innerHTML = `
			<div class="providerTop">
				<button type="button" class="dragHandle" draggable="true" data-action="drag" title="拖动排序" aria-label="拖动 ${escapeHtml(profile.name)} 排序">↕</button>
				<div class="providerInfo">
					<h3 class="providerName">${escapeHtml(profile.name)}</h3>
					<p class="providerUrl">${official ? "grok.com / auth.json" : providerLinkHtml(profile.base_url)}</p>
					<p class="providerMeta">${meta}</p>
				</div>
				<div class="providerFlags">
					${profile.pinned ? '<span class="pinBadge">已置顶</span>' : ""}
				</div>
			</div>
			<div class="providerActions">
				<button type="button" class="btn sm primary" data-action="enable">${profile.is_active ? "当前启用" : "启用"}</button>
				<button type="button" class="btn sm ghost" data-action="pin">${profile.pinned ? "取消置顶" : "置顶"}</button>
				${official ? "" : '<button type="button" class="btn sm" data-action="edit">编辑</button><button type="button" class="btn sm ghost" data-action="copy">复制</button><button type="button" class="btn sm ghost" data-action="export">导出</button><button type="button" class="btn sm danger" data-action="delete">删除</button>'}
			</div>
		`;

    const enableBtn = el.querySelector('[data-action="enable"]');
    if (profile.is_active) {
      enableBtn.disabled = true;
      enableBtn.classList.add("current");
		} else {
			enableBtn.onclick = () => official
				? activateOfficial(enableBtn)
				: activateProfile(profile.id, enableBtn, profile.name);
		}
		el.querySelector('[data-action="pin"]').onclick = () => toggleProviderPin(profile.key);
		bindProviderDrag(el, profile.key);

		if (!official) {
			el.querySelector('[data-action="edit"]').onclick = () => openEdit(profile);
			el.querySelector('[data-action="copy"]').onclick = () => {
				copyProfile(profile);
				showView("edit");
				$("name").focus();
			};
			el.querySelector('[data-action="export"]').onclick = () => exportProfile(profile);
			el.querySelector('[data-action="delete"]').onclick = () => run(async () => {
				if (!confirm(`删除「${profile.name}」？不可撤销。`)) return false;
				await api(`/api/profiles/${profile.id}`, { method: "DELETE" });
				await refreshAll();
				showView("home");
			}, { button: el.querySelector('[data-action="delete"]'), busyLabel: "删除中…", success: "已删除" });
		}

    $("profiles").appendChild(el);
  });
}

async function saveProviderLayout(order, pinned) {
	const next = {
		...(state.settings || {}),
		provider_order: order,
		pinned_provider_ids: pinned,
	};
	state.settings = await api("/api/settings", { method: "PUT", body: JSON.stringify(next) });
}

async function toggleProviderPin(key) {
	await run(async () => {
		const cards = providerCards();
		const pinned = new Set(state.settings?.pinned_provider_ids || []);
		if (pinned.has(key)) pinned.delete(key); else pinned.add(key);
		await saveProviderLayout(cards.map((card) => card.key), [...pinned]);
		renderProfiles();
  populateComposerModelSelect();
	}, { success: "卡片顺序已保存" });
}

function bindProviderDrag(card, key) {
	const handle = card.querySelector('[data-action="drag"]');
	handle.addEventListener("dragstart", (event) => {
		state.draggedProviderKey = key;
		card.classList.add("dragging");
		event.dataTransfer.effectAllowed = "move";
		event.dataTransfer.setData("text/plain", key);
	});
	handle.addEventListener("dragend", () => {
		state.draggedProviderKey = "";
		card.classList.remove("dragging");
		document.querySelectorAll(".provider.dragOver").forEach((item) => item.classList.remove("dragOver"));
	});
	card.addEventListener("dragover", (event) => {
		const source = document.querySelector(`[data-provider-key="${CSS.escape(state.draggedProviderKey)}"]`);
		if (!source || source === card || source.dataset.pinned !== card.dataset.pinned) return;
		event.preventDefault();
		card.classList.add("dragOver");
	});
	card.addEventListener("dragleave", () => card.classList.remove("dragOver"));
	card.addEventListener("drop", (event) => {
		event.preventDefault();
		card.classList.remove("dragOver");
		reorderProviderCards(state.draggedProviderKey, key);
	});
}

async function reorderProviderCards(sourceKey, targetKey) {
	if (!sourceKey || sourceKey === targetKey) return;
	await run(async () => {
		const cards = providerCards();
		const order = cards.map((card) => card.key);
		const sourceIndex = order.indexOf(sourceKey);
		const targetIndex = order.indexOf(targetKey);
		if (sourceIndex < 0 || targetIndex < 0) return false;
		order.splice(sourceIndex, 1);
		order.splice(targetIndex, 0, sourceKey);
		await saveProviderLayout(order, state.settings?.pinned_provider_ids || []);
		renderProfiles();
  populateComposerModelSelect();
	}, { success: "卡片顺序已保存" });
}

async function activateOfficial(button) {
	await run(async () => {
		const result = await api("/api/official/activate", { method: "POST" });
		await refreshAll();
		showView("home");
		if (result.login_required) toast("请在浏览器完成官方账号登录", "success");
	}, {
		button,
		busyLabel: "切换中…",
		success: "已切换到官方账号。新开 grok 会话生效。",
	});
}

async function activateProfile(id, button, name) {
  if (!id) return;
  await run(async () => {
    await api(`/api/profiles/${id}/activate`, { method: "POST" });
    await refreshAll();
    showView("home");
  }, {
    button,
    busyLabel: "启用中…",
    success: `已启用 ${name || "供应商"}。新开 grok 会话生效。`,
  });
}

function renderBackups(backups) {
  $("backups").innerHTML = "";
  const count = backups?.length || 0;
  if ($("backupCountLabel")) {
    $("backupCountLabel").textContent = count ? `${count} 个自动备份` : "暂无备份";
  }
  if (!count) {
    $("backups").innerHTML = `<p class="muted tiny">切换供应商时会自动创建。暂无历史备份。</p>`;
    return;
  }
  backups.forEach((backup) => {
    const el = document.createElement("div");
    el.className = "backup";
    el.innerHTML = `
      <strong>${escapeHtml(backup.file)}</strong>
      <p>${new Date(backup.created_at).toLocaleString()} · ${Math.round(backup.size / 1024)} KB</p>
      <button type="button" class="btn sm">还原</button>
    `;
    const btn = el.querySelector("button");
    btn.onclick = () => run(async () => {
      if (!confirm(`还原 ${backup.file}？当前配置会先自动备份。`)) return false;
      await api(`/api/backups/${encodeURIComponent(backup.file)}/restore`, { method: "POST" });
      await refreshAll();
    }, { button: btn, busyLabel: "还原中…", success: "已还原备份" });
    $("backups").appendChild(el);
  });
}

function renderSettings(settings) {
  $("autostart").checked = !!settings.autostart;
  $("silentAutostart").checked = !!settings.silent_autostart;
  $("autoOpenBrowser").checked = !!settings.auto_open_browser;
  $("port").value = settings.port;
  const actual = state.status?.port;
  const hint = $("portHint");
  if (actual && settings.port && actual !== settings.port) {
    hint.hidden = false;
    hint.textContent = `实际端口 ${actual}（配置 ${settings.port} 可能被占用）`;
  } else {
    hint.hidden = true;
  }
}

function renderGrokAuth(auth) {
  const configured = !!auth?.configured;
  const badge = $("grokAuthBadge");
  const connection = $("grokAuthConnection");
  const status = $("grokAuthStatus");
  if (badge) {
    badge.textContent = configured ? (auth.needs_refresh ? "需要刷新" : "已配置") : "未配置";
    badge.classList.toggle("active", configured && !auth.needs_refresh);
  }
  if (connection) connection.hidden = !configured;
  if ($("grokAuthBaseUrl")) $("grokAuthBaseUrl").value = auth?.base_url || "";
  if ($("grokAuthApiKey")) $("grokAuthApiKey").value = auth?.local_api_key || "";
  if ($("activateGrokAuthBtn")) $("activateGrokAuthBtn").hidden = !configured;
  if ($("refreshGrokAuthBtn")) $("refreshGrokAuthBtn").hidden = !auth?.single_configured || !!auth?.pool_accounts;
  if ($("deleteGrokAuthBtn")) $("deleteGrokAuthBtn").hidden = !auth?.single_configured || !!auth?.pool_accounts;
  if (!status) return;
  if (!configured) {
    status.textContent = "选择认证文件后，会生成稳定的本地 URL/key 和一个可直接启用的 Responses profile。";
    return;
  }
  const detail = [];
  if (auth.pool_accounts) detail.push(`统一号池 ${auth.pool_accounts} 个账号 · 已启用自动巡检`);
  if (auth.email) detail.push(auth.email);
  if (auth.expires_at) {
    detail.push(`${auth.needs_refresh ? "已过期或即将过期" : "有效期至"} ${new Date(auth.expires_at).toLocaleString()}`);
  }
  if (auth.source && auth.source !== "unified-pool") {
    detail.push(auth.source === "grok-auth-json" ? "Grok CLI auth.json" : "CPA xAI 凭据");
  }
  status.textContent = detail.join(" · ") || "凭据已配置";
}

const REGISTRAR_STATUS_LABELS = {
  starting: "启动中",
  running: "注册中",
  succeeded: "已完成",
  failed: "失败",
  cancelled: "已停止",
};

function registrarConfigFromForm() {
  const previous = state.registrar?.config || {};
  const provider = $("registrarEmailProvider").value;
  return {
    version: 1,
    browser_path: $("registrarBrowserPath").value.trim(),
    browser_mode: $("registrarBrowserMode").value,
    proxy_url: $("registrarProxyUrl").value.trim(),
    proxy_strategy: $("registrarProxyStrategy")?.value || "round_robin",
    proxy_cooldown_seconds: Number($("registrarProxyCooldown")?.value || 120),
    clash_controller: $("registrarClashController")?.value.trim() || "",
    clash_selector_group: $("registrarClashSelectorGroup")?.value.trim() || "",
    register_engine: $("registrarEngine")?.value || "browser",
    email_provider: provider,
    default_domains: (provider === "cloudflare"
      ? $("registrarCloudflareDomains").value
      : provider === "yyds"
        ? ($("registrarYydsDomains")?.value || "").trim()
        : $("registrarDefaultDomains").value).trim(),
    cloudmail_url: $("registrarCloudmailUrl").value.trim(),
    cloudmail_admin_email: $("registrarCloudmailAdminEmail").value.trim(),
    cloudmail_password: $("registrarCloudmailPassword").value,
    cloudflare_api_base: $("registrarCloudflareApiBase").value.trim(),
    cloudflare_api_key: $("registrarCloudflareApiKey").value.trim(),
    cloudflare_auth_mode: $("registrarCloudflareAuthMode").value,
    cloudflare_path_domains: $("registrarCloudflareDomainsPath").value.trim(),
    cloudflare_path_accounts: $("registrarCloudflareAccountsPath").value.trim(),
    cloudflare_path_token: $("registrarCloudflareTokenPath").value.trim(),
    cloudflare_path_messages: $("registrarCloudflareMessagesPath").value.trim(),
    hotmail_accounts_text: $("registrarHotmailAccounts").value,
    hotmail_max_aliases: Number($("registrarHotmailAliases").value || 5),
    gptmail_url: $("registrarGptmailUrl")?.value.trim() || "",
    gptmail_api_key: $("registrarGptmailApiKey")?.value.trim() || "",
    yyds_url: $("registrarYydsUrl")?.value.trim() || "",
    yyds_api_key: $("registrarYydsApiKey")?.value.trim() || "",
    count: Number($("registrarCount").value || 1),
    workers: Number($("registrarWorkers").value || 1),
    mail_timeout_seconds: Number($("registrarMailTimeout").value || 180),
    page_timeout_seconds: previous.page_timeout_seconds || 300,
    prefer_protocol_mint: $("registrarPreferProtocol").checked,
    protocol_only: $("registrarProtocolOnly").checked,
  };
}

function renderRegistrar(stateData) {
  const config = stateData?.config || {};
  if (!registrarFormDirty) {
    if ($("registrarBrowserPath")) $("registrarBrowserPath").value = config.browser_path || "";
    if ($("registrarBrowserMode")) $("registrarBrowserMode").value = config.browser_mode || "visible";
    if ($("registrarProxyUrl")) $("registrarProxyUrl").value = config.proxy_url || "";
    if ($("registrarProxyStrategy")) $("registrarProxyStrategy").value = config.proxy_strategy || "round_robin";
    if ($("registrarProxyCooldown")) $("registrarProxyCooldown").value = config.proxy_cooldown_seconds || 120;
    if ($("registrarClashController")) $("registrarClashController").value = config.clash_controller || "";
    if ($("registrarClashSelectorGroup")) $("registrarClashSelectorGroup").value = config.clash_selector_group || "";
    if ($("registrarEngine")) $("registrarEngine").value = config.register_engine || "browser";
    if ($("registrarEmailProvider")) $("registrarEmailProvider").value = config.email_provider || "cloudflare";
    if ($("registrarDefaultDomains")) $("registrarDefaultDomains").value = config.default_domains || "";
    if ($("registrarCloudmailUrl")) $("registrarCloudmailUrl").value = config.cloudmail_url || "";
    if ($("registrarCloudmailAdminEmail")) $("registrarCloudmailAdminEmail").value = config.cloudmail_admin_email || "";
    if ($("registrarCloudmailPassword")) $("registrarCloudmailPassword").value = config.cloudmail_password || "";
    if ($("registrarCloudflareApiBase")) $("registrarCloudflareApiBase").value = config.cloudflare_api_base || "";
    if ($("registrarCloudflareApiKey")) $("registrarCloudflareApiKey").value = config.cloudflare_api_key || "";
    if ($("registrarCloudflareAuthMode")) $("registrarCloudflareAuthMode").value = config.cloudflare_auth_mode || "none";
    if ($("registrarCloudflareDomains")) $("registrarCloudflareDomains").value = config.default_domains || "";
    if ($("registrarCloudflareDomainsPath")) $("registrarCloudflareDomainsPath").value = config.cloudflare_path_domains || "/api/domains";
    if ($("registrarCloudflareAccountsPath")) $("registrarCloudflareAccountsPath").value = config.cloudflare_path_accounts || "/api/new_address";
    if ($("registrarCloudflareTokenPath")) $("registrarCloudflareTokenPath").value = config.cloudflare_path_token || "/api/token";
    if ($("registrarCloudflareMessagesPath")) $("registrarCloudflareMessagesPath").value = config.cloudflare_path_messages || "/api/mails";
    if ($("registrarHotmailAccounts")) $("registrarHotmailAccounts").value = config.hotmail_accounts_text || "";
    if ($("registrarHotmailAliases")) $("registrarHotmailAliases").value = config.hotmail_max_aliases || 5;
    if ($("registrarGptmailUrl")) $("registrarGptmailUrl").value = config.gptmail_url || "";
    if ($("registrarGptmailApiKey")) $("registrarGptmailApiKey").value = config.gptmail_api_key || "";
    if ($("registrarYydsUrl")) $("registrarYydsUrl").value = config.yyds_url || "";
    if ($("registrarYydsApiKey")) $("registrarYydsApiKey").value = config.yyds_api_key || "";
    if ($("registrarYydsDomains")) $("registrarYydsDomains").value = config.default_domains || "";
    if ($("registrarCount")) $("registrarCount").value = config.count || 1;
    if ($("registrarWorkers")) $("registrarWorkers").value = config.workers || 1;
    if ($("registrarMailTimeout")) $("registrarMailTimeout").value = config.mail_timeout_seconds || 180;
    if ($("registrarPreferProtocol")) $("registrarPreferProtocol").checked = config.prefer_protocol_mint !== false;
    if ($("registrarProtocolOnly")) $("registrarProtocolOnly").checked = !!config.protocol_only;
  }
  updateRegistrarProviderFields();
  renderRegistrarJob(stateData?.job || null);
  if ($("registrarPaths")) {
    const paths = [];
    if (stateData?.auth_dir) paths.push(`CPA 目录：${stateData.auth_dir}`);
    if (stateData?.accounts_path) paths.push(`账本：${stateData.accounts_path}`);
    if (stateData?.cookie_dir) paths.push(`Cookie：${stateData.cookie_dir}`);
    $("registrarPaths").textContent = paths.join(" · ");
  }
}

function updateRegistrarProviderFields() {
  const provider = $("registrarEmailProvider")?.value || "cloudflare";
  const isCloudflare = provider === "cloudflare";
  if ($("registrarCloudflareEssentials")) $("registrarCloudflareEssentials").hidden = !isCloudflare;
  if ($("registrarAltProviderHint")) $("registrarAltProviderHint").hidden = isCloudflare;
  if ($("registrarHotmailFields")) $("registrarHotmailFields").hidden = provider !== "hotmail";
  if ($("registrarCloudmailFields")) $("registrarCloudmailFields").hidden = provider !== "cloudmail";
  if ($("registrarCloudflareFields")) $("registrarCloudflareFields").hidden = !isCloudflare;
  if ($("registrarGptmailFields")) $("registrarGptmailFields").hidden = provider !== "gptmail";
  if ($("registrarYydsFields")) $("registrarYydsFields").hidden = provider !== "yyds";
  // Non-default email modes need advanced fields; expand so users notice.
  const advanced = $("registrarAdvanced");
  if (advanced && !isCloudflare && !advanced.open) {
    advanced.open = true;
  }
}

function escapeRegistrarHtml(value) {
  return String(value ?? "")
    .replace(/&/g, "&amp;")
    .replace(/</g, "&lt;")
    .replace(/>/g, "&gt;")
    .replace(/"/g, "&quot;");
}

/** Parse the latest [CF] log line into a short status strip for the UI. */
function extractRegistrarChallengeStatus(lines) {
  if (!Array.isArray(lines) || !lines.length) return "";
  for (let i = lines.length - 1; i >= 0; i -= 1) {
    const line = String(lines[i] || "");
    const idx = line.indexOf("[CF]");
    if (idx < 0) continue;
    return line.slice(idx).trim();
  }
  // Fall back to stage markers when CF not yet hit.
  for (let i = lines.length - 1; i >= 0; i -= 1) {
    const line = String(lines[i] || "");
    if (line.includes("[Cloudflare 人机验证]") || line.includes("[资料页 Turnstile]") || line.includes("[提交邮箱]")) {
      const m = line.match(/\[([^\]]+)\]\s*(.*)$/);
      if (m) return `${m[1]}：${m[2]}`.trim();
    }
  }
  return "";
}

function renderRegistrarResults(job) {
  const el = $("registrarResults");
  if (!el) return;
  const results = job?.results || [];
  if (!results.length) {
    el.hidden = true;
    el.innerHTML = "";
    return;
  }
  el.hidden = false;
  el.innerHTML = results.map((r) => {
    const ok = r.status === "success";
    const email = escapeRegistrarHtml(r.email || "（未分配邮箱）");
    const status = ok ? "成功" : "失败";
    const mint = r.mint_method ? `<span class="registrarResultMint">铸造 ${escapeRegistrarHtml(r.mint_method)}</span>` : "";
    const cookie = r.cookie_file ? `<div class="registrarResultCookie">Cookie：<span class="mono">${escapeRegistrarHtml(r.cookie_file)}</span></div>` : "";
    const err = r.error
      ? `<div class="registrarResultError">${escapeRegistrarHtml(r.error)}</div>`
      : "";
    return `<div class="registrarResult ${ok ? "ok" : "fail"}">
      <div class="registrarResultHead">
        <span class="registrarResultEmail mono">${email}</span>
        <span class="registrarResultStatus">${status}</span>
        ${mint}
      </div>
      ${cookie}
      ${err}
    </div>`;
  }).join("");
}

function renderRegistrarJob(job) {
  clearTimeout(registrarPollTimer);
  const active = job && (job.status === "starting" || job.status === "running");
  const badge = $("registrarBadge");
  if (badge) {
    badge.textContent = job ? (REGISTRAR_STATUS_LABELS[job.status] || job.status) : "空闲";
    badge.classList.toggle("active", !!active);
  }
  if ($("startRegistrarBtn")) $("startRegistrarBtn").disabled = !!active;
  if ($("stopRegistrarBtn")) $("stopRegistrarBtn").hidden = !active;
  const progress = $("registrarProgress");
  if (progress) progress.hidden = !job;
  if (job) {
    const total = job.requested || 0;
    const completed = job.completed || 0;
    if ($("registrarProgressCount")) $("registrarProgressCount").textContent = `${completed} / ${total}`;
    if ($("registrarProgressDetail")) {
      const detail = [`成功 ${job.succeeded || 0}`, `失败 ${job.failed || 0}`];
      if (job.imported || job.updated) detail.push(`导入 ${job.imported || 0}，更新 ${job.updated || 0}`);
      if (job.error) detail.push(job.error);
      $("registrarProgressDetail").textContent = detail.join(" · ");
      $("registrarProgressDetail").title = job.error || detail.join(" · ");
    }
    if ($("registrarProgressBar")) {
      $("registrarProgressBar").max = Math.max(total, 1);
      $("registrarProgressBar").value = Math.min(completed, total);
    }
    const lines = job.log_tail || [];
    if ($("registrarLog")) {
      $("registrarLog").textContent = lines.length ? lines.join("\n") : "等待日志…";
      $("registrarLog").scrollTop = $("registrarLog").scrollHeight;
    }
    const challenge = $("registrarChallengeStatus");
    if (challenge) {
      const statusLine = extractRegistrarChallengeStatus(lines);
      if (statusLine) {
        challenge.hidden = false;
        challenge.textContent = statusLine;
        const failed = /未通过|硬拦截|超时|失败|blocked|stuck|token_missing|no_widget/i.test(statusLine);
        const passed = /通过|可操作|token_already|clearance_cookie|page_ready/i.test(statusLine) && !failed;
        challenge.classList.toggle("is-fail", failed);
        challenge.classList.toggle("is-ok", passed && !failed);
      } else {
        challenge.hidden = true;
        challenge.textContent = "";
        challenge.classList.remove("is-fail", "is-ok");
      }
    }
    renderRegistrarResults(job);
    if (!active && job.id && registrarTerminalNotice !== job.id) {
      registrarTerminalNotice = job.id;
      if (job.status === "succeeded") {
        const partial = job.failed > 0
          ? `注册部分完成：成功 ${job.succeeded || 0}，失败 ${job.failed || 0}`
          : "注册任务已完成，账号已进入号池";
        toast(partial, job.failed > 0 ? "warn" : "success");
      }
      if (job.status === "failed") {
        const msg = job.error || "注册任务失败";
        toast(msg.length > 180 ? `${msg.slice(0, 180)}…` : msg, "error");
      }
      if (job.status === "cancelled") toast(job.error || "注册任务已停止", "warn");
    }
  } else {
    renderRegistrarResults(null);
    if ($("registrarChallengeStatus")) {
      $("registrarChallengeStatus").hidden = true;
      $("registrarChallengeStatus").textContent = "";
    }
  }
  if (active && job.id) {
    registrarPollTimer = setTimeout(() => loadRegistrarJob().catch(() => {}), 1000);
  }
}

async function loadRegistrarJob() {
  const response = await api("/api/registrar/job");
  if (state.registrar) state.registrar.job = response.job;
  renderRegistrarJob(response.job);
  return response.job;
}

const GROK_POOL_CLASS_LABELS = {
  healthy: "健康",
  permission_denied: "权限被拒",
  quota_exhausted: "额度用尽",
  reauth: "需重新登录",
  model_unavailable: "模型不可用",
  probe_error: "探测异常",
  unknown: "未知",
  uninspected: "待巡检",
};

function renderGrokPool(pool) {
  clearTimeout(grokPoolPollTimer);
  const configured = !!pool?.configured;
  const summary = pool?.summary || {};
  if ($("grokPoolBadge")) {
    $("grokPoolBadge").textContent = `${summary.total || 0} 个账号 · ${summary.available || 0} 可用`;
  }
  if ($("grokPoolSummary")) {
    $("grokPoolSummary").innerHTML = [
      [summary.total || 0, "总账号"],
      [summary.available || 0, "代理可用"],
      [summary.healthy || 0, "健康"],
      [summary.abnormal || 0, "异常"],
    ].map(([value, label]) => `<div class="poolStat"><strong>${value}</strong><span>${label}</span></div>`).join("");
  }
  const settings = pool?.settings || {};
  if ($("grokPoolAutoEnabled")) $("grokPoolAutoEnabled").checked = !!settings.enabled;
  if ($("grokPoolInterval")) $("grokPoolInterval").value = settings.interval_minutes || 360;
  if ($("grokPoolWorkers")) $("grokPoolWorkers").value = settings.workers || 4;
  if ($("grokPoolProxyUrl")) $("grokPoolProxyUrl").value = settings.proxy_url || "";
  if ($("grokPoolAuthDir")) $("grokPoolAuthDir").value = settings.auth_dir || "";
  if ($("grokPoolWatchEnabled")) $("grokPoolWatchEnabled").checked = !!settings.watch_enabled;
  if ($("grokPoolWatchRecursive")) $("grokPoolWatchRecursive").checked = settings.watch_recursive !== false;
  if ($("grokPoolConnection")) $("grokPoolConnection").hidden = !configured;
  if ($("grokPoolBaseUrl")) $("grokPoolBaseUrl").value = configured && state.status?.port
    ? `http://127.0.0.1:${state.status.port}/grok/v1`
    : "";
  if ($("grokPoolApiKey")) $("grokPoolApiKey").value = pool?.local_api_key || "";
  if ($("activateGrokPoolBtn")) $("activateGrokPoolBtn").hidden = !configured;
  if ($("stopGrokPoolBtn")) $("stopGrokPoolBtn").hidden = !pool?.running;
  if ($("inspectGrokPoolBtn")) {
    $("inspectGrokPoolBtn").disabled = !configured || !!pool?.running;
  }
  if ($("batchRefreshGrokPoolBtn")) {
    $("batchRefreshGrokPoolBtn").disabled = !configured || !!pool?.running || batchRefreshRunning;
  }
  const abnormalAccounts = (pool?.accounts || []).filter((account) => {
    const classification = account.classification || "uninspected";
    return classification !== "healthy" && classification !== "uninspected";
  });
  if ($("batchDisableGrokPoolBtn")) {
    $("batchDisableGrokPoolBtn").disabled = !abnormalAccounts.some((account) => !account.disabled) || !!pool?.running;
  }
  if ($("batchDeleteGrokPoolBtn")) {
    $("batchDeleteGrokPoolBtn").disabled = !abnormalAccounts.length || !!pool?.running;
  }
  if ($("grokPoolProgress")) {
    const parts = [];
    if (pool?.running) parts.push(`巡检中 ${pool.done || 0}/${pool.total || 0}`);
    else if (pool?.last_run) parts.push(`上次巡检 ${new Date(pool.last_run).toLocaleString()}`);
    else parts.push(configured ? "等待首次巡检" : "尚未导入号池账号");
    if (!pool?.running && pool?.next_run) parts.push(`下次 ${new Date(pool.next_run).toLocaleString()}`);
    if (pool?.last_error) parts.push(pool.last_error);
    $("grokPoolProgress").textContent = parts.join(" · ");
  }
  if ($("grokPoolAuthDirStatus")) {
    const watchParts = [];
    if (pool?.resolved_auth_dir) watchParts.push(pool.resolved_auth_dir);
    if (settings.watch_enabled) watchParts.push(`热加载 ${pool.watch_file_count || 0} 个文件`);
    if (pool?.watch_last_import) watchParts.push(`最近导入 ${new Date(pool.watch_last_import).toLocaleString()}`);
    if (pool?.watch_last_error) watchParts.push(pool.watch_last_error);
    $("grokPoolAuthDirStatus").textContent = watchParts.join(" · ") || "认证目录尚未扫描";
  }
  // Refresh the visible account list page (paginated) without re-rendering
  // thousands of rows. Stats in the header come from the summary above.
  if ($("viewAccounts") && !$("viewAccounts").hidden) {
    refreshAccountsListView({ silent: true }).catch(() => {});
  }
  if (pool?.running) {
    grokPoolPollTimer = setTimeout(() => loadGrokPool().catch((err) => toast(err.message, "error")), 1500);
  }
}

function renderCpaMint(session, { notify = false } = {}) {
  clearTimeout(cpaMintPollTimer);
  cpaMintSession = session || null;
  const active = session && (session.status === "pending" || session.status === "polling");
  const start = $("startCpaMintBtn");
  const cancel = $("cancelCpaMintBtn");
  const open = $("openCpaMintUrlBtn");
  if (start) start.disabled = !!active;
  if (cancel) cancel.hidden = !active;
  if (open) open.hidden = !session?.verification_uri_complete;
  const codes = $("cpaMintCodes");
  if (codes) codes.hidden = !session?.verification_uri_complete;
  if ($("cpaMintUserCode")) $("cpaMintUserCode").textContent = session?.user_code || "";
  const verify = $("cpaMintVerifyLink");
  if (verify) {
    verify.textContent = session?.verification_uri_complete || "";
    verify.href = session?.verification_uri_complete || "#";
  }
  const status = $("cpaMintStatus");
  if (status) {
    if (!session) status.textContent = "尚未开始铸造。";
    else {
      const labels = {
        pending: "正在准备设备授权…",
        polling: "等待浏览器完成登录与授权…",
        success: `铸造成功：${session.path || "认证文件已写入"}`,
        failed: `铸造失败：${session.error || "未知错误"}`,
        cancelled: "铸造已取消。",
      };
      status.textContent = labels[session.status] || session.status || "状态未知";
      if (session.error && active) status.textContent += `（${session.error}）`;
    }
  }
  if (notify && session?.status === "success" && cpaMintTerminalNotice !== session.id) {
    cpaMintTerminalNotice = session.id;
    toast("CPA 凭据已写入并导入号池", "success");
    refreshAll().catch((err) => toast(err.message, "error"));
  }
  if (notify && session && (session.status === "failed" || session.status === "cancelled") && cpaMintTerminalNotice !== session.id) {
    cpaMintTerminalNotice = session.id;
    if (session.status === "failed") toast(session.error || "CPA 铸造失败", "error");
  }
  if (active) {
    cpaMintPollTimer = setTimeout(() => pollCpaMint(session.id), 1500);
  }
}

async function pollCpaMint(id) {
  try {
    const response = await api(`/api/cpa-mint?id=${encodeURIComponent(id)}`);
    renderCpaMint(response.session, { notify: true });
  } catch (err) {
    if (cpaMintSession?.id === id) {
      cpaMintPollTimer = setTimeout(() => pollCpaMint(id), 2500);
    }
  }
}

async function loadLatestCpaMint() {
  const response = await api("/api/cpa-mint");
  renderCpaMint(response.session);
}

function renderAccountFilterChips(summary) {
  const container = $("accountFilterChips");
  if (!container) return;
  const counts = {
    "": summary.total || 0,
    healthy: summary.healthy || 0,
    quota_exhausted: summary.quota_exhausted || 0,
    permission_denied: summary.permission_denied || 0,
    reauth: summary.reauth || 0,
    abnormal: summary.abnormal || 0,
    uninspected: summary.uninspected || 0,
    disabled: summary.disabled || 0,
  };
  container.innerHTML = ACCOUNT_FILTERS.map((filter) => {
    const active = accountListFilter === filter.key;
    return `<button type="button" class="accountChip ${active ? "active" : ""}" data-filter="${escapeAttr(filter.key)}">${escapeHtml(filter.label)} <span class="accountChipCount">${counts[filter.key] ?? 0}</span></button>`;
  }).join("");
  container.querySelectorAll(".accountChip").forEach((chip) => {
    chip.onclick = () => {
      accountListFilter = chip.dataset.filter;
      accountListPage = 1;
      loadAccountsList().catch((err) => toast(err.message, "error"));
    };
  });
}

// Entering the accounts view: prime the toolbar and load the first page.
async function loadAccountsView() {
  if ($("accountSort")) $("accountSort").value = accountListSort;
  if ($("accountSearch")) $("accountSearch").value = accountListQuery;
  await loadGrokPool();
  await loadAccountsList();
}

// Silent refresh used by the inspection poller: re-fetch only the visible page
// and update stats without resetting scroll/selection.
async function refreshAccountsListView({ silent = false } = {}) {
  await loadAccountsList({ silent });
}

async function loadAccountsList({ silent = false, append = false } = {}) {
  const container = $("grokPoolAccounts");
  if (!container || accountListInFlight) return;
  accountListInFlight = true;
  try {
    const params = new URLSearchParams({
      page: String(accountListPage),
      page_size: String(ACCOUNT_LIST_PAGE_SIZE),
      sort: accountListSort,
    });
    if (accountListQuery) params.set("q", accountListQuery);
    if (accountListFilter) params.set("classification", accountListFilter);
    const data = await api(`/api/grok-pool/accounts?${params.toString()}`);
    accountListTotal = data.total || 0;
    if (data.summary) renderAccountFilterChips(data.summary);
    renderAccountList(data.accounts || [], { append });
    renderAccountPager();
    const meta = $("accountListMeta");
    if (meta) {
      const loadedUpTo = Math.min(accountListPage * ACCOUNT_LIST_PAGE_SIZE, accountListTotal);
      meta.textContent = `共 ${accountListTotal} 个账号 · 已加载 ${loadedUpTo}`;
    }
  } catch (err) {
    if (!silent) toast(err.message, "error");
  } finally {
    accountListInFlight = false;
  }
}

function renderAccountList(accounts, { append = false } = {}) {
  const container = $("grokPoolAccounts");
  if (!container) return;
  if (!append) container.innerHTML = "";
  if (!accounts.length && !append) {
    container.innerHTML = `<p class="muted tiny">没有匹配的账号。可一次选择多个 CPA xai-*.json；也支持 Grok CLI auth.json。</p>`;
    return;
  }
  // Group accounts by classification (empty filter keeps the visual grouping).
  const groups = new Map();
  accounts.forEach((account) => {
    const classification = account.disabled ? "disabled" : (account.classification || "uninspected");
    if (!groups.has(classification)) groups.set(classification, []);
    groups.get(classification).push(account);
  });
  const order = ["healthy", "quota_exhausted", "permission_denied", "reauth", "abnormal", "uninspected", "disabled", "unknown", "model_unavailable", "probe_error"];
  const sortedGroups = Array.from(groups.entries()).sort((a, b) => {
    const ai = order.indexOf(a[0]);
    const bi = order.indexOf(b[0]);
    return (ai === -1 ? 99 : ai) - (bi === -1 ? 99 : bi);
  });
  sortedGroups.forEach(([classification, groupAccounts]) => {
    const label = classification === "disabled" ? "已禁用" : (GROK_POOL_CLASS_LABELS[classification] || classification);
    let group = container.querySelector(`details.accountGroup[data-group="${cssEscape(classification)}"]`);
    if (group) {
      const body = group.querySelector(".accountGroupBody");
      groupAccounts.forEach((account) => body.appendChild(buildAccountRow(account)));
      const countEl = group.querySelector(".accountGroupCount");
      if (countEl) countEl.textContent = String(body.children.length);
      return;
    }
    group = document.createElement("details");
    group.className = `accountGroup ${escapeAttr(classification)}`;
    group.dataset.group = classification;
    group.open = true;
    const summary = document.createElement("summary");
    summary.className = "accountGroupHead";
    summary.innerHTML = `<span class="accountGroupLabel">${escapeHtml(label)}</span><span class="accountGroupCount">${groupAccounts.length}</span>`;
    group.appendChild(summary);
    const body = document.createElement("div");
    body.className = "accountGroupBody";
    groupAccounts.forEach((account) => body.appendChild(buildAccountRow(account)));
    group.appendChild(body);
    container.appendChild(group);
  });
}

function cssEscape(value) {
  if (window.CSS?.escape) return window.CSS.escape(value);
  return String(value).replace(/[^a-zA-Z0-9_-]/g, (ch) => `\\${ch}`);
}

function buildAccountRow(account) {
  const classification = account.classification || "uninspected";
  const row = document.createElement("div");
  row.className = `accountRow ${escapeAttr(classification)}${account.disabled ? " is-disabled" : ""}`;
  const title = account.email || account.file_name || account.id;
  const inspected = account.last_inspected ? new Date(account.last_inspected).toLocaleString() : "未巡检";
  const expires = account.expires_at
    ? new Date(account.expires_at).toLocaleString()
    : "—";
  const statusText = account.disabled ? "手动禁用" : (GROK_POOL_CLASS_LABELS[classification] || classification);
  const errorBits = [];
  if (account.http_status) errorBits.push(`HTTP ${account.http_status}`);
  if (account.error_code) errorBits.push(account.error_code);
  row.innerHTML = `
    <div class="accountRowMain">
      <strong class="accountRowTitle">${escapeHtml(title)}</strong>
      <span class="accountRowSub muted tiny">${escapeHtml(account.model || "未选模型")} · 巡检 ${escapeHtml(inspected)} · Token ${escapeHtml(expires)}</span>
      ${errorBits.length || account.reason ? `<span class="accountRowReason">${escapeHtml([account.reason || "", ...errorBits].filter(Boolean).join(" · "))}</span>` : ""}
    </div>
    <span class="badge accountRowBadge ${escapeAttr(classification)}">${escapeHtml(statusText)}</span>
    <div class="accountRowActions">
      <button type="button" class="btn sm" data-action="toggle">${account.disabled ? "启用" : "禁用"}</button>
      <button type="button" class="btn sm" data-action="refresh" title="重新登录该账号并更新 Cookie / 铸造 CPA 凭据">刷新</button>
      <button type="button" class="btn sm danger" data-action="delete">删除</button>
    </div>
  `;
  const refreshCookie = row.querySelector('[data-action="refresh"]');
  refreshCookie.title = account.email
    ? `重新登录 ${account.email} 并更新 Cookie / 铸造 CPA 凭据`
    : "重新登录该账号并更新 Cookie / 铸造 CPA 凭据";
  refreshCookie.onclick = () => startAccountRefresh(account.id, refreshCookie);
  const toggle = row.querySelector('[data-action="toggle"]');
  toggle.onclick = () => run(async () => {
    await api(`/api/grok-pool/accounts/${encodeURIComponent(account.id)}`, {
      method: "PATCH",
      body: JSON.stringify({ disabled: !account.disabled }),
    });
    await Promise.all([loadGrokPool(), loadAccountsList({ silent: true })]);
  }, { button: toggle, busyLabel: "处理中…", success: account.disabled ? "账号已启用" : "账号已禁用" });
  const remove = row.querySelector('[data-action="delete"]');
  remove.onclick = () => run(async () => {
    if (!confirm(`删除号池账号 ${title}？此操作会删除 grok_switch 保存的凭据副本。`)) return false;
    await api(`/api/grok-pool/accounts/${encodeURIComponent(account.id)}`, { method: "DELETE" });
    await Promise.all([loadGrokPool(), loadAccountsList({ silent: true })]);
  }, { button: remove, busyLabel: "删除中…", success: "号池账号已删除" });
  return row;
}

const sleep = (ms) => new Promise((resolve) => setTimeout(resolve, ms));

async function pollReloginJob(jobId, timeoutMs) {
  const start = Date.now();
  const cap = timeoutMs || 10 * 60 * 1000;
  while (Date.now() - start < cap) {
    const job = await api(`/api/grok-pool/refresh-cookie/${encodeURIComponent(jobId)}`);
    if (!job.running) return job;
    await sleep(2000);
  }
  throw new Error("刷新任务超时");
}

async function startAccountRefresh(accountId, button) {
  const label = button?.textContent || "刷新";
  if (button) {
    button.disabled = true;
    button.textContent = "刷新中…";
  }
  try {
    const resp = await api("/api/grok-pool/refresh-cookie", {
      method: "POST",
      body: JSON.stringify({ ids: [accountId] }),
    });
    const job = await pollReloginJob(resp.id);
    const entry = job.entries?.[0];
    if (entry && entry.status === "success") {
      toast(`Cookie 已刷新：${entry.email}`, "success");
    } else {
      toast(`刷新失败：${entry?.email || ""} ${entry?.error || "未知原因"}`.trim(), "error");
    }
    await Promise.all([loadGrokPool(), loadAccountsList({ silent: true })]);
  } catch (err) {
    toast(err.message || "刷新失败", "error");
  } finally {
    if (button) {
      button.disabled = false;
      button.textContent = label;
    }
  }
}

let batchRefreshRunning = false;

async function batchRefreshAccounts(button) {
  if (batchRefreshRunning) {
    toast("批量刷新已在运行", "info");
    return;
  }
  const total = (state.grokPool?.accounts || []).length;
  if (!total) {
    toast("号池中还没有账号", "error");
    return;
  }
  if (!confirm(`确定批量刷新 ${total} 个号池账号？\n将逐个处理：先用现有 refresh_token 直接续期（秒级、不弹浏览器）；续期失败（吊销/过期）才回退浏览器重新登录铸造（每个约 40~90 秒）。\n没有在 registrar 账本（accounts_cli.txt）中保存密码的账号会被跳过。`)) return;
  batchRefreshRunning = true;
  if (button) {
    button.disabled = true;
    button.textContent = `批量刷新中…（${total}）`;
  }
  try {
    const resp = await api("/api/grok-pool/refresh-cookie", {
      method: "POST",
      body: JSON.stringify({ ids: [] }),
    });
    const meta = $("accountListMeta");
    const progressTimer = setInterval(async () => {
      try {
        const job = await api(`/api/grok-pool/refresh-cookie/${encodeURIComponent(resp.id)}`);
        if (meta) meta.textContent = `批量刷新 ${job.done}/${job.total} …`;
      } catch (_) { /* 轮询失败忽略 */ }
    }, 3000);
    const job = await pollReloginJob(resp.id, 120 * 60 * 1000);
    clearInterval(progressTimer);
    const entries = job.entries || [];
    const ok = entries.filter((e) => e.status === "success").length;
    const failed = entries.filter((e) => e.status !== "success").length;
    toast(`批量刷新完成：成功 ${ok}，失败/跳过 ${failed}`, ok > 0 ? "success" : "error");
    if (meta) meta.textContent = `共 ${accountListTotal} 个账号 · 批量刷新完成（成功 ${ok} / 失败 ${failed}）`;
    await Promise.all([loadGrokPool(), loadAccountsList({ silent: true })]);
  } catch (err) {
    toast(err.message || "批量刷新失败", "error");
  } finally {
    batchRefreshRunning = false;
    if (button) {
      button.disabled = false;
      button.textContent = "批量刷新";
    }
    await loadAccountsList({ silent: true }).catch(() => {});
  }
}

function renderAccountPager() {
  const loadMore = $("accountLoadMoreBtn");
  const info = $("accountPagerInfo");
  const loadedUpTo = accountListPage * ACCOUNT_LIST_PAGE_SIZE;
  const hasMore = loadedUpTo < accountListTotal;
  if (loadMore) loadMore.hidden = !hasMore;
  if (info) info.textContent = hasMore ? `已加载 ${Math.min(loadedUpTo, accountListTotal)} / ${accountListTotal}` : "";
}

async function loadGrokPool() {
  state.grokPool = await api("/api/grok-pool");
  renderGrokPool(state.grokPool);
  return state.grokPool;
}

async function copyText(value, successMessage) {
  if (!value) throw new Error("没有可复制的内容");
  if (navigator.clipboard?.writeText) {
    await navigator.clipboard.writeText(value);
  } else {
    const area = document.createElement("textarea");
    area.value = value;
    area.style.position = "fixed";
    area.style.opacity = "0";
    document.body.appendChild(area);
    area.select();
    const copied = document.execCommand("copy");
    area.remove();
    if (!copied) throw new Error("复制失败");
  }
  toast(successMessage, "success");
}

function openEdit(profile) {
  fillForm(profile || newProfileDraft());
  // Keep advanced sections collapsed by default for a simple add flow.
  if ($("connectBlock")) $("connectBlock").open = false;
  if ($("configPreviewBlock")) $("configPreviewBlock").open = false;
  showView("edit");
  $("name").focus();
}

function fillForm(profile) {
  $("formTitle").textContent = profile.id ? "编辑供应商" : "添加供应商";
  $("formHint").textContent = profile.id ? "修改后可保存，或保存并启用" : "名称、类型与 API Key 即可开始；模型可稍后设置";
  $("profileId").value = profile.id || "";
  $("name").value = profile.name || "";
  $("baseUrl").value = profile.base_url || "";
  $("profileApiKey").value = profile.api_key || firstModelKey(profile) || "";
  $("upstreamFormat").value = upstreamFormatValue(profile.upstream_format);
  $("templateSelect").value = templateValue(profile);
  // 生图能力是全局开关（settings），与单个供应商无关。
  const imageGenEnabled = !!(state.settings && state.settings.image_gen_enabled);
  if ($("imageGenEnabled")) $("imageGenEnabled").checked = imageGenEnabled;
  syncImageGenUI();
  refreshImageGenAccountStatus();
  state.availableModels = unique([
    ...(profile.available_models || []),
    ...(profile.models || []).map((model) => model.name || model.model),
  ]);
  $("modelsBody").innerHTML = "";
  (profile.models || []).forEach((model) => addModelCard(model));
  renderModelSelect();
  // Rebuild selects from enabled models, then restore saved values.
  const sa = subagentsModelsOf(profile);
  syncEnabledModelList({
    default_model: profile.default_model || "",
    web_search_model: profile.web_search_model || "",
    subagents_models: sa,
  });
  hideConnectionStatus();
  hideImageGenStatus();
  if ($("connectBlock")) $("connectBlock").open = false;
}

function applyTemplate(key) {
  const tpl = TEMPLATES[key];
  if (!tpl) return;
  const keepName = $("name").value.trim();
  const keepKey = $("profileApiKey").value.trim();
  // Template only fills connection skeleton — no default models, no enabled models.
  fillForm({
    id: $("profileId").value,
    name: keepName || tpl.name,
    template: key,
    upstream_format: tpl.upstream_format,
    base_url: tpl.base_url,
    api_key: keepKey || tpl.api_key || "",
    default_model: "",
    web_search_model: "",
    subagents_models: { explore: "", plan: "" },
    models: [],
    available_models: [],
    image_generation: { enabled: false, api_backend: "chat_completions", available_models: [] },
  });
  $("templateSelect").value = key;
  toast(`已套用「${tpl.name}」地址与协议，请自行启用模型`, "info");
}

function templateValue(profile) {
  if (TEMPLATE_KEYS.has(profile.template)) return profile.template;
  // Older profiles did not persist their selected template. Recover the
  // closest template from the protocol while defaulting new profiles to Responses.
  if (!profile.id && !profile.name && !profile.base_url) return "responses";
  if (profile.upstream_format === "openai_responses" || profile.upstream_format === "responses") return "responses";
  if (profile.upstream_format === "anthropic" || profile.upstream_format === "messages") return "anthropic";
  return "openai";
}

function copyProfile(profile) {
  const source = profile.id ? profile : profile;
  const clone = {
    ...source,
    id: "",
    name: `${source.name || "供应商"} 副本`,
    is_active: false,
    image_generation: { ...imageGenerationOf(source), available_models: [...imageGenerationOf(source).available_models] },
    models: (source.models || []).map((m) => ({ ...m, extra_headers: { ...(m.extra_headers || {}) } })),
  };
  fillForm(clone);
  toast("已载入副本，保存后生效", "info");
}

function stripSecrets(profile, includeKey) {
  const image = imageGenerationOf(profile);
  const out = {
    name: profile.name,
    template: profile.template || templateValue(profile),
    upstream_format: profile.upstream_format,
    base_url: profile.base_url,
    default_model: profile.default_model,
    default_reasoning_effort: profile.default_reasoning_effort || "high",
    web_search_model: profile.web_search_model,
    subagents_models: subagentsModelsOf(profile),
    image_generation: {
      enabled: image.enabled,
      base_url: image.base_url,
      api_backend: image.api_backend,
      model: image.model,
      available_models: image.available_models,
      ...(includeKey ? { api_key: image.api_key } : {}),
    },
    available_models: profile.available_models || [],
    models: (profile.models || []).map((m) => {
      const item = {
        name: m.name,
        display_name: m.display_name || "",
        model: m.model,
        base_url: m.base_url || "",
        api_backend: m.api_backend,
        extra_headers: m.extra_headers || {},
        supports_backend_search: !!m.supports_backend_search,
        supports_reasoning_effort: true,
        reasoning_efforts: m.reasoning_efforts?.length ? m.reasoning_efforts : ["low", "medium", "high"],
        context_window: m.context_window || 0,
        max_completion_tokens: m.max_completion_tokens || 0,
      };
      if (includeKey) item.api_key = m.api_key || profile.api_key || "";
      return item;
    }),
  };
  if (includeKey) out.api_key = profile.api_key || "";
  return out;
}

function exportProfile(profile) {
  const includeKey = confirm("导出是否包含 API Key？\n\n取消 = 仅结构（适合分享）\n确定 = 含密钥（仅私用）");
  const payload = {
    format: "grok_switch_profile",
    version: 1,
    exported_at: new Date().toISOString(),
    profile: stripSecrets(profile, includeKey),
  };
  const blob = new Blob([JSON.stringify(payload, null, 2)], { type: "application/json" });
  const a = document.createElement("a");
  const safe = (profile.name || "profile").replace(/[\\/:*?"<>|]+/g, "_");
  a.href = URL.createObjectURL(blob);
  a.download = `${safe}.json`;
  a.click();
  URL.revokeObjectURL(a.href);
  toast(includeKey ? "已导出（含密钥）" : "已导出（不含密钥）", "success");
}

function importProfileJSON(text) {
  let data;
  try {
    data = JSON.parse(text);
  } catch {
    throw new Error("JSON 解析失败");
  }
  const profile = data.profile || data;
  if (!profile || typeof profile !== "object") throw new Error("无效的供应商 JSON");
  fillForm({
    id: "",
    name: profile.name ? `${profile.name} 导入` : "Imported",
    upstream_format: profile.upstream_format || "openai_chat",
    base_url: profile.base_url || "",
    api_key: profile.api_key || "",
    default_model: profile.default_model || "",
    web_search_model: profile.web_search_model || "",
    subagents_models: subagentsModelsOf(profile),
    image_generation: imageGenerationOf(profile),
    available_models: profile.available_models || [],
    models: profile.models || [],
  });
  showView("edit");
  toast("已载入 JSON，确认后点保存", "success");
}

function upstreamFormatValue(value) {
  if (value === "openai_responses" || value === "anthropic") return value;
  return "openai_chat";
}

function apiBackendFor(upstream) {
  if (upstream === "openai_responses") return "responses";
  if (upstream === "anthropic") return "messages";
  return "chat_completions";
}

function serializeHeaders(headers) {
  if (!headers) return "";
  return Object.entries(headers).map(([k, v]) => `${k}: ${v}`).join("\n");
}

function parseHeaders(text) {
  const out = {};
  (text || "").split(/\r?\n/).forEach((line) => {
    const trimmed = line.trim();
    if (!trimmed) return;
    const idx = trimmed.indexOf(":");
    if (idx <= 0) return;
    const key = trimmed.slice(0, idx).trim();
    const val = trimmed.slice(idx + 1).trim();
    if (key) out[key] = val;
  });
  return out;
}

function firstModelKey(profile) {
  return (profile.models || []).find((model) => model.api_key)?.api_key || "";
}

function isMediaModelID(value) {
  const id = String(value || "").trim().toLowerCase();
  return id === "grok-imagine-image" || id === "grok-imagine-image-quality" || id === "grok-imagine-video";
}

function syncImageGenUI() {
  // 生图能力是全局开关；切换后由 setImageGenEnabled 持久化到 settings。
  const enabled = !!$("imageGenEnabled")?.checked;
  if ($("imageGenFields")) $("imageGenFields").hidden = !enabled;
}

async function setImageGenEnabled(enabled) {
  const next = { ...(state.settings || {}) };
  next.image_gen_enabled = !!enabled;
  state.settings = await api("/api/settings", { method: "PUT", body: JSON.stringify(next) });
  toast(enabled ? "已启用生图（账号池 MCP 工具）" : "已关闭生图能力，模型回到无生图状态", "success");
}

function refreshImageGenAccountStatus() {
  const el = $("imageGenAccountStatus");
  if (!el) return;
  const count = Number((state.status && state.status.imagine_accounts) || 0);
  el.textContent = count > 0 ? `账号池：${count} 个可用账号` : "账号池：无可用账号，生图不可用";
}

function renderImageModelOptions() {
  const list = $("imageGenModelOptions");
  if (!list) return;
  list.innerHTML = "";
  unique(state.imageAvailableModels || []).forEach((model) => {
    const option = document.createElement("option");
    option.value = model;
    list.appendChild(option);
  });
}

function showImageGenStatus(ok, text) {
  const status = $("imageGenStatus");
  if (!status) return;
  status.hidden = false;
  status.textContent = text;
  status.classList.toggle("ok", ok);
  status.classList.toggle("fail", !ok);
}

function hideImageGenStatus() {
  const status = $("imageGenStatus");
  if (!status) return;
  status.hidden = true;
  status.textContent = "";
  status.classList.remove("ok", "fail");
}

function removeModelByName(modelName) {
  [...$("modelsBody").querySelectorAll(".modelCard")].forEach((card) => {
    const name = card.querySelector('[data-field="name"]')?.value.trim();
    const model = card.querySelector('[data-field="model"]')?.value.trim();
    if (name === modelName || model === modelName) card.remove();
  });
  syncEnabledModelList();
}

function renderModelSelect() {
  const query = $("modelSearchInput")?.value.trim().toLowerCase() || "";
  const enabled = new Set(readEnabledModelNames());
  const models = state.availableModels
    .filter((model) => !query || model.toLowerCase().includes(query))
    .slice(0, 24);
  $("modelSuggestions").innerHTML = "";
  if (!state.availableModels.length) {
    $("modelPoolStatus").textContent = "尚未拉取模型。点 chip 仅启用，不会自动设置默认模型。";
    $("modelSuggestions").innerHTML = `<button type="button" class="chip mutedChip">先拉取模型</button>`;
    return;
  }
  $("modelPoolStatus").textContent = `已缓存 ${state.availableModels.length} 个模型。点 chip 启用/取消；默认模型请手动填写。`;
  models.forEach((model) => {
    const chip = document.createElement("button");
    chip.type = "button";
    const isOn = enabled.has(model);
    chip.className = isOn ? "chip selected" : "chip";
    chip.textContent = isOn ? `${model} ✓` : model;
    chip.onclick = () => {
      if (isOn) removeModelByName(model);
      else {
        addModelCard({
          name: model,
          model,
          api_backend: apiBackendFor($("upstreamFormat").value),
          context_window: 0,
          max_completion_tokens: 0,
        });
      }
      renderModelSelect();
      syncEnabledModelList();
    };
    $("modelSuggestions").appendChild(chip);
  });
  if (!models.length) {
    $("modelSuggestions").innerHTML = `<button type="button" class="chip mutedChip">没有匹配</button>`;
  }
}

function syncEnabledModelList(preferred) {
  const isReservedForMedia = (name) => isMediaModelID(name);
  const names = unique(readEnabledModelNames().filter((name) => !isMediaModelID(name)));
  const fields = [
    { id: "defaultModel", emptyLabel: "（请先启用模型）", required: false },
    { id: "webSearchModel", emptyLabel: "（可选）", required: false },
    { id: "subagentsExploreModel", emptyLabel: "（继承主模型）", required: false },
    { id: "subagentsPlanModel", emptyLabel: "（继承主模型）", required: false },
  ];
  const currentSA = preferred?.subagents_models || {
    explore: $("subagentsExploreModel")?.value || "",
    plan: $("subagentsPlanModel")?.value || "",
  };
  const prefer = preferred || {
    default_model: $("defaultModel")?.value || "",
    web_search_model: $("webSearchModel")?.value || "",
    subagents_models: currentSA,
  };
  const sa = prefer.subagents_models || currentSA;
  const values = {
    defaultModel: prefer.default_model ?? "",
    webSearchModel: prefer.web_search_model ?? "",
    subagentsExploreModel: sa.explore ?? "",
    subagentsPlanModel: sa.plan ?? "",
  };

  fields.forEach(({ id, emptyLabel }) => {
    const sel = $(id);
    if (!sel) return;
    const current = values[id] || "";
    sel.innerHTML = "";
    const empty = document.createElement("option");
    empty.value = "";
    empty.textContent = names.length ? emptyLabel.replace("请先启用模型", "未选择") : emptyLabel;
    sel.appendChild(empty);
    names.forEach((name) => {
      const opt = document.createElement("option");
      opt.value = name;
      opt.textContent = name;
      sel.appendChild(opt);
    });
    // Keep saved value even if not currently in enabled list (e.g. mid-edit).
    if (current && isReservedForMedia(current)) {
      sel.value = "";
    } else if (current && !names.includes(current)) {
      const orphan = document.createElement("option");
      orphan.value = current;
      orphan.textContent = `${current}（未启用）`;
      sel.appendChild(orphan);
      sel.value = current;
    } else if (current && names.includes(current)) {
      sel.value = current;
    } else {
      sel.value = "";
    }
  });
}

function syncAdvancedUI() {
  $("modelsBody").dataset.advanced = state.showAdvanced ? "1" : "0";
  $("toggleAdvancedBtn").textContent = state.showAdvanced ? "收起高级" : "高级字段";
}

function renderLANAccess(access) {
  const remote = !!access?.remote;
  if ($("lanAccessCard")) $("lanAccessCard").hidden = remote;
  if ($("lanAccessEnabled")) $("lanAccessEnabled").disabled = remote;
  if (remote) return;
  const enabled = !!state.settings?.lan_access_enabled && !!access?.enabled;
  const badge = $("lanAccessBadge");
  const empty = $("lanAccessDisabled");
  const details = $("lanAccessDetails");
  if (badge) {
    badge.textContent = enabled ? "已开启" : "未开启";
    badge.classList.toggle("active", enabled);
  }
  if (empty) empty.hidden = enabled;
  if (details) details.hidden = !enabled;
  if (!enabled) return;

  const addresses = access?.addresses || [];
  const select = $("lanAccessAddress");
  if (!select) return;
  const current = select.value;
  select.innerHTML = addresses.length
    ? addresses.map((item, index) => `<option value="${index}">${escapeHtml(item.address)}</option>`).join("")
    : `<option value="">未找到局域网地址</option>`;
  if (addresses.length) {
    const selected = Number.isInteger(Number(current)) && Number(current) < addresses.length ? Number(current) : 0;
    select.value = String(selected);
    renderLANAddress(addresses[selected], access);
  } else {
    renderLANAddress(null, access);
  }
}

function renderLANAddress(address, access) {
  const qr = $("lanAccessQr");
  const url = $("lanAccessUrl");
  const code = $("lanAccessCode");
  const expiry = $("lanAccessExpiry");
  if (qr) {
    qr.hidden = !address?.qr_code;
    if (address?.qr_code) qr.src = address.qr_code;
    else qr.removeAttribute("src");
  }
  if (url) url.value = address?.pair_url || "";
  if (code) code.textContent = access?.pairing_code || "—";
  if (expiry) {
    expiry.textContent = access?.pairing_expiry
      ? `有效至 ${new Date(access.pairing_expiry).toLocaleTimeString()}`
      : "";
  }
}

function syncModelBaseURLs() {
  const baseURL = $("baseUrl")?.value.trim() || "";
  $("modelsBody")?.querySelectorAll('[data-field="base_url"]').forEach((input) => {
    input.value = baseURL;
  });
}

function addModelCard(model = {}) {
  const backend = model.api_backend || apiBackendFor($("upstreamFormat").value);
  const modelBaseURL = model.base_url || $("baseUrl")?.value.trim() || "";
  const card = document.createElement("div");
  card.className = "modelCard";
  card.innerHTML = `
    <div class="modelCardTop">
      <strong><span data-role="model-title">${escapeHtml(model.display_name || model.name || model.model || "新模型")}</span></strong>
      <div class="inlineActions">
        <button type="button" class="btn sm" data-action="test-model">测试连通</button>
        <button type="button" class="btn sm danger" data-action="remove-model">删除</button>
      </div>
    </div>
    <p class="muted tiny modelProbeStatus" data-field="probe_status" hidden></p>
    <div class="modelCardGrid">
      <label class="field">配置键
        <input data-field="name" class="mono" value="${escapeAttr(model.name || "")}" placeholder="例如 grok-chat">
      </label>
      <label class="field">Model
        <input data-field="model" class="mono" value="${escapeAttr(model.model || "")}" placeholder="上游模型 ID">
      </label>
      <label class="field full">显示名称
        <input data-field="display_name" value="${escapeAttr(model.display_name || "")}" placeholder="可选，例如 Grok Imagine Image">
      </label>
      <label class="field advancedOnly">Base URL
        <input data-field="base_url" class="mono" value="${escapeAttr(modelBaseURL)}" placeholder="与供应商服务地址保持一致">
      </label>
      <label class="field advancedOnly">API Backend
        <select data-field="api_backend" class="mono">
          <option value="chat_completions">chat_completions</option>
          <option value="responses">responses</option>
          <option value="messages">messages</option>
        </select>
      </label>
      <label class="field advancedOnly">Context Window
        <input data-field="context_window" type="number" min="0" step="1" value="${model.context_window > 0 ? model.context_window : ""}" placeholder="空为默认" title="留空：config 中不写入，Grok 使用默认（新模型约 20 万）">
      </label>
      <label class="field advancedOnly">Max Tokens
        <input data-field="max_completion_tokens" type="number" min="0" step="1" value="${model.max_completion_tokens > 0 ? model.max_completion_tokens : ""}" placeholder="空为默认" title="留空：config 中不写入；可在 [models] 设全局 max_completion_tokens">
      </label>
      <label class="check advancedOnly"><input type="checkbox" data-field="supports_backend_search"> 支持后端搜索</label>
      <label class="field full advancedOnly">Extra Headers
        <textarea data-field="extra_headers" rows="2" placeholder="Key: Value"></textarea>
      </label>
    </div>
  `;
  card.querySelector('[data-field="api_backend"]').value = backend;
  const backendSelect = card.querySelector('[data-field="api_backend"]');
  backendSelect.addEventListener("change", () => {
    backendSelect.dataset.touched = "1";
  });
  card.querySelector('[data-field="supports_backend_search"]').checked = model.supports_backend_search ?? true;
  card.querySelector('[data-field="extra_headers"]').value = serializeHeaders(model.extra_headers);
  const nameInput = card.querySelector('[data-field="name"]');
  const modelInput = card.querySelector('[data-field="model"]');
  const displayNameInput = card.querySelector('[data-field="display_name"]');
  const onFieldChange = () => {
    card.querySelector('[data-role="model-title"]').textContent = displayNameInput.value.trim() || nameInput.value.trim() || modelInput.value.trim() || "新模型";
    renderModelSelect();
    syncEnabledModelList();
  };
  nameInput.addEventListener("input", onFieldChange);
  modelInput.addEventListener("input", onFieldChange);
  displayNameInput.addEventListener("input", onFieldChange);
  card.querySelector('[data-action="remove-model"]').onclick = () => {
    card.remove();
    renderModelSelect();
    syncEnabledModelList();
    scheduleProviderPreview();
  };
  card.querySelector('[data-action="test-model"]').onclick = () => testSingleModel(card);
  $("modelsBody").appendChild(card);
  scheduleProviderPreview();
}

async function testSingleModel(card) {
  const btn = card.querySelector('[data-action="test-model"]');
  const statusEl = card.querySelector('[data-field="probe_status"]');
  const modelName = card.querySelector('[data-field="model"]')?.value.trim()
    || card.querySelector('[data-field="name"]')?.value.trim();
  const modelBase = card.querySelector('[data-field="base_url"]')?.value.trim();
  const backend = card.querySelector('[data-field="api_backend"]')?.value;
  await run(async () => {
    const current = readForm();
    if (!current.base_url && !modelBase) throw new Error("先填写服务地址");
    if (!current.api_key) throw new Error("先填写 API Key");
    if (!modelName) throw new Error("模型名为空");
    if (statusEl) {
      statusEl.hidden = false;
      statusEl.textContent = "测试中…";
      statusEl.classList.remove("ok", "fail");
    }
    const result = await api("/api/connection/test", {
      method: "POST",
      body: JSON.stringify({
        profile_id: current.id,
        base_url: modelBase || current.base_url,
        api_key: current.api_key,
        upstream_format: current.upstream_format,
        api_backend: backend || apiBackendFor(current.upstream_format),
        model: modelName,
      }),
    });
    if (!result.ok) {
      if (statusEl) {
        statusEl.textContent = `失败 ${result.latency_ms}ms：${result.error || "未知错误"}`;
        statusEl.classList.add("fail");
      }
      throw new Error(result.error || `${modelName} 不通`);
    }
    if (statusEl) {
      statusEl.textContent = `连通 ${result.latency_ms}ms`;
      statusEl.classList.add("ok");
    }
    toast(`${modelName} 连通（${result.latency_ms}ms）`, "success");
  }, { button: btn, busyLabel: "测试中…" });
}

function syncModelBackends() {
  const backend = apiBackendFor($("upstreamFormat").value);
  $("modelsBody").querySelectorAll('[data-field="api_backend"]').forEach((sel) => {
    if (!sel.dataset.touched) sel.value = backend;
  });
}

function readEnabledModelNames() {
  return [...$("modelsBody").querySelectorAll(".modelCard")].map((row) => {
    const name = row.querySelector('[data-field="name"]')?.value.trim();
    const model = row.querySelector('[data-field="model"]')?.value.trim();
    return name || model;
  }).filter(Boolean);
}

function readForm() {
  const rows = [...$("modelsBody").querySelectorAll(".modelCard")];
  const apiKey = $("profileApiKey").value.trim();
  return {
    id: $("profileId").value,
    name: $("name").value.trim(),
    template: $("templateSelect").value || "responses",
    upstream_format: $("upstreamFormat").value,
    base_url: $("baseUrl").value.trim(),
    api_key: apiKey,
    available_models: state.availableModels,
    default_model: $("defaultModel")?.value?.trim() || "",
    default_reasoning_effort: "high",
    web_search_model: $("webSearchModel")?.value?.trim() || "",
    subagents_models: {
      explore: $("subagentsExploreModel")?.value?.trim() || "",
      plan: $("subagentsPlanModel")?.value?.trim() || "",
    },
    models: rows.map((row) => {
      const get = (field) => row.querySelector(`[data-field="${field}"]`)?.value.trim() || "";
      const num = (field) => Number(get(field) || 0);
      return {
        name: get("name"),
        display_name: get("display_name"),
        model: get("model"),
        base_url: get("base_url"),
        api_key: apiKey,
        api_backend: row.querySelector('[data-field="api_backend"]')?.value || apiBackendFor($("upstreamFormat").value),
        extra_headers: parseHeaders(row.querySelector('[data-field="extra_headers"]')?.value || ""),
        supports_backend_search: !!row.querySelector('[data-field="supports_backend_search"]')?.checked,
        supports_reasoning_effort: true,
        reasoning_efforts: ["low", "medium", "high"],
        context_window: num("context_window"),
        max_completion_tokens: num("max_completion_tokens"),
      };
    }).filter((m) => m.name || m.model),
  };
}

function escapeHtml(value) {
  return String(value ?? "").replace(/[&<>"']/g, (ch) => ({
    "&": "&amp;", "<": "&lt;", ">": "&gt;", '"': "&quot;", "'": "&#39;",
  }[ch]));
}

function escapeAttr(value) {
  return escapeHtml(value);
}

function unique(values) {
  return [...new Set(values.filter(Boolean))];
}

function hideConnectionStatus() {
  const el = $("connectionStatus");
  el.hidden = true;
  el.textContent = "";
  el.classList.remove("ok", "fail");
}

function showConnectionStatus(ok, text) {
  const el = $("connectionStatus");
  el.hidden = false;
  el.textContent = text;
  el.classList.toggle("ok", ok);
  el.classList.toggle("fail", !ok);
}

async function importCurrentConfig(button) {
  const name = prompt("供应商名称", "Imported");
  if (!name) return;
  await run(async () => {
    await api("/api/import", { method: "POST", body: JSON.stringify({ name, active: false }) });
    await refreshAll();
    const imported = state.profiles.find((p) => p.name === name) || state.profiles[state.profiles.length - 1];
    if (imported) openEdit(imported);
  }, { button, busyLabel: "导入中…", success: "已从 config.toml 导入" });
}

async function saveCurrentProfile() {
  const profile = readForm();
  if (!profile.name) throw new Error("请填写名称");
  if (!profile.base_url) throw new Error("请填写服务地址");
  const chatRoles = [
    profile.default_model,
    profile.web_search_model,
    profile.subagents_models?.explore,
    profile.subagents_models?.plan,
  ];
  if (chatRoles.some((model) => isMediaModelID(model))) {
    throw new Error("内置生图别名不能作为默认、搜索或子代理模型");
  }
  if (profile.id) {
    return await api(`/api/profiles/${profile.id}`, { method: "PUT", body: JSON.stringify(profile) });
  }
  return await api("/api/profiles", { method: "POST", body: JSON.stringify(profile) });
}

// Navigation
$("navHomeBtn").onclick = () => showView("home");
$("navSkillsBtn").onclick = () => showView("skills");
$("navAccountsBtn").onclick = () => showView("accounts");
$("navImagineBtn").onclick = () => showView("imagine");
$("navSettingsBtn").onclick = () => showView("settings");
$("imagineGenerateBtn").onclick = () => generateImagine();
$("imagineClearBtn").onclick = () => clearImagineGallery();
$("backFromEditBtn").onclick = () => showView("home");
$("backFromSkillsBtn").onclick = () => showView("home");
$("backFromSettingsBtn").onclick = () => showView("home");
$("backFromAccountsBtn").onclick = () => showView("home");
$("goAccountsFromSettingsBtn").onclick = () => showView("accounts");
$("accountLoadMoreBtn").onclick = () => {
  accountListPage += 1;
  loadAccountsList({ append: true }).catch((err) => toast(err.message, "error"));
};
$("accountSort").onchange = () => {
  accountListSort = $("accountSort").value;
  accountListPage = 1;
  loadAccountsList().catch((err) => toast(err.message, "error"));
};
$("accountSearch").oninput = () => {
  clearTimeout(accountSearchTimer);
  accountSearchTimer = setTimeout(() => {
    accountListQuery = $("accountSearch").value.trim();
    accountListPage = 1;
    loadAccountsList().catch((err) => toast(err.message, "error"));
  }, 300);
};
$("chatBtn").onclick = () => showView("chat");
$("addBtn").onclick = () => openEdit(newProfileDraft());
$("emptyNewBtn").onclick = () => openEdit(newProfileDraft());
$("emptyImportBtn").onclick = () => importCurrentConfig($("emptyImportBtn"));
$("importHeaderBtn").onclick = () => importCurrentConfig($("importHeaderBtn"));
$("reapplyBtn").onclick = () => {
  const id = state.status?.active_profile?.id;
  const name = state.status?.active_profile?.name;
  activateProfile(id, $("reapplyBtn"), name);
};
$("openConfigFromDriftBtn").onclick = () => showView("settings");

$("agentStartBtn").onclick = () => run(startAgent, { button: $("agentStartBtn"), busyLabel: "连接中…" });
$("agentNewSessionBtn").onclick = () => run(async () => {
  // Prefer active project workspace; otherwise current cwd; else guide user to add a project.
  const active = (state.projects || []).find((p) => p.id === state.activeProjectId);
  if (active) return newAgentSession(active);
  const cwd = $("agentCwd")?.value.trim();
  if (!cwd) {
    toast("请先添加或选择一个项目，会话会创建在该工作空间下", "info");
    await addProjectFromPrompt();
    const next = (state.projects || []).find((p) => p.id === state.activeProjectId);
    if (next) return newAgentSession(next);
    return false;
  }
  return newAgentSession();
}, { button: $("agentNewSessionBtn"), busyLabel: "创建中…" });
$("agentStopBtn").onclick = () => run(stopAgent, { button: $("agentStopBtn"), busyLabel: "停止中…" });
$("agentSessionSearch").oninput = () => {
  clearTimeout(agentSessionSearchTimer);
  agentSessionSearchTimer = setTimeout(() => loadAgentSessions().catch((err) => toast(err.message, "error")), 180);
};
$("toggleOrphanSessionsBtn")?.addEventListener("click", () => {
  state.orphanSessionsOpen = !state.orphanSessionsOpen;
  renderAgentSessionList();
});
$("openSessionSidebarBtn").onclick = () => toggleSessionSidebar();
$("closeSessionSidebarBtn").onclick = () => toggleSessionSidebar(false);
$("openContextRailBtn").onclick = () => toggleContextRail();
$("closeContextRailBtn").onclick = () => toggleContextRail(false);
$("nativeChatScrim").onclick = closeNativeChatPanels;
$("navHomeFromChatBtn")?.addEventListener("click", () => showView("home"));
$("sidebarSearchFocusBtn")?.addEventListener("click", () => {
  const input = $("agentSessionSearch");
  if (!input) return;
  input.focus();
  input.select?.();
});
$("openLocationBtn")?.addEventListener("click", () => openWorkingDirectoryInExplorer().catch((err) => toast(err.message, "error")));
$("pickCwdTopBtn")?.addEventListener("click", () => pickWorkingDirectory().catch((err) => toast(err.message, "error")));
$("composerProjectBtn")?.addEventListener("click", () => pickWorkingDirectory().catch((err) => toast(err.message, "error")));
$("composerAccessSelect")?.addEventListener("change", () => applyComposerAccessSelect());
bindChatPanelResizer("left");
bindChatPanelResizer("right");
// agentCwd is a hidden state field; updates go through pickWorkingDirectory / openProjectById.
$("permissionAllowBtn").onclick = () => respondAgentPermission(true, false);
$("permissionSessionBtn") && ($("permissionSessionBtn").onclick = () => respondAgentPermission(true, true));
$("permissionRejectBtn").onclick = () => respondAgentPermission(false, false);
$("planApproveBtn") && ($("planApproveBtn").onclick = () => respondAgentPlan("approved"));
$("planReviseBtn") && ($("planReviseBtn").onclick = () => respondAgentPlan("cancelled"));
$("planDismissBtn") && ($("planDismissBtn").onclick = () => respondAgentPlan("abandoned"));
$("agentSessionAutoApprove")?.addEventListener("change", () => {
  const enabled = !!$("agentSessionAutoApprove").checked;
  if (agentSocket && agentSocket.readyState === WebSocket.OPEN) {
    agentSocket.send(JSON.stringify({ type: "set_session_auto_approve", allow: enabled, remember: enabled }));
  }
  renderAgentStatus({ ...state.agentStatus, session_auto_approve: enabled });
  syncComposerAccessSelect();
});
$("agentAlwaysApprove")?.addEventListener("change", () => syncComposerAccessSelect());
$("addProjectBtn")?.addEventListener("click", () => addProjectFromPrompt().catch((err) => toast(err.message, "error")));
$("chatWorkspaceFileBtn")?.addEventListener("click", () => openWorkspaceFilePicker().catch((err) => toast(err.message, "error")));
$("workspaceFileCloseBtn")?.addEventListener("click", () => hideWorkspaceFilePicker());
$("workspaceFileSearch")?.addEventListener("input", () => filterWorkspaceFileList());
$("permissionRemember")?.addEventListener("change", syncPermissionAllowLabel);
$("chatComposer").onsubmit = (event) => {
  event.preventDefault();
  sendAgentMessage().catch((err) => toast(err.message || String(err), "error"));
};
$("chatStopBtn")?.addEventListener("click", () => {
  cancelAgentGeneration().catch((err) => toast(err.message || String(err), "error"));
});
$("chatAttachBtn")?.addEventListener("click", () => $("chatAttachFile")?.click());
$("chatAttachFile")?.addEventListener("change", (event) => {
  const input = event.target;
  handleChatFiles(input.files).finally(() => { input.value = ""; });
});
$("chatInput").addEventListener("paste", (event) => {
  const items = event.clipboardData?.items;
  if (!items) return;
  const imageItems = Array.from(items).filter((item) => item.type.startsWith("image/"));
  if (!imageItems.length) return;
  event.preventDefault();
  const files = imageItems.map((item) => item.getAsFile()).filter(Boolean);
  handleChatFiles(files);
});
$("agentReadonlyNewBtn").onclick = () => run(async () => {
  try { await api("/api/agent/session/bootstrap", { method: "POST", body: JSON.stringify({ clear: true }) }); } catch {}
  state.agentNeedsBootstrap = false;
  return newAgentSession();
}, { button: $("agentReadonlyNewBtn"), busyLabel: "创建中…" });
$("chatInput").oninput = () => {
  renderAgentStatus(state.agentStatus);
  showSkillsPopup();
};
$("chatInput").onkeydown = (event) => {
  const popup = $("skillsPopup");
  const popupOpen = popup && !popup.hidden;
  if (event.key === "Enter" && !event.shiftKey) {
    event.preventDefault();
    if (popupOpen) {
      if (skillsPopupVisible.length > 0) {
        selectSkillsPopupItem(skillsPopupIdx >= 0 ? skillsPopupIdx : 0);
      }
      return;
    }
    $("chatComposer").requestSubmit();
    return;
  }
  if (event.key === "Escape") {
    if (popupOpen) {
      event.preventDefault();
      hideSkillsPopup();
    }
    return;
  }
  if ((event.key === "ArrowDown" || event.key === "Tab") && popupOpen) {
    if (skillsPopupVisible.length === 0) return;
    event.preventDefault();
    const next = skillsPopupIdx < 0 ? 0 : (skillsPopupIdx + 1) % skillsPopupVisible.length;
    skillsPopupIdx = next;
    highlightSkillsPopupItems();
    return;
  }
  if (event.key === "ArrowUp" && popupOpen) {
    if (skillsPopupVisible.length === 0) return;
    event.preventDefault();
    const prev = skillsPopupIdx <= 0 ? skillsPopupVisible.length - 1 : skillsPopupIdx - 1;
    skillsPopupIdx = prev;
    highlightSkillsPopupItems();
  }
};
if ($("skillsSearch")) {
  $("skillsSearch").oninput = () => {
    skillsSearchQuery = $("skillsSearch").value || "";
    renderSkillsList();
  };
}
if ($("refreshSkillsBtn")) {
  $("refreshSkillsBtn").onclick = () => run(async () => {
    await loadSkills();
    await loadSkillsForPopup();
  }, { button: $("refreshSkillsBtn"), busyLabel: "刷新中…", success: "已刷新 Skills" });
}
$("reloadConfigBtn").onclick = () => run(async () => {
  await loadConfigEditor();
}, { button: $("reloadConfigBtn"), busyLabel: "加载中…", success: "已重新加载" });
$("saveConfigBtn").onclick = () => saveConfigEditor($("saveConfigBtn"));
if ($("refreshPreviewBtn")) {
  $("refreshPreviewBtn").onclick = () => run(async () => {
    await refreshProviderConfigPreview();
  }, { button: $("refreshPreviewBtn"), busyLabel: "生成中…" });
}
if ($("previewFullConfig")) {
  $("previewFullConfig").onchange = () => refreshProviderConfigPreview();
}
if ($("configPreviewBlock")) {
  $("configPreviewBlock").addEventListener("toggle", () => {
    if ($("configPreviewBlock").open) refreshProviderConfigPreview();
  });
}
["name", "baseUrl", "profileApiKey", "defaultModel", "webSearchModel", "subagentsExploreModel", "subagentsPlanModel", "upstreamFormat"].forEach((id) => {
  const el = $(id);
  if (!el) return;
  el.addEventListener("input", scheduleProviderPreview);
  el.addEventListener("change", scheduleProviderPreview);
  if (id === "baseUrl") {
    el.addEventListener("input", syncModelBaseURLs);
    el.addEventListener("change", syncModelBaseURLs);
  }
});

if ($("providerSearch")) {
  $("providerSearch").value = state.search || "";
  $("providerSearch").oninput = () => {
    state.search = $("providerSearch").value;
    renderProfiles();
  populateComposerModelSelect();
  };
}
if ($("layoutCardBtn")) {
  $("layoutCardBtn").onclick = () => {
    state.layout = "card";
    applyLayoutUI();
    renderProfiles();
  populateComposerModelSelect();
  };
}
if ($("layoutListBtn")) {
  $("layoutListBtn").onclick = () => {
    state.layout = "list";
    applyLayoutUI();
    renderProfiles();
  populateComposerModelSelect();
  };
}

// Edit form
$("templateSelect").onchange = () => {
  const key = $("templateSelect").value;
  if (key !== "custom") applyTemplate(key);
};
$("cancelBtn").onclick = () => fillForm(newProfileDraft());
$("upstreamFormat").onchange = syncModelBackends;
$("copyProfileBtn").onclick = () => {
  const current = readForm();
  if (!current.name && !current.base_url) {
    toast("请先填写供应商信息", "error");
    return;
  }
  copyProfile(current);
};
$("exportProfileBtn").onclick = () => {
  const current = readForm();
  if (!current.name && !current.base_url) {
    toast("请先填写供应商信息", "error");
    return;
  }
  exportProfile(current);
};
$("importProfileJsonBtn").onclick = () => $("importProfileFile").click();
$("importProfileFile").onchange = async (event) => {
  const file = event.target.files?.[0];
  event.target.value = "";
  if (!file) return;
  try {
    importProfileJSON(await file.text());
  } catch (err) {
    toast(err.message || String(err), "error");
  }
};
$("importBtn").onclick = () => importCurrentConfig($("importBtn"));
$("privacyProtectBtn").onclick = () => run(async () => {
	await api("/api/config/privacy", { method: "POST" });
	if ($("configPreviewBlock")?.open) await refreshProviderConfigPreview();
}, {
	button: $("privacyProtectBtn"),
	busyLabel: "应用中…",
	success: "隐私保护配置已写入 config.toml",
});
$("toggleAdvancedBtn").onclick = () => {
  state.showAdvanced = !state.showAdvanced;
  syncAdvancedUI();
};
$("modelSearchInput").oninput = renderModelSelect;
$("toggleProfileKey").onclick = () => {
  const input = $("profileApiKey");
  input.type = input.type === "password" ? "text" : "password";
  $("toggleProfileKey").textContent = input.type === "password" ? "显示" : "隐藏";
};
$("addModelBtn").onclick = () => {
  addModelCard();
  syncEnabledModelList();
};
$("imageGenEnabled")?.addEventListener("change", async () => {
  syncImageGenUI();
  const enabled = !!$("imageGenEnabled")?.checked;
  try {
    await setImageGenEnabled(enabled);
  } catch (err) {
    $("imageGenEnabled").checked = !enabled;
    syncImageGenUI();
    toast(`切换失败：${err.message || err}`, "error");
  }
});
$("testImageModelBtn")?.addEventListener("click", () => run(async () => {
  const result = await api("/api/imagine/generate", {
    method: "POST",
    body: JSON.stringify({ prompt: "测试生图：一只可爱的小猫", model: "grok-imagine-image", aspect_ratio: "1:1" }),
  });
  if (!result.ok) {
    showImageGenStatus(false, `失败：${result.err_msg || "未知错误"}`);
    throw new Error(result.err_msg || "生图测试失败");
  }
  showImageGenStatus(true, `生图成功（${result.width}x${result.height}，账号 ${result.account}）`);
  toast("生图测试成功", "success");
}, { button: $("testImageModelBtn"), busyLabel: "生图中…" }));
$("testConnectionBtn").onclick = () => run(async () => {
  const current = readForm();
  if (!current.base_url) throw new Error("先填写服务地址");
  if (!current.api_key) throw new Error("先填写 API Key");
  const result = await api("/api/connection/test", {
    method: "POST",
    body: JSON.stringify({
      profile_id: current.id,
      base_url: current.base_url,
      api_key: current.api_key,
      upstream_format: current.upstream_format,
    }),
  });
  if (!result.ok) {
    showConnectionStatus(false, `失败 ${result.latency_ms}ms：${result.error}`);
    throw new Error(result.error || "连接失败");
  }
  if (result.sample_models?.length) {
    state.availableModels = unique([...(state.availableModels || []), ...result.sample_models]);
    renderModelSelect();
  }
  showConnectionStatus(true, `成功 · ${result.latency_ms}ms · ${result.model_count} 模型`);
  toast(`连接成功（${result.latency_ms}ms）`, "success");
}, { button: $("testConnectionBtn"), busyLabel: "测试中…" });
$("fetchModelsBtn").onclick = () => run(async () => {
  const current = readForm();
  if (!current.base_url) throw new Error("先填写服务地址");
  if (!current.api_key) throw new Error("先填写 API Key");
  const result = await api("/api/models/fetch", {
    method: "POST",
    body: JSON.stringify({
      profile_id: current.id,
      base_url: current.base_url,
      api_key: current.api_key,
      upstream_format: current.upstream_format,
    }),
  });
  state.availableModels = unique(result.models);
  if (Array.isArray(result.enabled_models) && result.enabled_models.length) {
    applyOfficialGrokModels(result);
  } else {
    renderModelSelect();
  }
  if ($("connectBlock")) $("connectBlock").open = true;
  const detail = result.official ? "（已按官方列表更新已启用模型）" : "";
  showConnectionStatus(true, `已获取 ${result.models.length} 个模型${detail}`);
  if (result.warning) toast(result.warning, "error");
  toast(`获取到 ${result.models.length} 个模型${detail}`, "success");
}, { button: $("fetchModelsBtn"), busyLabel: "拉取中…" });

function applyOfficialGrokModels(result) {
  $("modelsBody").innerHTML = "";
  result.enabled_models.forEach((model) => addModelCard(model));
  syncEnabledModelList();
  if (result.default_model) $("defaultModel").value = result.default_model;
  if (result.websearch_model) $("webSearchModel").value = result.websearch_model;
  if (result.subagents_models?.explore) $("subagentsExploreModel").value = result.subagents_models.explore;
  if (result.subagents_models?.plan) $("subagentsPlanModel").value = result.subagents_models.plan;
  renderModelSelect();
}

$("profileForm").onsubmit = (event) => {
  event.preventDefault();
  run(async () => {
    const saved = await saveCurrentProfile();
    await refreshAll();
    if (saved?.id) {
      const latest = state.profiles.find((p) => p.id === saved.id) || saved;
      fillForm(latest);
    }
  }, { button: $("saveProfileBtn"), busyLabel: "保存中…", success: "已保存" });
};

$("activateCurrentBtn").onclick = () => run(async () => {
  const saved = await saveCurrentProfile();
  if (saved?.id) {
    await api(`/api/profiles/${saved.id}/activate`, { method: "POST" });
    await refreshAll();
    showView("home");
  }
}, {
  button: $("activateCurrentBtn"),
  busyLabel: "启用中…",
  success: "已保存并启用。新开 grok 会话生效。",
});

$("themeToggleBtn").onclick = () => run(async () => {
  await cycleTheme();
});

$("settingsForm").onsubmit = (event) => {
  event.preventDefault();
  run(async () => {
    const settings = {
      ...state.settings,
      autostart: $("autostart").checked,
      silent_autostart: $("silentAutostart").checked,
      auto_open_browser: $("autoOpenBrowser").checked,
      lan_access_enabled: $("lanAccessEnabled").checked,
      theme: themeSetting(),
      port: Number($("port").value || 17878),
    };
    await api("/api/settings", { method: "PUT", body: JSON.stringify(settings) });
    await refreshAll();
  }, { button: $("saveSettingsBtn"), busyLabel: "保存中…", success: "设置已保存" });
};

$("lanAccessAddress").onchange = () => {
  const index = Number($("lanAccessAddress").value);
  renderLANAddress(state.lanAccess?.addresses?.[index], state.lanAccess);
};

$("copyLanAccessUrlBtn").onclick = () => run(async () => {
  await copyText($("lanAccessUrl").value, "手机配对地址已复制");
}, { button: $("copyLanAccessUrlBtn"), busyLabel: "复制中…" });

$("refreshLanPairingBtn").onclick = () => run(async () => {
  state.lanAccess = await api("/api/lan-access", { method: "POST" });
  renderLANAccess(state.lanAccess);
}, { button: $("refreshLanPairingBtn"), busyLabel: "生成中…", success: "新的配对二维码已生成" });

$("importGrokAuthBtn").onclick = () => $("grokAuthFile").click();
$("grokAuthFile").onchange = async (event) => {
  const file = event.target.files?.[0];
  event.target.value = "";
  if (!file) return;
  await run(async () => {
    const result = await api("/api/grok-auth", {
      method: "POST",
      body: await file.text(),
    });
    await refreshAll();
    if (result.warning) {
      toast(result.warning, "error");
      return false;
    }
  }, {
    button: $("importGrokAuthBtn"),
    busyLabel: "导入中…",
    success: "Grok auth 已导入统一号池，已进入自动巡检",
  });
};

$("importGrokAuthDirBtn").onclick = () => $("grokAuthDirectory").click();
$("grokAuthDirectory").onchange = async (event) => {
  const files = [...(event.target.files || [])].filter((file) => file.name.toLowerCase().endsWith(".json"));
  event.target.value = "";
  if (!files.length) {
    toast("所选目录中没有 JSON 文件", "error");
    return;
  }
  await importGrokPoolFiles(files, $("importGrokAuthDirBtn"));
};

$("toggleGrokAuthKeyBtn").onclick = () => {
  const input = $("grokAuthApiKey");
  input.type = input.type === "password" ? "text" : "password";
  $("toggleGrokAuthKeyBtn").textContent = input.type === "password" ? "显示" : "隐藏";
};

$("copyGrokAuthUrlBtn").onclick = () => run(
  () => copyText($("grokAuthBaseUrl").value, "Base URL 已复制"),
  { button: $("copyGrokAuthUrlBtn") },
);

$("copyGrokAuthKeyBtn").onclick = () => run(
  () => copyText($("grokAuthApiKey").value, "本地 API Key 已复制"),
  { button: $("copyGrokAuthKeyBtn") },
);

$("refreshGrokAuthBtn").onclick = () => run(async () => {
  await api("/api/grok-auth/refresh", { method: "POST" });
  await refreshAll();
}, {
  button: $("refreshGrokAuthBtn"),
  busyLabel: "刷新中…",
  success: "xAI token 已刷新",
});

$("activateGrokAuthBtn").onclick = () => {
  const profile = state.profiles.find((item) => item.name === "Grok Auth（本地代理）");
  if (!profile) {
    toast("没有找到本地 Grok profile，请重新导入 JSON", "error");
    return;
  }
  activateProfile(profile.id, $("activateGrokAuthBtn"), profile.name);
};

$("deleteGrokAuthBtn").onclick = () => run(async () => {
  if (!confirm("删除已导入的 Grok OAuth 凭据和本地代理 profile？")) return false;
  await api("/api/grok-auth", { method: "DELETE" });
  $("grokAuthApiKey").type = "password";
  $("toggleGrokAuthKeyBtn").textContent = "显示";
  await refreshAll();
}, {
  button: $("deleteGrokAuthBtn"),
  busyLabel: "删除中…",
  success: "Grok auth 已删除",
});

$("registrarEmailProvider").onchange = updateRegistrarProviderFields;

$("registrarClashAutoDetectBtn")?.addEventListener("click", async () => {
  const btn = $("registrarClashAutoDetectBtn");
  const hint = $("registrarClashAutoDetectHint");
  btn.disabled = true;
  btn.textContent = "🔍 正在扫描本地端口…";
  hint.hidden = false;
  hint.textContent = "正在扫描 9090 / 9097 / 9091 等常见 Clash 控制器端口…";
  try {
    const res = await fetch("/api/registrar/clash-autodetect", { method: "POST" });
    const data = await res.json();
    if (data.found) {
      if (data.controller && $("registrarClashController")) $("registrarClashController").value = data.controller;
      if (data.group && $("registrarClashSelectorGroup")) $("registrarClashSelectorGroup").value = data.group;
      const portInfo = data.mixed_port ? `（mixed-port=${data.mixed_port}）` : "";
      hint.innerHTML = `✅ 已检测到 Clash 核心 v${data.core_version}，控制器=<code>${data.controller}</code>，选择器组=<code>${data.group || "未找到"}</code>（${data.group_node_count || 0} 个可用节点）${portInfo}。配置已自动填入。`;
      registrarFormDirty = true;
    } else {
      hint.innerHTML = "❌ 未检测到正在运行的 Clash/mihomo 控制器。请确认 FlClash / ClashVerge / ClashX 已启动并开启了外部控制器（默认端口 9090）。";
    }
  } catch (e) {
    hint.textContent = "❌ 检测失败：" + e.message;
  } finally {
    btn.disabled = false;
    btn.textContent = "🔍 自动检测 Clash";
  }
});

["registrarBrowserPath", "registrarBrowserMode", "registrarProxyUrl", "registrarProxyStrategy", "registrarProxyCooldown", "registrarClashController", "registrarClashSelectorGroup", "registrarEngine", "registrarEmailProvider",
  "registrarDefaultDomains", "registrarCloudmailUrl", "registrarCloudmailAdminEmail",
  "registrarCloudmailPassword", "registrarCloudflareApiBase", "registrarCloudflareApiKey",
  "registrarCloudflareAuthMode", "registrarCloudflareDomains", "registrarCloudflareDomainsPath",
  "registrarCloudflareAccountsPath", "registrarCloudflareTokenPath", "registrarCloudflareMessagesPath",
  "registrarHotmailAccounts", "registrarHotmailAliases",
  "registrarCount", "registrarWorkers", "registrarMailTimeout", "registrarPreferProtocol",
  "registrarProtocolOnly"].forEach((id) => {
  const el = $(id);
  if (!el) return;
  el.addEventListener("input", () => { registrarFormDirty = true; });
  el.addEventListener("change", () => { registrarFormDirty = true; });
});

$("registrarForm").onsubmit = (event) => {
  event.preventDefault();
  run(async () => {
    state.registrar = await api("/api/registrar", {
      method: "PUT",
      body: JSON.stringify(registrarConfigFromForm()),
    });
    registrarFormDirty = false;
    renderRegistrar(state.registrar);
  }, { button: $("saveRegistrarBtn"), busyLabel: "保存中…", success: "注册机配置已保存" });
};

$("probeRegistrarBtn").onclick = () => run(async () => {
  const result = await api("/api/registrar/probe", {
    method: "POST",
    body: JSON.stringify(registrarConfigFromForm()),
  });
  const lines = (result.checks || []).map((check) => `${check.ok ? "OK" : "失败"} · ${check.name} · ${check.detail || ""}`);
  $("registrarLog").textContent = lines.join("\n") || "没有检测结果";
  if (!result.ok) {
    toast("注册环境检测未通过", "error");
    return false;
  }
}, { button: $("probeRegistrarBtn"), busyLabel: "检测中…", success: "注册环境可用" });

$("startRegistrarBtn").onclick = () => run(async () => {
  state.registrar = await api("/api/registrar", {
    method: "PUT",
    body: JSON.stringify(registrarConfigFromForm()),
  });
  registrarFormDirty = false;
  registrarTerminalNotice = "";
  const response = await api("/api/registrar/start", { method: "POST", body: "{}" });
  state.registrar.job = response.job;
  renderRegistrarJob(response.job);
}, { button: $("startRegistrarBtn"), busyLabel: "启动中…" });

$("stopRegistrarBtn").onclick = () => run(async () => {
  await api("/api/registrar/stop", { method: "POST", body: "{}" });
  await loadRegistrarJob();
}, { button: $("stopRegistrarBtn"), busyLabel: "停止中…", success: "已请求停止注册" });

$("grokPoolSettingsForm").onsubmit = (event) => {
  event.preventDefault();
  run(async () => {
    state.grokPool = await api("/api/grok-pool", {
      method: "PUT",
      body: JSON.stringify({
        enabled: $("grokPoolAutoEnabled").checked,
        interval_minutes: Number($("grokPoolInterval").value || 360),
        workers: Number($("grokPoolWorkers").value || 4),
        proxy_url: $("grokPoolProxyUrl").value.trim(),
        auth_dir: $("grokPoolAuthDir").value.trim(),
        watch_enabled: $("grokPoolWatchEnabled").checked,
        watch_recursive: $("grokPoolWatchRecursive").checked,
      }),
    });
    renderGrokPool(state.grokPool);
  }, { button: $("saveGrokPoolSettingsBtn"), busyLabel: "保存中…", success: "号池巡检设置已保存" });
};

$("importGrokPoolAuthDirBtn").onclick = () => run(async () => {
  const response = await api("/api/grok-pool/import-dir", {
    method: "POST",
    body: JSON.stringify({ use_auth_dir: true }),
  });
  await refreshAll();
  if (response.result?.failed?.length) {
    toast(`部分文件失败：${response.result.failed.join("；")}`, "error");
    return false;
  }
}, {
  button: $("importGrokPoolAuthDirBtn"),
  busyLabel: "导入中…",
  success: "认证目录已导入号池",
});

$("openGrokPoolAuthDirBtn").onclick = () => run(
  () => api("/api/grok-pool/open-auth-dir", { method: "POST" }),
  { button: $("openGrokPoolAuthDirBtn"), busyLabel: "打开中…" },
);

$("importGrokPoolPathBtn").onclick = () => {
  const path = prompt("输入服务器本机上的认证目录绝对路径：", $("grokPoolAuthDir").value.trim());
  if (!path?.trim()) return;
  run(async () => {
    const response = await api("/api/grok-pool/import-dir", {
      method: "POST",
      body: JSON.stringify({ path: path.trim(), recursive: true }),
    });
    await refreshAll();
    if (response.result?.failed?.length) {
      toast(`部分文件失败：${response.result.failed.join("；")}`, "error");
      return false;
    }
  }, {
    button: $("importGrokPoolPathBtn"),
    busyLabel: "导入中…",
    success: "指定目录已导入号池",
  });
};

$("startCpaMintBtn").onclick = () => run(async () => {
  cpaMintTerminalNotice = "";
  const response = await api("/api/cpa-mint", {
    method: "POST",
    body: JSON.stringify({
      email: $("cpaMintEmail").value.trim(),
      open_browser: true,
    }),
  });
  renderCpaMint(response.session);
}, {
  button: $("startCpaMintBtn"),
  busyLabel: "启动中…",
});

$("cancelCpaMintBtn").onclick = () => run(async () => {
  if (!cpaMintSession?.id) return false;
  const response = await api(`/api/cpa-mint?id=${encodeURIComponent(cpaMintSession.id)}`, {
    method: "DELETE",
  });
  renderCpaMint(response.session, { notify: true });
}, {
  button: $("cancelCpaMintBtn"),
  busyLabel: "取消中…",
});

$("openCpaMintUrlBtn").onclick = () => {
  const url = cpaMintSession?.verification_uri_complete;
  if (!url) return;
  window.open(url, "_blank", "noopener");
};

$("importGrokPoolBtn").onclick = () => $("grokPoolFiles").click();
$("grokPoolFiles").onchange = async (event) => {
  const files = [...(event.target.files || [])];
  event.target.value = "";
  await importGrokPoolFiles(files, $("importGrokPoolBtn"));
};

$("importGrokPoolDirBtn").onclick = () => $("grokPoolDirectory").click();
$("grokPoolDirectory").onchange = async (event) => {
  const files = [...(event.target.files || [])].filter((file) => file.name.toLowerCase().endsWith(".json"));
  event.target.value = "";
  if (!files.length) {
    toast("所选目录中没有 JSON 文件", "error");
    return;
  }
  await importGrokPoolFiles(files, $("importGrokPoolDirBtn"));
};

async function importGrokPoolFiles(files, button) {
  if (!files.length) return;
  await run(async () => {
    const payload = await Promise.all(files.map(async (file) => ({
      name: file.webkitRelativePath || file.name,
      content: await file.text(),
    })));
    const response = await api("/api/grok-pool", {
      method: "POST",
      body: JSON.stringify({ files: payload }),
    });
    await refreshAll();
    const failed = response.result?.failed || [];
    if (failed.length) {
      toast(`部分文件失败：${failed.join("；")}`, "error");
      return false;
    }
  }, { button, busyLabel: "导入中…", success: `已处理 ${files.length} 个 JSON 文件` });
}

$("inspectGrokPoolBtn").onclick = () => run(async () => {
  state.grokPool = await api("/api/grok-pool/inspect", { method: "POST" });
  renderGrokPool(state.grokPool);
}, { button: $("inspectGrokPoolBtn"), busyLabel: "启动中…", success: "号池巡检已启动" });

if ($("batchRefreshGrokPoolBtn")) {
  $("batchRefreshGrokPoolBtn").onclick = () => batchRefreshAccounts($("batchRefreshGrokPoolBtn"));
}

$("stopGrokPoolBtn").onclick = () => run(async () => {
  await api("/api/grok-pool/inspect", { method: "DELETE" });
  await loadGrokPool();
}, { button: $("stopGrokPoolBtn"), busyLabel: "停止中…", success: "已请求停止巡检" });

async function bulkGrokPoolAction(action, button) {
  const abnormal = (state.grokPool?.accounts || []).filter((account) => {
    const classification = account.classification || "uninspected";
    return classification !== "healthy" && classification !== "uninspected" &&
      (action !== "disable" || !account.disabled);
  });
  if (!abnormal.length) {
    toast("当前没有已巡检的异常账号", "error");
    return;
  }
  const verb = action === "delete" ? "删除" : "禁用";
  const extra = action === "delete" ? "删除后需要重新导入才能恢复。" : "原凭据仍会保留。";
  if (!confirm(`确定批量${verb} ${abnormal.length} 个异常账号？\n${extra}`)) return;
  await run(async () => {
    const response = await api("/api/grok-pool/bulk", {
      method: "POST",
      body: JSON.stringify({ action }),
    });
    await refreshAll();
    if (response.result?.failed?.length) {
      toast(`操作完成，但有文件清理失败：${response.result.failed.join("；")}`, "error");
      return false;
    }
    if (action === "delete" && response.result?.deleted_files) {
      toast(`已批量删除 ${abnormal.length} 个异常账号（含 ${response.result.deleted_files} 个源 JSON 文件）`, "success");
      return false;
    }
  }, { button, busyLabel: `${verb}中…`, success: `已批量${verb} ${abnormal.length} 个异常账号` });
}

$("batchDisableGrokPoolBtn").onclick = () => bulkGrokPoolAction("disable", $("batchDisableGrokPoolBtn"));
$("batchDeleteGrokPoolBtn").onclick = () => bulkGrokPoolAction("delete", $("batchDeleteGrokPoolBtn"));

$("exportGrokPoolDirBtn")?.addEventListener("click", async () => {
  const target = await pickDirectoryPath();
  if (!target) return;
  await run(async () => {
    const response = await api("/api/grok-pool/export-dir", {
      method: "POST",
      body: JSON.stringify({ path: target }),
    });
    if (response.ok) {
      const msg = `已导出 ${response.exported} 个 JSON 文件到：${response.path}` + (response.skipped ? `（跳过 ${response.skipped} 个）` : "");
      toast(msg, "success");
      try { await api("/api/agent/open-path", { method: "POST", body: JSON.stringify({ path: target }) }); } catch (_) {}
      return false;
    }
  }, { button: $("exportGrokPoolDirBtn"), busyLabel: "导出中…", success: "导出完成" });
});

$("activateGrokPoolBtn").onclick = () => {
  const profile = state.profiles.find((item) => item.name === "Grok Auth（本地代理）");
  if (!profile) {
    toast("没有找到号池本地 profile，请重新导入账号", "error");
    return;
  }
  activateProfile(profile.id, $("activateGrokPoolBtn"), profile.name);
};

$("toggleGrokPoolKeyBtn").onclick = () => {
  const input = $("grokPoolApiKey");
  input.type = input.type === "password" ? "text" : "password";
  $("toggleGrokPoolKeyBtn").textContent = input.type === "password" ? "显示" : "隐藏";
};

$("copyGrokPoolUrlBtn").onclick = () => run(
  () => copyText($("grokPoolBaseUrl").value, "号池 Base URL 已复制"),
  { button: $("copyGrokPoolUrlBtn") },
);

$("copyGrokPoolKeyBtn").onclick = () => run(
  () => copyText($("grokPoolApiKey").value, "号池 API Key 已复制"),
  { button: $("copyGrokPoolKeyBtn") },
);

$("refreshBackupsBtn").onclick = () => run(async () => {
  renderBackups(await api("/api/backups"));
}, { button: $("refreshBackupsBtn"), busyLabel: "…" });

function scheduleRefresh() {
  clearTimeout(refreshTimer);
  refreshTimer = setTimeout(() => {
    refreshAll().catch((err) => toast(err.message, "error"));
  }, 400);
}

document.addEventListener("visibilitychange", () => {
  if (document.visibilityState === "visible") scheduleRefresh();
});

$("updateDismissBtn")?.addEventListener("click", () => {
  state.updateHidden = true;
  renderUpdate(state.update);
});

$("updateSkipBtn")?.addEventListener("click", () => run(async () => {
  const version = state.update?.latest_version;
  if (!version) return false;
  if (!confirm(`跳过 ${version}？下一个更高版本仍会提醒。`)) return false;
  const info = await api("/api/update", {
    method: "POST",
    body: JSON.stringify({ action: "skip", version }),
  });
  state.update = info;
  state.updateHidden = true;
  renderUpdate(info);
  toast(`已跳过 ${version}`, "success");
  return false;
}, { button: $("updateSkipBtn"), busyLabel: "保存中…" }));
window.addEventListener("focus", () => scheduleRefresh());
window.addEventListener("resize", () => {
  clearTimeout(chatLayoutResizeTimer);
  chatLayoutResizeTimer = setTimeout(applyStoredChatPanelWidths, 100);
});

document.addEventListener("keydown", (event) => {
  if ((event.ctrlKey || event.metaKey) && event.key.toLowerCase() === "f" && state.view === "chat") {
    event.preventDefault();
    openChatFindBar();
    return;
  }
  if ((event.ctrlKey || event.metaKey) && event.key.toLowerCase() === "s") {
    if (state.view === "edit") {
      event.preventDefault();
      $("saveProfileBtn").click();
    }
  }
  if (event.key === "Escape" && $("chatFindBar") && !$("chatFindBar").hidden) {
    event.preventDefault();
    closeChatFindBar();
    return;
  }
  if (event.key === "Escape" && $("chatThemeDialog")?.open) {
    return;
  }
  if (event.key === "Escape" && state.view === "chat" && (state.agentStatus?.state === "busy" || state.agentStatus?.busy)) {
    event.preventDefault();
    cancelAgentGeneration().catch((err) => toast(err.message || String(err), "error"));
    return;
  }
  if (event.key === "Escape" && state.view !== "home") {
    showView("home");
  }
});

initialiseChatThemes();
showView("home");
setTimeout(checkForUpdates, 15_000);
refreshAll()
  .then(() => {
    loadLatestCpaMint().catch((err) => toast(err.message, "error"));
  })
  .catch((err) => toast(err.message, "error"));

/* Slash palette + skills popup state */
let skillsPopupSkills = [];
let skillsPopupVisible = [];
let skillsPopupIdx = -1;
let skillsPopupLoading = false;
let workspaceFileEntries = [];
let workspaceFileFilter = "";

const BUILTIN_SLASH_COMMANDS = [
  { id: "plan", name: "plan", title: "Plan 模式", description: "进入规划模式", kind: "command", insert: "/plan " },
  { id: "goal", name: "goal", title: "Goal", description: "设定目标", kind: "command", insert: "/goal " },
  { id: "compact", name: "compact", title: "压缩上下文", description: "请求 Agent 压缩上下文", kind: "command", insert: "/compact", action: "compact" },
  { id: "status", name: "status", title: "状态", description: "查看当前 Agent 状态", kind: "command", action: "status" },
  { id: "mcp", name: "mcp", title: "MCP", description: "发送 /mcp 查看 MCP", kind: "command", insert: "/mcp" },
  { id: "doctor", name: "doctor", title: "Doctor", description: "环境自检提示", kind: "command", action: "doctor" },
  { id: "new", name: "new", title: "新对话", description: "创建新会话", kind: "command", action: "newChat" },
  { id: "yolo", name: "yolo", title: "YOLO", description: "切换自动批准工具", kind: "command", action: "yolo" },
  { id: "skills", name: "skills", title: "Skills", description: "浏览并插入 Skill", kind: "command", insert: "/skills " },
];

function slashPopupFilterQuery() {
  const input = $("chatInput");
  if (!input) return null;
  const text = input.value;
  const cursorPos = input.selectionStart ?? text.length;
  const lineStart = text.lastIndexOf("\n", cursorPos - 1) + 1;
  const currentLine = text.slice(lineStart, cursorPos);
  if (!currentLine.startsWith("/")) return null;
  // After a completed `/skills name ` keep closed.
  const skillsMatch = currentLine.match(/^\/skills\s+(\S+)\s+/i);
  if (skillsMatch) return null;
  const body = currentLine.slice(1);
  return body.toLowerCase();
}

function highlightSkillsPopupItems() {
  const list = $("skillsPopupList");
  if (!list) return;
  list.querySelectorAll(".skillsPopupItem").forEach((el, i) => {
    el.classList.toggle("is-selected", i === skillsPopupIdx);
  });
  list.querySelector(".skillsPopupItem.is-selected")?.scrollIntoView({ block: "nearest" });
}

function buildSlashCatalog(filterQuery) {
  const q = String(filterQuery || "").toLowerCase();
  const skillsMode = q.startsWith("skills");
  const skillFilter = skillsMode ? q.replace(/^skills\s*/, "") : q;
  const commands = BUILTIN_SLASH_COMMANDS.filter((cmd) => {
    if (skillsMode && cmd.name !== "skills") return false;
    if (!q) return true;
    return cmd.name.includes(q) || cmd.title.toLowerCase().includes(q) || cmd.description.toLowerCase().includes(q);
  }).map((cmd) => ({ ...cmd, section: "命令" }));
  const skills = (skillsPopupSkills || [])
    .filter((sk) => skillMatchesQuery(sk, skillFilter))
    .map((sk) => ({
      id: `skill:${sk.name}`,
      name: sk.name,
      title: sk.name,
      description: skillSourceMeta(sk.source).short + (sk.path ? ` · ${sk.path}` : ""),
      kind: "skill",
      section: "Skills",
      skill: sk,
    }));
  return [...commands, ...skills];
}

function showSkillsPopup() {
  const popup = $("skillsPopup");
  if (!popup) return;
  const input = $("chatInput");
  if (!input || input.disabled) {
    hideSkillsPopup();
    return;
  }
  const filterQuery = slashPopupFilterQuery();
  if (filterQuery === null) {
    hideSkillsPopup();
    return;
  }

  const list = $("skillsPopupList");
  const countEl = $("skillsPopupCount");
  if (!list) return;

  if (skillsPopupLoading && skillsPopupSkills.length === 0 && filterQuery.startsWith("skills")) {
    skillsPopupVisible = [];
    skillsPopupIdx = -1;
    list.innerHTML = `<div class="skillsPopupLoading">正在加载 Skills…</div>`;
    if (countEl) countEl.textContent = "…";
    popup.hidden = false;
    return;
  }

  skillsPopupVisible = buildSlashCatalog(filterQuery);
  if (!skillsPopupVisible.length) {
    skillsPopupIdx = -1;
    list.innerHTML = `<div class="skillsPopupEmpty"><strong>无匹配项</strong><span>试试 /plan /compact /skills</span></div>`;
    if (countEl) countEl.textContent = "0";
    popup.hidden = false;
    return;
  }

  if (skillsPopupIdx < 0 || skillsPopupIdx >= skillsPopupVisible.length) {
    skillsPopupIdx = 0;
  }

  let lastSection = "";
  list.innerHTML = skillsPopupVisible.map((item, i) => {
    const sectionHtml = item.section !== lastSection
      ? `<div class="slashSectionLabel">${escapeHtml(item.section)}</div>`
      : "";
    lastSection = item.section;
    const icon = item.kind === "skill"
      ? `<img class="skillsPopupItemIcon" src="/skill.svg" alt="" aria-hidden="true">`
      : `<span class="skillsPopupItemIcon slashCmdIcon">/</span>`;
    return `${sectionHtml}<button type="button" class="skillsPopupItem${i === skillsPopupIdx ? " is-selected" : ""}" role="option" data-index="${i}" aria-selected="${i === skillsPopupIdx ? "true" : "false"}">
      ${icon}
      <span class="skillsPopupItemInfo">
        <span class="skillsPopupItemName">${escapeHtml(item.title || item.name)}</span>
        <span class="skillsPopupItemMeta"><span class="skillsPopupItemSource">${escapeHtml(item.description || "")}</span></span>
      </span>
    </button>`;
  }).join("");

  list.querySelectorAll(".skillsPopupItem").forEach((btn) => {
    btn.onmouseenter = () => {
      skillsPopupIdx = Number(btn.dataset.index);
      highlightSkillsPopupItems();
    };
    btn.onclick = (event) => {
      event.preventDefault();
      selectSkillsPopupItem(Number(btn.dataset.index));
    };
  });

  if (countEl) countEl.textContent = String(skillsPopupVisible.length);
  if ($("slashPopupTitle")) $("slashPopupTitle").textContent = "命令 / Skills";
  popup.hidden = false;
  highlightSkillsPopupItems();
}

function hideSkillsPopup() {
  const popup = $("skillsPopup");
  if (popup) popup.hidden = true;
  skillsPopupIdx = -1;
  skillsPopupVisible = [];
}

function selectSkillsPopupItem(index) {
  const items = skillsPopupVisible;
  if (index < 0 || index >= items.length) return;
  const item = items[index];
  if (item.kind === "skill") {
    insertSlashLine(`/skills ${item.name} `);
    hideSkillsPopup();
    return;
  }
  if (item.action === "newChat") {
    hideSkillsPopup();
    run(newAgentSession, { busyLabel: "创建中…" });
    return;
  }
  if (item.action === "yolo") {
    hideSkillsPopup();
    const box = $("agentAlwaysApprove");
    if (box) {
      box.checked = !box.checked;
      toast(box.checked ? "已勾选 YOLO（下次启动生效）" : "已取消 YOLO", "info");
    }
    return;
  }
  if (item.action === "status") {
    hideSkillsPopup();
    const s = state.agentStatus || {};
    toast(`Agent ${s.state || "idle"} · model ${s.model || "—"} · cwd ${s.cwd || "—"}`, "info");
    return;
  }
  if (item.action === "doctor") {
    hideSkillsPopup();
    toast(state.agentStatus?.available === false
      ? "未检测到 grok 可执行文件，请安装 Grok Build 并加入 PATH"
      : `Grok 可用：${state.agentStatus?.grok_path || "已发现"}`, "info");
    return;
  }
  if (item.action === "compact") {
    if (!confirm("向 Agent 发送 /compact 以压缩上下文？")) return;
    insertSlashLine("/compact");
    hideSkillsPopup();
    sendAgentMessage().catch((err) => toast(err.message, "error"));
    return;
  }
  if (item.insert) {
    insertSlashLine(item.insert);
    hideSkillsPopup();
    if (item.insert === "/skills ") loadSkillsForPopup();
  }
}

function insertSlashLine(insert) {
  const input = $("chatInput");
  if (!input) return;
  const text = input.value;
  const cursorPos = input.selectionStart ?? text.length;
  const lineStart = text.lastIndexOf("\n", cursorPos - 1) + 1;
  const lineEnd = text.indexOf("\n", cursorPos);
  const before = text.slice(0, lineStart);
  const after = text.slice(lineEnd >= 0 ? lineEnd : text.length);
  input.value = before + insert + after;
  input.selectionStart = input.selectionEnd = before.length + insert.length;
  input.focus();
  renderAgentStatus(state.agentStatus);
}

async function loadSkillsForPopup() {
  skillsPopupLoading = true;
  showSkillsPopup();
  try {
    const data = await api("/api/skills");
    skillsPopupSkills = Array.isArray(data) ? data : [];
  } catch {
    skillsPopupSkills = [];
  } finally {
    skillsPopupLoading = false;
    showSkillsPopup();
  }
}

async function loadAgentProjects() {
  try {
    const list = await api("/api/agent/projects");
    state.projects = Array.isArray(list) ? list : [];
  } catch {
    state.projects = [];
  }
  // Sync active project from current cwd without forcing a re-render loop.
  const activePath = normalizePathKey($("agentCwd")?.value || state.activeAgentSession?.cwd || "");
  const active = (state.projects || []).find((p) => normalizePathKey(p.path) === activePath);
  state.activeProjectId = active?.id || state.activeProjectId || "";
  renderSidebarTree();
  updateComposerProjectLabel();
}

/**
 * Project tree: each workspace expands to show its sessions (cwd match).
 * Mirrors grok-app sidebar nesting: Project → sessions; orphans listed separately.
 */
function renderAgentProjects() {
  const host = $("agentProjectList");
  if (!host) return;
  const list = state.projects || [];
  const datalist = $("recentProjectPaths");
  if (datalist) {
    datalist.innerHTML = list.map((p) => `<option value="${escapeAttr(p.path)}"></option>`).join("");
  }

  const activePath = normalizePathKey($("agentCwd")?.value || state.activeAgentSession?.cwd || "");
  const active = list.find((p) => normalizePathKey(p.path) === activePath);
  if (active) state.activeProjectId = active.id;

  if (!list.length) {
    host.innerHTML = `<p class="sessionListEmpty">暂无项目。点击 ＋ 添加工作目录后，会话会归入对应项目。</p>`;
    updateComposerProjectLabel();
    return;
  }

  host.innerHTML = "";
  for (const project of list) {
    const pathKey = normalizePathKey(project.path);
    const isActive = !!activePath && pathKey === activePath;
    const missing = project.path_ok === false;
    const expanded = isProjectExpanded(project.id);
    const sessions = sessionsForProject(project);

    const folder = document.createElement("div");
    folder.className = `projectFolder${isActive ? " is-active" : ""}${missing ? " is-missing" : ""}${expanded ? " is-expanded" : ""}`;
    folder.dataset.projectId = project.id;

    const row = document.createElement("div");
    row.className = "projectItem";
    row.role = "button";
    row.tabIndex = 0;
    row.setAttribute("aria-expanded", expanded ? "true" : "false");
    row.title = project.path || "";

    const chevron = document.createElement("span");
    chevron.className = "projectItemChevron";
    chevron.setAttribute("aria-hidden", "true");
    chevron.textContent = expanded ? "▾" : "▸";

    const body = document.createElement("span");
    body.className = "projectItemBody";
    const name = document.createElement("span");
    name.className = "projectItemName";
    name.textContent = `${project.pinned ? "📌 " : ""}${project.name || "未命名项目"}`;
    if (!project.trusted) {
      const badge = document.createElement("span");
      badge.className = "projectItemBadge";
      badge.textContent = "未信任";
      name.append(" ", badge);
    }
    if (missing) {
      const badge = document.createElement("span");
      badge.className = "projectItemBadge";
      badge.textContent = "路径失效";
      name.append(" ", badge);
    }
    const pathEl = document.createElement("span");
    pathEl.className = "projectItemPath mono";
    pathEl.textContent = project.path || "";
    body.append(name, pathEl);

    const count = document.createElement("span");
    count.className = "projectItemCount";
    count.textContent = String(sessions.length);
    count.title = `${sessions.length} 个会话`;

    const actions = document.createElement("div");
    actions.className = "projectItemActions";
    if (missing) {
      const fixBtn = document.createElement("button");
      fixBtn.type = "button";
      fixBtn.className = "sessionActionBtn projectFixPathBtn";
      fixBtn.title = "重新选择目录（修复中文/失效路径）";
      fixBtn.setAttribute("aria-label", `修复 ${project.name} 的路径`);
      fixBtn.textContent = "↻";
      fixBtn.onclick = (event) => {
        event.stopPropagation();
        relocateProjectPath(project).catch((err) => toast(err.message || String(err), "error"));
      };
      actions.append(fixBtn);
    }
    const newBtn = document.createElement("button");
    newBtn.type = "button";
    newBtn.className = "sessionActionBtn projectNewSessionBtn";
    newBtn.title = "在此项目下新建会话";
    newBtn.setAttribute("aria-label", `在 ${project.name} 下新建会话`);
    newBtn.textContent = "✎";
    newBtn.disabled = missing;
    newBtn.onclick = (event) => {
      event.stopPropagation();
      run(() => newAgentSession(project), { busyLabel: "创建中…" }).catch((err) => {
        if (!isAbortError(err)) toast(err.message || String(err), "error");
      });
    };
    actions.append(newBtn);

    row.append(chevron, body, count, actions);

    const toggleExpand = () => {
      setProjectExpanded(project.id, !isProjectExpanded(project.id));
      renderSidebarTree();
    };
    row.onclick = (event) => {
      // Row click toggles expand; double-activate workspace when already expanded.
      if (event.detail > 1) {
        openProjectById(project.id).catch((err) => toast(err.message, "error"));
        return;
      }
      toggleExpand();
    };
    row.onkeydown = (event) => {
      if (event.key === "Enter" || event.key === " ") {
        event.preventDefault();
        toggleExpand();
      }
    };

    folder.append(row);

    if (expanded) {
      const children = document.createElement("div");
      children.className = "projectSessionList";
      if (missing) {
        const fixHint = document.createElement("button");
        fixHint.type = "button";
        fixHint.className = "projectTrustHint";
        fixHint.textContent = "路径不可用，点击重新选择目录";
        fixHint.onclick = (event) => {
          event.stopPropagation();
          relocateProjectPath(project).catch((err) => toast(err.message || String(err), "error"));
        };
        children.append(fixHint);
      } else if (!project.trusted) {
        const trustBtn = document.createElement("button");
        trustBtn.type = "button";
        trustBtn.className = "projectTrustHint";
        trustBtn.textContent = "信任此项目以创建会话";
        trustBtn.onclick = (event) => {
          event.stopPropagation();
          openProjectById(project.id).catch((err) => toast(err.message, "error"));
        };
        children.append(trustBtn);
      }
      if (!sessions.length) {
        const empty = document.createElement("p");
        empty.className = "sessionListEmpty projectSessionEmpty";
        empty.textContent = missing
          ? "修复路径后可继续使用"
          : (project.trusted ? "暂无会话，点 ✎ 新建" : "信任后可在此创建会话");
        children.append(empty);
      } else {
        for (const session of sessions) {
          // Nested under project: path is implied by parent workspace.
          children.append(buildSessionItemEl(session, { nested: true, showPath: false }));
        }
      }
      folder.append(children);
    }

    host.append(folder);
  }
  updateComposerProjectLabel();
}

async function openProjectById(id) {
  const project = (state.projects || []).find((p) => p.id === id);
  if (!project) return;
  if (project.path_ok === false) {
    await relocateProjectPath(project);
    return;
  }
  const ok = await activateProjectWorkspace(project, { createSession: false });
  if (!ok) return;
  updateConversationIdentity();
  toast(`已切换工作空间：${project.name}`, "info");
}

/** Re-pick directory for a project whose path is missing or was garbled (Chinese). */
async function relocateProjectPath(project) {
  if (!project?.id) return false;
  toast(`请重新选择「${project.name || "项目"}」的目录`, "info");
  const path = await pickDirectoryPath(project.path || "");
  if (!path) {
    toast("已取消修复", "info");
    return false;
  }
  const updated = await api("/api/agent/projects/relocate", {
    method: "POST",
    body: JSON.stringify({ id: project.id, path }),
  });
  await loadAgentProjects();
  const next = (state.projects || []).find((p) => p.id === project.id) || updated;
  if (next?.path) {
    if ($("agentCwd")) $("agentCwd").value = next.path;
    state.activeProjectId = next.id;
    setProjectExpanded(next.id, true);
  }
  updateConversationIdentity();
  toast(`已修复路径：${next?.path || path}`, "success");
  return true;
}

/** Open native folder dialog; returns absolute path or "" if cancelled. */
async function pickDirectoryPath(start = "") {
  const initial = String(start || $("agentCwd")?.value || state.agentStatus?.cwd || "").trim();
  const result = await api("/api/agent/pick-directory", {
    method: "POST",
    body: JSON.stringify({ start: initial }),
  });
  if (result?.cancelled || !result?.path) return "";
  return String(result.path).trim();
}

async function pickWorkingDirectory() {
  const path = await pickDirectoryPath();
  if (!path) {
    toast("已取消选择", "info");
    return;
  }
  if ($("agentCwd")) $("agentCwd").value = path;
  // If the path is already a registered project, activate that workspace.
  const matched = projectPathKeys().get(normalizePathKey(path));
  if (matched) {
    state.activeProjectId = matched.id;
    setProjectExpanded(matched.id, true);
  } else {
    state.activeProjectId = "";
  }
  updateConversationIdentity();
  updateComposerProjectLabel();
  toast(matched ? `已切换到项目：${matched.name}` : `已选择工作目录：${path}`, "success");
}

async function addProjectFromPrompt() {
  // Prefer native folder picker; fall back to prompt if the API is unavailable.
  let path = "";
  try {
    path = await pickDirectoryPath($("agentCwd")?.value || "");
  } catch (err) {
    path = prompt("项目目录绝对路径（文件夹选择器不可用时手动输入）", $("agentCwd")?.value || "") || "";
    if (!path && err?.message) toast(err.message, "error");
  }
  if (!path) return;
  const item = await api("/api/agent/projects", {
    method: "POST",
    body: JSON.stringify({ path, trusted: true }),
  });
  await loadAgentProjects();
  if (item?.id) {
    await openProjectById(item.id);
    setProjectExpanded(item.id, true);
    renderSidebarTree();
  }
}

async function saveCurrentCwdAsProject() {
  const path = $("agentCwd")?.value.trim();
  if (!path) throw new Error("请先填写或选择工作目录");
  await api("/api/agent/projects", { method: "POST", body: JSON.stringify({ path, trusted: true }) });
  await loadAgentProjects();
  toast("已保存为项目", "success");
}

async function openWorkspaceFilePicker() {
  const cwd = $("agentCwd")?.value.trim() || state.agentStatus?.cwd || "";
  if (!cwd) throw new Error("请先设置工作目录");
  const popup = $("workspaceFilePopup");
  const list = $("workspaceFileList");
  if (!popup || !list) return;
  list.innerHTML = `<div class="skillsPopupLoading">加载中…</div>`;
  popup.hidden = false;
  workspaceFileEntries = await api(`/api/agent/fs?cwd=${encodeURIComponent(cwd)}`);
  if (!Array.isArray(workspaceFileEntries)) workspaceFileEntries = [];
  workspaceFileFilter = "";
  if ($("workspaceFileSearch")) $("workspaceFileSearch").value = "";
  filterWorkspaceFileList();
}

function hideWorkspaceFilePicker() {
  if ($("workspaceFilePopup")) $("workspaceFilePopup").hidden = true;
}

function filterWorkspaceFileList() {
  const list = $("workspaceFileList");
  if (!list) return;
  const q = ($("workspaceFileSearch")?.value || "").toLowerCase();
  workspaceFileFilter = q;
  const items = (workspaceFileEntries || []).filter((e) => {
    if (e.is_dir) return false;
    if (!q) return true;
    return String(e.name || "").toLowerCase().includes(q) || String(e.path || "").toLowerCase().includes(q);
  }).slice(0, 80);
  if (!items.length) {
    list.innerHTML = `<div class="skillsPopupEmpty"><strong>无匹配文件</strong></div>`;
    return;
  }
  list.innerHTML = items.map((e) =>
    `<button type="button" class="skillsPopupItem" data-path="${escapeAttr(e.path)}" data-name="${escapeAttr(e.name)}">
      <span class="skillsPopupItemInfo">
        <span class="skillsPopupItemName">${escapeHtml(e.name)}</span>
        <span class="skillsPopupItemMeta"><span class="skillsPopupItemPath">${escapeHtml(e.path)}</span></span>
      </span>
    </button>`).join("");
  list.querySelectorAll(".skillsPopupItem").forEach((btn) => {
    btn.onclick = () => {
      addPathAttachment(btn.dataset.path, btn.dataset.name);
      hideWorkspaceFilePicker();
      toast(`已引用 ${btn.dataset.name}`, "info");
    };
  });
}

async function populateComposerModelSelect() {
  const sel = $("composerModelSelect");
  if (!sel) return;
  const config = await loadComposerConfig();
  const models = config.models;
  const defaultModel = models.includes(config.defaultModel) ? config.defaultModel : (models[0] || "");
  if (composerModelOverride && !models.includes(composerModelOverride)) composerModelOverride = "";
  const current = composerModelOverride || (models.includes(sel.value) ? sel.value : defaultModel);
  sel.innerHTML = models.length
    ? models.map((model) => {
        const label = model === defaultModel ? `${model}（默认）` : model;
        return `<option value="${escapeHtml(model)}"${model === current ? " selected" : ""}>${escapeHtml(label)}</option>`;
      }).join("")
    : '<option value="">配置中没有启用模型</option>';
  sel.value = current;
  sel.disabled = models.length === 0 || composerControlsUnavailable();
  sel.onchange = () => {
    composerModelOverride = sel.value;
    populateComposerStrengthSelect();
  };
  populateComposerStrengthSelect();
}

async function populateComposerStrengthSelect() {
  const sel = $("composerStrengthSelect");
  if (!sel) return;
  const config = await loadComposerConfig();
  const model = $("composerModelSelect")?.value || config.defaultModel;
  const configured = Array.isArray(config.reasoningEfforts[model]) ? config.reasoningEfforts[model] : [];
  const efforts = configured.length ? configured : ["low", "medium", "high"];
  const defaultStrength = efforts.includes(config.defaultStrength) ? config.defaultStrength : "auto";
  if (composerStrengthOverride !== "auto" && composerStrengthOverride && !efforts.includes(composerStrengthOverride)) {
    composerStrengthOverride = "";
  }
  const current = composerStrengthOverride || defaultStrength;
  const labels = { low: "低", medium: "中", high: "高" };
  sel.innerHTML = `<option value="auto"${current === "auto" ? " selected" : ""}>自动（配置默认）</option>` + efforts.map((effort) => {
    const label = `${labels[effort] || effort}${effort === config.defaultStrength ? "（默认）" : ""}`;
    return `<option value="${escapeHtml(effort)}"${effort === current ? " selected" : ""}>${escapeHtml(label)}</option>`;
  }).join("");
  sel.value = current;
  sel.disabled = efforts.length === 0 || composerControlsUnavailable();
  sel.onchange = () => {
    composerStrengthOverride = sel.value;
  };
}

function composerControlsUnavailable() {
  const status = state.agentStatus;
  return status?.state !== "ready" || !!status?.busy || state.agentEngineState === "loading";
}

function composerConfigFromActiveProfile() {
  const profile = activeProfile();
  if (!profile) return { models: [], defaultModel: "", defaultStrength: "", reasoningEfforts: {} };
  const models = [];
  const reasoningEfforts = {};
  for (const model of profile.models || []) {
    const name = String(model?.name || model?.model || "").trim();
    if (!name || models.includes(name)) continue;
    models.push(name);
    if (Array.isArray(model.reasoning_efforts) && model.reasoning_efforts.length) {
      reasoningEfforts[name] = model.reasoning_efforts.filter(Boolean);
    }
  }
  return {
    models: models.sort(),
    defaultModel: profile.default_model || "",
    defaultStrength: profile.default_reasoning_effort || "",
    reasoningEfforts,
  };
}

async function loadComposerConfig() {
  if (composerConfigLoaded) return composerConfig;
  if (composerConfigPromise) return composerConfigPromise;
  composerConfigPromise = api("/api/grok-config-models").then((data) => {
    const models = Array.from(new Set((Array.isArray(data?.models) ? data.models : []).filter(Boolean))).sort();
    composerConfig = {
      models,
      defaultModel: String(data?.default_model || ""),
      defaultStrength: String(data?.default_reasoning_effort || ""),
      reasoningEfforts: data?.reasoning_efforts && typeof data.reasoning_efforts === "object" ? data.reasoning_efforts : {},
    };
    composerConfigLoaded = true;
    return composerConfig;
  }).catch(() => {
    composerConfig = composerConfigFromActiveProfile();
    composerConfigLoaded = true;
    return composerConfig;
  }).finally(() => {
    composerConfigPromise = null;
  });
  return composerConfigPromise;
}

// Close skills popup on click outside
document.addEventListener("click", (event) => {
  const popup = $("skillsPopup");
  if (popup && !popup.hidden && !event.target.closest("#skillsPopup") && event.target.id !== "chatInput") {
    hideSkillsPopup();
  }
});

// ===== Imagine (生图) =====
async function refreshImagineStatus() {
  const statusEl = $("imagineStatus");
  const emptyEl = $("imagineEmpty");
  try {
    const data = await api("/api/status");
    const ready = !!(data && data.imagine_ready);
    const count = (data && Number(data.imagine_accounts)) || 0;
    if (statusEl) {
      statusEl.hidden = false;
      statusEl.className = "muted tiny imagineStatus" + (ready ? " ok" : "");
      statusEl.textContent = ready
        ? `生图引擎就绪 · 可用账号 ${count} 个`
        : "生图引擎未就绪：registrar/cookies 中未找到可用账号";
    }
  } catch {
    if (statusEl) {
      statusEl.hidden = false;
      statusEl.className = "muted tiny imagineStatus";
      statusEl.textContent = "无法获取生图引擎状态";
    }
  }
  if (emptyEl) {
    const gallery = $("imagineGallery");
    emptyEl.hidden = !!(gallery && gallery.children.length > 0);
  }
}

async function generateImagine() {
  const promptEl = $("imaginePrompt");
  const modelEl = $("imagineModel");
  const ratioEl = $("imagineRatio");
  const statusEl = $("imagineStatus");
  const btn = $("imagineGenerateBtn");
  const prompt = (promptEl && promptEl.value || "").trim();
  if (!prompt) {
    toast("请先输入提示词", "error");
    if (promptEl) promptEl.focus();
    return;
  }
  const model = modelEl ? modelEl.value : "grok-imagine-image";
  const ratio = ratioEl ? ratioEl.value : "1:1";

  if (statusEl) {
    statusEl.hidden = false;
    statusEl.className = "muted tiny imagineStatus busy";
    statusEl.textContent = "正在生成，自动轮换账号中…";
  }
  setBusy(btn, true, "生成中…");
  try {
    const res = await fetch("/api/imagine/generate", {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify({ prompt, model, aspect_ratio: ratio }),
    });
    const data = await res.json().catch(() => ({}));
    if (!res.ok || !data.ok) {
      const msg = data.err_msg || data.error || res.statusText || "生图失败";
      throw new Error(msg);
    }
    const images = Array.isArray(data.images) ? data.images : [];
    if (images.length === 0) {
      throw new Error("未返回任何图片");
    }
    renderImagineImages(images, data);
    if (statusEl) {
      statusEl.hidden = false;
      statusEl.className = "muted tiny imagineStatus ok";
      const meta = [data.model_name, data.width && data.height ? `${data.width}x${data.height}` : null, data.account ? `账号 ${data.account}` : null]
        .filter(Boolean).join(" · ");
      statusEl.textContent = `生成成功（${images.length} 张）${meta ? " · " + meta : ""}`;
    }
    toast(`生成成功，${images.length} 张图片`, "success");
  } catch (err) {
    if (statusEl) {
      statusEl.hidden = false;
      statusEl.className = "muted tiny imagineStatus error";
      statusEl.textContent = "生成失败：" + (err.message || err);
    }
    toast("生图失败：" + (err.message || err), "error");
  } finally {
    setBusy(btn, false);
  }
}

function renderImagineImages(images, data) {
  const gallery = $("imagineGallery");
  const emptyEl = $("imagineEmpty");
  if (!gallery) return;
  const frag = document.createDocumentFragment();
  for (const url of images) {
    const card = document.createElement("div");
    card.className = "imagineCard";

    const img = document.createElement("img");
    img.src = url;
    img.alt = (data && data.model_name ? data.model_name + " " : "") + "生成图片";
    img.loading = "lazy";
    card.appendChild(img);

    const actions = document.createElement("div");
    actions.className = "imagineCardActions";
    const dl = document.createElement("button");
    dl.type = "button";
    dl.className = "btn sm";
    dl.textContent = "下载";
    dl.onclick = (e) => {
      e.stopPropagation();
      downloadImagine(url);
    };
    actions.appendChild(dl);
    card.appendChild(actions);

    card.onclick = () => openImagineLightbox(url);
    frag.appendChild(card);
  }
  gallery.appendChild(frag);
  if (emptyEl) emptyEl.hidden = true;
}

function downloadImagine(url) {
  const a = document.createElement("a");
  a.href = url;
  a.download = url.split("/").pop() || "imagine.png";
  document.body.appendChild(a);
  a.click();
  a.remove();
}

function openImagineLightbox(url) {
  const existing = $("imagineLightbox");
  if (existing) existing.remove();
  const box = document.createElement("div");
  box.className = "imagineLightbox";
  box.id = "imagineLightbox";

  const bar = document.createElement("div");
  bar.className = "imagineLightboxBar";
  const dl = document.createElement("button");
  dl.type = "button";
  dl.className = "btn sm";
  dl.textContent = "下载";
  dl.onclick = (e) => { e.stopPropagation(); downloadImagine(url); };
  bar.appendChild(dl);
  const close = document.createElement("button");
  close.type = "button";
  close.className = "btn sm";
  close.textContent = "关闭";
  close.onclick = (e) => { e.stopPropagation(); box.remove(); };
  bar.appendChild(close);
  box.appendChild(bar);

  const img = document.createElement("img");
  img.src = url;
  img.alt = "预览";
  box.appendChild(img);

  box.onclick = () => box.remove();
  document.body.appendChild(box);
}

function clearImagineGallery() {
  const gallery = $("imagineGallery");
  if (gallery) gallery.innerHTML = "";
  const emptyEl = $("imagineEmpty");
  if (emptyEl) emptyEl.hidden = false;
}
