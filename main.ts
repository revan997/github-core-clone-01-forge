// main.ts — the entrypoint and its lifecycle.
//
// Trial mode (MAKEFUN_ENV_URL is set): bind an ephemeral port — never 8080, that is the
// environment's own port — register it, then poll GET $MAKEFUN_ENV_URL/done until it says
// {"done": true}, and exit 0. Never hold a request open; never exit before done is true.
// If MAKEFUN_ENV_URL is unset, exit 1 — unless LOCAL_DEV=1, which starts the forge on a
// local port for grading and keeps it running.

import { createForgeServer, authToken } from './app.ts';

const envUrl = process.env.MAKEFUN_ENV_URL ?? '';
const localDev = process.env.LOCAL_DEV === '1';

async function sleep(ms: number): Promise<void> {
  await new Promise((resolve) => setTimeout(resolve, ms));
}

async function register(port: number): Promise<void> {
  // The env may still be coming up when the entry first dials it: retry briefly.
  let lastError: unknown = null;
  for (let attempt = 0; attempt < 10; attempt += 1) {
    try {
      const response = await fetch(`${envUrl}/register`, {
        method: 'POST',
        headers: { 'content-type': 'application/json' },
        body: JSON.stringify({ port }),
      });
      if (response.ok || response.status === 409) return;
      lastError = new Error(`register failed with HTTP ${response.status}`);
    } catch (err) {
      lastError = err;
    }
    await sleep(200 * (attempt + 1));
  }
  throw lastError instanceof Error ? lastError : new Error('register failed');
}

async function waitForDone(): Promise<void> {
  for (;;) {
    try {
      const response = await fetch(`${envUrl}/done`);
      if (response.ok) {
        const body = (await response.json()) as { done?: boolean };
        if (body.done === true) return;
      }
    } catch {
      // A transient env hiccup must not kill the entry; keep polling.
    }
    await sleep(500);
  }
}

async function main(): Promise<void> {
  if (!envUrl && !localDev) {
    console.error('MAKEFUN_ENV_URL is not set');
    process.exit(1);
  }
  const server = createForgeServer();
  const port = await new Promise<number>((resolve, reject) => {
    server.once('error', reject);
    server.listen(0, '127.0.0.1', () => {
      const address = server.address();
      if (address && typeof address === 'object') resolve(address.port);
      else reject(new Error('could not bind a port'));
    });
  });
  if (localDev && !envUrl) {
    const token = authToken();
    console.log(`makefun-forge listening on http://127.0.0.1:${port}`);
    console.log(token === '' ? 'no MAKEFUN_SHOP_TOKEN set: any non-empty Bearer token is accepted' : 'MAKEFUN_SHOP_TOKEN is set');
    return; // keep running until killed
  }
  await register(port);
  await waitForDone();
  server.close();
  process.exit(0);
}

main().catch((error: unknown) => {
  console.error(error instanceof Error ? error.message : String(error));
  process.exit(1);
});
