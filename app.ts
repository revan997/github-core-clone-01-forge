// app.ts — the HTTP server: routing, auth, body reading, and the JSON 404 fallback.
//
// Routing order: /healthz (no auth) → /api/graphql → /mcp → /api/v3* → git smart HTTP
// (/<owner>/<repo>.git/info/refs, /git-upload-pack, /git-receive-pack) → JSON 404.
// Handlers never crash the process: a throw becomes a 500 with a JSON body.
// Auth: when MAKEFUN_SHOP_TOKEN is set, the Bearer token must equal it; when unset
// (local development), any non-empty Bearer token is accepted; a missing token is 401.
// Bodies are capped at 64 MiB.

import { createServer, type IncomingMessage, type Server, type ServerResponse } from 'node:http';
import { Store } from './store.ts';
import { handleInfoRefs, handleReceivePack, handleUploadPack, type GitResponse } from './git.ts';
import { handleGraphQL } from './graphql.ts';
import { handleMcp } from './mcp.ts';
import { handleRest, type RestResponse } from './rest.ts';

const MAX_BODY_BYTES = 64 * 1024 * 1024;

export function authToken(): string {
  return process.env.MAKEFUN_SHOP_TOKEN ?? '';
}

function bearerToken(req: IncomingMessage): string | null {
  const header = req.headers.authorization;
  if (!header) return null;
  if (!header.startsWith('Bearer ')) return null;
  const token = header.slice('Bearer '.length).trim();
  return token === '' ? null : token;
}

function authorized(req: IncomingMessage): boolean {
  const expected = authToken();
  const presented = bearerToken(req);
  if (!presented) return false;
  if (expected === '') return true;
  return presented === expected;
}

function readBody(req: IncomingMessage): Promise<Buffer> {
  return new Promise((resolve, reject) => {
    const chunks: Buffer[] = [];
    let total = 0;
    let tooLarge = false;
    req.on('data', (chunk: Buffer) => {
      total += chunk.length;
      if (total > MAX_BODY_BYTES) {
        tooLarge = true;
        return;
      }
      chunks.push(chunk);
    });
    req.on('end', () => {
      if (tooLarge) reject(new Error('request body too large'));
      else resolve(Buffer.concat(chunks));
    });
    req.on('error', reject);
  });
}

function sendJson(res: ServerResponse, status: number, body: Buffer, extraHeaders: Record<string, string> | undefined, requestId: string): void {
  res.writeHead(status, {
    'content-type': 'application/json; charset=utf-8',
    'x-github-request-id': requestId,
    ...(extraHeaders ?? {}),
  });
  res.end(body);
}

function sendGit(res: ServerResponse, response: GitResponse, requestId: string): void {
  res.writeHead(response.status, {
    'content-type': response.contentType,
    'x-github-request-id': requestId,
  });
  res.end(response.body);
}

export function createForgeServer(): Server {
  const store = new Store();
  const server = createServer((req, res) => {
    void handleRequest(store, req, res);
  });
  return server;
}

async function handleRequest(store: Store, req: IncomingMessage, res: ServerResponse): Promise<void> {
  const requestId = store.nextRequestId();
  try {
    const rawUrl = req.url ?? '/';
    const host = req.headers.host ?? '127.0.0.1';
    const url = new URL(rawUrl, `http://${host}`);
    const method = (req.method ?? 'GET').toUpperCase();
    const segments = url.pathname.split('/').filter((segment) => segment !== '');
    const decoded = segments.map((segment) => {
      try {
        return decodeURIComponent(segment);
      } catch {
        return segment;
      }
    });

    // GET /healthz — the boot probe, no auth.
    if (method === 'GET' && url.pathname === '/healthz') {
      sendJson(res, 200, Buffer.from(JSON.stringify({ ok: true }), 'utf8'), undefined, requestId);
      return;
    }

    // Everything else needs a valid bearer token.
    if (!authorized(req)) {
      sendJson(res, 401, Buffer.from(JSON.stringify({ message: 'Requires authentication' }), 'utf8'), undefined, requestId);
      return;
    }

    const body = method === 'GET' || method === 'HEAD' ? Buffer.alloc(0) : await readBody(req);

    // POST /api/graphql
    if (url.pathname === '/api/graphql') {
      if (method !== 'POST') {
        sendJson(res, 404, Buffer.from(JSON.stringify({ message: 'Not Found' }), 'utf8'), undefined, requestId);
        return;
      }
      const response = handleGraphQL(store, body);
      sendJson(res, response.status, response.body, response.headers, requestId);
      return;
    }

    // POST /mcp
    if (url.pathname === '/mcp') {
      if (method !== 'POST') {
        sendJson(res, 404, Buffer.from(JSON.stringify({ message: 'Not Found' }), 'utf8'), undefined, requestId);
        return;
      }
      const response = handleMcp(store, body);
      res.writeHead(response.status, { 'content-type': 'application/json; charset=utf-8', 'x-github-request-id': requestId });
      res.end(response.body);
      return;
    }

    // /api/v3/*
    if (url.pathname === '/api/v3' || url.pathname.startsWith('/api/v3/')) {
      const restPath = url.pathname === '/api/v3' ? [] : decoded.slice(2);
      const response: RestResponse = handleRest({ store, method, url, path: restPath, body });
      sendJson(res, response.status, response.body, response.headers, requestId);
      return;
    }

    // Git smart HTTP: /<owner>/<repo>.git/info/refs, /git-upload-pack, /git-receive-pack.
    if (decoded.length === 3 && decoded[1]!.endsWith('.git')) {
      const owner = decoded[0]!;
      const repoName = decoded[1]!.slice(0, -'.git'.length);
      if (decoded[2] === 'git-upload-pack' && method === 'POST') {
        sendGit(res, handleUploadPack(store, owner, repoName, body), requestId);
        return;
      }
      if (decoded[2] === 'git-receive-pack' && method === 'POST') {
        sendGit(res, handleReceivePack(store, owner, repoName, body), requestId);
        return;
      }
    }
    if (decoded.length === 4 && decoded[1]!.endsWith('.git') && decoded[2] === 'info' && decoded[3] === 'refs' && method === 'GET') {
      const owner = decoded[0]!;
      const repoName = decoded[1]!.slice(0, -'.git'.length);
      const service = url.searchParams.get('service');
      if (service !== 'git-upload-pack' && service !== 'git-receive-pack') {
        sendJson(res, 400, Buffer.from(JSON.stringify({ message: 'Not Found' }), 'utf8'), undefined, requestId);
        return;
      }
      sendGit(res, handleInfoRefs(store, owner, repoName, service, req.headers['git-protocol']), requestId);
      return;
    }

    // Anything else: a JSON 404.
    sendJson(res, 404, Buffer.from(JSON.stringify({ message: 'Not Found' }), 'utf8'), undefined, requestId);
  } catch (err) {
    if (!res.headersSent) {
      const detail = err instanceof Error ? err.message : 'internal error';
      res.writeHead(500, { 'content-type': 'application/json; charset=utf-8', 'x-github-request-id': requestId });
      res.end(JSON.stringify({ message: detail }));
    } else {
      res.end();
    }
  }
}
