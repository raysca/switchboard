const http = require('http');
const fs = require('fs');
const path = require('path');
const crypto = require('crypto');
const jwt = require('jsonwebtoken');

const PORT = 3000;
const SECRET_KEY = 'dev-insecure-secret-key-for-testing-only-12345';
const WS_URL = 'ws://127.0.0.1:8080/ws';

const server = http.createServer((req, res) => {
  const url = new URL(req.url, `http://${req.headers.host}`);

  // CORS headers
  res.setHeader('Access-Control-Allow-Origin', '*');
  res.setHeader('Access-Control-Allow-Methods', 'GET, POST, OPTIONS');
  res.setHeader('Access-Control-Allow-Headers', 'Content-Type, Authorization');

  if (req.method === 'OPTIONS') {
    res.writeHead(204);
    res.end();
    return;
  }

  // API: Mint Admission Token
  if (url.pathname === '/api/token') {
    const pipeline = url.searchParams.get('pipeline') || 'default';
    const now = Math.floor(Date.now() / 1000);
    const token = jwt.sign(
      {
        jti: crypto.randomUUID(),
        sub: 'web-user-demo',
        aud: 'govoice-v1-client',
        iss: 'govoice-v1',
        exp: now + 300,
        iat: now,
        pipeline: pipeline
      },
      SECRET_KEY,
      { algorithm: 'HS256', keyid: 'v1' }
    );

    res.writeHead(200, { 'Content-Type': 'application/json' });
    res.end(JSON.stringify({
      token: token,
      pipeline: pipeline,
      wsUrl: `${WS_URL}?token=${token}&pipeline=${pipeline}`
    }));
    return;
  }

  // Static File Serving
  let filePath = path.join(__dirname, url.pathname === '/' ? 'index.html' : url.pathname);
  if (!fs.existsSync(filePath)) {
    res.writeHead(404, { 'Content-Type': 'text/plain' });
    res.end('Not Found');
    return;
  }

  let contentType = 'text/html';
  if (filePath.endsWith('.js')) contentType = 'application/javascript';
  if (filePath.endsWith('.css')) contentType = 'text/css';
  if (filePath.endsWith('.json')) contentType = 'application/json';

  res.writeHead(200, { 'Content-Type': contentType });
  fs.createReadStream(filePath).pipe(res);
});

server.listen(PORT, () => {
  console.log(`\n============================================================`);
  console.log(` Pipecat RTVI Web Test Client running at:`);
  console.log(`   http://localhost:${PORT}`);
  console.log(` Connects to Go Voice Framework backend at:`);
  console.log(`   ${WS_URL}`);
  console.log(`============================================================\n`);
});
