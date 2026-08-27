// Dev-only static server + API proxy. Zero dependencies (node:http only).
// Serves ui/ and forwards /v1 and /health to the NATIVE llama-server on the host,
// so the browser sees one origin (no CORS) exactly like it will on the USB stick.
import http from 'node:http';
import { readFile } from 'node:fs/promises';
import { extname, join, normalize } from 'node:path';

const PORT     = Number(process.env.PORT ?? 5173);
const UPSTREAM = process.env.LLAMA_HOST ?? 'host.docker.internal:11434';
const UI_DIR   = process.env.UI_DIR ?? new URL('../ui', import.meta.url).pathname;

const TYPES = { '.html':'text/html', '.js':'text/javascript', '.css':'text/css',
                '.json':'application/json', '.svg':'image/svg+xml', '.ico':'image/x-icon' };

const server = http.createServer(async (req, res) => {
  // ---- proxy API calls to the host's native server ----
  if (req.url.startsWith('/v1') || req.url.startsWith('/health') || req.url.startsWith('/props')) {
    const [host, port] = UPSTREAM.split(':');
    const up = http.request(
      { host, port: port ?? 80, path: req.url, method: req.method, headers: { ...req.headers, host: UPSTREAM } },
      (r) => { res.writeHead(r.statusCode, r.headers); r.pipe(res); }
    );
    up.on('error', (e) => {
      res.writeHead(502, { 'Content-Type': 'application/json' });
      res.end(JSON.stringify({ error: `upstream ${UPSTREAM} unreachable: ${e.code}` }));
    });
    req.pipe(up);
    return;
  }

  // ---- static files (path-traversal safe) ----
  const rel  = normalize(decodeURIComponent(req.url.split('?')[0])).replace(/^(\.\.[/\\])+/, '');
  const file = join(UI_DIR, rel === '/' ? 'index.html' : rel);
  if (!file.startsWith(UI_DIR)) { res.writeHead(403).end('forbidden'); return; }
  try {
    const body = await readFile(file);
    res.writeHead(200, { 'Content-Type': TYPES[extname(file)] ?? 'application/octet-stream',
                         'Cache-Control': 'no-store' });
    res.end(body);
  } catch { res.writeHead(404).end('not found'); }
});

server.listen(PORT, '0.0.0.0', () =>
  console.log(`ui  → http://localhost:${PORT}\napi → ${UPSTREAM}`));
