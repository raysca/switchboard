import { describe, expect, it } from "vitest";
import { readFileSync } from "node:fs";
import { resolve } from "node:path";

const css = readFileSync(resolve(__dirname, "../styles.css"), "utf8");
const token = (name: string) => {
  const m = css.match(new RegExp(`--${name}:\\s*(#[0-9a-fA-F]{6})`));
  if (!m) throw new Error(`token --${name} not defined in styles.css`);
  return m[1];
};
const lum = (hex: string) => {
  const c = [1, 3, 5]
    .map((i) => parseInt(hex.slice(i, i + 2), 16) / 255)
    .map((v) => (v <= 0.03928 ? v / 12.92 : ((v + 0.055) / 1.055) ** 2.4));
  return 0.2126 * c[0] + 0.7152 * c[1] + 0.0722 * c[2];
};
const ratio = (a: string, b: string) => {
  const [x, y] = [lum(a), lum(b)].sort((p, q) => q - p);
  return (x + 0.05) / (y + 0.05);
};

describe("Switchboard tokens", () => {
  const pairs: [string, string, number][] = [
    ["ink", "paper", 4.5],
    ["ink", "surface", 4.5],
    ["ink-2", "paper", 4.5],
    ["ink-2", "surface", 4.5],
    ["caller", "paper", 4.5],
    ["caller", "surface", 4.5],
    ["ok", "paper", 4.5],
    ["bad", "paper", 4.5],
    ["ink", "lamp", 4.5], // text on an amber fill
    ["rule", "paper", 1.2], // hairline: decorative, must still be visible
  ];
  for (const [fg, bg, min] of pairs) {
    it(`--${fg} on --${bg} is at least ${min}:1`, () => {
      expect(ratio(token(fg), token(bg))).toBeGreaterThanOrEqual(min);
    });
  }
  it("amber is a fill or border, never text on the paper", () => {
    expect(ratio(token("lamp"), token("paper"))).toBeLessThan(4.5);
    expect(ratio(token("lamp"), token("ink"))).toBeGreaterThanOrEqual(4.5);
  });

  it("the dark call header uses light tokens for text and focus", () => {
    expect(ratio(token("paper"), token("ink"))).toBeGreaterThanOrEqual(4.5);
    expect(ratio(token("rule"), token("ink"))).toBeGreaterThanOrEqual(4.5);
    const dark = [...css.matchAll(/([^{}]*(?:call-panel__top|side-panel__head--dark)[^{}]*)\{([^}]*)\}/g)];
    expect(dark.length).toBeGreaterThan(0);
    for (const [, selector, body] of dark) {
      expect(body, selector).not.toMatch(/(?<!-)(?:^|[\s;])color:\s*var\(--(bad|ink-2|caller)\)/);
    }
    // The dark head needs its own focus ring: --caller on --ink is below 3:1.
    expect(css).toMatch(/call-panel__top[^{}]*:focus-visible[^{}]*\{[^}]*outline-color:\s*var\(--paper\)/);
  });
});
