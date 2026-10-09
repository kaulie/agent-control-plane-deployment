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

// 契约里的 URL 只存路径（host+port 在部署时按目标机器拼）：页面要显示拼出来的真地址。
test('service edit page: URL 只填路径，页面显示部署时拼出来的地址', async (t) => {
  const { dom, flush } = makePage('web-cursor', {
    payload: {
      registry: { url: 'http://127.0.0.1:4240', enabled: true, ok: true, services: 1 },
      services: [{
        serviceId: 'web-cursor', name: 'Web Cursor', runtimeDir: '/tmp/web-cursor',
        healthUrl: '/health', port: 4211,
        restartNotifyUrl: '/api/ops/restart-notify', restartPollUrl: '/api/ops/restart-status',
        startCmd: 'start', stopCmd: 'stop', restartCmd: 'restart',
        defaultBranch: 'main', registered: true, configured: true,
      }],
    },
  });
  t.after(() => dom.window.close());
  const doc = dom.window.document;
  await flush();

  // 表单里存的就是路径本身
  assert.equal(doc.querySelector('#svc-healthUrl').value, '/health');
  // 提示里给出拼好的地址（本机 / 远端都是这个地址，只是执行位置不同）
  const hint = doc.querySelector('#svc-url-hint').textContent;
  assert.match(hint, /http:\/\/127\.0\.0\.1:4211\/health/, '要显示拼出来的健康检查地址：' + hint);
  assert.match(hint, /http:\/\/127\.0\.0\.1:4211\/api\/ops\/restart-notify/, 'graceful 通知也一样：' + hint);
  assert.match(hint, /只填路径/, '要说明只填路径：' + hint);
});

test('service edit page: 旧契约里的完整 loopback URL 归一后保存（存路径）', async (t) => {
  const { dom, flush, puts } = makePage('web-cursor', {
    payload: {
      registry: { url: 'http://127.0.0.1:4240', enabled: true, ok: true, services: 1 },
      services: [{
        serviceId: 'web-cursor', name: 'Web Cursor', runtimeDir: '/tmp/web-cursor',
        healthUrl: 'http://127.0.0.1:4211/health', port: 4211,
        startCmd: 'start', stopCmd: 'stop', restartCmd: 'restart',
        defaultBranch: 'main', registered: true, configured: true,
      }],
    },
  });
  t.after(() => dom.window.close());
  const doc = dom.window.document;
  await flush();

  // 预览按「保存后会被归一成的样子」显示：/health + 端口
  const hint = doc.querySelector('#svc-url-hint').textContent;
  assert.match(hint, /http:\/\/127\.0\.0\.1:4211\/health/, hint);

  // 用户把字段改成路径后保存 → PUT 里就是路径（服务端也会再归一一次）
  const health = doc.querySelector('#svc-healthUrl');
  health.value = 'http://127.0.0.1:4211/health';
  health.dispatchEvent(new dom.window.Event('input', { bubbles: true }));
  assert.match(doc.querySelector('#svc-url-hint').textContent, /http:\/\/127\.0\.0\.1:4211\/health/);
  doc.querySelector('#svc-save').click();
  await flush();
  const put = puts().at(-1);
  assert.ok(put, '保存要发 PUT');
  assert.equal(JSON.parse(put.body).healthUrl, 'http://127.0.0.1:4211/health', '原样提交，服务端归一成路径');
});

test('service edit page: runtimeDir 可按平台（mac/linux）分开配', async (t) => {
  const { dom, flush, puts } = makePage('web-cursor', {
    payload: {
      registry: { url: 'http://127.0.0.1:4240', enabled: true, ok: true, services: 1 },
      services: [{
        serviceId: 'web-cursor', name: 'Web Cursor',
        runtimeDir: '/Users/gaolei/runtime/web-cursor',
        runtimeDirs: { linux: '/home/ubuntu/runtime/web-cursor' },
        healthUrl: '/health', port: 4211,
        startCmd: 'start', stopCmd: 'stop', restartCmd: 'restart',
        defaultBranch: 'main', registered: true, configured: true,
      }],
    },
  });
  t.after(() => dom.window.close());
  const doc = dom.window.document;
  await flush();

  assert.equal(doc.querySelector('#svc-runtimeDir').value, '/Users/gaolei/runtime/web-cursor');
  assert.equal(doc.querySelector('#svc-runtimeDir-darwin').value, '', 'macOS 没配就是空（用默认）');
  assert.equal(doc.querySelector('#svc-runtimeDir-linux').value, '/home/ubuntu/runtime/web-cursor');
  const hint = doc.querySelector('#svc-runtimeDir-hint').textContent;
  assert.match(hint, /按平台分开配/, '要说明分平台怎么生效：' + hint);
  assert.match(hint, /Linux 机器/, hint);

  // 填上 macOS 专属路径并保存 → PUT 里带 runtimeDirs
  doc.querySelector('#svc-runtimeDir-darwin').value = '/tmp/mac-cursor';
  doc.querySelector('#svc-runtimeDir-darwin').dispatchEvent(new dom.window.Event('input', { bubbles: true }));
  doc.querySelector('#svc-save').click();
  await flush();
  const body = JSON.parse(puts().at(-1).body);
  assert.deepEqual(body.runtimeDirs, { darwin: '/tmp/mac-cursor', linux: '/home/ubuntu/runtime/web-cursor' });
  assert.equal(body.runtimeDir, '/Users/gaolei/runtime/web-cursor', '默认仍随 runtimeDir 提交');
});

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
  assert.equal(body.supervise, false, 'supervise checkbox defaults off');
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

// 字段按含义分区：十几个字段平铺成一坨很难读，页面按「标识 / 目录 / 接口 / 命令」分四区，
// 每区「小标题 + 一行说明 + 字段网格」，提示跟着它解释的字段走。
test('service edit page: 字段按含义分区（标识 / 运行目录 / 服务接口 / 监督 / 启停命令）', async (t) => {
  const { dom, flush } = makePage('web-cursor');
  t.after(() => dom.window.close());
  const doc = dom.window.document;
  await flush();

  const sections = [...doc.querySelectorAll('.form-card > section.cfg')];
  assert.deepEqual(
    sections.map((s) => s.id),
    ['cfg-identity', 'cfg-runtimedir', 'cfg-endpoints', 'cfg-supervise', 'cfg-commands'],
  );
  assert.deepEqual(
    sections.map((s) => s.querySelector('.cfg__title').firstChild.textContent.trim()),
    ['服务标识', '运行目录', '服务接口', '本机监督', '启停命令'],
  );
  for (const s of sections) {
    assert.ok(s.querySelector('.cfg__desc').textContent.trim().length > 0, s.id + ' 要有分区说明');
    assert.ok(s.querySelectorAll('.form-grid').length === 1, s.id + ' 里是一块字段网格');
  }

  // 每个字段落在与它含义相符的分区；提示跟着它解释的字段
  const owner = (sel) => doc.querySelector(sel).closest('section.cfg').id;
  const expected = {
    '#svc-serviceId': 'cfg-identity',
    '#svc-name': 'cfg-identity',
    '#svc-gitRepoUrl': 'cfg-identity',
    '#svc-defaultBranch': 'cfg-identity',
    '#svc-runtimeDir': 'cfg-runtimedir',
    '#svc-runtimeDir-darwin': 'cfg-runtimedir',
    '#svc-runtimeDir-linux': 'cfg-runtimedir',
    '#svc-runtimeDir-hint': 'cfg-runtimedir',
    '#svc-port': 'cfg-endpoints',
    '#svc-healthUrl': 'cfg-endpoints',
    '#svc-restartNotifyUrl': 'cfg-endpoints',
    '#svc-restartPollUrl': 'cfg-endpoints',
    '#svc-gracefulRestartMaxWaitMs': 'cfg-endpoints',
    '#svc-url-hint': 'cfg-endpoints',
    '#svc-supervise': 'cfg-supervise',
    '#svc-startCmd': 'cfg-commands',
    '#svc-stopCmd': 'cfg-commands',
    '#svc-restartCmd': 'cfg-commands',
  };
  for (const [sel, want] of Object.entries(expected)) {
    assert.equal(owner(sel), want, sel + ' 应属于 ' + want);
    // 分组不能靠复制控件：每个字段全页只有一个
    assert.equal(doc.querySelectorAll(sel).length, 1, sel + ' 应只有一个控件');
  }

  // 保存按钮在所有分区之外（全页只有一组操作按钮）
  const save = doc.querySelector('#svc-save');
  assert.equal(doc.querySelectorAll('#svc-save').length, 1);
  assert.equal(save.closest('section.cfg'), null, '保存按钮不应落在某个分区里');

  // 样式要跟着分区走，否则只是多了几个没层次的标题
  const css = fs.readFileSync(path.join(root, 'web', 'styles.css'), 'utf8');
  assert.match(css, /\.cfg \{/, 'styles.css 要有分区样式');
  assert.match(css, /\.cfg__title \{/);
  assert.match(css, /\.cfg__desc \{/);
  assert.match(css, /\.form-grid \.hint \{\s*grid-column: 1 \/ -1;/, '分区里的提示要横跨整行');
  assert.match(css, /\.form-card > \.form-actions \{/, '按钮区移到网格外后要有自己的样式');
});

