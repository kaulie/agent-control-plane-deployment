// Standalone service configuration page. It is opened from the 服务契约 list's
// [配置] button (service-edit.html?serviceId=...), loads the merged catalog and
// PUTs only the deployment settings this control plane owns.
'use strict';

const API = ''; // same origin; API paths already begin with /api

const $ = (sel, root = document) => root.querySelector(sel);
const esc = (s) => String(s ?? '').replace(/[&<>"']/g, (c) => ({
  '&': '&amp;', '<': '&lt;', '>': '&gt;', '"': '&quot;', "'": '&#39;'
}[c]));

// ---- caller identity (same convention as app.js) ---------------------------
const IDENTITY_KEY = 'acp.identity';
const IDENTITY_DEFAULT = { role: 'user', id: 'user_001' };

function loadIdentity() {
  try {
    const raw = localStorage.getItem(IDENTITY_KEY);
    if (raw) return { ...IDENTITY_DEFAULT, ...JSON.parse(raw) };
  } catch { /* ignore malformed storage */ }
  return { ...IDENTITY_DEFAULT };
}

const identity = loadIdentity();
const identityHeaders = () => ({ identity_role: identity.role, identity_id: identity.id });

// ---- API helpers -----------------------------------------------------------
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

function setMessage(msg) {
  const el = $('#svc-msg');
  if (el) el.textContent = msg;
}

const serviceId = new URLSearchParams(window.location.search).get('serviceId') || '';
let services = [];

function setFormEnabled(enabled) {
  const save = $('#svc-save');
  if (save) save.disabled = !enabled;
}

// 契约里的 URL 只存**路径**（host+port 在部署时按目标机器拼）——这里把拼出来的真地址
// 显示出来，让人一眼看到「本机 / 远端机器上」实际会请求什么。
function isLoopbackHost(host) {
  const h = (host || '').toLowerCase();
  return h === 'localhost' || h === '::1' || /^127\./.test(h);
}

function composedPreview(path, port) {
  const p = (path || '').trim();
  if (!p) return '—';
  if (/^[a-z][a-z0-9+.-]*:\/\//i.test(p)) {
    // 完整 URL：loopback + http 的会在保存时被归一成路径（预览就按归一后的样子显示），
    // 其余（https / 外部主机）是显式端点，原样使用。
    try {
      const u = new URL(p);
      if (u.protocol === 'http:' && isLoopbackHost(u.hostname)) {
        const q = u.pathname + (u.search || '');
        return composedPreview(q, Number(u.port || 0) || port);
      }
    } catch { /* 解析不了就按外部地址显示 */ }
    return p + '（外部地址，原样使用）';
  }
  if (!port) return 'http://127.0.0.1:<port>' + (p.startsWith('/') ? p : '/' + p);
  return 'http://127.0.0.1:' + port + (p.startsWith('/') ? p : '/' + p);
}

function renderURLHint() {
  const hint = $('#svc-url-hint');
  if (!hint) return;
  const port = Number($('#svc-port').value) || 0;
  const parts = [
    '健康 ' + composedPreview($('#svc-healthUrl').value, port),
    'graceful 通知 ' + composedPreview($('#svc-restartNotifyUrl').value, port),
    'graceful 轮询 ' + composedPreview($('#svc-restartPollUrl').value, port),
  ];
  hint.textContent = 'URL 只填路径（不要写 host:port）：部署时按目标机器拼 —— ' + parts.join(' · ') +
    '。本机部署就在本机请求；远端部署会把同一个地址拿到那台机器上执行（问它自己的 127.0.0.1）。';
}

function fillForm(svc) {
  const reg = svc.registry || {};
  $('#svc-edit-title').textContent = (svc.configured ? '编辑部署配置：' : '配置部署参数：') + svc.serviceId;
  $('#svc-serviceId').value = svc.serviceId || '';
  $('#svc-serviceId').disabled = true; // serviceId always comes from the list
  $('#svc-name').value = svc.name || reg.description || '';
  $('#svc-runtimeDir').value = svc.runtimeDir || '';
  // 按平台（mac/linux）覆盖的路径：部署时按目标机器平台选。
  const perOS = svc.runtimeDirs || {};
  $('#svc-runtimeDir-darwin').value = perOS.darwin || '';
  $('#svc-runtimeDir-linux').value = perOS.linux || '';
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
  $('#svc-supervise').checked = !!svc.supervise;
  $('#svc-intervalSec').value = Number(svc.intervalSec) > 0 ? svc.intervalSec : 30;
  renderRuntimeEnvRows(svc.runtimeEnv);
  setMessage(svc.registered
    ? ''
    : '⚠ service_registry 未返回该服务（未登记 / 注册中心不可用）：已配置的仍可编辑，新建会被拒绝。');
  renderURLHint();
  renderRuntimeDirHint();
  setFormEnabled(true);
}

async function loadService() {
  if (!serviceId) {
    setMessage('缺少 serviceId 参数：请从服务列表点「配置」进入本页。');
    setFormEnabled(false);
    return;
  }
  try {
    const data = await apiGet('/api/services');
    services = data.services || [];
    const svc = services.find((s) => s.serviceId === serviceId);
    if (!svc) {
      setMessage('未找到服务 ' + serviceId + '：它可能已在注册中心下线，或本机配置已被清除。');
      setFormEnabled(false);
      return;
    }
    fillForm(svc);
  } catch (e) {
    setMessage('加载服务列表失败：' + e.message);
    setFormEnabled(false);
  }
}


// runtimeDir 分平台：默认（本机）+ macOS / Linux 各自的路径。部署时按**目标机器**的平台选，
// 远端的优先级是「该平台的路径 > 部署通道约定的 <remote-home>/<serviceId> > 默认」。
function runtimeDirForPlatform(os) {
  const map = { darwin: '#svc-runtimeDir-darwin', linux: '#svc-runtimeDir-linux' };
  const v = $(map[os]) ? $(map[os]).value.trim() : '';
  if (!v) return '';
  return { darwin: 'macOS', linux: 'Linux' }[os] + ' 机器：' + v;
}

function renderRuntimeDirHint() {
  const hint = $('#svc-runtimeDir-hint');
  if (!hint) return;
  const parts = ['本机默认 ' + ($('#svc-runtimeDir').value.trim() || '—')];
  for (const os of ['darwin', 'linux']) {
    const v = runtimeDirForPlatform(os);
    if (v) parts.push(v);
  }
  hint.textContent = 'runtimeDir 可按平台分开配（' + parts.join(' · ') +
    '）：部署时按目标机器平台选 —— 远端优先用该平台的路径，没配才用部署通道里的 <远端家目录>/<serviceId>，最后才回落到默认。';
}

// 平台在启动时自己写的变量。这里再配同名键会被盖掉，所以保存时直接拒绝。
const PLATFORM_ENV_KEYS = new Set(['SERVICE_PORT', 'PORT', 'APP_VERSION', 'DEPLOY_MACHINE', 'RUNTIME_DIR']);
const RUNTIME_ENV_KEY = /^[A-Za-z_][A-Za-z0-9_]*$/;
const MAX_RUNTIME_ENV = 64;
const MAX_RUNTIME_ENV_VALUE = 8192;

function renderRuntimeEnvRows(env) {
  const box = $('#svc-runtime-env-rows');
  if (!box) return;
  box.replaceChildren();
  const entries = Object.entries(env || {}).sort((a, b) => a[0].localeCompare(b[0]));
  for (const [key, value] of entries) addRuntimeEnvRow(key, value);
  renderRuntimeEnvHint();
}

function addRuntimeEnvRow(key, value) {
  const box = $('#svc-runtime-env-rows');
  const row = document.createElement('div');
  row.className = 'env-row';
  const keyInput = document.createElement('input');
  keyInput.type = 'text';
  keyInput.className = 'env-key';
  keyInput.placeholder = '变量名，例如 LOG_LEVEL';
  keyInput.spellcheck = false;
  keyInput.setAttribute('aria-label', '变量名');
  keyInput.value = key || '';
  const valueInput = document.createElement('input');
  valueInput.type = 'text';
  valueInput.className = 'env-value';
  valueInput.placeholder = '值';
  valueInput.spellcheck = false;
  valueInput.setAttribute('aria-label', '变量值');
  valueInput.value = value ?? '';
  const remove = document.createElement('button');
  remove.type = 'button';
  remove.className = 'btn btn--ghost btn--sm env-remove';
  remove.textContent = '删除';
  row.append(keyInput, valueInput, remove);
  box.append(row);
  return row;
}

function renderRuntimeEnvHint() {
  const hint = $('#svc-runtime-env-hint');
  if (!hint) return;
  const n = document.querySelectorAll('#svc-runtime-env-rows .env-row').length;
  hint.textContent = '启动时注入到目标机器的进程环境（本机部署与远端部署同一份）。' +
    '平台另外固定写入 SERVICE_PORT、PORT、APP_VERSION、DEPLOY_MACHINE、RUNTIME_DIR，同名键以平台为准，不要在这里填。' +
    '部署时间线只记录变量名，不记录值。' +
    (n ? ' 当前 ' + n + ' 项。' : ' 不填则只注入平台变量。');
}

function collectRuntimeEnv() {
  const rows = [...document.querySelectorAll('#svc-runtime-env-rows .env-row')];
  const env = {};
  const seen = new Set();
  for (const row of rows) {
    const key = row.querySelector('.env-key').value.trim();
    const value = row.querySelector('.env-value').value;
    if (!key && !String(value).trim()) continue;
    if (!key) return { error: '运行时环境变量的名字不能为空' };
    if (!RUNTIME_ENV_KEY.test(key)) {
      return { error: `运行时环境变量 ${key} 无效：名字只能包含字母、数字和下划线，且不能以数字开头` };
    }
    if (PLATFORM_ENV_KEYS.has(key)) {
      return { error: `运行时环境变量 ${key} 由平台在启动时注入，不能在这里配置` };
    }
    if (seen.has(key)) return { error: `运行时环境变量 ${key} 重复了` };
    if (String(value).includes('\n') || String(value).includes('\r')) {
      return { error: `运行时环境变量 ${key} 的值不能包含换行` };
    }
    if (String(value).length > MAX_RUNTIME_ENV_VALUE) {
      return { error: `运行时环境变量 ${key} 的值过长（上限 ${MAX_RUNTIME_ENV_VALUE}）` };
    }
    seen.add(key);
    env[key] = value;
  }
  if (seen.size > MAX_RUNTIME_ENV) return { error: `运行时环境变量最多 ${MAX_RUNTIME_ENV} 项` };
  return { env };
}

function serviceFormBody() {
  const serviceID = $('#svc-serviceId').value.trim();
  const name = $('#svc-name').value.trim();
  const runtimeDir = $('#svc-runtimeDir').value.trim();
  const healthUrl = $('#svc-healthUrl').value.trim();
  const portRaw = $('#svc-port').value.trim();
  const port = portRaw === '' ? 0 : Number(portRaw);
  const startCmd = $('#svc-startCmd').value.trim();
  const stopCmd = $('#svc-stopCmd').value.trim();
  const restartCmd = $('#svc-restartCmd').value.trim();
  if (!serviceID) return { error: '缺少 serviceId：请从服务列表点「配置」进入本页' };
  if (!runtimeDir || !healthUrl || !startCmd || !stopCmd || !restartCmd) {
    return { error: 'runtimeDir / 健康检查路径 / startCmd / stopCmd / restartCmd 为必填项' };
  }
  if (!Number.isInteger(port) || port < 1 || port > 65535) {
    return { error: '服务端口必填，且必须是 1..65535 的整数（启动时会注入 SERVICE_PORT）' };
  }
  // 端口唯一性：本机目录里已经有的服务列表就能查（后端也会再校验一次，防并发）。
  const holder = services.find((s) => s.serviceId !== serviceID && Number(s.port) === port);
  if (holder) {
    return { error: `端口 ${port} 已被服务 ${holder.serviceId} 占用；服务端口必须唯一，请换一个` };
  }
  const intervalRaw = $('#svc-intervalSec').value.trim();
  const intervalSec = intervalRaw === '' ? 30 : Number(intervalRaw);
  if (!Number.isInteger(intervalSec) || intervalSec < 2 || intervalSec > 86400) {
    return { error: '探活周期必须是 2..86400 的整数秒' };
  }
  const collected = collectRuntimeEnv();
  if (collected.error) return collected;
  const runtimeDirs = {};
  if ($('#svc-runtimeDir-darwin').value.trim()) runtimeDirs.darwin = $('#svc-runtimeDir-darwin').value.trim();
  if ($('#svc-runtimeDir-linux').value.trim()) runtimeDirs.linux = $('#svc-runtimeDir-linux').value.trim();
  return {
    serviceId: serviceID,
    body: {
      // gitRepoUrl 不在这里发送：它来自 service_registry，本机不能改（后端也会拒绝改）。
      name, runtimeDir, healthUrl, port, startCmd, stopCmd, restartCmd,
      runtimeDirs,
      defaultBranch: $('#svc-defaultBranch').value.trim(),
      restartNotifyUrl: $('#svc-restartNotifyUrl').value.trim(),
      restartPollUrl: $('#svc-restartPollUrl').value.trim(),
      gracefulRestartMaxWaitMs: Number($('#svc-gracefulRestartMaxWaitMs').value) || 0,
      supervise: $('#svc-supervise').checked,
      intervalSec,
      runtimeEnv: collected.env,
    },
  };
}

const runtimeEnvRows = $('#svc-runtime-env-rows');
if (runtimeEnvRows) {
  runtimeEnvRows.addEventListener('click', (e) => {
    const btn = e.target.closest('.env-remove');
    if (!btn) return;
    btn.closest('.env-row').remove();
    renderRuntimeEnvHint();
  });
  runtimeEnvRows.addEventListener('input', renderRuntimeEnvHint);
}
const runtimeEnvAdd = $('#svc-runtime-env-add');
if (runtimeEnvAdd) {
  runtimeEnvAdd.addEventListener('click', () => {
    const row = addRuntimeEnvRow('', '');
    renderRuntimeEnvHint();
    const key = row.querySelector('.env-key');
    if (key) key.focus();
  });
}

for (const id of ['#svc-runtimeDir', '#svc-runtimeDir-darwin', '#svc-runtimeDir-linux']) {
  const el = $(id);
  if (el) el.addEventListener('input', renderRuntimeDirHint);
}

for (const id of ['#svc-healthUrl', '#svc-restartNotifyUrl', '#svc-restartPollUrl', '#svc-port']) {
  const el = $(id);
  if (el) el.addEventListener('input', renderURLHint);
}

async function saveService() {
  const built = serviceFormBody();
  if (built.error) { toast(built.error, 'err'); setMessage(built.error); return; }
  try {
    await apiSend('PUT', '/api/services/' + encodeURIComponent(built.serviceId), built.body);
    toast('已保存 ' + built.serviceId + ' 的部署配置', 'ok');
    setMessage('已保存 ' + built.serviceId + ' 的部署配置，可返回服务列表查看。');
  } catch (e) {
    toast('保存失败：' + e.message, 'err');
    setMessage('保存失败：' + e.message);
  }
}

$('#svc-save').addEventListener('click', saveService);

loadService();

