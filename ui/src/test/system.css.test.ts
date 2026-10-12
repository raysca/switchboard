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
  // 999px is the filter pills of the Calls list (canvas board 13); everything else is the token or a dot.
  it("uses one radius: the token, 50% for dots, 999px for pills", () => {
    const off = decls(/border-radius:\s*([^;]+);/g).filter((d) => d.value !== "var(--radius)" && d.value !== "50%" && d.value !== "999px");
    expect(off).toEqual([]);
  });
  it("has no all-caps labels", () => {
    expect(decls(/text-transform:\s*([^;]+);/g).filter((d) => d.value === "uppercase")).toEqual([]);
  });
  it("uses only the 450, 600 and 800 weights", () => {
    const off = decls(/font-weight:\s*([^;]+);/g).filter((d) => !["450", "600", "800"].includes(d.value));
    expect(off).toEqual([]);
  });
  it("never fades text with opacity (it silently breaks the contrast the tokens guarantee)", () => {
    const faded = rules.filter((r) => /agent-node__(meta|status)/.test(r.sel) && /opacity\s*:/.test(r.body));
    expect(faded).toEqual([]);
  });
  it("has no drop shadows except the active-node ring", () => {
    const sh = decls(/box-shadow:\s*([^;]+);/g).filter((d) => d.sel !== ".agent-node--active");
    expect(sh).toEqual([]);
  });
});
