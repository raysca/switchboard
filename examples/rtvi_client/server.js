const http = require('http');
const fs = require('fs');
const path = require('path');

const PORT = Number(process.env.PORT || 3000);
const HOST = process.env.HOST || '127.0.0.1';
const INDEX = path.join(__dirname, 'index.html');

// Optional defaults (set by `make example-web`). With GOVOICE_MANAGEMENT_API_KEY
// set, this server mints tokens with its own key, only ever against ADMIN_URL,
// and the browser never sees the key.
const ADMIN_URL = process.env.GOVOICE_ADMIN_URL || 'http://127.0.0.1:9090';
const WS_URL = process.env.GOVOICE_WS_URL || 'ws://127.0.0.1:8080/ws';
const AGENT_ID = process.env.GOVOICE_AGENT_ID || '';
const SERVER_KEY = process.env.GOVOICE_MANAGEMENT_API_KEY || '';

function sendJson(res, status, body) {
  res.writeHead(status, { 'Content-Type': 'application/json' });
  res.end(JSON.stringify(body));
}

// POST /api/voice-token {adminUrl, managementKey, agentId, principal?}
// Relays to {adminUrl}/api/v1/voice-tokens so the browser needs no CORS on the
// admin API. The management key comes from the page and is never stored here.
async function proxyVoiceToken(req, res) {
  // Only this page may use the relay: a cross-site form or fetch from another
  // origin must not mint tokens or make this server call arbitrary URLs.
  const origin = req.headers.origin;
  if (origin && origin !== `http://${req.headers.host}`) {
    return sendJson(res, 403, { error: { code: 'forbidden_origin', message: 'cross-origin requests are not allowed' } });
  }
  if (!(req.headers['content-type'] || '').startsWith('application/json')) {
    return sendJson(res, 415, { error: { code: 'unsupported_media_type', message: 'use application/json' } });
  }
  let raw = '';
  for await (const chunk of req) {
    raw += chunk;
    if (raw.length > 16384) return sendJson(res, 413, { error: { code: 'too_large', message: 'request body too large' } });
  }
  let input;
  try {
    input = JSON.parse(raw);
  } catch {
    return sendJson(res, 400, { error: { code: 'invalid_json', message: 'request body must be JSON' } });
  }
  let { adminUrl, managementKey, agentId } = input || {};
  if (!managementKey && SERVER_KEY) {
    managementKey = SERVER_KEY;
    adminUrl = ADMIN_URL; // the server's key goes to the configured admin API only
  }
  const principal = input?.principal || 'rtvi-web-client';
  let target;
  try {
    target = new URL('/api/v1/voice-tokens', adminUrl);
    if (target.protocol !== 'http:' && target.protocol !== 'https:') throw new Error('scheme');
  } catch {
    return sendJson(res, 400, { error: { code: 'invalid_admin_url', message: 'Admin URL must be an http(s) URL' } });
  }
  if (!managementKey || !agentId) {
    return sendJson(res, 400, { error: { code: 'missing_fields', message: 'Management key and Agent ID are required' } });
  }
  try {
    const upstream = await fetch(target, {
      method: 'POST',
      headers: { 'Authorization': `Bearer ${managementKey}`, 'Content-Type': 'application/json' },
      body: JSON.stringify({ principal, agent_id: agentId })
    });
    const text = await upstream.text();
    res.writeHead(upstream.status, { 'Content-Type': 'application/json' });
    res.end(text);
  } catch (err) {
    sendJson(res, 502, { error: { code: 'admin_unreachable', message: `cannot reach ${target.origin}: ${err.message}` } });
  }
}

const server = http.createServer((req, res) => {
  const url = new URL(req.url, `http://${req.headers.host}`);

  if (url.pathname === '/api/defaults') {
    return sendJson(res, 200, { adminUrl: ADMIN_URL, wsUrl: WS_URL, agentId: AGENT_ID, serverKey: Boolean(SERVER_KEY) });
  }

  if (url.pathname === '/api/voice-token') {
    if (req.method !== 'POST') return sendJson(res, 405, { error: { code: 'method_not_allowed', message: 'use POST' } });
    proxyVoiceToken(req, res).catch(err => sendJson(res, 500, { error: { code: 'internal', message: err.message } }));
    return;
  }

  // ponytail: the page is self-contained, so index.html is the only static file.
  if (url.pathname === '/' || url.pathname === '/index.html') {
    res.writeHead(200, { 'Content-Type': 'text/html; charset=utf-8' });
    fs.createReadStream(INDEX).pipe(res);
    return;
  }
  res.writeHead(404, { 'Content-Type': 'text/plain' });
  res.end('Not Found');
});

server.listen(PORT, HOST, () => {
  console.log(`\n============================================================`);
  console.log(` Pipecat RTVI Web Test Client running at:`);
  console.log(`   http://${HOST === '0.0.0.0' ? 'localhost' : HOST}:${PORT}`);
  console.log(` Voice tokens are relayed via POST /api/voice-token`);
  console.log(`============================================================\n`);
});
