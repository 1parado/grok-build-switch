const state = {
  profiles: [],
  settings: null,
  status: null,
  grokAuth: null,
  grokPool: null,
  registrar: null,
  lanAccess: null,
  availableModels: [],
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
  agentPermission: null,
  lastUserMessage: "",
  chatStickToBottom: true,
  agentAutoRestoring: false,
  pendingAttachments: [],
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
let agentActiveAssistant = null;
let agentActiveThought = null;
let agentRetryNotice = null;
let agentSessionSearchTimer = null;
let mermaidReady = false;
let mermaidRenderID = 0;
let chatLayoutResizeTimer = null;
let lastAssistantMessageEl = null;
const chatNodes = []; // ordered list of { id, article, el } for navigation jumps

const HISTORY_PAGE_SIZE = 60; // messages rendered per incremental window
let pendingHistory = [];     // full message list for the current restored session
let historyCap = HISTORY_PAGE_SIZE; // how many tail messages are currently rendered
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
  };
}

async function api(path, options = {}) {
  const res = await fetch(path, {
    headers: { "Content-Type": "application/json" },
    ...options,
  });
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
  // Coerce to strict boolean for UI.
  if (state.status && typeof state.status.config_matches_active !== "boolean") {
    state.status.config_matches_active = true;
  }
  renderDrift();
  renderEmptyState();
  renderProfiles();
  renderBackups(backups);
  renderSettings(settings);
  renderLANAccess(lanAccess);
  renderGrokAuth(grokAuth);
  renderGrokPool(grokPool);
  renderRegistrar(registrar);
  syncAdvancedUI();
  const detail = [];
  if (state.status?.config_path) detail.push(state.status.config_path);
  if (state.status?.port) detail.push(`端口 ${state.status.port}`);
  if ($("statusDetail")) $("statusDetail").textContent = detail.join(" · ");
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
  if (chat) {
    chat.hidden = name !== "chat";
    chat.style.display = name === "chat" ? "" : "none";
  }
  if ($("navHomeBtn")) $("navHomeBtn").hidden = name === "home";
  document.querySelectorAll("[data-home-only]").forEach((el) => {
    el.hidden = name !== "home";
  });
  // Keep header add/import only on home list.
  if ($("headerSubtitle")) {
    $("headerSubtitle").textContent =
      name === "settings" ? "设置" : name === "edit" ? ( $("profileId")?.value ? "编辑供应商" : "添加供应商") : name === "chat" ? "对话" : "供应商";
  }
  if (name === "settings") {
    loadConfigEditor().catch((err) => toast(err.message, "error"));
  }
  if (name === "chat") {
    openAgentView().catch((err) => toast(err.message, "error"));
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
  updateConversationIdentity();
  applyStoredChatPanelWidths();
  bindChatScrollTracking();
  updateChatJumpBottomBtn();
  bindChatExtras();
  if (window.matchMedia("(min-width: 821px)").matches) {
    const shell = $("viewChat")?.querySelector(".nativeChatShell");
    shell?.classList.toggle("sidebarCollapsed", localStorage.getItem("gs_chat_sidebar_hidden") === "1");
    if (window.matchMedia("(min-width: 1181px)").matches) {
      shell?.classList.toggle("contextCollapsed", localStorage.getItem("gs_chat_context_hidden") === "1");
    }
  }
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
  renderAgentSessionList();
  return state.agentSessions;
}

function renderAgentSessionList() {
  const list = $("agentSessionList");
  if (!list) return;
  list.innerHTML = "";
  if ($("agentSessionCount")) $("agentSessionCount").textContent = String(state.agentSessions.length);
  if (!state.agentSessions.length) {
    list.innerHTML = `<p class="sessionListEmpty">没有匹配的历史会话</p>`;
    return;
  }
  for (const session of state.agentSessions) {
    const button = document.createElement("div");
    button.className = `sessionItem${session.id === state.activeAgentSession?.id ? " active" : ""}`;
    button.role = "button";
    button.tabIndex = 0;
    button.dataset.sessionId = session.id;
    const title = document.createElement("span");
    title.className = "sessionItemTitle";
    title.textContent = session.title || "未命名会话";
    const meta = document.createElement("span");
    meta.className = "sessionItemMeta";
    meta.textContent = [formatSessionTime(session.updated_at), session.model].filter(Boolean).join(" · ");
    const path = document.createElement("span");
    path.className = "sessionItemPath";
    path.textContent = session.cwd || "";
    const renameBtn = document.createElement("button");
    renameBtn.type = "button";
    renameBtn.className = "sessionRenameBtn";
    renameBtn.title = "重命名会话";
    renameBtn.setAttribute("aria-label", `重命名会话 ${session.title || ""}`);
    renameBtn.textContent = "✎";
    renameBtn.onclick = (event) => {
      event.stopPropagation();
      startRenameSession(session);
    };
    button.append(title, meta, path, renameBtn);
    const activate = async () => {
      try {
        button.setAttribute("aria-busy", "true");
        button.classList.add("busy");
        await resumeAgentSession(session);
      } catch (err) {
        toast(err.message || String(err), "error");
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
    list.append(button);
  }
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
    renderAgentSessionList();
    toast(title ? `已重命名为「${title}」` : "已恢复默认会话名", "success");
  } catch (err) {
    toast(err.message || String(err), "error");
  }
}

async function resumeAgentSession(session) {
  const history = await api(`/api/agent/sessions/${encodeURIComponent(session.id)}`);
  state.activeAgentSession = history.session || session;
  if ($("agentCwd")) $("agentCwd").value = state.activeAgentSession.cwd || "";
  clearAgentTranscript(false);
  renderStoredHistory(history.messages || []);
  setAgentEngineState("loading", "正在恢复引擎上下文…");
  updateConversationIdentity();
  renderAgentSessionList();
  connectAgentSocket();
  let status;
  try {
    status = await api("/api/agent/session/load", {
      method: "POST",
      body: JSON.stringify({
        cwd: state.activeAgentSession.cwd,
        session_id: state.activeAgentSession.id,
        always_approve: $("agentAlwaysApprove").checked,
      }),
    });
  } catch (err) {
    if (handleSessionLoadFallback(err)) {
      closeNativeChatPanels();
      scrollChatToBottom();
      return false;
    }
    setAgentEngineState("readonly", "仅显示本地历史，引擎上下文恢复失败。请开启新对话后再发送消息。");
    const latestStatus = await api("/api/agent/status").catch(() => state.agentStatus);
    if (latestStatus) renderAgentStatus(latestStatus);
    throw err;
  }
  setAgentEngineState("attached");
  state.agentStatus = status;
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
  if (mode !== "readonly") state.agentFallbackSessionReady = false;
  const banner = $("agentEngineBanner");
  if (!banner) return;
  const visible = mode === "loading" || mode === "readonly";
  banner.hidden = !visible;
  banner.dataset.state = mode;
  if ($("agentEngineBannerText")) $("agentEngineBannerText").textContent = message;
  if ($("agentReadonlyNewBtn")) $("agentReadonlyNewBtn").hidden = mode !== "readonly";
}

function handleSessionLoadFallback(err) {
  if (err?.code !== "session_load_overflow") return false;
  const status = err.data?.status;
  if (status) {
    state.agentStatus = status;
    renderAgentStatus(status);
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
  if ($("activeChatPath")) $("activeChatPath").textContent = session?.cwd || state.agentStatus?.cwd || $("agentCwd")?.value || "尚未选择工作目录";
  if ($("contextSessionId")) $("contextSessionId").textContent = session?.id || state.agentStatus?.session_id || "—";
  renderAgentSessionList();
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
  const loading = state.agentEngineState === "loading";
  const composerReady = stateName === "ready" && !loading;
  if ($("chatInput")) {
    $("chatInput").disabled = !composerReady || busy;
    $("chatInput").placeholder = busy
      ? "正在生成…可点击停止"
      : composerReady
        ? "询问代码、规划任务或继续这段对话…"
        : "启动 Agent 后即可发送消息";
  }
  if ($("chatSendBtn")) {
    $("chatSendBtn").hidden = busy;
    const hasContent = !!$("chatInput")?.value.trim() || state.pendingAttachments.length > 0;
    $("chatSendBtn").disabled = !composerReady || busy || !hasContent;
  }
  if ($("chatAttachBtn")) $("chatAttachBtn").disabled = !composerReady || busy;
  if ($("chatStopBtn")) {
    $("chatStopBtn").hidden = !busy;
    $("chatStopBtn").disabled = !busy;
  }
  const hint = $("composerHint");
  if (hint) {
    hint.classList.toggle("composerBusyHint", busy);
    if (busy) hint.textContent = "正在生成回复…点击 ■ 停止 · Esc 也可停止";
    else if (status.session_auto_approve) hint.textContent = "本会话已自动允许工具 · Enter 发送 · Shift + Enter 换行";
    else hint.textContent = "Enter 发送 · Shift + Enter 换行";
  }
  refreshMessageActionButtons();
  if (!status.available && status.error) {
    const empty = $("chatEmpty");
    if (empty) {
      empty.querySelector("strong").textContent = "未找到 Grok Build";
      empty.querySelector("span").textContent = status.error;
    }
  }
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
      appendAssistantChunk(event.text || "");
      break;
    case "thought_chunk":
      appendThoughtChunk(event.text || "");
      break;
    case "tool_call":
    case "tool_update":
      renderAgentTool(event.tool || {}, event.type === "tool_update");
      break;
    case "permission_request":
      showAgentPermission(event.permission);
      break;
    case "retry_state":
      renderAgentRetry(event.retry);
      break;
    case "turn_done":
      finalizeAssistantMessage();
      agentActiveAssistant = null;
      agentActiveThought = null;
      agentRetryNotice = null;
      if (event.stop_reason === "cancelled") {
        appendAgentNotice("已停止生成");
      }
      renderAgentStatus({ ...state.agentStatus, state: "ready", running: true, busy: false, error: "" });
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
  messages.innerHTML = showEmpty
    ? `<div id="chatEmpty" class="chatEmpty"><img src="/icon.svg" alt="" class="chatEmptyMark chatEmptyIcon"></div>`
    : "";
  agentTools.clear();
  if ($("toolActivityCount")) $("toolActivityCount").textContent = "0";
  if ($("toolActivityList")) $("toolActivityList").innerHTML = "<span>暂无工具活动</span>";
  agentActiveAssistant = null;
  agentActiveThought = null;
  agentRetryNotice = null;
  lastAssistantMessageEl = null;
  state.agentPermission = null;
  state.lastUserMessage = "";
  state.chatStickToBottom = true;
  resetChatNodes();
  clearChatAttachments();
  if ($("permissionBar")) $("permissionBar").hidden = true;
  updateContextUsage();
  updateChatJumpBottomBtn();
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

// --- 长历史增量加载 ---
function renderStoredHistory(messages) {
  pendingHistory = Array.isArray(messages) ? messages.slice() : [];
  historyCap = HISTORY_PAGE_SIZE;
  renderHistoryWindow();
}

function renderHistoryWindow() {
  const messages = $("chatMessages");
  if (!messages) return;
  // Keep the user's reading position stable when prepending older messages.
  const wasLoadingMore = state._loadingMoreHistory;
  state.chatStickToBottom = !wasLoadingMore;
  messages.innerHTML = "";
  agentTools.clear();
  if ($("toolActivityCount")) $("toolActivityCount").textContent = "0";
  if ($("toolActivityList")) $("toolActivityList").innerHTML = "<span>暂无工具活动</span>";
  agentActiveAssistant = null;
  agentActiveThought = null;
  agentRetryNotice = null;
  lastAssistantMessageEl = null;
  resetChatNodes();

  const total = pendingHistory.length;
  const start = Math.max(0, total - historyCap);
  const slice = pendingHistory.slice(start);
  for (const message of slice) {
    appendHistoryMessage(message);
  }
  agentActiveAssistant = null;
  agentActiveThought = null;
  // rebuild nodes from DOM so indices reflect the visible (possibly capped) window
  rebuildChatNodesFromDom();
  ensureHistorySentinel(start, total);

  // Track the last user message for regenerate / edit flows.
  let lastUserText = "";
  for (let i = pendingHistory.length - 1; i >= 0; i--) {
    if (pendingHistory[i].role === "user") { lastUserText = pendingHistory[i].content || ""; break; }
  }
  state.lastUserMessage = lastUserText;
  refreshMessageActionButtons();

  if (wasLoadingMore) {
    messages.scrollTop = 0; // show the newly prepended older messages
    state._loadingMoreHistory = false;
  } else {
    forceScrollChatToBottom();
  }
  updateContextUsage();
}

function appendHistoryMessage(message) {
  switch (message.role) {
    case "user":
      appendChatMessage("user", message.content || "", "", true);
      break;
    case "assistant":
      appendChatMessage("assistant", message.content || "", message.model || state.activeAgentSession?.model || "", true);
      break;
    case "thought":
      appendThoughtChunk(message.content || "");
      agentActiveThought = null;
      break;
    case "tool":
      renderAgentTool(message.tool || {}, false);
      break;
    case "tool_result":
      renderAgentTool({ ...(message.tool || {}), raw_output: message.content || "", status: "completed" }, true);
      break;
  }
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
    messages.prepend(sentinel);
    sentinel.onclick = () => loadOlderHistory();
  }
  sentinel.textContent = `↑ 加载更早 ${start} 条会话`;
  if (sentinel !== messages.firstChild) messages.prepend(sentinel);
}

function loadOlderHistory() {
  if (!pendingHistory.length) return;
  const renderedStart = Math.max(0, pendingHistory.length - historyCap);
  if (renderedStart <= 0) return;
  state._loadingMoreHistory = true;
  historyCap += HISTORY_PAGE_SIZE;
  renderHistoryWindow();
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
  sendBtn.onclick = () => resendEditedUserMessage(article, textarea.value).catch((err) => toast(err.message || String(err), "error"));
  actions.append(cancelBtn, sendBtn);
  body.replaceChildren(textarea, actions);
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
  updateContextUsage();
  agentSocket.send(JSON.stringify({ type: "user_message", text }));
  renderAgentStatus({ ...state.agentStatus, state: "busy", running: true, busy: true });
  forceScrollChatToBottom();
}

// --- 上下文用量估算（按字符数 / 4 粗估 token） ---
function updateContextUsage() {
  const text = $("contextUsageText");
  const fill = $("contextUsageFill");
  const bar = $("contextUsageBar");
  if (!text || !fill) return;
  const messages = $("chatMessages");
  let chars = 0;
  let count = 0;
  if (messages) {
    messages.querySelectorAll(".chatMessage[data-role='user'], .chatMessage[data-role='assistant']").forEach((article) => {
      chars += (article._rawText || "").length;
      count += 1;
    });
  }
  const tokens = Math.max(0, Math.round(chars / 4));
  const CONTEXT_GUESS = 200000; // rough modern context window ceiling for the bar
  const ratio = Math.min(1, tokens / CONTEXT_GUESS);
  fill.style.width = `${(ratio * 100).toFixed(1)}%`;
  fill.classList.toggle("warn", ratio >= 0.7 && ratio < 0.9);
  fill.classList.toggle("danger", ratio >= 0.9);
  if (count === 0) {
    text.textContent = "尚未发送消息";
  } else {
    text.textContent = `≈ ${tokens.toLocaleString()} tokens · ${count} 条消息（估算，非精确）`;
  }
  if (bar) bar.title = `字符 ${chars.toLocaleString()} · 估算 token ${tokens.toLocaleString()}`;
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

function createChatMessage(role, text, model = "", final = false, attachments = null) {
  const article = document.createElement("article");
  article.className = `chatMessage ${role}`;
  article._rawText = text || "";
  article.dataset.role = role;
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
      regenBtn.onclick = () => regenerateLastAssistant(article).catch((err) => toast(err.message || String(err), "error"));
      actions.append(regenBtn);
      lastAssistantMessageEl = article;
    } else {
      const editBtn = document.createElement("button");
      editBtn.type = "button";
      editBtn.className = "messageActionBtn";
      editBtn.dataset.action = "edit";
      editBtn.textContent = "编辑";
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

function appendChatMessage(role, text, model = "", final = false, attachments = null) {
  removeChatEmpty();
  const article = createChatMessage(role, text, model, final, attachments);
  $("chatMessages").append(article);
  renderMessageMarkdown(article, final);
  if (role === "user" && Array.isArray(attachments) && attachments.length) {
    renderMessageAttachments(article, attachments);
  }
  if (role === "user") addChatNodeFor(article);
  updateContextUsage();
  scrollChatToBottom();
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

function appendAssistantChunk(text) {
  if (!text) return;
  markAgentRetryRecovered();
  document.querySelectorAll(".chatMessage.system").forEach((notice) => {
    if ((notice._rawText || "").includes("正在重新生成")) notice.remove();
  });
  if (!agentActiveAssistant || !agentActiveAssistant.isConnected) {
    agentActiveAssistant = appendChatMessage("assistant", "", state.agentStatus?.model || state.activeAgentSession?.model || "");
  }
  agentActiveAssistant._rawText = (agentActiveAssistant._rawText || "") + text;
  scheduleMessageMarkdown(agentActiveAssistant);
  scheduleContextUsageUpdate();
  scrollChatToBottom();
}

let contextUsageTimer = null;
function scheduleContextUsageUpdate() {
  clearTimeout(contextUsageTimer);
  contextUsageTimer = setTimeout(updateContextUsage, 300);
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
  updateContextUsage();
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

async function renderMermaidBlocks(root) {
  const mermaidAPI = window.mermaid;
  if (!mermaidAPI?.render) return;
  if (!mermaidReady) {
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
  const blocks = [...root.querySelectorAll("pre > code.language-mermaid")];
  for (const code of blocks) {
    const pre = code.parentElement;
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
  removeChatEmpty();
  if (!agentActiveThought || !agentActiveThought.isConnected) {
    const details = document.createElement("details");
    details.className = "agentThought";
    const summary = document.createElement("summary");
    summary.textContent = "思考过程";
    const body = document.createElement("pre");
    details.append(summary, body);
    $("chatMessages").append(details);
    agentActiveThought = details;
  }
  agentActiveThought.querySelector("pre").textContent += text;
  scrollChatToBottom();
}

function renderAgentTool(tool, isUpdate) {
  removeChatEmpty();
  const id = tool.id || `tool-${agentTools.size + 1}`;
  let details = agentTools.get(id);
  if (!details) {
    details = document.createElement("details");
    details.className = "agentTool";
    details.innerHTML = `<summary><span class="agentToolSummaryMain"><span class="agentToolTitle"></span><span class="agentToolStatus"></span></span></summary><pre class="agentToolDetail"></pre>`;
    $("chatMessages").append(details);
    agentTools.set(id, details);
  }
  const title = tool.title || details.querySelector(".agentToolTitle").textContent || "工具调用";
  const status = tool.status || details.dataset.status || (isUpdate ? "更新" : "等待");
  details.dataset.status = status;
  details.querySelector(".agentToolTitle").textContent = title;
  details.querySelector(".agentToolStatus").textContent = agentToolStatusLabel(status);
  const payload = tool.raw_output ?? tool.raw_input;
  const pre = details.querySelector("pre.agentToolDetail");
  if (payload != null && pre) {
    pre.textContent = formatAgentPayload(payload);
    pre.hidden = false;
  } else if (pre && !pre.textContent) {
    pre.textContent = "展开查看输入/输出";
  }
  // Keep collapsed by default; only auto-open failures lightly via status color.
  renderToolActivity(tool, id, title, status);
  scrollChatToBottom();
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
  if ($("permissionRemember")) $("permissionRemember").checked = false;
  if ($("permissionAllowBtn")) {
    $("permissionAllowBtn").textContent = "允许一次";
  }
  $("permissionBar").hidden = false;
  scrollChatToBottom(true);
}

function syncPermissionAllowLabel() {
  const remember = !!$("permissionRemember")?.checked;
  if ($("permissionAllowBtn")) {
    $("permissionAllowBtn").textContent = remember ? "允许并记住" : "允许一次";
  }
}

function respondAgentPermission(allow) {
  const permission = state.agentPermission;
  if (!permission) return;
  if (!agentSocket || agentSocket.readyState !== WebSocket.OPEN) {
    toast("对话连接已断开，请稍后重试", "error");
    return;
  }
  const remember = allow && !!$("permissionRemember")?.checked;
  agentSocket.send(JSON.stringify({
    type: "permission_response",
    request_id: permission.request_id,
    allow,
    remember,
  }));
  state.agentPermission = null;
  $("permissionBar").hidden = true;
  if (allow && remember) {
    renderAgentStatus({ ...state.agentStatus, session_auto_approve: true });
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

async function newAgentSession() {
  const cwd = $("agentCwd").value.trim();
  if (!cwd) throw new Error("请提供工作目录");
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
  // Drop the last assistant bubble for a clean regen UX (agent still has prior context).
  if (article?.isConnected && article.dataset.role === "assistant") {
    article.remove();
  }
  if (lastAssistantMessageEl === article) lastAssistantMessageEl = null;
  agentActiveAssistant = null;
  agentActiveThought = null;
  agentRetryNotice = null;
  agentSocket.send(JSON.stringify({ type: "user_message", text }));
  renderAgentStatus({ ...state.agentStatus, state: "busy", running: true, busy: true });
  appendAgentNotice("正在重新生成…");
  forceScrollChatToBottom();
}

// --- 附件 / 截图上传 ---
const TEXT_FILE_MAX = 1_000_000;     // 1 MB cap for text attachments
const IMAGE_FILE_MAX = 16_000_000;   // 16 MB cap for image attachments

function readImageFile(file) {
  return new Promise((resolve, reject) => {
    if (file.size > IMAGE_FILE_MAX) { reject(new Error(`图片过大（${(file.size / 1_000_000).toFixed(1)} MB），上限 16 MB`)); return; }
    const reader = new FileReader();
    reader.onerror = () => reject(new Error("读取图片失败"));
    reader.onload = () => {
      const dataUrl = String(reader.result || "");
      const match = dataUrl.match(/^data:([^;]+);base64,(.*)$/s);
      if (!match) { reject(new Error("图片格式无法解析")); return; }
      resolve({ kind: "image", data: match[2], dataUrl, mimeType: match[1], name: file.name || "image" });
    };
    reader.readAsDataURL(file);
  });
}

function readTextFile(file) {
  return new Promise((resolve, reject) => {
    const reader = new FileReader();
    reader.onerror = () => reject(new Error("读取文件失败"));
    reader.onload = () => {
      let text = String(reader.result || "");
      const truncated = file.size > TEXT_FILE_MAX;
      if (truncated) text = text.slice(0, TEXT_FILE_MAX) + "\n…（文件过大，已截断为前 1 MB）";
      resolve({ kind: "text_file", name: file.name || "file.txt", text, truncated });
    };
    reader.readAsText(file.slice(0, TEXT_FILE_MAX + 65536), "utf-8");
  });
}

async function handleChatFiles(fileList) {
  const files = Array.from(fileList || []);
  if (!files.length) return;
  for (const file of files) {
    try {
      if (file.type.startsWith("image/")) {
        state.pendingAttachments.push(await readImageFile(file));
      } else {
        state.pendingAttachments.push(await readTextFile(file));
      }
    } catch (err) {
      toast(`${file.name || "附件"}：${err.message || String(err)}`, "error");
    }
  }
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
  if (!article?.isConnected || !Array.isArray(attachments) || !attachments.length) return;
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

function buildOutboundAttachments() {
  return state.pendingAttachments.map((att) => {
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
  if (state.agentEngineState === "readonly") {
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
  updateContextUsage();
  forceScrollChatToBottom();
  agentSocket.send(JSON.stringify({ type: "user_message", text, attachments }));
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
					<p class="providerUrl">${official ? "grok.com / auth.json" : escapeHtml(profile.base_url || hostOf(profile.base_url))}</p>
					<p class="providerMeta">${meta}</p>
				</div>
				<div class="providerFlags">
					${profile.pinned ? '<span class="pinBadge">已置顶</span>' : ""}
					${profile.is_active ? '<span class="badge">当前启用</span>' : ""}
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
  $("lanAccessEnabled").checked = !!settings.lan_access_enabled;
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
    email_provider: provider,
    default_domains: (provider === "cloudflare" ? $("registrarCloudflareDomains").value : $("registrarDefaultDomains").value).trim(),
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
  // Non-default email modes need advanced fields; expand so users notice.
  const advanced = $("registrarAdvanced");
  if (advanced && !isCloudflare && !advanced.open) {
    advanced.open = true;
  }
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
    }
    if ($("registrarProgressBar")) {
      $("registrarProgressBar").max = Math.max(total, 1);
      $("registrarProgressBar").value = Math.min(completed, total);
    }
    if ($("registrarLog")) {
      const lines = job.log_tail || [];
      $("registrarLog").textContent = lines.length ? lines.join("\n") : "等待日志…";
      $("registrarLog").scrollTop = $("registrarLog").scrollHeight;
    }
    if (!active && job.id && registrarTerminalNotice !== job.id) {
      registrarTerminalNotice = job.id;
      if (job.status === "succeeded") toast("注册任务已完成，账号已进入号池", "success");
      if (job.status === "failed") toast(job.error || "注册任务失败", "error");
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
  renderGrokPoolAccounts(pool?.accounts || []);
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

function renderGrokPoolAccounts(accounts) {
  const container = $("grokPoolAccounts");
  if (!container) return;
  container.innerHTML = "";
  if (!accounts.length) {
    container.innerHTML = `<p class="muted tiny">可一次选择多个 CPA xai-*.json；也支持 Grok CLI auth.json。</p>`;
    return;
  }
  accounts.forEach((account) => {
    const classification = account.classification || "uninspected";
    const el = document.createElement("div");
    el.className = `poolAccount ${escapeAttr(classification)}`;
    const title = account.email || account.file_name || account.id;
    const inspected = account.last_inspected ? new Date(account.last_inspected).toLocaleString() : "未巡检";
    const errorParts = [];
    if (account.error_code) errorParts.push(`错误码：${account.error_code}`);
    if (account.error_message) errorParts.push(`原因：${account.error_message}`);
    const errorDetail = errorParts.length
      ? `<p class="poolAccountError">${escapeHtml(errorParts.join("\n"))}</p>`
      : "";
    el.innerHTML = `
      <div class="poolAccountTop">
        <div class="poolAccountTitle">
          <strong>${escapeHtml(title)}</strong>
          <p class="poolAccountMeta">${escapeHtml(account.file_name || account.id)} · ${escapeHtml(account.model || "未选模型")} · ${escapeHtml(inspected)}</p>
        </div>
        <span class="badge">${account.disabled ? "手动禁用 · " : ""}${escapeHtml(GROK_POOL_CLASS_LABELS[classification] || classification)}</span>
      </div>
      <p class="poolAccountReason">${escapeHtml(account.reason || "等待巡检")}${account.http_status ? ` · HTTP ${account.http_status}` : ""}</p>
      ${errorDetail}
      <div class="inlineActions" style="margin-top:10px">
        <button type="button" class="btn sm" data-action="toggle">${account.disabled ? "启用" : "禁用"}</button>
        <button type="button" class="btn sm danger" data-action="delete">删除</button>
      </div>
    `;
    const toggle = el.querySelector('[data-action="toggle"]');
    toggle.onclick = () => run(async () => {
      await api(`/api/grok-pool/accounts/${encodeURIComponent(account.id)}`, {
        method: "PATCH",
        body: JSON.stringify({ disabled: !account.disabled }),
      });
      await loadGrokPool();
    }, { button: toggle, busyLabel: "处理中…", success: account.disabled ? "账号已启用" : "账号已禁用" });
    const remove = el.querySelector('[data-action="delete"]');
    remove.onclick = () => run(async () => {
      if (!confirm(`删除号池账号 ${title}？此操作会删除 grok_switch 保存的凭据副本。`)) return false;
      await api(`/api/grok-pool/accounts/${encodeURIComponent(account.id)}`, { method: "DELETE" });
      await refreshAll();
    }, { button: remove, busyLabel: "删除中…", success: "号池账号已删除" });
    container.appendChild(el);
  });
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
    models: (source.models || []).map((m) => ({ ...m, extra_headers: { ...(m.extra_headers || {}) } })),
  };
  fillForm(clone);
  toast("已载入副本，保存后生效", "info");
}

function stripSecrets(profile, includeKey) {
  const out = {
    name: profile.name,
    template: profile.template || templateValue(profile),
    upstream_format: profile.upstream_format,
    base_url: profile.base_url,
    default_model: profile.default_model,
    default_reasoning_effort: profile.default_reasoning_effort || "high",
    web_search_model: profile.web_search_model,
    subagents_models: subagentsModelsOf(profile),
    available_models: profile.available_models || [],
    models: (profile.models || []).map((m) => {
      const item = {
        name: m.name,
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
  const names = unique(readEnabledModelNames());
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
    if (current && !names.includes(current)) {
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
      <strong>${escapeHtml(model.name || model.model || "新模型")}</strong>
      <div class="inlineActions">
        <button type="button" class="btn sm" data-action="test-model">测试连通</button>
        <button type="button" class="btn sm danger" data-action="remove-model">删除</button>
      </div>
    </div>
    <p class="muted tiny modelProbeStatus" data-field="probe_status" hidden></p>
    <div class="modelCardGrid">
      <label class="field">名称
        <input data-field="name" class="mono" value="${escapeAttr(model.name || "")}" placeholder="配置中的模型名">
      </label>
      <label class="field">Model
        <input data-field="model" class="mono" value="${escapeAttr(model.model || "")}" placeholder="上游模型 ID">
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
  const onFieldChange = () => {
    card.querySelector("strong").textContent = nameInput.value.trim() || modelInput.value.trim() || "新模型";
    renderModelSelect();
    syncEnabledModelList();
  };
  nameInput.addEventListener("input", onFieldChange);
  modelInput.addEventListener("input", onFieldChange);
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
  if (profile.id) {
    return await api(`/api/profiles/${profile.id}`, { method: "PUT", body: JSON.stringify(profile) });
  }
  return await api("/api/profiles", { method: "POST", body: JSON.stringify(profile) });
}

// Navigation
$("navHomeBtn").onclick = () => showView("home");
$("navSettingsBtn").onclick = () => showView("settings");
$("backFromEditBtn").onclick = () => showView("home");
$("backFromSettingsBtn").onclick = () => showView("home");
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
$("agentNewSessionBtn").onclick = () => run(newAgentSession, { button: $("agentNewSessionBtn"), busyLabel: "创建中…" });
$("agentStopBtn").onclick = () => run(stopAgent, { button: $("agentStopBtn"), busyLabel: "停止中…" });
$("agentSessionSearch").oninput = () => {
  clearTimeout(agentSessionSearchTimer);
  agentSessionSearchTimer = setTimeout(() => loadAgentSessions().catch((err) => toast(err.message, "error")), 180);
};
$("openSessionSidebarBtn").onclick = () => toggleSessionSidebar();
$("closeSessionSidebarBtn").onclick = () => toggleSessionSidebar(false);
$("openContextRailBtn").onclick = () => toggleContextRail();
$("closeContextRailBtn").onclick = () => toggleContextRail(false);
$("nativeChatScrim").onclick = closeNativeChatPanels;
bindChatPanelResizer("left");
bindChatPanelResizer("right");
$("agentCwd").oninput = updateConversationIdentity;
$("permissionAllowBtn").onclick = () => respondAgentPermission(true);
$("permissionRejectBtn").onclick = () => respondAgentPermission(false);
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
$("agentReadonlyNewBtn").onclick = () => run(newAgentSession, { button: $("agentReadonlyNewBtn"), busyLabel: "创建中…" });
$("chatInput").oninput = () => renderAgentStatus(state.agentStatus);
$("chatInput").onkeydown = (event) => {
  if (event.key === "Enter" && !event.shiftKey) {
    event.preventDefault();
    $("chatComposer").requestSubmit();
  }
};
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
  };
}
if ($("layoutCardBtn")) {
  $("layoutCardBtn").onclick = () => {
    state.layout = "card";
    applyLayoutUI();
    renderProfiles();
  };
}
if ($("layoutListBtn")) {
  $("layoutListBtn").onclick = () => {
    state.layout = "list";
    applyLayoutUI();
    renderProfiles();
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
  renderModelSelect();
  if ($("connectBlock")) $("connectBlock").open = true;
  showConnectionStatus(true, `已获取 ${result.models.length} 个模型`);
  toast(`获取到 ${result.models.length} 个模型`, "success");
}, { button: $("fetchModelsBtn"), busyLabel: "拉取中…" });

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

$("settingsForm").onsubmit = (event) => {
  event.preventDefault();
  run(async () => {
    const settings = {
      ...state.settings,
      autostart: $("autostart").checked,
      silent_autostart: $("silentAutostart").checked,
      auto_open_browser: $("autoOpenBrowser").checked,
      lan_access_enabled: $("lanAccessEnabled").checked,
      theme: "light",
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

["registrarBrowserPath", "registrarBrowserMode", "registrarProxyUrl", "registrarEmailProvider",
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
  }, { button, busyLabel: `${verb}中…`, success: `已批量${verb} ${abnormal.length} 个异常账号` });
}

$("batchDisableGrokPoolBtn").onclick = () => bulkGrokPoolAction("disable", $("batchDisableGrokPoolBtn"));
$("batchDeleteGrokPoolBtn").onclick = () => bulkGrokPoolAction("delete", $("batchDeleteGrokPoolBtn"));

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
refreshAll()
  .then(() => {
    loadLatestCpaMint().catch((err) => toast(err.message, "error"));
  })
  .catch((err) => toast(err.message, "error"));
