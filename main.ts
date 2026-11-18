// The entrypoint: start the static Go core, register its port with the env,
// wait for the env to say the trial is done, then exit 0.
//
// The Go binary (./forge, committed beside this file) is the whole forge:
// smart-HTTP git, REST v3, GraphQL and MCP over one in-memory store. This
// file is only the lifecycle the harness drives: spawn the core, learn the
// ephemeral port it bound, tell the env, and poll /done.
//
//   1. MAKEFUN_ENV_URL names the env's origin. Without it there is nothing
//      to register with, so the process exits 1 at once.
//   2. Spawn ./forge (resolved beside this file, so it works from /bot).
//      It binds 127.0.0.1:0 and prints `PORT <n>` on its first stdout line.
//      Never bind 8080 — the env owns it.
//   3. POST $MAKEFUN_ENV_URL/register {"port": N}. The first register wins;
//      a 409 means another entry already registered, and this process just
//      keeps waiting.
//   4. GET $MAKEFUN_ENV_URL/done until it answers {"done": true}. Poll with
//      a short backoff; never exit before it says true.
//   5. Kill the core and exit 0.

import { spawn, type ChildProcess } from 'node:child_process';
import { dirname, join } from 'node:path';
import { fileURLToPath } from 'node:url';

const ENV_URL = (process.env.MAKEFUN_ENV_URL ?? '').replace(/\/+$/, '');
if (ENV_URL === '') {
  process.stderr.write('[entry] MAKEFUN_ENV_URL is not set: there is no env to register with\n');
  process.exit(1);
}

const REQUEST_TIMEOUT_MS = 30_000;
const RETRY_FIRST_MS = 250;
const RETRY_MAX_MS = 2_000;

const sleep = (ms: number): Promise<void> => new Promise((resolve) => setTimeout(resolve, ms));

function backoff(attempt: number): number {
  return Math.min(RETRY_MAX_MS, RETRY_FIRST_MS * 2 ** Math.min(attempt, 8));
}

async function register(port: number): Promise<void> {
  for (let attempt = 0; ; attempt += 1) {
    try {
      const res = await fetch(`${ENV_URL}/register`, {
        method: 'POST',
        headers: { 'content-type': 'application/json' },
        body: JSON.stringify({ port }),
        signal: AbortSignal.timeout(REQUEST_TIMEOUT_MS),
      });
      if (res.status === 200 || res.status === 409) return;
      if (res.status === 400 || res.status === 405) {
        throw new Error(`register answered HTTP ${res.status}: ${await res.text()}`);
      }
    } catch (err) {
      if (err instanceof Error && err.message.startsWith('register answered HTTP ')) throw err;
    }
    await sleep(backoff(attempt));
  }
}

async function waitForDone(): Promise<void> {
  for (let attempt = 0; ; attempt += 1) {
    try {
      const res = await fetch(`${ENV_URL}/done`, { signal: AbortSignal.timeout(REQUEST_TIMEOUT_MS) });
      if (res.ok) {
        const body = (await res.json()) as { done?: unknown };
        if (body.done === true) return;
      }
    } catch {
      // The env is not answering yet, or the poll timed out: not done, poll again.
    }
    await sleep(backoff(attempt));
  }
}

/** Start ./forge and resolve with the port it prints on its first line. */
function startCore(): Promise<{ child: ChildProcess; port: number }> {
  const dir = dirname(fileURLToPath(import.meta.url));
  const binary = join(dir, 'forge');
  const child = spawn(binary, [], { stdio: ['ignore', 'pipe', 'inherit'] });
  return new Promise((resolve, reject) => {
    let buffer = '';
    let settled = false;
    const onData = (chunk: Buffer): void => {
      if (settled) return;
      buffer += chunk.toString('utf8');
      const newline = buffer.indexOf('\n');
      if (newline < 0) return;
      const line = buffer.slice(0, newline).trim();
      const match = /^PORT (\d+)$/.exec(line);
      if (match === null) {
        settled = true;
        reject(new Error(`forge printed an unexpected first line: ${JSON.stringify(line)}`));
        return;
      }
      settled = true;
      resolve({ child, port: Number(match[1]) });
    };
    child.stdout?.on('data', onData);
    child.once('error', (err) => {
      if (!settled) {
        settled = true;
        reject(err);
      }
    });
    child.once('exit', (code) => {
      if (!settled) {
        settled = true;
        reject(new Error(`forge exited early with code ${code}`));
      }
    });
  });
}

async function main(): Promise<void> {
  const { child, port } = await startCore();
  process.stdout.write(`[entry] forge listening on port ${port}\n`);

  await register(port);
  process.stdout.write('[entry] registered; waiting for the env to say done\n');

  await waitForDone();
  child.kill('SIGTERM');
  process.stdout.write('[entry] done\n');
}

try {
  await main();
  process.exit(0);
} catch (err) {
  process.stderr.write(`[entry] ${err instanceof Error ? err.message : String(err)}\n`);
  process.exit(1);
}
