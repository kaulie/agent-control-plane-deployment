// Deployment Control Plane — vanilla JS panel. Polls the same-origin HTTP API.
'use strict';

const API = ''; // same origin; API paths already begin with /api
const POLL_MS = 3000;

const $ = (sel, root = document) => root.querySelector(sel);
const $$ = (sel, root = document) => Array.from(root.querySelectorAll(sel));
const esc = (s) => String(s ?? '').replace(/[&<>"']/g, (c) => ({
  '&': '&amp;', '<': '&lt;', '>': '&gt;', '"': '&quot;', "'": '&#39;'
}[c]));
const fmtTime = (t) => (t ? String(t).replace('T', ' ').replace(/\.\d+Z$/, 'Z') : '—');

// Canonical deployment-pipeline event levels. These names mirror the Go
// eventlevel package (info|success|warn|error) and keep the panel rendering
// aligned with what the API stores.
const EVENT_LEVELS = Object.freeze(['info', 'success', 'warn', 'error']);
const DEFAULT_EVENT_LEVEL = 'info';

function eventLevelName(level) {
  return EVENT_LEVELS.includes(level) ? level : DEFAULT_EVENT_LEVEL;
}

function eventLevelClass(level) {
  return 'evlog--' + eventLevelName(level);
}

// ---- state badges ----------------------------------------------------------
function stateBadge(state) {
  const map = {
    queued: 'badge--wait', packaging: 'badge--violet', deploying: 'badge--violet',
    running: 'badge--run', succeeded: 'badge--ok', failed: 'badge--bad',
    cancelled: 'badge--muted',
  };
  const cls = map[state] || 'badge--muted';
  return `<span class="badge ${cls}">${esc(state)}</span>`;
}

// ---- toast ----------------------------------------------------------------
let toastTimer = null;
function toast(msg, kind = '') {
  const el = $('#toast');
  el.textContent = msg;
  el.className = 'toast' + (kind ? ' toast--' + kind : '');
  el.hidden = false;
  clearTimeout(toastTimer);
  toastTimer = setTimeout(() => { el.hidden = true; }, 3500);
}

// ---- API helpers ----------------------------------------------------------
async function apiGet(path) {
  const res = await fetch(API + path, { headers: { 'cache-control': 'no-store' } });
  if (!res.ok) throw new Error(`${res.status} ${await res.text()}`);
  return res.json();
}
async function apiSend(method, path, body) {
  const opt = {
    method,
    headers: { 'content-type': 'application/json', ...identityHeaders() },
  };
  if (body !== undefined) opt.body = JSON.stringify(body);
  const res = await fetch(API + path, opt);
  const text = await res.text();
  let data = null;
  try { data = text ? JSON.parse(text) : null; } catch { data = { raw: text }; }
  if (!res.ok) throw new Error((data && data.error) || `${res.status} ${text}`);
  return data;
}

// ---- caller identity (phase 1) --------------------------------------------
// The deploy APIs require two plain headers identifying who triggers a deploy:
// identity_role (user|agent) + identity_id (user_001 / agent_002 / ...). The
// picker in the top bar is remembered in localStorage and attached to every
// write request.
const IDENTITY_KEY = 'acp.identity';
const IDENTITY_DEFAULT = { role: 'user', id: 'user_001' };

function loadIdentity() {
  try {
    const raw = localStorage.getItem(IDENTITY_KEY);
    if (raw) return { ...IDENTITY_DEFAULT, ...JSON.parse(raw) };
  } catch { /* ignore malformed storage */ }
  return { ...IDENTITY_DEFAULT };
}
let identity = loadIdentity();

function identityHeaders() {
  return { identity_role: identity.role, identity_id: identity.id };
}

function identityLabel(role, id) {
  if (!role && !id) return '—';
  return (role || '?') + ':' + (id || '?');
}

// Renders the triggerer of a deploy/pipeline job (role badge + id).
function identityCell(job) {
  const role = job.triggeredByRole;
  const id = job.triggeredById;
  if (!role && !id) return '<span class="muted">—</span>';
  const cls = role === 'agent' ? 'badge--violet' : 'badge--run';
  return `<span class="badge ${cls}">${esc(role || '?')}</span> ` +
    `<span class="mono">${esc(id || '?')}</span>`;
}

function syncIdentityInputs() {
  $('#identity-role').value = identity.role;
  $('#identity-id').value = identity.id;
}

function saveIdentity() {
  const id = $('#identity-id').value.trim() || IDENTITY_DEFAULT.id;
  identity = { role: $('#identity-role').value, id };
  try { localStorage.setItem(IDENTITY_KEY, JSON.stringify(identity)); } catch { /* ignore */ }
  syncIdentityInputs();
  toast('当前身份：' + identityLabel(identity.role, identity.id));
}

$$('#identity-role, #identity-id').forEach((el) => {
  el.addEventListener('change', saveIdentity);
});
$('#identity-id').addEventListener('keydown', (e) => {
  if (e.key === 'Enter') { e.preventDefault(); saveIdentity(); }
});
syncIdentityInputs();

// ---- tabs -----------------------------------------------------------------
$$('#tabs .tab').forEach((btn) => {
  btn.addEventListener('click', () => {
    $$('#tabs .tab').forEach((b) => b.classList.toggle('tab--active', b === btn));
    const tab = btn.dataset.tab;
    $$('.tabpanel').forEach((p) => p.classList.toggle('tabpanel--active', p.id === 'tab-' + tab));
    refresh();
  });
});

// ---- health / meta --------------------------------------------------------
async function refreshHealth() {
  try {
    const h = await apiGet('/health');
    $('#health-badge').textContent = 'online';
    $('#health-badge').className = 'badge badge--ok';
  } catch {
    $('#health-badge').textContent = 'offline';
    $('#health-badge').className = 'badge badge--bad';
  }
}

async function refreshMeta() {
  const pre = $('#meta-pre');
  try {
    const m = await apiGet('/api/meta');
    pre.textContent = JSON.stringify(m, null, 2);
    $('#meta-home').textContent = 'home: ' + (m.home || '—');
    $('#meta-port').textContent = 'port: ' + (m.port || '—');
  } catch (e) {
    pre.textContent = '加载失败：' + e.message;
  }
}

// ---- services (dropdown options + 服务契约 tab) ---------------------------
// The catalog comes from service_registry (GET /api/services merges the
// registry's contracts with this control plane's deployment config); the panel
// only ever *configures* a service the registry knows — never creates one.
let services = [];
let servicesError = '';
let registryStatus = null;

async function refreshServices() {
  try {
    const data = await apiGet('/api/services');
    services = data.services || [];
    registryStatus = data.registry || null;
    servicesError = '';
  } catch (e) {
    services = [];
    registryStatus = null;
    servicesError = e.message;
  }
  populateServiceSelects();
  renderServiceContracts();
  renderRegistryStatus();
}

function populateServiceSelects() {
  // Trigger selects (发起部署 / 发起流水线) list only services with deployment
  // config: without runtimeDir/commands a trigger can only fail. History
  // filters keep every service id so old jobs stay filterable.
  const configured = services.filter((s) => s.configured);
  for (const id of ['#pipe-service', '#dep-service']) {
    const sel = $(id);
    if (!sel) continue;
    const prev = sel.value;
    sel.innerHTML = configured.map((s) =>
      `<option value="${esc(s.serviceId)}">${esc(s.serviceId)}</option>`).join('');
    if (configured.find((s) => s.serviceId === prev)) sel.value = prev;
  }
  // History filters: keep an empty "全部" option so a filter can be cleared.
  for (const id of ['#pipe-f-serviceId', '#dep-f-serviceId']) {
    const sel = $(id);
    if (!sel) continue;
    const prev = sel.value;
    sel.innerHTML = `<option value="">全部</option>` +
      services.map((s) => `<option value="${esc(s.serviceId)}">${esc(s.serviceId)}</option>`).join('');
    sel.value = prev && services.find((s) => s.serviceId === prev) ? prev : '';
  }
  // Artifacts filter: keep an "全部" (all) option so the tab can list every
  // service's artifacts, not just one.
  const artSel = $('#art-service');
  if (artSel) {
    const prev = artSel.value;
    artSel.innerHTML = `<option value="">全部</option>` +
      services.map((s) => `<option value="${esc(s.serviceId)}">${esc(s.serviceId)}</option>`).join('');
    artSel.value = prev && services.find((s) => s.serviceId === prev) ? prev : '';
  }
}

// ---- 服务契约 (service contracts) ------------------------------------------
// Renders the merged catalog into the 服务契约 tab. Each row shows where the
// service stands: 已登记 (service_registry) / 未登记 (local config only) and
// configured / 未配置 (needs deployment config before it can be deployed).
function serviceGracefulLabel(svc) {
  return (svc.restartNotifyUrl && svc.restartPollUrl) ? 'enabled' : '—';
}

// Where the catalog came from + whether the pull worked, so a registry outage
// never looks like "no services".
function renderRegistryStatus() {
  const badge = $('#svc-registry-status');
  const url = $('#svc-registry-url');
  if (!badge) return;
  if (servicesError) {
    badge.textContent = '加载失败';
    badge.className = 'badge badge--bad';
    badge.title = servicesError;
    return;
  }
  if (!registryStatus) {
    badge.textContent = '未知';
    badge.className = 'badge badge--muted';
    return;
  }
  if (!registryStatus.enabled) {
    if (url) url.textContent = '未启用';
    badge.textContent = '仅本地配置';
    badge.className = 'badge badge--muted';
    badge.title = 'SERVICE_REGISTRY_URL=off：只显示本机已配置的服务';
    return;
  }
  if (url) url.textContent = registryStatus.url;
  if (registryStatus.ok) {
    badge.textContent = `在线 · ${registryStatus.services} 个服务`;
    badge.className = 'badge badge--ok';
    badge.title = '服务列表来自 service_registry';
  } else {
    badge.textContent = '拉取失败';
    badge.className = 'badge badge--bad';
    badge.title = registryStatus.error || 'service_registry 拉取失败';
  }
}

function registryBadge(svc) {
  return svc.registered
    ? `<span class="badge badge--ok">已登记</span>`
    : `<span class="badge badge--bad" title="service_registry 未返回该服务（未登记，或注册中心暂时不可用）">未登记</span>`;
}

// 服务端口：必填项（服务启动时注入 SERVICE_PORT）。这里展示契约里显式配置的值；
// 老契约（还没补填）才按 healthUrl 推导，并明确标出"未指定"。
function servicePortLabel(svc) {
  if (svc.port > 0) return `<span class="mono">${esc(String(svc.port))}</span>`;
  const m = /^[a-z][a-z0-9+.-]*:\/\/[^/?#]*?:(\d+)(?:[/?#]|$)/i.exec(svc.healthUrl || '');
  const derived = m ? `<span class="mono">${esc(m[1])}</span> <span class="muted">(healthUrl)</span>` : '';
  return `<span class="badge badge--wait">未指定</span> ${derived}`;
}

function renderServiceContracts() {
  const tbody = $('#svc-table tbody');
  if (!tbody) return;
  if (servicesError) {
    tbody.innerHTML = `<tr><td colspan="9" class="muted">加载失败：${esc(servicesError)}</td></tr>`;
    return;
  }
  if (!services.length) {
    const empty = registryStatus && registryStatus.enabled && registryStatus.ok
      ? 'service_registry 里还没有已登记的服务（在注册中心登记后这里就会出现）'
      : '暂无服务';
    tbody.innerHTML = `<tr><td colspan="9" class="muted">${empty}</td></tr>`;
    return;
  }
  tbody.innerHTML = services.map((s) => {
    const reg = s.registry || {};
    const versionOwner = [reg.version, reg.owner].filter(Boolean).join(' / ') || '—';
    // 列表里不展示 gitRepoUrl（注册中心同步过来的信息，本机不能改；要看去表单里看只读值）。
    const state = registryBadge(s) +
      (s.configured ? '' : ' <span class="badge badge--wait">未配置</span>');
    const actions = s.configured
      ? `<button class="btn btn--sm" data-svc-edit="${esc(s.serviceId)}">配置</button>
        <button class="btn btn--sm btn--danger" data-svc-delete="${esc(s.serviceId)}">清除</button>`
      : `<button class="btn btn--sm btn--primary" data-svc-edit="${esc(s.serviceId)}">配置</button>`;
    return `<tr>
      <td class="mono">${esc(s.serviceId)}</td>
      <td>${esc(s.name || reg.description || '—')}</td>
      <td>${state}</td>
      <td class="mono">${esc(versionOwner)}</td>
      <td class="mono">${esc(s.runtimeDir || '—')}</td>
      <td class="mono">${esc(s.healthUrl || '—')}</td>
      <td>${servicePortLabel(s)}</td>
      <td>${esc(serviceGracefulLabel(s))}</td>
      <td class="cell-actions">${actions}</td>
    </tr>`;
  }).join('');
}

const SVC_FORM_FIELDS = [
  '#svc-serviceId', '#svc-name', '#svc-runtimeDir', '#svc-healthUrl', '#svc-port',
  '#svc-startCmd', '#svc-stopCmd', '#svc-restartCmd', '#svc-gitRepoUrl',
  '#svc-defaultBranch', '#svc-restartNotifyUrl', '#svc-restartPollUrl',
  '#svc-gracefulRestartMaxWaitMs',
];

let editingServiceID = null;

function clearServiceForm() {
  for (const id of SVC_FORM_FIELDS) {
    const el = $(id);
    if (el) el.value = '';
  }
}

function resetServiceForm() {
  editingServiceID = null;
  clearServiceForm();
  $('#svc-form-title').textContent = '配置服务';
  const idEl = $('#svc-serviceId');
  if (idEl) idEl.disabled = true; // serviceId always comes from the list
  const cancel = $('#svc-form-cancel');
  if (cancel) cancel.hidden = true;
  setServiceFormMsg('');
}

// Visible hint inside the form card (e.g. "未在注册中心登记，保存会被拒绝").
function setServiceFormMsg(msg) {
  const el = $('#svc-msg');
  if (el) el.textContent = msg;
}

function fillServiceForm(svc) {
  editingServiceID = svc.serviceId;
  const reg = svc.registry || {};
  $('#svc-form-title').textContent = (svc.configured ? '编辑部署配置：' : '配置部署参数：') + svc.serviceId;
  $('#svc-serviceId').value = svc.serviceId || '';
  $('#svc-serviceId').disabled = true;
  $('#svc-name').value = svc.name || reg.description || '';
  $('#svc-runtimeDir').value = svc.runtimeDir || '';
  $('#svc-healthUrl').value = svc.healthUrl || '';
  $('#svc-port').value = svc.port || '';
  $('#svc-startCmd').value = svc.startCmd || '';
  $('#svc-stopCmd').value = svc.stopCmd || '';
  $('#svc-restartCmd').value = svc.restartCmd || '';
  // 注册中心登记值优先展示（它才是真源）；只有注册中心没登记时才显示本机镜像的旧值。
  $('#svc-gitRepoUrl').value = reg.gitRepoUrl || svc.gitRepoUrl || '';
  $('#svc-defaultBranch').value = svc.defaultBranch || '';
  $('#svc-restartNotifyUrl').value = svc.restartNotifyUrl || '';
  $('#svc-restartPollUrl').value = svc.restartPollUrl || '';
  $('#svc-gracefulRestartMaxWaitMs').value = svc.gracefulRestartMaxWaitMs || '';
  setServiceFormMsg(svc.registered
    ? ''
    : '⚠ service_registry 未返回该服务（未登记 / 注册中心不可用）：已配置的仍可编辑，新建会被拒绝。');
  const cancel = $('#svc-form-cancel');
  if (cancel) cancel.hidden = false;
  const card = $('#svc-form-card');
  if (card && card.scrollIntoView) card.scrollIntoView({ behavior: 'smooth', block: 'start' });
}

function serviceFormBody() {
  const serviceId = $('#svc-serviceId').value.trim();
  const name = $('#svc-name').value.trim();
  const runtimeDir = $('#svc-runtimeDir').value.trim();
  const healthUrl = $('#svc-healthUrl').value.trim();
  const portRaw = $('#svc-port').value.trim();
  const port = portRaw === '' ? 0 : Number(portRaw);
  const startCmd = $('#svc-startCmd').value.trim();
  const stopCmd = $('#svc-stopCmd').value.trim();
  const restartCmd = $('#svc-restartCmd').value.trim();
  if (!serviceId) return { error: '请先从列表里点「配置」选择服务' };
  if (!runtimeDir || !healthUrl || !startCmd || !stopCmd || !restartCmd) {
    return { error: 'runtimeDir / healthUrl / startCmd / stopCmd / restartCmd 为必填项' };
  }
  if (!Number.isInteger(port) || port < 1 || port > 65535) {
    return { error: '服务端口必填，且必须是 1..65535 的整数（启动时会注入 SERVICE_PORT）' };
  }
  // 端口唯一性：本机目录里已经有的服务列表就能查（后端也会再校验一次，防并发）。
  const holder = services.find((s) => s.serviceId !== serviceId && Number(s.port) === port);
  if (holder) {
    return { error: `端口 ${port} 已被服务 ${holder.serviceId} 占用；服务端口必须唯一，请换一个` };
  }
  return {
    serviceId,
    body: {
      // gitRepoUrl 不在这里发送：它来自 service_registry，本机不能改（后端也会拒绝改）。
      name, runtimeDir, healthUrl, port, startCmd, stopCmd, restartCmd,
      defaultBranch: $('#svc-defaultBranch').value.trim(),
      restartNotifyUrl: $('#svc-restartNotifyUrl').value.trim(),
      restartPollUrl: $('#svc-restartPollUrl').value.trim(),
      gracefulRestartMaxWaitMs: Number($('#svc-gracefulRestartMaxWaitMs').value) || 0,
    },
  };
}

async function saveServiceContract() {
  const built = serviceFormBody();
  if (built.error) { toast(built.error, 'err'); setServiceFormMsg(built.error); return; }
  try {
    await apiSend('PUT', '/api/services/' + encodeURIComponent(built.serviceId), built.body);
    toast('已保存 ' + built.serviceId + ' 的部署配置', 'ok');
    resetServiceForm();
    await refreshServices();
  } catch (e) {
    toast('保存失败：' + e.message, 'err');
    setServiceFormMsg('保存失败：' + e.message);
  }
}

// DELETE clears this machine's deployment config only; the service itself stays
// in service_registry and can be configured again.
async function deleteServiceContract(serviceId) {
  if (!window.confirm('确认清除「' + serviceId + '」在本机的部署配置？（服务仍在 service_registry，可重新配置）')) return;
  try {
    await apiSend('DELETE', '/api/services/' + encodeURIComponent(serviceId));
    toast('已清除本地配置 ' + serviceId, 'ok');
    if (editingServiceID === serviceId) resetServiceForm();
    await refreshServices();
  } catch (e) {
    toast('清除失败：' + e.message, 'err');
  }
}

$('#svc-save').addEventListener('click', saveServiceContract);
$('#svc-refresh').addEventListener('click', refreshServices);
$('#svc-form-cancel').addEventListener('click', resetServiceForm);
$('#svc-table tbody').addEventListener('click', (e) => {
  const editBtn = e.target.closest('[data-svc-edit]');
  if (editBtn) {
    const svc = services.find((s) => s.serviceId === editBtn.dataset.svcEdit);
    if (svc) fillServiceForm(svc);
    return;
  }
  const delBtn = e.target.closest('[data-svc-delete]');
  if (delBtn) deleteServiceContract(delBtn.dataset.svcDelete);
});

// ---- sub-tabs: 「发起」 / 「历史列表」 -------------------------------------
// Each top tab (部署流水线 / 部署任务) is split into a 发起 sub-panel and a
// 历史列表 sub-panel. A sub-tab click toggles the button + matching sub-panel.
$$('.subtabs').forEach((nav) => {
  $$('.subtab', nav).forEach((btn) => {
    btn.addEventListener('click', () => {
      selectSubTab(nav.id, btn.dataset.subtab);
      refresh();
    });
  });
});

// selectSubTab toggles a tab's sub-panels. Used by the sub-tab buttons and by
// the 发起 flow, which jumps straight to the new job's detail page.
function selectSubTab(navId, panelId) {
  const nav = $('#' + navId);
  if (!nav) return;
  $$('.subtab', nav).forEach((b) => b.classList.toggle('subtab--active', b.dataset.subtab === panelId));
  const section = nav.parentElement;
  $$('.subpanel', section).forEach((p) => p.classList.toggle('subpanel--active', p.id === panelId));
}

// Id of the currently visible sub-panel of a tab nav (e.g. 'pipe-history').
function activeSubPanel(navId) {
  const nav = $('#' + navId);
  const active = nav ? $('.subtab--active', nav) : null;
  return active ? active.dataset.subtab : '';
}

// ---- history lists (filters + server-side pagination) --------------------
// One controller per history tab. Both share the API query contract:
//   ?serviceId=&state=&triggeredByRole=&triggeredById=&ref=&deployment=
//    &version=&q=&from=&to=&page=&pageSize=
// and the response { <listKey>: [...], total, page, pageSize }.
// Filters live in an "applied" snapshot so the 3s auto-refresh never picks up
// half-typed values: only 查询 / 重置 change the applied set for auto-applying
// histories. When autoApply is disabled (部署任务历史列表), neither the filter
// controls (selects/text/page size) nor 重置 refresh on their own — 查询 is the
// only way to apply the selected filters and refresh the list.
function makeHistory(cfg) {
  const state = { page: 1, pageSize: 20, applied: {} };
  const el = (name) => $('#' + cfg.prefix + '-f-' + name);
  const autoApply = cfg.autoApply !== false;

  function readUI() {
    const out = {};
    for (const name of cfg.fields) {
      const e = el(name);
      out[name] = e ? e.value.trim() : '';
    }
    return out;
  }

  function queryString() {
    const p = new URLSearchParams();
    for (const name of cfg.fields) {
      let v = state.applied[name];
      if (!v) continue;
      // date inputs -> inclusive full-day UTC bounds on requested_at
      if (name === 'from') v = v + 'T00:00:00.000Z';
      else if (name === 'to') v = v + 'T23:59:59.999Z';
      p.set(name, v);
    }
    p.set('page', String(state.page));
    p.set('pageSize', String(state.pageSize));
    return p.toString();
  }

  async function refresh() {
    const tb = $(cfg.tableSel);
    const pager = $('#' + cfg.prefix + '-pager');
    try {
      const data = await apiGet(cfg.endpoint + '?' + queryString());
      const list = data[cfg.listKey] || [];
      const total = data.total != null ? data.total : list.length;
      const pageSize = data.pageSize || state.pageSize;
      const pages = Math.max(1, Math.ceil(total / pageSize));
      if (state.page > pages) { state.page = pages; return refresh(); }
      tb.innerHTML = list.length
        ? list.map(cfg.renderRow).join('')
        : `<tr><td colspan="${cfg.colspan}" class="muted">没有匹配的记录</td></tr>`;
      renderPager(pager, total, state.page, pageSize, (p) => { state.page = p; refresh(); });
    } catch (e) {
      tb.innerHTML = `<tr><td colspan="${cfg.colspan}" class="muted">加载失败：${esc(e.message)}</td></tr>`;
      if (pager) pager.innerHTML = '';
    }
  }

  // 查询: snapshot the form into the applied filters and jump back to page 1.
  function apply() {
    state.applied = readUI();
    state.pageSize = Number(el('pageSize') && el('pageSize').value) || 20;
    state.page = 1;
    refresh();
  }
  // 重置: clear the form + applied filters. Query-only histories (autoApply
  // disabled) just clear the form here; the list refreshes on the next 查询.
  function reset() {
    for (const name of cfg.fields) { const e = el(name); if (e) e.value = ''; }
    if (el('pageSize')) el('pageSize').value = '20';
    state.applied = {};
    state.pageSize = 20;
    state.page = 1;
    if (autoApply) refresh();
  }

  const applyBtn = $('#' + cfg.prefix + '-f-apply');
  if (applyBtn) applyBtn.addEventListener('click', apply);
  const resetBtn = $('#' + cfg.prefix + '-f-reset');
  if (resetBtn) resetBtn.addEventListener('click', reset);
  const pageSizeEl = el('pageSize');
  if (pageSizeEl && autoApply) pageSizeEl.addEventListener('change', apply);
  for (const name of cfg.fields) {
    const e = el(name);
    if (!e || !autoApply) continue;
    // Discrete choices (selects) apply at once; free text needs 查询 or Enter.
    if (e.tagName === 'SELECT') {
      e.addEventListener('change', apply);
    } else if (e.tagName === 'INPUT' && e.type !== 'date') {
      e.addEventListener('keydown', (ev) => { if (ev.key === 'Enter') { ev.preventDefault(); apply(); } });
    }
  }

  state.applied = readUI();
  return { refresh, apply, reset, state };
}

// renderPager paints "共 N 条 · 第 p/t 页" + prev/next onto a .pager element.
function renderPager(host, total, page, pageSize, setPage) {
  if (!host) return;
  const pages = Math.max(1, Math.ceil(total / pageSize));
  const cur = Math.min(Math.max(1, page), pages);
  const info = document.createElement('span');
  info.className = 'pager__info';
  info.textContent = `共 ${total} 条 · 第 ${cur}/${pages} 页`;
  const prev = document.createElement('button');
  prev.className = 'btn btn--sm';
  prev.textContent = '← 上一页';
  prev.disabled = cur <= 1;
  prev.addEventListener('click', () => setPage(cur - 1));
  const next = document.createElement('button');
  next.className = 'btn btn--sm';
  next.textContent = '下一页 →';
  next.disabled = cur >= pages;
  next.addEventListener('click', () => setPage(cur + 1));
  host.replaceChildren(info, prev, next);
}

const historyPipelines = makeHistory({
  prefix: 'pipe',
  endpoint: '/api/pipelines',
  listKey: 'pipelines',
  tableSel: '#pipe-table tbody',
  colspan: 10,
  fields: ['serviceId', 'state', 'triggeredByRole', 'triggeredById', 'ref', 'deployment', 'version', 'q', 'from', 'to'],
  renderRow: (j) => `<tr class="rowlink" data-pipe-open="${esc(j.requestId)}">
      <td class="mono">${esc(j.requestId)}</td>
      <td class="mono">${esc(j.serviceId)}</td>
      <td class="mono">${esc(j.ref)}</td>
      <td>${stateBadge(j.state)}</td>
      <td class="mono">${esc(j.deployment || '—')}</td>
      <td class="mono">${esc(j.version || '—')}</td>
      <td class="mono">${esc(j.deployRequestId || '—')}</td>
      <td>${identityCell(j)}</td>
      <td class="mono">${fmtTime(j.requestedAt)}</td>
      <td class="wrap">${esc(j.error || j.message || '—')}</td>
    </tr>`,
});

const historyDeploys = makeHistory({
  prefix: 'dep',
  endpoint: '/api/deploys',
  listKey: 'deploys',
  tableSel: '#dep-table tbody',
  colspan: 10,
  autoApply: false,
  fields: ['serviceId', 'state', 'triggeredByRole', 'triggeredById', 'deployment', 'version', 'q', 'from', 'to'],
  renderRow: (j) => `<tr class="rowlink" data-dep-open="${esc(j.requestId)}">
      <td class="mono">${esc(j.requestId)}</td>
      <td class="mono">${esc(j.serviceId)}</td>
      <td class="mono">${esc(j.deployment)}</td>
      <td>${stateBadge(j.state)}</td>
      <td class="mono">${esc(j.version || '—')}</td>
      <td>${identityCell(j)}</td>
      <td class="mono">${fmtTime(j.requestedAt)}</td>
      <td class="mono">${fmtTime(j.startedAt)}</td>
      <td class="mono">${fmtTime(j.finishedAt)}</td>
      <td class="wrap">${esc(j.error || j.message || '—')}</td>
    </tr>`,
});

function refreshPipelines() { return historyPipelines.refresh(); }
function refreshDeploys() { return historyDeploys.refresh(); }

// ---- pipelines: 发起（触发打包+部署） -------------------------------------
$('#pipe-trigger').addEventListener('click', async () => {
  const serviceId = $('#pipe-service').value;
  const ref = $('#pipe-ref').value.trim();
  if (!serviceId) { toast('请先在「服务契约」里为已登记的服务配置部署参数', 'err'); return; }
  try {
    const body = { serviceId };
    if (ref) body.ref = ref;
    const r = await apiSend('POST', '/api/deploy-notify', body);
    toast('已触发流水线 ' + r.requestId + '（' + identityLabel(identity.role, identity.id) + '）', 'ok');
    $('#pipe-ref').value = '';
    // jump to the history list and open the new pipeline's detail page
    selectSubTab('pipe-subtabs', 'pipe-history');
    openPipelineDetail(r.requestId);
  } catch (e) { toast('触发失败：' + e.message, 'err'); }
});

// ---- pipeline detail -------------------------------------------------------
let pipeDetailID = null;

$('#pipe-table tbody').addEventListener('click', (e) => {
  const tr = e.target.closest('[data-pipe-open]');
  if (!tr) return;
  openPipelineDetail(tr.dataset.pipeOpen);
});

$('#pipe-detail-back').addEventListener('click', closePipelineDetail);

function fieldRow(label, value, cls = '') {
  return `<div class="detail-field"><span class="detail-label">${esc(label)}</span>` +
    `<span class="detail-value mono ${cls}">${esc(value)}</span></div>`;
}

async function openPipelineDetail(requestId) {
  pipeDetailID = requestId;
  $('#pipe-list-view').hidden = true;
  $('#pipe-detail').hidden = false;
  $('#pipe-detail-title').textContent = '流水线 ' + requestId;
  await refreshPipelineDetail();
  $('#pipe-detail').scrollIntoView({ behavior: 'smooth', block: 'start' });
}

function closePipelineDetail() {
  pipeDetailID = null;
  $('#pipe-detail').hidden = true;
  $('#pipe-list-view').hidden = false;
}

async function refreshPipelineDetail() {
  if (!pipeDetailID) return;
  const id = pipeDetailID;
  let job = null, events = [], deploy = null;
  try {
    job = await apiGet('/api/pipelines/' + encodeURIComponent(id));
  } catch (e) {
    $('#pipe-detail-fields').innerHTML =
      `<div class="muted">加载失败：${esc(e.message)}</div>`;
    $('#pipe-detail-events').innerHTML = '';
    $('#pipe-detail-deploy-wrap').hidden = true;
    return;
  }
  try {
    const ed = await apiGet('/api/pipelines/' + encodeURIComponent(id) + '/events');
    events = ed.events || [];
  } catch { events = []; }
  // merge deploy execution events (rsync / restart / stop / start / health)
  // into the same timeline, keyed by the linked deployRequestId.
  let deployEvents = [];
  if (job.deployRequestId) {
    try {
      const dd = await apiGet('/api/deploys/' + encodeURIComponent(job.deployRequestId) + '/events');
      deployEvents = (dd.events || []).map((e) => ({ ...e, source: 'deploy' }));
    } catch { deployEvents = []; }
  }
  const merged = events.map((e) => ({ ...e, source: 'pipeline' }))
    .concat(deployEvents)
    .sort((a, b) => (a.ts || '').localeCompare(b.ts || '') || (a.id - b.id));

  const fields = $('#pipe-detail-fields');
  fields.innerHTML = [
    fieldRow('requestId', job.requestId),
    fieldRow('serviceId', job.serviceId),
    fieldRow('ref', job.ref),
    `<div class="detail-field"><span class="detail-label">状态</span>` +
      `<span class="detail-value">${stateBadge(job.state)}</span></div>`,
    fieldRow('deployment', job.deployment || '—'),
    fieldRow('version', job.version || '—'),
    fieldRow('deployRequestId', job.deployRequestId || '—'),
    `<div class="detail-field"><span class="detail-label">触发者</span>` +
      `<span class="detail-value">${identityCell(job)}</span></div>`,
    fieldRow('请求时间', fmtTime(job.requestedAt)),
    fieldRow('开始时间', fmtTime(job.startedAt)),
    fieldRow('结束时间', fmtTime(job.finishedAt)),
    `<div class="detail-field detail-field--full"><span class="detail-label">消息</span>` +
      `<span class="detail-value">${esc(job.message || '—')}</span></div>`,
    job.error
      ? `<div class="detail-field detail-field--full"><span class="detail-label">错误</span>` +
        `<span class="detail-value detail-value--err">${esc(job.error)}</span></div>`
      : '',
  ].join('');

  const evList = $('#pipe-detail-events');
  if (!merged.length) {
    evList.innerHTML = `<li class="muted">暂无事件</li>`;
  } else {
    evList.innerHTML = merged.map((ev) => {
      const cls = eventLevelClass(ev.level);
      const src = ev.source === 'deploy' ? '部署' : '流水线';
      return `<li class="evlog ${cls}">` +
        `<span class="evlog__ts mono">${fmtTime(ev.ts)}</span>` +
        `<span class="evlog__lvl">${esc(eventLevelName(ev.level))}</span>` +
        `<span class="evlog__src">${src}</span>` +
        `<span class="evlog__msg">${esc(ev.message)}</span>` +
        `</li>`;
    }).join('');
  }

  const depWrap = $('#pipe-detail-deploy-wrap');
  if (job.deployRequestId) {
    try {
      deploy = await apiGet('/api/deploys/' + encodeURIComponent(job.deployRequestId));
    } catch { deploy = null; }
  }
  if (deploy) {
    depWrap.hidden = false;
    $('#pipe-detail-deploy').innerHTML = [
      fieldRow('requestId', deploy.requestId),
      `<div class="detail-field"><span class="detail-label">状态</span>` +
        `<span class="detail-value">${stateBadge(deploy.state)}</span></div>`,
      fieldRow('deployment', deploy.deployment || '—'),
      fieldRow('version', deploy.version || '—'),
      `<div class="detail-field"><span class="detail-label">触发者</span>` +
        `<span class="detail-value">${identityCell(deploy)}</span></div>`,
      fieldRow('请求时间', fmtTime(deploy.requestedAt)),
      fieldRow('开始时间', fmtTime(deploy.startedAt)),
      fieldRow('结束时间', fmtTime(deploy.finishedAt)),
      `<div class="detail-field detail-field--full"><span class="detail-label">消息</span>` +
        `<span class="detail-value">${esc(deploy.message || '—')}</span></div>`,
      deploy.error
        ? `<div class="detail-field detail-field--full"><span class="detail-label">错误</span>` +
          `<span class="detail-value detail-value--err">${esc(deploy.error)}</span></div>`
        : '',
    ].join('');
  } else {
    depWrap.hidden = true;
  }
}

// ---- deploys: 发起（触发部署已有制品） ------------------------------------
$('#dep-trigger').addEventListener('click', async () => {
  const serviceId = $('#dep-service').value;
  const deployment = $('#dep-deployment').value.trim();
  if (!serviceId) { toast('请先在「服务契约」里为已登记的服务配置部署参数', 'err'); return; }
  if (!deployment) { toast('请输入 deployment / hash', 'err'); return; }
  try {
    const r = await apiSend('POST', '/api/deploys', { serviceId, deployment });
    toast('已提交部署 ' + r.requestId + '（' + identityLabel(identity.role, identity.id) + '）', 'ok');
    $('#dep-deployment').value = '';
    // jump to the history list and open the new deploy's detail page
    selectSubTab('dep-subtabs', 'dep-history');
    openDeployDetail(r.requestId);
  } catch (e) { toast('提交失败：' + e.message, 'err'); }
});

// ---- deploy detail --------------------------------------------------------
// Same shape as the pipeline detail: fields + the deploy's event timeline.
let depDetailID = null;

$('#dep-table tbody').addEventListener('click', (e) => {
  const tr = e.target.closest('[data-dep-open]');
  if (!tr) return;
  openDeployDetail(tr.dataset.depOpen);
});

$('#dep-detail-back').addEventListener('click', closeDeployDetail);

function openDeployDetail(requestId) {
  depDetailID = requestId;
  $('#dep-list-view').hidden = true;
  $('#dep-detail').hidden = false;
  $('#dep-detail-title').textContent = '部署 ' + requestId;
  refreshDeployDetail();
  $('#dep-detail').scrollIntoView({ behavior: 'smooth', block: 'start' });
}

function closeDeployDetail() {
  depDetailID = null;
  $('#dep-detail').hidden = true;
  $('#dep-list-view').hidden = false;
}

async function refreshDeployDetail() {
  if (!depDetailID) return;
  const id = depDetailID;
  let job = null, events = [];
  try {
    job = await apiGet('/api/deploys/' + encodeURIComponent(id));
  } catch (e) {
    $('#dep-detail-fields').innerHTML = `<div class="muted">加载失败：${esc(e.message)}</div>`;
    $('#dep-detail-events').innerHTML = '';
    return;
  }
  try {
    const ed = await apiGet('/api/deploys/' + encodeURIComponent(id) + '/events');
    events = ed.events || [];
  } catch { events = []; }

  $('#dep-detail-fields').innerHTML = [
    fieldRow('requestId', job.requestId),
    fieldRow('serviceId', job.serviceId),
    fieldRow('deployment', job.deployment || '—'),
    `<div class="detail-field"><span class="detail-label">状态</span>` +
      `<span class="detail-value">${stateBadge(job.state)}</span></div>`,
    fieldRow('version', job.version || '—'),
    `<div class="detail-field"><span class="detail-label">触发者</span>` +
      `<span class="detail-value">${identityCell(job)}</span></div>`,
    fieldRow('请求时间', fmtTime(job.requestedAt)),
    fieldRow('开始时间', fmtTime(job.startedAt)),
    fieldRow('结束时间', fmtTime(job.finishedAt)),
    `<div class="detail-field detail-field--full"><span class="detail-label">消息</span>` +
      `<span class="detail-value">${esc(job.message || '—')}</span></div>`,
    job.error
      ? `<div class="detail-field detail-field--full"><span class="detail-label">错误</span>` +
        `<span class="detail-value detail-value--err">${esc(job.error)}</span></div>`
      : '',
  ].join('');

  const list = $('#dep-detail-events');
  if (!events.length) {
    list.innerHTML = `<li class="muted">暂无事件</li>`;
  } else {
    list.innerHTML = events.map((ev) => {
      const cls = eventLevelClass(ev.level);
      return `<li class="evlog ${cls}">` +
        `<span class="evlog__ts mono">${fmtTime(ev.ts)}</span>` +
        `<span class="evlog__lvl">${esc(eventLevelName(ev.level))}</span>` +
        `<span class="evlog__src">部署</span>` +
        `<span class="evlog__msg">${esc(ev.message)}</span>` +
        `</li>`;
    }).join('');
  }
}

// ---- artifacts -------------------------------------------------------------
// Human-readable byte size (e.g. 12.3 MB) for the artifact table.
function fmtSize(n) {
  if (n === null || n === undefined || n === '') return '—';
  let v = Number(n);
  if (!Number.isFinite(v) || v <= 0) return v === 0 ? '0 B' : '—';
  const units = ['B', 'KB', 'MB', 'GB', 'TB'];
  let i = 0;
  while (v >= 1024 && i < units.length - 1) { v /= 1024; i++; }
  return (i === 0 ? v : v.toFixed(1)) + ' ' + units[i];
}
const shortCommit = (c) => (c ? String(c).slice(0, 10) : '—');

// Download / release links for an artifact row.
function artifactLinks(a) {
  const links = [];
  const dl = a.browserDownloadUrl || a.assetUrl;
  if (dl) links.push(`<a class="btn btn--sm" href="${esc(dl)}" target="_blank" rel="noopener">下载</a>`);
  if (a.releaseUrl) links.push(`<a class="btn btn--sm" href="${esc(a.releaseUrl)}" target="_blank" rel="noopener">Release</a>`);
  return links.join('') || '—';
}

async function refreshArtifacts() {
  const tbody = $('#art-table tbody');
  const serviceId = $('#art-service').value;
  const path = '/api/artifacts' + (serviceId ? '?serviceId=' + encodeURIComponent(serviceId) : '');
  try {
    const data = await apiGet(path);
    const arts = data.artifacts || [];
    if (!arts.length) {
      tbody.innerHTML = `<tr><td colspan="8" class="muted">暂无制品记录</td></tr>`;
      return;
    }
    tbody.innerHTML = arts.map((a) => `<tr>
      <td class="mono">${esc(a.serviceId)}</td>
      <td class="mono">${esc(a.tag)}</td>
      <td class="mono">${esc(a.version || '—')}</td>
      <td class="mono">${esc(shortCommit(a.commit))}</td>
      <td class="mono">${fmtSize(a.size)}</td>
      <td class="mono">${esc(a.storage || '—')}</td>
      <td class="mono">${fmtTime(a.createdAt)}</td>
      <td class="cell-actions">${artifactLinks(a)}</td>
    </tr>`).join('');
  } catch (e) {
    tbody.innerHTML = `<tr><td colspan="8" class="muted">加载失败：${esc(e.message)}</td></tr>`;
  }
}

$('#art-service').addEventListener('change', refreshArtifacts);
$('#art-refresh').addEventListener('click', refreshArtifacts);

$('#art-scan').addEventListener('click', async () => {
  const serviceId = $('#art-service').value;
  if (!serviceId) { toast('请先选择一个服务再扫描', 'err'); return; }
  $('#art-msg').textContent = '扫描中…';
  try {
    const r = await apiSend('POST', '/api/artifacts/scan?serviceId=' + encodeURIComponent(serviceId));
    toast(`扫描完成：发现 ${r.scanned}，记录 ${r.recorded}`, 'ok');
    refreshArtifacts();
  } catch (e) {
    toast('扫描失败：' + e.message, 'err');
  } finally {
    $('#art-msg').textContent = '';
  }
});

// ---- refresh loop ---------------------------------------------------------
function refreshActiveTab() {
  const active = $('#tabs .tab--active').dataset.tab;
  if (active === 'pipelines') {
    // only the 历史列表 sub-panel has a list to poll
    if (activeSubPanel('pipe-subtabs') !== 'pipe-history') return;
    if (pipeDetailID) refreshPipelineDetail();
    else refreshPipelines();
  }
  else if (active === 'deploys') {
    if (activeSubPanel('dep-subtabs') !== 'dep-history') return;
    if (depDetailID) refreshDeployDetail();
    else refreshDeploys();
  }
  else if (active === 'artifacts') refreshArtifacts();
  else if (active === 'meta') refreshMeta();
  else if (active === 'services') refreshServices();
}

function refresh() {
  refreshHealth();
  refreshActiveTab();
}

let timer = null;
function startPolling() {
  if (timer) return;
  timer = setInterval(() => {
    if ($('#autorefresh').checked) refreshActiveTab();
  }, POLL_MS);
}
function stopPolling() { clearInterval(timer); timer = null; }

$('#autorefresh').addEventListener('change', () => {
  if ($('#autorefresh').checked) startPolling(); else stopPolling();
});

// init
resetServiceForm();
refreshServices();
refresh();
startPolling();
