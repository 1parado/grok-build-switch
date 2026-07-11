const state = {
  profiles: [],
  settings: null,
  status: null,
  availableModels: [],
  backups: [],
  showAdvanced: false,
  view: "home",
  layout: localStorage.getItem("gs_layout") || "card",
  search: "",
};

const $ = (id) => document.getElementById(id);
let toastTimer = null;
let refreshTimer = null;

const TEMPLATES = {
  openai: {
    name: "OpenAI 兼容",
    upstream_format: "openai_chat",
    base_url: "https://api.openai.com/v1",
    default_model: "",
    web_search_model: "",
    subagents_default_model: "",
    models: [],
    available_models: [],
  },
  anthropic: {
    name: "Anthropic",
    upstream_format: "anthropic",
    base_url: "https://api.anthropic.com",
    default_model: "",
    web_search_model: "",
    subagents_default_model: "",
    models: [],
    available_models: [],
  },
  ollama: {
    name: "Ollama 本地",
    upstream_format: "openai_chat",
    base_url: "http://127.0.0.1:11434/v1",
    api_key: "ollama",
    default_model: "",
    web_search_model: "",
    subagents_default_model: "",
    models: [],
    available_models: [],
  },
  openrouter: {
    name: "OpenRouter",
    upstream_format: "openai_chat",
    base_url: "https://openrouter.ai/api/v1",
    default_model: "",
    web_search_model: "",
    subagents_default_model: "",
    models: [],
    available_models: [],
  },
};

async function api(path, options = {}) {
  const res = await fetch(path, {
    headers: { "Content-Type": "application/json" },
    ...options,
  });
  const data = await res.json().catch(() => ({}));
  if (!res.ok) throw new Error(data.error || res.statusText || "请求失败");
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
  const [status, profiles, backups, settings] = await Promise.all([
    api("/api/status"),
    api("/api/profiles"),
    api("/api/backups"),
    api("/api/settings"),
  ]);
  state.status = status;
  state.profiles = profiles;
  state.backups = backups;
  state.settings = settings;
  // Coerce to strict boolean for UI.
  if (state.status && typeof state.status.config_matches_active !== "boolean") {
    state.status.config_matches_active = true;
  }
  renderDrift();
  renderEmptyState();
  renderProfiles();
  renderBackups(backups);
  renderSettings(settings);
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
  const home = $("viewHome");
  const edit = $("viewEdit");
  const settings = $("viewSettings");
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
  if ($("navHomeBtn")) $("navHomeBtn").hidden = name === "home";
  document.querySelectorAll("[data-home-only]").forEach((el) => {
    el.hidden = name !== "home";
  });
  // Keep header add/import only on home list.
  if ($("headerSubtitle")) {
    $("headerSubtitle").textContent =
      name === "settings" ? "设置" : name === "edit" ? ( $("profileId")?.value ? "编辑供应商" : "添加供应商") : "供应商";
  }
  if (name === "settings") {
    loadConfigEditor().catch((err) => toast(err.message, "error"));
  }
}

function renderEmptyState() {
  const empty = !state.profiles.length;
  $("emptyState").hidden = !empty;
  if ($("listControls")) $("listControls").hidden = empty;
  $("profiles").hidden = empty;
  if ($("searchEmpty")) $("searchEmpty").hidden = true;
  $("profileCount").textContent = `${state.profiles.length} 个`;
}

function filteredProfiles() {
  const q = (state.search || "").trim().toLowerCase();
  if (!q) return state.profiles;
  return state.profiles.filter((p) => (p.name || "").toLowerCase().includes(q));
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
  const emptyAll = !state.profiles.length;
  if ($("searchEmpty")) {
    $("searchEmpty").hidden = emptyAll || list.length > 0;
  }
  if (emptyAll) return;

  list.forEach((profile) => {
    const el = document.createElement("article");
    el.className = `provider${profile.is_active ? " active" : ""}`;
    el.innerHTML = `
      <div class="providerTop">
        <div>
          <h3 class="providerName">${escapeHtml(profile.name)}</h3>
          <p class="providerUrl">${escapeHtml(profile.base_url || hostOf(profile.base_url))}</p>
          <p class="providerMeta">${escapeHtml(profile.default_model || "未设默认模型")} · ${formatUpstream(profile.upstream_format)} · ${profile.models?.length || 0} 模型</p>
        </div>
        ${profile.is_active ? '<span class="badge">当前启用</span>' : ""}
      </div>
      <div class="providerActions">
        <button type="button" class="btn sm primary" data-action="enable">${profile.is_active ? "当前启用" : "启用"}</button>
        <button type="button" class="btn sm" data-action="edit">编辑</button>
        <button type="button" class="btn sm ghost" data-action="copy">复制</button>
        <button type="button" class="btn sm ghost" data-action="export">导出</button>
        <button type="button" class="btn sm danger" data-action="delete">删除</button>
      </div>
    `;

    const enableBtn = el.querySelector('[data-action="enable"]');
    if (profile.is_active) {
      enableBtn.disabled = true;
      enableBtn.classList.add("current");
    } else {
      enableBtn.onclick = () => activateProfile(profile.id, enableBtn, profile.name);
    }

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

    $("profiles").appendChild(el);
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

function openEdit(profile) {
  fillForm(profile || { models: [], available_models: [], upstream_format: "openai_chat" });
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
  $("templateSelect").value = "";
  state.availableModels = unique([
    ...(profile.available_models || []),
    ...(profile.models || []).map((model) => model.name || model.model),
  ]);
  $("modelsBody").innerHTML = "";
  (profile.models || []).forEach((model) => addModelCard(model));
  renderModelSelect();
  // Rebuild selects from enabled models, then restore saved values.
  syncEnabledModelList({
    default_model: profile.default_model || "",
    web_search_model: profile.web_search_model || "",
    subagents_default_model: profile.subagents_default_model || "",
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
    upstream_format: tpl.upstream_format,
    base_url: tpl.base_url,
    api_key: keepKey || tpl.api_key || "",
    default_model: "",
    web_search_model: "",
    subagents_default_model: "",
    models: [],
    available_models: [],
  });
  $("templateSelect").value = key;
  toast(`已套用「${tpl.name}」地址与协议，请自行启用模型`, "info");
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
    upstream_format: profile.upstream_format,
    base_url: profile.base_url,
    default_model: profile.default_model,
    web_search_model: profile.web_search_model,
    subagents_default_model: profile.subagents_default_model,
    available_models: profile.available_models || [],
    models: (profile.models || []).map((m) => {
      const item = {
        name: m.name,
        model: m.model,
        base_url: m.base_url || "",
        api_backend: m.api_backend,
        extra_headers: m.extra_headers || {},
        supports_backend_search: !!m.supports_backend_search,
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
    subagents_default_model: profile.subagents_default_model || "",
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
    { id: "subagentsDefaultModel", emptyLabel: "（可选）", required: false },
  ];
  const prefer = preferred || {
    default_model: $("defaultModel")?.value || "",
    web_search_model: $("webSearchModel")?.value || "",
    subagents_default_model: $("subagentsDefaultModel")?.value || "",
  };
  const values = {
    defaultModel: prefer.default_model ?? "",
    webSearchModel: prefer.web_search_model ?? "",
    subagentsDefaultModel: prefer.subagents_default_model ?? "",
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

function addModelCard(model = {}) {
  const backend = model.api_backend || apiBackendFor($("upstreamFormat").value);
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
        <input data-field="base_url" class="mono" value="${escapeAttr(model.base_url || "")}" placeholder="空=供应商地址">
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
  card.querySelector('[data-field="supports_backend_search"]').checked = !!model.supports_backend_search;
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
    upstream_format: $("upstreamFormat").value,
    base_url: $("baseUrl").value.trim(),
    api_key: apiKey,
    available_models: state.availableModels,
    default_model: $("defaultModel")?.value?.trim() || "",
    web_search_model: $("webSearchModel")?.value?.trim() || "",
    subagents_default_model: $("subagentsDefaultModel")?.value?.trim() || "",
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
$("addBtn").onclick = () => openEdit({ models: [], available_models: [], upstream_format: "openai_chat" });
$("emptyNewBtn").onclick = () => openEdit({ models: [], available_models: [], upstream_format: "openai_chat" });
$("emptyImportBtn").onclick = () => importCurrentConfig($("emptyImportBtn"));
$("importHeaderBtn").onclick = () => importCurrentConfig($("importHeaderBtn"));
$("reapplyBtn").onclick = () => {
  const id = state.status?.active_profile?.id;
  const name = state.status?.active_profile?.name;
  activateProfile(id, $("reapplyBtn"), name);
};
$("openConfigFromDriftBtn").onclick = () => showView("settings");
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
["name", "baseUrl", "profileApiKey", "defaultModel", "webSearchModel", "subagentsDefaultModel", "upstreamFormat"].forEach((id) => {
  const el = $(id);
  if (!el) return;
  el.addEventListener("input", scheduleProviderPreview);
  el.addEventListener("change", scheduleProviderPreview);
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
  if ($("templateSelect").value) applyTemplate($("templateSelect").value);
};
$("cancelBtn").onclick = () => fillForm({ models: [], available_models: [], upstream_format: "openai_chat" });
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
      theme: "light",
      port: Number($("port").value || 17878),
    };
    await api("/api/settings", { method: "PUT", body: JSON.stringify(settings) });
    await refreshAll();
  }, { button: $("saveSettingsBtn"), busyLabel: "保存中…", success: "设置已保存" });
};

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

document.addEventListener("keydown", (event) => {
  if ((event.ctrlKey || event.metaKey) && event.key.toLowerCase() === "s") {
    if (state.view === "edit") {
      event.preventDefault();
      $("saveProfileBtn").click();
    }
  }
  if (event.key === "Escape" && state.view !== "home") {
    showView("home");
  }
});

showView("home");
refreshAll()
  .then(() => {
    if (!state.profiles.length) return;
  })
  .catch((err) => toast(err.message, "error"));
