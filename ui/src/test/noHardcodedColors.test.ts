import { describe, expect, it } from "vitest";
import { readFileSync } from "node:fs";
import { resolve } from "node:path";

const colour = /#[0-9a-fA-F]{3,8}\b|rgba?\(/;

// Every non-test component source, so a new file cannot dodge the check.
const sources = import.meta.glob(["../**/*.tsx", "!../**/*.test.tsx"], { query: "?raw", import: "default", eager: true }) as Record<string, string>;

describe("no hard-coded colours outside the token block", () => {
  it("finds the component sources", () => {
    expect(Object.keys(sources).length).toBeGreaterThan(30);
  });
  for (const [f, src] of Object.entries(sources)) {
    it(f, () => {
      const bad = src.split("\n").map((l, i) => [i + 1, l] as const).filter(([, l]) => colour.test(l));
      expect(bad).toEqual([]);
    });
  }
  it("styles.css defines colours only in the :root token block", () => {
    const css = readFileSync(resolve(__dirname, "../styles.css"), "utf8");
    const after = css.split("/* end tokens */")[1] ?? css;
    const leaks = after.split("\n").filter((l) => colour.test(l) && !l.trim().startsWith("/*"));
    expect(leaks).toEqual([]);
  });
});
