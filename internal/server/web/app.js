const state = {
  view: "overview",
  range: 7,
  device: "",
  app: "",
  provider: "",
  model: "",
  session: "",
  page: 1,
  pageSize: 50,
  trendMetric: "tokens",
  devices: [],
  providers: [],
  models: [],
};

const $ = (selector) => document.querySelector(selector);
const $$ = (selector) => [...document.querySelectorAll(selector)];

function toast(message) {
  const el = $("#toast");
  el.textContent = message;
  el.classList.remove("hidden");
  clearTimeout(toast.timer);
  toast.timer = setTimeout(() => el.classList.add("hidden"), 2600);
}

async function api(path, params = new URLSearchParams()) {
  const query = params.toString();
  const response = await fetch(`/api/v1${path}${query ? `?${query}` : ""}`);
  if (!response.ok) {
    const body = await response.json().catch(() => ({}));
    throw new Error(body.message || body.error || `HTTP ${response.status}`);
  }
  return response.json();
}

function filters() {
  const params = new URLSearchParams();
  const now = Math.floor(Date.now() / 1000);
  if (state.range > 0) params.set("from", String(now - state.range * 86400));
  params.set("to", String(now));
  if (state.device) params.set("deviceId", state.device);
  if (state.app) params.set("appType", state.app);
  if (state.provider) params.set("providerId", state.provider);
  if (state.model) params.set("model", state.model);
  if (state.session) params.set("sessionId", state.session);
  return params;
}

function compact(value) {
  const number = Number(value || 0);
  if (number >= 1e12) return `${(number / 1e12).toFixed(2)}T`;
  if (number >= 1e9) return `${(number / 1e9).toFixed(2)}B`;
  if (number >= 1e6) return `${(number / 1e6).toFixed(2)}M`;
  if (number >= 1e3) return `${(number / 1e3).toFixed(2)}K`;
  return number.toLocaleString();
}

function money(value) {
  const number = Number(value || 0);
  if (number >= 1000) return `$${number.toFixed(2)}`;
  if (number >= 1) return `$${number.toFixed(3)}`;
  return `$${number.toFixed(6)}`;
}

function pct(value) {
  return `${(Number(value || 0) * 100).toFixed(1)}%`;
}

function escapeHtml(value) {
  return String(value ?? "").replace(/[&<>"']/g, (char) => ({
    "&": "&amp;", "<": "&lt;", ">": "&gt;", '"': "&quot;", "'": "&#039;",
  })[char]);
}

async function loadDevices() {
  const devices = await api("/devices");
  state.devices = devices.devices || devices || [];
  const select = $("#device-select");
  select.innerHTML = `<option value="">全部设备</option>${state.devices.map((device) =>
    `<option value="${escapeHtml(device.deviceId)}">${escapeHtml(device.name)}</option>`
  ).join("")}`;
  select.value = state.device;
}

async function loadFilters() {
  const [providers, models] = await Promise.all([
    api("/providers", filters()),
    api("/models", filters()),
  ]);
  state.providers = providers.items || [];
  state.models = models.items || [];
  fillSelect("#provider-select", state.providers.map((item) => [item.providerId, item.providerName || item.providerId]));
  fillSelect("#model-select", state.models.map((item) => [item.model, item.model]));
  fillSelect("#app-select", [...new Set([
    ...state.providers.map((item) => item.appType),
    ...state.models.map((item) => item.appType).filter(Boolean),
  ])].map((value) => [value, value]));
}

function fillSelect(selector, entries) {
  const select = $(selector);
  const current = select.value;
  select.innerHTML = `<option value="">全部</option>${entries.filter(Boolean).map(([value, label]) =>
    `<option value="${escapeHtml(value)}">${escapeHtml(label)}</option>`
  ).join("")}`;
  select.value = current;
}

async function loadOverview() {
  const params = filters();
  const [summary, trends, providers, models] = await Promise.all([
    api("/summary", params),
    api("/trends", params),
    api("/providers", params),
    api("/models", params),
  ]);
  renderSummary(summary.summary || summary);
  renderTrend(trends.points || trends);
  renderProviders(providers.items || []);
  renderModels(models.items || []);
}

function renderSummary(summary) {
  const cards = [
    ["真实消耗 Tokens", compact(summary.realTotalTokens), `${compact(summary.totalInputTokens)} 输入 / ${compact(summary.totalOutputTokens)} 输出`],
    ["总请求数", compact(summary.totalRequests), `成功率 ${(Number(summary.successRate || 0)).toFixed(1)}%`],
    ["估算费用", money(summary.totalCost), "按当前定价估算"],
    ["缓存命中率", pct(summary.cacheHitRate), `${compact(summary.totalCacheReadTokens)} 缓存读取`],
  ];
  $("#summary-cards").innerHTML = cards.map(([label, value, sub]) => `
    <div class="card"><div class="card-label">${label}</div><div class="card-value">${value}</div><div class="card-sub">${sub}</div></div>
  `).join("");
}

function renderTrend(points) {
  const container = $("#trend-chart");
  if (!points.length) {
    container.innerHTML = `<div class="muted" style="padding:40px;text-align:center">暂无数据</div>`;
    return;
  }
  const width = Math.max(container.clientWidth, 640);
  const height = 250;
  const padding = { top: 16, right: 18, bottom: 34, left: 58 };
  const innerW = width - padding.left - padding.right;
  const innerH = height - padding.top - padding.bottom;
  const metric = state.trendMetric;
  const values = points.map((point) => metric === "tokens" ? Number(point.totalTokens || 0)
    : metric === "requests" ? Number(point.requestCount || 0) : Number(point.totalCost || 0));
  const max = Math.max(...values, 1);
  const x = (index) => padding.left + (points.length === 1 ? innerW / 2 : index * innerW / (points.length - 1));
  const y = (value) => padding.top + innerH - value / max * innerH;
  const line = values.map((value, index) => `${index === 0 ? "M" : "L"}${x(index)},${y(value)}`).join(" ");
  const area = `${line} L${x(values.length - 1)},${padding.top + innerH} L${x(0)},${padding.top + innerH} Z`;
  const grid = [0, 0.25, 0.5, 0.75, 1].map((ratio) => {
    const gy = padding.top + innerH - ratio * innerH;
    const label = metric === "cost" ? money(max * ratio) : compact(max * ratio);
    return `<line class="grid-line" x1="${padding.left}" y1="${gy}" x2="${width - padding.right}" y2="${gy}"/><text class="axis-text" x="${padding.left - 8}" y="${gy + 3}" text-anchor="end">${label}</text>`;
  }).join("");
  const labels = points.map((point, index) => {
    if (points.length > 12 && index % Math.ceil(points.length / 10) !== 0 && index !== points.length - 1) return "";
    return `<text class="axis-text" x="${x(index)}" y="${height - 10}" text-anchor="middle">${escapeHtml(String(point.date || point.bucket || "").slice(5))}</text>`;
  }).join("");
  const dots = values.map((value, index) => `<circle class="dot" cx="${x(index)}" cy="${y(value)}" r="2.5"><title>${escapeHtml(points[index].date || points[index].bucket)}: ${metric === "cost" ? money(value) : compact(value)}</title></circle>`).join("");
  container.innerHTML = `<svg viewBox="0 0 ${width} ${height}" preserveAspectRatio="none">
    <defs><linearGradient id="area-gradient" x1="0" y1="0" x2="0" y2="1"><stop offset="0%" stop-color="#7c9cff" stop-opacity="0.28"/><stop offset="100%" stop-color="#7c9cff" stop-opacity="0"/></linearGradient></defs>
    ${grid}<path class="area" d="${area}"/><path class="line" d="${line}"/>${dots}${labels}
  </svg>`;
}

function renderProviders(items) {
  const rows = items.slice(0, 12).map((item) => `<tr>
    <td>${escapeHtml(item.providerName || item.providerId || "未知供应商")}</td>
    <td><span class="tag">${escapeHtml(item.appType || "")}</span></td>
    <td class="numeric">${compact(item.requestCount)}</td>
    <td class="numeric">${compact(item.totalTokens)}</td>
    <td class="numeric">${money(item.totalCost)}</td>
    <td class="numeric">${Number(item.successRate || 0).toFixed(1)}%</td>
  </tr>`).join("");
  $("#provider-table").innerHTML = `<thead><tr><th>供应商</th><th>应用</th><th class="numeric">请求</th><th class="numeric">Tokens</th><th class="numeric">费用</th><th class="numeric">成功率</th></tr></thead><tbody>${rows || `<tr><td colspan="6" class="muted">暂无数据</td></tr>`}</tbody>`;
}

function renderModels(items) {
  const rows = items.slice(0, 12).map((item) => `<tr>
    <td class="mono">${escapeHtml(item.model || "unknown")}</td>
    <td class="numeric">${compact(item.requestCount)}</td>
    <td class="numeric">${compact(item.totalTokens)}</td>
    <td class="numeric">${money(item.totalCost)}</td>
    <td class="numeric">${Number(item.successRate || 0).toFixed(1)}%</td>
  </tr>`).join("");
  $("#model-table").innerHTML = `<thead><tr><th>模型</th><th class="numeric">请求</th><th class="numeric">Tokens</th><th class="numeric">费用</th><th class="numeric">成功率</th></tr></thead><tbody>${rows || `<tr><td colspan="5" class="muted">暂无数据</td></tr>`}</tbody>`;
}

async function loadRequests() {
  const params = filters();
  params.set("page", String(state.page));
  params.set("pageSize", String(state.pageSize));
  const result = await api("/requests", params);
  const items = result.items || result.logs || [];
  const total = result.total || items.length;
  $("#page-label").textContent = `${state.page} / ${Math.max(1, Math.ceil(total / state.pageSize))}`;
  const rows = items.map((item) => `<tr>
    <td class="mono muted">${new Date(item.occurredAt).toLocaleString()}</td>
    <td>${escapeHtml(item.deviceName || item.deviceId || "")}</td>
    <td><span class="tag">${escapeHtml(item.sourceApp || item.appType || "")}</span></td>
    <td>${escapeHtml(item.providerName || item.providerId || "会话日志")}</td>
    <td class="mono">${escapeHtml(item.model || "unknown")}</td>
    <td class="numeric">${compact(item.inputTokens)}</td>
    <td class="numeric">${compact(item.outputTokens)}</td>
    <td class="numeric">${compact(item.cacheReadTokens)}</td>
    <td class="numeric">${money(item.totalCostUsd)}</td>
    <td>${item.statusCode ? `<span class="tag ${item.statusCode >= 400 ? "bad" : "ok"}">${item.statusCode}</span>` : ""}</td>
  </tr>`).join("");
  $("#request-table").innerHTML = `<thead><tr><th>时间</th><th>设备</th><th>应用</th><th>供应商</th><th>模型</th><th class="numeric">输入</th><th class="numeric">输出</th><th class="numeric">缓存读</th><th class="numeric">费用</th><th>状态</th></tr></thead><tbody>${rows || `<tr><td colspan="10" class="muted">暂无数据</td></tr>`}</tbody>`;
}

async function loadDeviceView() {
  const devices = await api("/devices");
  state.devices = devices.devices || devices || [];
  $("#device-cards").innerHTML = state.devices.map((device) => {
    const stats = device.summary || {};
    return `<div class="device-card">
      <div class="device-card-head"><div><div class="device-name">${escapeHtml(device.name)}</div><div class="device-meta">${escapeHtml(device.platform)} · ${escapeHtml(device.arch)}</div></div><span class="tag ok">在线</span></div>
      <div class="device-meta mono">${escapeHtml(device.deviceId)}</div>
      <div class="device-stats">
        <div class="device-stat"><small>请求</small><strong>${compact(stats.totalRequests)}</strong></div>
        <div class="device-stat"><small>Tokens</small><strong>${compact(stats.realTotalTokens)}</strong></div>
        <div class="device-stat"><small>费用</small><strong>${money(stats.totalCost)}</strong></div>
        <div class="device-stat"><small>最后同步</small><strong>${device.lastSeenAt ? new Date(device.lastSeenAt).toLocaleString() : "—"}</strong></div>
      </div>
    </div>`;
  }).join("") || `<div class="muted">暂无设备</div>`;
}

async function loadSessions() {
  const result = await api("/sessions", filters());
  const items = result.items || [];
  const rows = items.map((item) => `<tr data-session="${escapeHtml(item.sessionId || "")}" style="cursor:pointer">
    <td>${escapeHtml(item.deviceName || item.deviceId || "")}</td>
    <td><span class="tag">${escapeHtml(item.sourceApp || "")}</span></td>
    <td class="mono">${escapeHtml(item.sessionId || "")}</td>
    <td class="numeric">${compact(item.requestCount)}</td>
    <td class="numeric">${compact(item.totalTokens)}</td>
    <td class="numeric">${money(item.totalCostUsd)}</td>
    <td>${new Date(item.firstRequestAt).toLocaleString()}</td>
    <td>${new Date(item.lastRequestAt).toLocaleString()}</td>
  </tr>`).join("");
  $("#session-table").innerHTML = `<thead><tr><th>设备</th><th>应用</th><th>会话 ID</th><th class="numeric">请求</th><th class="numeric">Tokens</th><th class="numeric">费用</th><th>首次请求</th><th>最后请求</th></tr></thead><tbody>${rows || `<tr><td colspan="8" class="muted">暂无会话</td></tr>`}</tbody>`;
  $$("#session-table tbody tr[data-session]").forEach((row) => row.addEventListener("click", () => {
    state.session = row.dataset.session;
    state.page = 1;
    switchView("requests");
  }));
}

async function loadPricing() {
  const result = await api("/pricing");
  const models = result.models || [];
  const sync = result.sync || {};
  $("#pricing-sync").textContent = `版本 ${result.version || "-"} · 哈希 ${(result.contentHash || "").slice(0, 12)} · 最后更新 ${sync.lastUpdatedAt ? new Date(sync.lastUpdatedAt).toLocaleString() : "-"} · 最后同步 ${sync.lastSyncedAt ? new Date(sync.lastSyncedAt).toLocaleString() : "-"}${sync.lastError ? ` · 错误 ${sync.lastError}` : ""}`;
  $("#pricing-table").innerHTML = `<thead><tr><th>模型</th><th>输入</th><th>输出</th><th>缓存读</th><th>缓存写</th><th>币种</th><th>来源</th></tr></thead><tbody>${models.map((model) => `<tr>
    <td class="mono">${escapeHtml(model.modelId)}</td>
    <td class="numeric">${model.inputCostPerMillion}</td>
    <td class="numeric">${model.outputCostPerMillion}</td>
    <td class="numeric">${model.cacheReadCostPerMillion}</td>
    <td class="numeric">${model.cacheCreationCostPerMillion}</td>
    <td>${escapeHtml(model.currency || "USD")}</td>
    <td class="muted">${escapeHtml(model.source || "")}</td>
  </tr>`).join("") || `<tr><td colspan="7" class="muted">暂无定价</td></tr>`}</tbody>`;
  await loadAliases();
}

async function loadAliases() {
  const result = await api("/aliases");
  const items = result.items || [];
  $("#alias-table").innerHTML = `<thead><tr><th>别名 / 模式</th><th>标准模型</th><th>匹配</th><th>作用域</th><th>优先级</th></tr></thead><tbody>${items.map((alias) => `<tr>
    <td class="mono">${escapeHtml(alias.aliasPattern)}</td>
    <td class="mono">${escapeHtml(alias.canonicalModelId)}</td>
    <td><span class="tag">${escapeHtml(alias.matchType)}</span></td>
    <td>${escapeHtml(alias.scopeApp || "全局")}</td>
    <td class="numeric">${alias.priority || 0}</td>
  </tr>`).join("") || `<tr><td colspan="5" class="muted">暂无别名</td></tr>`}</tbody>`;
}

async function refresh() {
  $("#refresh-btn").disabled = true;
  try {
    await loadDevices();
    if (state.view === "overview") await loadOverview();
    if (state.view === "requests") await loadRequests();
    if (state.view === "sessions") await loadSessions();
    if (state.view === "devices") await loadDeviceView();
    if (state.view === "pricing") await loadPricing();
  } catch (error) {
    toast(error.message);
  } finally {
    $("#refresh-btn").disabled = false;
  }
}

function switchView(view) {
  state.view = view;
  $$(".nav-item").forEach((button) => button.classList.toggle("active", button.dataset.view === view));
  $$(".view").forEach((section) => section.classList.toggle("hidden", section.id !== `view-${view}`));
  refresh();
}

$$(".nav-item").forEach((button) => button.addEventListener("click", () => switchView(button.dataset.view)));
$("#refresh-btn").addEventListener("click", refresh);
$("#range-select").addEventListener("change", (event) => { state.range = Number(event.target.value); state.page = 1; refresh(); });
$("#device-select").addEventListener("change", (event) => { state.device = event.target.value; state.page = 1; refresh(); });
$("#app-select").addEventListener("change", (event) => { state.app = event.target.value; state.page = 1; refresh(); });
$("#provider-select").addEventListener("change", (event) => { state.provider = event.target.value; state.page = 1; refresh(); });
$("#model-select").addEventListener("change", (event) => { state.model = event.target.value; state.page = 1; refresh(); });
$("#prev-page").addEventListener("click", () => { if (state.page > 1) { state.page -= 1; loadRequests(); } });
$("#next-page").addEventListener("click", () => { state.page += 1; loadRequests(); });
$("#reload-pricing").addEventListener("click", loadPricing);
$("#alias-add").addEventListener("click", async () => {
  const payload = {
    aliasPattern: $("#alias-pattern").value.trim(),
    canonicalModelId: $("#alias-canonical").value.trim(),
    matchType: $("#alias-match").value,
    scopeApp: "",
    priority: 100,
  };
  if (!payload.aliasPattern || !payload.canonicalModelId) { toast("请填写别名和标准模型 ID"); return; }
  const response = await fetch("/api/v1/aliases", { method: "POST", headers: { "Content-Type": "application/json" }, body: JSON.stringify(payload) });
  if (!response.ok) { toast("添加失败"); return; }
  $("#alias-pattern").value = "";
  $("#alias-canonical").value = "";
  await loadAliases();
});
$$("#trend-metric button").forEach((button) => button.addEventListener("click", () => {
  state.trendMetric = button.dataset.metric;
  $$("#trend-metric button").forEach((item) => item.classList.toggle("active", item === button));
  loadOverview();
}));

(async () => {
  try {
    const health = await api("/health");
    $("#agent-version").textContent = health.version || "v0.1.0";
    $("#sync-status").textContent = "运行中";
  } catch (error) {
    $("#sync-status").textContent = "API 离线";
  }
  await loadDevices();
  await loadFilters();
  await refresh();
})();
