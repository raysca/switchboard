import { spawn } from "node:child_process";
import fs from "node:fs";
import net from "node:net";
import path from "node:path";

// Shared by the free suites' global setups (agents, one-port): process start-up and readiness.

export async function portOpen(port: number): Promise<boolean> {
  return new Promise((resolve) => {
    const s = net.connect(port, "127.0.0.1");
    s.once("connect", () => (s.destroy(), resolve(true)));
    s.once("error", () => resolve(false));
  });
}

export async function waitFor(logDir: string, what: string, ok: () => Promise<boolean>, timeoutMs = 90_000) {
  const end = Date.now() + timeoutMs;
  while (Date.now() < end) {
    if (await ok().catch(() => false)) return;
    await new Promise((r) => setTimeout(r, 250));
  }
  throw new Error(`timed out waiting for ${what} (logs: ${logDir})`);
}

// Every catalog provider's variables (server/catalog/data), MCP tokens and the suite's own.
const PROVIDER_VARS = /^(GOOGLE_|GEMINI_|XAI_|DEEPGRAM_|CARTESIA_|ELEVENLABS_|OPENAI_|NVIDIA_|MCP_TOKEN_|E2E_)/;

export function start(logDir: string, name: string, cmd: string, args: string[], cwd: string, env: NodeJS.ProcessEnv): number {
  const log = fs.openSync(path.join(logDir, `${name}.log`), "w");
  // detached => own process group, so teardown can kill children (vite, go) too.
  // No provider variables are inherited: a child gets exactly the ones passed in env.
  const base = Object.fromEntries(Object.entries(process.env).filter(([k]) => !PROVIDER_VARS.test(k)));
  // ENV_FILE=/dev/null: a development server would otherwise load examples/.env itself.
  const child = spawn(cmd, args, { cwd, env: { ...base, ENV_FILE: "/dev/null", ...env }, stdio: ["ignore", log, log], detached: true });
  child.unref();
  return child.pid!;
}
