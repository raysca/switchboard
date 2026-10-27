import { spawnSync } from "node:child_process";
import fs from "node:fs";
import path from "node:path";
import type { Stack } from "./stack";

// Demos are seeded through the standalone CLI (demos/seed, built as bin/seed), never the Dashboard:
// the server has no demo API, no demo config and no demo environment. Each demo spec seeds its own
// demo in beforeAll because 05-first-run needs an empty store and 50-screens archives every agent
// before the demo specs run, so global setup must not seed. bin/seed wipes and recreates the demo,
// so a rerun after a spec archived it leaves exactly one live agent.

export type DemoName = "dental" | "atlas";

/** The fixture each demo's seed resolves --mcp <logical>=<base url>, and the port it runs on. */
export const DEMO_MCP: Record<DemoName, { logical: string; port: number }> = {
  dental: { logical: "scheduling", port: 18597 },
  atlas: { logical: "inventory", port: 18598 },
};

// The demo fixtures' token env names (each demo's MCPServerSpec.TokenEnv) and the values global
// setup starts the fixtures with. Seed reads the token from the process environment: never an argument.
export const DEMO_TOKEN_ENV: Record<DemoName, string> = { dental: "DENTAL_MCP_TOKEN", atlas: "ATLAS_MCP_TOKEN" };
export const DEMO_TOKEN: Record<DemoName, string> = { dental: "e2e-dental-token", atlas: "e2e-atlas-token" };

/** The management key is read from this env var by bin/seed; it is never on the command line. */
export const MANAGEMENT_KEY_ENV = "GOVOICE_MANAGEMENT_API_KEY";

export function seedArgs(stack: Stack, demo: DemoName): string[] {
  const { logical, port } = DEMO_MCP[demo];
  return [
    "--server", stack.adminUrl,
    "--key-env", MANAGEMENT_KEY_ENV,
    "--demo", demo,
    "--mcp", `${logical}=http://127.0.0.1:${port}`,
    "--yes",
  ];
}

/** Seeds one demo through bin/seed and fails the spec file loudly if the CLI exits non-zero. */
export function seedDemo(stack: Stack, demo: DemoName): void {
  const bin = path.join(stack.repo, "bin/seed");
  const args = seedArgs(stack, demo);
  const log = path.join(stack.logDir, `seed-${demo}.log`);
  const result = spawnSync(bin, args, {
    cwd: stack.repo,
    encoding: "utf8",
    // Only the seed's own inputs are added; the management key and the demo token travel in the
    // environment, so neither appears in the process list or in the log.
    env: { ...process.env, [MANAGEMENT_KEY_ENV]: stack.managementKey, [DEMO_TOKEN_ENV[demo]]: DEMO_TOKEN[demo] },
  });
  const output = `$ bin/seed ${args.join(" ")}\n${result.stdout ?? ""}${result.stderr ?? ""}`;
  fs.writeFileSync(log, output);
  if (result.error) throw new Error(`bin/seed --demo ${demo} could not start: ${result.error.message} (log: ${log})`);
  if (result.status !== 0) {
    const reason = result.signal ? `killed by ${result.signal}` : `exit ${result.status}`;
    throw new Error(`bin/seed --demo ${demo} failed (${reason}; log: ${log})\n${output.trim()}`);
  }
}
