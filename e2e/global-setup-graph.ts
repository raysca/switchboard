import { execFileSync, spawn } from "node:child_process";
import crypto from "node:crypto";
import fs from "node:fs";
import net from "node:net";
import os from "node:os";
import path from "node:path";
import { graphStackFile, type Stack } from "./lib/stack";
import { stopStack } from "./global-teardown";

// Starts the stack for the graph suite. By default with the FAKE providers (go
// build -tags fakeproviders): no secrets, no network, no cost; the fake LLM labels
// each reply with its node's prompt and calls a node's transfer tool on "transfer
// to <node>". With E2E_PROVIDER=xai it is the real server (go build -tags
// xaispeech) and the specs use a stored xAI credential: real STT/LLM/TTS calls.
// Ports differ from the paid suite's, so both can run on one machine.
const LIVE = process.env.E2E_PROVIDER === "xai";
const PORTS = { public: 18581, admin: 19591, ui: 15174 };
const repo = path.resolve(__dirname, "..");
const logDir = path.join(__dirname, "test-results-graph", "logs");

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

function start(name: string, cmd: string, args: string[], cwd: string, env: NodeJS.ProcessEnv): number {
  const log = fs.openSync(path.join(logDir, `${name}.log`), "w");
  // detached => own process group, so teardown can kill children (vite, go) too.
  const base = Object.fromEntries(Object.entries(process.env).filter(([k]) => !/^(GOOGLE_|GEMINI_|XAI_|E2E_)/.test(k)));
  const child = spawn(cmd, args, { cwd, env: { ...base, ...env }, stdio: ["ignore", log, log], detached: true });
  child.unref();
  return child.pid!;
}

function toneWav(rate: number, seconds: number, hz: number, amplitude: number): Buffer {
  const n = Math.round(rate * seconds);
  const pcm = Buffer.alloc(2 * n);
  for (let i = 0; i < n; i++) pcm.writeInt16LE(Math.round(amplitude * Math.sin((2 * Math.PI * hz * i) / rate)), 2 * i);
  const h = Buffer.alloc(44);
  h.write("RIFF", 0);
  h.writeUInt32LE(36 + pcm.length, 4);
  h.write("WAVEfmt ", 8);
  h.writeUInt32LE(16, 16);
  h.writeUInt16LE(1, 20); // PCM
  h.writeUInt16LE(1, 22); // mono
  h.writeUInt32LE(rate, 24);
  h.writeUInt32LE(rate * 2, 28);
  h.writeUInt16LE(2, 32);
  h.writeUInt16LE(16, 34);
  h.write("data", 36);
  h.writeUInt32LE(pcm.length, 40);
  return Buffer.concat([h, pcm]);
}

export default async function globalSetup() {
  if (LIVE && !process.env.E2E_XAI_SECRET) {
    console.warn("\n[e2e] No XAI_API_KEY (env or examples/.env): the live xAI run is skipped\n");
    return;
  }
  fs.mkdirSync(logDir, { recursive: true });
  fs.rmSync(path.join(__dirname, "evidence"), { recursive: true, force: true });
  for (const [name, port] of Object.entries(PORTS))
    if (await portOpen(port)) throw new Error(`port ${port} (${name}) already in use; stop the old stack first`);

  // The tags match `make build` (xaispeech); the fake build adds fakeproviders. Never ship the fake binary.
  const bin = LIVE ? "bin/server-live" : "bin/server-fake";
  execFileSync("go", ["build", "-tags", LIVE ? "xaispeech" : "xaispeech fakeproviders", "-o", bin, "./cmd/server"], {
    cwd: repo,
    stdio: ["ignore", fs.openSync(path.join(logDir, "build.log"), "w"), "inherit"],
  });

  const tmpDir = fs.mkdtempSync(path.join(os.tmpdir(), "govoice-e2e-graph-"));
  const origins = [`http://127.0.0.1:${PORTS.ui}`, `http://localhost:${PORTS.ui}`];
  const config = fs
    .readFileSync(path.join(repo, "config/development.yaml"), "utf8")
    .replace(/^public_listen_addr:.*$/m, `public_listen_addr: "127.0.0.1:${PORTS.public}"`)
    .replace(/^admin_listen_addr:.*$/m, `admin_listen_addr: "127.0.0.1:${PORTS.admin}"`)
    .replace(/^origin_allowlist:\n(?:\s+-.*\n)+/m, `origin_allowlist:\n${origins.map((o) => `  - "${o}"\n`).join("")}`);
  // A handoff cue sound: a 0.4 s 440 Hz tone (PCM16 mono 24 kHz) the agents can name as "chime".
  const chime = path.join(tmpDir, "chime.wav");
  fs.writeFileSync(chime, toneWav(24000, 0.4, 440, 12000));
  const configPath = path.join(tmpDir, "server.yaml");
  fs.writeFileSync(configPath, `${config}\naudio:\n  cues:\n    chime:\n      file: "${chime}"\n      gain_db: -20\n`);

  const stack: Stack = {
    repo,
    publicUrl: `http://127.0.0.1:${PORTS.public}`,
    adminUrl: `http://127.0.0.1:${PORTS.admin}`,
    uiUrl: `http://127.0.0.1:${PORTS.ui}`,
    customClientUrl: "",
    managementKey: crypto.randomBytes(32).toString("hex"),
    tokenSecret: crypto.randomBytes(32).toString("hex"),
    devDir: path.join(tmpDir, "dev"),
    tmpDir,
    logDir,
    pids: {},
  };
  const save = () => fs.writeFileSync(graphStackFile, JSON.stringify(stack, null, 2), { mode: 0o600 });
  save();

  try {
    stack.pids.server = start("server", path.join(repo, bin), ["-config", configPath, "-vad-mode", "inprocess"], repo, {
      GOVOICE_ROOT: repo,
      GOVOICE_DEV_DIR: stack.devDir,
      TOKEN_SECRET: stack.tokenSecret,
      GOVOICE_MANAGEMENT_API_KEY: stack.managementKey,
      ...(LIVE ? {} : { GOVOICE_FAKE_LLM_LABEL: "1" }),
    });
    save();
    stack.pids.ui = start("ui", process.execPath, [path.join(repo, "ui/node_modules/vite/bin/vite.js"), "--config", path.join(__dirname, "vite.e2e.config.mjs")], path.join(repo, "ui"), {
      E2E_UI_PORT: String(PORTS.ui),
      E2E_ADMIN_URL: stack.adminUrl,
      E2E_PUBLIC_URL: stack.publicUrl,
    });
    save();

    await waitFor("server /ready", async () => (await fetch(`${stack.adminUrl}/ready`)).status === 200);
    await waitFor("admin UI", async () => (await fetch(stack.uiUrl)).ok);
  } catch (err) {
    stopStack(graphStackFile);
    throw err;
  }
}
