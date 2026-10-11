import { execFileSync, spawn } from "node:child_process";
import crypto from "node:crypto";
import fs from "node:fs";
import net from "node:net";
import os from "node:os";
import path from "node:path";
import { SKIP_REASON, stackFile, type Stack } from "./lib/stack";
import { stopStack } from "./global-teardown";

// Non-default ports: 8080 is OrbStack's on this machine.
const PORTS = { public: 18580, admin: 19590, ui: 15173, custom: 13000 };
const repo = path.resolve(__dirname, "..");
const logDir = path.join(__dirname, "test-results", "logs");

async function portOpen(port: number): Promise<boolean> {
  return new Promise((resolve) => {
    const s = net.connect(port, "127.0.0.1");
    s.once("connect", () => (s.destroy(), resolve(true)));
    s.once("error", () => resolve(false));
  });
}

async function waitFor(what: string, ok: () => Promise<boolean>, timeoutMs = 60_000) {
  const end = Date.now() + timeoutMs;
  while (Date.now() < end) {
    if (await ok().catch(() => false)) return;
    await new Promise((r) => setTimeout(r, 250));
  }
  throw new Error(`timed out waiting for ${what} (logs: ${logDir})`);
}

// Every catalog provider's variables (server/catalog/data), MCP tokens and the suite's own.
const PROVIDER_VARS = /^(GOOGLE_|GEMINI_|XAI_|DEEPGRAM_|CARTESIA_|ELEVENLABS_|OPENAI_|NVIDIA_|MCP_TOKEN_|E2E_)/;

function start(name: string, cmd: string, args: string[], cwd: string, env: NodeJS.ProcessEnv): number {
  const log = fs.openSync(path.join(logDir, `${name}.log`), "w");
  // detached => own process group, so teardown can kill children (vite, go) too.
  // No provider variables are inherited: a child gets exactly the ones passed in env.
  const base = Object.fromEntries(Object.entries(process.env).filter(([k]) => !PROVIDER_VARS.test(k)));
  // ENV_FILE=/dev/null: a development server would otherwise load examples/.env itself.
  const child = spawn(cmd, args, { cwd, env: { ...base, ENV_FILE: "/dev/null", ...env }, stdio: ["ignore", log, log], detached: true });
  child.unref();
  return child.pid!;
}

export default async function globalSetup() {
  if (!process.env.E2E_GOOGLE_SECRET) {
    console.warn(`\n[e2e] ${SKIP_REASON}\n`);
    return;
  }
  fs.mkdirSync(logDir, { recursive: true });
  fs.rmSync(path.join(__dirname, "evidence"), { recursive: true, force: true });
  for (const [name, port] of Object.entries(PORTS))
    if (await portOpen(port)) throw new Error(`port ${port} (${name}) already in use; stop the old stack first`);

  execFileSync("make", ["build"], { cwd: repo, stdio: ["ignore", fs.openSync(path.join(logDir, "build.log"), "w"), "inherit"] });

  const tmpDir = fs.mkdtempSync(path.join(os.tmpdir(), "govoice-e2e-"));
  const origins = [PORTS.ui, PORTS.custom].flatMap((p) => [`http://127.0.0.1:${p}`, `http://localhost:${p}`]);
  const config = fs
    .readFileSync(path.join(repo, "config/development.yaml"), "utf8")
    .replace(/^public_listen_addr:.*$/m, `public_listen_addr: "127.0.0.1:${PORTS.public}"`)
    .replace(/^admin_listen_addr:.*$/m, `admin_listen_addr: "127.0.0.1:${PORTS.admin}"`)
    .replace(/^origin_allowlist:\n(?:\s+-.*\n)+/m, `origin_allowlist:\n${origins.map((o) => `  - "${o}"\n`).join("")}`);
  const configPath = path.join(tmpDir, "server.yaml");
  fs.writeFileSync(configPath, config);

  const stack: Stack = {
    repo,
    publicUrl: `http://127.0.0.1:${PORTS.public}`,
    adminUrl: `http://127.0.0.1:${PORTS.admin}`,
    uiUrl: `http://127.0.0.1:${PORTS.ui}`,
    customClientUrl: `http://127.0.0.1:${PORTS.custom}`,
    managementKey: crypto.randomBytes(32).toString("hex"),
    tokenSecret: crypto.randomBytes(32).toString("hex"),
    devDir: path.join(tmpDir, "dev"),
    tmpDir,
    logDir,
    pids: {},
  };
  // The server reads provider secrets from its environment only: the service account
  // as a file (GOOGLE_APPLICATION_CREDENTIALS), the Gemini key as GOOGLE_API_KEY.
  const speech = process.env.E2E_GOOGLE_SECRET!;
  const isSA = speech.trim().startsWith("{");
  const providerEnv: Record<string, string> = {};
  if (isSA) {
    providerEnv.GOOGLE_APPLICATION_CREDENTIALS = path.join(tmpDir, "sa.json");
    fs.writeFileSync(providerEnv.GOOGLE_APPLICATION_CREDENTIALS, speech, { mode: 0o600 });
  }
  const llmKey = process.env.E2E_GOOGLE_LLM_SECRET ?? (isSA ? "" : speech);
  if (llmKey) providerEnv.GOOGLE_API_KEY = llmKey;

  const save = () => fs.writeFileSync(stackFile, JSON.stringify(stack, null, 2), { mode: 0o600 });
  save();

  try {
    stack.pids.server = start("server", path.join(repo, "bin/server"), ["-config", configPath, "-vad-mode", "inprocess"], repo, {
      GOVOICE_ROOT: repo,
      GOVOICE_DEV_DIR: stack.devDir,
      TOKEN_SECRET: stack.tokenSecret,
      GOVOICE_MANAGEMENT_API_KEY: stack.managementKey,
      ...providerEnv,
    });
    save();
    stack.pids.ui = start("ui", process.execPath, [path.join(repo, "ui/node_modules/vite/bin/vite.js"), "--config", path.join(__dirname, "vite.e2e.config.mjs")], path.join(repo, "ui"), {
      E2E_UI_PORT: String(PORTS.ui),
      E2E_ADMIN_URL: stack.adminUrl,
      E2E_PUBLIC_URL: stack.publicUrl,
    });
    save();
    stack.pids.customClient = start("custom-client", process.execPath, ["server.js"], path.join(repo, "examples/rtvi_client"), {
      PORT: String(PORTS.custom),
      HOST: "127.0.0.1",
    });
    save();

    await waitFor("server /ready", async () => (await fetch(`${stack.adminUrl}/ready`)).status === 200);
    await waitFor("admin UI", async () => (await fetch(stack.uiUrl)).ok);
    await waitFor("custom client", () => portOpen(PORTS.custom));
  } catch (err) {
    stopStack();
    throw err;
  }
}
