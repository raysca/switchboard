import { describe, expect, it } from "vitest";
import { readFileSync } from "node:fs";
import { resolve } from "node:path";

const css = readFileSync(resolve(__dirname, "../../../styles.css"), "utf8");

// The body of every top-level @media block whose query contains `query`.
function mediaBodies(query: string): string[] {
  const out: string[] = [];
  let i = 0;
  while ((i = css.indexOf("@media", i)) !== -1) {
    const open = css.indexOf("{", i);
    const head = css.slice(i, open);
    let depth = 1;
    let j = open + 1;
    while (depth > 0 && j < css.length) depth += css[j] === "{" ? 1 : css[j] === "}" ? -1 : 0, j++;
    if (head.includes(query)) out.push(css.slice(open + 1, j - 1));
    i = j;
  }
  return out;
}
const outsideMedia = (() => {
  let rest = css;
  for (const q of ["@media"]) {
    let i;
    while ((i = rest.indexOf(q)) !== -1) {
      const open = rest.indexOf("{", i);
      let depth = 1;
      let j = open + 1;
      while (depth > 0 && j < rest.length) depth += rest[j] === "{" ? 1 : rest[j] === "}" ? -1 : 0, j++;
      rest = rest.slice(0, i) + rest.slice(j);
    }
  }
  return rest;
})();

describe("the handoff cable and lamp motion", () => {
  it("only moves inside prefers-reduced-motion: no-preference", () => {
    expect(mediaBodies("no-preference").join("\n")).toContain(".handoff-edge--just");
    const moving = /\.(handoff-edge--just|step-node--active)[^{}]*\{[^{}]*(animation|transition)\s*:/;
    expect(outsideMedia).not.toMatch(moving);
  });
  it("is switched off under reduce", () => {
    const reduce = mediaBodies("reduce").join("\n");
    expect(reduce).toMatch(/animation-duration:\s*0\.01ms/);
  });
});
