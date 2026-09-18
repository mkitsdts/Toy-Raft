/* Raft 配置中心前端 */
"use strict";

const state = {
  token: localStorage.getItem("cc_token") || "",
  isAdmin: false,
  namespaces: [],
  currentNamespace: null,
  configs: [],
  roles: [],
  editingRoleId: null,
};

const $ = (sel) => document.querySelector(sel);
const $$ = (sel) => document.querySelectorAll(sel);

/* ---------- API ---------- */

async function api(path, options = {}) {
  const resp = await fetch(path, {
    ...options,
    headers: { "Auth": state.token, "Content-Type": "application/json", ...(options.headers || {}) },
  });
  if (resp.status === 401) {
    logout();
    throw new Error("Token 无效或已过期");
  }
  if (resp.status === 204) return null;
  const body = await resp.json().catch(() => ({}));
  if (!resp.ok) throw new Error(body.error || `请求失败 (${resp.status})`);
  return body;
}

/* ---------- 登录 / 退出 ---------- */

function logout() {
  localStorage.removeItem("cc_token");
  state.token = "";
  $("#app-view").classList.add("hidden");
  $("#login-view").classList.remove("hidden");
}

async function login(token) {
  state.token = token;
  const who = await api("/api/v1/whoami");
  localStorage.setItem("cc_token", token);
  state.isAdmin = who.type === "admin";

  $("#identity-badge").textContent = state.isAdmin ? "管理员" : `角色: ${who.role.name}`;
  $$(".admin-only").forEach((el) => el.classList.toggle("hidden", !state.isAdmin));

  $("#login-view").classList.add("hidden");
  $("#app-view").classList.remove("hidden");
  switchTab("configs");
  await loadNamespaces();
}

/* ---------- 命名空间 & 配置 ---------- */

async function loadNamespaces() {
  state.namespaces = await api("/api/v1/namespaces");
  renderNamespaceList();
  const dl = $("#namespace-datalist");
  dl.innerHTML = state.namespaces.map((ns) => `<option value="${esc(ns)}">`).join("");
}

function renderNamespaceList() {
  const ul = $("#namespace-list");
  ul.innerHTML = "";
  if (state.namespaces.length === 0) {
    ul.innerHTML = `<li class="muted" style="cursor:default">暂无命名空间</li>`;
    return;
  }
  for (const ns of state.namespaces) {
    const li = document.createElement("li");
    li.textContent = ns;
    li.classList.toggle("active", ns === state.currentNamespace);
    li.onclick = () => selectNamespace(ns);
    ul.appendChild(li);
  }
}

async function selectNamespace(ns) {
  state.currentNamespace = ns;
  renderNamespaceList();
  state.configs = await api(`/api/v1/configs?namespace=${encodeURIComponent(ns)}`);
  $("#current-namespace").textContent = ns;
  $("#config-empty").classList.add("hidden");
  $("#config-panel").classList.remove("hidden");
  $("#config-filter").value = "";
  renderConfigTable();
}

function renderConfigTable() {
  const filter = $("#config-filter").value.trim().toLowerCase();
  const tbody = $("#config-tbody");
  tbody.innerHTML = "";
  const rows = state.configs.filter((c) => c.key.toLowerCase().includes(filter));
  if (rows.length === 0) {
    tbody.innerHTML = `<tr><td colspan="5" class="muted" style="cursor:default;text-align:center">暂无配置</td></tr>`;
    return;
  }
  for (const cfg of rows) {
    const tr = document.createElement("tr");
    tr.innerHTML = `
      <td>${esc(cfg.key)}</td>
      <td><span class="tag ${esc(cfg.format)}">${esc(cfg.format)}</span></td>
      <td>v${cfg.version}</td>
      <td>${formatTime(cfg.updated_at)}</td>
      <td class="admin-only ${state.isAdmin ? "" : "hidden"}">
        <div class="actions">
          <button class="btn small" data-act="edit">编辑</button>
          <button class="btn small danger" data-act="del">删除</button>
        </div>
      </td>`;
    tr.onclick = (e) => {
      const act = e.target.dataset?.act;
      if (act === "del") { deleteConfig(cfg); return; }
      openConfigModal(cfg);
    };
    tbody.appendChild(tr);
  }
}

/* ---------- 配置弹窗 ---------- */

function openConfigModal(cfg) {
  const isNew = !cfg;
  $("#config-modal-title").textContent = isNew ? "新建配置" : (state.isAdmin ? "编辑配置" : "查看配置");
  $("#cfg-namespace").value = cfg ? cfg.namespace : (state.currentNamespace || "");
  $("#cfg-key").value = cfg ? cfg.key : "";
  $("#cfg-format").value = cfg ? cfg.format : "text";
  $("#cfg-value").value = cfg ? cfg.value : "";
  $("#cfg-version").textContent = cfg ? `(版本 v${cfg.version})` : "";
  $("#cfg-error").textContent = "";

  const readonly = !state.isAdmin;
  $("#cfg-namespace").disabled = !isNew || readonly;
  $("#cfg-key").disabled = !isNew || readonly;
  $("#cfg-format").disabled = readonly;
  $("#cfg-value").readOnly = readonly;
  $("#cfg-save").classList.toggle("hidden", readonly);

  $("#config-modal").classList.remove("hidden");
}

function closeConfigModal() { $("#config-modal").classList.add("hidden"); }

async function saveConfig() {
  const namespace = $("#cfg-namespace").value.trim();
  const key = $("#cfg-key").value.trim();
  const format = $("#cfg-format").value;
  const value = $("#cfg-value").value;
  $("#cfg-error").textContent = "";

  if (!namespace || !key) { $("#cfg-error").textContent = "命名空间和 Key 不能为空"; return; }
  if (format === "json") {
    try { JSON.parse(value); } catch (e) { $("#cfg-error").textContent = "JSON 格式校验失败: " + e.message; return; }
  }

  try {
    await api("/api/v1/configs", {
      method: "POST",
      body: JSON.stringify({ namespace, key, value, format }),
    });
    closeConfigModal();
    toast("已保存");
    await loadNamespaces();
    if (state.namespaces.includes(namespace)) await selectNamespace(namespace);
  } catch (e) {
    $("#cfg-error").textContent = e.message; // 服务端 yaml/json 校验错误也会显示在这里
  }
}

async function deleteConfig(cfg) {
  if (!confirm(`确定删除配置 ${cfg.namespace}/${cfg.key} ?`)) return;
  try {
    await api(`/api/v1/configs/${encodeURIComponent(cfg.namespace)}/${encodeURIComponent(cfg.key)}`, { method: "DELETE" });
    toast("已删除");
    await selectNamespace(cfg.namespace);
  } catch (e) { toast(e.message, true); }
}

/* ---------- 角色管理 ---------- */

async function loadRoles() {
  state.roles = await api("/api/v1/roles");
  const tbody = $("#role-tbody");
  tbody.innerHTML = "";
  if (state.roles.length === 0) {
    tbody.innerHTML = `<tr><td colspan="6" class="muted" style="cursor:default;text-align:center">暂无角色</td></tr>`;
    return;
  }
  for (const role of state.roles) {
    const nsNames = (role.namespaces || []).map((n) => n.name);
    const tr = document.createElement("tr");
    tr.style.cursor = "default";
    tr.innerHTML = `
      <td>${esc(role.name)}</td>
      <td>${esc(role.description || "-")}</td>
      <td>${nsNames.length ? nsNames.map((n) => `<span class="ns-chip">${esc(n)}</span>`).join("") : '<span class="muted">无</span>'}</td>
      <td><div class="token-cell">
        <span title="${esc(role.token)}">${esc(role.token.slice(0, 14))}…</span>
        <button class="btn small" data-act="copy">复制</button>
      </div></td>
      <td>${formatTime(role.created_at)}</td>
      <td><div class="actions">
        <button class="btn small" data-act="edit">编辑</button>
        <button class="btn small" data-act="rotate">重置Token</button>
        <button class="btn small danger" data-act="del">删除</button>
      </div></td>`;
    tr.onclick = async (e) => {
      const act = e.target.dataset?.act;
      try {
        if (act === "copy") { await navigator.clipboard.writeText(role.token); toast("Token 已复制"); }
        else if (act === "edit") { openRoleModal(role); }
        else if (act === "rotate") {
          if (!confirm(`确定重置角色 ${role.name} 的 Token？旧 Token 将立即失效。`)) return;
          await api(`/api/v1/roles/${role.id}/rotate`, { method: "POST", body: "{}" });
          toast("Token 已重置");
          await loadRoles();
        }
        else if (act === "del") {
          if (!confirm(`确定删除角色 ${role.name}？`)) return;
          await api(`/api/v1/roles/${role.id}`, { method: "DELETE" });
          toast("已删除");
          await loadRoles();
        }
      } catch (err) { toast(err.message, true); }
    };
    tbody.appendChild(tr);
  }
}

function openRoleModal(role) {
  state.editingRoleId = role ? role.id : null;
  $("#role-modal-title").textContent = role ? "编辑角色" : "新建角色";
  $("#role-name").value = role ? role.name : "";
  $("#role-desc").value = role ? (role.description || "") : "";
  $("#role-error").textContent = "";

  const granted = new Set(role ? (role.namespaces || []).map((n) => n.name) : []);
  const box = $("#role-ns-box");
  box.innerHTML = state.namespaces.length === 0
    ? `<span class="muted">暂无命名空间，可先在配置管理中创建</span>`
    : "";
  for (const ns of state.namespaces) {
    const label = document.createElement("label");
    label.innerHTML = `<input type="checkbox" value="${esc(ns)}" ${granted.has(ns) ? "checked" : ""}> ${esc(ns)}`;
    box.appendChild(label);
  }
  $("#role-modal").classList.remove("hidden");
}

async function saveRole() {
  const name = $("#role-name").value.trim();
  const description = $("#role-desc").value.trim();
  const namespaces = [...$("#role-ns-box").querySelectorAll("input:checked")].map((i) => i.value);
  $("#role-error").textContent = "";
  if (!name) { $("#role-error").textContent = "名称不能为空"; return; }
  try {
    if (state.editingRoleId) {
      await api(`/api/v1/roles/${state.editingRoleId}`, {
        method: "PUT", body: JSON.stringify({ name, description, namespaces }),
      });
    } else {
      await api("/api/v1/roles", { method: "POST", body: JSON.stringify({ name, description, namespaces }) });
    }
    $("#role-modal").classList.add("hidden");
    toast("已保存");
    await loadRoles();
  } catch (e) { $("#role-error").textContent = e.message; }
}

/* ---------- 工具 ---------- */

function switchTab(name) {
  $$(".tab").forEach((t) => t.classList.toggle("active", t.dataset.tab === name));
  $$(".tab-page").forEach((p) => p.classList.add("hidden"));
  $(`#tab-${name}`).classList.remove("hidden");
  if (name === "roles") loadRoles().catch((e) => toast(e.message, true));
}

function esc(s) {
  return String(s ?? "").replace(/[&<>"']/g, (c) =>
    ({ "&": "&amp;", "<": "&lt;", ">": "&gt;", '"': "&quot;", "'": "&#39;" }[c]));
}

function formatTime(iso) {
  if (!iso) return "-";
  const d = new Date(iso);
  return isNaN(d) ? iso : d.toLocaleString("zh-CN", { hour12: false });
}

let toastTimer = null;
function toast(msg, isError = false) {
  const el = $("#toast");
  el.textContent = msg;
  el.classList.toggle("error", isError);
  el.classList.remove("hidden");
  clearTimeout(toastTimer);
  toastTimer = setTimeout(() => el.classList.add("hidden"), 2500);
}

/* ---------- 事件绑定 ---------- */

$("#login-btn").onclick = async () => {
  $("#login-error").textContent = "";
  try { await login($("#token-input").value.trim()); }
  catch (e) { $("#login-error").textContent = e.message; }
};
$("#token-input").addEventListener("keydown", (e) => { if (e.key === "Enter") $("#login-btn").click(); });
$("#logout-btn").onclick = logout;
$$(".tab").forEach((t) => (t.onclick = () => switchTab(t.dataset.tab)));
$("#new-config-btn").onclick = () => openConfigModal(null);
$("#config-filter").addEventListener("input", renderConfigTable);
$("#cfg-cancel").onclick = closeConfigModal;
$("#cfg-save").onclick = saveConfig;
$("#new-role-btn").onclick = () => openRoleModal(null);
$("#role-cancel").onclick = () => $("#role-modal").classList.add("hidden");
$("#role-save").onclick = saveRole;
$$(".modal-mask").forEach((m) =>
  m.addEventListener("click", (e) => { if (e.target === m) m.classList.add("hidden"); }));

/* ---------- 启动 ---------- */

(async () => {
  if (state.token) {
    try { await login(state.token); return; } catch { /* token 失效，回登录页 */ }
  }
  logout();
})();
