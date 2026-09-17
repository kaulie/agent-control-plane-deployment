'use strict';
// Panel behavior test: the 部署任务 (deploys) history list must not refresh when
// a filter selection changes; it must refresh only when the 查询 button is
// clicked. The panel itself is plain vanilla JS, so we run it inside jsdom with
// a stubbed fetch and assert on the requests it makes.
const { test } = require('node:test');
const assert = require('node:assert');
const fs = require('node:fs');
const path = require('node:path');
const { JSDOM } = require('jsdom');

const root = path.resolve(__dirname, '..');
const html = fs.readFileSync(path.join(root, 'web', 'index.html'), 'utf8')
  .replace('<script src="app.js"></script>', '');
const appSource = fs.readFileSync(path.join(root, 'web', 'app.js'), 'utf8');

function makePanel(options = {}) {
  const calls = [];
  const requests = [];
  // 服务目录（GET /api/services）现在 = service_registry 的契约 + 本机部署配置：
  // web-cursor 已登记且已配置，event-center 已登记但未配置，acp 只有本机配置。
  const servicesPayload = options.services || {
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
  const payload = (pathname) => {
    if (pathname === '/api/services') return servicesPayload;
    if (pathname === '/api/deploys') return { deploys: [], total: 0, page: 1, pageSize: 20 };
    if (pathname === '/api/pipelines') return { pipelines: [], total: 0, page: 1, pageSize: 20 };
    if (pathname === '/health') return { ok: true };
    return {};
  };
  async function fetchMock(url, options = {}) {
    const u = new URL(url, 'http://localhost/');
    calls.push(u.pathname + u.search);
    requests.push({
      method: options.method || 'GET',
      pathname: u.pathname,
      search: u.search,
      body: options.body || '',
    });
    const data = payload(u.pathname);
    return {
      ok: true,
      status: 200,
      async json() { return data; },
      async text() { return JSON.stringify(data); },
    };
  }

  const dom = new JSDOM(html, {
    runScripts: 'dangerously',
    url: 'http://localhost/',
    pretendToBeVisual: true,
    beforeParse(window) {
      window.fetch = fetchMock;
      window.setInterval = () => 0; // keep the 3s auto-refresh from running in tests
      window.confirm = () => true;
    },
  });

  dom.window.eval(appSource);

  const deploysCalls = () => calls.filter((c) => c.startsWith('/api/deploys'));
  const servicesRequests = () => requests.filter((r) => r.pathname.startsWith('/api/services'));
  const flush = () => new Promise((resolve) => setTimeout(resolve, 0));

  return { dom, flush, deploysCalls, servicesRequests };
}

test('deploy history: filter change does not refresh; 查询 does', async (t) => {
  const { dom, flush, deploysCalls } = makePanel();
  t.after(() => dom.window.close());
  const doc = dom.window.document;

  doc.querySelector('[data-tab="deploys"]').click();
  await flush();
  doc.querySelector('#dep-subtabs [data-subtab="dep-history"]').click();
  await flush();

  const before = deploysCalls().length;
  assert.ok(before > 0, 'the history list should have loaded at least once');

  const state = doc.querySelector('#dep-f-state');
  state.value = 'failed';
  state.dispatchEvent(new dom.window.Event('change', { bubbles: true }));
  await flush();
  assert.equal(deploysCalls().length, before, 'changing a filter select must not refresh');

  doc.querySelector('#dep-f-apply').click();
  await flush();
  assert.equal(deploysCalls().length, before + 1, 'the query button must refresh exactly once');
  assert.match(deploysCalls().at(-1), /state=failed/, 'the query must send the selected filter');
});

test('deploy history: page size change also waits for 查询', async (t) => {
  const { dom, flush, deploysCalls } = makePanel();
  t.after(() => dom.window.close());
  const doc = dom.window.document;

  doc.querySelector('[data-tab="deploys"]').click();
  await flush();
  doc.querySelector('#dep-subtabs [data-subtab="dep-history"]').click();
  await flush();

  const before = deploysCalls().length;

  const pageSize = doc.querySelector('#dep-f-pageSize');
  pageSize.value = '50';
  pageSize.dispatchEvent(new dom.window.Event('change', { bubbles: true }));
  await flush();
  assert.equal(deploysCalls().length, before, 'changing page size must not refresh');

  doc.querySelector('#dep-f-apply').click();
  await flush();
  assert.equal(deploysCalls().length, before + 1, 'the query button must refresh after page size change');
  assert.match(deploysCalls().at(-1), /pageSize=50/, 'the query must send the selected page size');
});

test('deploy history: text/date filters also wait for 查询', async (t) => {
  const { dom, flush, deploysCalls } = makePanel();
  t.after(() => dom.window.close());
  const doc = dom.window.document;

  doc.querySelector('[data-tab="deploys"]').click();
  await flush();
  doc.querySelector('#dep-subtabs [data-subtab="dep-history"]').click();
  await flush();

  const before = deploysCalls().length;
  assert.ok(before > 0, 'the history list should have loaded at least once');

  const fields = {
    'dep-f-triggeredById': 'user_002',
    'dep-f-deployment': 'deployment-abc',
    'dep-f-version': 'v1.2.3',
    'dep-f-q': 'restart',
    'dep-f-from': '2026-09-01',
    'dep-f-to': '2026-09-16',
  };
  for (const [id, value] of Object.entries(fields)) {
    const input = doc.querySelector('#' + id);
    input.value = value;
    input.dispatchEvent(new dom.window.Event('input', { bubbles: true }));
    input.dispatchEvent(new dom.window.Event('change', { bubbles: true }));
  }
  await flush();
  assert.equal(deploysCalls().length, before, 'typing or picking text/date filters must not refresh');

  // Enter on a text filter is also not the query trigger for the deploy history.
  doc.querySelector('#dep-f-q').dispatchEvent(
    new dom.window.KeyboardEvent('keydown', { key: 'Enter', bubbles: true }),
  );
  await flush();
  assert.equal(deploysCalls().length, before, 'Enter in a text filter must not refresh');

  doc.querySelector('#dep-f-apply').click();
  await flush();
  assert.equal(deploysCalls().length, before + 1, 'the query button must refresh exactly once');
  const lastCall = deploysCalls().at(-1);
  assert.match(lastCall, /triggeredById=user_002/);
  assert.match(lastCall, /deployment=deployment-abc/);
  assert.match(lastCall, /version=v1\.2\.3/);
  assert.match(lastCall, /q=restart/);
  assert.match(lastCall, /from=2026-09-01T00%3A00%3A00\.000Z/);
  assert.match(lastCall, /to=2026-09-16T23%3A59%3A59\.999Z/);
});

test('deploy history: remaining select filters also wait for 查询', async (t) => {
  const { dom, flush, deploysCalls } = makePanel();
  t.after(() => dom.window.close());
  const doc = dom.window.document;

  doc.querySelector('[data-tab="deploys"]').click();
  await flush();
  doc.querySelector('#dep-subtabs [data-subtab="dep-history"]').click();
  await flush();

  const before = deploysCalls().length;
  assert.ok(before > 0, 'the history list should have loaded at least once');

  const serviceId = doc.querySelector('#dep-f-serviceId');
  serviceId.value = 'acp';
  serviceId.dispatchEvent(new dom.window.Event('change', { bubbles: true }));
  const role = doc.querySelector('#dep-f-triggeredByRole');
  role.value = 'agent';
  role.dispatchEvent(new dom.window.Event('change', { bubbles: true }));
  await flush();
  assert.equal(deploysCalls().length, before, 'changing any filter select must not refresh');

  doc.querySelector('#dep-f-apply').click();
  await flush();
  assert.equal(deploysCalls().length, before + 1, 'the query button must refresh exactly once');
  const lastCall = deploysCalls().at(-1);
  assert.match(lastCall, /serviceId=acp/, 'the query must send the selected service');
  assert.match(lastCall, /triggeredByRole=agent/, 'the query must send the selected trigger role');
});

test('deploy history: 重置 also waits for 查询', async (t) => {
  const { dom, flush, deploysCalls } = makePanel();
  t.after(() => dom.window.close());
  const doc = dom.window.document;

  doc.querySelector('[data-tab="deploys"]').click();
  await flush();
  doc.querySelector('#dep-subtabs [data-subtab="dep-history"]').click();
  await flush();

  // Apply a filter first so 重置 has something to clear.
  const state = doc.querySelector('#dep-f-state');
  state.value = 'failed';
  doc.querySelector('#dep-f-apply').click();
  await flush();
  const before = deploysCalls().length;
  assert.match(deploysCalls().at(-1), /state=failed/, '查询 must first apply the selected filter');

  doc.querySelector('#dep-f-reset').click();
  await flush();
  assert.equal(deploysCalls().length, before, '重置 must not refresh the list');
  assert.equal(doc.querySelector('#dep-f-state').value, '', '重置 must clear the selected filter');
  assert.equal(doc.querySelector('#dep-f-pageSize').value, '20', '重置 must restore the default page size');

  doc.querySelector('#dep-f-apply').click();
  await flush();
  assert.equal(deploysCalls().length, before + 1, '查询 after 重置 must refresh exactly once');
  assert.doesNotMatch(deploysCalls().at(-1), /state=failed/, '查询 after 重置 must not send the cleared filter');
});


test('service contracts: tab lists the registry catalog with 登记/配置 state', async (t) => {
  const { dom, flush, servicesRequests } = makePanel();
  t.after(() => dom.window.close());
  const doc = dom.window.document;

  doc.querySelector('[data-tab="services"]').click();
  await flush();

  const gets = servicesRequests().filter((r) => r.method === 'GET');
  assert.ok(gets.length > 0, 'the service contract tab must load /api/services');

  const tbody = doc.querySelector('#svc-table tbody');
  assert.equal(tbody.children.length, 3, 'the table must render every catalog entry');
  assert.match(tbody.textContent, /web-cursor/);
  assert.match(tbody.textContent, /已登记/, 'registered services must be marked');
  assert.match(tbody.textContent, /未登记/, 'local-only services must be marked as not registered');
  assert.match(tbody.textContent, /未配置/, 'a registered service without deployment config must be marked');
  assert.match(tbody.textContent, /1\.2\.3 \/ kaulie/, 'registry version/owner must be shown');
  assert.doesNotMatch(tbody.textContent, /github\.com/, '服务列表里不展示 gitRepoUrl');
  assert.doesNotMatch(tbody.textContent, /\/health\b/, '服务列表里不展示 healthUrl');
  assert.doesNotMatch(tbody.textContent, /\/tmp\//, '服务列表里不展示 runtimeDir');
  assert.doesNotMatch(doc.querySelector('#svc-table thead').textContent, /gitRepoUrl|healthUrl|runtimeDir/,
    '表头也不该有 gitRepoUrl / healthUrl / runtimeDir 列');
  // 表头列数与每行单元格数保持一致（改列时最容易漏的地方）。
  const heads = doc.querySelectorAll('#svc-table thead th').length;
  for (const tr of doc.querySelectorAll('#svc-table tbody tr')) {
    assert.equal(tr.children.length, heads, `行单元格数(${tr.children.length}) != 表头列数(${heads})`);
  }

  // Every row can be configured; only configured rows can be cleared.
  assert.ok(doc.querySelector('[data-svc-edit="event-center"]'), 'a registry service must be configurable');
  assert.ok(doc.querySelector('[data-svc-edit="web-cursor"]'), 'a configured service must be editable');
  assert.ok(doc.querySelector('[data-svc-delete="web-cursor"]'), 'a configured service can be cleared');
  assert.ok(!doc.querySelector('[data-svc-delete="event-center"]'),
    'a service without local config has nothing to clear');
});

test('service contracts: the catalog source and status are visible', async (t) => {
  const { dom, flush } = makePanel();
  t.after(() => dom.window.close());
  const doc = dom.window.document;

  doc.querySelector('[data-tab="services"]').click();
  await flush();

  assert.equal(doc.querySelector('#svc-registry-url').textContent, 'http://127.0.0.1:4240');
  assert.match(doc.querySelector('#svc-registry-status').textContent, /在线 · 2 个服务/);
});

test('service contracts: a registry outage is shown, not hidden as "no services"', async (t) => {
  const { dom, flush } = makePanel({
    services: {
      registry: { url: 'http://127.0.0.1:4240', enabled: true, ok: false, services: 0, error: 'connection refused' },
      services: [
        {
          serviceId: 'acp', name: 'Control Plane', runtimeDir: '/tmp/acp',
          healthUrl: 'http://127.0.0.1:4220/health', startCmd: 'start', stopCmd: 'stop',
          restartCmd: 'restart', registered: false, configured: true,
        },
      ],
    },
  });
  t.after(() => dom.window.close());
  const doc = dom.window.document;

  doc.querySelector('[data-tab="services"]').click();
  await flush();

  assert.match(doc.querySelector('#svc-registry-status').textContent, /拉取失败/);
  assert.equal(doc.querySelector('#svc-registry-status').title, 'connection refused');
  const tbody = doc.querySelector('#svc-table tbody');
  assert.equal(tbody.children.length, 1, 'local config must survive a registry outage');
  assert.match(tbody.textContent, /未登记/);
});

test('service contracts: no way to create a service from the panel', async (t) => {
  const { dom, flush } = makePanel();
  t.after(() => dom.window.close());
  const doc = dom.window.document;

  doc.querySelector('[data-tab="services"]').click();
  await flush();

  assert.equal(doc.querySelector('#svc-new'), null, 'the panel must not offer a 新建 entry');
  assert.ok(!doc.querySelector('#tab-services').textContent.includes('新建服务契约'),
    'the tab must say the catalog comes from service_registry');
  assert.ok(doc.querySelector('#svc-serviceId').readOnly,
    'serviceId must come from the list, never typed');
});

test('service contracts: configuring a service fills and locks serviceId', async (t) => {
  const { dom, flush } = makePanel();
  t.after(() => dom.window.close());
  const doc = dom.window.document;

  doc.querySelector('[data-tab="services"]').click();
  await flush();
  doc.querySelector('[data-svc-edit="web-cursor"]').click();
  await flush();

  assert.equal(doc.querySelector('#svc-form-title').textContent, '编辑部署配置：web-cursor');
  assert.equal(doc.querySelector('#svc-serviceId').value, 'web-cursor');
  assert.equal(doc.querySelector('#svc-serviceId').disabled, true, 'serviceId must be locked while editing');
  assert.equal(doc.querySelector('#svc-runtimeDir').value, '/tmp/web-cursor');
  assert.equal(doc.querySelector('#svc-form-cancel').hidden, false, 'cancel must be visible while editing');
});

test('service contracts: an unconfigured registry service shows the registry repo read-only', async (t) => {
  const { dom, flush, servicesRequests } = makePanel();
  t.after(() => dom.window.close());
  const doc = dom.window.document;

  doc.querySelector('[data-tab="services"]').click();
  await flush();
  doc.querySelector('[data-svc-edit="event-center"]').click();
  await flush();

  assert.equal(doc.querySelector('#svc-form-title').textContent, '配置部署参数：event-center');
  assert.equal(doc.querySelector('#svc-serviceId').value, 'event-center');
  assert.equal(doc.querySelector('#svc-name').value, '统一事件中心', 'name falls back to the registry description');
  assert.equal(doc.querySelector('#svc-gitRepoUrl').value, 'https://github.com/kaulie/event-center',
    'gitRepoUrl is shown from the registry');
  assert.ok(doc.querySelector('#svc-gitRepoUrl').readOnly,
    'gitRepoUrl comes from service_registry and cannot be edited locally');
  assert.equal(doc.querySelector('#svc-msg').textContent, '', 'a registered service needs no warning');

  doc.querySelector('#svc-runtimeDir').value = '/tmp/event-center';
  doc.querySelector('#svc-healthUrl').value = 'http://127.0.0.1:4241/health';
  doc.querySelector('#svc-port').value = '4241'; // 服务端口必填
  doc.querySelector('#svc-startCmd').value = 'start';
  doc.querySelector('#svc-stopCmd').value = 'stop';
  doc.querySelector('#svc-restartCmd').value = 'restart';
  doc.querySelector('#svc-save').click();
  await flush();

  const put = servicesRequests().find((r) => r.method === 'PUT');
  assert.ok(put, 'save must issue a PUT request');
  assert.equal(put.pathname, '/api/services/event-center');
  assert.ok(!('gitRepoUrl' in JSON.parse(put.body)),
    'the panel must not send gitRepoUrl: it is registry-owned');
});

test('service contracts: an unregistered service warns but stays editable', async (t) => {
  const { dom, flush } = makePanel();
  t.after(() => dom.window.close());
  const doc = dom.window.document;

  doc.querySelector('[data-tab="services"]').click();
  await flush();
  doc.querySelector('[data-svc-edit="acp"]').click();
  await flush();

  assert.match(doc.querySelector('#svc-msg').textContent, /未返回该服务/,
    'the panel must say why saving a brand-new id would be rejected');
  assert.equal(doc.querySelector('#svc-serviceId').value, 'acp');
  assert.equal(doc.querySelector('#svc-form-cancel').hidden, false);
});

test('service contracts: trigger selects only list configured services', async (t) => {
  const { dom, flush } = makePanel();
  t.after(() => dom.window.close());
  const doc = dom.window.document;

  doc.querySelector('[data-tab="services"]').click();
  await flush();

  const values = Array.from(doc.querySelectorAll('#pipe-service option')).map((o) => o.value);
  assert.deepEqual(values.sort(), ['acp', 'web-cursor'],
    'an unconfigured registry service cannot be triggered');
  // ...but it stays filterable in the history lists.
  const filterValues = Array.from(doc.querySelectorAll('#pipe-f-serviceId option')).map((o) => o.value);
  assert.ok(filterValues.includes('event-center'), 'history filters keep every service id');
});

test('service contracts: save sends PUT /api/services/:id with the form body', async (t) => {
  const { dom, flush, servicesRequests } = makePanel();
  t.after(() => dom.window.close());
  const doc = dom.window.document;

  doc.querySelector('[data-tab="services"]').click();
  await flush();
  doc.querySelector('[data-svc-edit="web-cursor"]').click();
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

  const put = servicesRequests().find((r) => r.method === 'PUT');
  assert.ok(put, 'save must issue a PUT request');
  assert.equal(put.pathname, '/api/services/web-cursor');
  const body = JSON.parse(put.body);
  assert.equal(body.name, 'Web Cursor Agent');
  assert.equal(body.runtimeDir, '/tmp/runtime');
  assert.equal(body.healthUrl, 'http://127.0.0.1:4211/health');
  assert.equal(body.startCmd, 'start');
  assert.equal(body.stopCmd, 'stop');
  assert.equal(body.restartCmd, 'restart');
  assert.equal(body.gracefulRestartMaxWaitMs, 90000);
  assert.ok(!('gitRepoUrl' in body), 'gitRepoUrl is registry-owned: never sent from the panel');
});

test('service contracts: 服务端口 column shows explicit port, else 未指定', async (t) => {
  const { dom, flush } = makePanel();
  t.after(() => dom.window.close());
  const doc = dom.window.document;

  doc.querySelector('[data-tab="services"]').click();
  await flush();

  const rows = Array.from(doc.querySelectorAll('#svc-table tbody tr'));
  const rowText = (id) => rows.find((tr) => tr.textContent.includes(id)).textContent;
  assert.match(rowText('web-cursor'), /4212/, '显式配置的 port 直接显示');
  assert.ok(!rowText('web-cursor').includes('未指定'), '显式 port 不标未指定');
  assert.match(rowText('acp'), /未指定/, '老契约（没配 port）标出未指定');
  assert.match(rowText('acp'), /4220/, '未指定时仍展示 healthUrl 推导值作为参考');
  assert.match(doc.querySelector('#svc-table thead').textContent, /端口/, '表头要有「端口」列');
});

test('service contracts: 服务端口 必填，读出来并作为数字提交', async (t) => {
  const { dom, flush, servicesRequests } = makePanel();
  t.after(() => dom.window.close());
  const doc = dom.window.document;

  doc.querySelector('[data-tab="services"]').click();
  await flush();

  // 已配置的服务：表单里读出显式端口
  doc.querySelector('[data-svc-edit="web-cursor"]').click();
  await flush();
  assert.equal(doc.querySelector('#svc-port').value, '4212');
  assert.ok(doc.querySelector('#svc-port').required, '端口在表单里是必填项');

  // 老契约：端口为空 → 直接保存会被前端拦下（不发 PUT）
  doc.querySelector('[data-svc-edit="acp"]').click();
  await flush();
  assert.equal(doc.querySelector('#svc-port').value, '', '没配 port 时表单为空');
  const before = servicesRequests().filter((r) => r.method === 'PUT').length;
  doc.querySelector('#svc-save').click();
  await flush();
  assert.equal(servicesRequests().filter((r) => r.method === 'PUT').length, before,
    '端口为空时不能提交');
  assert.match(doc.querySelector('#svc-msg').textContent, /端口/, '要给出可见的必填提示');

  // 填上端口再保存 → 数字提交
  doc.querySelector('#svc-port').value = '4301';
  doc.querySelector('#svc-save').click();
  await flush();
  const put = servicesRequests().filter((r) => r.method === 'PUT').at(-1);
  assert.ok(put, '填了端口应该能保存');
  assert.equal(JSON.parse(put.body).port, 4301, '端口要作为数字提交');
});

test('service contracts: 非法服务端口（0 / 越界）不会被提交', async (t) => {
  const { dom, flush, servicesRequests } = makePanel();
  t.after(() => dom.window.close());
  const doc = dom.window.document;

  doc.querySelector('[data-tab="services"]').click();
  await flush();
  doc.querySelector('[data-svc-edit="web-cursor"]').click();
  await flush();

  for (const bad of ['0', '70000']) {
    const before = servicesRequests().filter((r) => r.method === 'PUT').length;
    doc.querySelector('#svc-port').value = bad;
    doc.querySelector('#svc-save').click();
    await flush();
    const puts = servicesRequests().filter((r) => r.method === 'PUT');
    assert.equal(puts.length, before, `port=${bad} 不能被提交`);
    assert.ok(puts.every((p) => String(JSON.parse(p.body).port) !== bad),
      `port=${bad} 不能出现在请求里`);
  }
  assert.match(doc.querySelector('#svc-msg').textContent, /端口/, '要给出可见的必填提示');
});

test('service contracts: 端口唯一性（前端先拦，不发请求）', async (t) => {
  const { dom, flush, servicesRequests } = makePanel();
  t.after(() => dom.window.close());
  const doc = dom.window.document;

  doc.querySelector('[data-tab="services"]').click();
  await flush();
  doc.querySelector('[data-svc-edit="acp"]').click();
  await flush();

  // 4212 已被 web-cursor 占用 → 前端直接给出可见提示，不发 PUT
  const before = servicesRequests().filter((r) => r.method === 'PUT').length;
  doc.querySelector('#svc-port').value = '4212';
  doc.querySelector('#svc-save').click();
  await flush();
  assert.equal(servicesRequests().filter((r) => r.method === 'PUT').length, before,
    '端口冲突时不能发 PUT');
  const msg = doc.querySelector('#svc-msg').textContent;
  assert.match(msg, /4212/, '提示要指出冲突端口');
  assert.match(msg, /web-cursor/, '提示要指出占用者');
  assert.match(msg, /唯一/, '提示要说明端口必须唯一');

  // 换一个没人用的端口 → 正常保存
  doc.querySelector('#svc-port').value = '4310';
  doc.querySelector('#svc-save').click();
  await flush();
  const put = servicesRequests().filter((r) => r.method === 'PUT').at(-1);
  assert.ok(put && JSON.parse(put.body).port === 4310, '换端口后可以保存');
});

test('service contracts: clear sends DELETE /api/services/:id (local config only)', async (t) => {
  const { dom, flush, servicesRequests } = makePanel();
  t.after(() => dom.window.close());
  const doc = dom.window.document;

  doc.querySelector('[data-tab="services"]').click();
  await flush();
  doc.querySelector('[data-svc-delete="acp"]').click();
  await flush();

  const del = servicesRequests().find((r) => r.method === 'DELETE');
  assert.ok(del, 'clearing local config must issue a DELETE request');
  assert.equal(del.pathname, '/api/services/acp');
});

