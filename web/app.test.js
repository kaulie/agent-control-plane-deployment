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

function makePanel() {
  const calls = [];
  const payload = (pathname) => {
    if (pathname === '/api/services') {
      return { services: [{ serviceId: 'web-cursor' }, { serviceId: 'acp' }] };
    }
    if (pathname === '/api/deploys') return { deploys: [], total: 0, page: 1, pageSize: 20 };
    if (pathname === '/api/pipelines') return { pipelines: [], total: 0, page: 1, pageSize: 20 };
    if (pathname === '/health') return { ok: true };
    return {};
  };
  async function fetchMock(url) {
    const u = new URL(url, 'http://localhost/');
    calls.push(u.pathname + u.search);
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
    },
  });

  dom.window.eval(appSource);

  const deploysCalls = () => calls.filter((c) => c.startsWith('/api/deploys'));
  const flush = () => new Promise((resolve) => setTimeout(resolve, 0));

  return { dom, flush, deploysCalls };
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


