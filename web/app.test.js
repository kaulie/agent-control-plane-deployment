'use strict';
// Panel behavior test: the 部署任务 (deploys) history list must not refresh when
// a filter selection changes; it must refresh only when the 查询 button is
// clicked. The panel itself is plain vanilla JS, so we run it inside jsdom with
// a stubbed fetch and assert on the requests it makes.
const { test } = require('node:test');
const assert = require('node:assert');
const fs = require('node:fs');
const path = require('node:path');
const { JSDOM, VirtualConsole } = require('jsdom');

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
  const historyPayloads = options.history || {};
  const metaPayload = options.meta || {};
  const inventoryPayload = options.inventory || {
    defaultDeployMachine: 'local',
    registry: { enabled: true, ok: true, services: 1 },
    services: [{
      serviceId: 'web-cursor',
      name: 'Web Cursor',
      referenceVersion: 'v100',
      machines: [
        { machineId: 'local', host: '127.0.0.1', port: 4211, version: 'v100', state: 'succeeded' },
        { machineId: 'gpu-2', host: '10.0.0.8', port: 4211, version: 'v99', versionDrift: true, state: 'succeeded' },
      ],
    }],
  };
  // Test-visible knobs: metaError=true 让 /api/meta 一直失败，'first' 只失败第一次；
  // 测完可以让用例把它关掉（服务恢复）验证面板自愈。
  const state = { metaError: options.metaError || false, metaCalls: 0 };
  const payload = (pathname) => {
    if (pathname === '/api/services') return servicesPayload;
    if (pathname === '/api/meta') return metaPayload;
    if (pathname === '/api/deployment-inventory') return inventoryPayload;
    if (pathname === '/api/deploys') return { deploys: [], total: 0, page: 1, pageSize: 20 };
    if (pathname === '/api/pipelines') return { pipelines: [], total: 0, page: 1, pageSize: 20 };
    if (pathname === '/health') return { ok: true };
    // 清除服务配置前的确认数据：GET /api/services/:id/history
    const m = /^\/api\/services\/([^/]+)\/history$/.exec(pathname);
    if (m) {
      const id = decodeURIComponent(m[1]);
      if (historyPayloads[id]) return historyPayloads[id];
      return {
        serviceId: id,
        history: { pipelines: 0, deploys: 0, artifacts: 0, inflightPipelines: 0, inflightDeploys: 0 },
        inflight: 0,
        targets: [{ serviceId: 'agent-control-plane', name: 'Agent Control Plane', runtimeDir: '/tmp/agent-control-plane', registered: true }],
      };
    }
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
    // /api/meta 拉不到的模拟（服务升级/重启窗口）：面板必须自己兜住。
    if (u.pathname === '/api/meta' && state.metaError) {
      state.metaCalls += 1;
      if (state.metaError === true || state.metaCalls === 1) {
        return {
          ok: false,
          status: 500,
          async json() { throw new Error('500'); },
          async text() { return 'boom'; },
        };
      }
    }
    const data = payload(u.pathname);
    return {
      ok: true,
      status: 200,
      async json() { return data; },
      async text() { return JSON.stringify(data); },
    };
  }

  // jsdom 没有实现 location.reload()（升级后自动刷新会调它），它会报
  // 「Not implemented: navigation」——把它当重载次数记下来；其它 jsdom 报错照旧可见。
  let navAttempts = 0;
  const virtualConsole = new VirtualConsole();
  virtualConsole.on('jsdomError', (err) => {
    if (/Not implemented: navigation/.test(err.message)) { navAttempts += 1; return; }
    console.error('[jsdom]', err.message);
  });

  const dom = new JSDOM(html, {
    runScripts: 'dangerously',
    url: 'http://localhost/',
    pretendToBeVisual: true,
    virtualConsole,
    beforeParse(window) {
      window.fetch = fetchMock;
      window.setInterval = () => 0; // keep the 3s auto-refresh from running in tests
      window.confirm = () => true;
      // jsdom 没有实现 scrollIntoView（详情页渲染时会调用）。
      window.HTMLElement.prototype.scrollIntoView = function () {};
    },
  });

  dom.window.eval(appSource);

  const deploysCalls = () => calls.filter((c) => c.startsWith('/api/deploys'));
  const servicesRequests = () => requests.filter((r) => r.pathname.startsWith('/api/services'));
  const flush = () => new Promise((resolve) => setTimeout(resolve, 0));

  return { dom, flush, calls, requests, deploysCalls, servicesRequests, state, navAttempts: () => navAttempts };
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

  // Every row can be configured from a link to the standalone edit page; the
  // inline form and the clear button must not appear in the list.
  const webCursorEdit = doc.querySelector('[data-svc-edit="web-cursor"]');
  assert.ok(webCursorEdit, 'a configured service must be editable');
  assert.equal(webCursorEdit.tagName, 'A', '配置 must be a link, not a button');
  assert.equal(webCursorEdit.getAttribute('href'), 'service-edit.html?serviceId=web-cursor');
  const eventCenterEdit = doc.querySelector('[data-svc-edit="event-center"]');
  assert.ok(eventCenterEdit, 'a registry service must be configurable');
  assert.equal(eventCenterEdit.getAttribute('href'), 'service-edit.html?serviceId=event-center');
  assert.equal(doc.querySelector('#svc-form-card'), null, 'the services tab must not contain the inline edit form');
  assert.ok(!doc.querySelector('[data-svc-delete]'), 'the service list must not render a clear button');
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
  assert.equal(doc.querySelector('#svc-serviceId'), null,
    'the services list must not contain an inline serviceId input');
});

test('service contracts: 配置 links open the standalone edit page', async (t) => {
  const { dom, flush } = makePanel();
  t.after(() => dom.window.close());
  const doc = dom.window.document;

  doc.querySelector('[data-tab="services"]').click();
  await flush();

  const links = Array.from(doc.querySelectorAll('#svc-table tbody [data-svc-edit]'));
  assert.equal(links.length, 3, 'every catalog row must have a 配置 link');
  for (const link of links) {
    assert.equal(link.tagName, 'A', '配置 entries must be links');
    assert.equal(link.getAttribute('href'),
      'service-edit.html?serviceId=' + encodeURIComponent(link.dataset.svcEdit));
    assert.match(link.textContent, /配置/);
  }
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

// ---- 发起流水线：「打包走本机代理」选项 -----------------------------------

const triggerRequest = (requests) =>
  requests.filter((r) => r.pathname === '/api/deploy-notify').at(-1);

test('pipeline trigger: 勾选「走本机代理」→ useProxy 随请求发出', async (t) => {
  const { dom, flush, requests } = makePanel({ meta: { proxyConfigured: true, proxyEnvFile: '/tmp/dep/data/proxy.env' } });
  t.after(() => dom.window.close());
  const doc = dom.window.document;
  await flush();

  const box = doc.querySelector('#pipe-use-proxy');
  assert.ok(box, '发起卡片要有「打包走本机代理」勾选框');
  assert.equal(box.checked, true, '本机有代理配置时默认勾选');
  assert.match(doc.querySelector('#pipe-proxy-hint').textContent, /proxy\.env/, '要说明代理配置来源');

  box.checked = false; // 用户显式取消 → 直连
  doc.querySelector('#pipe-trigger').click();
  await flush();
  const direct = triggerRequest(requests);
  assert.ok(direct, 'should POST /api/deploy-notify');
  assert.equal(direct.method, 'POST');
  assert.ok(!direct.body.includes('useProxy'), '不勾选时请求体不带 useProxy：' + direct.body);

  box.checked = true;
  doc.querySelector('#pipe-trigger').click();
  await flush();
  const proxied = triggerRequest(requests);
  assert.equal(JSON.parse(proxied.body).useProxy, true, '勾选后请求体带 useProxy:true');
});

test('pipeline trigger: 本机没有代理配置时不默认勾选，并给出提示', async (t) => {
  const { dom, flush } = makePanel({ meta: { proxyConfigured: false, proxyEnvFile: '/tmp/dep/data/proxy.env' } });
  t.after(() => dom.window.close());
  const doc = dom.window.document;
  await flush();

  const box = doc.querySelector('#pipe-use-proxy');
  assert.equal(box.checked, false, '没有代理配置时不该默认勾选');
  assert.match(doc.querySelector('#pipe-proxy-hint').textContent, /未检测到代理/, '要提示勾了也不生效');
});

// ---- 发起流水线：「部署机器」选项 -----------------------------------------

test('pipeline trigger: 选择「部署机器」→ targetMachine 随请求发出', async (t) => {
  const { dom, flush, requests } = makePanel({
    meta: { deployMachines: ['local', 'gpu-2'], defaultDeployMachine: 'local' },
  });
  t.after(() => dom.window.close());
  const doc = dom.window.document;
  await flush();

  const sel = doc.querySelector('#pipe-machine');
  assert.ok(sel, '发起卡片要有「部署机器」下拉');
  assert.deepEqual(Array.from(sel.options).map((o) => o.value), ['local', 'gpu-2'], '下拉列出已知机器');
  assert.equal(sel.value, 'local', '默认选中默认机器');
  assert.match(doc.querySelector('#pipe-machine-hint').textContent, /local/, '提示里说明默认机器');

  // 默认机器 → 请求体不带 targetMachine（由服务端按默认机器处理）。
  doc.querySelector('#pipe-trigger').click();
  await flush();
  const dflt = triggerRequest(requests);
  assert.ok(dflt, 'should POST /api/deploy-notify');
  assert.ok(!dflt.body.includes('targetMachine'), '默认机器不显式发送：' + dflt.body);

  // 选非默认机器 → 请求体带 targetMachine。
  sel.value = 'gpu-2';
  doc.querySelector('#pipe-trigger').click();
  await flush();
  const chosen = triggerRequest(requests);
  assert.equal(JSON.parse(chosen.body).targetMachine, 'gpu-2', '选定机器后请求体带 targetMachine');
});

// 这个 bug 的真实形态：ACP 自己升级（#69 部署）时 /api/meta 拿不到机器列表，发起页的
// 「部署机器」下拉就空着 —— 而且空着不会自己恢复，看着像功能坏了。
test('pipeline trigger: /api/meta 没给机器列表时下拉也非空（退回默认机器）', async (t) => {
  const { dom, flush } = makePanel({ meta: { port: 4220 } });
  t.after(() => dom.window.close());
  const doc = dom.window.document;
  await flush();

  const sel = doc.querySelector('#pipe-machine');
  assert.ok(sel.options.length >= 1, '下拉不能是空的');
  assert.equal(sel.value, 'local', '退回默认机器 local');
  assert.match(doc.querySelector('#pipe-machine-hint').textContent, /默认机器/, '提示说明按默认机器处理');
});

test('pipeline trigger: 面板被重新部署后，已经打开的页面自动刷新一次', async (t) => {
  const meta = { panelVersion: 'panel-1', deployMachines: ['local'], defaultDeployMachine: 'local' };
  const { dom, flush, navAttempts } = makePanel({ meta });
  t.after(() => dom.window.close());
  const doc = dom.window.document;
  await flush();
  assert.equal(navAttempts(), 0, '指纹没变不该刷新');
  assert.equal(doc.querySelector('#toast').hidden, true, '指纹没变不该提示');

  // 服务端换了面板静态资源（部署把我们升级了）：下一次刷新（3s 轮询 / 切 tab）要比出来。
  meta.panelVersion = 'panel-2';
  doc.querySelector('[data-tab="pipelines"]').click();
  await flush();
  await new Promise((r) => setTimeout(r, 400)); // 等 toast 里的 reload
  assert.match(doc.querySelector('#toast').textContent, /刷新/, '要提示面板已更新');
  assert.equal(navAttempts(), 1, '要自动重载一次，别让页面停在旧 JS 上');

  // 重载之后指纹就一致了，不会反复刷（同一个版本只刷一次）。
  meta.panelVersion = 'panel-2';
  doc.querySelector('[data-tab="pipelines"]').click();
  await flush();
  await new Promise((r) => setTimeout(r, 400));
  assert.equal(navAttempts(), 1, '同一个版本不能反复重载');
});

test('pipeline trigger: 机器下拉标出本机/远端通道', async (t) => {
  const { dom, flush, requests } = makePanel({
    meta: {
      deployMachines: ['local', '43.162.117.240'],
      defaultDeployMachine: 'local',
      deployMachineTargets: {
        local: { kind: 'local' },
        '43.162.117.240': { kind: 'ssh', host: '43.162.117.240', runtimeHome: '/home/ubuntu/runtime', platform: 'linux/amd64' },
      },
    },
  });
  t.after(() => dom.window.close());
  const doc = dom.window.document;
  await flush();

  const labels = Array.from(doc.querySelectorAll('#pipe-machine option')).map((o) => o.textContent);
  assert.match(labels[0], /local（本机）/, '本机标成「本机」');
  assert.match(labels[1], /（远端 43\.162\.117\.240 linux\/amd64）/, '远端机器标出主机与构建平台');

  // 选远端机器 → 请求体带 targetMachine（服务端会真的部署过去）。
  const sel = doc.querySelector('#pipe-machine');
  sel.value = '43.162.117.240';
  doc.querySelector('#pipe-trigger').click();
  await flush();
  const req = triggerRequest(requests);
  assert.equal(JSON.parse(req.body).targetMachine, '43.162.117.240', '远端机器随请求发出');
});

test('pipeline trigger: 机器列表的说明用服务端文案（写明数据源）', async (t) => {
  const { dom, flush } = makePanel({
    meta: {
      deployMachines: ['local'],
      defaultDeployMachine: 'local',
      deployMachineHint: '发起流水线时可选择「部署机器」…；列表来自 service-registry 登记的实例主机。',
    },
  });
  t.after(() => dom.window.close());
  const doc = dom.window.document;
  await flush();

  assert.match(doc.querySelector('#pipe-machine-hint').textContent, /service-registry/,
    '说明里要带数据源，别让面板自己编一份容易过时的文案');
});

test('pipeline trigger: /api/meta 拉失败时下拉仍可用，并在刷新里自动重试', async (t) => {
  const meta = { deployMachines: ['local', 'gpu-2'], defaultDeployMachine: 'local' };
  const { dom, flush, requests, state } = makePanel({ meta, metaError: true });
  t.after(() => dom.window.close());
  const doc = dom.window.document;
  await flush();

  const sel = doc.querySelector('#pipe-machine');
  assert.ok(sel.options.length >= 1, '拉失败也不能是空下拉');
  assert.equal(sel.value, 'local', '先按默认机器兜住');
  assert.match(doc.querySelector('#pipe-machine-hint').textContent, /暂未取到/, '提示说明没取到列表');
  const metaCalls = () => requests.filter((r) => r.pathname === '/api/meta').length;
  const before = metaCalls();
  assert.ok(before >= 1, 'init 会拉 /api/meta');

  // 服务回来了（升级结束）：下一次刷新应当重试，并把真实机器列表补上。
  state.metaError = false;
  doc.querySelector('[data-tab="pipelines"]').click();
  await flush();
  assert.ok(metaCalls() > before, '失败后必须重试，不能一直空着');
  assert.deepEqual(Array.from(sel.options).map((o) => o.value), ['local', 'gpu-2'], '重试成功后列出真实机器');
});

test('inventory tab: loads deployment-inventory and renders machine rows', async (t) => {
  const { dom, flush, calls } = makePanel({});
  t.after(() => dom.window.close());
  const doc = dom.window.document;
  await flush();
  doc.querySelector('[data-tab="inventory"]').click();
  await flush();
  assert.ok(calls.some((c) => c.startsWith('/api/deployment-inventory')), '切到机器版本 tab 应拉 inventory');
  const root = doc.querySelector('#inv-root');
  assert.match(root.textContent, /web-cursor/, '应渲染服务 id');
  assert.match(root.textContent, /v99/, '应渲染机器上的版本');
  assert.match(root.textContent, /不一致/, '版本漂移应高亮');
});


