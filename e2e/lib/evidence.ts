import type { Page } from "@playwright/test";
import { test } from "@playwright/test";
import fs from "node:fs";
import path from "node:path";

const dir = path.resolve(__dirname, "../evidence"); // emptied by global-setup

// Full-page screenshot to e2e/evidence/<NN>-<slug>.png, attached to the report.
// Numbering continues from the files already there, so it survives worker restarts.
export async function step(page: Page, name: string): Promise<string> {
  fs.mkdirSync(dir, { recursive: true });
  const next = fs.readdirSync(dir).filter((f) => f.endsWith(".png")).length + 1;
  const slug = name.toLowerCase().replace(/[^a-z0-9]+/g, "-").replace(/^-|-$/g, "");
  const file = path.join(dir, `${String(next).padStart(2, "0")}-${slug}.png`);
  await page.screenshot({ path: file, fullPage: true });
  await test.info().attach(name, { path: file, contentType: "image/png" });
  return file;
}
