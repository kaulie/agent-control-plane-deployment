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

function fillForm(svc) {
  const reg = svc.registry || {};
  $('#svc-edit-title').textContent = (svc.configured ? '编辑部署配置：' : '配置部署参数：') + svc.serviceId;
  $('#svc-serviceId').value = svc.serviceId || '';
  $('#svc-serviceId').disabled = true; // serviceId always comes from the list
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
  setMessage(svc.registered
    ? ''
    : '⚠ service_registry 未返回该服务（未登记 / 注册中心不可用）：已配置的仍可编辑，新建会被拒绝。');
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
    return { error: 'runtimeDir / healthUrl / startCmd / stopCmd / restartCmd 为必填项' };
  }
  if (!Number.isInteger(port) || port < 1 || port > 65535) {
    return { error: '服务端口必填，且必须是 1..65535 的整数（启动时会注入 SERVICE_PORT）' };
  }
  // 端口唯一性：本机目录里已经有的服务列表就能查（后端也会再校验一次，防并发）。
  const holder = services.find((s) => s.serviceId !== serviceID && Number(s.port) === port);
  if (holder) {
    return { error: `端口 ${port} 已被服务 ${holder.serviceId} 占用；服务端口必须唯一，请换一个` };
  }
  return {
    serviceId: serviceID,
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

