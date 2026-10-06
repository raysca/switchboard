const http = require('http');
const fs = require('fs');
const path = require('path');

const PORT = Number(process.env.PORT || 3000);
const HOST = process.env.HOST || '127.0.0.1';
const INDEX = path.join(__dirname, 'index.html');

function sendJson(res, status, body) {
  res.writeHead(status, { 'Content-Type': 'application/json' });
  res.end(JSON.stringify(body));
}

// POST /api/voice-token {adminUrl, managementKey, agentId, principal?}
// Relays to {adminUrl}/api/v1/voice-tokens so the browser needs no CORS on the
// admin API. The management key comes from the page and is never stored here.
async function proxyVoiceToken(req, res) {
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
  const { adminUrl, managementKey, agentId } = input || {};
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
