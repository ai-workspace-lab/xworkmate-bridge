import http from 'node:http';
import https from 'node:https';
import { readFileSync } from 'node:fs';
import { randomBytes, createHash, timingSafeEqual } from 'node:crypto';
import { Readable } from 'node:stream';
import { pipeline } from 'node:stream/promises';
import { pathToFileURL } from 'node:url';

const digest = value => createHash('sha256').update(value).digest();
const equal = (a, b) => typeof a === 'string' && timingSafeEqual(digest(a), digest(b));
const noStore = { 'cache-control': 'no-store', 'x-content-type-options': 'nosniff',
  'referrer-policy': 'no-referrer', 'content-security-policy': "default-src 'self'; frame-ancestors 'none'; base-uri 'none'; form-action 'self'" };
const json = (res, status, body, headers = {}) => {
  if (res.destroyed) return;
  res.writeHead(status, { ...noStore, 'content-type': 'application/json', ...headers });
  res.end(JSON.stringify(body));
};
function clean(value) {
  if (Array.isArray(value)) return value.map(clean);
  if (!value || typeof value !== 'object') return value;
  return Object.fromEntries(Object.entries(value).map(([key, item]) => [key,
    /^(?:key|keys|authorization|.*(?:api[_-]?key|token|secret|password|private[_-]?key))$/i.test(key) ? '[redacted]' : clean(item)]));
}
async function body(req) {
  let size = 0; const chunks = [];
  for await (const chunk of req) {
    if ((size += chunk.length) > 1024 * 1024) throw Object.assign(new Error(), { status: 413 });
    chunks.push(chunk);
  }
  return Buffer.concat(chunks);
}
export function createGateway(config) {
  const { token, origin, upstreams, newapi = {}, ttlMs = 15 * 60 * 1000,
    now = Date.now, fetcher = fetch, tls } = config;
  if (typeof token !== 'string' || token.length < 24 || /\s/.test(token)) throw new Error('Workspace token required');
  const site = new URL(origin);
  if (!['localhost', '127.0.0.1', '[::1]'].includes(site.hostname) || !['http:', 'https:'].includes(site.protocol)) throw new Error('Loopback origin required');
  if (site.protocol !== 'https:' && !config.testMode) throw new Error('TLS required');
  for (const name of ['bridge', 'qmd', 'dsh', 'cpa', 'newapi']) {
    const u = new URL(upstreams[name]);
    if (u.protocol !== 'http:' || u.username || u.password || u.search || u.hash || u.pathname !== '/') throw new Error('Invalid fixed upstream');
    if (!['xworkmate-bridge', 'qmd', 'dsh', 'cpa', 'newapi', '127.0.0.1', 'localhost'].includes(u.hostname)) throw new Error('Upstream not allowed');
  }
  const sessions = new Map(), attempts = new Map();
  let admin = null, adminPending = null;
  async function adminAuth(signal) {
    if (admin && admin.expires > now()) return admin;
    if (!newapi.username || !newapi.password) throw Object.assign(new Error(), { status: 503 });
    if (adminPending) return adminPending;
    adminPending = (async () => {
      const enc = await fetcher(upstreams.newapi + '/api/user/login/encryption-key', { signal, redirect: 'manual' });
      if (!enc.ok || (await enc.json()).data?.enabled) throw new Error('Interactive admin login required');
      const response = await fetcher(upstreams.newapi + '/api/user/login', {
        method: 'POST', signal, redirect: 'manual', headers: { 'content-type': 'application/json' },
        body: JSON.stringify({ username: newapi.username, password: newapi.password }),
      });
      const value = await response.json();
      const data = value.data;
      if (!response.ok || value.success !== true || !Number.isInteger(data?.user?.id) || data.user.id <= 0 ||
          typeof data.access_token !== 'string' || !data.access_token || data.token_type?.toLowerCase() !== 'bearer') throw new Error('Admin authentication failed');
      admin = { key: data.access_token, user: String(data.user.id), expires: now() + 4 * 60 * 1000 };
      return admin;
    })().finally(() => { adminPending = null; });
    return adminPending;
  }
  async function handler(req, res) {
    try {
      if (req.headers.host !== site.host) return json(res, 421, { error: 'Unexpected host' });
      if (req.url.length > 8192 || /%(?:2f|5c|2e|25)|\\|(?:^|\/)\.\.?(?:\/|$)/i.test(req.url.split('?')[0])) return json(res, 400, { error: 'Invalid path' });
      const url = new URL(req.url, origin), path = url.pathname;
      const changed = !['GET', 'HEAD', 'OPTIONS'].includes(req.method);
      if (req.headers.origin && req.headers.origin !== site.origin) return json(res, 403, { error: 'Origin rejected' });
      if (req.headers['sec-fetch-site'] === 'cross-site') return json(res, 403, { error: 'Cross-site request rejected' });
      if (path === '/live' && req.method === 'GET') return json(res, 200, { live: true });
      if (req.method === 'GET' && (path === '/' || path === '/ui.js')) {
        res.writeHead(200, { ...noStore, 'content-type': path === '/' ? 'text/html; charset=utf-8' : 'text/javascript' });
        res.end(readFileSync(new URL(path === '/' ? './index.html' : './ui.js', import.meta.url))); return;
      }
      if (path === '/workspace/login' && req.method === 'POST') {
        if (req.headers.origin !== site.origin || !/^application\/json\b/i.test(req.headers['content-type'] || '')) return json(res, 403, { error: 'Same-origin JSON login required' });
        const current = now();
        for (const [id, entry] of sessions) if (entry.expires <= current) sessions.delete(id);
        for (const [id, entry] of attempts) if (entry.until <= current) attempts.delete(id);
        const peer = req.socket.remoteAddress;
        const attempt = attempts.get(peer) || { count: 0, until: current + 60000 };
        if (attempt.count >= 5) return json(res, 429, { error: 'Try again later' });
        let input; try { input = JSON.parse((await body(req)).toString()); } catch { return json(res, 400, { error: 'Invalid JSON' }); }
        if (!equal(input.token, token)) { attempt.count++; attempts.set(peer, attempt); return json(res, 401, { error: 'Invalid workspace token' }); }
        if (sessions.size >= 128) return json(res, 503, { error: 'Session limit reached' });
        const old = /(?:^|;\s*)workspace_session=([a-f0-9]{64})(?:;|$)/.exec(req.headers.cookie || '')?.[1];
        if (old) sessions.delete(old);
        const id = randomBytes(32).toString('hex'), csrf = randomBytes(32).toString('hex');
        sessions.set(id, { csrf, expires: current + ttlMs }); attempts.delete(peer);
        return json(res, 200, { authenticated: true, csrf, expiresInSeconds: ttlMs / 1000 }, {
          'set-cookie': `workspace_session=${id}; HttpOnly; SameSite=Strict; Path=/; Max-Age=${Math.floor(ttlMs / 1000)}${site.protocol === 'https:' ? '; Secure' : ''}`,
        });
      }
      const auth = req.headers.authorization;
      if (req.rawHeaders.filter((value, index) => index % 2 === 0 && value.toLowerCase() === 'authorization').length > 1) return json(res, 401, { error: 'Ambiguous authentication rejected' });
      const bearer = typeof auth === 'string' && /^Bearer ([^\s,]+)$/.exec(auth);
      const headerAuth = !!bearer && equal(bearer[1], token);
      const cookie = /(?:^|;\s*)workspace_session=([a-f0-9]{64})(?:;|$)/.exec(req.headers.cookie || '')?.[1];
      const session = cookie && sessions.get(cookie);
      const cookieAuth = session && session.expires > now();
      if (session && !cookieAuth) sessions.delete(cookie);
      // A supplied bad Authorization cannot silently fall back to a valid cookie.
      if ((auth && !headerAuth) || (!headerAuth && !cookieAuth)) return json(res, 401, { error: 'Workspace authentication required' });
      if (!headerAuth && changed && (req.headers.origin !== site.origin || !equal(req.headers['x-workspace-csrf'], session.csrf))) return json(res, 403, { error: 'CSRF rejected' });
      if (path === '/workspace/session' && req.method === 'GET') return json(res, 200, { authenticated: true, csrf: cookieAuth ? session.csrf : null });
      if (path === '/workspace/logout' && req.method === 'POST') {
        if (cookie) sessions.delete(cookie);
        return json(res, 200, { loggedOut: true }, { 'set-cookie': `workspace_session=; HttpOnly; SameSite=Strict; Path=/; Max-Age=0${site.protocol === 'https:' ? '; Secure' : ''}` });
      }
      if (path === '/workspace/status' && req.method === 'GET') return json(res, 200, {
        singleUserPrivate: true, sharedCredentialExposureUnresolved: true,
        newapiModelCredentialConfigured: !!newapi.modelKey, inferenceVerified: false,
      });
      let name, suffix, mode = 'workspace';
      for (const candidate of ['bridge', 'qmd', 'dsh', 'cpa', 'newapi']) {
        if (path.startsWith('/' + candidate + '/')) { name = candidate; suffix = path.slice(candidate.length + 1); break; }
      }
      if (path.startsWith('/cpa-management/')) {
        name = 'cpa'; suffix = '/v8/management/' + path.slice('/cpa-management/'.length);
        if (!((req.method === 'GET' && ['/v8/management/oauth/auth-url', '/v8/management/oauth/status'].includes(suffix)) ||
              (req.method === 'POST' && suffix === '/v8/management/oauth/callback'))) return json(res, 403, { error: 'Management route not exposed' });
      } else if (name === 'cpa') {
        if (!((req.method === 'GET' && suffix === '/v1/models') ||
              (req.method === 'POST' && ['/v1/chat/completions', '/v1/responses'].includes(suffix)))) return json(res, 403, { error: 'CPA route not exposed' });
      } else if (name === 'newapi') {
        if ((req.method === 'GET' && suffix === '/v1/models') ||
            (req.method === 'POST' && ['/v1/chat/completions', '/v1/responses'].includes(suffix))) mode = 'model';
        else if ((req.method === 'GET' && ['/api/user/self', '/api/channel/', '/api/status'].includes(suffix)) ||
                 (req.method === 'POST' && /^\/api\/channel\/[1-9][0-9]*\/status$/.test(suffix))) mode = 'admin';
        else return json(res, 403, { error: 'Management route not exposed' });
      }
      if (!name || !suffix || suffix.startsWith('//')) return json(res, 404, { error: 'Route not found' });
      const controller = new AbortController();
      const timer = setTimeout(() => controller.abort(), name === 'dsh' || mode === 'model' ? 9 * 60 * 1000 : 30000);
      req.once('aborted', () => controller.abort());
      res.once('close', () => { if (!res.writableFinished) controller.abort(); });
      try {
        const headers = {};
        for (const key of ['accept', 'content-type', 'last-event-id', 'mcp-session-id', 'mcp-protocol-version', 'idempotency-key']) if (req.headers[key]) headers[key] = req.headers[key];
        if (mode === 'admin') {
          const credential = await adminAuth(controller.signal);
          headers.authorization = 'Bearer ' + credential.key; headers['new-api-user'] = credential.user;
        } else if (mode === 'model') {
          if (!newapi.modelKey) return json(res, 503, { error: 'Model credential not configured; complete provider authorization' });
          headers.authorization = 'Bearer ' + newapi.modelKey;
        } else headers.authorization = 'Bearer ' + token;
        const payload = ['GET', 'HEAD'].includes(req.method) ? undefined : await body(req);
        const response = await fetcher(upstreams[name] + suffix + url.search, {
          method: req.method, headers, body: payload, signal: controller.signal, redirect: 'manual',
        });
        if (response.status >= 300 && response.status < 400) return json(res, 502, { error: 'Upstream redirect refused' });
        if (mode === 'admin') {
          if (response.status === 401) admin = null; // Never retry an operation automatically.
          if (!response.ok) { await response.body?.cancel(); return json(res, response.status, { error: 'Management request rejected' }); }
          return json(res, response.status, clean(await response.json()));
        }
        if (!response.ok) { await response.body?.cancel(); return json(res, response.status, { error: 'Upstream request rejected' }); }
        const out = { ...noStore };
        for (const key of ['content-type', 'mcp-session-id', 'mcp-protocol-version']) if (response.headers.has(key)) out[key] = response.headers.get(key);
        res.writeHead(response.status, out);
        if (!response.body || req.method === 'HEAD') res.end();
        else await pipeline(Readable.fromWeb(response.body), res);
      } finally { clearTimeout(timer); }
    } catch (error) {
      if (res.headersSent) res.destroy();
      else json(res, error.status || 502, { error: error.status === 413 ? 'Request too large' : 'Request failed; upstream details withheld' });
    }
  }
  const server = tls ? https.createServer(tls, handler) : http.createServer(handler);
  server.headersTimeout = 15000; server.requestTimeout = 10 * 60 * 1000;
  server.maxHeadersCount = 64;
  server.on('clientError', (_error, socket) => socket.end('HTTP/1.1 400 Bad Request\r\nConnection: close\r\n\r\n'));
  server.on('close', () => { sessions.clear(); admin = null; });
  return server;
}

if (process.argv[1] && import.meta.url === pathToFileURL(process.argv[1]).href) {
  try {
    const e = process.env;
    const server = createGateway({ token: e.WORKSPACE_AUTH_TOKEN, origin: 'https://127.0.0.1:18787',
      tls: { key: readFileSync('/tls/server.key'), cert: readFileSync('/tls/server.crt') },
      upstreams: { bridge: 'http://xworkmate-bridge:8787', qmd: 'http://qmd:8181', dsh: 'http://dsh:3000', cpa: 'http://cpa:8317', newapi: 'http://newapi:3000' },
      newapi: { username: e.NEWAPI_ADMIN_USERNAME, password: e.NEWAPI_ADMIN_PASSWORD, modelKey: e.AI_GATEWAY_API_KEY },
    });
    server.listen(18787, '0.0.0.0', () => console.log('Workspace authenticated TLS entry ready'));
  } catch { console.error('Workspace gateway refused startup; validate required runtime credentials and TLS'); process.exitCode = 1; }
}
