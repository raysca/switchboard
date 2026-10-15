import { describe, expect, it } from "vitest";
import { readFileSync } from "node:fs";
import { resolve } from "node:path";

const css = readFileSync(resolve(__dirname, "../styles.css"), "utf8");
const rules = [...css.matchAll(/([^{}]+)\{([^{}]*)\}/g)].map((m) => ({ sel: m[1].trim(), body: m[2] }));
const decls = (re: RegExp) => rules.flatMap((r) => [...r.body.matchAll(re)].map((m) => ({ sel: r.sel, value: m[1].trim() })));

describe("Switchboard system rules", () => {
  it("status lamps use the ok and bad tokens", () => {
    const good = rules.find((r) => r.sel === ".status--good::before");
    const bad = rules.find((r) => r.sel === ".status--bad::before");
    expect(good?.body).toContain("var(--ok)");
    expect(bad?.body).toContain("var(--bad)");
  });
  // Square everywhere: pills, dots and the switch use the token too, so one edit changes the whole UI.
  it("uses only the radius tokens", () => {
    const off = decls(/border-radius:\s*([^;]+);/g).filter((d) => !["var(--radius)", "var(--radius-lg)"].includes(d.value));
    expect(off).toEqual([]);
  });
  // Small all-caps is for metric labels and dashboard table heads only; nothing else shouts.
  it("has no all-caps labels outside metric labels and dashboard table heads", () => {
    const caps = decls(/text-transform:\s*([^;]+);/g).filter((d) => d.value === "uppercase");
    expect(caps.map((d) => d.sel).sort()).toEqual([".dash-card th", ".kpi__label"]);
  });
  it("uses only the 450, 600 and 800 weights", () => {
    const off = decls(/font-weight:\s*([^;]+);/g).filter((d) => !["450", "600", "800"].includes(d.value));
    expect(off).toEqual([]);
  });
  it("never fades text with opacity (it silently breaks the contrast the tokens guarantee)", () => {
    const faded = rules.filter((r) => /step-node__(meta|status)/.test(r.sel) && /opacity\s*:/.test(r.body));
    expect(faded).toEqual([]);
  });
  it("has no drop shadows except the active-step ring", () => {
    const sh = decls(/box-shadow:\s*([^;]+);/g).filter((d) => d.sel !== ".step-node--active");
    expect(sh).toEqual([]);
  });
});
