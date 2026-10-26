import { withEchoProvider } from "./lib/echo-config";
import { execFileSync } from "node:child_process";
import crypto from "node:crypto";
import fs from "node:fs";
import os from "node:os";
import path from "node:path";
import { agentsStackFile, type Stack } from "./lib/stack";
import { portOpen, start, waitFor } from "./lib/stack-proc";
import { stopStack } from "./global-teardown";

// Starts the stack for the agent suite. By default with the simulated echo provider
// (no secrets, no network, no cost; providers.echo in the server config); the echo LLM labels
// each reply with its step's prompt and calls a step's transfer tool on "transfer
// to <step>". With E2E_PROVIDER=xai it is the real server (go build -tags
// xaispeech) reading XAI_API_KEY from its environment: real STT/LLM/TTS calls.
// Ports differ from the paid suite's, so both can run on one machine.
const LIVE = process.env.E2E_PROVIDER === "xai";
const PORTS = { public: 18581, admin: 19591, ui: 15174, mcp: 18595, mcpHole: 18596, dental: 18597, atlas: 18598 };
const repo = path.resolve(__dirname, "..");
const logDir = path.join(__dirname, "test-results-agents", "logs");

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

  // The tags match `make build` (xaispeech); the echo provider is in every build.
  const bin = LIVE ? "bin/server-live" : "bin/server-echo";
  execFileSync("go", ["build", "-tags", "xaispeech", "-o", bin, "./cmd/server"], {
    cwd: repo,
    stdio: ["ignore", fs.openSync(path.join(logDir, "build.log"), "w"), "inherit"],
  });
  execFileSync("go", ["build", "-o", "bin/mcptest", "./cmd/mcptest"], {
    cwd: repo,
    stdio: ["ignore", fs.openSync(path.join(logDir, "build-mcptest.log"), "w"), "inherit"],
  });

  execFileSync("go", ["build", "-o", "bin/dental-mcp", "./cmd/dental-mcp"], {
    cwd: repo,
    stdio: ["ignore", fs.openSync(path.join(logDir, "build-dental-mcp.log"), "w"), "inherit"],
  });

  execFileSync("go", ["build", "-o", "bin/atlas-mcp", "./cmd/atlas-mcp"], {
    cwd: repo,
    stdio: ["ignore", fs.openSync(path.join(logDir, "build-atlas-mcp.log"), "w"), "inherit"],
  });

  const tmpDir = fs.mkdtempSync(path.join(os.tmpdir(), "govoice-e2e-graph-"));
  const origins = [`http://127.0.0.1:${PORTS.ui}`, `http://localhost:${PORTS.ui}`];
  const config = withEchoProvider(fs
    .readFileSync(path.join(repo, "config/development.yaml"), "utf8")
    .replace(/^public_listen_addr:.*$/m, `public_listen_addr: "127.0.0.1:${PORTS.public}"`)
    .replace(/^admin_listen_addr:.*$/m, `admin_listen_addr: "127.0.0.1:${PORTS.admin}"`)
    .replace(/^origin_allowlist:\n(?:\s+-.*\n)+/m, `origin_allowlist:\n${origins.map((o) => `  - "${o}"\n`).join("")}`)
    // Local test MCP servers; a short MCP timeout keeps the failure cases quick.
    .replace(/^  allow_local_http: true$/m, "  allow_local_http: true\n  timeout: 2s"));
  // A handoff cue sound: a 0.4 s 440 Hz tone (PCM16 mono 24 kHz) the steps can name as "chime".
  const chime = path.join(tmpDir, "chime.wav");
  fs.writeFileSync(chime, toneWav(24000, 0.4, 440, 12000));
  // A background sound an agent can name as "office" (a quiet 1 s 220 Hz hum, looped).
  const office = path.join(tmpDir, "office.wav");
  fs.writeFileSync(office, toneWav(24000, 1, 220, 6000));
  const configPath = path.join(tmpDir, "server.yaml");
  // Quiet-caller handling is on by default; the suite switches it off server-wide so a spec that waits
  // is not hung up on, and the call-behavior spec turns it on per agent.
  fs.writeFileSync(
    configPath,
    `${config}\naudio:\n  cues:\n    chime:\n      file: "${chime}"\n      gain_db: -20\n  backgrounds:\n    office:\n      file: "${office}"\n      gain_db: -30\nidle:\n  user_timeout: 0s\n`,
  );

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
    mcpUrl: `http://127.0.0.1:${PORTS.mcp}`,
  };
  // The server reads provider secrets from its environment only. Echo needs none for the suite's own
  // provider. The dentistry demo's agent names google, xai and elevenlabs stages, so placeholder keys
  // for those make it creatable; nothing builds them (dental-helpers rewrites every stage to echo).
  // Live: the xAI key and nothing else.
  let providerEnv: Record<string, string>;
  if (LIVE) providerEnv = { XAI_API_KEY: process.env.E2E_XAI_SECRET! };
  else {
    const sa = path.join(tmpDir, "sa.json");
    fs.writeFileSync(sa, JSON.stringify({ type: "service_account" }), { mode: 0o600 });
    providerEnv = { GOOGLE_API_KEY: "fake-google-key", GOOGLE_APPLICATION_CREDENTIALS: sa, XAI_API_KEY: "fake-xai-key", ELEVENLABS_API_KEY: "fake-elevenlabs-key" };
  }

  const save = () => fs.writeFileSync(agentsStackFile, JSON.stringify(stack, null, 2), { mode: 0o600 });
  save();

  try {
    stack.pids.server = start(logDir, "server", path.join(repo, bin), ["-config", configPath, "-vad-mode", "inprocess"], repo, {
      GOVOICE_ROOT: repo,
      GOVOICE_DEV_DIR: stack.devDir,
      TOKEN_SECRET: stack.tokenSecret,
      GOVOICE_MANAGEMENT_API_KEY: stack.managementKey,
      MCP_TOKEN_TEST: "e2e-mcp-token",
      SWITCHBOARD_SECRETS_KEY: "e2e:AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=",
      DENTAL_MCP_URL: `http://127.0.0.1:${PORTS.dental}/mcp`,
      MCP_TOKEN_DENTAL: "e2e-dental-token",
      // The Atlas demo needs BOTH of these for GET /api/v1/demo to report available.
      ATLAS_DEMO_URL: `http://127.0.0.1:${PORTS.atlas}`,
      MCP_TOKEN_ATLAS: "e2e-atlas-token",
      ...providerEnv,
    });
    save();
    stack.pids.mcptest = start(logDir, "mcptest", path.join(repo, "bin/mcptest"), ["-addr", `127.0.0.1:${PORTS.mcp}`, "-blackhole-addr", `127.0.0.1:${PORTS.mcpHole}`, "-token", "e2e-mcp-token"], repo, {});
    save();
    stack.pids.dental = start(logDir, "dental-mcp", path.join(repo, "bin/dental-mcp"), [], repo, { PORT: String(PORTS.dental), DENTAL_MCP_TOKEN: "e2e-dental-token" });
    save();
    stack.pids.atlas = start(logDir, "atlas-mcp", path.join(repo, "bin/atlas-mcp"), [], repo, { PORT: String(PORTS.atlas), ATLAS_MCP_TOKEN: "e2e-atlas-token" });
    save();
    stack.pids.ui = start(logDir, "ui", process.execPath, [path.join(repo, "ui/node_modules/vite/bin/vite.js"), "--config", path.join(__dirname, "vite.e2e.config.mjs")], path.join(repo, "ui"), {
      E2E_UI_PORT: String(PORTS.ui),
      E2E_ADMIN_URL: stack.adminUrl,
      E2E_PUBLIC_URL: stack.publicUrl,
    });
    save();

    await waitFor(logDir, "server /ready", async () => (await fetch(`${stack.adminUrl}/ready`)).status === 200);
    await waitFor(logDir, "admin UI", async () => (await fetch(stack.uiUrl)).ok);
    await waitFor(logDir, "dental-mcp", async () => (await fetch(`http://127.0.0.1:${PORTS.dental}/health`)).ok);
    await waitFor(logDir, "atlas-mcp", async () => (await fetch(`http://127.0.0.1:${PORTS.atlas}/health`)).ok);
    await waitFor(logDir, "mcptest", async () => (await fetch(`${stack.mcpUrl}/_record`)).ok);
  } catch (err) {
    stopStack(agentsStackFile);
    throw err;
  }
}
