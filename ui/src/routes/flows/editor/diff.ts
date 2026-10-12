import type { GraphDraft, GraphEdge } from "./model";

type Rec = Record<string, unknown>;
const rec = (v: unknown): Rec => (v && typeof v === "object" && !Array.isArray(v) ? (v as Rec) : {});
const s = (v: unknown): string => (typeof v === "string" ? v : "");
const same = (a: unknown, b: unknown) => JSON.stringify(a ?? null) === JSON.stringify(b ?? null);
const FLOW_SETTINGS = [
  ["stt", "listening"],
  ["vad_stop_ms", "turn-taking"],
  ["context", "memory"],
  ["recording", "recording"],
] as const;

/** Plain-language changes from the published version to the draft, for the publish dialog. */
export function describeChanges(before: GraphDraft | null, after: GraphDraft): string[] {
  if (!before) return [];
  const out: string[] = [];
  const nameIn = (g: GraphDraft, id: string) => g.nodes.find((n) => n.id === id)?.name.trim() || "Untitled step";
  const old = new Map(before.nodes.map((n) => [n.id, n]));
  const now = new Map(after.nodes.map((n) => [n.id, n]));

  for (const n of after.nodes) if (!old.has(n.id)) out.push(`Added step ${n.name}`);
  for (const n of before.nodes) if (!now.has(n.id)) out.push(`Removed step ${n.name}`);
  for (const n of after.nodes) {
    const o = old.get(n.id);
    if (!o) continue;
    const [ol, nl, ot, nt] = [rec(o.config.llm), rec(n.config.llm), rec(o.config.tts), rec(n.config.tts)];
    if (o.name !== n.name) out.push(`Renamed ${o.name} to ${n.name}`);
    if (s(ol.provider) !== s(nl.provider) || s(ol.model) !== s(nl.model)) out.push(`${n.name}: model ${s(ol.model) || "none"} → ${s(nl.model) || "none"}`);
    if (s(ot.provider) !== s(nt.provider) || s(ot.voice) !== s(nt.voice))
      out.push(`${n.name}: voice ${s(ot.voice) || s(ot.provider) || "none"} → ${s(nt.voice) || s(nt.provider) || "none"}`);
    if (s(ol.system_prompt) !== s(nl.system_prompt)) out.push(`${n.name}: instructions changed`);
    if (!same(ol.fallbacks, nl.fallbacks) || !same(ot.fallbacks, nt.fallbacks) || !same(ol.max_parallel, nl.max_parallel) || !same(ot.clean_text, nt.clean_text))
      out.push(`${n.name}: advanced settings changed`);
    if (!same([...o.tools].sort(), [...n.tools].sort())) out.push(`${n.name}: tools changed`);
  }

  const key = (e: GraphEdge) => `${e.from}>${e.to}`;
  const oldEdges = new Map(before.edges.map((e) => [key(e), e]));
  const newEdges = new Map(after.edges.map((e) => [key(e), e]));
  for (const e of after.edges) {
    const o = oldEdges.get(key(e));
    if (!o) out.push(`New route ${nameIn(after, e.from)} → ${nameIn(after, e.to)}`);
    else if (o.condition !== e.condition) out.push(`Route ${nameIn(after, e.from)} → ${nameIn(after, e.to)}: condition changed`);
  }
  for (const e of before.edges) if (!newEdges.has(key(e))) out.push(`Removed route ${nameIn(before, e.from)} → ${nameIn(before, e.to)}`);

  if (before.entry !== after.entry) out.push(`Start step is now ${nameIn(after, after.entry)}`);
  const oldStart = rec(old.get(before.entry)?.config);
  const newStart = rec(now.get(after.entry)?.config);
  for (const [k, label] of FLOW_SETTINGS) if (!same(oldStart[k], newStart[k])) out.push(`Flow settings: ${label} changed`);
  if (!same(before.handoff_cue, after.handoff_cue)) out.push("Flow settings: route sound changed");
  return out;
}
