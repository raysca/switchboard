import fs from "node:fs";
import { stackFile, type Stack } from "./lib/stack";

function alive(pid: number) {
  try { process.kill(pid, 0); return true; } catch { return false; }
}

// Stops every process group in .stack.json (SIGTERM, then SIGKILL after 10s).
export function stopStack(file: string = stackFile) {
  if (!fs.existsSync(file)) return;
  const stack: Stack = JSON.parse(fs.readFileSync(file, "utf8"));
  const pids = Object.values(stack.pids);
  for (const pid of pids) try { process.kill(-pid, "SIGTERM"); } catch {}
  const end = Date.now() + 10_000;
  while (pids.some(alive) && Date.now() < end) Atomics.wait(new Int32Array(new SharedArrayBuffer(4)), 0, 0, 100);
  for (const pid of pids) try { process.kill(-pid, "SIGKILL"); } catch {}
  fs.rmSync(stack.tmpDir, { recursive: true, force: true });
  fs.rmSync(file, { force: true });
}

export default async function globalTeardown() {
  stopStack();
}
