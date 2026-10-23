import { describe, expect, it } from "vitest";
import { SETTING_HELP } from "./settingCopy";

const sources = import.meta.glob("./*.tsx", { query: "?raw", import: "default", eager: true }) as Record<string, string>;

const referenced = () =>
  new Set(
    Object.entries(sources)
      .filter(([path]) => !path.endsWith(".test.tsx"))
      .flatMap(([, src]) => [...src.matchAll(/<SettingHelp id="([^"]+)"/g)].map((m) => m[1])),
  );

describe("SETTING_HELP", () => {
  it("gives every entry a label, what it does and its default, with no placeholder text", () => {
    for (const [id, e] of Object.entries(SETTING_HELP)) {
      for (const text of [e.label, e.what, e.defaults, e.when, e.careful]) {
        if (text === undefined) continue;
        expect(text.trim(), id).not.toBe("");
        expect(text, id).not.toMatch(/TODO|TBD/);
      }
      expect(e.label, id).toBeTruthy();
      expect(e.what, id).toBeTruthy();
      expect(e.defaults, id).toBeTruthy();
    }
  });

  it("is used by controls: no entry is dead copy and no control names a missing entry", () => {
    const used = referenced();
    expect([...used].filter((id) => !(id in SETTING_HELP))).toEqual([]);
    expect(Object.keys(SETTING_HELP).filter((id) => !used.has(id))).toEqual([]);
  });
});
