'use strict';
// Hermetic DOM-contract tests: no browser process, package install, or network.
const { test } = require('node:test');
const assert = require('node:assert/strict');
const fs = require('node:fs');
const path = require('node:path');
const vm = require('node:vm');
const html = fs.readFileSync(path.join(__dirname, 'index.html'), 'utf8');
const script = html.split('<script>')[1].split('</script>')[0];

class Element {
  constructor(tag = 'div') { this.tag = tag; this.children = []; this.handlers = {}; this.dataset = {}; this.value = ''; this.checked = false; this.disabled = false; this._text = ''; }
  set textContent(value) { this._text = String(value); this.children = []; }
  get textContent() { return this._text + this.children.map(child => child.textContent).join('\n'); }
  set innerHTML(_) { throw new Error('HTML interpolation is forbidden'); }
  set outerHTML(_) { throw new Error('HTML interpolation is forbidden'); }
  appendChild(child) { this.children.push(child); return child; }
  replaceChildren(...children) { this._text = ''; this.children = children; }
  addEventListener(name, handler) { this.handlers[name] = handler; }
  dispatch(name) { return this.handlers[name]?.({ preventDefault() {} }); }
}
const configuration = {
  mode: 'shadow', providers: ['antigravity', 'codex'], 'refresh-concurrency': 2,
  'antigravity-interval': '5m', 'codex-interval': '15m', 'refresh-min-interval': '30s',
  'max-wait': '2s', 'max-in-flight': 4, 'unknown-max-in-flight': 1, 'queue-limit': 64
};
const payload = '<img src=x onerror=alert(1)>';
function state() {
  return {
    config: configuration, runtime_mode: 'shadow', total: 1, server_time: '2026-10-24T00:00:00Z',
    events: [{ reason: payload }], accounts: [{
      auth_id: payload, provider: 'antigravity', mode: 'shadow', state: 'ready', reason: payload,
      age_seconds: null, observed_at: '0001-01-01T00:00:00Z', expires_at: '0001-01-01T00:00:00Z',
      in_flight: 0, weight: 0.123456789, allowed: true, disabled: false,
      pools: [{ id: 'gemini', models: [payload], windows: [{ id: payload, duration_seconds: 300, remaining: 0.0000123456789, reset_at: '2026-10-24T01:00:00Z' }] }]
    }]
  };
}
function harness() {
  const nodes = new Map([...html.matchAll(/\bid="([^"]+)"/g)].map(match => [match[1], new Element()]));
  nodes.get('page-size').value = '50';
  nodes.get('migration').value = '{"schema_version":1}';
  const document = { visibilityState: 'visible', getElementById: id => nodes.get(id), createElement: tag => new Element(tag) };
  const calls = [], timers = [], events = {};
  let nextResponse;
  const context = vm.createContext({
    document, URLSearchParams, AbortController,
    window: { setInterval(callback, delay) { timers.push({ callback, delay }); }, addEventListener(event, callback) { events[event] = callback; }, confirm() { return true; } },
    fetch: async (url, options) => {
      calls.push({ url, options });
      if (nextResponse) { const result = nextResponse; nextResponse = undefined; return result; }
      return { status: 200, ok: true, json: async () => url.includes('status?') ? state() : url.endsWith('/config') ? { config: JSON.parse(options.body) } : { queued: true, dry_run: true } };
    }
  });
  vm.runInContext(script, context);
  return { nodes, document, calls, timers, events, respondWith(response) { nextResponse = response; } };
}
const flush = () => new Promise(resolve => setImmediate(resolve));
async function connect(h) {
  h.nodes.get('key').value = 'memory-only-test-key';
  h.nodes.get('connect-form').dispatch('submit');
  await flush();
}

test('renders untrusted text without HTML, preserves quota precision and uses only authenticated fixed API paths', async () => {
  const h = harness();
  assert.equal(h.calls.length, 0);
  await connect(h);
  assert.match(h.nodes.get('accounts').textContent, /<img src=x onerror=alert\(1\)>/);
  assert.match(h.nodes.get('accounts').textContent, /0\.0000123456789/);
  assert.match(h.nodes.get('accounts').textContent, /年龄未知/);
  assert.match(h.nodes.get('events').textContent, /<img src=x/);
  assert.equal(h.nodes.get('key').value, '');
  const row = h.nodes.get('accounts').children[0];
  await row.children[6].children[0].dispatch('click');
  const request = h.calls.at(-1);
  assert.equal(request.url, '/v0/management/native-quota/refresh');
  assert.deepEqual(JSON.parse(request.options.body), { auth_id: payload });
  for (const call of h.calls) {
    assert.ok(call.url.startsWith('/v0/management/native-quota/'));
    assert.ok(!call.url.includes('memory-only-test-key'));
    assert.equal(call.options.headers.Authorization, 'Bearer memory-only-test-key');
    assert.equal(call.options.credentials, 'omit');
    assert.equal(call.options.redirect, 'error');
    assert.equal(call.options.cache, 'no-store');
  }
});

test('polling is opt-in at 30 seconds, visible only, and preserves unsaved config', async () => {
  const h = harness();
  await connect(h);
  assert.equal(h.timers.length, 1);
  assert.equal(h.timers[0].delay, 30000);
  h.timers[0].callback(); await flush();
  assert.equal(h.calls.length, 1);
  h.nodes.get('auto').checked = true;
  h.document.visibilityState = 'hidden';
  h.timers[0].callback(); await flush();
  assert.equal(h.calls.length, 1);
  h.nodes.get('config').value = 'unsaved edit';
  h.document.visibilityState = 'visible';
  h.timers[0].callback(); await flush();
  assert.equal(h.calls.length, 2);
  assert.equal(h.nodes.get('config').value, 'unsaved edit');
});

test('clearing memory aborts in-flight reads and never restores a late response', async () => {
  const h = harness();
  await connect(h);
  let release;
  h.respondWith(new Promise(resolve => { release = resolve; }));
  h.nodes.get('filter-form').dispatch('submit');
  h.nodes.get('forget').dispatch('click');
  assert.equal(h.calls.at(-1).options.signal.aborted, true);
  release({ status: 200, ok: true, json: async () => state() });
  await flush();
  assert.equal(h.nodes.get('config').value, '');
  assert.equal(h.nodes.get('migration').value, '');
  assert.ok(!h.nodes.get('accounts').textContent.includes(payload));
  assert.equal(h.nodes.get('auto').checked, false);
  assert.equal(h.nodes.get('save-config').disabled, true);
  const count = h.calls.length;
  h.timers[0].callback(); await flush();
  assert.equal(h.calls.length, count);
});

test('save and preview send only explicit JSON via protected mutation routes', async () => {
  const h = harness();
  await connect(h);
  h.nodes.get('config').value = JSON.stringify(configuration);
  await h.nodes.get('save-config').dispatch('click');
  assert.equal(h.calls.at(-1).url, '/v0/management/native-quota/config');
  assert.equal(h.calls.at(-1).options.method, 'PUT');
  const preview = { schema_version: 1, credential_mappings: [{ credential_id: 'old', auth_id: payload }] };
  h.nodes.get('migration').value = JSON.stringify(preview);
  await h.nodes.get('preview').dispatch('click');
  assert.equal(h.calls.at(-1).url, '/v0/management/native-quota/migration/preview');
  assert.equal(h.calls.at(-1).options.method, 'POST');
  assert.deepEqual(JSON.parse(h.calls.at(-1).options.body), preview);
  h.events.pagehide();
  assert.equal(h.nodes.get('config').value, '');
});
