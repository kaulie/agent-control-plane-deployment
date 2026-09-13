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
  const opt = { method, headers: { 'content-type': 'application/json' } };
  if (body !== undefined) opt.body = JSON.stringify(body);
  const res = await fetch(API + path, opt);
  const text = await res.text();
  let data = null;
  try { data = text ? JSON.parse(text) : null; } catch { data = { raw: text }; }
  if (!res.ok) throw new Error((data && data.error) || `${res.status} ${text}`);
  return data;
}

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

// ---- services -------------------------------------------------------------
let services = [];
async function refreshServices() {
  const tbody = $('#svc-table tbody');
  try {
    const data = await apiGet('/api/services');
    services = data.services || [];
    populateServiceSelects();
  } catch (e) {
    services = [];
    tbody.innerHTML = `<tr><td colspan="8" class="muted">加载失败：${esc(e.message)}</td></tr>`;
    return;
  }
  if (!services.length) {
    tbody.innerHTML = `<tr><td colspan="8" class="muted">暂无服务契约，点击「新建服务契约」</td></tr>`;
    return;
  }
  tbody.innerHTML = services.map((s) => {
    const graceful = (s.restartNotifyUrl && s.restartPollUrl)
      ? `<span class="badge badge--ok">on</span>`
      : `<span class="badge badge--muted">off</span>`;
    return `<tr>
      <td class="mono">${esc(s.serviceId)}</td>
      <td>${esc(s.name)}</td>
      <td class="mono wrap">${esc(s.runtimeDir)}</td>
      <td class="mono wrap">${esc(s.healthUrl)}</td>
      <td class="mono wrap">${esc(s.gitRepoUrl || '—')}</td>
      <td>${graceful}</td>
      <td class="mono">${fmtTime(s.updatedAt)}</td>
      <td class="cell-actions">
        <button class="btn btn--sm" data-svc-edit="${esc(s.serviceId)}">编辑</button>
      </td>
    </tr>`;
  }).join('');
}

function populateServiceSelects() {
  for (const id of ['#pipe-service', '#dep-service']) {
    const sel = $(id);
    const prev = sel.value;
    sel.innerHTML = services.map((s) =>
      `<option value="${esc(s.serviceId)}">${esc(s.serviceId)}</option>`).join('');
    if (services.find((s) => s.serviceId === prev)) sel.value = prev;
  }
}

// service editor: in-page form card (no modal, no forced popup)
const formCard = $('#svc-form-card');
const form = $('#svc-form');
function openServiceForm(svc) {
  $('#svc-form-title').textContent = svc ? '编辑服务契约' : '新建服务契约';
  $('#svc-delete').hidden = !svc;
  form.reset();
  if (svc) {
    form.serviceId.value = svc.serviceId;
    form.serviceId.readOnly = true;
    form.name.value = svc.name || '';
    form.runtimeDir.value = svc.runtimeDir || '';
    form.healthUrl.value = svc.healthUrl || '';
    form.startCmd.value = svc.startCmd || '';
    form.stopCmd.value = svc.stopCmd || '';
    form.restartCmd.value = svc.restartCmd || '';
    form.gitRepoUrl.value = svc.gitRepoUrl || '';
    form.defaultBranch.value = svc.defaultBranch || '';
    form.restartNotifyUrl.value = svc.restartNotifyUrl || '';
    form.restartPollUrl.value = svc.restartPollUrl || '';
    form.gracefulRestartMaxWaitMs.value = svc.gracefulRestartMaxWaitMs || '';
  } else {
    form.serviceId.readOnly = false;
  }
  $('#svc-form-msg').textContent = '';
  formCard.hidden = false;
  formCard.scrollIntoView({ behavior: 'smooth', block: 'nearest' });
}
function closeServiceForm() { formCard.hidden = true; }

$('#svc-new').addEventListener('click', () => openServiceForm(null));
$('#svc-cancel').addEventListener('click', closeServiceForm);

$('#svc-delete').addEventListener('click', async () => {
  const id = form.serviceId.value;
  if (!confirm(`确认删除服务契约「${id}」？`)) return;
  try {
    await apiSend('DELETE', '/api/services/' + encodeURIComponent(id));
    toast('已删除 ' + id, 'ok');
    closeServiceForm();
    refresh();
  } catch (e) { $('#svc-form-msg').textContent = '删除失败：' + e.message; }
});

form.addEventListener('submit', async (e) => {
  e.preventDefault();
  const id = form.serviceId.value.trim();
  const body = {
    name: form.name.value.trim(),
    runtimeDir: form.runtimeDir.value.trim(),
    healthUrl: form.healthUrl.value.trim(),
    startCmd: form.startCmd.value.trim(),
    stopCmd: form.stopCmd.value.trim(),
    restartCmd: form.restartCmd.value.trim(),
  };
  // nullable fields only sent when provided
  if (form.gitRepoUrl.value.trim() !== '') body.gitRepoUrl = form.gitRepoUrl.value.trim();
  if (form.defaultBranch.value.trim() !== '') body.defaultBranch = form.defaultBranch.value.trim();
  if (form.restartNotifyUrl.value.trim() !== '') body.restartNotifyUrl = form.restartNotifyUrl.value.trim();
  if (form.restartPollUrl.value.trim() !== '') body.restartPollUrl = form.restartPollUrl.value.trim();
  if (form.gracefulRestartMaxWaitMs.value.trim() !== '') {
    const n = parseInt(form.gracefulRestartMaxWaitMs.value, 10);
    if (!Number.isNaN(n)) body.gracefulRestartMaxWaitMs = n;
  }
  $('#svc-form-msg').textContent = '保存中…';
  try {
    await apiSend('PUT', '/api/services/' + encodeURIComponent(id), body);
    toast('已保存 ' + id, 'ok');
    closeServiceForm();
    refresh();
  } catch (err) {
    $('#svc-form-msg').textContent = '保存失败：' + err.message;
  }
});

// delegate edit buttons
$('#svc-table tbody').addEventListener('click', (e) => {
  const btn = e.target.closest('[data-svc-edit]');
  if (!btn) return;
  const svc = services.find((s) => s.serviceId === btn.dataset.svcEdit);
  if (svc) openServiceForm(svc);
});

// ---- pipelines ------------------------------------------------------------
async function refreshPipelines() {
  const tbody = $('#pipe-table tbody');
  try {
    const data = await apiGet('/api/pipelines?limit=100');
    const jobs = data.pipelines || [];
    if (!jobs.length) {
      tbody.innerHTML = `<tr><td colspan="9" class="muted">暂无流水线记录</td></tr>`;
      return;
    }
    tbody.innerHTML = jobs.map((j) => `<tr class="rowlink" data-pipe-open="${esc(j.requestId)}">
      <td class="mono">${esc(j.requestId)}</td>
      <td class="mono">${esc(j.serviceId)}</td>
      <td class="mono">${esc(j.ref)}</td>
      <td>${stateBadge(j.state)}</td>
      <td class="mono">${esc(j.deployment || '—')}</td>
      <td class="mono">${esc(j.version || '—')}</td>
      <td class="mono">${esc(j.deployRequestId || '—')}</td>
      <td class="mono">${fmtTime(j.requestedAt)}</td>
      <td class="wrap">${esc(j.error || j.message || '—')}</td>
    </tr>`).join('');
  } catch (e) {
    tbody.innerHTML = `<tr><td colspan="9" class="muted">加载失败：${esc(e.message)}</td></tr>`;
  }
}

$('#pipe-trigger').addEventListener('click', async () => {
  const serviceId = $('#pipe-service').value;
  const ref = $('#pipe-ref').value.trim();
  if (!serviceId) { toast('请先注册服务契约', 'err'); return; }
  try {
    const body = { serviceId };
    if (ref) body.ref = ref;
    const r = await apiSend('POST', '/api/deploy-notify', body);
    toast('已触发流水线 ' + r.requestId, 'ok');
    $('#pipe-ref').value = '';
    refresh();
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
  if (!events.length) {
    evList.innerHTML = `<li class="muted">暂无事件</li>`;
  } else {
    evList.innerHTML = events.map((ev) => {
      const cls = 'evlog--' + (ev.level || 'info');
      return `<li class="evlog ${cls}">` +
        `<span class="evlog__ts mono">${fmtTime(ev.ts)}</span>` +
        `<span class="evlog__lvl">${esc(ev.level || 'info')}</span>` +
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

// ---- deploys --------------------------------------------------------------
async function refreshDeploys() {
  const tbody = $('#dep-table tbody');
  try {
    const data = await apiGet('/api/deploys?limit=100');
    const jobs = data.deploys || [];
    if (!jobs.length) {
      tbody.innerHTML = `<tr><td colspan="9" class="muted">暂无部署任务</td></tr>`;
      return;
    }
    tbody.innerHTML = jobs.map((j) => `<tr>
      <td class="mono">${esc(j.requestId)}</td>
      <td class="mono">${esc(j.serviceId)}</td>
      <td class="mono">${esc(j.deployment)}</td>
      <td>${stateBadge(j.state)}</td>
      <td class="mono">${esc(j.version || '—')}</td>
      <td class="mono">${fmtTime(j.requestedAt)}</td>
      <td class="mono">${fmtTime(j.startedAt)}</td>
      <td class="mono">${fmtTime(j.finishedAt)}</td>
      <td class="wrap">${esc(j.error || j.message || '—')}</td>
    </tr>`).join('');
  } catch (e) {
    tbody.innerHTML = `<tr><td colspan="9" class="muted">加载失败：${esc(e.message)}</td></tr>`;
  }
}

$('#dep-trigger').addEventListener('click', async () => {
  const serviceId = $('#dep-service').value;
  const deployment = $('#dep-deployment').value.trim();
  if (!serviceId) { toast('请先注册服务契约', 'err'); return; }
  if (!deployment) { toast('请输入 deployment / hash', 'err'); return; }
  try {
    const r = await apiSend('POST', '/api/deploys', { serviceId, deployment });
    toast('已提交部署 ' + r.requestId, 'ok');
    $('#dep-deployment').value = '';
    refresh();
  } catch (e) { toast('提交失败：' + e.message, 'err'); }
});

// ---- refresh loop ---------------------------------------------------------
function refreshActiveTab() {
  const active = $('#tabs .tab--active').dataset.tab;
  if (active === 'services') refreshServices();
  else if (active === 'pipelines') {
    if (pipeDetailID) refreshPipelineDetail();
    else refreshPipelines();
  }
  else if (active === 'deploys') refreshDeploys();
  else if (active === 'meta') refreshMeta();
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
refresh();
startPolling();
