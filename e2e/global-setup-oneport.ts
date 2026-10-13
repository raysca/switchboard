import { execFileSync, spawn } from "node:child_process";
import crypto from "node:crypto";
import fs from "node:fs";
import net from "node:net";
import os from "node:os";
import path from "node:path";
import { oneportStackFile, type Stack } from "./lib/stack";
import { stopStack } from "./global-teardown";

// Starts the one-port stack: the fake-provider server with the admin UI embedded
// (make ui-embed), public_admin on, and the browser admitted ONLY by
// origin_allow_same_host. No Vite: the UI, the API and the socket share the public origin.
const PORTS = { public: 18582, admin: 19592 };
const repo = path.resolve(__dirname, "..");
const logDir = path.join(__dirname, "test-results-oneport", "logs");

async function portOpen(port: number): Promise<boolean> {
  return new Promise((resolve) => {
    const s = net.connect(port, "127.0.0.1");
    s.once("connect", () => (s.destroy(), resolve(true)));
    s.once("error", () => resolve(false));
  });
}

async function waitFor(what: string, ok: () => Promise<boolean>, timeoutMs = 90_000) {
  const end = Date.now() + timeoutMs;
  while (Date.now() < end) {
    if (await ok().catch(() => false)) return;
    await new Promise((r) => setTimeout(r, 250));
  }
  throw new Error(`timed out waiting for ${what} (logs: ${logDir})`);
}

const PROVIDER_VARS = /^(GOOGLE_|GEMINI_|XAI_|DEEPGRAM_|CARTESIA_|ELEVENLABS_|OPENAI_|NVIDIA_|MCP_TOKEN_|E2E_)/;

export default async function globalSetup() {
  if (!fs.existsSync(path.join(repo, "server/adminui/static/index.html")))
    throw new Error("server/adminui/static/index.html is missing: run make ui-embed");
  fs.mkdirSync(logDir, { recursive: true });
  for (const [name, port] of Object.entries(PORTS))
    if (await portOpen(port)) throw new Error(`port ${port} (${name}) already in use; stop the old stack first`);

  // Built after the UI is embedded, so the binary carries it.
  execFileSync("go", ["build", "-tags", "xaispeech fakeproviders", "-o", "bin/server-fake", "./cmd/server"], {
    cwd: repo,
    stdio: ["ignore", fs.openSync(path.join(logDir, "build.log"), "w"), "inherit"],
  });

  const tmpDir = fs.mkdtempSync(path.join(os.tmpdir(), "govoice-e2e-oneport-"));
  const config = fs
    .readFileSync(path.join(repo, "config/development.yaml"), "utf8")
    .replace(/^public_listen_addr:.*$/m, `public_listen_addr: "127.0.0.1:${PORTS.public}"`)
    .replace(/^admin_listen_addr:.*$/m, `admin_listen_addr: "127.0.0.1:${PORTS.admin}"`)
    // An unrelated origin: only origin_allow_same_host can admit the browser.
    .replace(/^origin_allowlist:\n(?:\s+-.*\n)+/m, `origin_allowlist:\n  - "https://not-this-site.example"\n`);
  const configPath = path.join(tmpDir, "server.yaml");
  fs.writeFileSync(configPath, `${config}\npublic_admin:\n  enabled: true\norigin_allow_same_host: true\n`);

  const stack: Stack = {
    repo,
    publicUrl: `http://127.0.0.1:${PORTS.public}`,
    adminUrl: `http://127.0.0.1:${PORTS.admin}`,
    uiUrl: `http://127.0.0.1:${PORTS.public}`,
    customClientUrl: "",
    managementKey: crypto.randomBytes(32).toString("hex"),
    tokenSecret: crypto.randomBytes(32).toString("hex"),
    devDir: path.join(tmpDir, "dev"),
    tmpDir,
    logDir,
    pids: {},
  };
  const sa = path.join(tmpDir, "sa.json");
  fs.writeFileSync(sa, JSON.stringify({ type: "service_account" }), { mode: 0o600 });
  const base = Object.fromEntries(Object.entries(process.env).filter(([k]) => !PROVIDER_VARS.test(k)));
  const save = () => fs.writeFileSync(oneportStackFile, JSON.stringify(stack, null, 2), { mode: 0o600 });
  save();

  try {
    const log = fs.openSync(path.join(logDir, "server.log"), "w");
    const child = spawn(path.join(repo, "bin/server-fake"), ["-config", configPath, "-vad-mode", "inprocess"], {
      cwd: repo,
      env: {
        ...base,
        ENV_FILE: "/dev/null",
        GOVOICE_ROOT: repo,
        GOVOICE_DEV_DIR: stack.devDir,
        TOKEN_SECRET: stack.tokenSecret,
        GOVOICE_MANAGEMENT_API_KEY: stack.managementKey,
        GOOGLE_API_KEY: "fake-google-key",
        GOOGLE_APPLICATION_CREDENTIALS: sa,
        GOVOICE_FAKE_LLM_LABEL: "1",
        XAI_API_KEY: "fake-xai-key",
        ELEVENLABS_API_KEY: "fake-elevenlabs-key",
      },
      stdio: ["ignore", log, log],
      detached: true,
    });
    child.unref();
    stack.pids.server = child.pid!;
    save();
    await waitFor("server /ready", async () => (await fetch(`${stack.adminUrl}/ready`)).status === 200);
    await waitFor("embedded UI", async () => (await fetch(stack.publicUrl)).ok);
  } catch (err) {
    stopStack(oneportStackFile);
    throw err;
  }
}
