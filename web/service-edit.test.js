'use strict';
// Behavior tests for the standalone service configuration page:
// /panel/service-edit.html?serviceId=... loads the merged catalog, fills the
// form, and PUTs only the deployment settings this control plane owns.
const { test } = require('node:test');
const assert = require('node:assert');
const fs = require('node:fs');
const path = require('node:path');
const { JSDOM } = require('jsdom');

const root = path.resolve(__dirname, '..');
const html = fs.readFileSync(path.join(root, 'web', 'service-edit.html'), 'utf8')
  .replace('<script src="service-edit.js"></script>', '');
const appSource = fs.readFileSync(path.join(root, 'web', 'service-edit.js'), 'utf8');

const servicesPayload = {
  registry: { url: 'http://127.0.0.1:4240', enabled: true, ok: true, services: 2 },
  services: [
    {
      serviceId: 'web-cursor', name: 'Web Cursor', runtimeDir: '/tmp/web-cursor',
      healthUrl: 'http://127.0.0.1:4211/health', port: 4212, startCmd: 'start', stopCmd: 'stop',
      restartCmd: 'restart', gitRepoUrl: 'https://github.com/kaulie/web-cursor',
      defaultBranch: 'main', updatedAt: '2026-09-17T00:00:00.000Z',
      registered: true, configured: true,
      registry: { name: 'web-cursor', version: '1.2.3', owner: 'kaulie', gitRepoUrl: 'https://github.com/kaulie/web-cursor' },
    },
    {
      serviceId: 'event-center', name: '', registered: true, configured: false,
      registry: {
        name: 'event-center', version: '0.9.0', owner: 'kaulie', description: '统一事件中心',
        gitRepoUrl: 'https://github.com/kaulie/event-center',
      },
    },
    {
      serviceId: 'acp', name: 'Control Plane', runtimeDir: '/tmp/acp',
      healthUrl: 'http://127.0.0.1:4220/health', startCmd: 'start', stopCmd: 'stop',
      restartCmd: 'restart', gitRepoUrl: '', defaultBranch: 'main',
      updatedAt: '2026-09-16T00:00:00.000Z', registered: false, configured: true,
    },
  ],
};

function makePage(serviceId = 'web-cursor', options = {}) {
  const calls = [];
  const requests = [];
  const payload = options.payload || servicesPayload;
  async function fetchMock(url, options = {}) {
    const u = new URL(url, 'http://localhost/');
    calls.push(u.pathname + u.search);
    requests.push({
      method: options.method || 'GET',
      pathname: u.pathname,
      search: u.search,
      body: options.body || '',
      headers: options.headers || {},
    });
    const data = u.pathname === '/api/services' ? payload : { ok: true };
    return {
      ok: true,
      status: 200,
      async json() { return data; },
      async text() { return JSON.stringify(data); },
    };
  }

  const url = 'http://localhost/service-edit.html' +
    (serviceId ? '?serviceId=' + encodeURIComponent(serviceId) : '');
  const dom = new JSDOM(html, {
    runScripts: 'dangerously',
    url,
    pretendToBeVisual: true,
    beforeParse(window) {
      window.fetch = fetchMock;
      window.setInterval = () => 0;
    },
  });
  dom.window.eval(appSource);

  const flush = () => new Promise((resolve) => setTimeout(resolve, 0));
  const puts = () => requests.filter((r) => r.method === 'PUT');
  return { dom, flush, calls, requests, puts };
}

test('service edit page: loads the selected service and locks registry-owned fields', async (t) => {
  const { dom, flush, calls } = makePage('web-cursor');
  t.after(() => dom.window.close());
  const doc = dom.window.document;

  await flush();
  assert.ok(calls.includes('/api/services'), 'the page must load /api/services');

  assert.equal(doc.querySelector('#svc-edit-title').textContent, '编辑部署配置：web-cursor');
  assert.equal(doc.querySelector('#svc-serviceId').value, 'web-cursor');
  assert.equal(doc.querySelector('#svc-serviceId').disabled, true, 'serviceId must be locked');
  assert.equal(doc.querySelector('#svc-gitRepoUrl').value, 'https://github.com/kaulie/web-cursor');
  assert.equal(doc.querySelector('#svc-gitRepoUrl').readOnly, true, 'gitRepoUrl is registry-owned');
  assert.equal(doc.querySelector('#svc-runtimeDir').value, '/tmp/web-cursor');
  assert.equal(doc.querySelector('#svc-port').value, '4212');
  assert.equal(doc.querySelector('#svc-save').disabled, false);
});

test('service edit page: an unconfigured registry service gets registry defaults', async (t) => {
  const { dom, flush } = makePage('event-center');
  t.after(() => dom.window.close());
  const doc = dom.window.document;

  await flush();
  assert.equal(doc.querySelector('#svc-edit-title').textContent, '配置部署参数：event-center');
  assert.equal(doc.querySelector('#svc-name').value, '统一事件中心',
    'name falls back to the registry description');
  assert.equal(doc.querySelector('#svc-gitRepoUrl').value, 'https://github.com/kaulie/event-center');
  assert.equal(doc.querySelector('#svc-msg').textContent, '', 'a registered service needs no warning');
});

test('service edit page: an unregistered service warns but stays editable', async (t) => {
  const { dom, flush } = makePage('acp');
  t.after(() => dom.window.close());
  const doc = dom.window.document;

  await flush();
  assert.equal(doc.querySelector('#svc-serviceId').value, 'acp');
  assert.match(doc.querySelector('#svc-msg').textContent, /未返回该服务/,
    'the page must say why saving a brand-new id would be rejected');
  assert.equal(doc.querySelector('#svc-save').disabled, false, 'existing config stays editable');
});


test('service edit page: save sends PUT /api/services/:id with the form body', async (t) => {
  const { dom, flush, puts } = makePage('web-cursor');
  t.after(() => dom.window.close());
  const doc = dom.window.document;

  await flush();
  doc.querySelector('#svc-name').value = 'Web Cursor Agent';
  doc.querySelector('#svc-runtimeDir').value = '/tmp/runtime';
  doc.querySelector('#svc-healthUrl').value = 'http://127.0.0.1:4211/health';
  doc.querySelector('#svc-startCmd').value = 'start';
  doc.querySelector('#svc-stopCmd').value = 'stop';
  doc.querySelector('#svc-restartCmd').value = 'restart';
  doc.querySelector('#svc-gracefulRestartMaxWaitMs').value = '90000';
  doc.querySelector('#svc-save').click();
  await flush();

  const put = puts().at(-1);
  assert.ok(put, 'save must issue a PUT request');
  assert.equal(put.pathname, '/api/services/web-cursor');
  const body = JSON.parse(put.body);
  assert.equal(body.name, 'Web Cursor Agent');
  assert.equal(body.runtimeDir, '/tmp/runtime');
  assert.equal(body.healthUrl, 'http://127.0.0.1:4211/health');
  assert.equal(body.port, 4212, 'port must be submitted as a number');
  assert.equal(body.startCmd, 'start');
  assert.equal(body.stopCmd, 'stop');
  assert.equal(body.restartCmd, 'restart');
  assert.equal(body.gracefulRestartMaxWaitMs, 90000);
  assert.ok(!('gitRepoUrl' in body), 'gitRepoUrl is registry-owned: never sent from the panel');
  assert.match(doc.querySelector('#svc-msg').textContent, /已保存/);
});

test('service edit page: missing serviceId disables the form with guidance', async (t) => {
  const { dom, flush } = makePage('');
  t.after(() => dom.window.close());
  const doc = dom.window.document;

  await flush();
  assert.match(doc.querySelector('#svc-msg').textContent, /缺少 serviceId/);
  assert.equal(doc.querySelector('#svc-save').disabled, true);
});

test('service edit page: an unknown service is reported and cannot be saved', async (t) => {
  const { dom, flush } = makePage('not-there');
  t.after(() => dom.window.close());
  const doc = dom.window.document;

  await flush();
  assert.match(doc.querySelector('#svc-msg').textContent, /未找到服务 not-there/);
  assert.equal(doc.querySelector('#svc-save').disabled, true);
});


test('service edit page: 端口必填且必须是合法整数', async (t) => {
  const { dom, flush, puts } = makePage('acp');
  t.after(() => dom.window.close());
  const doc = dom.window.document;

  await flush();
  assert.equal(doc.querySelector('#svc-port').value, '', '没配 port 时表单为空');

  for (const bad of ['', '0', '70000', 'abc']) {
    const before = puts().length;
    doc.querySelector('#svc-port').value = bad;
    doc.querySelector('#svc-save').click();
    await flush();
    assert.equal(puts().length, before, `port=${bad || 'empty'} 不能被提交`);
  }
  assert.match(doc.querySelector('#svc-msg').textContent, /端口/, '要给出可见的必填提示');
});

test('service edit page: 端口唯一性（前端先拦，不发请求）', async (t) => {
  const { dom, flush, puts } = makePage('acp');
  t.after(() => dom.window.close());
  const doc = dom.window.document;

  await flush();

  // 4212 已被 web-cursor 占用 → 前端直接给出可见提示，不发 PUT
  const before = puts().length;
  doc.querySelector('#svc-port').value = '4212';
  doc.querySelector('#svc-save').click();
  await flush();
  assert.equal(puts().length, before, '端口冲突时不能发 PUT');
  const msg = doc.querySelector('#svc-msg').textContent;
  assert.match(msg, /4212/, '提示要指出冲突端口');
  assert.match(msg, /web-cursor/, '提示要指出占用者');
  assert.match(msg, /唯一/, '提示要说明端口必须唯一');

  // 换一个没人用的端口 → 正常保存
  doc.querySelector('#svc-port').value = '4310';
  doc.querySelector('#svc-save').click();
  await flush();
  const put = puts().at(-1);
  assert.ok(put && JSON.parse(put.body).port === 4310, '换端口后可以保存');
});

